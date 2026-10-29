//go:build unix

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"twinwright/internal/agent/exectest"
)

// processAlive reports whether a process with this ID still exists. A zombie
// that nobody has reaped still answers signal 0, so a false result also proves
// the provider reaped what it started.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// shAgent is an agent that is a shell script, for the tests that need a process
// the helper binary cannot be: one with no environment at all, or that starts
// processes of its own.
func shAgent(script string, env ...string) ExecProvider {
	return ExecProvider{Command: []string{"/bin/sh", "-c", script}, Env: env, Timeout: 10 * time.Second}
}

// okResponse is a final answer, for scripts to print.
const okResponse = `{"protocol":"twinwright.agent.v1","content":"done","tool_calls":[]}`

// killOnCleanup ends the process whose pid is in file when the test is over, so
// a test that finds a leak does not leave it running.
func killOnCleanup(t *testing.T, file string) {
	t.Helper()
	t.Cleanup(func() {
		if raw, err := os.ReadFile(file); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
}

// The case the stdin goroutine exists for: an agent that never reads its input
// and never exits, sent far more than a pipe buffer holds. The agent that exits
// at once (TestExecProviderDoesNotBlockOnAnAgentThatNeverReads) does not test it,
// because its exit closes the pipe and a synchronous write fails instead of
// blocking. Written synchronously, the request would block the turn on a full
// pipe before the deadline is even watched, and a hung agent would stall a worker
// for ever; so the turn must end within its deadline and the agent must be gone.
func TestExecProviderTimesOutAnAgentThatNeverReadsALargeRequest(t *testing.T) {
	const deadline, grace = time.Second, 5 * time.Second
	pidFile := filepath.Join(t.TempDir(), "pid")
	killOnCleanup(t, pidFile)
	p := helperProvider(t, "hang", exectest.PIDFile+"="+pidFile)
	p.Timeout = deadline

	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		_, err := p.Next(context.Background(), strings.Repeat("a", 4<<20), nil, nil)
		done <- result{err, time.Since(start)}
	}()
	select {
	case got := <-done:
		var interrupt *CallInterrupted
		if !errors.As(got.err, &interrupt) || interrupt.Kind != InterruptTimeout {
			t.Fatalf("err = %v, want a timeout interruption", got.err)
		}
		if got.elapsed > deadline+grace {
			t.Fatalf("took %v, want about the %v deadline", got.elapsed, deadline)
		}
	case <-time.After(deadline + grace):
		t.Fatalf("the turn was still blocked %v after its %v deadline: the request is not written without blocking the turn", grace, deadline)
	}
	requireGone(t, pidFile, 3*time.Second)
}

// A turn that ends normally still ends whatever the agent left running in its
// process group: nothing the agent started survives the turn.
func TestExecProviderKillsTheGroupAfterACleanExit(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	killOnCleanup(t, pidFile)
	// The descendant lets go of every pipe, so the agent's exit is the whole turn.
	script := `sleep 60 </dev/null >/dev/null 2>&1 & echo $! > "$PIDF"; printf '%s' '` + okResponse + `'`
	message, err := shAgent(script, "PIDF="+pidFile).Next(context.Background(), "task", nil, nil)
	if err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	if message.Content != "done" {
		t.Fatalf("message = %+v", message)
	}
	requireGone(t, pidFile, 3*time.Second)
}

// A descendant that keeps stdout open makes the turn fail once Wait gives up on
// the pipe, and it is ended too: a failed turn leaves nothing behind either.
func TestExecProviderKillsTheGroupOnWaitDelay(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	killOnCleanup(t, pidFile)
	script := `sleep 60 & echo $! > "$PIDF"; printf '%s' '` + okResponse + `'`
	_, err := shAgent(script, "PIDF="+pidFile).Next(context.Background(), "task", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "kept its output open") {
		t.Fatalf("err = %v, want the output-held failure", err)
	}
	requireGone(t, pidFile, 3*time.Second)
}

// With neither PATH nor HOME to pass and nothing named, the environment is empty,
// and os/exec reads a nil Env as "inherit everything". The child must still get
// nothing.
func TestExecChildEnvironmentStaysEmptyWithoutPathAndHome(t *testing.T) {
	t.Setenv("TW_SENTINEL_PROVIDER_KEY", "parent-only-provider-key-value")
	t.Setenv("PATH", "")
	t.Setenv("HOME", "")
	os.Unsetenv("PATH")
	os.Unsetenv("HOME")

	env, _, err := ExecProvider{Command: []string{"/bin/sh"}}.childEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if env == nil || len(env) != 0 {
		t.Fatalf("child environment = %#v, want an empty slice that is not nil", env)
	}

	// printf is a shell builtin, so the script needs no PATH to run.
	script := `printf '{"protocol":"twinwright.agent.v1","content":"%s","tool_calls":[]}' "${TW_SENTINEL_PROVIDER_KEY:-absent}"`
	message, err := shAgent(script).Next(context.Background(), "task", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if message.Content != "absent" {
		t.Fatalf("the agent saw the parent's TW_SENTINEL_PROVIDER_KEY = %q", message.Content)
	}
}
