// Command e2e-fakewarrant is a minimal stand-in for a Warrant broker's
// POST /v1/authorize, used only by scripts/e2e.sh to exercise Harbour's
// Warrant integration (internal/warrant) end to end without a real Warrant
// deployment. It is not part of the Harbour product.
//
// It denies calls whose tool matches DENY_TOOL (exact match; empty means
// deny nothing) and allows everything else. DENY_TOOL can be changed at
// runtime via PUT /control (body: the new value, may be empty to clear it),
// so one running instance can cover both an allow and a deny scenario.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
)

type authorizeRequest struct {
	Token string         `json:"token"`
	SVID  string         `json:"svid"`
	Call  map[string]any `json:"call"`
}

type decision struct {
	Allow  bool   `json:"allow"`
	Reason string `json:"reason"`
}

func main() {
	addr := flag.String("addr", envOr("ADDR", ":8555"), "listen address")
	flag.Parse()

	var mu sync.Mutex
	denyTool := os.Getenv("DENY_TOOL")

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /control", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		denyTool = string(b)
		mu.Unlock()
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /v1/authorize", func(w http.ResponseWriter, r *http.Request) {
		var req authorizeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		tool, _ := req.Call["tool"].(string)
		mu.Lock()
		deny := denyTool
		mu.Unlock()
		d := decision{Allow: true}
		if deny != "" && tool == deny {
			d = decision{Allow: false, Reason: "e2e-fakewarrant: tool " + tool + " is denied"}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(d)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	log.Printf("e2e-fakewarrant listening on %s (deny_tool=%q)", *addr, denyTool)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
