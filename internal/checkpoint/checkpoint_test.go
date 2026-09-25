package checkpoint

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"twinwright/internal/agent"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

func checkpointFixture(t *testing.T) (*store.Store, string, compiler.Manifest) {
	t.Helper()
	ctx := context.Background()
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
	s, err := store.Open(filepath.Join(t.TempDir(), "checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	world, err := s.Seed(ctx, 42, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "scripted", "fixture-v1", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	message := agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "call-1", OperationID: "getCustomer", Arguments: map[string]any{"id": "C-104"}}}}
	transcript, err := json.Marshal([]agent.Message{message})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StartModelCall(ctx, run.ID, map[string]any{"task": run.Task, "provider": run.Provider, "model": run.Model, "history": []agent.Message{}, "operations": manifest.Operations}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTurn(ctx, run.ID, 1, string(transcript), message); err != nil {
		t.Fatal(err)
	}
	d := dispatch.Dispatcher{Store: s, Manifest: manifest}
	if _, err := d.Invoke(ctx, run.ID, "call-1", "getCustomer", map[string]any{"id": "C-104"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveStatus(ctx, run.ID, "paused", "execution.paused", map[string]any{"step": 1}); err != nil {
		t.Fatal(err)
	}
	return s, run.ID, manifest
}

func TestListCommittedCheckpointsWithoutWriting(t *testing.T) {
	s, runID, manifest := checkpointFixture(t)
	ctx := context.Background()
	before, err := s.Events(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	points, err := List(ctx, s, runID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 3 || points[0].EventSeq != 3 || points[1].EventSeq != 5 || points[2].EventSeq != 6 {
		t.Fatalf("checkpoint boundaries=%+v", points)
	}
	for _, point := range points {
		if point.ID == "" || point.PrefixDigest == "" || point.ManifestDigest != manifest.Digest || point.FormatVersion != 1 {
			t.Fatalf("invalid checkpoint=%+v", point)
		}
	}
	if points[1].ModelTurns != 1 || points[1].ToolCalls != 1 {
		t.Fatalf("checkpoint counts=%+v", points[1])
	}
	again, err := List(ctx, s, runID, manifest)
	if err != nil || again[1].ID != points[1].ID {
		t.Fatalf("checkpoint identity changed: %+v, err=%v", again, err)
	}
	if _, err := Select(points, 4); err == nil || !strings.Contains(err.Error(), "supported") {
		t.Fatalf("mid-dispatch event accepted: %v", err)
	}
	selected, err := Select(points, 5)
	if err != nil || selected.ID != points[1].ID {
		t.Fatalf("selected=%+v, err=%v", selected, err)
	}
	after, err := s.Events(ctx, runID)
	if err != nil || len(after) != len(before) {
		t.Fatalf("checkpoint listing changed source events: before=%d after=%d err=%v", len(before), len(after), err)
	}
}

func TestListRejectsMalformedLedgerAndManifest(t *testing.T) {
	s, runID, manifest := checkpointFixture(t)
	ctx := context.Background()
	wrong := manifest
	wrong.Digest = "different"
	if _, err := List(ctx, s, runID, wrong); err == nil {
		t.Fatal("changed manifest accepted")
	}
	if _, err := s.DB.ExecContext(ctx, "UPDATE events SET payload='{' WHERE run_id=? AND seq=3", runID); err != nil {
		t.Fatal(err)
	}
	if _, err := List(ctx, s, runID, manifest); err == nil {
		t.Fatal("malformed event accepted")
	}
}
