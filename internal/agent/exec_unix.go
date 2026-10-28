//go:build unix

package agent

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the agent in a process group of its own, so that a
// timeout can end it together with anything it started.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcessGroup(cmd *exec.Cmd) { signalProcessGroup(cmd, syscall.SIGTERM) }

func killProcessGroup(cmd *exec.Cmd) { signalProcessGroup(cmd, syscall.SIGKILL) }

// signalProcessGroup signals every process in the agent's group. Setpgid made
// the agent the group leader, so the group ID is its process ID, and a negative
// ID addresses the group. An error means the group is already gone.
func signalProcessGroup(cmd *exec.Cmd, signal syscall.Signal) {
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, signal)
	}
}
