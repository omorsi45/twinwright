package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"twinwright/internal/store"
)

// legacyTime is a coordination time in the variable-width form that builds
// before migration 3 wrote. Migration 3 rewrites it to a fixed width, so a text
// that is still in this form after a command ran proves the command did not
// migrate.
const legacyTime = "2026-01-01T12:00:01Z"

// seededDatabase is a SQLite database that holds one finished run and one
// queue entry.
type seededDatabase struct {
	path  string
	runID string
}

// newSeededDatabase records one scripted run through the CLI and queues it.
func newSeededDatabase(t *testing.T) seededDatabase {
	t.Helper()
	root := filepath.Join("..", "..", "examples", "billing")
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.json")
	path := filepath.Join(dir, "world.db")
	if err := runCLI([]string{"build", filepath.Join(root, "openapi.yaml"), "--bindings", filepath.Join(root, "bindings.yaml"), "--out", manifest}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runCLI([]string{"run", "duplicate-charge", "--agent", "scripted", "--manifest", manifest, "--db", path, "--seed", "42", "--steps", "10"}, &out); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Run.ID == "" {
		t.Fatalf("run output %q: %v", out.String(), err)
	}
	if err := runCLI([]string{"enqueue", result.Run.ID, "--db", path}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	return seededDatabase{path: path, runID: result.Run.ID}
}

// rewind makes the database look like one written by the previous release:
// migration 3 is forgotten and the coordination times are in the old form.
func (d seededDatabase) rewind(t *testing.T) {
	t.Helper()
	raw, err := sql.Open("sqlite", d.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"DELETE FROM schema_migrations WHERE version=3",
		"UPDATE run_leases SET expires_at='" + legacyTime + "'",
		"UPDATE work_queue SET available_at='" + legacyTime + "'",
	} {
		if _, err = raw.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}
}

// dropQueueTables leaves the database as it was before the distributed runtime
// existed: schema version 1, with no lease, queue, worker or ownership table.
func (d seededDatabase) dropQueueTables(t *testing.T) {
	t.Helper()
	raw, err := sql.Open("sqlite", d.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"DROP TABLE run_leases", "DROP TABLE work_queue", "DROP TABLE workers", "DROP TABLE run_ownership_log",
		"DELETE FROM schema_migrations WHERE version>1",
	} {
		if _, err = raw.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}
}

// digest is the SHA-256 of the database file, which changes if any page does.
func digest(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// recordedState reads what a migration would have changed, through a read-only
// open so that reading it cannot change it either.
func recordedState(t *testing.T, path string) (versions string, leaseExpiry string) {
	t.Helper()
	s, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.DB.Query("SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var version string
		if err = rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, version)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow("SELECT expires_at FROM run_leases LIMIT 1").Scan(&leaseExpiry); err != nil {
		t.Fatal(err)
	}
	return strings.Join(parts, ","), leaseExpiry
}

// readCommands lists the commands the documentation calls read-only, each as the
// argument list for a database.
func readCommands(d seededDatabase) map[string][]string {
	return map[string][]string{
		"inspect":   {"inspect", d.runID, "--db", d.path},
		"queue":     {"queue", "--db", d.path},
		"queue run": {"queue", "--run", d.runID, "--db", d.path},
		"doctor":    {"doctor", "--db", d.path},
	}
}

// TestReadCommandsDoNotMigrateTheSource pins the AGENTS.md invariant that
// inspection never mutates the source database, for the three commands that
// used to open it for writing. Each runs against a database one migration
// behind, and the file must come out byte for byte as it went in.
func TestReadCommandsDoNotMigrateTheSource(t *testing.T) {
	for name := range readCommands(seededDatabase{}) {
		t.Run(name, func(t *testing.T) {
			d := newSeededDatabase(t)
			d.rewind(t)
			wantVersions, wantExpiry := recordedState(t, d.path)
			if wantVersions != "1,2" || wantExpiry != legacyTime {
				t.Fatalf("setup: versions %q, lease expiry %q", wantVersions, wantExpiry)
			}
			before := digest(t, d.path)

			var out bytes.Buffer
			if err := runCLI(readCommands(d)[name], &out); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}

			if after := digest(t, d.path); after != before {
				t.Fatal("the command changed the database file")
			}
			gotVersions, gotExpiry := recordedState(t, d.path)
			if gotVersions != wantVersions || gotExpiry != wantExpiry {
				t.Fatalf("migrated: versions %q, lease expiry %q", gotVersions, gotExpiry)
			}
		})
	}
}

// TestReadCommandsRefuseAMissingDatabase: a mistyped --db must fail and name
// the path, not print an empty queue and leave a new database behind.
func TestReadCommandsRefuseAMissingDatabase(t *testing.T) {
	for _, name := range []string{"inspect", "queue", "queue run"} {
		t.Run(name, func(t *testing.T) {
			missing := filepath.Join(t.TempDir(), "typo.db")
			d := seededDatabase{path: missing, runID: "R-1"}
			var out bytes.Buffer
			err := runCLI(readCommands(d)[name], &out)
			if err == nil {
				t.Fatalf("a missing database was accepted:\n%s", out.String())
			}
			if !strings.Contains(err.Error(), missing) {
				t.Fatalf("the error does not name the path %q: %v", missing, err)
			}
			if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
				t.Fatalf("the command created %s: %v", missing, statErr)
			}
		})
	}
	t.Run("doctor", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "typo.db")
		var out bytes.Buffer
		if err := runCLI([]string{"doctor", "--db", missing}, &out); err != nil {
			t.Fatal(err)
		}
		var report struct {
			Healthy bool `json:"healthy"`
			Checks  []struct {
				Check  string `json:"check"`
				OK     bool   `json:"ok"`
				Detail any    `json:"detail"`
			} `json:"checks"`
		}
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatalf("%v\n%s", err, out.String())
		}
		if report.Healthy {
			t.Fatalf("doctor called a missing database healthy:\n%s", out.String())
		}
		if !strings.Contains(out.String(), missing) {
			t.Fatalf("doctor does not name the path %q:\n%s", missing, out.String())
		}
		if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
			t.Fatalf("doctor created %s: %v", missing, statErr)
		}
	})
}

