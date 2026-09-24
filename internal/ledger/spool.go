package ledger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"
)

// Spool makes Record calls durable even when the underlying Recorder (Ledger
// over HTTP) is unreachable: every record is first fsync'd to a local file
// under Dir before Record returns, so a crash right after Record never loses
// it. A background loop retries delivery with backoff, in file (creation)
// order, and survives process restarts because it simply re-scans Dir on
// Start. Multiple goroutines may call Record concurrently; each write uses a
// unique file name and an atomic create-then-rename, so no writer can
// observe or corrupt another's file.
type Spool struct {
	Dir      string
	Next     Recorder      // the real recorder to retry against (e.g. *HTTP)
	Interval time.Duration // base retry interval; defaults to 2s
	MaxBackoff time.Duration // defaults to 30s
	Log      *slog.Logger

	seq     uint64
	started int32
	cancel  context.CancelFunc
	done    chan struct{}
}

// NewSpool builds a spool writing to dir and retrying against next. Call
// Start to begin the background retry loop (Record works, and is durable,
// even before Start is called or after the loop stops).
func NewSpool(dir string, next Recorder) *Spool {
	return &Spool{Dir: dir, Next: next, Interval: 2 * time.Second, MaxBackoff: 30 * time.Second, Log: slog.Default()}
}

func (s *Spool) pendingDir() string { return filepath.Join(s.Dir, "pending") }

// Record durably appends r to the spool and returns. It never contacts the
// network and only fails if the local filesystem write itself fails, so a
// down or slow Ledger never blocks or drops a caller's write.
func (s *Spool) Record(_ context.Context, r Record) error {
	if err := os.MkdirAll(s.pendingDir(), 0o755); err != nil {
		return fmt.Errorf("ledger spool: %w", err)
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	name := s.fileName()
	tmp := filepath.Join(s.pendingDir(), "."+name+".tmp")
	final := filepath.Join(s.pendingDir(), name)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("ledger spool: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("ledger spool: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("ledger spool: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("ledger spool: %w", err)
	}
	// Rename is atomic on the same filesystem: a reader of pendingDir never
	// sees a partially written file, and a crash between write and rename
	// just leaves an orphan .tmp file that is never picked up (and is safe
	// to clean up manually; the write itself is not yet acknowledged in
	// that narrow window, matching a crash before Record returns at all).
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("ledger spool: %w", err)
	}
	return nil
}

// fileName is monotonic (timestamp + process-local counter) plus a random
// suffix, so concurrent callers and concurrent processes sharing a dir never
// collide, and files sort into write order for in-order retry.
func (s *Spool) fileName() string {
	n := atomic.AddUint64(&s.seq, 1)
	var rnd [4]byte
	rand.Read(rnd[:])
	return fmt.Sprintf("%020d-%08x-%s.json", time.Now().UnixNano(), n, hex.EncodeToString(rnd[:]))
}

// Start begins the background retry loop. It is idempotent: calling it more
// than once is a no-op. Cancel ctx (or call Stop) to end it.
func (s *Spool) Start(ctx context.Context) {
	if !atomic.CompareAndSwapInt32(&s.started, 0, 1) {
		return
	}
	s.recoverOrphans()
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.done = make(chan struct{})
	go s.loop(ctx)
}

// Stop ends the background retry loop and waits for it to exit.
func (s *Spool) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.done != nil {
		<-s.done
	}
}

func (s *Spool) loop(ctx context.Context) {
	defer close(s.done)
	backoff := s.Interval
	if backoff <= 0 {
		backoff = 2 * time.Second
	}
	cur := backoff
	for {
		sent, allOK := s.FlushOnce(ctx)
		if allOK {
			cur = backoff
		} else {
			cur *= 2
			if s.MaxBackoff > 0 && cur > s.MaxBackoff {
				cur = s.MaxBackoff
			}
		}
		if sent > 0 && s.Log != nil {
			s.Log.Debug("ledger spool: flushed", "sent", sent, "all_ok", allOK)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(cur):
		}
	}
}

// FlushOnce attempts to deliver every currently spooled record, oldest
// first. It returns how many were delivered and whether every attempted
// delivery succeeded (false means Next is still down; remaining files are
// left in place for the next pass, in order).
func (s *Spool) FlushOnce(ctx context.Context) (sent int, allOK bool) {
	entries, err := os.ReadDir(s.pendingDir())
	if err != nil {
		return 0, os.IsNotExist(err) // an empty/missing spool dir is not a failure
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && filepath.Ext(n) == ".json" && n[0] != '.' {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	allOK = true
	for _, name := range names {
		if ctx.Err() != nil {
			return sent, false
		}
		ok, err := s.deliverOne(ctx, name)
		if err != nil && s.Log != nil {
			s.Log.Warn("ledger spool: deliver", "file", name, "err", err)
		}
		if !ok {
			allOK = false
			break // keep order: don't skip ahead of a record that isn't landing yet
		}
		sent++
	}
	return sent, allOK
}

// deliverOne claims name by renaming it to a .sending sibling (so a crash
// mid-delivery leaves at most one orphan claim, never a lost or duplicated
// pending file — on restart FlushOnce also picks up any leftover .sending
// file, since it is renamed back on failure), reads it, and hands it to
// Next. On success the file is removed; on failure it is renamed back to
// pending so the next pass retries it.
func (s *Spool) deliverOne(ctx context.Context, name string) (bool, error) {
	dir := s.pendingDir()
	from := filepath.Join(dir, name)
	claim := filepath.Join(dir, name+".sending")
	if err := os.Rename(from, claim); err != nil {
		if os.IsNotExist(err) {
			return true, nil // another goroutine/process already claimed or finished it
		}
		return false, err
	}
	b, err := os.ReadFile(claim)
	if err != nil {
		os.Rename(claim, from)
		return false, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		// Malformed spool file: nothing further can be done with it. Drop it
		// rather than retrying forever, but surface the error.
		os.Remove(claim)
		return true, fmt.Errorf("corrupt spool file %s: %w", name, err)
	}
	if err := s.Next.Record(ctx, r); err != nil {
		os.Rename(claim, from)
		return false, err
	}
	if err := os.Remove(claim); err != nil {
		return false, err
	}
	return true, nil
}

// recoverOrphans renames any leftover .sending files back to pending, for
// startup after a crash mid-delivery. FlushOnce/deliverOne handle the normal
// retry path; this only matters if a .sending file is left with no matching
// pending entry (i.e., the process died between claim and Next.Record).
func (s *Spool) recoverOrphans() {
	dir := s.pendingDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		n := e.Name()
		if filepath.Ext(n) == ".sending" {
			os.Rename(filepath.Join(dir, n), filepath.Join(dir, n[:len(n)-len(".sending")]))
		}
	}
}
