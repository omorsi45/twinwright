package crashmatrix

import (
	"context"
	"testing"

	"twinwright/internal/pgtest"
	"twinwright/internal/store"
)

// TestCrashMatrixPostgres runs the first and last occurrence of every point for
// all three scenarios against a real PostgreSQL server, where workers hold
// genuinely concurrent connections and the fence check takes row locks. The
// full matrix stays on SQLite, where it is fast; this proves the same recovery
// invariants hold on the other dialect.
func TestCrashMatrixPostgres(t *testing.T) {
	if !pgtest.Available() {
		t.Skipf("%s is not set; skipping PostgreSQL crash matrix", pgtest.DSNEnv)
	}
	runMatrix(t, func(t *testing.T) *store.Store {
		t.Helper()
		s, err := store.OpenDSN(context.Background(), pgtest.SchemaDSN(t))
		if err != nil {
			t.Fatalf("open postgres store: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}, true)
}
