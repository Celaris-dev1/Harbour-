// Package store is the Postgres-backed durable state for Harbour.
package store

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/Celaris-dev1/Harbour-/internal/fsm"
	"github.com/Celaris-dev1/Harbour-/internal/ledger"
	"github.com/Celaris-dev1/Harbour-/internal/registry"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

var (
	ErrNotFound  = errors.New("not found")
	ErrLeaseLost = errors.New("lease lost")
	// ErrConflict is returned when a caller-supplied goal_id already names a
	// goal with a different spec (name/agent/input/parent).
	ErrConflict = errors.New("goal_id conflict")
)

type Store struct {
	Pool   *pgxpool.Pool
	Ledger ledger.Recorder
}

func Open(ctx context.Context, url string, rec ledger.Recorder) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		rec = ledger.Noop{}
	}
	s := &Store{Pool: pool, Ledger: rec}
	if err := s.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() { s.Pool.Close() }

func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.Pool.Exec(ctx, `SELECT pg_advisory_lock(7263001)`)
	if err != nil {
		return err
	}
	defer s.Pool.Exec(ctx, `SELECT pg_advisory_unlock(7263001)`)
	_, err = s.Pool.Exec(ctx, schema)
	return err
}

type Goal struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Agent        string          `json:"agent"`
	Input        json.RawMessage `json:"input"`
	State        fsm.State       `json:"state"`
	PausedFrom   *string         `json:"paused_from,omitempty"`
	ParentID     *string         `json:"parent_id,omitempty"`
	CreatedBy    string          `json:"created_by"`
	Cursor       int             `json:"cursor"`
	LeaseOwner   *string         `json:"lease_owner,omitempty"`
	LeaseEpoch   int64           `json:"lease_epoch"`
	LeaseExpires *time.Time      `json:"lease_expires,omitempty"`
	Reason       string          `json:"reason"`
	WarrantToken string          `json:"-"`
	WarrantSVID  string          `json:"-"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

const goalCols = `id,name,agent,input,state,paused_from,parent_id,created_by,cursor,lease_owner,lease_epoch,lease_expires,reason,warrant_token,warrant_svid,created_at,updated_at`

func scanGoal(r pgx.Row) (*Goal, error) {
	var g Goal
	var st string
	err := r.Scan(&g.ID, &g.Name, &g.Agent, &g.Input, &st, &g.PausedFrom, &g.ParentID, &g.CreatedBy, &g.Cursor, &g.LeaseOwner, &g.LeaseEpoch, &g.LeaseExpires, &g.Reason, &g.WarrantToken, &g.WarrantSVID, &g.CreatedAt, &g.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	g.State = fsm.State(st)
	return &g, err
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "g_" + hex.EncodeToString(b)
}

// validGoalID reports whether a caller-supplied goal_id is an acceptable
// external identifier: non-empty, bounded length, and a conservative charset
// so it is always safe in URLs, logs and Ledger records.
func validGoalID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.', r == ':':
		default:
			return false
		}
	}
	return true
}

// jsonEqual compares two JSON documents by value (not byte-for-byte), so
// whitespace/key-order differences don't defeat idempotent resubmission.
func jsonEqual(a, b json.RawMessage) bool {
	var av, bv any
	if len(a) == 0 {
		a = json.RawMessage(`{}`)
	}
	if len(b) == 0 {
		b = json.RawMessage(`{}`)
	}
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return bytes.Equal(bytes.TrimSpace(a), bytes.TrimSpace(b))
	}
	return reflect.DeepEqual(av, bv)
}

// ActorChain builds the Ledger actor chain for a goal: originating human first.
func ActorChain(createdBy, agent, worker string) []ledger.Actor {
	c := []ledger.Actor{{Kind: "human", ID: createdBy}, {Kind: "service", ID: "harbourd"}}
	if worker != "" {
		c[1].ID = "harbourd/" + worker
	}
	if agent != "" {
		c = append(c, ledger.Actor{Kind: "agent", ID: agent})
	}
	return c
}


func addEvent(ctx context.Context, tx pgx.Tx, goalID, kind string, data any) error {
	b, _ := json.Marshal(data)
	_, err := tx.Exec(ctx, `INSERT INTO events(goal_id,kind,data) VALUES($1,$2,$3)`, goalID, kind, b)
	return err
}

type CreateGoal struct {
	// ID is an optional caller-supplied external goal id. If set, submit is
	// idempotent: resubmitting the same id with an identical spec (name,
	// agent, input, parent_id) returns the existing goal unchanged; a
	// resubmit with a different spec fails with ErrConflict (API: 409).
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name"`
	Agent        string          `json:"agent"`
	Input        json.RawMessage `json:"input"`
	ParentID     string          `json:"parent_id,omitempty"`
	CreatedBy    string          `json:"created_by"`
	Approve      bool            `json:"approve"`
	WarrantToken string          `json:"warrant_token,omitempty"`
	WarrantSVID  string          `json:"warrant_svid,omitempty"`
}

