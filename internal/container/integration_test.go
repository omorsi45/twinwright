package container

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Integration tests against a real Docker daemon.
//
// These are opt-in, like the PostgreSQL tests: without TWINWRIGHT_TEST_DOCKER=1
// they skip, so `go test ./...` never reaches for a daemon or pulls an image on a
// contributor's machine. CI sets the variable in a job of its own.
//
// There is deliberately no fake here. The unit tests already cover the argv the
// executor builds; what these prove is the part a fake cannot: that the command
// line docker actually accepts, that the health gate means the published port is
// listening, and that a failed start leaves nothing behind.

const dockerEnv = "TWINWRIGHT_TEST_DOCKER"

func requireDocker(t *testing.T) {
	t.Helper()
	if os.Getenv(dockerEnv) != "1" {
		t.Skipf("%s is not 1; skipping Docker integration test", dockerEnv)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("%s=1 but docker is not on PATH: %v", dockerEnv, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
		t.Fatalf("%s=1 but the daemon does not answer: %v: %s", dockerEnv, err, strings.TrimSpace(string(out)))
	}
}

// uniqueName keeps concurrent runs from colliding on a container name.
func uniqueName(t *testing.T, prefix string) string {
	t.Helper()
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	return prefix + "-" + hex.EncodeToString(raw[:])
}

// The shipped example config is used rather than a config invented here, so this
// test also proves the documented example still works.
func TestDockerStartsTheExampleSidecarAndItIsActuallyListening(t *testing.T) {
	requireDocker(t)
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "container", "http-echo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(raw)
	if err != nil {
		t.Fatalf("the shipped example config does not parse: %v", err)
	}
	cfg.Name = uniqueName(t, "twinwright-it")

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	d := NewDocker(nil)
	// Stop is idempotent enough to be a cleanup: a container already removed is
	// not an error worth failing the test over.
	t.Cleanup(func() { _ = d.Stop(context.Background(), cfg) })

	handle, err := d.Start(ctx, cfg)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if handle.ID == "" || !handle.Running || !handle.Healthy {
		t.Fatalf("handle=%+v", handle)
	}

	// The point of waiting for health is that the address works when Start
	// returns. Without this assertion the health gate is unproven.
	port := cfg.Publish[0][:strings.Index(cfg.Publish[0], ":")]
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Get("http://127.0.0.1:" + port + "/")
	if err != nil {
		t.Fatalf("published port was not serving when Start returned: %v", err)
	}
	defer response.Body.Close()
	if _, err = io.ReadAll(response.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Errorf("status=%d", response.StatusCode)
	}

	status, err := d.Status(ctx, cfg.Name)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !status.Running || status.ID != handle.ID {
		t.Errorf("status=%+v handle=%+v", status, handle)
	}
	if _, err = d.Logs(ctx, cfg.Name); err != nil {
		t.Errorf("logs: %v", err)
	}

	if err = d.Stop(ctx, cfg); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err = d.Status(ctx, cfg.Name); err == nil {
		t.Error("status still resolves a removed container")
	}
}

// A container that exits immediately must fail the start, carry its own output,
// and leave nothing behind for the next start to collide with.
func TestDockerFailedStartLeavesNothingBehind(t *testing.T) {
	requireDocker(t)
	name := uniqueName(t, "twinwright-it-fail")
	cfg, err := Parse([]byte(`
version: 1
name: ` + name + `
runtime: docker
image: busybox:1.36
network: none
health:
  command: ["true"]
  interval: 50ms
  retries: 5
startup_timeout: 20s
stop_timeout: 1s
label: experimental
`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	d := NewDocker(nil)
	t.Cleanup(func() { _ = d.Stop(context.Background(), cfg) })

	// busybox with no command runs `sh`, which exits at once without a TTY.
	if _, err = d.Start(ctx, cfg); err == nil {
		t.Fatal("a container that exits immediately was reported as started")
	}

	// The name must be free: a leftover container would make the next start fail
	// on a collision rather than on the real problem.
	if out, lookupErr := exec.CommandContext(ctx, "docker", "ps", "-a", "--filter", "name="+name, "--format", "{{.Names}}").CombinedOutput(); lookupErr != nil {
		t.Fatalf("docker ps: %v", lookupErr)
	} else if strings.Contains(string(out), name) {
		t.Errorf("failed start left a container behind: %s", strings.TrimSpace(string(out)))
	}
}
