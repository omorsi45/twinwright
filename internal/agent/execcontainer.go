package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"

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
// request, read-only root with a small writable /tmp, every capability dropped,
// no privilege gain, the unprivileged "nobody" user, the network, and the memory,
// CPU and process caps. The optional read-only mount and the passed variable
// names come after those and before the image, which is where docker stops
// reading options.
func (o ContainerOptions) Wrap(argv []string) []string {
	out := []string{
		"docker", "run", "--rm", "-i", "--read-only",
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
