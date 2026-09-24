// Package executor runs tool calls idempotently with two-phase commit.
//
//	key := sha256(tool + "\n" + canonical(args) + "\n" + goalID + ":" + step)
//	(a) CommitIntent  -> effects row status=intent (durable before the effect)
//	(b) tool.Execute  -> the external side effect, given the key
//	(c) CommitResult  -> effects row status=committed|failed
//
// If an effect for the key is already committed, its stored result is returned
// and the tool is not called. If it is stuck in intent (a crash between (a)
// and (c)), Reconcile consults the tool's Prober; without a probe the effect is
// marked needs_review and never blindly retried.
package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/Celaris-dev1/Harbour-/internal/registry"
	"github.com/Celaris-dev1/Harbour-/internal/store"
	"github.com/Celaris-dev1/Harbour-/internal/warrant"
)

var (
	ErrNeedsReview = errors.New("effect needs operator review")
	ErrToolFailed  = errors.New("tool failed")
	// ErrSimulatedCrash, returned from a Hook, makes the worker abandon the
	// goal exactly as a dead process would: no result row, no lease release.
	ErrSimulatedCrash = errors.New("simulated crash")
	// ErrWarrantDenied means Warrant refused to authorize this effect. The
	// effect row is left untouched (still 'intent', attempts unchanged if
	// this happened before the attempt was recorded) so a resume safely
	// re-authorizes rather than replaying a stale denial.
	ErrWarrantDenied = errors.New("warrant denied")
)

// Canonical returns canonical JSON: object keys sorted, no whitespace.
func Canonical(raw json.RawMessage) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("null"), nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	writeCanon(&buf, v)
	return buf.Bytes(), nil
}

func writeCanon(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			b.Write(kb)
			b.WriteByte(':')
			writeCanon(b, x[k])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanon(b, e)
		}
		b.WriteByte(']')
	default:
		eb, _ := json.Marshal(x)
		b.Write(eb)
	}
}

