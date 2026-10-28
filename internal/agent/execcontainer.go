package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"twinwright/internal/container"
)

// ContainerOptions describes the hardened `docker run` an exec agent can be put
// behind, with ExecProvider.Wrap = ContainerOptions.Wrap.
//
// The container is the boundary the agent process runs inside: no network by
// default, a read-only root filesystem, no capabilities, no privilege gain, a
// non-root user, and capped memory, CPU and process count. It does not make the
// agent trustworthy. What it limits is what an agent that misbehaves can reach,
// and docs/adr/0033-agent-protocol-and-isolation.md says where that stops.
type ContainerOptions struct {
	// Image is the image the agent runs in. It must already contain the
	// interpreter or binary the agent command needs.
	Image string
	// Network is the docker network mode: "none" for no network at all, or a
	// network name. "host" is refused.
	Network string
	// Memory and CPUs are the limits passed to docker, held to the same patterns
	// as a sidecar config's resources.
	Memory, CPUs string
	// PIDs caps the number of processes in the container.
	PIDs int
	// Mount is an absolute host directory made visible read-only at /agent. Empty
	// mounts nothing.
	Mount string
	// EnvNames are variables passed through to the agent by name. docker takes the
	// value from its own environment, which ExecProvider.Env supplies, so a value
	// is never an argument that a process listing could show.
	EnvNames []string
	// Logger receives the warning when Cleanup cannot remove a container. Nil
	// means slog.Default.
	Logger *slog.Logger
}

