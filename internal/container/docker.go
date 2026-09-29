package container

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
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

// logTailLines bounds captured container output. Enough to see a bind failure or
// a bad flag; not enough to bury the error that carries it.
const logTailLines = 50

// Docker starts sidecars with the docker CLI.
//
// Start does not return until the container is ready, or until it has been
// cleaned up. A handle for a container that is still starting would hand the
// caller an address nothing is listening on, and a failed start that leaves a
// container behind makes the next start fail on a name collision instead of on
// the real problem.
//
// Secrets must use env_file, never argv: a process list is readable by any user
// on the host.
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

// Start creates the container, waits for it to be ready, and returns its handle.
func (d *Docker) Start(ctx context.Context, cfg Config) (Handle, error) {
	if cfg.Runtime != "docker" {
		return Handle{}, fmt.Errorf("docker executor cannot start runtime %q", cfg.Runtime)
	}
	args := []string{"run", "-d", "--name", cfg.Name, "--label", "twinwright.experimental=true"}
	if cfg.Network != "" {
		args = append(args, "--network", cfg.Network)
	}
	if cfg.Resources.Memory != "" {
		args = append(args, "--memory", cfg.Resources.Memory)
	}
	if cfg.Resources.CPUs != "" {
		args = append(args, "--cpus", cfg.Resources.CPUs)
	}
	if cfg.Resources.PIDs > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(cfg.Resources.PIDs))
	}
	for _, p := range cfg.Publish {
		args = append(args, "-p", p)
	}
	if cfg.EnvFile != "" {
		args = append(args, "--env-file", cfg.EnvFile)
	}
	args = append(args, cfg.Image)

	stdout, stderr, err := d.Run(ctx, "docker", args...)
	if err != nil {
		// A failed `docker run` can still have created the container, so the
		// name is released before returning. Without this the next start fails
		// on a name collision and reports that instead of the real cause.
		_, _, _ = d.Run(ctx, "docker", "rm", "-f", cfg.Name)
		return Handle{}, fmt.Errorf("docker start failed (is the daemon running?): %s", message(stderr, err))
	}
	handle := Handle{Name: cfg.Name, Runtime: "docker", ID: strings.TrimSpace(stdout), Running: true}
	if len(cfg.Publish) > 0 {
		handle.Addr = cfg.Publish[0]
	}

	if err = d.waitReady(ctx, cfg); err != nil {
		logs := d.logTail(ctx, cfg.Name)
		// The container is torn down with the same grace period a normal stop
		// uses, so a failed start leaves the host as it found it.
		_ = d.Stop(ctx, cfg)
		if logs != "" {
			return Handle{}, fmt.Errorf("%w; container logs:\n%s", err, logs)
		}
		return Handle{}, err
	}
	handle.Healthy = true
	return handle, nil
}

// waitReady blocks until the container is ready or the startup budget is spent.
//
// Readiness is the health probe when one is configured. Without a probe the best
// available evidence is that docker still reports the container running, which is
// weaker and is documented as such rather than dressed up as a health check.
func (d *Docker) waitReady(ctx context.Context, cfg Config) error {
	deadline, cancel := context.WithTimeout(ctx, cfg.StartupTimeout)
	defer cancel()

	if len(cfg.Health.Command) == 0 && cfg.Health.HTTPPath == "" {
		running, err := d.running(deadline, cfg.Name)
		if err != nil {
			return err
		}
		if !running {
			return fmt.Errorf("container %s exited during startup", cfg.Name)
		}
		return nil
	}

	probe := d.probeFunc(cfg)
	var lastErr error
	for attempt := 1; attempt <= cfg.Health.Retries; attempt++ {
		// A container that has exited will never become healthy, so the loop
		// stops rather than spending every retry on a process that is gone.
		running, err := d.running(deadline, cfg.Name)
		if err != nil {
			return err
		}
		if !running {
			return fmt.Errorf("container %s exited during startup after %d health probe(s)", cfg.Name, attempt-1)
		}
		if err = probe(deadline); err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-deadline.Done():
			return fmt.Errorf("container %s did not become healthy within %s (timeout): last probe error: %v",
				cfg.Name, cfg.StartupTimeout, lastErr)
		case <-time.After(cfg.Health.Interval):
		}
	}
	return fmt.Errorf("container %s did not become healthy after %d probes: last probe error: %v",
		cfg.Name, cfg.Health.Retries, lastErr)
}

// probeFunc returns the configured readiness probe.
func (d *Docker) probeFunc(cfg Config) func(context.Context) error {
	if cfg.Health.HTTPPath != "" {
		return func(ctx context.Context) error { return d.probeHTTP(ctx, cfg) }
	}
	argv := append([]string{"exec", cfg.Name}, cfg.Health.Command...)
	return func(ctx context.Context) error {
		_, stderr, err := d.Run(ctx, "docker", argv...)
		if err == nil {
			return nil
		}
		// Exit 127 from an exec probe means the image does not contain the
		// command, which no amount of retrying will fix. Saying so here saves
		// the reader from concluding the service is broken.
		detail := message(stderr, err)
		if strings.Contains(detail, "127") {
			detail += " (exit 127 usually means the image does not contain that command; a scratch or distroless image has no shell, so use health.http_path instead)"
		}
		return fmt.Errorf("%s", detail)
	}
}

