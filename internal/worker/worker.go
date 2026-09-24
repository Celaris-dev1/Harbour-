// Package worker claims goals under a lease, heartbeats, and drives them
// through the state machine one committed step at a time.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Celaris-dev1/Harbour-/internal/executor"
	"github.com/Celaris-dev1/Harbour-/internal/fsm"
	"github.com/Celaris-dev1/Harbour-/internal/registry"
	"github.com/Celaris-dev1/Harbour-/internal/store"
)

type Worker struct {
	ID       string
	Store    *store.Store
	Reg      *registry.Registry
	Exec     *executor.Executor
	LeaseTTL time.Duration
	Poll     time.Duration
	MaxSteps int
	Log      *slog.Logger
}

func New(id string, s *store.Store, reg *registry.Registry) *Worker {
	return &Worker{ID: id, Store: s, Reg: reg, Exec: &executor.Executor{Store: s, Reg: reg},
		LeaseTTL: 10 * time.Second, Poll: 500 * time.Millisecond, MaxSteps: 1000, Log: slog.Default()}
}

// Loop claims and runs goals until ctx is done.
func (w *Worker) Loop(ctx context.Context) {
	for ctx.Err() == nil {
		did, err := w.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			w.Log.Warn("worker", "id", w.ID, "err", err)
		}
		if !did {
			select {
			case <-ctx.Done():
			case <-time.After(w.Poll):
			}
		}
	}
}

// RunOnce claims at most one goal and drives it until it stops being runnable
// by this worker. Returns whether a goal was claimed.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	g, err := w.Store.Claim(ctx, w.ID, w.LeaseTTL)
	if err != nil || g == nil {
		return false, err
	}
	l := store.Lease{GoalID: g.ID, Worker: w.ID, Epoch: g.LeaseEpoch}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w.heartbeat(rctx, cancel, l)
	err = w.drive(rctx, l)
	if errors.Is(err, executor.ErrSimulatedCrash) {
		return true, err // die without releasing: the lease must expire
	}
	if errors.Is(err, store.ErrLeaseLost) {
		err = nil // paused/cancelled/stolen: another actor owns the goal now
	}
	w.Store.Release(context.WithoutCancel(ctx), l.GoalID, l.Worker, l.Epoch)
	return true, err
}

func (w *Worker) heartbeat(ctx context.Context, cancel context.CancelFunc, l store.Lease) {
	t := time.NewTicker(w.LeaseTTL / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.Store.Heartbeat(ctx, l.GoalID, l.Worker, l.Epoch, w.LeaseTTL); errors.Is(err, store.ErrLeaseLost) {
				cancel()
				return
			}
		}
	}
}

func (w *Worker) drive(ctx context.Context, l store.Lease) error {
	for i := 0; i < w.MaxSteps; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		g, err := w.Store.LoadFenced(ctx, l)
		if err != nil {
			return err
		}
		agent, err := w.Reg.Agent(g.Agent)
		if err != nil {
			_, err2 := w.Store.TransitionFenced(ctx, l, failFrom(g.State), "unknown agent: "+g.Agent)
			return errors.Join(err, err2)
		}
		switch g.State {
		case fsm.Approved:
			if _, err := w.Store.TransitionFenced(ctx, l, fsm.Executing, "claimed by "+w.ID); err != nil {
				return err
			}
		case fsm.Executing:
			done, err := w.step(ctx, l, g, agent)
			if err != nil || done {
				return err
			}
		case fsm.Verifying:
			sc, err := w.context(ctx, g)
			if err != nil {
				return err
			}
			if verr := agent.Verify(ctx, sc); verr != nil {
				_, err = w.Store.TransitionFenced(ctx, l, fsm.Failed, "verify: "+verr.Error())
			} else {
				_, err = w.Store.TransitionFenced(ctx, l, fsm.Done, "verified")
			}
			return err
		default:
			return nil
		}
	}
	return fmt.Errorf("max steps exceeded")
}

func failFrom(s fsm.State) fsm.State {
	if s == fsm.Approved {
		return fsm.Cancelled
	}
	return fsm.Failed
}

func (w *Worker) context(ctx context.Context, g *store.Goal) (registry.StepContext, error) {
	hist, err := w.Store.Steps(ctx, g.ID)
	if err != nil {
		return registry.StepContext{}, err
	}
	msgs, err := w.Store.Messages(ctx, g.ID)
	if err != nil {
		return registry.StepContext{}, err
	}
	return registry.StepContext{GoalID: g.ID, Step: g.Cursor, Input: g.Input, History: hist, Messages: msgs}, nil
}

// step runs exactly one step. Resume rule: if an effect row already exists
// for the cursor step, its recorded tool+args are used and the agent is NOT
// re-asked, so a non-deterministic agent cannot fork a second side effect.
func (w *Worker) step(ctx context.Context, l store.Lease, g *store.Goal, agent registry.Agent) (bool, error) {
	var act registry.Action
	existing, err := w.Store.EffectByStep(ctx, g.ID, g.Cursor)
	switch {
	case err == nil:
		act = registry.Action{Tool: existing.Tool, Args: existing.Args}
	case errors.Is(err, store.ErrNotFound):
		sc, err := w.context(ctx, g)
		if err != nil {
			return false, err
		}
		act, err = agent.Next(ctx, sc)
		if err != nil {
			_, err2 := w.Store.TransitionFenced(ctx, l, fsm.Failed, "agent: "+err.Error())
			return true, err2
		}
	default:
		return false, err
	}
	if act.Fail != "" {
		_, err := w.Store.TransitionFenced(ctx, l, fsm.Failed, act.Fail)
		return true, err
	}
	if act.Finish {
		_, err := w.Store.TransitionFenced(ctx, l, fsm.Verifying, "agent finished")
		return false, err
	}
	e, err := w.Exec.Run(ctx, l, g.Cursor, act.Tool, act.Args, executor.Auth{Token: g.WarrantToken, SVID: g.WarrantSVID})
	switch {
	case err == nil:
		return false, w.Store.CommitStep(ctx, l, e)
	case errors.Is(err, executor.ErrNeedsReview):
		_, err2 := w.Store.TransitionFenced(ctx, l, fsm.Paused, fmt.Sprintf("needs_review: effect %d (step %d, %s)", e.ID, e.Step, e.Tool))
		return true, err2
	case errors.Is(err, executor.ErrWarrantDenied):
		// The effect was never attempted: pause so an operator can grant
		// authority (revoke/re-mint the token) and resume, which
		// re-authorizes the same recorded tool+args from scratch.
		_, err2 := w.Store.TransitionFenced(ctx, l, fsm.Paused, fmt.Sprintf("warrant denied: step %d (%s): %s", g.Cursor, act.Tool, err.Error()))
		return true, err2
	case errors.Is(err, executor.ErrToolFailed):
		_, err2 := w.Store.TransitionFenced(ctx, l, fsm.Failed, err.Error())
		return true, err2
	}
	return false, err
}
