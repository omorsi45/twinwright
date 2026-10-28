//go:build !unix

package agent

// processAlive cannot ask the operating system about a bare process ID here,
// so it reports false and the exit checks of the exec tests prove nothing on
// this platform. The unix build is the one CI runs.
func processAlive(int) bool { return false }
