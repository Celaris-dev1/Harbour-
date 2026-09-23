package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Celaris-dev1/Harbour-/internal/testdb"
)

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func waitHealthy(t *testing.T, base string) {
	for i := 0; i < 100; i++ {
		if r, err := http.Get(base + "/healthz"); err == nil {
			r.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("daemon never became healthy")
}

// TestProcessKilledMidRunResumesWithoutDoubleExecution runs the real harbourd
// binary, lets it die (exit 137, no cleanup, lease still held) right after a
// side effect but before its result row, then starts a fresh harbourd process
// which must finish the goal with every effect applied exactly once.
func TestProcessKilledMidRunResumesWithoutDoubleExecution(t *testing.T) {
	dbURL := testdb.URL(t)
	bin := filepath.Join(t.TempDir(), "harbourd")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	dir := t.TempDir()
	start := func(crashStep string) (*exec.Cmd, string, *bytes.Buffer) {
		addr := freePort(t)
		cmd := exec.Command(bin, "-addr", addr, "-db", dbURL, "-workers", "1", "-lease-ttl", "1500ms", "-demo-dir", dir)
		cmd.Env = append(os.Environ(), "HARBOUR_TEST_CRASH_AFTER_EFFECT_STEP="+crashStep, "LEDGER_URL=")
		var buf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		base := "http://" + addr
		waitHealthy(t, base)
		return cmd, base, &buf
	}

	p1, base1, log1 := start("3")
	body := `{"name":"proc-crash","agent":"demo.writer","created_by":"alice","approve":true,"input":{"file":"out.txt","lines":["l0","l1","l2","l3","l4","l5"]}}`
	resp, err := http.Post(base1+"/v1/goals", "application/json", strings.NewReader(body))
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("submit: %v %v", err, resp)
	}
	var g struct{ ID string }
	json.NewDecoder(resp.Body).Decode(&g)
	resp.Body.Close()

	errc := make(chan error, 1)
	go func() { errc <- p1.Wait() }()
	select {
	case err := <-errc:
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 137 {
			t.Fatalf("expected crash exit 137, got %v\n%s", err, log1)
		}
	case <-time.After(15 * time.Second):
		p1.Process.Kill()
		t.Fatalf("daemon did not crash\n%s", log1)
	}

	p2, base2, log2 := start("")
	defer func() { p2.Process.Kill(); p2.Wait() }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var detail struct {
		Goal    struct{ State string }
		Effects []struct {
			Step     int
			Status   string
			Attempts int
		}
	}
	for ctx.Err() == nil {
		r, err := http.Get(base2 + "/v1/goals/" + g.ID)
		if err == nil {
			json.NewDecoder(r.Body).Decode(&detail)
			r.Body.Close()
			if detail.Goal.State == "done" || detail.Goal.State == "failed" {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if detail.Goal.State != "done" {
		t.Fatalf("state %q\n%s", detail.Goal.State, log2)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "out.txt"))
	var got []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		got = append(got, strings.SplitN(l, "\t", 2)[1])
	}
	if strings.Join(got, ",") != "l0,l1,l2,l3,l4,l5" {
		t.Fatalf("double-executed or lost effects: %v", got)
	}
	for _, e := range detail.Effects {
		if e.Status != "committed" || e.Attempts != 1 {
			t.Fatalf("effect %+v", e)
		}
	}
	// the in-doubt step was settled by the reconciliation probe
	r, _ := http.Get(base2 + "/v1/goals/" + g.ID + "/events")
	var evs []struct {
		Kind string
		Data map[string]any
	}
	json.NewDecoder(r.Body).Decode(&evs)
	r.Body.Close()
	found := false
	for _, e := range evs {
		if e.Kind == "effect.result" && fmt.Sprint(e.Data["step"]) == "3" && e.Data["via"] == "probe" {
			found = true
		}
	}
	if !found {
		t.Fatalf("step 3 not reconciled via probe: %+v", evs)
	}
}
