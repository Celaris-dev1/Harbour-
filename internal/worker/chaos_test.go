package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Celaris-dev1/Harbour-/internal/fsm"
	"github.com/Celaris-dev1/Harbour-/internal/store"
)

// Chaos coverage beyond the crash/fencing tests elsewhere in this package:
//   - "kill worker mid-effect" is exercised for real (a separate OS process,
//     SIGKILL-equivalent exit) by cmd/harbourd's
//     TestProcessKilledMidRunResumesWithoutDoubleExecution, and in-process by
//     TestCrashBetweenEffectAndResult_ProbeReconciles /
//     TestCrashNoProbe_NeedsReview / TestRepeatedCrashesNeverDoubleExecute.
//   - "lease steal" under normal expiry is TestStaleLeaseIsFenced.
//
// This file adds the two variants those don't cover: a lease stolen while
// the original owner is still actively (concurrently, not just logically)
// mid-step, and a lease that expires "early" purely because the two
// workers' clocks disagree (clock skew), not because anyone was actually
// slow.

// TestChaosLeaseStealWhileOriginalOwnerStillRunning simulates clock skew by
// force-expiring worker A's lease (as if the DB server's clock had run
// ahead of A's) *while A is still concurrently in the middle of a step*,
// then lets a second worker steal it and drive the goal to completion.
// When A's stale goroutine finally tries to write, it must be fenced off,
// and the goal must show each line exactly once — proving the epoch fence
// protects correctness even when the "crash" is really just a clock
// disagreement rather than an actual process death.
func TestChaosLeaseStealWhileOriginalOwnerStillRunning(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	gate := make(chan struct{})
	e.reg.AddAgent(slowAgent{countAgent{3}, gate})
	g := e.submit(t, "skew", "slow", `{}`)

	doneA := make(chan error, 1)
	go func() { _, err := e.worker("A").RunOnce(ctx); doneA <- err }()

	// Wait until A has committed step 0 and is now blocked inside step 1's
	// agent.Next (holding a lease it believes is valid for another minute).
	for e.state(t, g.ID).Cursor < 1 {
		time.Sleep(10 * time.Millisecond)
	}

	// Simulate clock skew: force the lease to look expired *right now*,
	// independent of the TTL A thinks it has. A is still running (blocked
	// on gate), unaware.
	e.expireLeases(t)

	// A second worker, with a "faster" clock, claims the now-expired lease
	// and races A to finish the goal.
	b := e.worker("B")
	go b.Loop(ctx)

	// Now let A proceed: its in-flight step 1 write must be fenced (B has a
	// higher epoch), so A must not double-commit step 1.
	close(gate)
	if err := <-doneA; err != nil && !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("worker A: unexpected error %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e.state(t, g.ID).State == fsm.Done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cur := e.state(t, g.ID)
	if cur.State != fsm.Done {
		t.Fatalf("goal never finished: state=%s cursor=%d", cur.State, cur.Cursor)
	}
	// Exactly 3 real tool invocations total, and no key invoked more than
	// once, whichever worker happened to win each step.
	if e.c.total() != 3 || e.c.max() != 1 {
		t.Fatalf("total=%d max=%d, want total=3 max=1 (no double execution despite the clock-skewed lease steal)", e.c.total(), e.c.max())
	}
}

// TestChaosLeaseTTLSurvivesClockSkewViaFencingNotWallClock checks the
// mechanism directly: two claims of the same goal (simulating two nodes
// whose clocks disagree about whether a lease has expired) always produce
// strictly increasing epochs, and a write fenced on the older epoch is
// rejected even though, from that worker's own (skewed) clock, its lease
// still looks time-valid.
func TestChaosLeaseTTLSurvivesClockSkewViaFencingNotWallClock(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	g := e.submit(t, "skew2", "count", `{}`)

	a, err := e.st.Claim(ctx, "A", time.Hour) // A's clock: "I have an hour"
	if err != nil || a == nil {
		t.Fatalf("claim A: %v %v", err, a)
	}
	// The DB's clock (or an operator/monitor with a different clock) thinks
	// the lease is long expired, independent of what A believes its TTL is.
	e.expireLeases(t)
	b, err := e.st.Claim(ctx, "B", time.Hour)
	if err != nil || b == nil {
		t.Fatalf("claim B: %v %v", err, b)
	}
	if b.LeaseEpoch <= a.LeaseEpoch {
		t.Fatalf("epoch did not advance: a=%d b=%d", a.LeaseEpoch, b.LeaseEpoch)
	}
	// A, still trusting its own clock, tries to act: it must be fenced by
	// epoch, not by re-checking wall-clock time.
	la := store.Lease{GoalID: g.ID, Worker: "A", Epoch: a.LeaseEpoch}
	if _, err := e.st.TransitionFenced(ctx, la, fsm.Executing, "A still thinks it owns this"); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("fenced transition under A's stale epoch: err=%v, want ErrLeaseLost", err)
	}
	lb := store.Lease{GoalID: g.ID, Worker: "B", Epoch: b.LeaseEpoch}
	if _, err := e.st.TransitionFenced(ctx, lb, fsm.Executing, "B took over"); err != nil {
		t.Fatalf("B's fenced transition should succeed: %v", err)
	}
}
