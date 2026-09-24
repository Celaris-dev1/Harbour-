// Package engine defines the durability primitives Harbour's governance
// logic (idempotent effects, needs_review, Warrant authorization, Ledger
// receipts) is built on, as an interface any workflow engine can implement.
//
// The production runtime (internal/store + internal/executor + internal/worker)
// is the default, Postgres-backed implementation and is not rewritten on top
// of this interface — it predates it and is battle-tested by the crash/lease
// tests in internal/worker. This package instead proves the *portability* of
// Harbour's governance semantics: Runtime, defined here, implements the same
// idempotency-key scheme, needs_review behavior, Warrant gating and Ledger
// receipt emission purely in terms of the Engine interface, and the
// conformance suite in conformance.go runs identically against every Engine
// implementation — the built-in Postgres engine (postgres.go), an
// in-process Fake (fake.go) standing in for "a second engine", and,
// behind the `temporal` build tag, an adapter onto go.temporal.io/sdk
// (temporal.go) for teams that want Temporal's durable execution instead of
// Harbour's own Postgres lease loop.
//
// This is deliberately a parallel, self-contained layer: swapping the main
// harbourd runtime itself onto a pluggable engine is a larger migration this
// package does not attempt.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Celaris-dev1/Harbour-/internal/executor"
	"github.com/Celaris-dev1/Harbour-/internal/ledger"
	"github.com/Celaris-dev1/Harbour-/internal/warrant"
)

var (
	// ErrNeedsReview mirrors internal/executor.ErrNeedsReview: an in-doubt
	// step (attempted, no probe, no recorded result) needs an operator.
	ErrNeedsReview = errors.New("engine: step needs operator review")
	// ErrToolFailed mirrors internal/executor.ErrToolFailed.
	ErrToolFailed = errors.New("engine: step failed")
	// ErrWarrantDenied mirrors internal/executor.ErrWarrantDenied.
	ErrWarrantDenied = errors.New("engine: warrant denied")
	// ErrSimulatedCrash, returned from a StepFunc, makes a conforming Engine
	// record the attempt (so recovery treats the step as in-doubt) without
	// recording any final status — exactly like a real process dying
	// between invoking the tool and writing its result. It exists so the
	// conformance suite can test crash/recovery identically on every
	// engine, the same way internal/executor.ErrSimulatedCrash lets
	// internal/worker's tests do it for the production runtime.
	ErrSimulatedCrash = errors.New("engine: simulated crash")
)

// StepFunc performs one effect's real work.
type StepFunc func(ctx context.Context) (json.RawMessage, error)

// Prober checks whether a step's work already happened, for reconciling an
// in-doubt attempt without needs_review — the engine-level equivalent of
// internal/registry.Prober.
type Prober func(ctx context.Context) (happened bool, result json.RawMessage, err error)

// Engine is the durability substrate: schedule a step exactly once (with
// crash recovery via attempts + an optional probe), wait durably, signal,
// and query. All methods must be safe for concurrent use, including
// concurrent calls with the *same* key (only one attempt of a step's
// StepFunc may ever be "in flight" for a key at a time; the others observe
// its outcome).
type Engine interface {
	Name() string

	// ScheduleStep durably runs fn exactly once for key and returns its
	// result. A later call with the same key, from any process, returns
	// the same recorded outcome without invoking fn again — unless the
	// previous attempt is in doubt (recorded but never resolved, e.g. after
	// ErrSimulatedCrash) and either probe reports the work already
	// happened (result adopted, fn not called) or, absent a probe,
	// ErrNeedsReview is returned and an operator must call Resolve before
	// any later ScheduleStep call will try fn again.
	ScheduleStep(ctx context.Context, key string, fn StepFunc, probe Prober) (json.RawMessage, error)

	// StepStatus reports what is durably known about key without running
	// anything: status is "" (never scheduled), "intent" (in doubt),
	// "committed", "failed", or "needs_review".
	StepStatus(ctx context.Context, key string) (status string, result json.RawMessage, errMsg string, err error)

	// Resolve settles a needs_review step: action is "retry" (the next
	// ScheduleStep call runs fn again), "committed" (records result as the
	// operator-supplied truth), or "failed".
	Resolve(ctx context.Context, key, action string, result json.RawMessage) error

	// Timer durably waits until d has elapsed since the *first* call with
	// this key: a process that resumes waiting (new context, same key)
	// picks up the original deadline rather than restarting the clock.
	Timer(ctx context.Context, key string, d time.Duration) error

	// Signal delivers payload under key/name; AwaitSignal blocks until one
	// is delivered (FIFO per key/name) or ctx ends.
	Signal(ctx context.Context, key, name string, payload json.RawMessage) error
	AwaitSignal(ctx context.Context, key, name string) (json.RawMessage, error)
}

