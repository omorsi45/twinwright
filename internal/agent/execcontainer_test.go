package agent

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func sampleContainer(mount string) ContainerOptions {
	return ContainerOptions{Image: "python:3.12-alpine", Network: "none", Memory: "512m", CPUs: "1", PIDs: 128, Mount: mount}
}

// containerNamePattern is the name given to each turn's container: unique, so
// that it can be removed without touching any other container.
var containerNamePattern = regexp.MustCompile(`^tw-agent-[0-9a-f]{16}$`)

// withoutName returns the argv without its --name pair, and the name that was
// there. The name is random, so the rest of the argv is what is compared whole.
func withoutName(t *testing.T, argv []string) ([]string, string) {
	t.Helper()
	i := slices.Index(argv, "--name")
	if i < 0 || i+1 >= len(argv) {
		t.Fatalf("argv has no --name: %q", argv)
	}
	name := argv[i+1]
	if !containerNamePattern.MatchString(name) {
		t.Fatalf("container name %q does not match %s", name, containerNamePattern)
	}
	return slices.Concat(argv[:i], argv[i+2:]), name
}

// Each turn's container has a name of its own, so that Cleanup can remove it
// after a timeout without a pattern that could match somebody else's.
func TestContainerWrapNamesEachTurnsContainer(t *testing.T) {
	opts := sampleContainer("")
	first, firstName := withoutName(t, opts.Wrap([]string{"agent"}))
	_, secondName := withoutName(t, opts.Wrap([]string{"agent"}))
	if firstName == secondName {
		t.Fatalf("two turns share the container name %q", firstName)
	}
	wrapped := opts.Wrap([]string{"agent"})
	if !slices.Equal(wrapped[:5], []string{"docker", "run", "--rm", "-i", "--name"}) {
		t.Fatalf("the name is not among the first options: %q", wrapped[:6])
	}
	if first[len(first)-1] != "agent" {
		t.Fatalf("the agent argv moved: %q", first)
	}
}

// The argv is the isolation. It is asserted whole, in order, so a flag cannot be
// dropped, reordered into the wrong position relative to the image, or joined by
// a new one without a test noticing.
func TestContainerWrapIsHardened(t *testing.T) {
	head := []string{
		"docker", "run", "--rm", "-i", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--user", "65534:65534", "--network", "none", "--memory", "512m", "--cpus", "1", "--pids-limit", "128",
		"--tmpfs", "/tmp:rw,size=16m",
	}
	agentArgv := []string{"python3", "/agent/billing_agent.py"}

	t.Run("with a mount", func(t *testing.T) {
		got, _ := withoutName(t, sampleContainer("/work/agent dir").Wrap(agentArgv))
		want := slices.Concat(head, []string{"-v", "/work/agent dir:/agent:ro", "python:3.12-alpine"}, agentArgv)
		if !slices.Equal(got, want) {
			t.Fatalf("argv =\n%q\nwant\n%q", got, want)
		}
	})
	t.Run("without a mount", func(t *testing.T) {
		got, _ := withoutName(t, sampleContainer("").Wrap(agentArgv))
		want := slices.Concat(head, []string{"python:3.12-alpine"}, agentArgv)
		if !slices.Equal(got, want) {
			t.Fatalf("argv =\n%q\nwant\n%q", got, want)
		}
	})
	t.Run("with environment names", func(t *testing.T) {
		opts := sampleContainer("/work/agent")
		opts.EnvNames = []string{"MY_TOKEN", "OTHER"}
		wrapped := opts.Wrap(agentArgv)
		got, _ := withoutName(t, wrapped)
		// -e NAME only: the value is inherited from the docker client's own
		// environment, so it is never an argument anyone can read.
		want := slices.Concat(head, []string{"-v", "/work/agent:/agent:ro", "-e", "MY_TOKEN", "-e", "OTHER", "python:3.12-alpine"}, agentArgv)
		if !slices.Equal(got, want) {
			t.Fatalf("argv =\n%q\nwant\n%q", got, want)
		}
		for _, arg := range got {
			if strings.Contains(arg, "=") && strings.HasPrefix(arg, "MY_") {
				t.Fatalf("a value reached the argv: %q", arg)
			}
		}
	})
	t.Run("does not touch the agent argv", func(t *testing.T) {
		argv := slices.Clone(agentArgv)
		got := sampleContainer("").Wrap(argv)
		got[len(got)-1] = "changed"
		if !slices.Equal(argv, agentArgv) {
			t.Fatalf("Wrap shares memory with its argument: %q", argv)
		}
	})
}

// The docker client's connection variables belong to the client process. The
// container gets the names Wrap was given and nothing else, so none of them, and
// none of their values, can be in the command line.
func TestContainerWrapNeverCarriesTheDockerConnectionVariables(t *testing.T) {
	for _, name := range DockerClientEnv {
		t.Setenv(name, "value-of-"+name)
	}
	opts := sampleContainer("")
	opts.EnvNames = []string{"MY_TOKEN"}
	for _, arg := range opts.Wrap([]string{"agent"}) {
		for _, name := range DockerClientEnv {
			if strings.Contains(arg, name) || strings.Contains(arg, "value-of-"+name) {
				t.Errorf("the docker command line carries %s: %q", name, arg)
			}
		}
	}
}

