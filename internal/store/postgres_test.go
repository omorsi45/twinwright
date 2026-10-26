package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"twinwright/internal/pgsql"
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

	if err = s.SaveTurn(ctx, run.ID, 1, `[{"role":"assistant"}]`, map[string]any{"role": "assistant"}, nil); err != nil {
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

// TestPostgresMigrationThreeNormalizesCoordinationTimestamps upgrades a schema
// held at version 2 and checks the rewrite on a real server, because the
// migration reads and writes timestamp text through the PostgreSQL driver
// rather than SQLite's.
func TestPostgresMigrationThreeNormalizesCoordinationTimestamps(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.SchemaDSN(t)
	schema, err := searchPathSchema(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err = ensureSchema(ctx, dsn, schema); err != nil {
		t.Fatal(err)
	}
	pgsql.Register()
	db, err := sql.Open(pgsql.DriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = applyMigrations(ctx, db, DialectPostgres, migrations[:2]); err != nil {
		t.Fatalf("apply migrations 1-2: %v", err)
	}
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO runs(id,world_id,scenario,provider,task,status) VALUES(?,?,?,?,?,?)`,
			[]any{"R-1", "W-1", "duplicate-charge", "scripted", "task", "running"}},
		{`INSERT INTO run_leases(run_id,owner,fence,committed_fence,expires_at,acquired_at,heartbeat_at) VALUES(?,?,?,?,?,?,?)`,
			[]any{"R-1", "worker-a", 1, 0, "2026-01-01T12:00:01Z", "2026-01-01T12:00:00Z", "2026-01-01T12:00:00.25Z"}},
		{`INSERT INTO work_queue(run_id,state,max_steps,enqueued_at,available_at) VALUES(?,?,?,?,?)`,
			[]any{"R-1", QueueRunnable, 10, "2026-01-01T12:00:01Z", "2026-01-01T12:00:01Z"}},
		{`INSERT INTO workers(id,host,started_at,last_seen_at) VALUES(?,?,?,?)`,
			[]any{"worker-a", "host", "2026-01-01T12:00:00Z", "2026-01-01T12:00:01.5Z"}},
	} {
		if _, err = db.ExecContext(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed %.40s: %v", stmt.sql, err)
		}
	}

	if err = applyMigrations(ctx, db, DialectPostgres, migrations); err != nil {
		t.Fatalf("apply migration 3: %v", err)
	}

	for _, want := range []struct{ table, column, value string }{
		{"run_leases", "expires_at", "2026-01-01T12:00:01.000000000Z"},
		{"run_leases", "acquired_at", "2026-01-01T12:00:00.000000000Z"},
		{"run_leases", "heartbeat_at", "2026-01-01T12:00:00.250000000Z"},
		{"work_queue", "enqueued_at", "2026-01-01T12:00:01.000000000Z"},
		{"work_queue", "available_at", "2026-01-01T12:00:01.000000000Z"},
		{"workers", "started_at", "2026-01-01T12:00:00.000000000Z"},
		{"workers", "last_seen_at", "2026-01-01T12:00:01.500000000Z"},
	} {
		var got string
		if err = db.QueryRowContext(ctx, "SELECT "+want.column+" FROM "+want.table+" LIMIT 1").Scan(&got); err != nil {
			t.Fatalf("%s.%s: %v", want.table, want.column, err)
		}
		if got != want.value {
			t.Errorf("%s.%s=%q want %q", want.table, want.column, got, want.value)
		}
	}
	var version int
	if err = db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatalf("schema version=%d want 3", version)
	}
}