// probeHTTP requests the published port from the host. Any response below 500
// counts as ready: the question is whether the service is listening and serving,
// not whether this particular path is meaningful to it.
func (d *Docker) probeHTTP(ctx context.Context, cfg Config) error {
	port, err := hostPort(cfg.Publish[0])
	if err != nil {
		return err
	}
	url := "http://127.0.0.1:" + port + cfg.Health.HTTPPath
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := probeClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode >= 500 {
		return fmt.Errorf("%s answered %d", url, response.StatusCode)
	}
	return nil
}

// probeClient is deliberately short-tempered: a probe that hangs would eat the
// startup budget one attempt at a time.
var probeClient = &http.Client{Timeout: 5 * time.Second}

// running reports whether docker still considers the container running. A failed
// inspect during startup means the container is gone, which is an answer rather
// than an error.
func (d *Docker) running(ctx context.Context, name string) (bool, error) {
	stdout, _, err := d.Run(ctx, "docker", "inspect", "-f", "{{.State.Running}}", name)
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(stdout) == "true", nil
}

// logTail captures the container's own output, bounded. An operator told only
// "unhealthy" has to go and find out why; the reason is usually in these lines.
func (d *Docker) logTail(ctx context.Context, name string) string {
	stdout, stderr, err := d.Run(ctx, "docker", "logs", "--tail", strconv.Itoa(logTailLines), name)
	if err != nil && strings.TrimSpace(stdout) == "" && strings.TrimSpace(stderr) == "" {
		return ""
	}
	// A container writing to stderr is the common case for a startup failure, so
	// both streams are kept.
	combined := strings.TrimSpace(strings.TrimSpace(stdout) + "\n" + strings.TrimSpace(stderr))
	return strings.TrimSpace(combined)
}

// Logs returns the recent output of a container.
func (d *Docker) Logs(ctx context.Context, name string) (string, error) {
	stdout, stderr, err := d.Run(ctx, "docker", "logs", "--tail", strconv.Itoa(logTailLines), name)
	if err != nil {
		return "", fmt.Errorf("docker logs failed: %s", message(stderr, err))
	}
	return strings.TrimSpace(strings.TrimSpace(stdout) + "\n" + strings.TrimSpace(stderr)), nil
}

// Stop shuts a container down deterministically: it is asked to exit with a
// grace period and only then removed. `rm -f` sends SIGKILL immediately, which
// gives a service no chance to finish what it was doing.
func (d *Docker) Stop(ctx context.Context, cfg Config) error {
	grace := cfg.StopTimeout
	if grace <= 0 {
		grace = DefaultStopTimeout
	}
	seconds := int(grace.Round(time.Second) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	// A stop failure is not fatal on its own: the container may already be gone,
	// and the removal below is what has to succeed.
	_, stopErr, stopFailed := d.Run(ctx, "docker", "stop", "--time", strconv.Itoa(seconds), cfg.Name)
	_, rmErr, rmFailed := d.Run(ctx, "docker", "rm", cfg.Name)
	if rmFailed != nil {
		if stopFailed != nil {
			return fmt.Errorf("docker stop failed: %s; and remove failed: %s", message(stopErr, stopFailed), message(rmErr, rmFailed))
		}
		return fmt.Errorf("docker remove failed: %s", message(rmErr, rmFailed))
	}
	return nil
}

// Status reports identity, whether the container is running, and its health as
// docker sees it.
func (d *Docker) Status(ctx context.Context, name string) (Handle, error) {
	stdout, stderr, err := d.Run(ctx, "docker", "inspect", "-f",
		"{{.Id}} {{.State.Running}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}", name)
	if err != nil {
		return Handle{}, fmt.Errorf("docker status failed: %s", message(stderr, err))
	}
	fields := strings.Fields(strings.TrimSpace(stdout))
	handle := Handle{Name: name, Runtime: "docker"}
	if len(fields) > 0 {
		handle.ID = fields[0]
	}
	if len(fields) > 1 {
		handle.Running = fields[1] == "true"
	}
	// "none" means the image declares no HEALTHCHECK. A running container with no
	// declared health check is reported healthy, because running is then the only
	// evidence available and claiming otherwise would be inventing a signal.
	if len(fields) > 2 {
		handle.Healthy = fields[2] == "healthy" || (fields[2] == "none" && handle.Running)
	}
	return handle, nil
}

func message(stderr string, err error) string {
	if trimmed := strings.TrimSpace(stderr); trimmed != "" {
		return trimmed
	}
	if err != nil {
		return err.Error()
	}
	return "unknown error"
}
