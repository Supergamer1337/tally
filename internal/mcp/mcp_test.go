package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tally/internal/store"
	"tally/internal/tally"
)

type harness struct {
	t     *testing.T
	svc   *tally.Service
	srv   *httptest.Server
	token string
	sess  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "tally.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := tally.New(st, time.UTC)
	svc.Now = func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) }
	token, err := svc.MCPToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandler(svc, "test", slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	return &harness{t: t, svc: svc, srv: srv, token: token}
}

func (h *harness) post(body string) (*http.Response, map[string]any) {
	h.t.Helper()
	req, _ := http.NewRequest("POST", h.srv.URL, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if h.sess != "" {
		req.Header.Set(sessionHeader, h.sess)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	b, _ := io.ReadAll(res.Body)
	json.Unmarshal(b, &out)
	return res, out
}

func (h *harness) initialize() {
	h.t.Helper()
	res, out := h.post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"claude-code","version":"2"}}}`)
	if res.StatusCode != 200 {
		h.t.Fatalf("initialize: %d %v", res.StatusCode, out)
	}
	h.sess = res.Header.Get(sessionHeader)
	if h.sess == "" {
		h.t.Fatal("initialize should open a session")
	}
	if v := out["result"].(map[string]any)["protocolVersion"]; v != "2025-06-18" {
		h.t.Fatalf("should answer with the client's version, got %v", v)
	}
}

func (h *harness) call(name, args string) map[string]any {
	h.t.Helper()
	_, out := h.post(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`)
	r, ok := out["result"].(map[string]any)
	if !ok {
		h.t.Fatalf("%s: no result in %v", name, out)
	}
	return r
}

func toolNames(out map[string]any) []string {
	var names []string
	for _, t := range out["result"].(map[string]any)["tools"].([]any) {
		names = append(names, t.(map[string]any)["name"].(string))
	}
	return names
}

func TestNeedsToken(t *testing.T) {
	h := newHarness(t)
	for _, tok := range []string{"", "tly_wrong"} {
		h.token = tok
		res, _ := h.post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
		if res.StatusCode != http.StatusUnauthorized || res.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("token %q: got %d", tok, res.StatusCode)
		}
	}
}

func TestSessionAndNotifications(t *testing.T) {
	h := newHarness(t)
	h.initialize()
	if res, _ := h.post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); res.StatusCode != http.StatusAccepted {
		t.Errorf("a notification gets 202, got %d", res.StatusCode)
	}
	h.sess = "unknown"
	if res, _ := h.post(`{"jsonrpc":"2.0","id":3,"method":"ping"}`); res.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown session gets 404, got %d", res.StatusCode)
	}
	h.sess = ""
	if _, out := h.post(`{"jsonrpc":"2.0","id":4,"method":"nope"}`); out["error"].(map[string]any)["code"].(float64) != codeNoMethod {
		t.Errorf("unknown method: %v", out)
	}
	if res, out := h.post(`{not json`); res.StatusCode != http.StatusBadRequest || out["error"].(map[string]any)["code"].(float64) != codeParse {
		t.Errorf("bad json: %d %v", res.StatusCode, out)
	}
}