func TestContainerOptionsRejectBadValues(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	valid := sampleContainer(dir)
	if err := valid.Validate(); err != nil {
		t.Fatalf("the baseline is refused: %v", err)
	}
	if err := sampleContainer("").Validate(); err != nil {
		t.Fatalf("no mount is allowed: %v", err)
	}
	bridge := valid
	bridge.Network = "bridge"
	if err := bridge.Validate(); err != nil {
		t.Fatalf("bridge is a legitimate choice: %v", err)
	}
	named := valid
	named.Network = "my-net_1"
	named.Memory, named.CPUs = "1g", "0.5"
	if err := named.Validate(); err != nil {
		t.Fatalf("a named network with other limits is refused: %v", err)
	}

	cases := []struct {
		name string
		edit func(*ContainerOptions)
		want []string
	}{
		{"memory lots", func(o *ContainerOptions) { o.Memory = "lots" }, []string{"memory", "lots"}},
		// Docker reads zero as "no limit", so each of these would remove the cap.
		{"zero memory", func(o *ContainerOptions) { o.Memory = "0" }, []string{"memory", "no limit"}},
		{"zero memory with a unit", func(o *ContainerOptions) { o.Memory = "0m" }, []string{"memory", "no limit"}},
		{"zero memory in bytes", func(o *ContainerOptions) { o.Memory = "0b" }, []string{"memory", "no limit"}},
		{"zero memory written long", func(o *ContainerOptions) { o.Memory = "000G" }, []string{"memory", "no limit"}},
		{"zero cpus", func(o *ContainerOptions) { o.CPUs = "0" }, []string{"cpus", "no limit"}},
		{"zero cpus with a fraction", func(o *ContainerOptions) { o.CPUs = "0.0" }, []string{"cpus", "no limit"}},
		{"zero cpus written long", func(o *ContainerOptions) { o.CPUs = "00.000" }, []string{"cpus", "no limit"}},
		{"memory as a shell fragment", func(o *ContainerOptions) { o.Memory = "256m; rm -rf /" }, []string{"memory"}},
		{"no memory", func(o *ContainerOptions) { o.Memory = "" }, []string{"memory"}},
		{"cpus many", func(o *ContainerOptions) { o.CPUs = "many" }, []string{"cpus"}},
		{"cpus with a flag", func(o *ContainerOptions) { o.CPUs = "0.5 --privileged" }, []string{"cpus"}},
		{"no cpus", func(o *ContainerOptions) { o.CPUs = "" }, []string{"cpus"}},
		{"zero pids", func(o *ContainerOptions) { o.PIDs = 0 }, []string{"pids"}},
		{"negative pids", func(o *ContainerOptions) { o.PIDs = -5 }, []string{"pids"}},
		{"host network, by name", func(o *ContainerOptions) { o.Network = "host" }, []string{"host", "isolation"}},
		{"joining another container", func(o *ContainerOptions) { o.Network = "container:abc" }, []string{"network"}},
		{"network with a space", func(o *ContainerOptions) { o.Network = "a b" }, []string{"network"}},
		{"network that is a flag", func(o *ContainerOptions) { o.Network = "--privileged" }, []string{"network"}},
		{"no network", func(o *ContainerOptions) { o.Network = "" }, []string{"network"}},
		{"relative mount", func(o *ContainerOptions) { o.Mount = "examples/agents/python" }, []string{"mount", "absolute"}},
		{"mount that is missing", func(o *ContainerOptions) { o.Mount = filepath.Join(dir, "missing") }, []string{"mount", "directory"}},
		{"mount that is a file", func(o *ContainerOptions) { o.Mount = file }, []string{"mount", "directory"}},
		{"mount that rewrites the volume spec", func(o *ContainerOptions) { o.Mount = dir + ":/etc:rw" }, []string{"mount", "':'"}},
		{"empty image", func(o *ContainerOptions) { o.Image = "" }, []string{"image"}},
		{"blank image", func(o *ContainerOptions) { o.Image = "  " }, []string{"image"}},
		{"image that is a docker flag", func(o *ContainerOptions) { o.Image = "--privileged" }, []string{"image"}},
		{"image with a space", func(o *ContainerOptions) { o.Image = "python 3" }, []string{"image"}},
		{"environment name that is not a name", func(o *ContainerOptions) { o.EnvNames = []string{"NOT A NAME"} }, []string{"environment"}},
		{"environment entry with a value", func(o *ContainerOptions) { o.EnvNames = []string{"NAME=value"} }, []string{"environment"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := valid
			tc.edit(&opts)
			err := opts.Validate()
			if err == nil {
				t.Fatalf("%+v was accepted", opts)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}
