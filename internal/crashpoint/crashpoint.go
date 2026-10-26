// Package crashpoint names the durable boundaries of a run so a harness can
// simulate process death at each one.
//
// Crash tests that only cut inside a provider call miss the boundaries where
// recovery is hardest: between a request and its response, and on either side
// of a tool transaction commit. Naming them lets a matrix harness crash and
// recover at every one.
package crashpoint

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Point is a durable boundary at which a process may die.
type Point string

const (
	BeforeModelRequest Point = "before-model-request"
	AfterModelRequest  Point = "after-model-request"
	AfterModelResponse Point = "after-model-response"
	BeforeToolCommit   Point = "before-tool-commit"
	AfterToolCommit    Point = "after-tool-commit"
	BeforeCompletion   Point = "before-completion"
	// AfterCompletion is never fired by code. Nothing runs after the completed
	// status is written, so a harness simulates it by skipping the work that
	// would follow, such as FinishRun.
	AfterCompletion Point = "after-completion"
)

var points = []Point{
	BeforeModelRequest, AfterModelRequest, AfterModelResponse,
	BeforeToolCommit, AfterToolCommit, BeforeCompletion, AfterCompletion,
}

// Points returns every crash point in execution order.
func Points() []Point {
	return append([]Point(nil), points...)
}

// ParsePoint maps a name back to its Point. An unknown name is an error that
// lists the valid ones, so a mistyped matrix entry fails loudly instead of
// silently never firing.
func ParsePoint(s string) (Point, error) {
	for _, point := range points {
		if string(point) == s {
			return point, nil
		}
	}
	names := make([]string, len(points))
	for i, point := range points {
		names[i] = string(point)
	}
	return "", fmt.Errorf("unknown crash point %q; valid points: %s", s, strings.Join(names, ", "))
}

// Site identifies where in a run a point was reached. CallID and OperationID
// are set only for the tool points.
type Site struct {
	Point               Point
	RunID               string
	Step                int
	CallID, OperationID string
}

// Hook decides whether the process dies at a site. Returning an error means it
// does.
type Hook func(ctx context.Context, site Site) error

// ErrCrashed marks an error as a simulated process death rather than a failure
// the run should record.
var ErrCrashed = errors.New("simulated process death")

// Fire offers site to hook. A nil hook never crashes. When the hook returns an
// error, Fire wraps it with ErrCrashed and the caller must return it at once
// and write nothing more: a dead process records no error event and no failed
// status, and recovery has to cope with exactly the state left behind.
func Fire(ctx context.Context, hook Hook, site Site) error {
	if hook == nil {
		return nil
	}
	if err := hook(ctx, site); err != nil {
		return fmt.Errorf("%w at %s: %v", ErrCrashed, site.Point, err)
	}
	return nil
}
