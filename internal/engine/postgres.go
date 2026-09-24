package engine

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed engine_schema.sql
var engineSchema string

// Postgres is the built-in, default Engine implementation: the same
// database Harbour already runs on, using its own tables (engine_schema.sql)
// so it never collides with internal/store's.
type Postgres struct {
	Pool *pgxpool.Pool
}

// OpenPostgres opens (and migrates) a Postgres-backed Engine.
func OpenPostgres(ctx context.Context, url string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, `SELECT pg_advisory_lock(7263099)`); err != nil {
		pool.Close()
		return nil, err
	}
	_, err = pool.Exec(ctx, engineSchema)
	pool.Exec(ctx, `SELECT pg_advisory_unlock(7263099)`)
	if err != nil {
		pool.Close()
		return nil, err
	}
	return &Postgres{Pool: pool}, nil
}

func (p *Postgres) Close() { p.Pool.Close() }
func (p *Postgres) Name() string { return "postgres" }

type stepRow struct {
	status   string
	result   json.RawMessage
	errMsg   string
	attempts int
}

func (p *Postgres) getStepForUpdate(ctx context.Context, tx pgx.Tx, key string) (*stepRow, error) {
	var r stepRow
	var res []byte
	err := tx.QueryRow(ctx, `SELECT status,result,error,attempts FROM engine_steps WHERE key=$1 FOR UPDATE`, key).Scan(&r.status, &res, &r.errMsg, &r.attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if res != nil {
		r.result = res
	}
	return &r, nil
}

// ScheduleStep. See Engine for the contract; this uses a row per key
// (engine_steps), FOR UPDATE to serialize concurrent callers on the same
// key, mirroring internal/store's effects table.
func (p *Postgres) ScheduleStep(ctx context.Context, key string, fn StepFunc, probe Prober) (json.RawMessage, error) {
	action, err := p.claim(ctx, key)
	if err != nil {
		return nil, err
	}
	switch action {
	case actionReturnCommitted:
		return p.currentResult(ctx, key)
	case actionReturnFailed:
		_, _, errMsg, err := p.StepStatus(ctx, key)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", ErrToolFailed, errMsg)
	case actionNeedsReviewNow:
		return nil, ErrNeedsReview
	case actionReconcile:
		// attempts>0: the tool may already have run (a real crash between
		// invoking it and recording the result). Only the caller knows
		// whether a probe is available; the DB state alone can't decide.
		if probe == nil {
			if err := p.markNeedsReview(ctx, key, "in-doubt after crash; no probe"); err != nil {
				return nil, err
			}
			return nil, ErrNeedsReview
		}
		happened, res, perr := probe(ctx)
		if perr == nil && happened {
			if err := p.commit(ctx, key, "committed", res, ""); err != nil {
				return nil, err
			}
			return res, nil
		}
		return p.attempt(ctx, key, fn)
	case actionRun:
		return p.attempt(ctx, key, fn)
	}
	return nil, fmt.Errorf("engine: unreachable action %v", action)
}

type claimAction int

const (
	actionRun claimAction = iota
	actionReturnCommitted
	actionReturnFailed
	actionNeedsReviewNow
	actionReconcile
)

// claim opens (or finds) the row under FOR UPDATE and decides, still inside
// the transaction, what ScheduleStep should do next; it commits the
// transaction itself (so the row lock is released) before returning, since
// running fn or probe must not hold a DB row lock across arbitrary work.
func (p *Postgres) claim(ctx context.Context, key string) (claimAction, error) {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	row, err := p.getStepForUpdate(ctx, tx, key)
	if err != nil {
		return 0, err
	}
	if row == nil {
		if _, err := tx.Exec(ctx, `INSERT INTO engine_steps(key,status) VALUES($1,'intent')`, key); err != nil {
			return 0, err
		}
		return actionRun, tx.Commit(ctx)
	}
	switch row.status {
	case "committed":
		return actionReturnCommitted, tx.Commit(ctx)
	case "failed":
		return actionReturnFailed, tx.Commit(ctx)
	case "needs_review":
		return actionNeedsReviewNow, tx.Commit(ctx)
	}
	// intent
	if row.attempts == 0 {
		return actionRun, tx.Commit(ctx)
	}
	return actionReconcile, tx.Commit(ctx)
}

// attempt marks the attempt, runs fn outside any DB transaction, and
// records the outcome. If fn returns ErrSimulatedCrash the row is left at
// status 'intent' with attempts already bumped, exactly modeling a process
// that died between invoking the tool and recording its result.
func (p *Postgres) attempt(ctx context.Context, key string, fn StepFunc) (json.RawMessage, error) {
	if _, err := p.Pool.Exec(ctx, `UPDATE engine_steps SET attempts=attempts+1, updated_at=now() WHERE key=$1`, key); err != nil {
		return nil, err
	}
	res, err := fn(ctx)
	if errors.Is(err, ErrSimulatedCrash) {
		return nil, err
	}
	if err != nil {
		if cerr := p.commit(ctx, key, "failed", nil, err.Error()); cerr != nil {
			return nil, cerr
		}
		return nil, fmt.Errorf("%w: %s", ErrToolFailed, err.Error())
	}
	if err := p.commit(ctx, key, "committed", res, ""); err != nil {
		return nil, err
	}
	return res, nil
}

func (p *Postgres) commit(ctx context.Context, key, status string, result json.RawMessage, errMsg string) error {
	var r any
	if len(result) > 0 {
		r = []byte(result)
	}
	_, err := p.Pool.Exec(ctx, `UPDATE engine_steps SET status=$2, result=$3, error=$4, updated_at=now() WHERE key=$1 AND status='intent'`, key, status, r, errMsg)
	return err
}

func (p *Postgres) markNeedsReview(ctx context.Context, key, why string) error {
	_, err := p.Pool.Exec(ctx, `UPDATE engine_steps SET status='needs_review', error=$2, updated_at=now() WHERE key=$1 AND status='intent'`, key, why)
	return err
}

func (p *Postgres) currentResult(ctx context.Context, key string) (json.RawMessage, error) {
	_, res, _, err := p.StepStatus(ctx, key)
	return res, err
}

func (p *Postgres) StepStatus(ctx context.Context, key string) (string, json.RawMessage, string, error) {
	var status, errMsg string
	var res []byte
	err := p.Pool.QueryRow(ctx, `SELECT status,result,error FROM engine_steps WHERE key=$1`, key).Scan(&status, &res, &errMsg)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, "", nil
	}
	if err != nil {
		return "", nil, "", err
	}
	return status, res, errMsg, nil
}

