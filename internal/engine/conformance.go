package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Celaris-dev1/Harbour-/internal/executor"
	"github.com/Celaris-dev1/Harbour-/internal/ledger"
	"github.com/Celaris-dev1/Harbour-/internal/warrant"
)

// RunConformance exercises the semantics Harbour's governance layer depends
// on against newEngine(t). Every Engine implementation (Postgres, Fake,
// and, behind the `temporal` tag, Temporal) runs this identical suite so
// none of it is accidentally specific to one engine's internals.
func RunConformance(t *testing.T, newEngine func(t *testing.T) Engine) {
	t.Run("IdempotentNoDoubleExecution", func(t *testing.T) { testIdempotent(t, newEngine(t)) })
	t.Run("CrashThenNeedsReviewWithoutProbe", func(t *testing.T) { testCrashNeedsReview(t, newEngine(t)) })
	t.Run("CrashThenProbeReconciles", func(t *testing.T) { testCrashProbe(t, newEngine(t)) })
	t.Run("ResolveRetryRunsExactlyOnceMore", func(t *testing.T) { testResolveRetry(t, newEngine(t)) })
	t.Run("ResolveCommittedRecordsOperatorResult", func(t *testing.T) { testResolveCommitted(t, newEngine(t)) })
	t.Run("FailedStepIsSticky", func(t *testing.T) { testFailedSticky(t, newEngine(t)) })
	t.Run("WarrantDenyBlocksWithoutRunning", func(t *testing.T) { testWarrantDeny(t, newEngine(t)) })
	t.Run("WarrantAllowRuns", func(t *testing.T) { testWarrantAllow(t, newEngine(t)) })
	t.Run("LedgerReceiptsCarryGoalID", func(t *testing.T) { testLedgerReceipts(t, newEngine(t)) })
	t.Run("SignalAwaitDeliversPayload", func(t *testing.T) { testSignal(t, newEngine(t)) })
}

