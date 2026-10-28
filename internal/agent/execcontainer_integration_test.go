package agent

import (
	"context"
	"errors"
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
		provider := ExecProvider{Command: []string{"python3", "/agent/billing_agent.py"}, Timeout: 2 * time.Minute, Wrap: opts.Wrap, Cleanup: opts.Cleanup, ClientEnv: DockerClientEnv}
		run, report := completeBillingRun(t, provider)
		if run.Status != "completed" || !report.Passed {
			t.Fatalf("run = %s, evaluation = %+v", run.Status, report)
		}
	})

	t.Run("network none leaves the agent without a network", func(t *testing.T) {
		message := runProbe(t, "none")
		t.Logf("the agent answered: %s", message.Content)
		// "blocked" alone would also be a firewall that drops packets, which times
		// out or refuses. With --network none there is no route at all, and the
		// kernel says so.
		if !strings.HasPrefix(message.Content, "blocked") || !strings.Contains(message.Content, "Errno 101") {
			t.Fatalf("with --network none the agent reported %q, want it blocked with errno 101 (network unreachable)", message.Content)
		}
	})

	t.Run("the probe does connect on a bridge network", func(t *testing.T) {
		// The control that gives the test above its meaning: the same probe, on a
		// network that has a route out, must connect. A probe that fails everywhere
		// would pass the isolation test without the isolation having done anything.
		message := runProbe(t, "bridge")
		t.Logf("the agent answered: %s", message.Content)
		if !strings.HasPrefix(message.Content, "connected") {
			t.Skipf("the bridge network has no outbound connectivity on this runner (%s), so the isolation test above cannot be compared with a probe that connects", message.Content)
		}
	})
}

// runProbe runs networkProbe as the agent in the hardened container on the given
// network and returns its answer.
func runProbe(t *testing.T, network string) Message {
	t.Helper()
	dir := t.TempDir()
	// The container runs as user 65534, which must be able to read what is
	// mounted; a TempDir is private to its owner by default.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "probe.py"), []byte(networkProbe), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := ContainerOptions{Image: pythonTestImage, Network: network, Memory: "512m", CPUs: "1", PIDs: 128, Mount: dir}
	if err := opts.Validate(); err != nil {
		t.Fatal(err)
	}
	provider := ExecProvider{Command: []string{"python3", "/agent/probe.py"}, Timeout: 2 * time.Minute, Wrap: opts.Wrap, Cleanup: opts.Cleanup, ClientEnv: DockerClientEnv}
	message, err := provider.Next(context.Background(), "probe", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

// dockerContainers lists the containers, running or not, whose name contains name.
func dockerContainers(t *testing.T, name string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "-a", "--filter", "name="+name, "--format", "{{.Names}} {{.State}}").CombinedOutput()
	if err != nil {
		t.Fatalf("docker ps: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// hangingAgent and floodingAgent are agents that read their request and then
// never answer: one sleeps, the other first writes more than the stdout limit.
const (
	hangingAgent = `import sys, time

sys.stdin.read()
time.sleep(600)
`
	floodingAgent = `import sys, time

sys.stdin.read()
chunk = "x" * 65536
for _ in range(64):
    sys.stdout.write(chunk)
sys.stdout.flush()
time.sleep(600)
`
)

// Killing the docker client does not stop the container, and a Python agent that
// is PID 1 ignores the SIGTERM the client proxies. A turn that is cut short, by
// its deadline or by an output flood, must not leave its container running.
func TestHardenedContainerIsGoneAfterOverflowAndTimeout(t *testing.T) {
	requireDockerImage(t, pythonTestImage)

	cases := []struct {
		name, script string
		timeout      time.Duration
		want         func(error) bool
	}{
		{"timeout", hangingAgent, 6 * time.Second, func(err error) bool {
			var interrupt *CallInterrupted
			return errors.As(err, &interrupt) && interrupt.Kind == InterruptTimeout
		}},
		{"stdout overflow", floodingAgent, 2 * time.Minute, func(err error) bool {
			return err != nil && strings.Contains(err.Error(), "exceeded")
		}},
	}
	for _, tc := range cases {
		// run starts one turn of the agent, with or without the removal, and
		// returns the name of its container.
		run := func(t *testing.T, cleanup bool) string {
			t.Helper()
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "agent.py"), []byte(tc.script), 0o644); err != nil {
				t.Fatal(err)
			}
			opts := ContainerOptions{Image: pythonTestImage, Network: "none", Memory: "512m", CPUs: "1", PIDs: 128, Mount: dir}
			if err := opts.Validate(); err != nil {
				t.Fatal(err)
			}
			var name string
			provider := ExecProvider{
				Command: []string{"python3", "/agent/agent.py"}, Timeout: tc.timeout, ClientEnv: DockerClientEnv,
				Wrap: func(argv []string) []string {
					wrapped := opts.Wrap(argv)
					name, _ = containerName(wrapped)
					return wrapped
				},
			}
			if cleanup {
				provider.Cleanup = opts.Cleanup
			}
			_, err := provider.Next(context.Background(), "task", nil, nil)
			if !tc.want(err) {
				t.Fatalf("err = %v", err)
			}
			if name == "" {
				t.Fatal("the container was not given a name")
			}
			return name
		}

		t.Run(tc.name+": the container is removed", func(t *testing.T) {
			name := run(t, true)
			if got := dockerContainers(t, name); got != "" {
				exec.Command("docker", "rm", "-f", name).Run()
				t.Fatalf("the container outlived the turn: %q", got)
			}
		})

		// The control that gives the test above its meaning, and the behaviour the
		// removal exists for: without it the container is still running after its
		// client was killed. It is removed here, by name, because nothing else will.
		t.Run(tc.name+": without the removal the container outlives its client", func(t *testing.T) {
			name := run(t, false)
			defer exec.Command("docker", "rm", "-f", name).Run()
			if got := dockerContainers(t, name); !strings.Contains(got, "running") {
				t.Fatalf("containers named %s after the client was killed: %q, want it running", name, got)
			}
		})
	}
}
