package store

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestSeedReproducesInitialWorld(t *testing.T) {
	snapshot := func(seed int64) string {
		t.Helper()
		s, err := Open(t.TempDir() + "/world.db")
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		w, err := s.Seed(context.Background(), seed, "manifest-digest")
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Snapshot(context.Background(), w.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if snapshot(42) != snapshot(42) {
		t.Fatal("same seed produced different state")
	}
	if snapshot(42) == snapshot(43) {
		t.Fatal("different seeds produced identical state")
	}
}

func TestOpenMigratesPriorDevelopmentRunTable(t *testing.T) {
	path := t.TempDir() + "/world.db"
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`CREATE TABLE runs (id TEXT PRIMARY KEY, world_id TEXT NOT NULL, scenario TEXT NOT NULL, provider TEXT NOT NULL, task TEXT NOT NULL, status TEXT NOT NULL, step INTEGER NOT NULL DEFAULT 0, transcript TEXT NOT NULL DEFAULT '[]', fault_operation TEXT NOT NULL DEFAULT '', fault_consumed INTEGER NOT NULL DEFAULT 0)`)
	if err != nil {
		t.Fatal(err)
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	world, err := s.Seed(ctx, 42, "digest")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "openai", "test-model", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Model != "test-model" {
		t.Fatalf("model=%q", saved.Model)
	}
	for _, table := range []string{"checkpoints", "fork_lineage"} {
		var count int
		if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("migration did not create %s", table)
		}
	}
}

func TestLedgerOrderAndStableIDs(t *testing.T) {
	s, err := Open(t.TempDir() + "/world.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	w, err := s.Seed(ctx, 42, "digest")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, w.ID, "duplicate-charge", "scripted", "fixture-v1", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, run.ID, "model.request", map[string]any{"step": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, run.ID, "model.response", map[string]any{"step": 1}); err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Type != "execution.started" || events[1].ID != run.ID+"/2" || events[2].Seq != 3 {
		t.Fatalf("events=%+v", events)
	}
}

func TestSameSeedCreatesIndependentWorldInstances(t *testing.T) {
	s, err := Open(t.TempDir() + "/world.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	a, err := s.Seed(ctx, 42, "digest")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Seed(ctx, 42, "digest")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("two runs would share mutable world state")
	}
	beforeA, err := s.Snapshot(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeB, err := s.Snapshot(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if beforeA != beforeB {
		t.Fatal("same seed created different initial state")
	}
	if _, err = s.DB.Exec("UPDATE charges SET refunded_cents=100 WHERE world_id=? AND id='CH-1002'", a.ID); err != nil {
		t.Fatal(err)
	}
	afterB, err := s.Snapshot(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterB != beforeB {
		t.Fatal("mutation leaked across worlds")
	}
}

func TestRunPersistsProviderModel(t *testing.T) {
	s, err := Open(t.TempDir() + "/world.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	w, err := s.Seed(ctx, 42, "digest")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRun(ctx, w.ID, "duplicate-charge", "openai", "test-model", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Run(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Model != "test-model" {
		t.Fatalf("model=%q", loaded.Model)
	}
}

func TestCreateReplayRunPreservesIDAndIsolation(t *testing.T) {
	ctx := context.Background()
	source, err := Open(t.TempDir() + "/source.db")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	sourceWorld, err := source.Seed(ctx, 42, "digest")
	if err != nil {
		t.Fatal(err)
	}
	original, err := source.CreateRun(ctx, sourceWorld.ID, "duplicate-charge", "scripted", "fixture-v1", "task", "listCharges")
	if err != nil {
		t.Fatal(err)
	}

	target, err := Open(t.TempDir() + "/target.db")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	targetWorld, err := target.Seed(ctx, 42, "digest")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := target.CreateReplayRun(ctx, original, targetWorld.ID)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != original.ID || replayed.WorldID != targetWorld.ID {
		t.Fatalf("replayed=%+v original=%+v", replayed, original)
	}
	saved, err := source.Run(ctx, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.WorldID != sourceWorld.ID {
		t.Fatalf("source run changed: %+v", saved)
	}
	events, err := target.Events(ctx, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "execution.started" {
		t.Fatalf("replay start events=%+v", events)
	}
}

func TestOpenReadOnlyDoesNotCreateOrMigrate(t *testing.T) {
	missing := t.TempDir() + "/missing.db"
	if s, err := OpenReadOnly(missing); err == nil {
		s.Close()
		t.Fatal("missing database opened")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("missing database created: %v", err)
	}

	path := t.TempDir() + "/legacy.db"
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Exec("CREATE TABLE runs (id TEXT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}
	source, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err = source.DB.Exec("ALTER TABLE runs ADD COLUMN model TEXT"); err == nil {
		t.Fatal("read-only database accepted a write")
	}
	var columns int
	if err = source.DB.QueryRow("SELECT count(*) FROM pragma_table_info('runs') WHERE name='model'").Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 0 {
		t.Fatal("legacy database migrated during read-only open")
	}
}

func TestSeedCompanyIncident(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/world.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w, err := s.SeedScenario(ctx, 42, "digest", "company-incident")
	if err != nil {
		t.Fatal(err)
	}

	var customer, status, representative string
	err = s.DB.QueryRowContext(ctx, "SELECT customer_id,status,representative_id FROM crm_accounts WHERE world_id=? AND id='A-104'", w.ID).Scan(&customer, &status, &representative)
	if err != nil {
		t.Fatal(err)
	}
	if customer != "C-104" || status != "active" || representative == "" {
		t.Fatalf("account customer=%q status=%q representative=%q", customer, status, representative)
	}
	var noteID, body string
	err = s.DB.QueryRowContext(ctx, "SELECT id,body FROM crm_notes WHERE world_id=? AND account_id='A-104'", w.ID).Scan(&noteID, &body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(noteID, "SEED-") || !strings.Contains(strings.ToLower(body), "retry") {
		t.Fatalf("incident evidence id=%q body=%q", noteID, body)
	}
	for _, query := range []string{
		"SELECT count(*) FROM subscriptions WHERE world_id=? AND id='SUB-104' AND customer_id='C-104'",
		"SELECT count(*) FROM crm_contacts WHERE world_id=? AND account_id='A-104'",
		"SELECT count(*) FROM ticket_projects WHERE world_id=? AND id='PROJ-ENG'",
		"SELECT count(*) FROM message_workspaces WHERE world_id=? AND id='WS-1'",
		"SELECT count(*) FROM message_channels WHERE world_id=? AND id='CH-SUPPORT' AND workspace_id='WS-1'",
		"SELECT count(*) FROM message_members WHERE world_id=? AND channel_id='CH-SUPPORT'",
	} {
		var count int
		if err := s.DB.QueryRowContext(ctx, query, w.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			t.Fatalf("missing company fixture: %s", query)
		}
	}
}

func TestSeedCompanyRoutineHasNoIncidentEvidence(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/world.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w, err := s.SeedScenario(ctx, 42, "digest", "company-routine")
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM crm_notes WHERE world_id=? AND id LIKE 'SEED-%'", w.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("routine world has %d incident evidence notes", count)
	}
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM charges WHERE world_id=? AND invoice_id='INV-104'", w.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("routine world has %d charges for INV-104", count)
	}
}

func TestSeedCompanyNoDuplicateOmitsSecondCharge(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/world.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w, err := s.SeedScenario(ctx, 42, "digest", "company-no-duplicate")
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM charges WHERE world_id=? AND invoice_id='INV-104'", w.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("no-duplicate world has %d charges for INV-104", count)
	}
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM crm_notes WHERE world_id=? AND id LIKE 'SEED-%'", w.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("no-duplicate world has %d incident evidence notes", count)
	}
}

func TestSeedCompanyRejectsUnknownScenario(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/world.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.SeedScenario(ctx, 42, "digest", "unknown"); err == nil {
		t.Fatal("unknown scenario accepted")
	}
	var count int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM worlds").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("unknown scenario created %d worlds", count)
	}
}

func TestCompanyWorldIsolation(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir() + "/world.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := s.SeedScenario(ctx, 42, "digest", "company-incident")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.SeedScenario(ctx, 42, "digest", "company-incident")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("world IDs overlap")
	}
	for _, table := range []string{"subscriptions", "crm_accounts", "crm_contacts", "crm_notes", "ticket_projects", "message_workspaces", "message_channels", "message_members"} {
		var aCount, bCount int
		query := "SELECT count(*) FROM " + table + " WHERE world_id=?"
		if err := s.DB.QueryRowContext(ctx, query, a.ID).Scan(&aCount); err != nil {
			t.Fatal(err)
		}
		if err := s.DB.QueryRowContext(ctx, query, b.ID).Scan(&bCount); err != nil {
			t.Fatal(err)
		}
		if aCount == 0 || aCount != bCount {
			t.Fatalf("%s counts: %d, %d", table, aCount, bCount)
		}
	}
	if _, err := s.DB.ExecContext(ctx, "UPDATE crm_accounts SET status='escalated' WHERE world_id=? AND id='A-104'", a.ID); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.DB.QueryRowContext(ctx, "SELECT status FROM crm_accounts WHERE world_id=? AND id='A-104'", b.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("world B status changed to %q", status)
	}
}
