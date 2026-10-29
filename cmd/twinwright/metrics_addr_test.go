package main

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"twinwright/internal/store"
)

// A worker that was asked for metrics and cannot serve them must not run. The
// endpoint used to start in a goroutine, and a bind failure was one stderr line
// while the worker claimed on: with two workers on one host given the same address,
// Prometheus scraped the other process and this worker's counters were never seen.
func TestWorkerFailsWhenTheMetricsAddressIsTaken(t *testing.T) {
	dir := t.TempDir()
	manifestPath := buildBillingManifest(t, dir)
	db := filepath.Join(dir, "world.db")
	runID := enqueueBillingRun(t, manifestPath, db)

	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	addr := held.Addr().String()

	var out bytes.Buffer
	err = runCLI([]string{"worker", "--manifest", manifestPath, "--db", db, "--id", "metrics-worker",
		"--drain", "--lease-ttl", "60s", "--metrics-addr", addr}, &out)
	if err == nil {
		t.Fatalf("the worker ran although it could not serve its metrics:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), addr) || !strings.Contains(err.Error(), "--metrics-addr") {
		t.Fatalf("err = %q, want it to name the flag and the address %s", err, addr)
	}

	// It stopped before it claimed anything or registered itself.
	ctx := context.Background()
	s, err := store.OpenDSN(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entry, err := s.QueueEntryFor(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.State != store.QueueRunnable || entry.Attempts != 0 {
		t.Fatalf("the queue entry was touched: %+v", entry)
	}
	workers, err := s.Workers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 0 {
		t.Fatalf("the worker registered itself before it failed: %+v", workers)
	}
}

// An address nobody holds is bound once, before the first claim, and the command
// serves on the listener it bound.
func TestWorkerServesMetricsOnTheListenerItBound(t *testing.T) {
	var bound []string
	previous := listenMetrics
	listenMetrics = func(address string) (net.Listener, error) {
		bound = append(bound, address)
		return net.Listen("tcp", "127.0.0.1:0")
	}
	t.Cleanup(func() { listenMetrics = previous })

	dir := t.TempDir()
	manifestPath := buildBillingManifest(t, dir)
	db := filepath.Join(dir, "world.db")
	enqueueBillingRun(t, manifestPath, db)
	var out bytes.Buffer
	if err := runCLI([]string{"worker", "--manifest", manifestPath, "--db", db, "--id", "metrics-worker",
		"--drain", "--lease-ttl", "60s", "--metrics-addr", "127.0.0.1:9"}, &out); err != nil {
		t.Fatalf("worker: %v\n%s", err, out.String())
	}
	if len(bound) != 1 || bound[0] != "127.0.0.1:9" {
		t.Fatalf("the command bound %v, want the flag's address once", bound)
	}
}
