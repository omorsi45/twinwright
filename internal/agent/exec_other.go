//go:build !unix

package agent

import "os/exec"

// There is no portable process group here, so the agent is ended with a plain
// kill and anything it started is left to the operating system. The unix build
// is the one with the group semantics the protocol document describes.

func setProcessGroup(*exec.Cmd) {}

func terminateProcessGroup(cmd *exec.Cmd) { killProcessGroup(cmd) }

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		cmd.Process.Kill()
	}
}
