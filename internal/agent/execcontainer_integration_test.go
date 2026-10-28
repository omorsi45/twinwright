package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Integration tests of container mode against a real Docker daemon.
//
// Opt-in, like the sidecar tests in internal/container: without
// TWINWRIGHT_TEST_DOCKER=1 they skip, so `go test ./...` never reaches for a
// daemon. They also skip, with the command that fixes it, when the image is not
// on the machine, so a missing pull is not reported as a failure of the code.
// What they prove is the part a recorded argv cannot: that the daemon accepts
// the hardened command line, that the reference agent still does the job inside
// it, and that --network none really leaves the agent without a network.

const (
	dockerTestEnv   = "TWINWRIGHT_TEST_DOCKER"
	pythonTestImage = "python:3.12-alpine"
)

func requireDockerImage(t *testing.T, image string) {
	t.Helper()
	if os.Getenv(dockerTestEnv) != "1" {
		t.Skipf("%s is not 1; skipping Docker integration test", dockerTestEnv)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("%s=1 but docker is not on PATH: %v", dockerTestEnv, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "image", "inspect", image).CombinedOutput(); err != nil {
		t.Skipf("image %s is not available locally (run `docker pull %s`): %v: %s", image, image, err, strings.TrimSpace(string(out)))
	}
}

// networkProbe is an agent that tries to open an outbound connection and says
// whether it could. It answers with the result as its final text, so the test
// reads what happened from inside the container.
const networkProbe = `import json, socket, sys

json.load(sys.stdin)
try:
    socket.create_connection(("1.1.1.1", 53), timeout=3).close()
    answer = "connected"
except OSError as error:
    answer = "blocked: %s" % error
print(json.dumps({"protocol": "twinwright.agent.v1", "content": answer, "tool_calls": []}))
`

func TestPythonAgentInHardenedContainer(t *testing.T) {
	requireDockerImage(t, pythonTestImage)

	t.Run("the reference agent completes the billing run", func(t *testing.T) {
		agentDir, err := filepath.Abs(filepath.Join("..", "..", "examples", "agents", "python"))
		if err != nil {
			t.Fatal(err)
		}
		opts := ContainerOptions{Image: pythonTestImage, Network: "none", Memory: "512m", CPUs: "1", PIDs: 128, Mount: agentDir}
		if err := opts.Validate(); err != nil {
			t.Fatal(err)
		}
		provider := ExecProvider{Command: []string{"python3", "/agent/billing_agent.py"}, Timeout: 2 * time.Minute, Wrap: opts.Wrap}
		run, report := completeBillingRun(t, provider)
		if run.Status != "completed" || !report.Passed {
			t.Fatalf("run = %s, evaluation = %+v", run.Status, report)
		}
	})

	t.Run("network none leaves the agent without a network", func(t *testing.T) {
		dir := t.TempDir()
		// The container runs as user 65534, which must be able to read what is
		// mounted; a TempDir is private to its owner by default.
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		script := filepath.Join(dir, "probe.py")
		if err := os.WriteFile(script, []byte(networkProbe), 0o644); err != nil {
			t.Fatal(err)
		}
		opts := ContainerOptions{Image: pythonTestImage, Network: "none", Memory: "512m", CPUs: "1", PIDs: 128, Mount: dir}
		if err := opts.Validate(); err != nil {
			t.Fatal(err)
		}
		provider := ExecProvider{Command: []string{"python3", "/agent/probe.py"}, Timeout: 2 * time.Minute, Wrap: opts.Wrap}
		message, err := provider.Next(context.Background(), "probe", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("the agent answered: %s", message.Content)
		if !strings.HasPrefix(message.Content, "blocked") {
			t.Fatalf("with --network none the agent reported %q, want it blocked", message.Content)
		}
	})
}
