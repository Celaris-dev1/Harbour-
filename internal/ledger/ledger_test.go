package ledger

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPRecord(t *testing.T) {
	var got Record
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/records" || r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(400)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(201)
	}))
	defer srv.Close()
	h := &HTTP{BaseURL: srv.URL, Token: "tok", Client: srv.Client()}
	err := h.Record(context.Background(), Record{Type: "harbour.goal.transition", ActorChain: []Actor{{Kind: "human", ID: "op"}}, Payload: map[string]any{"a": 1}})
	if err != nil || got.Chain != "harbour" || got.Type != "harbour.goal.transition" {
		t.Fatalf("err=%v got=%+v", err, got)
	}
	if h.Record(context.Background(), Record{ActorChain: []Actor{{Kind: "agent", ID: "x"}}}) == nil {
		t.Fatal("expected error for non-human origin")
	}
}
