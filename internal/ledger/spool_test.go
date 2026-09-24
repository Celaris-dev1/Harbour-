package ledger

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRecorder fails the first `failN` calls (per key, if keyed), then
// succeeds. It records everything it ever received a successful call for.
type fakeRecorder struct {
	mu      sync.Mutex
	fail    int32 // calls remaining to fail, atomically decremented
	got     []Record
	calls   int32
}

func (f *fakeRecorder) Record(_ context.Context, r Record) error {
	atomic.AddInt32(&f.calls, 1)
	if atomic.AddInt32(&f.fail, -1) >= 0 {
		return fmt.Errorf("simulated ledger outage")
	}
	f.mu.Lock()
	f.got = append(f.got, r)
	f.mu.Unlock()
	return nil
}

func rec(goalID string) Record {
	return Record{Type: "harbour.goal.transition", GoalID: goalID, ActorChain: []Actor{{Kind: "human", ID: "op"}}, Payload: map[string]any{"x": 1}}
}

func TestSpoolNeverDropsWhileDown(t *testing.T) {
	dir := t.TempDir()
	fr := &fakeRecorder{fail: 1000} // always failing
	sp := NewSpool(dir, fr)
	sp.Interval = 5 * time.Millisecond

	for i := 0; i < 10; i++ {
		if err := sp.Record(context.Background(), rec(fmt.Sprintf("g_%d", i))); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	// Nothing delivered yet: Ledger is down.
	sent, ok := sp.FlushOnce(context.Background())
	if sent != 0 || ok {
		t.Fatalf("expected no delivery while down, got sent=%d ok=%v", sent, ok)
	}
	if got := len(fr.got); got != 0 {
		t.Fatalf("fake recorder got %d records while down", got)
	}

	// Ledger recovers.
	atomic.StoreInt32(&fr.fail, 0)
	sent, ok = sp.FlushOnce(context.Background())
	if !ok || sent != 10 {
		t.Fatalf("flush after recovery: sent=%d ok=%v", sent, ok)
	}
	if len(fr.got) != 10 {
		t.Fatalf("expected 10 delivered, got %d", len(fr.got))
	}
	for i, r := range fr.got {
		if r.GoalID != fmt.Sprintf("g_%d", i) {
			t.Fatalf("delivered out of order: index %d has goal %s", i, r.GoalID)
		}
	}
}

func TestSpoolSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	down := &fakeRecorder{fail: 1000}
	sp1 := NewSpool(dir, down)
	for i := 0; i < 5; i++ {
		if err := sp1.Record(context.Background(), rec(fmt.Sprintf("r_%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	// sp1 "crashes": no Start was ever called, nothing was ever delivered.

	// A fresh Spool over the same directory (simulating a process restart)
	// must still find and deliver every record.
	up := &fakeRecorder{}
	sp2 := NewSpool(dir, up)
	sent, ok := sp2.FlushOnce(context.Background())
	if !ok || sent != 5 {
		t.Fatalf("restart flush: sent=%d ok=%v", sent, ok)
	}
	if len(up.got) != 5 {
		t.Fatalf("expected 5 delivered after restart, got %d", len(up.got))
	}
}

func TestSpoolConcurrentRecordIsRaceSafe(t *testing.T) {
	dir := t.TempDir()
	up := &fakeRecorder{}
	sp := NewSpool(dir, up)

	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := sp.Record(context.Background(), rec(fmt.Sprintf("c_%d", i))); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()

	sent, ok := sp.FlushOnce(context.Background())
	if !ok || sent != n {
		t.Fatalf("sent=%d ok=%v want %d", sent, ok, n)
	}
	seen := map[string]bool{}
	for _, r := range up.got {
		if seen[r.GoalID] {
			t.Fatalf("duplicate delivery of %s", r.GoalID)
		}
		seen[r.GoalID] = true
	}
	if len(seen) != n {
		t.Fatalf("delivered %d distinct records, want %d", len(seen), n)
	}
}

func TestSpoolBackgroundLoopDelivers(t *testing.T) {
	dir := t.TempDir()
	fr := &fakeRecorder{fail: 2} // fails twice, then succeeds
	sp := NewSpool(dir, fr)
	sp.Interval = 5 * time.Millisecond
	sp.MaxBackoff = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sp.Start(ctx)
	defer sp.Stop()

	if err := sp.Record(context.Background(), rec("bg_1")); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(2 * time.Second)
	for {
		fr.mu.Lock()
		n := len(fr.got)
		fr.mu.Unlock()
		if n == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("background loop never delivered the record")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestSpoolStartIdempotent(t *testing.T) {
	dir := t.TempDir()
	sp := NewSpool(dir, &fakeRecorder{})
	ctx, cancel := context.WithCancel(context.Background())
	sp.Start(ctx)
	sp.Start(ctx) // must not spawn a second loop or panic
	cancel()
	sp.Stop()
}

func TestSpoolFlushOnceEmptyDirIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	sp := NewSpool(dir, &fakeRecorder{})
	sent, ok := sp.FlushOnce(context.Background())
	if sent != 0 || !ok {
		t.Fatalf("sent=%d ok=%v", sent, ok)
	}
}
