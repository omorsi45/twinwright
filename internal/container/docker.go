package container

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner executes a command. Tests inject a fake; production uses exec.
type Runner func(ctx context.Context, name string, args ...string) (stdout string, stderr string, err error)

func defaultRunner(ctx context.Context, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// Docker starts sidecars with the docker CLI. Secrets must use env_file, never argv.
type Docker struct {
	Run Runner
}

// NewDocker builds a Docker executor. A nil runner uses the system docker binary.
func NewDocker(run Runner) *Docker {
	if run == nil {
		run = defaultRunner
	}
	return &Docker{Run: run}
}

func (d *Docker) Start(ctx context.Context, cfg Config) (Handle, error) {
	if cfg.Runtime != "docker" {
		return Handle{}, fmt.Errorf("docker executor cannot start runtime %q", cfg.Runtime)
	}
	args := []string{"run", "-d", "--name", cfg.Name, "--label", "twinwright.experimental=true"}
	for _, p := range cfg.Publish {
		args = append(args, "-p", p)
	}
	if cfg.EnvFile != "" {
		args = append(args, "--env-file", cfg.EnvFile)
	}
	args = append(args, cfg.Image)
	stdout, stderr, err := d.Run(ctx, "docker", args...)
	if err != nil {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = err.Error()
		}
		return Handle{}, fmt.Errorf("docker start failed (is the daemon running?): %s", msg)
	}
	id := strings.TrimSpace(stdout)
	addr := ""
	if len(cfg.Publish) > 0 {
		addr = cfg.Publish[0]
	}
	return Handle{Name: cfg.Name, Runtime: "docker", ID: id, Addr: addr}, nil
}

func (d *Docker) Stop(ctx context.Context, name string) error {
	_, stderr, err := d.Run(ctx, "docker", "rm", "-f", name)
	if err != nil {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("docker stop failed: %s", msg)
	}
	return nil
}

func (d *Docker) Status(ctx context.Context, name string) (Handle, error) {
	stdout, stderr, err := d.Run(ctx, "docker", "inspect", "-f", "{{.Id}}", name)
	if err != nil {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = err.Error()
		}
		return Handle{}, fmt.Errorf("docker status failed: %s", msg)
	}
	return Handle{Name: name, Runtime: "docker", ID: strings.TrimSpace(stdout)}, nil
}
