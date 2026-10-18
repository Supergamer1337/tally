package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"tally/internal/store"
	"tally/internal/tally"
)

// state is everything the app draws from. It is small: one row per logged day.
type state struct {
	Today string `json:"today"`
	// Zone and Now let the browser move today on by itself at midnight, and right away when a
	// phone wakes, without waiting for the server.
	Zone    string        `json:"zone"`
	Now     int64         `json:"now"`
	Entries []store.Entry `json:"entries"`
	Agents  agentsState   `json:"agents"`
}

type agentsState struct {
	URL       string        `json:"url"`
	Token     string        `json:"token"`
	Write     bool          `json:"write"`
	Connected []store.Agent `json:"connected"`
}

func (s *Server) state(r *http.Request) (state, error) {
	ctx := r.Context()
	entries, err := s.svc.Entries(ctx)
	if err != nil {
		return state{}, err
	}
	token, err := s.svc.MCPToken(ctx)
	if err != nil {
		return state{}, err
	}
	write, err := s.svc.AgentsCanWrite(ctx)
	if err != nil {
		return state{}, err
	}
	agents, err := s.svc.Store.Agents(ctx)
	if err != nil {
		return state{}, err
	}
	return state{
		Today:   s.svc.Today(),
		Zone:    s.svc.Loc.String(),
		Now:     s.svc.Now().UnixMilli(),
		Entries: entries,
		Agents:  agentsState{URL: s.publicURL(r) + "/mcp", Token: token, Write: write, Connected: agents},
	}, nil
}

func (s *Server) getState(w http.ResponseWriter, r *http.Request) {
	st, err := s.state(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

type dayBody struct {
	Minutes *int   `json:"minutes"`
	Note    string `json:"note"`
	// Expect, when sent, makes a PUT conditional: it only applies while the day still holds
	// this (null for no entry). Undo sends what its action left behind.
	Expect json.RawMessage `json:"expect"`
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request")
		return false
	}
	return true
}

// addDay puts minutes onto a day the way the Add form does.
func (s *Server) addDay(w http.ResponseWriter, r *http.Request) {
	var b dayBody
	if !decode(w, r, &b) {
		return
	}
	if b.Minutes == nil {
		writeError(w, http.StatusBadRequest, "minutes is required")
		return
	}
	ch, err := s.svc.Add(r.Context(), r.PathValue("date"), *b.Minutes, b.Note)
	s.reply(w, ch, err)
}

// setDay makes a day hold exactly what is sent. Undo uses it.
func (s *Server) setDay(w http.ResponseWriter, r *http.Request) {
	var b dayBody
	if !decode(w, r, &b) {
		return
	}
	if b.Minutes == nil {
		writeError(w, http.StatusBadRequest, "minutes is required")
		return
	}
	if b.Expect == nil {
		ch, err := s.svc.Set(r.Context(), r.PathValue("date"), *b.Minutes, b.Note)
		s.reply(w, ch, err)
		return
	}
	var expect *store.Entry
	if err := json.Unmarshal(b.Expect, &expect); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request")
		return
	}
	var to *store.Entry
	if *b.Minutes != 0 {
		to = &store.Entry{Minutes: *b.Minutes, Note: b.Note}
	}
	ch, err := s.svc.Restore(r.Context(), r.PathValue("date"), to, expect)
	s.reply(w, ch, err)
}

func (s *Server) deleteDay(w http.ResponseWriter, r *http.Request) {
	ch, err := s.svc.Delete(r.Context(), r.PathValue("date"))
	s.reply(w, ch, err)
}

func (s *Server) setWrite(w http.ResponseWriter, r *http.Request) {
	var b struct {
		On *bool `json:"on"`
	}
	if !decode(w, r, &b) {
		return
	}
	if b.On == nil {
		writeError(w, http.StatusBadRequest, "on is required")
		return
	}
	if err := s.svc.SetAgentsCanWrite(r.Context(), *b.On); err != nil {
		s.fail(w, err)
		return
	}
	// add_day comes or goes, and agents that listen for it hear so at once.
	s.mcp.ToolsChanged()
	writeJSON(w, http.StatusOK, map[string]bool{"on": *b.On})
}

func (s *Server) reply(w http.ResponseWriter, ch tally.Change, err error) {
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, tally.ErrConflict) {
		writeError(w, http.StatusConflict, "Changed elsewhere")
		return
	}
	if errors.Is(err, tally.ErrInvalid) {
		writeError(w, http.StatusBadRequest, tally.Message(err))
		return
	}
	s.log.Error("request failed", "err", err)
	writeError(w, http.StatusInternalServerError, "Something went wrong")
}