// Auth carries the Warrant credentials to authorize a step with.
type Auth struct {
	Token string
	SVID  string
}

// Runtime is Harbour's governance layer, expressed purely against Engine:
// idempotency-key derivation (reusing internal/executor's exact scheme, so
// a key computed here and one computed by the production executor for the
// same tool+args+goal+step are identical), Warrant authorization before any
// attempt, and Ledger receipts around it. It behaves the same regardless of
// which Engine runs it, which is what the conformance suite checks.
type Runtime struct {
	Eng     Engine
	Warrant warrant.Client // nil: Warrant integration off
	Ledger  ledger.Recorder // nil: no receipts
}

// RunEffect is the Engine-based equivalent of internal/executor.Run.
func (r *Runtime) RunEffect(ctx context.Context, goalID string, step int, tool string, args json.RawMessage, auth Auth, work StepFunc, probe Prober) (json.RawMessage, error) {
	key, canon, err := executor.Key(tool, args, goalID, step)
	if err != nil {
		return nil, fmt.Errorf("args: %w", err)
	}
	if r.Warrant != nil {
		var argMap map[string]any
		if len(canon) > 0 {
			_ = json.Unmarshal(canon, &argMap)
		}
		d, aerr := r.Warrant.Authorize(ctx, auth.Token, auth.SVID, warrant.Call{Tool: tool, Args: argMap})
		if aerr != nil {
			return nil, fmt.Errorf("%w: %v", ErrWarrantDenied, aerr)
		}
		if !d.Allow {
			reason := d.Reason
			if reason == "" {
				reason = "denied"
			}
			// Deny happens before ScheduleStep: the idempotency key is
			// never touched, so a later allow re-authorizes cleanly.
			return nil, fmt.Errorf("%w: %s", ErrWarrantDenied, reason)
		}
	}
	r.record(ctx, goalID, "harbour.effect.intent", map[string]any{"tool": tool, "step": step, "idem_key": key, "args": json.RawMessage(canon)})
	res, err := r.Eng.ScheduleStep(ctx, key, work, probe)
	status, errMsg := classify(err)
	r.record(ctx, goalID, "harbour.effect.result", map[string]any{"tool": tool, "step": step, "idem_key": key, "status": status, "error": errMsg})
	return res, err
}

func classify(err error) (status, errMsg string) {
	switch {
	case err == nil:
		return "committed", ""
	case errors.Is(err, ErrNeedsReview):
		return "needs_review", ""
	case errors.Is(err, ErrSimulatedCrash):
		return "intent", ""
	default:
		return "failed", err.Error()
	}
}

func (r *Runtime) record(ctx context.Context, goalID, typ string, payload map[string]any) {
	if r.Ledger == nil {
		return
	}
	_ = r.Ledger.Record(ctx, ledger.Record{
		Chain:      "harbour",
		Type:       typ,
		GoalID:     goalID,
		ActorChain: []ledger.Actor{{Kind: "human", ID: "engine-runtime"}, {Kind: "service", ID: "harbourd/engine"}},
		Payload:    payload,
	})
}
