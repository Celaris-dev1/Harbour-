package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Celaris-dev1/Harbour-/internal/demo"
	"github.com/Celaris-dev1/Harbour-/internal/ledger"
	"github.com/Celaris-dev1/Harbour-/internal/registry"
	"github.com/Celaris-dev1/Harbour-/internal/testdb"
	"github.com/Celaris-dev1/Harbour-/internal/worker"
)

func post(t *testing.T, url, body string) *http.Response {
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAPIAndAttach(t *testing.T) {
	st := testdb.Open(t, ledger.Noop{})
	srv := httptest.NewServer((&Server{Store: st, Token: "tok", Poll: 20 * time.Millisecond}).Handler())
	defer srv.Close()
	if r, _ := http.Get(srv.URL + "/v1/goals"); r.StatusCode != 401 {
		t.Fatal("auth not enforced")
	}
	if r := post(t, srv.URL+"/v1/goals", `{"name":"api1","agent":"demo.writer","created_by":"alice","input":{"lines":["x","y"]}}`); r.StatusCode != 201 {
		t.Fatal(r.Status)
	}
	if r := post(t, srv.URL+"/v1/goals/api1/signals", `{"signal":"resume"}`); r.StatusCode != 409 {
		t.Fatalf("resume of proposed should be 409, got %s", r.Status)
	}
	if r := post(t, srv.URL+"/v1/goals/api1/messages", `{"source":"retrieved_document","sender":"http://x","body":"hi"}`); r.StatusCode != 201 {
		t.Fatal(r.Status)
	}
	if r := post(t, srv.URL+"/v1/goals/api1/messages", `{"source":"nope","body":"hi"}`); r.StatusCode != 400 {
		t.Fatal(r.Status)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/v1/goals/api1/attach", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal(err, resp.Header)
	}
	defer resp.Body.Close()
	if r := post(t, srv.URL+"/v1/goals/api1/signals", `{"signal":"approve"}`); r.StatusCode != 200 {
		t.Fatal(r.Status)
	}
	reg := registry.New()
	demo.Register(reg, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go worker.New("w", st, reg).RunOnce(ctx)
	seen := map[string]bool{}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if ev, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
			seen[ev] = true
			if ev == "end" {
				break
			}
		}
	}
	for _, k := range []string{"snapshot", "transition", "message", "effect.intent", "effect.result", "step.committed", "end"} {
		if !seen[k] {
			t.Errorf("attach missing %q (saw %v)", k, seen)
		}
	}
}
