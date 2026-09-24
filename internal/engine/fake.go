package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Fake is a minimal, in-process, non-Postgres Engine implementation. It
// exists to prove that Harbour's governance semantics (Runtime, and the
// conformance suite) do not secretly depend on the built-in Postgres
// engine: everything the production runtime relies on — exactly-once
// idempotent steps, needs_review without a probe, probe-based
// reconciliation, resolve/retry, durable timers and signals — is
// implemented here from scratch over plain Go maps and channels, and passes
// the identical conformance suite.
type Fake struct {
	mu      sync.Mutex
	steps   map[string]*fakeStep
	timers  map[string]time.Time
	signals map[string]chan json.RawMessage
}

type fakeStep struct {
	mu       sync.Mutex
	status   string // "intent", "committed", "failed", "needs_review"
	result   json.RawMessage
	errMsg   string
	attempts int
}

func NewFake() *Fake {
	return &Fake{steps: map[string]*fakeStep{}, timers: map[string]time.Time{}, signals: map[string]chan json.RawMessage{}}
}

func (f *Fake) Name() string { return "fake" }

func (f *Fake) step(key string) *fakeStep {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.steps[key]
	if !ok {
		s = &fakeStep{}
		f.steps[key] = s
	}
	return s
}

func (f *Fake) ScheduleStep(ctx context.Context, key string, fn StepFunc, probe Prober) (json.RawMessage, error) {
	s := f.step(key)
	s.mu.Lock()
	defer s.mu.Unlock()

	switch s.status {
	case "committed":
		return s.result, nil
	case "failed":
		return nil, fmt.Errorf("%w: %s", ErrToolFailed, s.errMsg)
	case "needs_review":
		return nil, ErrNeedsReview
	case "intent":
		// Already attempted (attempts>0 means an earlier call started fn
		// but the process "crashed" before recording a result): reconcile.
		if s.attempts > 0 {
			if probe != nil {
				happened, res, perr := probe(ctx)
				if perr == nil && happened {
					s.status, s.result = "committed", res
					return res, nil
				}
			} else {
				s.status, s.errMsg = "needs_review", "in-doubt after crash; no probe"
				return nil, ErrNeedsReview
			}
		}
	}
	// status == "" (new) or "intent" with no prior attempt, or a probe-miss
	// retry: run it.
	s.status = "intent"
	s.attempts++
	res, err := fn(ctx)
	if errors.Is(err, ErrSimulatedCrash) {
		// Leave status "intent": the attempt happened, nothing is settled.
		return nil, err
	}
	if err != nil {
		s.status, s.errMsg = "failed", err.Error()
		return nil, fmt.Errorf("%w: %s", ErrToolFailed, err.Error())
	}
	s.status, s.result = "committed", res
	return res, nil
}

func (f *Fake) StepStatus(_ context.Context, key string) (string, json.RawMessage, string, error) {
	f.mu.Lock()
	s, ok := f.steps[key]
	f.mu.Unlock()
	if !ok {
		return "", nil, "", nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.result, s.errMsg, nil
}

func (f *Fake) Resolve(_ context.Context, key, action string, result json.RawMessage) error {
	f.mu.Lock()
	s, ok := f.steps[key]
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown key %q", key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != "needs_review" {
		return fmt.Errorf("not in needs_review (status=%s)", s.status)
	}
	switch action {
	case "retry":
		s.status, s.attempts, s.errMsg = "intent", 0, ""
	case "committed":
		s.status, s.result, s.errMsg = "committed", result, ""
	case "failed":
		s.status, s.errMsg = "failed", "operator marked failed"
	default:
		return fmt.Errorf("action must be retry|committed|failed")
	}
	return nil
}

func (f *Fake) Timer(ctx context.Context, key string, d time.Duration) error {
	f.mu.Lock()
	deadline, ok := f.timers[key]
	if !ok {
		deadline = time.Now().Add(d)
		f.timers[key] = deadline
	}
	f.mu.Unlock()
	wait := time.Until(deadline)
	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (f *Fake) sigChan(key, name string) chan json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key + "\x00" + name
	ch, ok := f.signals[k]
	if !ok {
		ch = make(chan json.RawMessage, 64)
		f.signals[k] = ch
	}
	return ch
}

func (f *Fake) Signal(_ context.Context, key, name string, payload json.RawMessage) error {
	f.sigChan(key, name) <- payload
	return nil
}

func (f *Fake) AwaitSignal(ctx context.Context, key, name string) (json.RawMessage, error) {
	select {
	case p := <-f.sigChan(key, name):
		return p, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
