//go:build linux

package agent

import (
	"runtime"
	"syscall"
)

// parentDeathSignal asks the kernel to SIGKILL the agent if the process that
// started it dies, so that a Twinwright that is killed or crashes mid-turn does
// not leave a hung agent running for ever. It covers the agent itself and nothing
// it started: the kernel signals the child of the dying process, not the child's
// own children.
func parentDeathSignal(attr *syscall.SysProcAttr) { attr.Pdeathsig = syscall.SIGKILL }

// holdThread pins the calling goroutine to its OS thread until the returned
// function is called. The kernel ties the parent-death signal to the thread that
// started the child, not to the process, and Go retires threads when it likes: a
// thread that exits while the turn runs would kill the agent mid-answer. Holding
// the thread for the length of a turn keeps the signal tied to the life of the
// process.
func holdThread() func() {
	runtime.LockOSThread()
	return runtime.UnlockOSThread
}
