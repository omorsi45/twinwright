package chaos

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"twinwright/internal/store"
)

func TestDecideUsesRunLocalDeterministicCounters(t *testing.T) {
	ctx := context.Background()
	policy := Policy{Version: 1, Rules: []Rule{
		{ID: "first", Type: "http_error", Operations: []string{"getCharge"}, Status: 503, Times: 1},
		{ID: "second", Type: "latency", Operations: []string{"getCharge"}, DurationMS: 10},
	}}
	encoded, err := policy.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "chaos.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w, err := s.Seed(ctx, 42, "digest")
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for i := 0; i < 2; i++ {
		run, err := s.CreateRunWithChaos(ctx, w.ID, "duplicate-charge", "scripted", "fixture-v1", "task", "", encoded, policy.Digest())
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, run.ID)
	}
	for _, id := range ids {
		for i, want := range []string{"first", "second", "second"} {
			tx, err := s.DB.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			decision, err := Decide(ctx, tx, id, "getCharge", []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			if decision.RuleID != want {
				t.Fatalf("run=%s call=%d decision=%+v want=%s", id, i+1, decision, want)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
		var firstCalls, secondCalls int
		if err := s.DB.QueryRowContext(ctx, "SELECT matching_calls FROM chaos_rule_state WHERE run_id=? AND rule_id='first'", id).Scan(&firstCalls); err != nil {
			t.Fatal(err)
		}
		if err := s.DB.QueryRowContext(ctx, "SELECT matching_calls FROM chaos_rule_state WHERE run_id=? AND rule_id='second'", id).Scan(&secondCalls); err != nil {
			t.Fatal(err)
		}
		if firstCalls != 3 || secondCalls != 3 {
			t.Fatalf("counts=%d,%d", firstCalls, secondCalls)
		}
	}
}

func TestSnapshotsAreArgumentScoped(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "snapshots.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveSnapshot(ctx, tx, "run", "stale", "getCharge", []byte(`{"id":"A"}`), 200, []byte(`{"value":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadSnapshot(ctx, tx, "run", "stale", "getCharge", []byte(`{"id":"B"}`)); err != sql.ErrNoRows {
		t.Fatalf("different argument snapshot error=%v", err)
	}
	status, body, err := LoadSnapshot(ctx, tx, "run", "stale", "getCharge", []byte(`{"id":"A"}`))
	if err != nil || status != 200 || string(body) != `{"value":1}` {
		t.Fatalf("snapshot=%d %s err=%v", status, body, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
