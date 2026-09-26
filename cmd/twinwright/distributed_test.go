package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildBillingManifest compiles the billing example into a temp manifest the
// distributed commands can use.
func buildBillingManifest(t *testing.T, dir string) string {
	t.Helper()
	manifestPath := filepath.Join(dir, "twinwright.manifest.json")
	var out bytes.Buffer
	err := runCLI([]string{
		"build", filepath.Join("..", "..", "examples", "billing", "openapi.yaml"),
		"--bindings", filepath.Join("..", "..", "examples", "billing", "bindings.yaml"),
		"--out", manifestPath,
	}, &out)
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out.String())
	}
	return manifestPath
}

// TestDistributedCLIEnqueueAndWorkerDrain exercises the distributed path the way
// an operator would: create a run without executing it, then let a worker claim
// and finish it, then read the queue back.
func TestDistributedCLIEnqueueAndWorkerDrain(t *testing.T) {
	dir := t.TempDir()
	manifestPath := buildBillingManifest(t, dir)
	db := filepath.Join(dir, "world.db")

	var enqueued bytes.Buffer
	if err := runCLI([]string{
		"run", "duplicate-charge", "--manifest", manifestPath, "--db", db,
		"--agent", "scripted", "--enqueue", "--steps", "10",
	}, &enqueued); err != nil {
		t.Fatalf("run --enqueue: %v\n%s", err, enqueued.String())
	}
	var created struct {
		RunID    string `json:"run_id"`
		State    string `json:"state"`
		Delivery string `json:"delivery"`
	}
	if err := json.Unmarshal(enqueued.Bytes(), &created); err != nil {
		t.Fatalf("decode enqueue output: %v\n%s", err, enqueued.String())
	}
	if created.RunID == "" || created.State != "runnable" {
		t.Fatalf("unexpected enqueue result: %+v", created)
	}
	// The CLI must not advertise a delivery guarantee it does not provide.
	if created.Delivery != "at_least_once" {
		t.Fatalf("delivery=%q", created.Delivery)
	}

	// Enqueueing must not have executed anything yet.
	var beforeQueue bytes.Buffer
	if err := runCLI([]string{"queue", "--db", db}, &beforeQueue); err != nil {
		t.Fatalf("queue: %v\n%s", err, beforeQueue.String())
	}
	if !strings.Contains(beforeQueue.String(), `"runnable": 1`) {
		t.Fatalf("queue does not show the pending run:\n%s", beforeQueue.String())
	}

	var served bytes.Buffer
	if err := runCLI([]string{
		"worker", "--manifest", manifestPath, "--db", db,
		"--id", "cli-worker", "--drain", "--lease-ttl", "60s",
	}, &served); err != nil {
		t.Fatalf("worker --drain: %v\n%s", err, served.String())
	}
	var result struct {
		Worker   string `json:"worker"`
		Claims   int    `json:"claims"`
		Outcomes []struct {
			RunID       string `json:"run_id"`
			Fence       int64  `json:"fence"`
			Disposition string `json:"disposition"`
			RunStatus   string `json:"run_status"`
		} `json:"outcomes"`
	}
	if err := json.Unmarshal(served.Bytes(), &result); err != nil {
		t.Fatalf("decode worker output: %v\n%s", err, served.String())
	}
	if result.Worker != "cli-worker" || result.Claims != 1 {
		t.Fatalf("unexpected worker result: %+v", result)
	}
	outcome := result.Outcomes[0]
	if outcome.RunID != created.RunID || outcome.Disposition != "finished" || outcome.RunStatus != "completed" {
		t.Fatalf("unexpected outcome: %+v", outcome)
	}
	if outcome.Fence < 1 {
		t.Fatalf("worker ran without a fence: %+v", outcome)
	}

	// Ownership history must show the claim and the release.
	var perRun bytes.Buffer
	if err := runCLI([]string{"queue", "--db", db, "--run", created.RunID}, &perRun); err != nil {
		t.Fatalf("queue --run: %v\n%s", err, perRun.String())
	}
	text := perRun.String()
	for _, want := range []string{`"state": "finished"`, "lease.acquired", "run.finished", `"owner": "cli-worker"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("queue --run output is missing %q:\n%s", want, text)
		}
	}

	// The distributed run must still replay and evaluate like any other run.
	var replayed bytes.Buffer
	if err := runCLI([]string{"replay", created.RunID, "--manifest", manifestPath, "--db", db}, &replayed); err != nil {
		t.Fatalf("replay: %v\n%s", err, replayed.String())
	}
	if !strings.Contains(replayed.String(), `"verified":true`) {
		t.Fatalf("distributed run did not replay:\n%s", replayed.String())
	}

	// A second drain must find nothing rather than re-delivering the run.
	var again bytes.Buffer
	if err := runCLI([]string{
		"worker", "--manifest", manifestPath, "--db", db, "--id", "cli-worker", "--drain",
	}, &again); err != nil {
		t.Fatalf("second drain: %v\n%s", err, again.String())
	}
	if !strings.Contains(again.String(), `"claims": 0`) {
		t.Fatalf("second drain re-delivered work:\n%s", again.String())
	}
}

func TestEnqueueRejectsUnknownRun(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "world.db")
	var out bytes.Buffer
	// Create the schema first so the failure is about the run, not the file.
	if err := runCLI([]string{"queue", "--db", db}, &out); err != nil {
		t.Fatalf("queue: %v", err)
	}
	out.Reset()
	if err := runCLI([]string{"enqueue", "R-does-not-exist", "--db", db}, &out); err == nil {
		t.Fatal("expected enqueueing an unknown run to fail")
	}
}

func TestWorkerRejectsAMissingManifest(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	err := runCLI([]string{"worker", "--manifest", filepath.Join(dir, "absent.json"), "--db", filepath.Join(dir, "w.db"), "--drain"}, &out)
	if err == nil {
		t.Fatal("expected a missing manifest to fail before any database work")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "w.db")); statErr == nil {
		t.Fatal("a failed preflight created a database")
	}
}
