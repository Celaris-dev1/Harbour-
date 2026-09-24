package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Celaris-dev1/Harbour-/internal/demo"
	"github.com/Celaris-dev1/Harbour-/internal/ledger"
	"github.com/Celaris-dev1/Harbour-/internal/registry"
	"github.com/Celaris-dev1/Harbour-/internal/store"
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

// TestAuthRequiredOnEveryRoute guards against a route being added to the mux
// without also being covered by the auth wrapper: Handler wraps the whole
// mux, so this walks every registered method+path and confirms an
// unauthenticated request to each is rejected (healthz is the one
// deliberate exception).
func TestAuthRequiredOnEveryRoute(t *testing.T) {
	st := testdb.Open(t, ledger.Noop{})
	srv := httptest.NewServer((&Server{Store: st, Token: "tok"}).Handler())
	defer srv.Close()

	routes := []struct{ method, path string }{
		{"POST", "/v1/goals"},
		{"GET", "/v1/goals"},
		{"GET", "/v1/goals/x"},
		{"POST", "/v1/goals/x/signals"},
		{"POST", "/v1/goals/x/messages"},
		{"GET", "/v1/goals/x/messages"},
		{"GET", "/v1/goals/x/events"},
		{"GET", "/v1/goals/x/attach"},
		{"POST", "/v1/effects/1/resolve"},
	}
	for _, rt := range routes {
		req, _ := http.NewRequest(rt.method, srv.URL+rt.path, strings.NewReader("{}"))
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != 401 {
			t.Errorf("%s %s: status %d, want 401 without a token", rt.method, rt.path, r.StatusCode)
		}
	}
	// healthz is the deliberate exception.
	if r, err := http.Get(srv.URL + "/healthz"); err != nil || r.StatusCode != 200 {
		t.Fatalf("healthz: %v %v", err, r)
	}
}

func TestRequestBodySizeLimit(t *testing.T) {
	st := testdb.Open(t, ledger.Noop{})
	srv := httptest.NewServer((&Server{Store: st, Token: "tok"}).Handler())
	defer srv.Close()
	huge := strings.Repeat("x", MaxRequestBody+1)
	body := `{"name":"big","agent":"a","created_by":"alice","input":{"pad":"` + huge + `"}}`
	if r := post(t, srv.URL+"/v1/goals", body); r.StatusCode < 400 {
		t.Fatalf("oversized request accepted: %s", r.Status)
	}
}

// FuzzRequestParsing feeds arbitrary bytes to every request-body shape the
// API decodes (create/signal/message/resolve), the same way handlers do
// (json.NewDecoder(...).Decode), and asserts only that it never panics.
// Malformed JSON is expected to error, not crash.
func FuzzRequestParsing(f *testing.F) {
	seeds := []string{
		`{}`, `null`, `[]`, `true`, `123`, `"str"`,
		`{"id":"x","name":"n","agent":"a","created_by":"c","input":{"a":1},"approve":true}`,
		`{"signal":"approve","reason":"r","actor":"a"}`,
		`{"channel":"c","source":"operator","sender":"s","body":{"x":1}}`,
		`{"action":"retry","result":{"y":2},"actor":"a"}`,
		`{"id": ` + strings.Repeat("[", 200) + strings.Repeat("]", 200) + `}`,
		`{bad`, ``, "\x00\x01\x02", `{"id":"` + strings.Repeat("a", 10000) + `"}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		decodeNoPanic := func(v any) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("decoding into %T panicked: %v", v, r)
				}
			}()
			_ = json.NewDecoder(bytes.NewReader(in)).Decode(v)
		}
		decodeNoPanic(&store.CreateGoal{})
		decodeNoPanic(&signalReq{})
		decodeNoPanic(&msgReq{})
		decodeNoPanic(&resolveReq{})
	})
}

// TestExternalGoalIDViaAPI covers submit's external goal_id end to end
// through the HTTP API: idempotent resubmit and conflict both surface with
// the right status codes.
func TestExternalGoalIDViaAPI(t *testing.T) {
	st := testdb.Open(t, ledger.Noop{})
	srv := httptest.NewServer((&Server{Store: st, Token: "tok"}).Handler())
	defer srv.Close()
	body := `{"id":"ext-api-1","name":"eapi","agent":"demo.writer","created_by":"alice","input":{}}`
	r1 := post(t, srv.URL+"/v1/goals", body)
	if r1.StatusCode != 201 {
		t.Fatalf("first submit: %s", r1.Status)
	}
	r2 := post(t, srv.URL+"/v1/goals", body)
	if r2.StatusCode >= 300 {
		t.Fatalf("idempotent resubmit rejected: %s", r2.Status)
	}
	conflict := `{"id":"ext-api-1","name":"eapi","agent":"other-agent","created_by":"alice","input":{}}`
	r3 := post(t, srv.URL+"/v1/goals", conflict)
	if r3.StatusCode != 409 {
		t.Fatalf("conflicting resubmit: %s, want 409", r3.Status)
	}
}
