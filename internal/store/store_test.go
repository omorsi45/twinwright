package store

import (
	"context"
	"database/sql"
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