// Key computes the idempotency key.
func Key(tool string, args json.RawMessage, goalID string, step int) (string, []byte, error) {
	c, err := Canonical(args)
	if err != nil {
		return "", nil, err
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%s:%d", tool, c, goalID, step)))
	return hex.EncodeToString(h[:]), c, nil
}

// Hooks let tests simulate a process crash at precise points. A hook
// returning an error aborts Run immediately, leaving the DB as a crash would.
type Hooks struct {
	AfterIntent func(e *store.Effect) error
	AfterEffect func(e *store.Effect) error
}

type Executor struct {
	Store   *store.Store
	Reg     *registry.Registry
	Hooks   Hooks
	// Warrant, when non-nil, is consulted before every tool invocation
	// (fresh, retried, or recovered after a crash). A deny returns
	// ErrWarrantDenied without ever calling the tool.
	Warrant warrant.Client
}

// Auth carries the Warrant credentials to authorize a step's effect with,
// normally the goal's WarrantToken/WarrantSVID.
type Auth struct {
	Token string
	SVID  string
}

// Run executes one step's tool call exactly once (effectively).
func (x *Executor) Run(ctx context.Context, l store.Lease, step int, tool string, args json.RawMessage, auth Auth) (*store.Effect, error) {
	t, err := x.Reg.Tool(tool)
	if err != nil {
		return nil, err
	}
	key, canon, err := Key(tool, args, l.GoalID, step)
	if err != nil {
		return nil, fmt.Errorf("args: %w", err)
	}
	e, created, err := x.Store.CommitIntent(ctx, l, key, step, tool, canon)
	if err != nil {
		return nil, err
	}
	if !created {
		if e.Key != key {
			return nil, fmt.Errorf("step %d already bound to a different call (key %s)", step, e.Key)
		}
		return x.settle(ctx, t, e, auth)
	}
	if x.Hooks.AfterIntent != nil {
		if err := x.Hooks.AfterIntent(e); err != nil {
			return nil, err
		}
	}
	return x.execute(ctx, t, e, "execute", auth)
}

// settle handles an effect row that already existed.
func (x *Executor) settle(ctx context.Context, t registry.Tool, e *store.Effect, auth Auth) (*store.Effect, error) {
	switch e.Status {
	case "committed":
		return e, nil // idempotent replay: stored result, no re-run
	case "failed":
		return e, fmt.Errorf("%w: %s", ErrToolFailed, e.Error)
	case "needs_review":
		return e, ErrNeedsReview
	}
	return x.Reconcile(ctx, t, e, auth)
}

// Reconcile resolves an in-doubt (intent) effect after a crash.
func (x *Executor) Reconcile(ctx context.Context, t registry.Tool, e *store.Effect, auth Auth) (*store.Effect, error) {
	if e.Attempts == 0 {
		// Intent committed but the tool was never invoked: safe to run.
		return x.execute(ctx, t, e, "execute-after-recovery", auth)
	}
	p, ok := t.(registry.Prober)
	if !ok {
		if err := x.Store.MarkNeedsReview(ctx, e.ID, "in-doubt after crash; tool has no reconciliation probe"); err != nil {
			return nil, err
		}
		return e, ErrNeedsReview
	}
	pr, err := p.Probe(ctx, e.Key, e.Args)
	if err != nil {
		if err2 := x.Store.MarkNeedsReview(ctx, e.ID, "probe error: "+err.Error()); err2 != nil {
			return nil, err2
		}
		return e, ErrNeedsReview
	}
	if pr.Happened {
		ne, err := x.Store.CommitResult(ctx, e.ID, "committed", pr.Result, "", "probe")
		if err != nil {
			return nil, err
		}
		return x.settleFinal(ne)
	}
	return x.execute(ctx, t, e, "retry-after-probe", auth)
}

// authorize consults Warrant, if configured, before a tool is ever invoked.
func (x *Executor) authorize(ctx context.Context, e *store.Effect, auth Auth) error {
	if x.Warrant == nil {
		return nil
	}
	var argMap map[string]any
	if len(e.Args) > 0 {
		if err := json.Unmarshal(e.Args, &argMap); err != nil {
			argMap = map[string]any{"_raw": string(e.Args)}
		}
	}
	d, err := x.Warrant.Authorize(ctx, auth.Token, auth.SVID, warrant.Call{Tool: e.Tool, Args: argMap})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrWarrantDenied, err)
	}
	if !d.Allow {
		reason := d.Reason
		if reason == "" {
			reason = "denied"
		}
		return fmt.Errorf("%w: %s", ErrWarrantDenied, reason)
	}
	return nil
}

func (x *Executor) execute(ctx context.Context, t registry.Tool, e *store.Effect, via string, auth Auth) (*store.Effect, error) {
	if err := x.authorize(ctx, e, auth); err != nil {
		return e, err
	}
	// Record the attempt *before* invoking the tool: attempts>0 means "the
	// effect may have happened", which is what recovery keys off.
	if err := x.Store.MarkAttempt(ctx, e.ID); err != nil {
		return nil, err
	}
	res, terr := t.Execute(ctx, e.Key, e.Args)
	if x.Hooks.AfterEffect != nil {
		if err := x.Hooks.AfterEffect(e); err != nil {
			return nil, err
		}
	}
	var ne *store.Effect
	var err error
	if terr != nil {
		ne, err = x.Store.CommitResult(context.WithoutCancel(ctx), e.ID, "failed", nil, terr.Error(), via)
	} else {
		ne, err = x.Store.CommitResult(context.WithoutCancel(ctx), e.ID, "committed", res, "", via)
	}
	if err != nil {
		return nil, err
	}
	return x.settleFinal(ne)
}

func (x *Executor) settleFinal(e *store.Effect) (*store.Effect, error) {
	switch e.Status {
	case "committed":
		return e, nil
	case "failed":
		return e, fmt.Errorf("%w: %s", ErrToolFailed, e.Error)
	}
	return e, ErrNeedsReview
}
