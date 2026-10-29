package store

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestOpenReadOnlyNamesAMissingPath(t *testing.T) {
	missing := t.TempDir() + "/typo.db"
	s, err := OpenReadOnly(missing)
	if err == nil {
		s.Close()
		t.Fatal("a missing database opened")
	}
	if !strings.Contains(err.Error(), missing) || strings.Contains(err.Error(), "out of memory") {
		t.Fatalf("the error should name the path and not SQLite's wording: %v", err)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Fatalf("a read-only open created %s: %v", missing, statErr)
	}
}

// databaseAtVersion returns a read-only handle on a database whose schema record
// says version, or on a database with no record at all when version is 0.
func databaseAtVersion(t *testing.T, version int) *Store {
	t.Helper()
	path := t.TempDir() + "/versioned.db"
	writable, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = writable.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{"DELETE FROM schema_migrations WHERE version>" + strconv.Itoa(version)}
	switch {
	case version == 0:
		statements = []string{"DROP TABLE schema_migrations"}
	case version > CurrentSchemaVersion:
		statements = []string{"INSERT INTO schema_migrations(version,name,applied_at) VALUES(" + strconv.Itoa(version) + ",'future','2026-01-01T00:00:00Z')"}
	}
	for _, stmt := range statements {
		if _, err = raw.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { readOnly.Close() })
	return readOnly
}

// RequireSchema is what lets a read-only command refuse an old database instead
// of migrating it, and refuse a newer one the way the migrating open does.
func TestRequireSchema(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		version  int
		min      int
		contains string
	}{
		{"current database for the queue", CurrentSchemaVersion, SchemaVersionQueue, ""},
		{"previous release for the queue", 2, SchemaVersionQueue, ""},
		{"base schema for inspection", 1, SchemaVersionLedger, ""},
		{"base schema for the queue", 1, SchemaVersionQueue, "schema version 1 is older than the work queue needs (version 2)"},
		{"no migration record for inspection", 0, SchemaVersionLedger, "schema version 0 is older than inspection needs (version 1)"},
		{"newer build", 99, SchemaVersionLedger, "schema version 99 is newer than this build supports"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := databaseAtVersion(t, tc.version)
			feature := "inspection"
			if tc.min == SchemaVersionQueue {
				feature = "the work queue"
			}
			err := s.RequireSchema(ctx, tc.min, feature)
			if tc.contains == "" {
				if err != nil {
					t.Fatalf("RequireSchema: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("RequireSchema err=%v, want it to contain %q", err, tc.contains)
			}
		})
	}
}
