package container

import (
	"context"
	"fmt"
)

// Handle identifies a started sidecar.
type Handle struct {
	Name    string `json:"name"`
	Runtime string `json:"runtime"`
	ID      string `json:"id,omitempty"`
	Addr    string `json:"addr,omitempty"`
}

// Executor starts and stops optional sidecars.
type Executor interface {
	Start(ctx context.Context, cfg Config) (Handle, error)
	Stop(ctx context.Context, name string) error
	Status(ctx context.Context, name string) (Handle, error)
}

// Local keeps worlds in-process. Start records the name only.
type Local struct {
	running map[string]Handle
}

// NewLocal returns an in-process executor.
func NewLocal() *Local {
	return &Local{running: map[string]Handle{}}
}

func (l *Local) Start(_ context.Context, cfg Config) (Handle, error) {
	if cfg.Runtime != "local" {
		return Handle{}, fmt.Errorf("local executor cannot start runtime %q", cfg.Runtime)
	}
	if l.running == nil {
		l.running = map[string]Handle{}
	}
	h := Handle{Name: cfg.Name, Runtime: "local", ID: "local-" + cfg.Name, Addr: "in-process"}
	l.running[cfg.Name] = h
	return h, nil
}

func (l *Local) Stop(_ context.Context, name string) error {
	if _, ok := l.running[name]; !ok {
		return fmt.Errorf("container %q is not running", name)
	}
	delete(l.running, name)
	return nil
}

func (l *Local) Status(_ context.Context, name string) (Handle, error) {
	h, ok := l.running[name]
	if !ok {
		return Handle{}, fmt.Errorf("container %q is not running", name)
	}
	return h, nil
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
