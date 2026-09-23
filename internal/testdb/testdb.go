// Package testdb gives each test an isolated Postgres schema.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/Celaris-dev1/Harbour-/internal/ledger"
	"github.com/Celaris-dev1/Harbour-/internal/store"
	"github.com/jackc/pgx/v5"
)

// URL returns a database URL bound to a fresh schema, or skips the test if
// HARBOUR_TEST_DATABASE_URL is unset.
func URL(t testing.TB) string {
	base := os.Getenv("HARBOUR_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("HARBOUR_TEST_DATABASE_URL not set")
	}
	b := make([]byte, 6)
	rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)
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

// Open returns a migrated store on a fresh schema.
func Open(t testing.TB, rec ledger.Recorder) *store.Store {
	s, err := store.Open(context.Background(), URL(t), rec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}
