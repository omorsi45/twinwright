//go:build unix

package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDocker is a docker client for the tests that need to see what the provider
// asks docker to do, without a daemon. It is a script on PATH that records, in
// its own directory, the name it was told to give a container (run.name), its
// environment (run.env) and every removal it was asked for (rm.log). What `run`
// does comes from a "mode:" word among its arguments, which is the agent command:
// ok answers, hang never answers, flood writes far more than the stdout limit.
// A file named rm.fail makes `rm` fail.
func fakeDocker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
dir=$(dirname "$0")
case "$1" in
run)
  prev=""
  mode=""
  for a in "$@"; do
    if [ "$prev" = "--name" ]; then echo "$a" > "$dir/run.name"; fi
    case "$a" in mode:*) mode=${a#mode:};; esac
    prev=$a
  done
  env > "$dir/run.env"
  case "$mode" in
    ok) printf '%s' '` + okResponse + `' ;;
    hang) sleep 60 ;;
    flood) i=0; while [ $i -lt 2200 ]; do printf '%01000d' 0; i=$((i+1)); done; sleep 60 ;;
  esac
  ;;
rm)
  echo "$@" >> "$dir/rm.log"
  if [ -f "$dir/rm.fail" ]; then echo "Error response from daemon: could not remove" >&2; exit 1; fi
  ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// containerProvider is an agent behind the fake docker, in the given mode.
func containerProvider(mode string, timeout time.Duration) (ExecProvider, ContainerOptions) {
	opts := sampleContainer("")
	return ExecProvider{
		Command: []string{"mode:" + mode}, Timeout: timeout,
		Wrap: opts.Wrap, Cleanup: opts.Cleanup, ClientEnv: DockerClientEnv,
	}, opts
}

func readTrimmed(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(raw))
}

// Docker does not stop a container when its client dies, so killing the client
// on a timeout, an output overflow or a cancellation leaves the agent running
// unless the container is removed too, by the name this turn gave it.
func TestContainerIsRemovedWhenTheTurnIsCutShort(t *testing.T) {
	cases := []struct {
		name, mode string
		timeout    time.Duration
		cancel     bool
		check      func(t *testing.T, err error)
	}{
		{name: "timeout", mode: "hang", timeout: 500 * time.Millisecond, check: func(t *testing.T, err error) {
			var interrupt *CallInterrupted
			if !errors.As(err, &interrupt) || interrupt.Kind != InterruptTimeout {
				t.Fatalf("err = %v, want a timeout interruption", err)
			}
		}},
		{name: "stdout overflow", mode: "flood", timeout: 30 * time.Second, check: func(t *testing.T, err error) {
			if err == nil || !strings.Contains(err.Error(), "exceeded") {
				t.Fatalf("err = %v, want the output limit", err)
			}
		}},
		{name: "cancellation", mode: "hang", timeout: 30 * time.Second, cancel: true, check: func(t *testing.T, err error) {
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want the cancellation", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := fakeDocker(t)
			provider, _ := containerProvider(tc.mode, tc.timeout)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				go func() {
					for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
						if _, err := os.Stat(filepath.Join(dir, "run.name")); err == nil {
							break
						}
					}
					cancel()
				}()
			}
			_, err := provider.Next(ctx, "task", nil, nil)
			tc.check(t, err)

			name := readTrimmed(t, filepath.Join(dir, "run.name"))
			if !containerNamePattern.MatchString(name) {
				t.Fatalf("the container was named %q", name)
			}
			if got := readTrimmed(t, filepath.Join(dir, "rm.log")); got != "rm -f "+name {
				t.Fatalf("docker was asked %q, want %q: the container is left running", got, "rm -f "+name)
			}
		})
	}
}

// A turn that ends by itself leaves nothing to remove: --rm does it, and an
// extra docker call per turn would be noise.
func TestContainerIsLeftAloneAfterANormalTurn(t *testing.T) {
	dir := fakeDocker(t)
	provider, _ := containerProvider("ok", 10*time.Second)
	if _, err := provider.Next(context.Background(), "task", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "rm.log")); !os.IsNotExist(err) {
		t.Fatalf("docker rm was called after a turn that ended by itself: %v", err)
	}
}

// Removal is best effort: a docker that cannot do it is logged, and the turn's
// own error is still the one returned.
func TestContainerRemovalFailureIsLoggedNotReturned(t *testing.T) {
	dir := fakeDocker(t)
	if err := os.WriteFile(filepath.Join(dir, "rm.fail"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	provider, opts := containerProvider("hang", 500*time.Millisecond)
	opts.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	provider.Cleanup = opts.Cleanup
	_, err := provider.Next(context.Background(), "task", nil, nil)
	var interrupt *CallInterrupted
	if !errors.As(err, &interrupt) || interrupt.Kind != InterruptTimeout {
		t.Fatalf("err = %v, want the timeout, not the failed removal", err)
	}
	name := readTrimmed(t, filepath.Join(dir, "run.name"))
	for _, want := range []string{"could not remove", name} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log %q does not mention %q", logs.String(), want)
		}
	}
}

// The docker client finds its daemon through its own environment, so it gets the
// connection variables from this process; the container, which is untrusted, gets
// none of them, because Wrap passes it only the names it is given.
func TestExecContainerClientGetsTheDockerConnectionVariables(t *testing.T) {
	dir := fakeDocker(t)
	want := map[string]string{
		"DOCKER_HOST": "tcp://docker.example.test:2376", "DOCKER_CONTEXT": "remote-context", "DOCKER_CONFIG": "/home/operator/docker-config",
		"DOCKER_CERT_PATH": "/home/operator/certs", "DOCKER_TLS_VERIFY": "1",
	}
	for name, value := range want {
		t.Setenv(name, value)
	}
	provider, _ := containerProvider("ok", 10*time.Second)
	message, err := provider.Next(context.Background(), "task", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := readTrimmed(t, filepath.Join(dir, "run.env"))
	for name, value := range want {
		if !strings.Contains("\n"+env+"\n", "\n"+name+"="+value+"\n") {
			t.Errorf("the docker client's environment lacks %s=%s:\n%s", name, value, env)
		}
	}
	for _, arg := range provider.Wrap([]string{"agent"}) {
		for name, value := range want {
			// A value this short ("1") is inside other arguments by chance, so
			// only the names are looked for in that case.
			if strings.Contains(arg, name) || (len(value) >= 8 && strings.Contains(arg, value)) {
				t.Errorf("the docker command line carries %s: %q", name, arg)
			}
		}
	}
	if message.Content != "done" {
		t.Errorf("message = %+v", message)
	}
}

// The agent run directly is not a docker client and gets none of them.
func TestExecAgentDoesNotGetTheDockerConnectionVariables(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://docker.example.test:2376")
	t.Setenv("DOCKER_CONFIG", "/home/operator/docker-config")
	message, err := helperProvider(t, "environ").Next(context.Background(), "task", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(message.Content, "DOCKER") {
		t.Fatalf("the agent's environment names: %s", message.Content)
	}
}
