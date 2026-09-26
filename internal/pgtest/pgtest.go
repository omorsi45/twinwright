// Package pgtest supplies the PostgreSQL connection every integration test in
// this repository shares.
//
// PostgreSQL tests are opt-in: without TWINWRIGHT_TEST_POSTGRES_DSN they skip,
// so `go test ./...` stays hermetic on a machine with no database. CI sets the
// variable from a service container. There is deliberately no in-memory
// substitute - the dialect differences these tests exist to catch only appear
// against a real server.
package pgtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"twinwright/internal/pgsql"
)

// DSNEnv is the environment variable holding the base DSN, for example
// postgres://twinwright:twinwright@127.0.0.1:5432/twinwright?sslmode=disable
const DSNEnv = "TWINWRIGHT_TEST_POSTGRES_DSN"

// BaseDSN returns the configured DSN or skips the test.
func BaseDSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv(DSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set; skipping PostgreSQL integration test", DSNEnv)
	}
	return dsn
}

// Available reports whether PostgreSQL tests can run, for callers that want to
// branch rather than skip.
func Available() bool { return os.Getenv(DSNEnv) != "" }

// SchemaDSN returns a DSN scoped to a freshly created, uniquely named schema
// and drops that schema when the test finishes. Every test therefore starts
// from an empty database and tests can run in parallel against one server.
func SchemaDSN(t testing.TB) string {
	t.Helper()
	base := BaseDSN(t)
	schema := "tw_" + randomSuffix(t)
	scoped := withSearchPath(t, base, schema)
	t.Cleanup(func() {
		pgsql.Register()
		db, err := sql.Open(pgsql.DriverName, base)
		if err != nil {
			t.Logf("cleanup: open %s: %v", schema, err)
			return
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// schema is generated from hex here, never from user input.
		if _, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`); err != nil {
			t.Logf("cleanup: drop schema %s: %v", schema, err)
		}
	})
	return scoped
}

func withSearchPath(t testing.TB, dsn, schema string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", DSNEnv, err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func randomSuffix(t testing.TB) string {
	t.Helper()
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("random schema name: %v", err)
	}
	return hex.EncodeToString(raw[:])
}
