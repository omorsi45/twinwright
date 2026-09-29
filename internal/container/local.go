package container

import (
	"context"
	"fmt"
	"strings"
)

// Handle identifies a started sidecar.
type Handle struct {
	Name    string `json:"name"`
	Runtime string `json:"runtime"`
	ID      string `json:"id,omitempty"`
	Addr    string `json:"addr,omitempty"`
	// Running is what the runtime reports about the process.
	Running bool `json:"running"`
	// Healthy means readiness was established: a configured health probe
	// succeeded, or the runtime reports the container healthy. For a sidecar with
	// no probe it means only that the container is running, which is weaker
	// evidence and is documented as such.
	Healthy bool `json:"healthy"`
}

// Executor starts and stops optional sidecars.
//
// Stop takes the whole config rather than a name because shutdown is not just a
// name lookup: the grace period a container gets to exit is part of its
// configuration.
type Executor interface {
	Start(ctx context.Context, cfg Config) (Handle, error)
	Stop(ctx context.Context, cfg Config) error
	Status(ctx context.Context, name string) (Handle, error)
}

// Local keeps worlds in-process. There is no external process to track across CLI invocations.
type Local struct{}

// NewLocal returns an in-process executor.
func NewLocal() *Local { return &Local{} }

func (l *Local) Start(_ context.Context, cfg Config) (Handle, error) {
	if cfg.Runtime != "local" {
		return Handle{}, fmt.Errorf("local executor cannot start runtime %q", cfg.Runtime)
	}
	return Handle{Name: cfg.Name, Runtime: "local", ID: "local-" + cfg.Name, Addr: "in-process", Running: true, Healthy: true}, nil
}

func (l *Local) Stop(_ context.Context, cfg Config) error {
	if strings.TrimSpace(cfg.Name) == "" {
		return fmt.Errorf("container name is required")
	}
	return nil
}

func (l *Local) Status(_ context.Context, name string) (Handle, error) {
	if strings.TrimSpace(name) == "" {
		return Handle{}, fmt.Errorf("container name is required")
	}
	return Handle{Name: name, Runtime: "local", ID: "local-" + name, Addr: "in-process", Running: true, Healthy: true}, nil
}

// Select returns the executor for a config runtime.
func Select(runtime string, docker *Docker) (Executor, error) {
	switch runtime {
	case "local":
		return NewLocal(), nil
	case "docker":
		if docker == nil {
			docker = NewDocker(nil)
		}
		return docker, nil
	default:
		return nil, fmt.Errorf("unsupported container runtime %q", runtime)
	}
}
