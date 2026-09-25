package eval

import (
	"context"
	"testing"

	"twinwright/internal/store"
)

func TestDuplicateChargeEvaluation(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/world.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w, err := s.Seed(ctx, 42, "digest")
	if err != nil {
		t.Fatal(err)
	}
	before, err := DuplicateCharge(ctx, s, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Passed {
		t.Fatal("unrefunded world passed")
	}
	var amount int64
	if err = s.DB.QueryRow("SELECT amount_cents FROM charges WHERE world_id=? AND id='CH-1002'", w.ID).Scan(&amount); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("INSERT INTO refunds VALUES(?,?,?,?,?,?)", w.ID, "RF-test", "CH-1002", amount, "duplicate", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("UPDATE charges SET refunded_cents=? WHERE world_id=? AND id='CH-1002'", amount, w.ID); err != nil {
		t.Fatal(err)
	}
	after, err := DuplicateCharge(ctx, s, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Passed || len(after.Checks) != 4 {
		t.Fatalf("report=%+v", after)
	}
}
