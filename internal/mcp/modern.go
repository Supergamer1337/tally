package mcp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Keys in _meta, from MCP 2026-07-28.
const (
	metaVersion      = "io.modelcontextprotocol/protocolVersion"
	metaCapabilities = "io.modelcontextprotocol/clientCapabilities"
	metaClient       = "io.modelcontextprotocol/clientInfo"
	metaServer       = "io.modelcontextprotocol/serverInfo"
	metaSubscription = "io.modelcontextprotocol/subscriptionId"
)

// How long clients may keep a list. Tools change only when the user flips the switch, and
// listeners hear about that at once, so a minute is a safe upper bound for the rest.
const (
	toolsTTL    = time.Minute
	discoverTTL = time.Hour
)

// modern serves protocol 2026-07-28 and later: no session, and every request says in _meta
// which version it speaks, who the client is and what it can do. The headers mirror the body
// so proxies can route on them, and must agree with it.
func (h *Handler) modern(w http.ResponseWriter, r *http.Request, version string, body []byte) {
	fail := func(status int, id json.RawMessage, e *rpcError) { writeError(w, status, id, e) }
	if body[0] != '{' {
		fail(http.StatusBadRequest, nil, &rpcError{Code: codeInvalidRequest, Message: "Send one JSON-RPC message per request, batches are not part of " + version})
		return
	}
	msg, bad := parse(body)
	if bad != nil {
		fail(http.StatusBadRequest, bad.ID, bad.Error)
		return
	}
	if msg.response {
		fail(http.StatusBadRequest, msg.id, &rpcError{Code: codeInvalidRequest, Message: "Clients do not send responses in " + version})
		return
	}
	if !msg.hasID {
		// No client to server notifications are defined over HTTP any more. Accept and drop.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if got := r.Header.Get("Mcp-Method"); got != msg.method {
		fail(http.StatusBadRequest, msg.id, &rpcError{Code: codeHeaderMismatch, Message: fmt.Sprintf("Header mismatch: Mcp-Method header is %s but the body method is %q", quote(got), msg.method)})
		return
	}
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
		Name *string                    `json:"name"`
	}
	if len(msg.params) > 0 {
		json.Unmarshal(msg.params, &p)
	}
	var pv string
	json.Unmarshal(p.Meta[metaVersion], &pv)
	caps := p.Meta[metaCapabilities]
	if pv == "" || len(caps) == 0 || caps[0] != '{' {
		fail(http.StatusBadRequest, msg.id, &rpcError{Code: codeInvalidParams, Message: "params._meta needs " + metaVersion + " and " + metaCapabilities})
		return
	}
	if pv != version {
		fail(http.StatusBadRequest, msg.id, &rpcError{Code: codeHeaderMismatch, Message: fmt.Sprintf("Header mismatch: MCP-Protocol-Version header is %q but the body names %q", version, pv)})
		return
	}
	if msg.method == "tools/call" {
		got, ok := headerValue(r.Header.Get("Mcp-Name"))
		if !ok || p.Name == nil || got != *p.Name {
			fail(http.StatusBadRequest, msg.id, &rpcError{Code: codeHeaderMismatch, Message: "Header mismatch: Mcp-Name header must be the name of the tool being called"})
			return
		}
	}

	c := &call{ctx: r.Context(), agent: clientName(p.Meta[metaClient], r.UserAgent())}
	h.Log.Debug("mcp", "protocol", version, "method", msg.method, "agent", c.agent)
	var result map[string]any
	var err error
	switch msg.method {
	case "server/discover":
		if err := h.Svc.Store.SeeAgent(c.ctx, c.agent, "", time.Now()); err != nil {
			h.Log.Error("mcp: record agent", "err", err)
		}
		result = map[string]any{
			"supportedVersions": allVersions,
			"capabilities":      map[string]any{"tools": map[string]any{"listChanged": true}},
			"instructions":      instructions,
			"ttlMs":             discoverTTL.Milliseconds(),
			"cacheScope":        "public",
		}
	case "tools/list":
		result, err = h.listTools(c.ctx)
		if result != nil {
			// The list follows a setting of this user's, so it is not for shared caches.
			result["ttlMs"], result["cacheScope"] = toolsTTL.Milliseconds(), "private"
		}
	case "tools/call":
		result, err = h.callTool(c, msg.params)
	case "subscriptions/listen":
		h.listen(w, r, msg.id, msg.params)
		return
	default:
		// The transport answers a method the server does not have with 404.
		fail(http.StatusNotFound, msg.id, &rpcError{Code: codeNoMethod, Message: "Method not found: " + msg.method})
		return
	}
	if err != nil {
		fail(http.StatusOK, msg.id, h.rpcErr(msg.method, err))
		return
	}
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: msg.id, Result: h.complete(result)})
}