func testIdempotent(t *testing.T, e Engine) {
	ctx := context.Background()
	var calls int32
	fn := func(context.Context) (json.RawMessage, error) {
		atomic.AddInt32(&calls, 1)
		return json.RawMessage(`{"n":1}`), nil
	}
	r1, err := e.ScheduleStep(ctx, "k1", fn, nil)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := e.ScheduleStep(ctx, "k1", fn, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Compare by value, not raw bytes: a round-trip through Postgres JSONB
	// reformats whitespace (e.g. adds a space after ':'), which is not a
	// semantic difference.
	if !jsonEqual(r1, r2) {
		t.Fatalf("results differ: %s vs %s", r1, r2)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("fn called %d times, want 1", calls)
	}
}

func testCrashNeedsReview(t *testing.T, e Engine) {
	ctx := context.Background()
	crash := func(context.Context) (json.RawMessage, error) { return nil, ErrSimulatedCrash }
	if _, err := e.ScheduleStep(ctx, "k2", crash, nil); !errors.Is(err, ErrSimulatedCrash) {
		t.Fatalf("err=%v, want ErrSimulatedCrash", err)
	}
	status, _, _, err := e.StepStatus(ctx, "k2")
	if err != nil || status != "intent" {
		t.Fatalf("status=%q err=%v, want intent", status, err)
	}
	// Resume: no probe, so this must go to needs_review without calling fn.
	var calls int32
	fn := func(context.Context) (json.RawMessage, error) {
		atomic.AddInt32(&calls, 1)
		return json.RawMessage(`{}`), nil
	}
	if _, err := e.ScheduleStep(ctx, "k2", fn, nil); !errors.Is(err, ErrNeedsReview) {
		t.Fatalf("err=%v, want ErrNeedsReview", err)
	}
	if calls != 0 {
		t.Fatalf("fn was called during recovery without a probe: %d", calls)
	}
}

func testCrashProbe(t *testing.T, e Engine) {
	ctx := context.Background()
	crash := func(context.Context) (json.RawMessage, error) { return nil, ErrSimulatedCrash }
	if _, err := e.ScheduleStep(ctx, "k3", crash, nil); !errors.Is(err, ErrSimulatedCrash) {
		t.Fatal(err)
	}
	probeResult := json.RawMessage(`{"probed":true}`)
	probe := func(context.Context) (bool, json.RawMessage, error) { return true, probeResult, nil }
	var calls int32
	fn := func(context.Context) (json.RawMessage, error) {
		atomic.AddInt32(&calls, 1)
		return json.RawMessage(`{"ran":true}`), nil
	}
	res, err := e.ScheduleStep(ctx, "k3", fn, probe)
	if err != nil {
		t.Fatal(err)
	}
	if string(res) != string(probeResult) {
		t.Fatalf("result=%s, want probe result %s", res, probeResult)
	}
	if calls != 0 {
		t.Fatalf("fn was re-run despite the probe reporting success: %d calls", calls)
	}
	status, _, _, _ := e.StepStatus(ctx, "k3")
	if status != "committed" {
		t.Fatalf("status=%q, want committed", status)
	}
}

func testResolveRetry(t *testing.T, e Engine) {
	ctx := context.Background()
	crash := func(context.Context) (json.RawMessage, error) { return nil, ErrSimulatedCrash }
	e.ScheduleStep(ctx, "k4", crash, nil)
	if _, err := e.ScheduleStep(ctx, "k4", crash, nil); !errors.Is(err, ErrNeedsReview) {
		t.Fatalf("err=%v, want ErrNeedsReview", err)
	}
	if err := e.Resolve(ctx, "k4", "retry", nil); err != nil {
		t.Fatal(err)
	}
	var calls int32
	fn := func(context.Context) (json.RawMessage, error) {
		atomic.AddInt32(&calls, 1)
		return json.RawMessage(`{"ok":true}`), nil
	}
	res, err := e.ScheduleStep(ctx, "k4", fn, nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("fn called %d times after retry, want 1", calls)
	}
	if string(res) != `{"ok":true}` {
		t.Fatalf("res=%s", res)
	}
}

func testResolveCommitted(t *testing.T, e Engine) {
	ctx := context.Background()
	crash := func(context.Context) (json.RawMessage, error) { return nil, ErrSimulatedCrash }
	e.ScheduleStep(ctx, "k5", crash, nil)
	e.ScheduleStep(ctx, "k5", crash, nil) // -> needs_review
	opResult := json.RawMessage(`{"by":"operator"}`)
	if err := e.Resolve(ctx, "k5", "committed", opResult); err != nil {
		t.Fatal(err)
	}
	var calls int32
	fn := func(context.Context) (json.RawMessage, error) {
		atomic.AddInt32(&calls, 1)
		return nil, fmt.Errorf("must not be called")
	}
	res, err := e.ScheduleStep(ctx, "k5", fn, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(res, opResult) {
		t.Fatalf("res=%s, want %s", res, opResult)
	}
	if calls != 0 {
		t.Fatal("fn was called after operator recorded the result")
	}
}

func testFailedSticky(t *testing.T, e Engine) {
	ctx := context.Background()
	var calls int32
	fn := func(context.Context) (json.RawMessage, error) {
		atomic.AddInt32(&calls, 1)
		return nil, fmt.Errorf("boom")
	}
	if _, err := e.ScheduleStep(ctx, "k6", fn, nil); !errors.Is(err, ErrToolFailed) {
		t.Fatalf("err=%v, want ErrToolFailed", err)
	}
	if _, err := e.ScheduleStep(ctx, "k6", fn, nil); !errors.Is(err, ErrToolFailed) {
		t.Fatalf("err=%v, want ErrToolFailed again", err)
	}
	if calls != 1 {
		t.Fatalf("fn called %d times, want 1 (failed is sticky, not retried automatically)", calls)
	}
}

func testWarrantDeny(t *testing.T, e Engine) {
	ctx := context.Background()
	var calls int32
	fn := func(context.Context) (json.RawMessage, error) {
		atomic.AddInt32(&calls, 1)
		return json.RawMessage(`{}`), nil
	}
	rt := &Runtime{Eng: e, Warrant: fixedWarrant{allow: false, reason: "no scope"}}
	_, err := rt.RunEffect(ctx, "g1", 0, "deny-tool", json.RawMessage(`{}`), Auth{Token: "t"}, fn, nil)
	if !errors.Is(err, ErrWarrantDenied) {
		t.Fatalf("err=%v, want ErrWarrantDenied", err)
	}
	if calls != 0 {
		t.Fatal("tool ran despite warrant denial")
	}
	status, _, _, _ := e.StepStatus(ctx, mustKey(t, "deny-tool", json.RawMessage(`{}`), "g1", 0))
	if status != "" {
		t.Fatalf("denial should never touch the step's idempotency key, status=%q", status)
	}
}

func testWarrantAllow(t *testing.T, e Engine) {
	ctx := context.Background()
	var calls int32
	fn := func(context.Context) (json.RawMessage, error) {
		atomic.AddInt32(&calls, 1)
		return json.RawMessage(`{"ok":true}`), nil
	}
	rt := &Runtime{Eng: e, Warrant: fixedWarrant{allow: true}}
	res, err := rt.RunEffect(ctx, "g2", 0, "allow-tool", json.RawMessage(`{}`), Auth{Token: "t"}, fn, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(res) != `{"ok":true}` || calls != 1 {
		t.Fatalf("res=%s calls=%d", res, calls)
	}
}

func testLedgerReceipts(t *testing.T, e Engine) {
	ctx := context.Background()
	ml := &memLedger{}
	rt := &Runtime{Eng: e, Ledger: ml}
	fn := func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }
	if _, err := rt.RunEffect(ctx, "g3", 0, "ledger-tool", json.RawMessage(`{}`), Auth{}, fn, nil); err != nil {
		t.Fatal(err)
	}
	if len(ml.recs) != 2 {
		t.Fatalf("got %d ledger records, want 2 (intent+result)", len(ml.recs))
	}
	for _, r := range ml.recs {
		if r.GoalID != "g3" {
			t.Fatalf("record goal_id=%q, want g3", r.GoalID)
		}
	}
	if ml.recs[0].Type != "harbour.effect.intent" || ml.recs[1].Type != "harbour.effect.result" {
		t.Fatalf("record types: %s, %s", ml.recs[0].Type, ml.recs[1].Type)
	}
}

func testSignal(t *testing.T, e Engine) {
	ctx := context.Background()
	done := make(chan json.RawMessage, 1)
	go func() {
		p, err := e.AwaitSignal(ctx, "sigkey", "go")
		if err != nil {
			t.Error(err)
			return
		}
		done <- p
	}()
	// Give AwaitSignal a moment to start waiting (not required for
	// correctness — Signal must be delivered even if it arrives first —
	// but exercises the "waiter already blocked" path too).
	if err := e.Signal(ctx, "sigkey", "go", json.RawMessage(`{"v":42}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-done:
		if !jsonEqual(p, json.RawMessage(`{"v":42}`)) {
			t.Fatalf("payload=%s", p)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for signal")
	}
}

func jsonEqual(a, b json.RawMessage) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return string(a) == string(b)
	}
	ab, _ := json.Marshal(av)
	bb, _ := json.Marshal(bv)
	return string(ab) == string(bb)
}

func mustKey(t *testing.T, tool string, args json.RawMessage, goalID string, step int) string {
	t.Helper()
	// Re-derive the same way Runtime.RunEffect does, via internal/executor.
	k, _, err := executor.Key(tool, args, goalID, step)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// fixedWarrant is a fake warrant.Client for the conformance suite.
type fixedWarrant struct {
	allow  bool
	reason string
}

func (f fixedWarrant) Authorize(_ context.Context, _, _ string, _ warrant.Call) (warrant.Decision, error) {
	return warrant.Decision{Allow: f.allow, Reason: f.reason}, nil
}

// memLedger is a fake ledger.Recorder for the conformance suite.
type memLedger struct {
	mu   sync.Mutex
	recs []ledger.Record
}

func (m *memLedger) Record(_ context.Context, r ledger.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs = append(m.recs, r)
	return nil
}
