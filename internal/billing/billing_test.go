package billing

import (
	"context"
	"testing"

	"twinwright/internal/store"
)

func TestRefundHandlerRejectsWrongArgumentTypes(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/world.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	world, err := s.Seed(ctx, 42, "digest")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	status, _, _, err := Handle(ctx, tx, world.ID, "run", "call", "billing.createRefund", map[string]any{"charge_id": 123, "amount_cents": 100, "reason": true})
	if err != nil {
		t.Fatal(err)
	}
	if status != 400 {
		t.Fatalf("status=%d", status)
	}
}

func TestGetSubscriptionReadsSeededWorld(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/world.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	world, err := s.Seed(ctx, 42, "digest")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	status, body, mutation, err := Handle(ctx, tx, world.ID, "run", "call", "billing.getSubscription", map[string]any{"id": "SUB-104"})
	if err != nil {
		t.Fatal(err)
	}
	if status != 200 || mutation != nil {
		t.Fatalf("status=%d mutation=%v", status, mutation)
	}
	got, ok := body.(map[string]any)
	if !ok || got["customer_id"] != "C-104" || got["status"] != "active" {
		t.Fatalf("subscription=%v", body)
	}
}
