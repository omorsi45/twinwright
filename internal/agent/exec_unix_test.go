//go:build unix

package agent

import (
	"errors"
	"syscall"
)

// processAlive reports whether a process with this ID still exists. A zombie
// that nobody has reaped still answers signal 0, so a false result also proves
// the provider reaped what it started.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
