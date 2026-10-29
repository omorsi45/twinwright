//go:build unix

package agent

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the agent in a process group of its own, so that the end
// of a turn can end it together with anything it started that stayed in the
// group. On Linux it also asks the kernel to kill the agent if Twinwright dies.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	parentDeathSignal(cmd.SysProcAttr)
}

func terminateProcessGroup(cmd *exec.Cmd) { signalProcessGroup(cmd, syscall.SIGTERM) }

func killProcessGroup(cmd *exec.Cmd) { signalProcessGroup(cmd, syscall.SIGKILL) }

// signalProcessGroup signals every process in the agent's group. Setpgid made
// the agent the group leader, so the group ID is its process ID, and a negative
// ID addresses the group. The group outlives its reaped leader for as long as a
// member does, so this also reaches what a finished agent left behind. An error
// means the group is already gone.
func signalProcessGroup(cmd *exec.Cmd, signal syscall.Signal) {
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, signal)
	}
}
