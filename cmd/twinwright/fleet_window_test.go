package main

import (
	"testing"

	"twinwright/internal/metrics"
	"twinwright/internal/worker"
)

// The metrics window decides which workers are listed, and an idle worker is
// seen only through its heartbeat. The metrics package cannot import the worker
// package, so this is where the two are kept in proportion: the window must hold
// many heartbeats, or one late poll would drop a healthy idle worker from the
// series.
func TestMetricsWorkerWindowHoldsManyIdleHeartbeats(t *testing.T) {
	if got, want := metrics.DefaultWorkerWindow, 5*worker.IdleHeartbeatEvery; got < want {
		t.Fatalf("metrics.DefaultWorkerWindow=%v is under five idle heartbeats (%v)", got, want)
	}
}
