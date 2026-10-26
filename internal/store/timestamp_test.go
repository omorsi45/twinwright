package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestCoordinationTimeIsFixedWidthAndOrdered(t *testing.T) {
	a := time.Date(2026, 1, 1, 12, 0, 1, 0, time.UTC)
	if got, want := CoordinationTime(a), "2026-01-01T12:00:01.000000000Z"; got != want {
		t.Fatalf("CoordinationTime=%q want %q", got, want)
	}
	if !(CoordinationTime(a) < CoordinationTime(a.Add(500*time.Millisecond))) {
		t.Fatalf("whole second %q must sort before %q", CoordinationTime(a), CoordinationTime(a.Add(500*time.Millisecond)))
	}
	est := a.In(time.FixedZone("EST", -5*3600))
	if CoordinationTime(est) != CoordinationTime(a) {
		t.Fatalf("zone changed the stored text: %q vs %q", CoordinationTime(est), CoordinationTime(a))
	}
}

// openRawAtVersionTwo returns a bare database migrated only as far as version
// 2, which is what an upgrade from the previous release looks like on disk.
func openRawAtVersionTwo(ctx context.Context, t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", t.TempDir()+"/v2.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = applyMigrations(ctx, db, DialectSQLite, migrations[:2]); err != nil {
		t.Fatalf("apply migrations 1-2: %v", err)
	}
	return db
}

// seedV2Coordination inserts one row in each coordination table using the
// legacy RFC3339Nano text, with the given lease expiry.
func seedV2Coordination(ctx context.Context, t *testing.T, db *sql.DB, leaseExpiry string) {
	t.Helper()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO runs(id,world_id,scenario,provider,task,status) VALUES(?,?,?,?,?,?)`,
			[]any{"R-1", "W-1", "duplicate-charge", "scripted", "task", "running"}},
		{`INSERT INTO run_leases(run_id,owner,fence,committed_fence,expires_at,acquired_at,heartbeat_at) VALUES(?,?,?,?,?,?,?)`,
			[]any{"R-1", "worker-a", 1, 0, leaseExpiry, "2026-01-01T12:00:00Z", "2026-01-01T12:00:00.25Z"}},
		{`INSERT INTO work_queue(run_id,state,max_steps,enqueued_at,available_at) VALUES(?,?,?,?,?)`,
			[]any{"R-1", QueueRunnable, 10, "2026-01-01T12:00:01Z", "2026-01-01T12:00:01Z"}},
		{`INSERT INTO workers(id,host,started_at,last_seen_at) VALUES(?,?,?,?)`,
			[]any{"worker-a", "host", "2026-01-01T12:00:00Z", "2026-01-01T12:00:01.5Z"}},
	} {
		if _, err := db.ExecContext(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed %.40s: %v", stmt.sql, err)
		}
	}
}

func rawSchemaVersion(ctx context.Context, t *testing.T, db *sql.DB) int {
	t.Helper()
	var version int
	if err := db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func TestMigrationThreeNormalizesCoordinationTimestamps(t *testing.T) {
	ctx := context.Background()
	db := openRawAtVersionTwo(ctx, t)
	seedV2Coordination(ctx, t, db, "2026-01-01T12:00:01Z")

	if err := applyMigrations(ctx, db, DialectSQLite, migrations); err != nil {
		t.Fatalf("apply migration 3: %v", err)
	}

	for _, want := range []struct{ table, column, key, value string }{
		{"run_leases", "expires_at", "run_id", "2026-01-01T12:00:01.000000000Z"},
		{"run_leases", "acquired_at", "run_id", "2026-01-01T12:00:00.000000000Z"},
		{"run_leases", "heartbeat_at", "run_id", "2026-01-01T12:00:00.250000000Z"},
		{"work_queue", "enqueued_at", "run_id", "2026-01-01T12:00:01.000000000Z"},
		{"work_queue", "available_at", "run_id", "2026-01-01T12:00:01.000000000Z"},
		{"workers", "started_at", "id", "2026-01-01T12:00:00.000000000Z"},
		{"workers", "last_seen_at", "id", "2026-01-01T12:00:01.500000000Z"},
	} {
		var got string
		query := "SELECT " + want.column + " FROM " + want.table + " LIMIT 1"
		if err := db.QueryRowContext(ctx, query).Scan(&got); err != nil {
			t.Fatalf("%s.%s: %v", want.table, want.column, err)
		}
		if got != want.value {
			t.Errorf("%s.%s=%q want %q", want.table, want.column, got, want.value)
		}
	}
	if version := rawSchemaVersion(ctx, t, db); version != 3 {
		t.Fatalf("schema version=%d want 3", version)
	}
}

func TestMigrationThreeRefusesAnUnparseableTimestamp(t *testing.T) {
	ctx := context.Background()
	db := openRawAtVersionTwo(ctx, t)
	seedV2Coordination(ctx, t, db, "yesterday")

	err := applyMigrations(ctx, db, DialectSQLite, migrations)
	if err == nil {
		t.Fatal("migration 3 accepted an unparseable timestamp")
	}
	if !strings.Contains(err.Error(), "run_leases.expires_at") {
		t.Fatalf("error does not name the column: %v", err)
	}
	if !strings.Contains(err.Error(), `run_id="R-1"`) {
		t.Fatalf("error does not name the row key: %v", err)
	}
	if version := rawSchemaVersion(ctx, t, db); version != 2 {
		t.Fatalf("schema version=%d want 2 (the migration should have rolled back)", version)
	}
	var expires string
	if err = db.QueryRowContext(ctx, "SELECT expires_at FROM run_leases").Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if expires != "yesterday" {
		t.Fatalf("rollback left expires_at=%q", expires)
	}
}

// An empty value means "not set", which a coordination row can legitimately
// hold, so only non-empty text that does not parse is an error.
func TestMigrationThreeLeavesEmptyTimestampsAlone(t *testing.T) {
	ctx := context.Background()
	db := openRawAtVersionTwo(ctx, t)
	seedV2Coordination(ctx, t, db, "")

	if err := applyMigrations(ctx, db, DialectSQLite, migrations); err != nil {
		t.Fatalf("apply migration 3: %v", err)
	}
	var expires string
	if err := db.QueryRowContext(ctx, "SELECT expires_at FROM run_leases").Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if expires != "" {
		t.Fatalf("expires_at=%q want empty", expires)
	}
}
