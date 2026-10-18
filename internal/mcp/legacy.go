package mcp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"time"
)

const sessionHeader = "Mcp-Session-Id"

// legacy serves the protocol versions up to 2025-11-25: a session opened by initialize, and
// batches before 2025-06-18. Without the version header the transport says to assume 2025-03-26.
func (h *Handler) legacy(w http.ResponseWriter, r *http.Request, version string, trimmed []byte) {
	batch := trimmed[0] == '['
	var msgs []json.RawMessage
	if batch {
		// Batches were dropped from the protocol in 2025-06-18.
		if version >= "2025-06-18" {
			writeJSON(w, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: codeInvalidRequest, Message: "Batches are not part of protocol version " + version}})
			return
		}
		if err := json.Unmarshal(trimmed, &msgs); err != nil || len(msgs) == 0 {
			writeJSON(w, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: codeInvalidRequest, Message: "Invalid request"}})
			return
		}
	} else {
		msgs = []json.RawMessage{trimmed}
	}

	c := &call{ctx: r.Context(), session: r.Header.Get(sessionHeader)}
	if c.session != "" {
		client, ok, err := h.Svc.Store.MCPSession(c.ctx, c.session, time.Now())
		if err != nil {
			h.Log.Error("mcp: read session", "err", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		if !ok {
			// The transport says an unknown session gets 404, and the client starts a new one.
			writeJSON(w, http.StatusNotFound, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32001, Message: "Session not found, initialize again"}})
			return
		}
		c.agent = client
	} else {
		c.agent = knownAgent(r.UserAgent())
		if c.agent == "" {
			c.agent = "Agent"
		}
	}

	var replies []rpcResponse
	for _, m := range msgs {
		if res, ok := h.handle(c, m); ok {
			replies = append(replies, res)
		}
	}
	if c.newID != "" {
		w.Header().Set(sessionHeader, c.newID)
	}
	switch {
	case len(replies) == 0:
		// Only notifications or responses came in. They get an empty 202.
		w.WriteHeader(http.StatusAccepted)
	case batch:
		writeJSON(w, http.StatusOK, replies)
	case replies[0].Error != nil && replies[0].Error.Code == codeInvalidRequest:
		// A single message that is not valid JSON-RPC is input the server cannot accept.
		writeJSON(w, http.StatusBadRequest, replies[0])
	default:
		writeJSON(w, http.StatusOK, replies[0])
	}
}

// handle answers one message. ok is false for messages that get no answer.
func (h *Handler) handle(c *call, raw json.RawMessage) (res rpcResponse, ok bool) {
	msg, bad := parse(raw)
	if bad != nil {
		return *bad, true
	}
	if msg.response || !msg.hasID {
		// Responses (this server never asks the client anything) and notifications, such as
		// notifications/initialized, need nothing back.
		return rpcResponse{}, false
	}
	req := rpcRequest{ID: msg.id, Method: msg.method, Params: msg.params}
	h.Log.Debug("mcp", "protocol", "legacy", "method", req.Method, "agent", c.agent)
	result, err := h.dispatch(c, req)
	if err != nil {
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: h.rpcErr(req.Method, err)}, true
	}
	return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}, true
}

func (h *Handler) dispatch(c *call, req rpcRequest) (any, error) {
	switch req.Method {
	case "initialize":
		return h.initialize(c, req.Params)
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return h.listTools(c.ctx)
	case "tools/call":
		return h.callTool(c, req.Params)
	case "resources/list":
		return map[string]any{"resources": []any{}}, nil
	case "resources/templates/list":
		return map[string]any{"resourceTemplates": []any{}}, nil
	case "prompts/list":
		return map[string]any{"prompts": []any{}}, nil
	default:
		return nil, &rpcError{Code: codeNoMethod, Message: "Method not found: " + req.Method}
	}
}

func (h *Handler) initialize(c *call, params json.RawMessage) (any, error) {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
		ClientInfo      struct {
			Name  string `json:"name"`
			Title string `json:"title"`
		} `json:"clientInfo"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: "Invalid initialize params"}
		}
	}
	version := legacyVersions[0]
	if slices.Contains(legacyVersions, p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	c.agent = agentName(p.ClientInfo.Name, p.ClientInfo.Title)
	if c.session == "" {
		c.newID = newSessionID()
		if err := h.Svc.Store.CreateMCPSession(c.ctx, c.newID, c.agent, time.Now()); err != nil {
			return nil, err
		}
	}
	if err := h.Svc.Store.SeeAgent(c.ctx, c.agent, "", time.Now()); err != nil {
		h.Log.Error("mcp: record agent", "err", err)
	}
	return map[string]any{
		"protocolVersion": version,
		// Legacy sessions get no change notifications: that took a GET stream, which is not offered.
		"capabilities": map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":   map[string]any{"name": "tally", "title": "tally", "version": h.Version},
		"instructions": instructions,
	}, nil
}

func newSessionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
