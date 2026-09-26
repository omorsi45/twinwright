package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Dialect names the SQL backend a Store is talking to. Twinwright supports
// SQLite for local development, deterministic examples and replay, and
// PostgreSQL for the multi-worker runtime.
type Dialect string

const (
	DialectSQLite   Dialect = "sqlite"
	DialectPostgres Dialect = "postgres"
)

// CurrentSchemaVersion is the highest migration this build knows how to apply.
// A database recording a HIGHER version was written by a newer Twinwright and
// is refused rather than silently downgraded.
const CurrentSchemaVersion = 2

// intType keeps integer widths honest per backend. SQLite's INTEGER is a
// 64-bit signed value; PostgreSQL's INTEGER is 32-bit, which would overflow
// world seeds and cent amounts, so PostgreSQL gets BIGINT.
func intType(d Dialect) string {
	if d == DialectPostgres {
		return "BIGINT"
	}
	return "INTEGER"
}

// migration is one forward step. statements run in order inside a single
// transaction; columns are added only when absent, which is what lets the
// base migration adopt a database written before migrations were tracked.
type migration struct {
	version    int
	name       string
	statements func(Dialect) []string
	columns    []columnAddition
}

// columnAddition is an idempotent ALTER TABLE ... ADD COLUMN.
type columnAddition struct {
	table  string
	column string
	// definition is rendered with %INT% substituted for the dialect's
	// integer type.
	definition string
}