func TestReadTools(t *testing.T) {
	h := newHarness(t)
	h.initialize()
	ctx := context.Background()
	h.svc.Add(ctx, "2026-10-12", 90, "Deploy")
	h.svc.Add(ctx, "2026-10-14", -30, "")

	_, out := h.post(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if got := strings.Join(toolNames(out), ","); got != "get_balance,list_days,get_trend" {
		t.Fatalf("tools while read only: %s", got)
	}
	r := h.call("get_balance", `{}`)
	sc := r["structuredContent"].(map[string]any)
	if r["isError"] != false || sc["balance"].(map[string]any)["text"] != "+1h" || sc["week"].(map[string]any)["minutes"].(float64) != 60 {
		t.Fatalf("get_balance: %v", r)
	}
	r = h.call("list_days", `{"from":"2026-10-13"}`)
	days := r["structuredContent"].(map[string]any)["days"].([]any)
	if len(days) != 1 || days[0].(map[string]any)["balance"].(map[string]any)["minutes"].(float64) != 60 {
		t.Fatalf("list_days: %v", r)
	}
	if r = h.call("list_days", `{"from":"yesterday"}`); r["isError"] != true {
		t.Fatalf("a bad date is a tool error: %v", r)
	}
	r = h.call("get_trend", `{}`)
	if weeks := r["structuredContent"].(map[string]any)["weeks"].([]any); len(weeks) != 1 {
		t.Fatalf("get_trend: %v", r)
	}
	agents, _ := h.svc.Store.Agents(ctx)
	if len(agents) != 1 || agents[0].Name != "Claude" || agents[0].Tool != "get_trend" {
		t.Fatalf("agents: %+v", agents)
	}
}

func TestAddDayOnlyWhenAllowed(t *testing.T) {
	h := newHarness(t)
	h.initialize()
	ctx := context.Background()
	if r := h.call("add_day", `{"minutes":30}`); r["isError"] != true {
		t.Fatalf("add_day while off should fail: %v", r)
	}
	h.svc.SetAgentsCanWrite(ctx, true)
	_, out := h.post(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if got := strings.Join(toolNames(out), ","); !strings.HasSuffix(got, ",add_day") {
		t.Fatalf("tools while on: %s", got)
	}
	r := h.call("add_day", `{"minutes":30,"note":"Late"}`)
	if r["isError"] != false || r["structuredContent"].(map[string]any)["balance"].(map[string]any)["minutes"].(float64) != 30 {
		t.Fatalf("add_day: %v", r)
	}
	if r = h.call("add_day", `{"minutes":15,"date":"2026-10-19"}`); r["isError"] != true {
		t.Fatalf("a future day should fail: %v", r)
	}
	if r = h.call("add_day", `{"note":"x"}`); r["isError"] != true {
		t.Fatalf("minutes is required: %v", r)
	}
	all, _ := h.svc.Entries(ctx)
	if len(all) != 1 || all[0].Date != "2026-10-18" || all[0].Note != "Late" {
		t.Fatalf("entries: %+v", all)
	}
}

func TestBatch(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest("POST", h.srv.URL, strings.NewReader(`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/x"},{"jsonrpc":"2.0","id":2,"method":"ping"}]`))
	req.Header.Set("Authorization", "Bearer "+h.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out []map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	if len(out) != 2 {
		t.Fatalf("batch of two requests and a notification: %v", out)
	}
}

func (h *harness) raw(body string, headers ...string) (*http.Response, string) {
	h.t.Helper()
	req, _ := http.NewRequest("POST", h.srv.URL, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+h.token)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func TestInvalidMessagesNeverRun(t *testing.T) {
	h := newHarness(t)
	h.svc.SetAgentsCanWrite(context.Background(), true)
	add := `"method":"tools/call","params":{"name":"add_day","arguments":{"minutes":15}}`
	for _, body := range []string{
		`{"jsonrpc":"1.0","id":1,` + add + `}`,
		`{"jsonrpc":"2.0","id":{"x":1},` + add + `}`,
		`{"jsonrpc":"2.0","id":null,` + add + `}`,
		`{"id":1,` + add + `}`,
	} {
		if res, got := h.raw(body); res.StatusCode != http.StatusBadRequest || !strings.Contains(got, "-32600") {
			t.Errorf("%s: %d %s", body, res.StatusCode, got)
		}
	}
	for _, body := range []string{`{}`, `null`, `5`, `"x"`} {
		if res, got := h.raw(body); res.StatusCode != http.StatusBadRequest || !strings.Contains(got, "-32600") {
			t.Errorf("%s: %d %s", body, res.StatusCode, got)
		}
	}
	if all, _ := h.svc.Entries(context.Background()); len(all) != 0 {
		t.Fatalf("an invalid message changed data: %+v", all)
	}
}

func TestProtocolVersionHeader(t *testing.T) {
	h := newHarness(t)
	if res, _ := h.raw(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, "MCP-Protocol-Version", "2099-01-01"); res.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown version: %d", res.StatusCode)
	}
	if res, _ := h.raw(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, "MCP-Protocol-Version", "2025-06-18"); res.StatusCode != 200 {
		t.Errorf("known version: %d", res.StatusCode)
	}
	if res, _ := h.raw(`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, "MCP-Protocol-Version", "2025-06-18"); res.StatusCode != http.StatusBadRequest {
		t.Errorf("batches went away in 2025-06-18: %d", res.StatusCode)
	}
}

func TestUnknownArgumentsAreRefused(t *testing.T) {
	h := newHarness(t)
	h.svc.SetAgentsCanWrite(context.Background(), true)
	if r := h.call("add_day", `{"minutes":15,"data":"2026-10-01"}`); r["isError"] != true {
		t.Fatalf("a misspelt argument must not fall back to today: %v", r)
	}
	if r := h.call("get_balance", `{"x":1}`); r["isError"] != true {
		t.Fatalf("get_balance takes no arguments: %v", r)
	}
	if all, _ := h.svc.Entries(context.Background()); len(all) != 0 {
		t.Fatalf("entries: %+v", all)
	}
}

func TestBearerSchemeIgnoresCase(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest("POST", h.srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", "bearer "+h.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("lower case bearer: %d", res.StatusCode)
	}
}

func TestAgentNames(t *testing.T) {
	for in, want := range map[string]string{
		"claude-code":              "Claude",
		"Cl\u200baude":             "Claude",
		"my-agent\u202e\nline two": "my-agent line two",
		"":                         "Agent",
		strings.Repeat("x", 40):    strings.Repeat("x", 32),
	} {
		if got := agentName(in, ""); got != want {
			t.Errorf("agentName(%q) = %q, want %q", in, got, want)
		}
	}
}

// modern sends a 2026-07-28 request the way a conforming client does: headers mirrored from
// the body, and _meta with the version, client and capabilities.
func (h *harness) modern(method string, params map[string]any, headers ...string) (*http.Response, map[string]any) {
	h.t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	if _, ok := params["_meta"]; !ok {
		params["_meta"] = map[string]any{
			"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
			"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "claude-code", "version": "2.1"},
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		}
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params})
	req, _ := http.NewRequest("POST", h.srv.URL, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", method)
	if name, ok := params["name"].(string); ok {
		req.Header.Set("Mcp-Name", name)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "" {
			req.Header.Del(headers[i])
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res, out
}

func errCode(out map[string]any) float64 {
	e, _ := out["error"].(map[string]any)
	c, _ := e["code"].(float64)
	return c
}

func TestModernDiscoverAndTools(t *testing.T) {
	h := newHarness(t)
	h.svc.Add(context.Background(), "2026-10-12", 90, "Deploy")

	res, out := h.modern("server/discover", nil)
	r := out["result"].(map[string]any)
	if res.StatusCode != 200 || r["resultType"] != "complete" || r["ttlMs"] == nil || r["cacheScope"] != "public" {
		t.Fatalf("server/discover: %d %v", res.StatusCode, out)
	}
	if v := r["supportedVersions"].([]any); v[0] != "2026-07-28" || len(v) < 2 {
		t.Fatalf("supportedVersions: %v", v)
	}
	if r["capabilities"].(map[string]any)["tools"].(map[string]any)["listChanged"] != true {
		t.Fatalf("capabilities: %v", r["capabilities"])
	}
	if r["_meta"].(map[string]any)["io.modelcontextprotocol/serverInfo"].(map[string]any)["name"] != "tally" {
		t.Fatalf("serverInfo: %v", r["_meta"])
	}

	_, out = h.modern("tools/list", nil)
	r = out["result"].(map[string]any)
	if r["resultType"] != "complete" || r["cacheScope"] != "private" || r["ttlMs"].(float64) <= 0 || len(r["tools"].([]any)) != 3 {
		t.Fatalf("tools/list: %v", out)
	}

	_, out = h.modern("tools/call", map[string]any{"name": "get_balance", "arguments": map[string]any{}})
	r = out["result"].(map[string]any)
	if r["resultType"] != "complete" || r["isError"] != false || r["structuredContent"].(map[string]any)["balance"].(map[string]any)["text"] != "+1h 30m" {
		t.Fatalf("tools/call: %v", out)
	}
	// No session: who called is read from clientInfo on each request.
	agents, _ := h.svc.Store.Agents(context.Background())
	if len(agents) != 1 || agents[0].Name != "Claude" || agents[0].Tool != "get_balance" {
		t.Fatalf("agents: %+v", agents)
	}
}

func TestModernRequestsAreChecked(t *testing.T) {
	h := newHarness(t)
	call := map[string]any{"name": "get_balance", "arguments": map[string]any{}}
	clone := func(m map[string]any) map[string]any {
		b, _ := json.Marshal(m)
		var c map[string]any
		json.Unmarshal(b, &c)
		return c
	}
	cases := []struct {
		name    string
		method  string
		params  map[string]any
		headers []string
		status  int
		code    float64
	}{
		{"Mcp-Method missing", "tools/list", nil, []string{"Mcp-Method", ""}, 400, -32020},
		{"Mcp-Method differs", "tools/list", nil, []string{"Mcp-Method", "tools/call"}, 400, -32020},
		{"Mcp-Name missing", "tools/call", clone(call), []string{"Mcp-Name", ""}, 400, -32020},
		{"Mcp-Name differs", "tools/call", clone(call), []string{"Mcp-Name", "add_day"}, 400, -32020},
		{"Mcp-Name in Base64", "tools/call", clone(call), []string{"Mcp-Name", "=?base64?Z2V0X2JhbGFuY2U=?="}, 200, 0},
		{"no _meta", "tools/list", map[string]any{"_meta": map[string]any{}}, nil, 400, -32602},
		{"body names another version", "tools/list", map[string]any{"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion": "2025-11-25", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}}, nil, 400, -32020},
		{"unknown version", "tools/list", nil, []string{"MCP-Protocol-Version", "2099-01-01"}, 400, -32022},
		{"ping is gone", "ping", nil, nil, 404, -32601},
		{"initialize is gone", "initialize", nil, nil, 404, -32601},
	}
	for _, c := range cases {
		res, out := h.modern(c.method, c.params, c.headers...)
		if res.StatusCode != c.status || errCode(out) != c.code {
			t.Errorf("%s: %d %v, want %d %v", c.name, res.StatusCode, out, c.status, c.code)
		}
	}
	// The unsupported version error lists what is supported, so a client can retry.
	_, out := h.modern("tools/list", nil, "MCP-Protocol-Version", "2099-01-01")
	if data := out["error"].(map[string]any)["data"].(map[string]any); data["requested"] != "2099-01-01" || len(data["supported"].([]any)) == 0 {
		t.Fatalf("data: %v", data)
	}
	// A modern body without the header is a mismatch, not a legacy request.
	_, out = h.modern("tools/list", nil, "MCP-Protocol-Version", "")
	if errCode(out) != -32020 {
		t.Fatalf("modern body without header: %v", out)
	}
	if res, _ := h.raw(`[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`, "MCP-Protocol-Version", "2026-07-28"); res.StatusCode != 400 {
		t.Fatalf("batch: %d", res.StatusCode)
	}
}

// Legacy clients still get a session from initialize, which the modern path never mints.
func TestBothEras(t *testing.T) {
	h := newHarness(t)
	h.initialize()
	if res, _ := h.modern("tools/list", nil, "Mcp-Session-Id", h.sess); res.StatusCode != 200 || res.Header.Get(sessionHeader) != "" {
		t.Fatalf("modern request with a stray session id: %d %q", res.StatusCode, res.Header.Get(sessionHeader))
	}
	if _, out := h.post(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`); out["result"] == nil {
		t.Fatalf("legacy tools/list after a modern request: %v", out)
	}
}

func TestListenHearsTheSwitch(t *testing.T) {
	h := newHarness(t)
	handler := h.srv.Config.Handler.(*Handler)
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "sub-1", "method": "subscriptions/listen", "params": map[string]any{
		"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		},
		"notifications": map[string]any{"toolsListChanged": true, "promptsListChanged": true},
	}})
	req, _ := http.NewRequest("POST", h.srv.URL, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "subscriptions/listen")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("listen: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	events := make(chan map[string]any, 4)
	go func() {
		buf := make([]byte, 4096)
		var pending string
		for {
			n, err := res.Body.Read(buf)
			pending += string(buf[:n])
			for {
				i := strings.Index(pending, "\n\n")
				if i < 0 {
					break
				}
				for _, line := range strings.Split(pending[:i], "\n") {
					if data, ok := strings.CutPrefix(line, "data: "); ok {
						var m map[string]any
						json.Unmarshal([]byte(data), &m)
						events <- m
					}
				}
				pending = pending[i+2:]
			}
			if err != nil {
				close(events)
				return
			}
		}
	}()
	next := func() map[string]any {
		select {
		case m := <-events:
			return m
		case <-time.After(3 * time.Second):
			t.Fatal("no event")
			return nil
		}
	}
	ack := next()
	params := ack["params"].(map[string]any)
	if ack["method"] != "notifications/subscriptions/acknowledged" || params["_meta"].(map[string]any)["io.modelcontextprotocol/subscriptionId"] != "sub-1" {
		t.Fatalf("ack: %v", ack)
	}
	// Only what tally can send is agreed to.
	if n := params["notifications"].(map[string]any); n["toolsListChanged"] != true || n["promptsListChanged"] != nil {
		t.Fatalf("agreed: %v", n)
	}
	handler.ToolsChanged()
	if n := next(); n["method"] != "notifications/tools/list_changed" {
		t.Fatalf("change: %v", n)
	}
	// Shutting down answers the listen request before the stream closes.
	handler.Close()
	if end := next(); end["id"] != "sub-1" || end["result"].(map[string]any)["resultType"] != "complete" {
		t.Fatalf("end: %v", end)
	}
}

func TestModernShapedRequestsNeverRunTheLegacyWay(t *testing.T) {
	h := newHarness(t)
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"%s","io.modelcontextprotocol/clientCapabilities":{}}`
	for _, c := range []struct{ name, header, body string }{
		{"in a batch, no header", "", `[{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + fmt.Sprintf(meta, "2026-07-28") + `}}]`},
		{"future version, legacy header", "2025-11-25", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + fmt.Sprintf(meta, "2099-01-01") + `}}`},
		{"modern body, legacy header", "2025-11-25", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + fmt.Sprintf(meta, "2026-07-28") + `}}`},
	} {
		res, body := h.raw(c.body, "MCP-Protocol-Version", c.header)
		if res.StatusCode != http.StatusBadRequest || strings.Contains(body, `"result"`) {
			t.Errorf("%s: %d %s", c.name, res.StatusCode, body)
		}
	}
}

func TestVersionErrorsKeepTheID(t *testing.T) {
	h := newHarness(t)
	_, body := h.raw(`{"jsonrpc":"2.0","id":"review-73","method":"tools/list"}`, "MCP-Protocol-Version", "2099-01-01")
	if !strings.Contains(body, `"id":"review-73"`) || !strings.Contains(body, "-32022") {
		t.Fatalf("unsupported version: %s", body)
	}
	// An id that cannot be read is left out, not sent as null.
	_, body = h.raw(`{not json`, "MCP-Protocol-Version", "2026-07-28")
	if strings.Contains(body, `"id"`) {
		t.Fatalf("parse error: %s", body)
	}
}
