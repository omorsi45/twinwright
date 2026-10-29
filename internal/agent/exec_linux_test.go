//go:build linux

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// On Linux the agent gets SIGKILL when Twinwright dies, however it dies. This
// runs a Twinwright (this test binary, see runAsParent) mid-turn against an agent
// that never answers, kills it with SIGKILL, which gives it no chance to clean
// up, and requires the agent to be gone. It covers the direct child only: the
// kernel signals the process that was started, not what that process started.
func TestExecAgentDiesWithTheProcessThatStartedIt(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	killOnCleanup(t, pidFile)
	parent := exec.Command(exe)
	parent.Env = append(os.Environ(), parentPIDFile+"="+pidFile)
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	defer parent.Wait()
	defer parent.Process.Kill()

	var pid int
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if raw, err := os.ReadFile(pidFile); err == nil {
			if pid, err = strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				break
			}
		}
		if time.Now().After(end) {
			t.Fatal("the agent never started, so its exit cannot be checked")
		}
	}
	if !processAlive(pid) {
		t.Fatalf("agent %d is not running before Twinwright is killed", pid)
	}
	if err := parent.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	parent.Wait()
	requireGone(t, pidFile, 5*time.Second)
}
