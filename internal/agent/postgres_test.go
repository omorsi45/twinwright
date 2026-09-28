package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/eval"
	"twinwright/internal/pgtest"
	"twinwright/internal/store"
)

func billingManifest(t *testing.T) compiler.Manifest {
	t.Helper()
	root := filepath.Join("..", "..", "examples", "billing")
	spec, err := os.ReadFile(filepath.Join(root, "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := os.ReadFile(filepath.Join(root, "bindings.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := compiler.Compile(spec, bindings)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

// TestPostgresEndToEndRunRefundsExactlyOnce drives the whole agent loop -
// runner, dispatcher, validation, behavior handlers, ledger - against a real
// PostgreSQL server, including the injected 503 and the retry that follows it.
// Storage parity is only meaningful if the runtime, not just the schema, works.
func TestPostgresEndToEndRunRefundsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s, err := store.OpenDSN(ctx, pgtest.SchemaDSN(t))
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer s.Close()
	world, err := s.Seed(ctx, 42, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "test", "test-model", "task", "listCharges")
	if err != nil {
		t.Fatal(err)
	}
	runner := Runner{
		Store:    s,
		Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest},
		Manifest: manifest,
		Provider: scenarioProvider{},
	}
	completed, err := runner.Execute(ctx, run.ID, 10)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if completed.Status != "completed" {
		t.Fatalf("status=%s", completed.Status)
	}
	report, err := eval.DuplicateCharge(ctx, s, world.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed {
		t.Fatalf("evaluation failed on postgres: %+v", report)
	}
	var refunds int
	if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM refunds WHERE world_id=?", world.ID).Scan(&refunds); err != nil {
		t.Fatal(err)
	}
	if refunds != 1 {
		t.Fatalf("refunds=%d want exactly 1", refunds)
	}
	events, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("no events recorded on postgres")
	}
	for i, event := range events {
		if event.Seq != i+1 {
			t.Fatalf("ledger gap at index %d: seq=%d", i, event.Seq)
		}
	}
	saw := map[string]bool{}
	for _, event := range events {
		saw[event.Type] = true
	}
	for _, required := range []string{"execution.started", "tool.request", "tool.response", "state.mutation", "execution.completed"} {
		if !saw[required] {
			t.Fatalf("postgres run is missing %s events", required)
		}
	}
}

// TestPostgresDuplicateCallIDIsIdempotent is the storage-level statement of the
// delivery contract: the same call ID replayed returns the saved result and
// commits no second effect. This is what makes at-least-once delivery safe.
func TestPostgresDuplicateCallIDIsIdempotent(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s, err := store.OpenDSN(ctx, pgtest.SchemaDSN(t))
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer s.Close()
	world, err := s.Seed(ctx, 42, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "test", "test-model", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &dispatch.Dispatcher{Store: s, Manifest: manifest}
	args := map[string]any{"charge_id": "CH-1002", "amount_cents": 500, "reason": "duplicate"}
	first, err := dispatcher.Invoke(ctx, run.ID, "call-1", "createRefund", args)
	if err != nil {
		t.Fatalf("first invoke: %v", err)
	}
	if first.Status < 200 || first.Status >= 300 {
		t.Fatalf("first refund status=%d", first.Status)
	}
	second, err := dispatcher.Invoke(ctx, run.ID, "call-1", "createRefund", args)
	if err != nil {
		t.Fatalf("duplicate delivery of the same call ID must succeed idempotently: %v", err)
	}
	if string(second.Body) != string(first.Body) || second.Status != first.Status {
		t.Fatalf("duplicate delivery returned a different result:\n first  %d %s\n second %d %s", first.Status, first.Body, second.Status, second.Body)
	}
	var refunds int
	if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM refunds WHERE world_id=?", world.ID).Scan(&refunds); err != nil {
		t.Fatal(err)
	}
	if refunds != 1 {
		t.Fatalf("duplicate delivery committed %d refunds", refunds)
	}
	// Reusing a call ID with different arguments is a different request and
	// must be refused rather than silently answered from the cache.
	changed := map[string]any{"charge_id": "CH-1002", "amount_cents": 900, "reason": "duplicate"}
	if _, err = dispatcher.Invoke(ctx, run.ID, "call-1", "createRefund", changed); err == nil {
		t.Fatal("expected a reused call ID with changed arguments to fail")
	}
}