// agentImage is the shape of an image reference: a registry, repository, tag and
// digest are all made of these characters. What matters here is the first one.
// The image sits among docker's own arguments, so a reference starting with "-"
// would be read as one more option, and that is how a "--privileged" would get
// into an otherwise hardened command.
var agentImage = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// drive is a Windows drive prefix, the one place a colon is part of a host path.
var drive = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// Validate refuses options that would weaken the container or that cannot be an
// argument. Every value here ends up in an argv, so each is checked against a
// pattern and not passed through; nothing reaches a shell, which makes this the
// second barrier and not the first.
func (o ContainerOptions) Validate() error {
	if strings.TrimSpace(o.Image) == "" {
		return errors.New("container image is required")
	}
	if !agentImage.MatchString(o.Image) {
		return fmt.Errorf("container image %q is not an image reference: it must start with a letter or digit and contain no spaces or option-like text", o.Image)
	}
	switch {
	case o.Network == "host":
		return errors.New(`container network "host" is refused: it removes the network isolation this mode exists for`)
	case !container.ValidNetwork(o.Network):
		return fmt.Errorf("container network %q is not valid: use none, bridge or a user-defined network name", o.Network)
	}
	if !container.ValidMemory(o.Memory) {
		return fmt.Errorf("container memory %q is not valid: use a byte count with an optional b, k, m or g suffix", o.Memory)
	}
	if !container.ValidCPUs(o.CPUs) {
		return fmt.Errorf("container cpus %q is not valid: use a decimal number of CPUs", o.CPUs)
	}
	// The patterns accept zero, and docker reads a zero memory or CPU limit as no
	// limit at all, so it would drop a cap through the same kind of option that
	// "host" is refused for.
	if isZeroLimit(o.Memory) {
		return fmt.Errorf("container memory %q means no limit to docker: use a positive size", o.Memory)
	}
	if isZeroLimit(o.CPUs) {
		return fmt.Errorf("container cpus %q means no limit to docker: use a positive number of CPUs", o.CPUs)
	}
	if o.PIDs < 1 {
		return fmt.Errorf("container pids must be at least 1, got %d", o.PIDs)
	}
	for _, name := range o.EnvNames {
		if !envName.MatchString(name) {
			return errors.New("container environment entries must be variable NAMEs of letters, digits and underscores; a value is never passed on the command line")
		}
	}
	if o.Mount == "" {
		return nil
	}
	if !filepath.IsAbs(o.Mount) {
		return fmt.Errorf("container mount %q must be an absolute path", o.Mount)
	}
	// A colon would end the host path early and let the rest be read as the
	// container path or the mode, so a mount could become something else.
	if strings.Contains(o.Mount, ":") && !(runtime.GOOS == "windows" && drive.MatchString(o.Mount) && !strings.Contains(o.Mount[2:], ":")) {
		return fmt.Errorf("container mount path must not contain ':' because it would change the volume specification: %q", o.Mount)
	}
	info, err := os.Stat(o.Mount)
	if err != nil {
		return fmt.Errorf("container mount must be an existing directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("container mount %q must be a directory", o.Mount)
	}
	return nil
}

// Wrap puts argv behind the hardened docker command and returns the result. It
// does not validate: callers run Validate first, and Wrap stays a plain function
// of its options so that the argv it builds can be asserted whole.
//
// The flags, in order: remove the container on exit, keep stdin open for the
// request, a name for this turn's container alone, read-only root with a small
// writable /tmp, every capability dropped, no privilege gain, the unprivileged
// "nobody" user, the network, and the memory, CPU and process caps. The optional
// read-only mount and the passed variable names come after those and before the
// image, which is where docker stops reading options.
//
// The name is fresh on every call, so a turn that has to be cut short can have
// its container removed (Cleanup) without a pattern that could match a container
// that is somebody else's.
func (o ContainerOptions) Wrap(argv []string) []string {
	out := []string{
		"docker", "run", "--rm", "-i", "--name", newContainerName(), "--read-only",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--user", "65534:65534",
		"--network", o.Network, "--memory", o.Memory, "--cpus", o.CPUs, "--pids-limit", strconv.Itoa(o.PIDs),
		"--tmpfs", "/tmp:rw,size=16m",
	}
	if o.Mount != "" {
		out = append(out, "-v", o.Mount+":/agent:ro")
	}
	for _, name := range o.EnvNames {
		out = append(out, "-e", name)
	}
	out = append(out, o.Image)
	return slices.Concat(out, argv)
}

// agentContainerName is the shape of the names newContainerName makes: a fixed
// prefix and 16 hexadecimal digits.
var agentContainerName = regexp.MustCompile(`^tw-agent-[0-9a-f]{16}$`)

// newContainerName makes the name of one turn's container: random, so that two
// turns, runs or Twinwright processes never share one.
func newContainerName() string {
	var random [8]byte
	rand.Read(random[:])
	return "tw-agent-" + hex.EncodeToString(random[:])
}

// containerName reads back the name Wrap gave the container in the argv it
// returned. Only a name of the shape newContainerName makes is accepted, so what
// Cleanup is asked to remove can only be a container this package named.
func containerName(wrapped []string) (string, bool) {
	if len(wrapped) < 6 || wrapped[0] != "docker" || wrapped[1] != "run" {
		return "", false
	}
	for i := 2; i+1 < len(wrapped); i++ {
		if wrapped[i] == "--name" {
			return wrapped[i+1], agentContainerName.MatchString(wrapped[i+1])
		}
	}
	return "", false
}

// containerRemoveTimeout bounds `docker rm -f`: removing a container must not
// hold up the turn's error for longer than a daemon can be expected to take.
const containerRemoveTimeout = 10 * time.Second

// Cleanup removes the container that wrapped started, after the turn was cut
// short. Killing the docker client does not stop the container, and a Python
// agent running as PID 1 ignores the SIGTERM the client proxies, so on a timeout,
// a cancellation or an output overflow the agent would otherwise keep running,
// and keep writing, after Twinwright has given up on it.
//
// It is best effort: a failure is logged and the turn's own error stands. The
// call has a deadline of its own and does not stop when the turn's context has
// been cancelled, since a cancelled turn is when it matters. A container that is
// already gone, because --rm won the race, is not a failure. It needs the same
// connection variables as the client that started the container.
func (o ContainerOptions) Cleanup(ctx context.Context, wrapped []string) {
	name, ok := containerName(wrapped)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), containerRemoveTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "rm", "-f", name)
	cmd.Env = append(parentEnv("PATH", "HOME"), parentEnv(DockerClientEnv...)...)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return
	}
	text := strings.TrimSpace(string(output))
	if strings.Contains(text, "No such container") {
		return
	}
	// The daemon's address can be in docker's own message; it is masked like the
	// passed values are.
	var values []string
	for _, pair := range parentEnv(DockerClientEnv...) {
		_, value, _ := strings.Cut(pair, "=")
		values = append(values, value)
	}
	text = maskSpellings(text, secretSpellings(values))
	if len(text) > 500 {
		text = text[:500]
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("could not remove the agent container after the turn was cut short; it may still be running",
		"container", name, "error", err, "output", text)
}

// DockerClientEnv lists the variables the docker client reads to find its
// daemon. They are handed to the client process (ExecProvider.ClientEnv) and
// never to the container: Wrap passes the container only the names it is given.
var DockerClientEnv = []string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY"}

// isZeroLimit reports whether a memory or CPU limit that already matched its
// pattern is zero, which docker reads as no limit: all its digits are zeros,
// whatever the suffix or the fraction.
func isZeroLimit(limit string) bool {
	digits := strings.TrimRight(strings.ToLower(limit), "bkmg")
	digits = strings.ReplaceAll(digits, ".", "")
	return digits != "" && strings.Trim(digits, "0") == ""
}
