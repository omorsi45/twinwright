package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"twinwright/internal/pgsql"
)

// PostgresDSNPrefixes are the URL schemes OpenDSN recognises as PostgreSQL.
var PostgresDSNPrefixes = []string{"postgres://", "postgresql://"}

// defaultPostgresMaxConns bounds the pool. The multi-worker runtime wants real
// concurrency here, unlike SQLite's single writer, but an unbounded pool turns
// a worker crash loop into a connection-exhaustion incident on the server.
const defaultPostgresMaxConns = 16

// schemaIdentifier constrains the search_path schema to a plain identifier.
// The schema name reaches CREATE SCHEMA as text, so it cannot be a bind
// parameter; validating it here is what keeps that statement injection-free.
var schemaIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

// IsPostgresDSN reports whether dsn names a PostgreSQL server rather than a
// local SQLite file.
func IsPostgresDSN(dsn string) bool {
	for _, prefix := range PostgresDSNPrefixes {
		if strings.HasPrefix(dsn, prefix) {
			return true
		}
	}
	return false
}

// OpenDSN opens a world store from either a PostgreSQL URL or a local SQLite
// file path, migrating it to the current schema version.
//
// A DSN carrying an unsupported scheme is refused explicitly rather than being
// treated as a filename, which would otherwise create a nonsense SQLite file
// named after the URL.
func OpenDSN(ctx context.Context, dsn string) (*Store, error) {
	if IsPostgresDSN(dsn) {
		return OpenPostgres(ctx, dsn)
	}
	if idx := strings.Index(dsn, "://"); idx > 0 {
		return nil, fmt.Errorf("unsupported storage backend %q: Twinwright supports postgres:// URLs and local SQLite file paths", dsn[:idx])
	}
	return Open(dsn)
}

// OpenPostgres opens a PostgreSQL-backed world store.
//
// A `search_path=<schema>` query parameter scopes every table to that schema
// and is created if absent, which is how concurrent tests and several
// independent environments share one server without colliding.
func OpenPostgres(ctx context.Context, dsn string) (*Store, error) {
	pgsql.Register()
	schema, err := searchPathSchema(dsn)
	if err != nil {
		return nil, err
	}
	if schema != "" {
		if err = ensureSchema(ctx, dsn, schema); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open(pgsql.DriverName, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(defaultPostgresMaxConns)
	db.SetMaxIdleConns(defaultPostgresMaxConns)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	s := &Store{DB: db, Dialect: DialectPostgres}
	if err = s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// OpenReadOnlyDSN opens a store for reading only, from either backend.
//
// Replay, evaluation, tracing and inspection must not be able to change the run
// they are examining. On SQLite that is the file opened with mode=ro; on
// PostgreSQL it is `default_transaction_read_only=on`, which makes the SERVER
// reject a write rather than relying on this process to not attempt one. Neither
// path migrates: a read must never alter the schema of the history it is reading.
func OpenReadOnlyDSN(ctx context.Context, dsn string) (*Store, error) {
	if !IsPostgresDSN(dsn) {
		if idx := strings.Index(dsn, "://"); idx > 0 {
			return nil, fmt.Errorf("unsupported storage backend %q: Twinwright supports postgres:// URLs and local SQLite file paths", dsn[:idx])
		}
		return OpenReadOnly(dsn)
	}
	pgsql.Register()
	parsed, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres DSN: %w", err)
	}
	query := parsed.Query()
	query.Set("default_transaction_read_only", "on")
	parsed.RawQuery = query.Encode()
	db, err := sql.Open(pgsql.DriverName, parsed.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(defaultPostgresMaxConns)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	return &Store{DB: db, Dialect: DialectPostgres}, nil
}

// searchPathSchema extracts and validates the schema the DSN asks for.
func searchPathSchema(dsn string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse postgres DSN: %w", err)
	}
	schema := parsed.Query().Get("search_path")
	if schema == "" {
		return "", nil
	}
	if !schemaIdentifier.MatchString(schema) {
		return "", fmt.Errorf("search_path %q is not a plain SQL identifier", schema)
	}
	return schema, nil
}

// ensureSchema creates the target schema using a connection that does not yet
// select it, because selecting a schema that does not exist fails on connect
// for some server configurations.
func ensureSchema(ctx context.Context, dsn, schema string) error {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return err
	}
	query := parsed.Query()
	query.Del("search_path")
	parsed.RawQuery = query.Encode()
	bootstrap, err := sql.Open(pgsql.DriverName, parsed.String())
	if err != nil {
		return err
	}
	defer bootstrap.Close()
	if err = bootstrap.PingContext(ctx); err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	// schema passed schemaIdentifier above, so this interpolation is safe.
	if _, err = bootstrap.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS "`+schema+`"`); err != nil {
		return fmt.Errorf("create schema %q: %w", schema, err)
	}
	return nil
}