// complete adds what every modern result carries: its type, and who answered.
func (h *Handler) complete(result map[string]any) map[string]any {
	out := maps.Clone(result)
	out["resultType"] = "complete"
	meta, _ := out["_meta"].(map[string]any)
	meta = maps.Clone(meta)
	if meta == nil {
		meta = map[string]any{}
	}
	meta[metaServer] = map[string]any{"name": "tally", "title": "tally", "version": h.Version}
	out["_meta"] = meta
	return out
}

// headerValue reads a mirrored header value, which a client Base64 encodes as =?base64?...?=
// when it is not plain ASCII.
func headerValue(v string) (string, bool) {
	if v == "" {
		return "", false
	}
	if inner, ok := strings.CutPrefix(v, "=?base64?"); ok {
		inner, ok = strings.CutSuffix(inner, "?=")
		if !ok {
			return "", false
		}
		b, err := base64.StdEncoding.DecodeString(inner)
		return string(b), err == nil
	}
	for _, c := range v {
		if c != '\t' && (c < 0x20 || c > 0x7e) {
			return "", false
		}
	}
	return v, true
}

// clientName reads clientInfo from _meta. Clients that leave it out are named by User-Agent.
func clientName(raw json.RawMessage, userAgent string) string {
	var info struct{ Name, Title string }
	if len(raw) > 0 && json.Unmarshal(raw, &info) == nil && (info.Name != "" || info.Title != "") {
		return agentName(info.Name, info.Title)
	}
	if k := knownAgent(userAgent); k != "" {
		return k
	}
	return "Agent"
}

// listen keeps a response open as a stream of the change notifications the client asked for.
// tally has one kind: the tool list changes when the user turns adding time on or off.
func (h *Handler) listen(w http.ResponseWriter, r *http.Request, id, params json.RawMessage) {
	var p struct {
		Notifications struct {
			ToolsListChanged bool `json:"toolsListChanged"`
		} `json:"notifications"`
	}
	if len(params) > 0 && json.Unmarshal(params, &p) != nil {
		writeError(w, http.StatusBadRequest, id, &rpcError{Code: codeInvalidParams, Message: "Invalid subscriptions/listen params"})
		return
	}
	changed, ok := h.hub.join()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, id, &rpcError{Code: codeInternal, Message: "Too many open subscriptions"})
		return
	}
	defer h.hub.leave(changed)

	// The stream outlives the server's read timeout, which is for ordinary requests. Each write
	// gets a few seconds of its own instead, so a client that stops reading frees its slot and
	// cannot hold up a shutdown.
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Time{})
	rc.SetWriteDeadline(time.Now().Add(writeWait))
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sub := map[string]any{metaSubscription: id}
	write := func(s string) bool {
		if rc.SetWriteDeadline(time.Now().Add(writeWait)) != nil {
			return false
		}
		if _, err := io.WriteString(w, s); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	send := func(v any) bool {
		b, _ := json.Marshal(v)
		return write("event: message\ndata: " + string(b) + "\n\n")
	}
	agreed := map[string]any{}
	if p.Notifications.ToolsListChanged {
		agreed["toolsListChanged"] = true
	}
	if !send(map[string]any{"jsonrpc": "2.0", "method": "notifications/subscriptions/acknowledged", "params": map[string]any{"_meta": sub, "notifications": agreed}}) {
		return
	}
	// Ending on purpose answers the listen request, so the client knows it was not a drop.
	end := func() {
		send(rpcResponse{JSONRPC: "2.0", ID: id, Result: map[string]any{"resultType": "complete", "_meta": sub}})
	}

	alive := time.NewTicker(25 * time.Second)
	defer alive.Stop()
	limit := time.NewTimer(time.Hour)
	defer limit.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-changed:
			if p.Notifications.ToolsListChanged && !send(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed", "params": map[string]any{"_meta": sub}}) {
				return
			}
		case <-alive.C:
			// A comment keeps proxies from closing a quiet stream. A rotated token ends it.
			if ok, err := h.tokenOK(r); err == nil && !ok {
				end()
				return
			}
			if !write(":\n\n") {
				return
			}
		case <-limit.C:
			end()
			return
		case <-h.hub.done:
			end()
			return
		}
	}
}

// writeWait bounds one write to a subscription stream.
const writeWait = 5 * time.Second

// hub fans tool list changes out to open subscriptions.
type hub struct {
	mu     sync.Mutex
	subs   map[chan struct{}]bool
	done   chan struct{}
	closed bool
}

func newHub() *hub { return &hub{subs: map[chan struct{}]bool{}, done: make(chan struct{})} }

// At most this many streams are open at once, a few per agent is plenty for one person.
const maxListeners = 32

func (h *hub) join() (chan struct{}, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || len(h.subs) >= maxListeners {
		return nil, false
	}
	ch := make(chan struct{}, 1)
	h.subs[ch] = true
	return ch, true
}

func (h *hub) leave(ch chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, ch)
}

func (h *hub) notify() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default: // one pending notice is enough
		}
	}
}

func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.closed {
		h.closed = true
		close(h.done)
	}
}