// baseSchema is the schema as it stood when migration tracking was
// introduced, expressed once and rendered per dialect. Every statement is
// IF NOT EXISTS so it can adopt an existing database.
func baseSchema(d Dialect) []string {
	i := intType(d)
	tables := []string{
		`CREATE TABLE IF NOT EXISTS worlds (id TEXT PRIMARY KEY, seed %INT% NOT NULL, digest TEXT NOT NULL, base_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS customers (world_id TEXT NOT NULL, id TEXT NOT NULL, name TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS invoices (world_id TEXT NOT NULL, id TEXT NOT NULL, customer_id TEXT NOT NULL, amount_cents %INT% NOT NULL, subscription_id TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS charges (world_id TEXT NOT NULL, id TEXT NOT NULL, invoice_id TEXT NOT NULL, amount_cents %INT% NOT NULL, refunded_cents %INT% NOT NULL DEFAULT 0, created_at TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS refunds (world_id TEXT NOT NULL, id TEXT NOT NULL, charge_id TEXT NOT NULL, amount_cents %INT% NOT NULL, reason TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS subscriptions (world_id TEXT NOT NULL, id TEXT NOT NULL, customer_id TEXT NOT NULL, status TEXT NOT NULL, plan TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS crm_accounts (world_id TEXT NOT NULL, id TEXT NOT NULL, customer_id TEXT NOT NULL, status TEXT NOT NULL, representative_id TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS crm_contacts (world_id TEXT NOT NULL, id TEXT NOT NULL, account_id TEXT NOT NULL, name TEXT NOT NULL, email TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS crm_notes (world_id TEXT NOT NULL, id TEXT NOT NULL, account_id TEXT NOT NULL, body TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS ticket_projects (world_id TEXT NOT NULL, id TEXT NOT NULL, key TEXT NOT NULL, name TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS ticket_issues (world_id TEXT NOT NULL, id TEXT NOT NULL, project_id TEXT NOT NULL, account_id TEXT NOT NULL, title TEXT NOT NULL, status TEXT NOT NULL, priority TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS ticket_comments (world_id TEXT NOT NULL, id TEXT NOT NULL, issue_id TEXT NOT NULL, body TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS message_workspaces (world_id TEXT NOT NULL, id TEXT NOT NULL, name TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS message_channels (world_id TEXT NOT NULL, id TEXT NOT NULL, workspace_id TEXT NOT NULL, name TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS message_members (world_id TEXT NOT NULL, channel_id TEXT NOT NULL, principal_id TEXT NOT NULL, PRIMARY KEY(world_id,channel_id,principal_id))`,
		`CREATE TABLE IF NOT EXISTS message_messages (world_id TEXT NOT NULL, id TEXT NOT NULL, channel_id TEXT NOT NULL, body TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS runs (id TEXT PRIMARY KEY, world_id TEXT NOT NULL, scenario TEXT NOT NULL, provider TEXT NOT NULL, model TEXT NOT NULL DEFAULT '', task TEXT NOT NULL, status TEXT NOT NULL, step %INT% NOT NULL DEFAULT 0, transcript TEXT NOT NULL DEFAULT '[]', fault_operation TEXT NOT NULL DEFAULT '', fault_consumed %INT% NOT NULL DEFAULT 0, principal_id TEXT NOT NULL DEFAULT '` + LegacyPrincipal + `')`,
		`CREATE TABLE IF NOT EXISTS events (run_id TEXT NOT NULL, seq %INT% NOT NULL, id TEXT NOT NULL UNIQUE, recorded_at TEXT NOT NULL, world_at TEXT NOT NULL, type TEXT NOT NULL, payload TEXT NOT NULL, PRIMARY KEY(run_id,seq))`,
		`CREATE TABLE IF NOT EXISTS tool_results (run_id TEXT NOT NULL, call_id TEXT NOT NULL, operation_id TEXT NOT NULL, arguments TEXT NOT NULL, status %INT% NOT NULL, body TEXT NOT NULL, PRIMARY KEY(run_id,call_id))`,
		`CREATE TABLE IF NOT EXISTS checkpoints (id TEXT PRIMARY KEY, run_id TEXT NOT NULL, event_seq %INT% NOT NULL, format_version %INT% NOT NULL, manifest_digest TEXT NOT NULL, prefix_digest TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS fork_lineage (child_run_id TEXT PRIMARY KEY, parent_run_id TEXT NOT NULL, fork_event_seq %INT% NOT NULL, checkpoint_id TEXT NOT NULL, format_version %INT% NOT NULL, manifest_digest TEXT NOT NULL, prefix_digest TEXT NOT NULL, parent_provider TEXT NOT NULL, parent_model TEXT NOT NULL, chaos_replaced %INT% NOT NULL DEFAULT 0, auth_replaced %INT% NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS run_chaos (run_id TEXT PRIMARY KEY, policy_json TEXT NOT NULL, digest TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS chaos_rule_state (run_id TEXT NOT NULL, rule_id TEXT NOT NULL, matching_calls %INT% NOT NULL DEFAULT 0, injections %INT% NOT NULL DEFAULT 0, PRIMARY KEY(run_id,rule_id))`,
		`CREATE TABLE IF NOT EXISTS chaos_snapshots (run_id TEXT NOT NULL, rule_id TEXT NOT NULL, arguments_digest TEXT NOT NULL, status %INT% NOT NULL, body TEXT NOT NULL, PRIMARY KEY(run_id,rule_id,arguments_digest))`,
		`CREATE TABLE IF NOT EXISTS chaos_hidden_outcomes (run_id TEXT NOT NULL, call_id TEXT NOT NULL, rule_id TEXT NOT NULL, status %INT% NOT NULL, body TEXT NOT NULL, PRIMARY KEY(run_id,call_id))`,
		`CREATE TABLE IF NOT EXISTS run_auth (run_id TEXT PRIMARY KEY, policy_json TEXT NOT NULL, digest TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS auth_state (run_id TEXT PRIMARY KEY, call_index %INT% NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS fork_observations (child_run_id TEXT PRIMARY KEY, call_id TEXT NOT NULL, status %INT% NOT NULL, body TEXT NOT NULL)`,
	}
	out := make([]string, 0, len(tables))
	for _, stmt := range tables {
		out = append(out, strings.ReplaceAll(stmt, "%INT%", i))
	}
	return out
}

