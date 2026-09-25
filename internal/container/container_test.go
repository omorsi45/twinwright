package container

import (
	"context"
	"strings"
	"testing"
)

func TestParseDockerConfig(t *testing.T) {
	cfg, err := Parse([]byte(`
version: 1
name: echo-sidecar
runtime: docker
image: hashicorp/http-echo:1.0
publish: ["8080:5678"]
label: experimental
`))
	if err != nil || cfg.Runtime != "docker" || cfg.Image == "" {
		t.Fatalf("%+v err=%v", cfg, err)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"k8s":        "version: 1\nname: x\nruntime: kubernetes\nlabel: experimental\n",
		"no label":   "version: 1\nname: x\nruntime: local\n",
		"docker img": "version: 1\nname: x\nruntime: docker\nlabel: experimental\n",
		"abs env":    "version: 1\nname: x\nruntime: local\nlabel: experimental\nenv_file: /tmp/secrets\n",
		"escape env": "version: 1\nname: x\nruntime: local\nlabel: experimental\nenv_file: ../secrets\n",
	}
	for name, raw := range cases {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestLocalExecutor(t *testing.T) {
	ex := NewLocal()
	cfg, err := Parse([]byte("version: 1\nname: local-world\nruntime: local\nlabel: experimental\n"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := ex.Start(context.Background(), cfg)
	if err != nil || h.Addr != "in-process" {
		t.Fatalf("%+v err=%v", h, err)
	}
	if _, err := ex.Status(context.Background(), "local-world"); err != nil {
		t.Fatal(err)
	}
	if err := ex.Stop(context.Background(), "local-world"); err != nil {
		t.Fatal(err)
	}
}

func TestDockerExecutorUsesRunner(t *testing.T) {
	var saw []string
	d := NewDocker(func(ctx context.Context, name string, args ...string) (string, string, error) {
		saw = append(saw, name+" "+strings.Join(args, " "))
		if args[0] == "run" {
			return "cid123\n", "", nil
		}
		return "cid123\n", "", nil
	})
	cfg, err := Parse([]byte(`
version: 1
name: echo
runtime: docker
image: hashicorp/http-echo:1.0
publish: ["8080:5678"]
env_file: secrets.env
label: experimental
`))
	if err != nil {
		t.Fatal(err)
	}
	h, err := d.Start(context.Background(), cfg)
	if err != nil || h.ID != "cid123" {
		t.Fatalf("%+v err=%v", h, err)
	}
	joined := strings.Join(saw, "\n")
	if !strings.Contains(joined, "docker run") || !strings.Contains(joined, "--env-file secrets.env") {
		t.Fatalf("args=%v", saw)
	}
	if strings.Contains(joined, "SECRET=") || strings.Contains(joined, "-e ") {
		t.Fatalf("secrets must not appear on argv: %v", saw)
	}
	if err := d.Stop(context.Background(), "echo"); err != nil {
		t.Fatal(err)
	}
}

func TestDockerStartSurfacesDaemonErrors(t *testing.T) {
	d := NewDocker(func(ctx context.Context, name string, args ...string) (string, string, error) {
		return "", "Cannot connect to the Docker daemon", errDaemon
	})
	cfg := Config{Name: "x", Runtime: "docker", Image: "busybox", Label: "experimental"}
	_, err := d.Start(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "daemon") {
		t.Fatalf("err=%v", err)
	}
}

var errDaemon = &daemonError{}

type daemonError struct{}

func (*daemonError) Error() string { return "exit status 1" }

func TestSelect(t *testing.T) {
	if _, err := Select("local", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Select("k8s", nil); err == nil {
		t.Fatal("accepted")
	}
}
