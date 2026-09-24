package engine

import (
	"context"
	"testing"

	"github.com/Celaris-dev1/Harbour-/internal/testdb"
)

func TestPostgresEngineConformance(t *testing.T) {
	RunConformance(t, func(t *testing.T) Engine {
		p, err := OpenPostgres(context.Background(), testdb.URL(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p
	})
}