// migrations is the ordered forward-only list. Never edit an applied
// migration: add a new version instead.
var migrations = []migration{{
	version:    1,
	name:       "base schema",
	statements: baseSchema,
	// These columns were added by ad-hoc checks in earlier builds. Repeating
	// them here as guarded additions is what upgrades a database created
	// before the corresponding feature shipped.
	columns: []columnAddition{
		{table: "runs", column: "model", definition: "TEXT NOT NULL DEFAULT ''"},
		{table: "runs", column: "principal_id", definition: "TEXT NOT NULL DEFAULT '" + LegacyPrincipal + "'"},
		{table: "fork_lineage", column: "chaos_replaced", definition: "%INT% NOT NULL DEFAULT 0"},
		{table: "fork_lineage", column: "auth_replaced", definition: "%INT% NOT NULL DEFAULT 0"},
	},
}, {
	version:    2,
	name:       "distributed runtime leases and work queue",
	statements: distributedSchema,
}}

// distributedSchema adds the multi-worker runtime's own tables: fenced run
// ownership and the work queue workers claim from.
//
// Leases live in the same database as the runs they own, which is the point.
// A lease in a separate store cannot be checked in the same transaction as the
// state transition it protects, so a stale worker could pass the check and then
// commit anyway. Co-locating them makes the fence check and the write atomic.
func distributedSchema(d Dialect) []string {
	tables := []string{
		`CREATE TABLE IF NOT EXISTS run_leases (
  run_id TEXT PRIMARY KEY,
  owner TEXT NOT NULL,
  fence %INT% NOT NULL,
  committed_fence %INT% NOT NULL DEFAULT 0,
  expires_at TEXT NOT NULL,
  acquired_at TEXT NOT NULL,
  heartbeat_at TEXT NOT NULL
)`,
		`CREATE TABLE IF NOT EXISTS work_queue (
  run_id TEXT PRIMARY KEY,
  state TEXT NOT NULL,
  max_steps %INT% NOT NULL,
  attempts %INT% NOT NULL DEFAULT 0,
  enqueued_at TEXT NOT NULL,
  available_at TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT ''
)`,
		`CREATE INDEX IF NOT EXISTS work_queue_claimable ON work_queue(state, available_at)`,
		`CREATE TABLE IF NOT EXISTS workers (
  id TEXT PRIMARY KEY,
  host TEXT NOT NULL,
  started_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  claims %INT% NOT NULL DEFAULT 0
)`,
		// Ownership history is deliberately NOT in the run ledger.
		//
		// Fork lineage and checkpoint reconstruction digest the run's event
		// prefix. If ownership transitions lived there, an identical agent
		// trajectory would produce a different prefix digest depending on
		// which worker happened to execute it and how many times a lease
		// changed hands, and replay would have to special-case events that
		// cannot be regenerated. Keeping ownership in its own append-only
		// audit table leaves the ledger a pure record of what the agent did,
		// so a distributed run and a single-node replay of the same
		// trajectory produce byte-identical ledgers.
		`CREATE TABLE IF NOT EXISTS run_ownership_log (
  run_id TEXT NOT NULL,
  seq %INT% NOT NULL,
  at TEXT NOT NULL,
  kind TEXT NOT NULL,
  owner TEXT NOT NULL,
  fence %INT% NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(run_id,seq)
)`,
	}
	out := make([]string, 0, len(tables))
	for _, stmt := range tables {
		out = append(out, strings.ReplaceAll(stmt, "%INT%", intType(d)))
	}
	return out
}

const migrationTable = `CREATE TABLE IF NOT EXISTS schema_migrations (version %INT% PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`

// applyMigrations brings db up to the highest version in list. Each migration
// runs in its own transaction together with its bookkeeping row, so a failure
// leaves neither a half-applied schema nor a false record of success. Both
// SQLite and PostgreSQL support transactional DDL, which is what makes this
// guarantee real rather than aspirational.
func applyMigrations(ctx context.Context, db *sql.DB, dialect Dialect, list []migration) error {
	if _, err := db.ExecContext(ctx, strings.ReplaceAll(migrationTable, "%INT%", intType(dialect))); err != nil {
		return fmt.Errorf("migration table: %w", err)
	}
	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}
	for _, m := range list {
		if applied[m.version] {
			continue
		}
		if err := applyOne(ctx, db, dialect, m); err != nil {
			return fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
		}
	}
	return nil
}

