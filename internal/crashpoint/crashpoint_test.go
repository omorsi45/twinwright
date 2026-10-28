package crashpoint

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestFireWithNilHookIsANoOp(t *testing.T) {
	if err := Fire(context.Background(), nil, Site{Point: BeforeCompletion}); err != nil {
		t.Fatalf("nil hook returned %v", err)
	}
}

func TestFireWrapsHookErrorsAsCrashes(t *testing.T) {
	cause := errors.New("kill -9")
	var seen Site
	hook := func(_ context.Context, site Site) error {
		seen = site
		return cause
	}
	want := Site{Point: AfterToolCommit, RunID: "run-1", Step: 3, CallID: "call-1", OperationID: "createRefund"}
	err := Fire(context.Background(), hook, want)
	if !errors.Is(err, ErrCrashed) {
		t.Fatalf("error %v does not wrap ErrCrashed", err)
	}
	if !strings.Contains(err.Error(), string(AfterToolCommit)) {
		t.Fatalf("error %q does not name the point", err)
	}
	if !strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("error %q does not carry the hook's reason", err)
	}
	if seen != want {
		t.Fatalf("hook saw %+v, want %+v", seen, want)
	}
}

func TestFireReturnsNilWhenTheHookDeclines(t *testing.T) {
	hook := func(context.Context, Site) error { return nil }
	if err := Fire(context.Background(), hook, Site{Point: BeforeModelRequest}); err != nil {
		t.Fatalf("declining hook returned %v", err)
	}
}

func TestParsePointRoundTripsEveryPoint(t *testing.T) {
	points := Points()
	if len(points) != 7 {
		t.Fatalf("got %d points, want 7", len(points))
	}
	for _, point := range points {
		got, err := ParsePoint(string(point))
		if err != nil || got != point {
			t.Fatalf("ParsePoint(%q) = %q, %v", point, got, err)
		}
	}
	_, err := ParsePoint("mid-flight")
	if err == nil {
		t.Fatal("unknown point accepted")
	}
	for _, point := range points {
		if !strings.Contains(err.Error(), string(point)) {
			t.Fatalf("error %q does not list %q", err, point)
		}
	}
}

func TestPointsAreInDocumentedOrder(t *testing.T) {
	want := []string{
		"before-model-request", "after-model-request", "after-model-response",
		"before-tool-commit", "after-tool-commit", "before-completion", "after-completion",
	}
	got := Points()
	for i := range want {
		if string(got[i]) != want[i] {
			t.Fatalf("point %d = %q, want %q", i, got[i], want[i])
		}
	}
}
