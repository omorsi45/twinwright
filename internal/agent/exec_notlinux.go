//go:build !linux

package agent

import "syscall"

// Only Linux has a parent-death signal for a child. Elsewhere an agent outlives
// a Twinwright that dies mid-turn, which docs/protocol/agent-v1.md says.

func parentDeathSignal(*syscall.SysProcAttr) {}

func holdThread() func() { return func() {} }
