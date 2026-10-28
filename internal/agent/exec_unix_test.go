//go:build unix

package agent

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
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