func applyOne(ctx context.Context, db *sql.DB, dialect Dialect, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if m.statements != nil {
		for _, stmt := range m.statements(dialect) {
			if _, err = tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("%s: %w", firstLine(stmt), err)
			}
		}
	}
	for _, add := range m.columns {
		present, err := hasColumnTx(ctx, tx, dialect, add.table, add.column)
		if err != nil {
			return err
		}
		if present {
			continue
		}
		definition := strings.ReplaceAll(add.definition, "%INT%", intType(dialect))
		stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", add.table, add.column, definition)
		if _, err = tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version,name,applied_at) VALUES(?,?,?)", m.version, m.name, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

func appliedVersions(ctx context.Context, db *sql.DB) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	applied := map[int]bool{}
	for rows.Next() {
		var version int
		if err = rows.Scan(&version); err != nil {
			return nil, err
		}
		applied[version] = true
	}
	return applied, rows.Err()
}

func firstLine(stmt string) string {
	if idx := strings.IndexAny(stmt, "\r\n"); idx >= 0 {
		return stmt[:idx]
	}
	if len(stmt) > 80 {
		return stmt[:80]
	}
	return stmt
}

// SchemaVersion reports the highest applied migration. A database written
// before migration tracking existed reports 0 instead of an error, so callers
// can distinguish an old database from an unreadable one.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	present, err := s.HasTable(ctx, "schema_migrations")
	if err != nil {
		return 0, err
	}
	if !present {
		return 0, nil
	}
	var version sql.NullInt64
	if err := s.DB.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	if !version.Valid {
		return 0, nil
	}
	return int(version.Int64), nil
}

// HasTable reports whether the named table exists in the current schema.
func (s *Store) HasTable(ctx context.Context, table string) (bool, error) {
	return hasTableQuery(ctx, s.DB, s.Dialect, table)
}

// HasColumn reports whether table has the named column.
func (s *Store) HasColumn(ctx context.Context, table, column string) (bool, error) {
	return hasColumnQuery(ctx, s.DB, s.Dialect, table, column)
}

// TableExists reports whether table exists, for callers that hold a raw
// *sql.DB (replay compares a source and a target database side by side) and
// must not assume SQLite's catalog.
func TableExists(ctx context.Context, db *sql.DB, dialect Dialect, table string) (bool, error) {
	return hasTableQuery(ctx, db, dialect, table)
}

// querier is the read surface shared by *sql.DB and *sql.Tx.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func hasTableQuery(ctx context.Context, q querier, dialect Dialect, table string) (bool, error) {
	var count int
	var err error
	switch dialect {
	case DialectPostgres:
		err = q.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name=?", table).Scan(&count)
	default:
		err = q.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&count)
	}
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func hasColumnQuery(ctx context.Context, q querier, dialect Dialect, table, column string) (bool, error) {
	var count int
	var err error
	switch dialect {
	case DialectPostgres:
		err = q.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=? AND column_name=?", table, column).Scan(&count)
	default:
		err = q.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info(?) WHERE name=?", table, column).Scan(&count)
	}
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func hasTableTx(ctx context.Context, tx *sql.Tx, dialect Dialect, table string) (bool, error) {
	return hasTableQuery(ctx, tx, dialect, table)
}

func hasColumnTx(ctx context.Context, tx *sql.Tx, dialect Dialect, table, column string) (bool, error) {
	present, err := hasTableQuery(ctx, tx, dialect, table)
	if err != nil || !present {
		return false, err
	}
	return hasColumnQuery(ctx, tx, dialect, table, column)
}
