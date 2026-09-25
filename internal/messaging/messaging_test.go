package messaging

import (
	"context"
	"fmt"
	"testing"

	"twinwright/internal/store"
)

func TestMessagesAreStatefulAndWorldScoped(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/world.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, err := s.SeedScenario(ctx, 42, "digest", "company-incident")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.SeedScenario(ctx, 42, "digest", "company-incident")
	if err != nil {
		t.Fatal(err)
	}
	callNumber := 0
	invoke := func(worldID, behavior string, args map[string]any) (int, any, any) {
		t.Helper()
		callNumber++
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		status, body, mutation, err := Handle(ctx, tx, worldID, "run", fmt.Sprintf("call-%d", callNumber), behavior, args)
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return status, body, mutation
	}
	status, channels, _ := invoke(first.ID, "messaging.listChannels", map[string]any{"id": "WS-1"})
	if status != 200 || len(channels.([]map[string]any)) == 0 {
		t.Fatalf("channels: status=%d body=%v", status, channels)
	}
	status, posted, mutation := invoke(first.ID, "messaging.postMessage", map[string]any{"channel_id": "CH-SUPPORT", "body": "Customer C-104 duplicate billing incident"})
	if status != 201 || mutation == nil || posted.(map[string]any)["channel_id"] != "CH-SUPPORT" {
		t.Fatalf("post: status=%d body=%v mutation=%v", status, posted, mutation)
	}
	status, read, _ := invoke(first.ID, "messaging.readChannel", map[string]any{"id": "CH-SUPPORT"})
	messages := read.(map[string]any)["messages"].([]map[string]any)
	if status != 200 || len(messages) != 1 || messages[0]["body"] != "Customer C-104 duplicate billing incident" {
		t.Fatalf("first world read: status=%d body=%v", status, read)
	}
	status, read, _ = invoke(second.ID, "messaging.readChannel", map[string]any{"id": "CH-SUPPORT"})
	if status != 200 || len(read.(map[string]any)["messages"].([]map[string]any)) != 0 {
		t.Fatalf("second world leaked message: status=%d body=%v", status, read)
	}
	status, _, _ = invoke(first.ID, "messaging.postMessage", map[string]any{"channel_id": "missing", "body": "text"})
	if status != 404 {
		t.Fatalf("missing channel status=%d", status)
	}
	status, _, mutation = invoke(first.ID, "messaging.postMessage", map[string]any{"channel_id": "CH-SUPPORT", "body": " \t "})
	if status != 400 || mutation != nil {
		t.Fatalf("blank message status=%d mutation=%v", status, mutation)
	}
}

func TestMessagePostRollsBackWithTransaction(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/world.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	world, err := s.SeedScenario(ctx, 42, "digest", "company-incident")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	status, _, mutation, err := Handle(ctx, tx, world.ID, "run", "call-rollback", "messaging.postMessage", map[string]any{"channel_id": "CH-SUPPORT", "body": "rolled back"})
	if err != nil || status != 201 || mutation == nil {
		t.Fatalf("post status=%d mutation=%v error=%v", status, mutation, err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM message_messages WHERE world_id=?", world.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rolled-back post persisted %d messages", count)
	}
}
