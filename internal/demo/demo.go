// Package demo provides example tools and a demo agent.
package demo

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Celaris-dev1/Harbour-/internal/registry"
)

// Echo returns its args. Pure, no probe (in-doubt echo calls go to review).
type Echo struct{}

func (Echo) Name() string { return "echo" }
func (Echo) Execute(_ context.Context, _ string, args json.RawMessage) (json.RawMessage, error) {
	return args, nil
}

// FileAppend appends a line to a file under Dir, tagged with the idempotency
// key so the probe can tell whether the write already happened.
type FileAppend struct {
	Dir string
	mu  sync.Mutex
}

func (*FileAppend) Name() string { return "fs.append" }

type appendArgs struct {
	File string `json:"file"`
	Line string `json:"line"`
}

func (f *FileAppend) path(a appendArgs) (string, error) {
	if a.File == "" || strings.Contains(a.File, "..") || filepath.IsAbs(a.File) {
		return "", fmt.Errorf("bad file %q", a.File)
	}
	return filepath.Join(f.Dir, a.File), nil
}

func (f *FileAppend) Execute(_ context.Context, key string, raw json.RawMessage) (json.RawMessage, error) {
	var a appendArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	p, err := f.path(a)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	fh, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	if _, err := fmt.Fprintf(fh, "%s\t%s\n", key, a.Line); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"file": a.File, "written": a.Line})
}

func (f *FileAppend) Probe(_ context.Context, key string, raw json.RawMessage) (registry.ProbeResult, error) {
	var a appendArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return registry.ProbeResult{}, err
	}
	p, err := f.path(a)
	if err != nil {
		return registry.ProbeResult{}, err
	}
	fh, err := os.Open(p)
	if os.IsNotExist(err) {
		return registry.ProbeResult{}, nil
	}
	if err != nil {
		return registry.ProbeResult{}, err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), key+"\t") {
			r, _ := json.Marshal(map[string]any{"file": a.File, "written": a.Line})
			return registry.ProbeResult{Happened: true, Result: r}, nil
		}
	}
	return registry.ProbeResult{}, sc.Err()
}

// Writer is the demo agent. Input: {"file":"out.txt","lines":["a","b"]}.
// It appends each line, then appends the "append" text of every *trusted*
// operator message. Text from untrusted sources (peer agents, retrieved
// documents, tool results) is never treated as an instruction.
type Writer struct{}

func (Writer) Name() string { return "demo.writer" }

type writerInput struct {
	File  string   `json:"file"`
	Lines []string `json:"lines"`
}

func (Writer) plan(sc registry.StepContext) (writerInput, []string, error) {
	var in writerInput
	if err := json.Unmarshal(sc.Input, &in); err != nil {
		return in, nil, err
	}
	if in.File == "" {
		in.File = "out.txt"
	}
	lines := append([]string{}, in.Lines...)
	for _, m := range sc.Messages {
		if m.Source != registry.SrcOperator || !m.Trusted {
			continue
		}
		var b struct {
			Append string `json:"append"`
		}
		if json.Unmarshal(m.Body, &b) == nil && b.Append != "" {
			lines = append(lines, b.Append)
		}
	}
	return in, lines, nil
}

func (w Writer) Next(_ context.Context, sc registry.StepContext) (registry.Action, error) {
	in, lines, err := w.plan(sc)
	if err != nil {
		return registry.Action{}, err
	}
	if sc.Step >= len(lines) {
		return registry.Action{Finish: true}, nil
	}
	args, _ := json.Marshal(appendArgs{File: in.File, Line: lines[sc.Step]})
	return registry.Action{Tool: "fs.append", Args: args}, nil
}

func (w Writer) Verify(_ context.Context, sc registry.StepContext) error {
	_, lines, err := w.plan(sc)
	if err != nil {
		return err
	}
	if len(sc.History) < len(lines) {
		return fmt.Errorf("only %d of %d lines committed", len(sc.History), len(lines))
	}
	return nil
}

// Echoer calls the probe-less Echo tool N times (input: {"n":2}). It exists
// to exercise the needs_review path end to end (a crash mid-effect on a
// tool with no Prober must pause the goal for an operator, not silently
// retry) with a plain built-in tool, e.g. from scripts/e2e.sh.
type Echoer struct{}

func (Echoer) Name() string { return "demo.echoer" }

type echoerInput struct {
	N int `json:"n"`
}

func (Echoer) n(sc registry.StepContext) int {
	var in echoerInput
	json.Unmarshal(sc.Input, &in)
	if in.N <= 0 {
		in.N = 1
	}
	return in.N
}

func (e Echoer) Next(_ context.Context, sc registry.StepContext) (registry.Action, error) {
	if sc.Step >= e.n(sc) {
		return registry.Action{Finish: true}, nil
	}
	args, _ := json.Marshal(map[string]int{"i": sc.Step})
	return registry.Action{Tool: "echo", Args: args}, nil
}

func (e Echoer) Verify(_ context.Context, sc registry.StepContext) error {
	if len(sc.History) < e.n(sc) {
		return fmt.Errorf("only %d of %d steps committed", len(sc.History), e.n(sc))
	}
	return nil
}

// Register adds the demo tools and agents.
func Register(r *registry.Registry, dir string) {
	r.AddTool(Echo{})
	r.AddTool(&FileAppend{Dir: dir})
	r.AddAgent(Writer{})
	r.AddAgent(Echoer{})
}
