// Package mcp lets AI agents read tally, and add to it when the user allows that, over the
// Model Context Protocol on the Streamable HTTP transport. It speaks the stateless protocol of
// 2026-07-28 (modern.go) and, for clients that still open a session with initialize, the
// versions before it (legacy.go). Both share the tools in tools.go.
package mcp

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"tally/internal/tally"
)

// Versions this server speaks, newest first. Modern ones carry everything in each request,
// legacy ones open a session with initialize.
var (
	modernVersions = []string{"2026-07-28"}
	legacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}
	allVersions    = append(slices.Clone(modernVersions), legacyVersions...)
)

type Handler struct {
	Svc     *tally.Service
	Version string
	Log     *slog.Logger
	hub     *hub
	calls   bucket
}

func NewHandler(svc *tally.Service, version string, log *slog.Logger) *Handler {
	return &Handler{Svc: svc, Version: version, Log: log, hub: newHub(), calls: bucket{size: 60, per: time.Second}}
}

// ToolsChanged tells agents listening for it that the list of tools changed, as it does when
// the user turns adding time on or off.
func (h *Handler) ToolsChanged() { h.hub.notify() }

// Close ends every open subscription, for a graceful shutdown.
func (h *Handler) Close() { h.hub.close() }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	switch {
	case r.Method == http.MethodPost:
		h.post(w, r)
	case r.Method == http.MethodDelete && r.Header.Get(sessionHeader) != "":
		// A legacy client is done with its session.
		if err := h.Svc.Store.DeleteMCPSession(r.Context(), r.Header.Get(sessionHeader)); err != nil {
			h.Log.Error("mcp: end session", "err", err)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		// No GET stream: change notifications come over subscriptions/listen.
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) tokenOK(r *http.Request) (bool, error) {
	want, err := h.Svc.MCPToken(r.Context())
	if err != nil {
		return false, err
	}
	scheme, got, _ := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	return strings.EqualFold(scheme, "Bearer") && want != "" && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(want)) == 1, nil
}

