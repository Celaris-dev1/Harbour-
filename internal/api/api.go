// Package api is harbourd's HTTP API, including SSE attach.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Celaris-dev1/Harbour-/internal/fsm"
	"github.com/Celaris-dev1/Harbour-/internal/store"
)

type Server struct {
	Store *store.Store
	Token string
	Poll  time.Duration
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"ok": true}) })
	m.HandleFunc("POST /v1/goals", s.create)
	m.HandleFunc("GET /v1/goals", s.list)
	m.HandleFunc("GET /v1/goals/{id}", s.get)
	m.HandleFunc("POST /v1/goals/{id}/signals", s.signal)
	m.HandleFunc("POST /v1/goals/{id}/messages", s.postMessage)
	m.HandleFunc("GET /v1/goals/{id}/messages", s.messages)
	m.HandleFunc("GET /v1/goals/{id}/events", s.events)
	m.HandleFunc("GET /v1/goals/{id}/attach", s.attach)
	m.HandleFunc("POST /v1/effects/{id}/resolve", s.resolve)
	return s.auth(m)
}

func (s *Server) auth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" && r.URL.Path != "/healthz" && r.Header.Get("Authorization") != "Bearer "+s.Token {
			writeErr(w, 401, errors.New("unauthorized"))
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	var ill fsm.ErrIllegal
	if errors.As(err, &ill) {
		code = 409
	} else if errors.Is(err, store.ErrNotFound) {
		code = 404
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var c store.CreateGoal
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeErr(w, 400, err)
		return
	}
	g, err := s.Store.Create(r.Context(), c)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 201, g)
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	gs, err := s.Store.List(r.Context())
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if gs == nil {
		gs = []*store.Goal{}
	}
	writeJSON(w, 200, gs)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	g, err := s.Store.Get(ctx, r.PathValue("id"))
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	steps, _ := s.Store.Steps(ctx, g.ID)
	effs, _ := s.Store.Effects(ctx, g.ID)
	writeJSON(w, 200, map[string]any{"goal": g, "steps": steps, "effects": effs})
}

type signalReq struct {
	Signal   string `json:"signal"` // approve|pause|resume|cancel|reparent
	ParentID string `json:"parent_id"`
	Reason   string `json:"reason"`
	Actor    string `json:"actor"`
}

func (s *Server) signal(w http.ResponseWriter, r *http.Request) {
	var q signalReq
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		writeErr(w, 400, err)
		return
	}
	if q.Actor == "" {
		q.Actor = "operator"
	}
	id, ctx := r.PathValue("id"), r.Context()
	var g *store.Goal
	var err error
	switch q.Signal {
	case "approve":
		g, err = s.Store.Transition(ctx, id, fsm.Approved, q.Reason, q.Actor)
	case "pause":
		g, err = s.Store.Transition(ctx, id, fsm.Paused, q.Reason, q.Actor)
	case "resume":
		g, err = s.Store.Resume(ctx, id, q.Actor)
	case "cancel":
		g, err = s.Store.Transition(ctx, id, fsm.Cancelled, q.Reason, q.Actor)
	case "reparent":
		g, err = s.Store.Reparent(ctx, id, q.ParentID, q.Actor)
	default:
		err = fmt.Errorf("unknown signal %q", q.Signal)
	}
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, g)
}

type msgReq struct {
	Channel string          `json:"channel"`
	Source  string          `json:"source"`
	Sender  string          `json:"sender"`
	Body    json.RawMessage `json:"body"`
}

func (s *Server) postMessage(w http.ResponseWriter, r *http.Request) {
	var q msgReq
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		writeErr(w, 400, err)
		return
	}
	g, err := s.Store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	m, err := s.Store.AddMessage(r.Context(), g.ID, q.Channel, q.Source, q.Sender, q.Body)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 201, m)
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	g, err := s.Store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	ms, err := s.Store.Messages(r.Context(), g.ID)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, ms)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	g, err := s.Store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	ev, err := s.Store.Events(r.Context(), g.ID, after)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, ev)
}

// attach streams a snapshot then every new event as SSE until the goal is
// terminal (plus a final snapshot) or the client disconnects. Events live in
// Postgres, so any harbourd node can serve attach for any goal.
func (s *Server) attach(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	g, err := s.Store.Get(ctx, r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	send := func(kind string, id int64, v any) {
		b, _ := json.Marshal(v)
		if id > 0 {
			fmt.Fprintf(w, "id: %d\n", id)
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b)
		fl.Flush()
	}
	send("snapshot", 0, g)
	after, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	if v := r.URL.Query().Get("after"); v != "" {
		after, _ = strconv.ParseInt(v, 10, 64)
	}
	poll := s.Poll
	if poll == 0 {
		poll = 300 * time.Millisecond
	}
	for {
		evs, err := s.Store.Events(ctx, g.ID, after)
		if err != nil {
			return
		}
		for _, e := range evs {
			send(e.Kind, e.Seq, e)
			after = e.Seq
		}
		cur, err := s.Store.Get(ctx, g.ID)
		if err == nil && fsm.Terminal(cur.State) && len(evs) == 0 {
			send("end", 0, cur)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
	}
}

type resolveReq struct {
	Action string          `json:"action"`
	Result json.RawMessage `json:"result"`
	Actor  string          `json:"actor"`
}

func (s *Server) resolve(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	var q resolveReq
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		writeErr(w, 400, err)
		return
	}
	if q.Actor == "" {
		q.Actor = "operator"
	}
	e, err := s.Store.ResolveReview(r.Context(), id, q.Action, q.Result, q.Actor)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, e)
}

