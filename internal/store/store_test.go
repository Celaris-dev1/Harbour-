package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/Celaris-dev1/Harbour-/internal/ledger"
	"github.com/jackc/pgx/v5"
)

// testURL and open are a self-contained equivalent of internal/testdb (which
// this package cannot import: testdb imports store, and package store's own
// _test.go files compile into the store package itself, so that would be an
// import cycle). Each test gets its own schema, dropped on cleanup.
func testURL(t *testing.T) string {
	base := os.Getenv("HARBOUR_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("HARBOUR_TEST_DATABASE_URL not set")
	}
	schema := "t_" + newID()[2:] // reuse newID's random hex, minus the "g_" prefix
	ctx := context.Background()
	c, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	c.Close(ctx)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err == nil {
			c.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
			c.Close(context.Background())
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

func open(t *testing.T) *Store {
	st, err := Open(context.Background(), testURL(t), ledger.Noop{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

func TestCreateWithExternalGoalID(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	g, err := st.Create(ctx, CreateGoal{ID: "ext-123", Name: "n1", Agent: "a", Input: json.RawMessage(`{"x":1}`), CreatedBy: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if g.ID != "ext-123" {
		t.Fatalf("got id %q", g.ID)
	}
}

func TestCreateIdempotentResubmitSameSpec(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	c := CreateGoal{ID: "ext-idem", Name: "n2", Agent: "a", Input: json.RawMessage(`{"a":1,"b":2}`), CreatedBy: "alice"}
	g1, err := st.Create(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	// Resubmit with the same spec but reordered JSON keys: must return the
	// same goal, not error, and must not create a duplicate.
	c2 := c
	c2.Input = json.RawMessage(`{"b":2,"a":1}`)
	g2, err := st.Create(ctx, c2)
	if err != nil {
		t.Fatal(err)
	}
	if g2.ID != g1.ID {
		t.Fatalf("resubmit produced a different goal: %s vs %s", g2.ID, g1.ID)
	}
	all, err := st.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, g := range all {
		if g.ID == "ext-idem" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one goal with id ext-idem, got %d", n)
	}
}

func TestCreateConflictingSpecSameGoalID(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	c := CreateGoal{ID: "ext-conf", Name: "n3", Agent: "a", Input: json.RawMessage(`{}`), CreatedBy: "alice"}
	if _, err := st.Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	c2 := c
	c2.Agent = "b" // different spec, same id
	_, err := st.Create(ctx, c2)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
	c3 := c
	c3.Input = json.RawMessage(`{"different":true}`)
	if _, err := st.Create(ctx, c3); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict for different input, got %v", err)
	}
}

// TestCreateDuplicateNameCleanError covers submitting a second goal with a name
// that's already taken (no caller-supplied ID, so the two goals get distinct
// generated IDs and only the `goals_name_key` unique constraint fires). The
// resulting error must be a clean, user-facing message, not a raw Postgres
// error with an embedded SQLSTATE code.
func TestCreateDuplicateNameCleanError(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	c := CreateGoal{Name: "dupname", Agent: "a", Input: json.RawMessage(`{}`), CreatedBy: "alice"}
	if _, err := st.Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	_, err := st.Create(ctx, c)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
	msg := err.Error()
	if strings.Contains(msg, "SQLSTATE") || strings.Contains(msg, "ERROR:") {
		t.Fatalf("error message leaks raw postgres error: %q", msg)
	}
	if !strings.Contains(msg, "dupname") {
		t.Fatalf("expected error to name the conflicting name, got %q", msg)
	}
}

func TestCreateInvalidGoalID(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	for _, bad := range []string{"has space", "has/slash", "", string(make([]byte, 200))} {
		if bad == "" {
			continue // empty means "auto-generate", tested separately
		}
		_, err := st.Create(ctx, CreateGoal{ID: bad, Name: "x", Agent: "a", CreatedBy: "alice"})
		if err == nil {
			t.Fatalf("expected error for goal_id %q", bad)
		}
	}
}

func TestCreateWithoutIDStillAutogenerates(t *testing.T) {
	st := open(t)
	g, err := st.Create(context.Background(), CreateGoal{Name: "auto", Agent: "a", CreatedBy: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if g.ID == "" {
		t.Fatal("expected an auto-generated id")
	}
}

func TestGoalIDCarriedIntoLedgerRecords(t *testing.T) {
	var got []ledger.Record
	rec := recorderFunc(func(_ context.Context, r ledger.Record) error {
		got = append(got, r)
		return nil
	})
	st, err := Open(context.Background(), testURL(t), rec)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	g, err := st.Create(context.Background(), CreateGoal{ID: "ext-ledger", Name: "led", Agent: "a", CreatedBy: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("expected at least one ledger record")
	}
	for _, r := range got {
		if r.GoalID != g.ID {
			t.Fatalf("record carries goal_id %q, want %q", r.GoalID, g.ID)
		}
	}
}

type recorderFunc func(context.Context, ledger.Record) error

func (f recorderFunc) Record(ctx context.Context, r ledger.Record) error { return f(ctx, r) }

// Canonical-JSON/idempotency-key equivalence with internal/executor's
// Key/Canonical is exercised end to end by
// internal/worker.TestRequestApprovalRetryReusesExecutorKey (an
// executor+store import cycle would otherwise be needed to check it here
// directly).
