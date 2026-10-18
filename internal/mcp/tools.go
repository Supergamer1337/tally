package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"tally/internal/tally"
)

type tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations"`
	write       bool
	run         func(h *Handler, ctx context.Context, args json.RawMessage) (any, error)
}

func dateProp(desc string) map[string]any {
	return map[string]any{"type": "string", "format": "date", "pattern": `^\d{4}-\d{2}-\d{2}$`, "description": desc}
}

func object(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func readOnly(title string) map[string]any {
	return map[string]any{"title": title, "readOnlyHint": true, "openWorldHint": false}
}

var tools = []tool{
	{
		Name:        "get_balance",
		Title:       "Balance now",
		Description: "The balance now, which is every logged minute up to today added together, and how much it changed this week (from Monday), this month, this quarter and this year. Also says what today is and how many days are logged.",
		InputSchema: object(map[string]any{}),
		Annotations: readOnly("Balance now"),
		run: func(h *Handler, ctx context.Context, raw json.RawMessage) (any, error) {
			if err := decodeArgs(raw, &struct{}{}); err != nil {
				return nil, err
			}
			return h.Svc.Summary(ctx)
		},
	},
	{
		Name:        "list_days",
		Title:       "Days in a range",
		Description: "Logged days in a range, oldest first. Each has its signed minutes, its note and the balance at the end of that day. Days that are not listed were normal days. Defaults to the last 30 days.",
		InputSchema: object(map[string]any{
			"from": dateProp("First day, YYYY-MM-DD. Defaults to 30 days before to."),
			"to":   dateProp("Last day, YYYY-MM-DD. Defaults to today."),
		}),
		Annotations: readOnly("Days in a range"),
		run: func(h *Handler, ctx context.Context, raw json.RawMessage) (any, error) {
			var a struct{ From, To string }
			if err := decodeArgs(raw, &a); err != nil {
				return nil, err
			}
			list, err := h.Svc.Days(ctx, a.From, a.To)
			if err != nil {
				return nil, err
			}
			return struct {
				Today string `json:"today"`
				tally.DayList
			}{h.Svc.Today(), list}, nil
		},
	},
	{
		Name:        "get_trend",
		Title:       "Balance by week",
		Description: "The balance at the end of each ISO week (Monday to Sunday), with that week's net change and its more and less parts. Defaults to the last 12 weeks.",
		InputSchema: object(map[string]any{
			"from": dateProp("A day in the first week, YYYY-MM-DD. Defaults to 12 weeks before to."),
			"to":   dateProp("A day in the last week, YYYY-MM-DD. Defaults to today."),
		}),
		Annotations: readOnly("Balance by week"),
		run: func(h *Handler, ctx context.Context, raw json.RawMessage) (any, error) {
			var a struct{ From, To string }
			if err := decodeArgs(raw, &a); err != nil {
				return nil, err
			}
			trend, err := h.Svc.Trend(ctx, a.From, a.To)
			if err != nil {
				return nil, err
			}
			return struct {
				Today string `json:"today"`
				tally.TrendList
			}{h.Svc.Today(), trend}, nil
		},
	},
	{
		Name:  "add_day",
		Title: "Add time",
		Description: "Add time to a day, the same way the user does in the app. minutes is signed against a normal day: positive when more was worked, negative when less. " +
			"If the day already has an entry the minutes are added to it, so to correct a day add the difference. A note replaces the day's note, leaving it out keeps the old one. A day that reaches 0 is removed. " +
			"Only today and past days. Confirm with the user before adding unless they asked for it.",
		InputSchema: object(map[string]any{
			"minutes": map[string]any{"type": "integer", "minimum": -tally.MaxAdd, "maximum": tally.MaxAdd, "description": "Signed minutes, for example 90 for 1h 30m more or -45 for 45m less."},
			"date":    dateProp("The day, YYYY-MM-DD. Defaults to today."),
			"note":    map[string]any{"type": "string", "maxLength": tally.MaxNote, "description": "Optional short note, at most 40 characters."},
		}, "minutes"),
		Annotations: map[string]any{"title": "Add time", "readOnlyHint": false, "destructiveHint": false, "idempotentHint": false, "openWorldHint": false},
		write:       true,
		run: func(h *Handler, ctx context.Context, raw json.RawMessage) (any, error) {
			var a struct {
				Minutes *int
				Date    string
				Note    string
			}
			if err := decodeArgs(raw, &a); err != nil {
				return nil, err
			}
			if a.Minutes == nil {
				return nil, invalidArg("minutes is required")
			}
			if a.Date == "" {
				a.Date = h.Svc.Today()
			}
			ch, err := h.Svc.Add(ctx, a.Date, *a.Minutes, a.Note)
			if err != nil {
				return nil, err
			}
			sum, err := h.Svc.Summary(ctx)
			if err != nil {
				return nil, err
			}
			day := map[string]any{"date": a.Date, "minutes": 0, "text": tally.Format(0), "removed": true}
			if ch.After != nil {
				day = map[string]any{"date": a.Date, "minutes": ch.After.Minutes, "text": tally.Format(ch.After.Minutes), "note": ch.After.Note}
			}
			return map[string]any{"today": sum.Today, "added": map[string]any{"minutes": *a.Minutes, "text": tally.Format(*a.Minutes)}, "day": day, "balance": sum.Balance}, nil
		},
	},
}

func decodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	// Unknown names are refused, so a typo like "data" for "date" cannot quietly mean today.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalidArg("arguments do not match the input schema: " + strings.TrimPrefix(err.Error(), "json: "))
	}
	return nil
}

func invalidArg(msg string) error { return toolError(msg) }

// toolError is a mistake in the arguments. It goes back to the agent as a tool result with
// isError set, so the agent can read it and try again.
type toolError string

func (e toolError) Error() string { return string(e) }

// listTools lists the tools in a fixed order, so clients and prompt caches see the same list
// until it really changes.
func (h *Handler) listTools(ctx context.Context) (map[string]any, error) {
	write, err := h.Svc.AgentsCanWrite(ctx)
	if err != nil {
		return nil, err
	}
	list := []tool{}
	for _, t := range tools {
		if !t.write || write {
			list = append(list, t)
		}
	}
	return map[string]any{"tools": list}, nil
}

func (h *Handler) callTool(c *call, params json.RawMessage) (map[string]any, error) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "Invalid tools/call params"}
	}
	var t *tool
	for i := range tools {
		if tools[i].Name == p.Name {
			t = &tools[i]
		}
	}
	if t == nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "Unknown tool: " + p.Name}
	}
	if !h.calls.allow() {
		return errorResult("Too many calls. Wait a minute and try again."), nil
	}
	if t.write {
		on, err := h.Svc.AgentsCanWrite(c.ctx)
		if err != nil {
			return nil, err
		}
		if !on {
			return errorResult("Adding time is off. The user can turn on Let agents add time on the Agents screen in tally."), nil
		}
	}
	if err := h.Svc.Store.SeeAgent(c.ctx, c.agent, t.Name, time.Now()); err != nil {
		h.Log.Error("mcp: record agent", "err", err)
	}
	out, err := t.run(h, c.ctx, p.Arguments)
	var te toolError
	switch {
	case errors.As(err, &te):
		return errorResult(te.Error()), nil
	case errors.Is(err, tally.ErrInvalid):
		return errorResult(tally.Message(err)), nil
	case err != nil:
		return nil, err
	}
	text, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(text)}},
		"structuredContent": out,
		"isError":           false,
	}, nil
}

func errorResult(msg string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": msg}}, "isError": true}
}