// sameSpec reports whether an existing goal matches a resubmitted CreateGoal
// closely enough to treat the resubmit as idempotent.
func sameSpec(g *Goal, c CreateGoal) bool {
	gotParent := ""
	if g.ParentID != nil {
		gotParent = *g.ParentID
	}
	return g.Name == c.Name && g.Agent == c.Agent && gotParent == c.ParentID && jsonEqual(g.Input, c.Input)
}

func (s *Store) Create(ctx context.Context, c CreateGoal) (*Goal, error) {
	if c.Name == "" || c.Agent == "" || c.CreatedBy == "" {
		return nil, fmt.Errorf("name, agent and created_by are required")
	}
	if c.ID != "" && !validGoalID(c.ID) {
		return nil, fmt.Errorf("invalid goal_id %q: must be 1-128 chars of [A-Za-z0-9_.:-]", c.ID)
	}
	if len(c.Input) == 0 {
		c.Input = json.RawMessage(`{}`)
	}
	if c.ID != "" {
		existing, err := s.Pool.Query(ctx, `SELECT `+goalCols+` FROM goals WHERE id=$1`, c.ID)
		if err != nil {
			return nil, err
		}
		var g *Goal
		if existing.Next() {
			g, err = scanGoal(existing)
		}
		existing.Close()
		if err != nil {
			return nil, err
		}
		if g != nil {
			if sameSpec(g, c) {
				return g, nil
			}
			return nil, fmt.Errorf("%w: goal_id %q already exists with a different spec", ErrConflict, c.ID)
		}
	}
	var parent *string
	if c.ParentID != "" {
		parent = &c.ParentID
	}
	id := c.ID
	if id == "" {
		id = newID()
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	g, err := scanGoal(tx.QueryRow(ctx, `INSERT INTO goals(id,name,agent,input,state,parent_id,created_by,warrant_token,warrant_svid) VALUES($1,$2,$3,$4,'proposed',$5,$6,$7,$8) RETURNING `+goalCols,
		id, c.Name, c.Agent, c.Input, parent, c.CreatedBy, c.WarrantToken, c.WarrantSVID))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Unique violation: a concurrent submit won the race on this
			// external goal_id (or the name). Re-read and treat as the
			// idempotent case rather than surfacing a raw DB error.
			if c.ID != "" {
				if g2, gerr := s.Get(ctx, c.ID); gerr == nil && sameSpec(g2, c) {
					return g2, nil
				}
			}
			return nil, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return nil, err
	}
	if err := addEvent(ctx, tx, g.ID, "transition", map[string]any{"from": "", "to": "proposed", "actor": c.CreatedBy}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.record(ctx, g, "", "harbour.goal.transition", map[string]any{"from": "", "to": "proposed", "name": g.Name})
	if c.Approve {
		return s.Transition(ctx, g.ID, fsm.Approved, "auto-approved at submit", c.CreatedBy)
	}
	return g, nil
}

func (s *Store) record(ctx context.Context, g *Goal, worker, typ string, payload map[string]any) {
	payload["goal_name"] = g.Name
	_ = s.Ledger.Record(ctx, ledger.Record{Chain: "harbour", Type: typ, GoalID: g.ID, ActorChain: ActorChain(g.CreatedBy, g.Agent, worker), Payload: payload})
}

// Get looks up by ID or by name.
func (s *Store) Get(ctx context.Context, idOrName string) (*Goal, error) {
	return scanGoal(s.Pool.QueryRow(ctx, `SELECT `+goalCols+` FROM goals WHERE id=$1 OR name=$1`, idOrName))
}

func (s *Store) List(ctx context.Context) ([]*Goal, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+goalCols+` FROM goals ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Goal
	for rows.Next() {
		g, err := scanGoal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Transition moves a goal to `to`, enforcing the legal-transition table.
// Moving to paused remembers the prior state; moving to a paused/terminal
// state drops any lease so the owning worker's fenced writes fail.
func (s *Store) Transition(ctx context.Context, idOrName string, to fsm.State, reason, actor string) (*Goal, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	g, err := scanGoal(tx.QueryRow(ctx, `SELECT `+goalCols+` FROM goals WHERE id=$1 OR name=$1 FOR UPDATE`, idOrName))
	if err != nil {
		return nil, err
	}
	from := g.State
	if err := fsm.Check(from, to); err != nil {
		return nil, err
	}
	var pausedFrom any
	if to == fsm.Paused {
		pausedFrom = string(from)
	}
	dropLease := to == fsm.Paused || fsm.Terminal(to)
	g, err = scanGoal(tx.QueryRow(ctx, `UPDATE goals SET state=$2, paused_from=$3, reason=$4, updated_at=now(),
		lease_owner = CASE WHEN $5 THEN NULL ELSE lease_owner END,
		lease_expires = CASE WHEN $5 THEN NULL ELSE lease_expires END
		WHERE id=$1 RETURNING `+goalCols, g.ID, string(to), pausedFrom, reason, dropLease))
	if err != nil {
		return nil, err
	}
	if err := addEvent(ctx, tx, g.ID, "transition", map[string]any{"from": from, "to": to, "reason": reason, "actor": actor}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.record(ctx, g, "", "harbour.goal.transition", map[string]any{"from": string(from), "to": string(to), "reason": reason, "actor": actor})
	return g, nil
}

// Resume returns a paused goal to the state it was paused from.
func (s *Store) Resume(ctx context.Context, idOrName, actor string) (*Goal, error) {
	g, err := s.Get(ctx, idOrName)
	if err != nil {
		return nil, err
	}
	if g.State != fsm.Paused || g.PausedFrom == nil {
		return nil, fsm.ErrIllegal{From: g.State, To: "resume"}
	}
	return s.Transition(ctx, g.ID, fsm.State(*g.PausedFrom), "resumed", actor)
}

// Reparent changes a goal's parent (supervisor), rejecting cycles.
func (s *Store) Reparent(ctx context.Context, idOrName, parent, actor string) (*Goal, error) {
	g, err := s.Get(ctx, idOrName)
	if err != nil {
		return nil, err
	}
	var p any
	if parent != "" {
		pg, err := s.Get(ctx, parent)
		if err != nil {
			return nil, fmt.Errorf("parent: %w", err)
		}
		// cycle check: walk up from new parent
		var cyc bool
		err = s.Pool.QueryRow(ctx, `WITH RECURSIVE up(id,parent_id) AS (SELECT id,parent_id FROM goals WHERE id=$1
			UNION SELECT g.id,g.parent_id FROM goals g JOIN up ON g.id=up.parent_id) SELECT EXISTS(SELECT 1 FROM up WHERE id=$2)`, pg.ID, g.ID).Scan(&cyc)
		if err != nil {
			return nil, err
		}
		if cyc {
			return nil, fmt.Errorf("reparent would create a cycle")
		}
		p = pg.ID
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	g, err = scanGoal(tx.QueryRow(ctx, `UPDATE goals SET parent_id=$2, updated_at=now() WHERE id=$1 RETURNING `+goalCols, g.ID, p))
	if err != nil {
		return nil, err
	}
	if err := addEvent(ctx, tx, g.ID, "reparent", map[string]any{"parent_id": p, "actor": actor}); err != nil {
		return nil, err
	}
	return g, tx.Commit(ctx)
}

// ---- leases ----

// Claim takes the lease on one runnable goal (approved/executing/verifying)
// whose lease is absent or expired. The epoch increments on every claim and
// fences all later writes by this worker.
func (s *Store) Claim(ctx context.Context, worker string, ttl time.Duration) (*Goal, error) {
	g, err := scanGoal(s.Pool.QueryRow(ctx, `UPDATE goals SET lease_owner=$1, lease_epoch=lease_epoch+1,
		lease_expires=now()+$2::interval, updated_at=now()
		WHERE id = (SELECT id FROM goals WHERE state IN ('approved','executing','verifying')
		  AND (lease_owner IS NULL OR lease_expires < now()) ORDER BY updated_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING `+goalCols, worker, ttl.String()))
	if err == ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err == nil {
		addEvent(ctx, tx, g.ID, "lease", map[string]any{"worker": worker, "epoch": g.LeaseEpoch})
		tx.Commit(ctx)
	}
	return g, nil
}

// Heartbeat extends the lease; returns ErrLeaseLost if it is no longer ours.
func (s *Store) Heartbeat(ctx context.Context, id, worker string, epoch int64, ttl time.Duration) error {
	ct, err := s.Pool.Exec(ctx, `UPDATE goals SET lease_expires=now()+$4::interval WHERE id=$1 AND lease_owner=$2 AND lease_epoch=$3`, id, worker, epoch, ttl.String())
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

func (s *Store) Release(ctx context.Context, id, worker string, epoch int64) error {
	_, err := s.Pool.Exec(ctx, `UPDATE goals SET lease_owner=NULL, lease_expires=NULL WHERE id=$1 AND lease_owner=$2 AND lease_epoch=$3`, id, worker, epoch)
	return err
}

// Lease identifies a held lease for fencing.
type Lease struct {
	GoalID string
	Worker string
	Epoch  int64
}

func checkLease(ctx context.Context, tx pgx.Tx, l Lease) (*Goal, error) {
	g, err := scanGoal(tx.QueryRow(ctx, `SELECT `+goalCols+` FROM goals WHERE id=$1 FOR UPDATE`, l.GoalID))
	if err != nil {
		return nil, err
	}
	if g.LeaseOwner == nil || *g.LeaseOwner != l.Worker || g.LeaseEpoch != l.Epoch {
		return nil, ErrLeaseLost
	}
	return g, nil
}

// TransitionFenced is Transition but only if the caller still holds the lease.
func (s *Store) TransitionFenced(ctx context.Context, l Lease, to fsm.State, reason string) (*Goal, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	g, err := checkLease(ctx, tx, l)
	if err != nil {
		return nil, err
	}
	from := g.State
	if err := fsm.Check(from, to); err != nil {
		return nil, err
	}
	var pausedFrom any
	if to == fsm.Paused {
		pausedFrom = string(from)
	}
	drop := to == fsm.Paused || fsm.Terminal(to)
	g, err = scanGoal(tx.QueryRow(ctx, `UPDATE goals SET state=$2, paused_from=$3, reason=$4, updated_at=now(),
		lease_owner = CASE WHEN $5 THEN NULL ELSE lease_owner END,
		lease_expires = CASE WHEN $5 THEN NULL ELSE lease_expires END
		WHERE id=$1 RETURNING `+goalCols, g.ID, string(to), pausedFrom, reason, drop))
	if err != nil {
		return nil, err
	}
	if err := addEvent(ctx, tx, g.ID, "transition", map[string]any{"from": from, "to": to, "reason": reason, "actor": "worker:" + l.Worker}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.record(ctx, g, l.Worker, "harbour.goal.transition", map[string]any{"from": string(from), "to": string(to), "reason": reason, "epoch": l.Epoch})
	return g, nil
}

// LoadFenced returns the goal if the lease is still held.
func (s *Store) LoadFenced(ctx context.Context, l Lease) (*Goal, error) {
	g, err := s.Get(ctx, l.GoalID)
	if err != nil {
		return nil, err
	}
	if g.LeaseOwner == nil || *g.LeaseOwner != l.Worker || g.LeaseEpoch != l.Epoch {
		return nil, ErrLeaseLost
	}
	return g, nil
}

// ---- effects (two-phase commit) ----

type Effect struct {
	ID        int64           `json:"id"`
	Key       string          `json:"idem_key"`
	GoalID    string          `json:"goal_id"`
	Step      int             `json:"step"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
	Status    string          `json:"status"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
	Attempts  int             `json:"attempts"`
	Epoch     int64           `json:"lease_epoch"`
	Worker    string          `json:"worker"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

const effCols = `id,idem_key,goal_id,step,tool,args,status,result,error,attempts,lease_epoch,worker,created_at,updated_at`

func scanEffect(r pgx.Row) (*Effect, error) {
	var e Effect
	var res []byte
	err := r.Scan(&e.ID, &e.Key, &e.GoalID, &e.Step, &e.Tool, &e.Args, &e.Status, &res, &e.Error, &e.Attempts, &e.Epoch, &e.Worker, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if res != nil {
		e.Result = res
	}
	return &e, err
}

func (s *Store) EffectByKey(ctx context.Context, key string) (*Effect, error) {
	return scanEffect(s.Pool.QueryRow(ctx, `SELECT `+effCols+` FROM effects WHERE idem_key=$1`, key))
}

func (s *Store) EffectByStep(ctx context.Context, goalID string, step int) (*Effect, error) {
	return scanEffect(s.Pool.QueryRow(ctx, `SELECT `+effCols+` FROM effects WHERE goal_id=$1 AND step=$2`, goalID, step))
}

func (s *Store) EffectByID(ctx context.Context, id int64) (*Effect, error) {
	return scanEffect(s.Pool.QueryRow(ctx, `SELECT `+effCols+` FROM effects WHERE id=$1`, id))
}

func (s *Store) Effects(ctx context.Context, goalID string) ([]*Effect, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+effCols+` FROM effects WHERE goal_id=$1 ORDER BY step`, goalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Effect
	for rows.Next() {
		e, err := scanEffect(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CommitIntent is phase (a): durably record "about to do X" under the lease.
// If an effect with this key already exists it is returned unchanged.
func (s *Store) CommitIntent(ctx context.Context, l Lease, key string, step int, tool string, args json.RawMessage) (*Effect, bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	g, err := checkLease(ctx, tx, l)
	if err != nil {
		return nil, false, err
	}
	e, err := scanEffect(tx.QueryRow(ctx, `INSERT INTO effects(idem_key,goal_id,step,tool,args,status,lease_epoch,worker)
		VALUES($1,$2,$3,$4,$5,'intent',$6,$7) ON CONFLICT DO NOTHING RETURNING `+effCols, key, l.GoalID, step, tool, args, l.Epoch, l.Worker))
	if err == ErrNotFound {
		e, err = scanEffect(tx.QueryRow(ctx, `SELECT `+effCols+` FROM effects WHERE idem_key=$1 OR (goal_id=$2 AND step=$3)`, key, l.GoalID, step))
		return e, false, err
	}
	if err != nil {
		return nil, false, err
	}
	if err := addEvent(ctx, tx, l.GoalID, "effect.intent", map[string]any{"effect_id": e.ID, "step": step, "tool": tool, "idem_key": key}); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	s.record(ctx, g, l.Worker, "harbour.effect.intent", map[string]any{"effect_id": e.ID, "step": step, "tool": tool, "idem_key": key, "args": json.RawMessage(args)})
	return e, true, nil
}

// MarkAttempt increments the attempt counter just before running the tool.
func (s *Store) MarkAttempt(ctx context.Context, id int64) error {
	_, err := s.Pool.Exec(ctx, `UPDATE effects SET attempts=attempts+1, updated_at=now() WHERE id=$1 AND status='intent'`, id)
	return err
}

// CommitResult is phase (c). It is intentionally not lease-fenced: recording
// the truth about an effect that happened is always correct. Only the first
// writer wins (status must still be 'intent'). via tells how it was learned.
func (s *Store) CommitResult(ctx context.Context, id int64, status string, result json.RawMessage, errMsg, via string) (*Effect, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	e, err := scanEffect(tx.QueryRow(ctx, `UPDATE effects SET status=$2, result=$3, error=$4, updated_at=now()
		WHERE id=$1 AND status='intent' RETURNING `+effCols, id, status, nullJSON(result), errMsg))
	if err == ErrNotFound {
		e, err = scanEffect(tx.QueryRow(ctx, `SELECT `+effCols+` FROM effects WHERE id=$1`, id))
		return e, err
	}
	if err != nil {
		return nil, err
	}
	if err := addEvent(ctx, tx, e.GoalID, "effect.result", map[string]any{"effect_id": e.ID, "step": e.Step, "status": status, "via": via, "error": errMsg}); err != nil {
		return nil, err
	}
	if status == "committed" {
		body, _ := json.Marshal(map[string]any{"tool": e.Tool, "step": e.Step, "result": json.RawMessage(nullOr(result))})
		if _, err := tx.Exec(ctx, `INSERT INTO messages(goal_id,channel,source,sender,trusted,body) VALUES($1,'tools','tool_result',$2,false,$3)`, e.GoalID, e.Tool, body); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if g, err := s.Get(ctx, e.GoalID); err == nil {
		s.record(ctx, g, e.Worker, "harbour.effect.result", map[string]any{"effect_id": e.ID, "step": e.Step, "tool": e.Tool, "idem_key": e.Key, "status": status, "via": via, "error": errMsg})
	}
	return e, nil
}

// ResolveReview lets an operator settle a needs_review effect: "retry" puts it
// back to intent (so the executor will run it again), "committed" records a
// result supplied by the operator, "failed" gives up.
func (s *Store) ResolveReview(ctx context.Context, id int64, action string, result json.RawMessage, actor string) (*Effect, error) {
	var status string
	switch action {
	case "retry":
		status = "intent"
	case "committed", "failed":
		status = action
	default:
		return nil, fmt.Errorf("action must be retry|committed|failed")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	e, err := scanEffect(tx.QueryRow(ctx, `UPDATE effects SET status=$2, result=$3, attempts=CASE WHEN $2='intent' THEN 0 ELSE attempts END, error=CASE WHEN $2='failed' THEN 'operator marked failed' ELSE '' END, updated_at=now()
		WHERE id=$1 AND status='needs_review' RETURNING `+effCols, id, status, nullJSON(result)))
	if err != nil {
		return nil, fmt.Errorf("effect not in needs_review: %w", err)
	}
	addEvent(ctx, tx, e.GoalID, "effect.resolved", map[string]any{"effect_id": id, "action": action, "actor": actor})
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if g, err := s.Get(ctx, e.GoalID); err == nil {
		s.record(ctx, g, "", "harbour.effect.result", map[string]any{"effect_id": e.ID, "step": e.Step, "tool": e.Tool, "idem_key": e.Key, "status": status, "via": "operator:" + actor})
	}
	return e, nil
}

// MarkNeedsReview flags an in-doubt effect that cannot be probed.
func (s *Store) MarkNeedsReview(ctx context.Context, id int64, why string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var gid string
	err = tx.QueryRow(ctx, `UPDATE effects SET status='needs_review', error=$2, updated_at=now() WHERE id=$1 AND status='intent' RETURNING goal_id`, id, why).Scan(&gid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	addEvent(ctx, tx, gid, "effect.needs_review", map[string]any{"effect_id": id, "why": why})
	return tx.Commit(ctx)
}

// CommitStep advances the goal cursor past a committed effect (fenced).
func (s *Store) CommitStep(ctx context.Context, l Lease, e *Effect) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	g, err := checkLease(ctx, tx, l)
	if err != nil {
		return err
	}
	if g.Cursor != e.Step {
		return fmt.Errorf("cursor %d != step %d", g.Cursor, e.Step)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO steps(goal_id,step,tool,args,result,idem_key) VALUES($1,$2,$3,$4,$5,$6)`, e.GoalID, e.Step, e.Tool, e.Args, nullJSON(e.Result), e.Key); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE goals SET cursor=cursor+1, updated_at=now() WHERE id=$1`, e.GoalID); err != nil {
		return err
	}
	if err := addEvent(ctx, tx, e.GoalID, "step.committed", map[string]any{"step": e.Step, "tool": e.Tool}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) Steps(ctx context.Context, goalID string) ([]registry.StepResult, error) {
	rows, err := s.Pool.Query(ctx, `SELECT step,tool,args,result FROM steps WHERE goal_id=$1 ORDER BY step`, goalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []registry.StepResult
	for rows.Next() {
		var r registry.StepResult
		var res []byte
		if err := rows.Scan(&r.Step, &r.Tool, &r.Args, &res); err != nil {
			return nil, err
		}
		r.Result = res
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- messages ----

func (s *Store) AddMessage(ctx context.Context, goalID, channel, source, sender string, body json.RawMessage) (*registry.Message, error) {
	if !registry.ValidSource(source) {
		return nil, fmt.Errorf("invalid provenance source %q", source)
	}
	if channel == "" {
		channel = "default"
	}
	if !json.Valid(body) {
		b, _ := json.Marshal(string(body))
		body = b
	}
	trusted := source == registry.SrcOperator
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	m := registry.Message{Channel: channel, Source: source, Sender: sender, Trusted: trusted, Body: body}
	if err := tx.QueryRow(ctx, `INSERT INTO messages(goal_id,channel,source,sender,trusted,body) VALUES($1,$2,$3,$4,$5,$6) RETURNING id,created_at`,
		goalID, channel, source, sender, trusted, body).Scan(&m.ID, &m.CreatedAt); err != nil {
		return nil, err
	}
	addEvent(ctx, tx, goalID, "message", map[string]any{"id": m.ID, "channel": channel, "source": source, "sender": sender, "trusted": trusted})
	return &m, tx.Commit(ctx)
}

func (s *Store) Messages(ctx context.Context, goalID string) ([]registry.Message, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id,channel,source,sender,trusted,body,created_at FROM messages WHERE goal_id=$1 ORDER BY id`, goalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []registry.Message
	for rows.Next() {
		var m registry.Message
		if err := rows.Scan(&m.ID, &m.Channel, &m.Source, &m.Sender, &m.Trusted, &m.Body, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---- events ----

type Event struct {
	Seq       int64           `json:"seq"`
	GoalID    string          `json:"goal_id"`
	Kind      string          `json:"kind"`
	Data      json.RawMessage `json:"data"`
	CreatedAt time.Time       `json:"created_at"`
}

func (s *Store) Events(ctx context.Context, goalID string, after int64) ([]Event, error) {
	rows, err := s.Pool.Query(ctx, `SELECT seq,goal_id,kind,data,created_at FROM events WHERE goal_id=$1 AND seq>$2 ORDER BY seq LIMIT 500`, goalID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Seq, &e.GoalID, &e.Kind, &e.Data, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func nullJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

func nullOr(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return b
}
