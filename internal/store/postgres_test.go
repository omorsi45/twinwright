package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"twinwright/internal/pgtest"
)

// openTestPostgres opens a store in a schema of its own, skipping when no
// server is configured.
func openTestPostgres(ctx context.Context, t *testing.T) *Store {
	t.Helper()
	s, err := OpenDSN(ctx, pgtest.SchemaDSN(t))
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestPostgresRuntimeParity exercises the same runtime writes the SQLite store
// serves, against a real PostgreSQL server. It is skipped without a DSN so the
// default `go test ./...` stays hermetic; CI supplies one from a service
// container. There is no mock: a fake would not catch the dialect differences
// this test exists to catch.
func TestPostgresRuntimeParity(t *testing.T) {
	ctx := context.Background()
	s := openTestPostgres(ctx, t)

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("postgres schema version=%d want %d", version, CurrentSchemaVersion)
	}
	if s.Dialect != DialectPostgres {
		t.Fatalf("dialect=%q", s.Dialect)
	}

	// A seed larger than 2^31 would silently overflow a PostgreSQL INTEGER.
	const bigSeed = int64(5_000_000_000)
	world, err := s.SeedScenario(ctx, bigSeed, "digest-pg", "company-incident")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	var storedSeed int64
	if err = s.DB.QueryRowContext(ctx, "SELECT seed FROM worlds WHERE id=?", world.ID).Scan(&storedSeed); err != nil {
		t.Fatal(err)
	}
	if storedSeed != bigSeed {
		t.Fatalf("seed round-trip lost precision: %d", storedSeed)
	}

	run, err := s.CreateRun(ctx, world.ID, "company-incident", "scripted", "fixture-v1", "task", "")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	// AppendEventTx allocates seq with an aggregate beside a plain column,
	// which PostgreSQL only accepts when grouped.
	for i := 0; i < 3; i++ {
		if err = s.Append(ctx, run.ID, "model.request", map[string]any{"step": i}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	events, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("events=%d want 4 (execution.started plus three appends)", len(events))
	}
	for i, event := range events {
		if event.Seq != i+1 {
			t.Fatalf("event %d has seq %d", i, event.Seq)
		}
	}

	if err = s.SaveTurn(ctx, run.ID, 1, `[{"role":"assistant"}]`, map[string]any{"role": "assistant"}); err != nil {
		t.Fatalf("save turn: %v", err)
	}
	reloaded, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Step != 1 || reloaded.Status != "running" {
		t.Fatalf("run=%+v", reloaded)
	}
	if reloaded.PrincipalID != UnrestrictedPrincipal {
		t.Fatalf("principal=%q", reloaded.PrincipalID)
	}

	snapshot, err := s.Snapshot(ctx, world.ID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var rows []map[string]string
	if err = json.Unmarshal([]byte(snapshot), &rows); err != nil {
		t.Fatalf("snapshot json: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("snapshot returned no rows")
	}

	// A missing policy must still read as ErrNoRows, not as a dialect error.
	if _, _, err = s.ChaosPolicy(ctx, run.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ChaosPolicy err=%v want sql.ErrNoRows", err)
	}
	if _, err = s.Lineage(ctx, run.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Lineage err=%v want sql.ErrNoRows", err)
	}
}

func TestPostgresWorldsAreIsolatedBySeedInstance(t *testing.T) {
	ctx := context.Background()
	s := openTestPostgres(ctx, t)
	first, err := s.Seed(ctx, 42, "iso")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Seed(ctx, 42, "iso")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("two seeds produced the same world %q", first.ID)
	}
	if _, err = s.DB.ExecContext(ctx, "UPDATE charges SET refunded_cents=100 WHERE world_id=?", first.ID); err != nil {
		t.Fatal(err)
	}
	var leaked int
	if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM charges WHERE world_id=? AND refunded_cents<>0", second.ID).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("mutation leaked into the sibling world: %d rows", leaked)
	}
}

func TestOpenDSNRejectsUnsupportedScheme(t *testing.T) {
	if _, err := OpenDSN(context.Background(), "mysql://localhost/twinwright"); err == nil {
		t.Fatal("expected an explicit unsupported-backend error")
	}
}