func (p *Postgres) Resolve(ctx context.Context, key, action string, result json.RawMessage) error {
	var status string
	switch action {
	case "retry":
		status = "intent"
	case "committed", "failed":
		status = action
	default:
		return fmt.Errorf("action must be retry|committed|failed")
	}
	var r any
	if len(result) > 0 {
		r = []byte(result)
	}
	ct, err := p.Pool.Exec(ctx, `UPDATE engine_steps SET status=$2, result=$3, error=CASE WHEN $2='failed' THEN 'operator marked failed' ELSE '' END,
		attempts=CASE WHEN $2='intent' THEN 0 ELSE attempts END, updated_at=now() WHERE key=$1 AND status='needs_review'`, key, status, r)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("key %q is not in needs_review", key)
	}
	return nil
}

func (p *Postgres) Timer(ctx context.Context, key string, d time.Duration) error {
	var deadline time.Time
	err := p.Pool.QueryRow(ctx, `INSERT INTO engine_timers(key,deadline) VALUES($1, now()+$2::interval)
		ON CONFLICT (key) DO UPDATE SET key=engine_timers.key RETURNING deadline`, key, d.String()).Scan(&deadline)
	if err != nil {
		return err
	}
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

func (p *Postgres) Signal(ctx context.Context, key, name string, payload json.RawMessage) error {
	var b any
	if len(payload) > 0 {
		b = []byte(payload)
	} else {
		b = []byte("null")
	}
	_, err := p.Pool.Exec(ctx, `INSERT INTO engine_signals(key,name,payload) VALUES($1,$2,$3)`, key, name, b)
	return err
}

func (p *Postgres) AwaitSignal(ctx context.Context, key, name string) (json.RawMessage, error) {
	for {
		var payload []byte
		var id int64
		err := p.Pool.QueryRow(ctx, `DELETE FROM engine_signals WHERE id = (
			SELECT id FROM engine_signals WHERE key=$1 AND name=$2 ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED
		) RETURNING id, payload`, key, name).Scan(&id, &payload)
		if err == nil {
			return payload, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Millisecond):
		}
	}
}
