package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"twinwright/internal/agent/exectest"
	"twinwright/internal/store"
)

// TestWorkerStopDuringADrainIsACleanStop: Ctrl-C or SIGTERM while a bounded worker
// (--drain or --max-runs) is executing a run used to end the command with
// "requeue run: context canceled" and no summary, leave the lease held until its
// TTL, and count the cut-short delivery. It now exits normally with a summary that
// says it stopped, the run goes back to the queue at once, and the delivery is not
// counted.
//
// The stop is delivered by ending the context the command's signal handler is
// derived from, which is what a signal does to it; delivering a real signal to
// the test process is not attempted.
func TestWorkerStopDuringADrainIsACleanStop(t *testing.T) {
	dir := t.TempDir()
	manifest := buildBillingManifest(t, dir)
	db := filepath.Join(dir, "world.db")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(dir, "agent.pid")
	// The helper agent hangs until it is killed, so the claim is in flight until
	// the worker stops it.
	t.Setenv(exectest.Mode, "hang")
	t.Setenv(exectest.PIDFile, pidFile)
	command := `"` + exe + `"`
	agentFlags := []string{"--agent-command", command, "--agent-env", exectest.Mode, "--agent-env", exectest.PIDFile}

	runID := queueRun(t, manifest, db, append([]string{"--agent", "exec", "--model", "helper@test"}, agentFlags...)...)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		args := append([]string{"worker", "--manifest", manifest, "--db", db, "--id", "stop-worker", "--drain", "--lease-ttl", "60s", "--max-attempts", "1"}, agentFlags...)
		done <- workerCommand(ctx, args, &out)
	}()

	// The agent writes its pid file when it starts, which is when the run is in
	// flight under a lease.
	deadline := time.After(30 * time.Second)
	for {
		if _, statErr := os.Stat(pidFile); statErr == nil {
			break
		}
		select {
		case err = <-done:
			t.Fatalf("the worker ended before the run was in flight: %v\n%s", err, out.String())
		case <-deadline:
			t.Fatal("the agent never started")
		case <-time.After(10 * time.Millisecond):
		}
	}
	stop()

	select {
	case err = <-done:
		if err != nil {
			t.Fatalf("a stop ended the command with an error: %v\n%s", err, out.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the worker did not stop")
	}
	var summary struct {
		Stopped  bool `json:"stopped"`
		Claims   int  `json:"claims"`
		Outcomes []struct {
			RunID       string `json:"run_id"`
			Disposition string `json:"disposition"`
		} `json:"outcomes"`
	}
	if err = json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatalf("the stop produced no summary: %v\n%s", err, out.String())
	}
	if !summary.Stopped || summary.Claims != 1 || len(summary.Outcomes) != 1 || summary.Outcomes[0].RunID != runID || summary.Outcomes[0].Disposition != "requeued" {
		t.Fatalf("summary=%+v, want one requeued claim and stopped=true\n%s", summary, out.String())
	}

	// With --max-attempts 1 a counted delivery would fail the run at the next
	// claim. The queue entry shows the run waiting, with the delivery given back.
	s, err := store.OpenReadOnly(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entry, err := s.QueueEntryFor(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.State != store.QueueRunnable || entry.Attempts != 0 {
		t.Fatalf("queue entry=%+v, want runnable with no delivery counted", entry)
	}
	lease, err := s.RunLease(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Owner != "" {
		t.Fatalf("the stopped worker still holds the lease: %+v", lease)
	}
}
