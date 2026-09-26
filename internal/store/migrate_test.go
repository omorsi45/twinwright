package store

import (
	"context"
	"database/sql"
	"testing"
)

func TestOpenRecordsSchemaVersion(t *testing.T) {
	s, err := Open(t.TempDir() + "/fresh.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Dialect != DialectSQLite {
		t.Fatalf("dialect=%q", s.Dialect)
	}
	version, err := s.SchemaVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("version=%d want %d", version, CurrentSchemaVersion)
	}
	if CurrentSchemaVersion < 1 {
		t.Fatal("CurrentSchemaVersion must be at least 1")
	}
}

func TestOpenIsIdempotentAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/reopen.db"
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	world, err := first.Seed(ctx, 7, "digest")
	if err != nil {
		t.Fatal(err)
	}
	run, err := first.CreateRun(ctx, world.ID, "duplicate-charge", "scripted", "fixture-v1", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err = second.Run(ctx, run.ID); err != nil {
		t.Fatalf("reopen lost the run: %v", err)
	}
	var applied int
	if err = second.DB.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != CurrentSchemaVersion {
		t.Fatalf("applied=%d want %d (reopen re-recorded or skipped migrations)", applied, CurrentSchemaVersion)
	}
}

// A database written before schema_migrations existed must be upgraded in
// place without losing its rows: this is the on-disk history of real runs.
func TestOpenBaselinesLegacyDatabaseWithoutDataLoss(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/legacy-baseline.db"
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE worlds (id TEXT PRIMARY KEY, seed INTEGER NOT NULL, digest TEXT NOT NULL, base_at TEXT NOT NULL)`,
		`CREATE TABLE runs (id TEXT PRIMARY KEY, world_id TEXT NOT NULL, scenario TEXT NOT NULL, provider TEXT NOT NULL, task TEXT NOT NULL, status TEXT NOT NULL, step INTEGER NOT NULL DEFAULT 0, transcript TEXT NOT NULL DEFAULT '[]', fault_operation TEXT NOT NULL DEFAULT '', fault_consumed INTEGER NOT NULL DEFAULT 0)`,
		`INSERT INTO worlds VALUES('W-legacy',1,'digest','2026-01-01T00:00:00Z')`,
		`INSERT INTO runs(id,world_id,scenario,provider,task,status) VALUES('R-legacy','W-legacy','duplicate-charge','scripted','task','completed')`,
	} {
		if _, err = raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("legacy version=%d want %d", version, CurrentSchemaVersion)
	}
	old, err := s.Run(ctx, "R-legacy")
	if err != nil {
		t.Fatalf("legacy run lost: %v", err)
	}
	if old.PrincipalID != LegacyPrincipal {
		t.Fatalf("legacy principal=%q", old.PrincipalID)
	}
}

// A read-only open must not migrate: replay, evaluate and trace all open the
// source database read-only and must leave it byte-identical.
func TestOpenReadOnlyReportsSchemaVersionWithoutWriting(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/ro.db"
	writable, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = writable.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	if readOnly.Dialect != DialectSQLite {
		t.Fatalf("dialect=%q", readOnly.Dialect)
	}
	version, err := readOnly.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("read-only version=%d want %d", version, CurrentSchemaVersion)
	}
}

// SchemaVersion on a database that predates the migration table must report 0
// rather than failing, so callers can tell "old" from "broken".
func TestSchemaVersionIsZeroWithoutMigrationTable(t *testing.T) {
	path := t.TempDir() + "/no-table.db"
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Exec(`CREATE TABLE placeholder (id TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	version, err := readOnly.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("SchemaVersion on a pre-migration database: %v", err)
	}
	if version != 0 {
		t.Fatalf("version=%d want 0", version)
	}
}

// A migration that fails must leave no half-applied schema behind and must not
// be recorded as applied.
func TestFailedMigrationLeavesNoPartialSchema(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/partial.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	broken := []migration{{
		version: CurrentSchemaVersion + 1,
		name:    "intentionally broken",
		statements: func(Dialect) []string {
			return []string{
				`CREATE TABLE migration_probe (id TEXT PRIMARY KEY)`,
				`THIS IS NOT SQL`,
			}
		},
	}}
	if err = applyMigrations(ctx, s.DB, s.Dialect, broken); err == nil {
		t.Fatal("expected the broken migration to fail")
	}
	var tables int
	if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='migration_probe'").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatal("failed migration left migration_probe behind")
	}
	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("failed migration was recorded: version=%d", version)
	}
}

func TestDialectIntrospectionFindsTablesAndColumns(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/introspect.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	present, err := s.HasTable(ctx, "runs")
	if err != nil || !present {
		t.Fatalf("HasTable(runs)=%v err=%v", present, err)
	}
	absent, err := s.HasTable(ctx, "definitely_not_a_table")
	if err != nil || absent {
		t.Fatalf("HasTable(missing)=%v err=%v", absent, err)
	}
	column, err := s.HasColumn(ctx, "runs", "principal_id")
	if err != nil || !column {
		t.Fatalf("HasColumn(runs,principal_id)=%v err=%v", column, err)
	}
	missing, err := s.HasColumn(ctx, "runs", "not_a_column")
	if err != nil || missing {
		t.Fatalf("HasColumn(missing)=%v err=%v", missing, err)
	}
}