// TestDoctorReportsTheAppliedSchemaVersionWithoutMigrating: doctor is the
// command an operator runs before an upgrade, so it must report the version the
// database has and say what is pending, not apply it and report success.
func TestDoctorReportsTheAppliedSchemaVersionWithoutMigrating(t *testing.T) {
	d := newSeededDatabase(t)
	d.rewind(t)
	before := digest(t, d.path)

	var out bytes.Buffer
	if err := runCLI([]string{"doctor", "--db", d.path}, &out); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Healthy bool `json:"healthy"`
		Checks  []struct {
			Check  string `json:"check"`
			OK     bool   `json:"ok"`
			Detail any    `json:"detail"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	var detail string
	for _, check := range report.Checks {
		if check.Check == "storage backend" {
			detail, _ = check.Detail.(string)
			if !check.OK {
				t.Fatalf("storage backend check failed: %v", check.Detail)
			}
		}
	}
	for _, want := range []string{"schema version 2", "migration to 3 is pending"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail %q lacks %q", detail, want)
		}
	}
	if digest(t, d.path) != before {
		t.Fatal("doctor changed the database file")
	}
}

// TestReadCommandsAskForAMigrationWhenTheSchemaIsTooOld: a database that has
// no queue tables cannot be read by queue, and a read-only command must say so
// rather than create the tables.
func TestReadCommandsAskForAMigrationWhenTheSchemaIsTooOld(t *testing.T) {
	d := newSeededDatabase(t)
	d.dropQueueTables(t)
	before := digest(t, d.path)

	for _, args := range [][]string{{"queue", "--db", d.path}, {"queue", "--run", d.runID, "--db", d.path}} {
		err := runCLI(args, &bytes.Buffer{})
		if err == nil {
			t.Fatalf("%v: a version 1 database was read as a queue", args)
		}
		for _, want := range []string{"schema version 1", "(version 2)", "run, resume, enqueue or worker"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%v: error %q lacks %q", args, err, want)
			}
		}
	}
	// inspect needs only the base schema, which this database still has.
	if err := runCLI([]string{"inspect", d.runID, "--db", d.path}, &bytes.Buffer{}); err != nil {
		t.Fatalf("inspect of a version 1 database: %v", err)
	}
	if digest(t, d.path) != before {
		t.Fatal("a command changed the database file")
	}
}

// TestReadCommandsRefuseADatabaseFromANewerBuild keeps the refusal that the
// migrating open used to give: operating on an unknown future schema is how
// history gets corrupted.
func TestReadCommandsRefuseADatabaseFromANewerBuild(t *testing.T) {
	d := newSeededDatabase(t)
	raw, err := sql.Open("sqlite", d.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Exec("INSERT INTO schema_migrations(version,name,applied_at) VALUES(99,'future','2026-01-01T00:00:00Z')"); err != nil {
		t.Fatal(err)
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"inspect", "queue"} {
		err := runCLI(readCommands(d)[name], &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "newer than this build supports") {
			t.Fatalf("%s: err=%v, want the newer-schema refusal", name, err)
		}
	}
	var out bytes.Buffer
	if err = runCLI([]string{"doctor", "--db", d.path}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"healthy": false`) || !strings.Contains(out.String(), "newer than this build supports") {
		t.Fatalf("doctor accepted a newer schema:\n%s", out.String())
	}
}
