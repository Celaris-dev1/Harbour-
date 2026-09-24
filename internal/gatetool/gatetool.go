// Package gatetool is a Harbour Tool that runs Gate's own verification (`gate run --format
// json`) as an effect: Harbour asks Gate to review a diff and records Gate's verdict as a
// committed (or failed) effect, with the effect's stack-receipt/v1 linked to Gate's own
// gate.verdict receipt (via GateRun's "run_id", which Gate's own ledger record for
// gate.run.decided uses as its `subject`; see docs/receipt-spec.md's "links" field).
//
// This composes the two products at the same point every other tool composes: an ordinary
// registry.Tool, executed and recorded exactly like fs.append or echo — Gate is invoked as a
// real subprocess (the `gate` binary), never imported as a library.
package gatetool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// Tool runs `gate run --format json` for one repo/base/head and reports Gate's verdict.
type Tool struct {
	// Bin is the gate binary to exec. Defaults to GATE_BIN env, then "gate" (resolved via PATH).
	Bin string
}

func (t *Tool) bin() string {
	if t.Bin != "" {
		return t.Bin
	}
	if b := os.Getenv("GATE_BIN"); b != "" {
		return b
	}
	return "gate"
}

func (*Tool) Name() string { return "gate.verify" }

// args is this tool's input: what to hand `gate run`.
type args struct {
	Repo  string `json:"repo"`
	Base  string `json:"base"`
	Head  string `json:"head"`  // empty: gate reviews the working tree against Base
	Goal  string `json:"goal"`  // Ledger goal id to group Gate's own records under
	Human string `json:"human"` // originating human, for Gate's own actor chain
}

// gateReport is the subset of Gate's `gate run --format json` output this tool needs. Gate's
// own JSON is documented in its README; only ID, Verdict and Ledger are read here.
type gateReport struct {
	ID      string `json:"id"`
	Verdict struct {
		Decision string  `json:"decision"`
		Score    float64 `json:"score"`
	} `json:"verdict"`
	Warnings []string `json:"warnings"`
}

// result is what Execute returns: Gate's verdict, plus linked_run_id so Harbour's store links
// this effect's stack-receipt to Gate's own gate.verdict receipt for the same run (see
// store.CommitResult, which looks for exactly this field).
type result struct {
	Decision    string  `json:"decision"`
	Score       float64 `json:"score"`
	RunID       string  `json:"run_id"`
	LinkedRunID string  `json:"linked_run_id"`
	Warnings    []string `json:"warnings,omitempty"`
}

// Execute runs `gate run --repo REPO --base BASE [--head HEAD] --format json` and reports the
// verdict. A non-allow decision is not a Go error (the effect commits either way — Gate said
// no, and that answer is exactly what gets recorded); only a Gate crash or unparsable output is.
func (t *Tool) Execute(ctx context.Context, _ string, raw json.RawMessage) (json.RawMessage, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("gate.verify: bad args: %w", err)
	}
	if a.Repo == "" || a.Base == "" {
		return nil, fmt.Errorf("gate.verify: repo and base are required")
	}
	cmdArgs := []string{"run", "--repo", a.Repo, "--base", a.Base, "--format", "json", "--exit-zero"}
	if a.Head != "" {
		cmdArgs = append(cmdArgs, "--head", a.Head)
	}
	if a.Goal != "" {
		cmdArgs = append(cmdArgs, "--goal", a.Goal)
	}
	if a.Human != "" {
		cmdArgs = append(cmdArgs, "--human", a.Human)
	}
	cmd := exec.CommandContext(ctx, t.bin(), cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// gate run exits non-zero on a non-allow verdict (see Gate's README); that's still a
	// completed, valid verification, not a tool failure, so only report an error if we got no
	// parseable JSON at all.
	runErr := cmd.Run()
	var rep gateReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		if runErr != nil {
			return nil, fmt.Errorf("gate.verify: gate run: %w: %s", runErr, stderr.String())
		}
		return nil, fmt.Errorf("gate.verify: parsing gate output: %w: %s", err, stdout.String())
	}
	out := result{
		Decision: rep.Verdict.Decision, Score: rep.Verdict.Score,
		RunID: rep.ID, LinkedRunID: rep.ID, Warnings: rep.Warnings,
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return b, nil
}