func (h *Handler) authorized(w http.ResponseWriter, r *http.Request) bool {
	ok, err := h.tokenOK(r)
	if err != nil {
		h.Log.Error("mcp: read token", "err", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return false
	}
	if ok {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="tally"`)
	writeJSON(w, http.StatusUnauthorized, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: codeUnauthorized, Message: "Missing or wrong token. Copy it from the Agents screen in tally."}})
	return false
}

// post picks the era from the version a request names and hands it on.
func (h *Handler) post(w http.ResponseWriter, r *http.Request) {
	version := r.Header.Get("MCP-Protocol-Version")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, nil, &rpcError{Code: codeInvalidRequest, Message: "Request too large"})
		return
	}
	trimmed := bytes.TrimSpace(body)
	if !json.Valid(trimmed) {
		// Input the server cannot accept gets an HTTP error, with a JSON-RPC error that has no id.
		writeError(w, http.StatusBadRequest, nil, &rpcError{Code: codeParse, Message: "Parse error"})
		return
	}
	// The body is read first, so errors about the version can carry the request's id and a
	// client can match them to what it sent.
	id, metaVersions := scan(trimmed)
	if version != "" && !slices.Contains(allVersions, version) {
		writeError(w, http.StatusBadRequest, id, unsupported(version))
		return
	}
	if slices.Contains(modernVersions, version) {
		h.modern(w, r, version, trimmed)
		return
	}
	// Only modern requests carry their protocol version in _meta. One that does is never served
	// the legacy way: its header has to name that same modern version.
	if len(metaVersions) > 0 {
		v := metaVersions[0]
		if v != "" && !slices.Contains(allVersions, v) {
			writeError(w, http.StatusBadRequest, id, unsupported(v))
			return
		}
		writeError(w, http.StatusBadRequest, id, &rpcError{Code: codeHeaderMismatch,
			Message: "Header mismatch: MCP-Protocol-Version header is " + quote(version) + " but the body names " + quote(v)})
		return
	}
	h.legacy(w, r, version, trimmed)
}

// errorResponse is an error answer. An id that could not be read is left out, as the modern
// schema has it, rather than sent as null.
type errorResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Error   *rpcError       `json:"error"`
}

func writeError(w http.ResponseWriter, status int, id json.RawMessage, e *rpcError) {
	if !validID(id) {
		id = nil
	}
	writeJSON(w, status, errorResponse{JSONRPC: "2.0", ID: id, Error: e})
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeNoMethod       = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	// Defined by MCP 2026-07-28.
	codeHeaderMismatch = -32020
	codeUnsupported    = -32022
	// tally's own, outside the range JSON-RPC and MCP reserve.
	codeUnauthorized = -31001
)

func unsupported(requested string) *rpcError {
	return &rpcError{Code: codeUnsupported, Message: "Unsupported protocol version",
		Data: map[string]any{"supported": allVersions, "requested": requested}}
}

// rpcErr turns an error from a method into what the client sees. Anything that is not already
// a JSON-RPC error is a failure on this side, logged and not shown.
func (h *Handler) rpcErr(method string, err error) *rpcError {
	var re *rpcError
	if !errors.As(err, &re) {
		h.Log.Error("mcp: "+method, "err", err)
		re = &rpcError{Code: codeInternal, Message: "Internal error"}
	}
	return re
}

// call carries what one request knows about its caller.
type call struct {
	ctx     context.Context
	session string
	agent   string
	newID   string // set when a legacy initialize opened a session
}

// message is one JSON-RPC message that passed the checks in parse.
type message struct {
	id       json.RawMessage
	hasID    bool
	method   string
	params   json.RawMessage
	response bool
}

// parse checks the JSON-RPC envelope of one message. bad is the error to answer with when it
// is not valid.
func parse(raw json.RawMessage) (msg message, bad *rpcResponse) {
	invalid := func(id json.RawMessage) (message, *rpcResponse) {
		return message{}, &rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: codeInvalidRequest, Message: "Invalid request"}}
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return invalid(nil)
	}
	id, hasID := m["id"]
	// MCP ids are strings or numbers, never null.
	if hasID && !validID(id) {
		return invalid(nil)
	}
	if string(m["jsonrpc"]) != `"2.0"` {
		return invalid(id)
	}
	rawMethod, hasMethod := m["method"]
	if !hasMethod {
		_, isResult := m["result"]
		_, isError := m["error"]
		if (isResult && hasID) || isError {
			return message{id: id, hasID: hasID, response: true}, nil
		}
		return invalid(id)
	}
	msg = message{id: id, hasID: hasID}
	if err := json.Unmarshal(rawMethod, &msg.method); err != nil || msg.method == "" {
		return invalid(id)
	}
	if p, ok := m["params"]; ok {
		if len(p) == 0 || (p[0] != '{' && string(p) != "null") {
			return invalid(id)
		}
		msg.params = p
	}
	return msg, nil
}

func validID(id json.RawMessage) bool {
	var v any
	if json.Unmarshal(id, &v) != nil {
		return false
	}
	switch v.(type) {
	case string, float64:
		return true
	}
	return false
}

// scan reads what routing needs from a body: the id of a single message, and the protocol
// version every message names in params._meta. A version key that is there but not a string
// counts too, as "".
func scan(body []byte) (id json.RawMessage, versions []string) {
	msgs := []json.RawMessage{body}
	if body[0] == '[' {
		msgs = nil
		json.Unmarshal(body, &msgs)
	}
	for _, m := range msgs {
		var env struct {
			ID     json.RawMessage `json:"id"`
			Params struct {
				Meta map[string]json.RawMessage `json:"_meta"`
			} `json:"params"`
		}
		if json.Unmarshal(m, &env) != nil {
			continue
		}
		if body[0] == '{' && validID(env.ID) {
			id = env.ID
		}
		if raw, ok := env.Params.Meta[metaVersion]; ok {
			var v string
			json.Unmarshal(raw, &v)
			versions = append(versions, v)
		}
	}
	return id, versions
}

func quote(s string) string {
	if s == "" {
		return "missing"
	}
	return `"` + s + `"`
}

// bucket allows size calls at once, then one per per. Tool calls go through it, so a runaway
// agent cannot hammer the database.
type bucket struct {
	mu     sync.Mutex
	size   int
	per    time.Duration
	tokens float64
	last   time.Time
}

func (b *bucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if b.last.IsZero() {
		b.tokens = float64(b.size)
	} else {
		b.tokens = min(float64(b.size), b.tokens+float64(now.Sub(b.last))/float64(b.per))
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

const instructions = `tally tracks working time that differs from a normal day. Each entry is one day, with signed minutes against a normal day (positive means more was worked, negative less) and an optional note of at most 40 characters. Only days that differed are logged, so a day with no entry was a normal day. The balance is the sum of every entry up to today. Dates are YYYY-MM-DD in the user's time zone, and every tool result says what today is.`

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// agentName turns a client's self description into the short name the Agents screen shows.
func agentName(name, title string) string {
	if k := knownAgent(name + " " + title); k != "" {
		return k
	}
	for _, s := range []string{title, name} {
		if s = cleanName(s); s != "" {
			return s
		}
	}
	return "Agent"
}

// cleanName keeps a client's name to one plain line of at most 32 characters. Control, zero
// width and direction marks go, so one agent cannot pass itself off as another.
func cleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 32 {
		s = strings.TrimSpace(string(r[:32]))
	}
	return s
}

// knownAgent names the agents people are likely to use, or returns "".
func knownAgent(s string) string {
	known := []struct{ match, name string }{
		{"claude", "Claude"}, {"codex", "Codex"}, {"cursor", "Cursor"}, {"gemini", "Gemini"},
		{"chatgpt", "ChatGPT"}, {"openai", "ChatGPT"}, {"copilot", "Copilot"}, {"vscode", "VS Code"},
		{"windsurf", "Windsurf"}, {"zed", "Zed"}, {"goose", "Goose"}, {"opencode", "opencode"},
		{"inspector", "MCP Inspector"},
	}
	lower := strings.ToLower(s)
	for _, k := range known {
		if strings.Contains(lower, k.match) {
			return k.name
		}
	}
	return ""
}
