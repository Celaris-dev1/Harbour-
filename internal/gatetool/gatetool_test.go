package gatetool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// fakeGate writes a stand-in `gate` binary (a shell script) that emits a fixed
// `gate run --format json` report, so this test exercises Tool.Execute's process-exec and
// JSON-parsing path without needing a real Gate checkout.
func fakeGate(t *testing.T, decision string, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary; unix only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "gate")
	script := "#!/bin/sh\ncat <<'EOF'\n{\"id\":\"run-123\",\"verdict\":{\"decision\":\"" + decision + "\",\"score\":0.4},\"warnings\":[\"note\"]}\nEOF\nexit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecuteAllow(t *testing.T) {
	tool := &Tool{Bin: fakeGate(t, "allow", 0)}
	raw, err := tool.Execute(context.Background(), "key1", json.RawMessage(`{"repo":"/tmp/repo","base":"main"}`))
	if err != nil {
		t.Fatal(err)
	}
	var r result
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	if r.Decision != "allow" || r.RunID != "run-123" || r.LinkedRunID != "run-123" {
		t.Fatalf("unexpected result: %+v", r)
	}
}

func TestExecuteDenyStillCommits(t *testing.T) {
	// Even a nonzero exit (a real deny without --exit-zero) must not be treated as a tool
	// failure as long as gate printed a valid report: the verdict IS the outcome.
	tool := &Tool{Bin: fakeGate(t, "deny", 1)}
	raw, err := tool.Execute(context.Background(), "key1", json.RawMessage(`{"repo":"/tmp/repo","base":"main"}`))
	if err != nil {
		t.Fatal(err)
	}
	var r result
	_ = json.Unmarshal(raw, &r)
	if r.Decision != "deny" {
		t.Fatalf("unexpected result: %+v", r)
	}
}

func TestExecuteRequiresRepoAndBase(t *testing.T) {
	tool := &Tool{Bin: "/bin/true"}
	if _, err := tool.Execute(context.Background(), "key1", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error for missing repo/base")
	}
}

func TestName(t *testing.T) {
	if (&Tool{}).Name() != "gate.verify" {
		t.Fatal("unexpected tool name")
	}
}
