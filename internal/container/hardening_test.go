package container

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// recorder is a fake docker CLI. It records every argv it was given and answers
// from a scripted response table, so a test can assert the exact command line
// the executor builds without a daemon.
type recorder struct {
	calls     [][]string
	responses map[string]response
	fallback  response
}

type response struct {
	stdout string
	stderr string
	err    error
}

func (r *recorder) runner() Runner {
	return func(_ context.Context, name string, args ...string) (string, string, error) {
		r.calls = append(r.calls, append([]string{name}, args...))
		if reply, ok := r.responses[args[0]]; ok {
			return reply.stdout, reply.stderr, reply.err
		}
		return r.fallback.stdout, r.fallback.stderr, r.fallback.err
	}
}

func (r *recorder) argv(verb string) []string {
	for _, call := range r.calls {
		if len(call) > 1 && call[1] == verb {
			return call
		}
	}
	return nil
}

func (r *recorder) count(verb string) int {
	n := 0
	for _, call := range r.calls {
		if len(call) > 1 && call[1] == verb {
			n++
		}
	}
	return n
}

func hardenedConfig(t *testing.T, extra string) Config {
	t.Helper()
	doc := `
version: 1
name: twinwright-probe
runtime: docker
image: hashicorp/http-echo:1.0
network: bridge
publish: ["18080:5678"]
startup_timeout: 2s
stop_timeout: 3s
health:
  command: ["wget", "-qO-", "http://127.0.0.1:5678/"]
  interval: 1ms
  retries: 5
resources:
  memory: 256m
  cpus: "0.5"
  pids: 128
label: experimental
` + extra
	cfg, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cfg
}

// A started container is not a ready container. Returning a handle the moment
// docker prints an ID hands the caller an address nothing is listening on yet.
func TestStartWaitsForHealthBeforeReturning(t *testing.T) {
	attempts := 0
	rec := &recorder{responses: map[string]response{
		"run": {stdout: "c0ffee123456\n"},
	}}
	inner := rec.runner()
	runner := func(ctx context.Context, name string, args ...string) (string, string, error) {
		if len(args) > 0 && args[0] == "exec" {
			attempts++
			if attempts < 3 {
				return inner(ctx, name, args...)
			}
			rec.calls = append(rec.calls, append([]string{name}, args...))
			return "ok", "", nil
		}
		return inner(ctx, name, args...)
	}
	rec.fallback = response{err: fmt.Errorf("exit status 1"), stderr: "connection refused"}
	rec.responses["inspect"] = response{stdout: "true\n"}

	d := NewDocker(runner)
	handle, err := d.Start(context.Background(), hardenedConfig(t, ""))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if handle.ID != "c0ffee123456" {
		t.Errorf("id=%q", handle.ID)
	}
	if !handle.Healthy {
		t.Error("handle does not report the container as healthy")
	}
	if attempts != 3 {
		t.Errorf("health probes=%d want 3", attempts)
	}

	// The probe must run inside the container, not on the host.
	exec := rec.argv("exec")
	if exec == nil || exec[2] != "twinwright-probe" {
		t.Fatalf("health probe argv=%v", exec)
	}
	if !strings.Contains(strings.Join(exec, " "), "http://127.0.0.1:5678/") {
		t.Errorf("health probe lost its command: %v", exec)
	}
}

// Every hardening option has to reach the command line. A config field that is
// parsed and then dropped is worse than one that does not exist: the config
// claims a limit the container does not have.
func TestStartPassesNetworkAndResourceLimits(t *testing.T) {
	rec := &recorder{responses: map[string]response{
		"run":     {stdout: "abc123\n"},
		"exec":    {stdout: "ok"},
		"inspect": {stdout: "true\n"},
	}}
	d := NewDocker(rec.runner())
	if _, err := d.Start(context.Background(), hardenedConfig(t, "")); err != nil {
		t.Fatalf("start: %v", err)
	}
	argv := strings.Join(rec.argv("run"), " ")
	for _, want := range []string{
		"--network bridge",
		"--memory 256m",
		"--cpus 0.5",
		"--pids-limit 128",
		"-p 18080:5678",
		"--label twinwright.experimental=true",
		"hashicorp/http-echo:1.0",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("run argv missing %q\ngot: %s", want, argv)
		}
	}
}

// A container that never becomes healthy must not be left running, and the
// error has to carry the container's own output. Without the logs the operator
// is told "unhealthy" and nothing about why.
func TestStartCleansUpAndReportsLogsWhenHealthNeverPasses(t *testing.T) {
	rec := &recorder{
		responses: map[string]response{
			"run":     {stdout: "dead1234\n"},
			"inspect": {stdout: "true\n"},
			"logs":    {stdout: "listen tcp 0.0.0.0:5678: bind: address already in use\n"},
		},
		fallback: response{err: fmt.Errorf("exit status 7"), stderr: "refused"},
	}
	d := NewDocker(rec.runner())
	_, err := d.Start(context.Background(), hardenedConfig(t, ""))
	if err == nil {
		t.Fatal("an unhealthy container was reported as started")
	}
	if !strings.Contains(err.Error(), "address already in use") {
		t.Errorf("error does not carry the container logs: %v", err)
	}
	if rec.count("logs") != 1 {
		t.Errorf("logs captured %d times, want 1", rec.count("logs"))
	}
	// Cleanup is the point: a failed start leaves nothing behind for the next
	// start to collide with.
	if rec.count("rm") == 0 && rec.count("stop") == 0 {
		t.Errorf("no cleanup was attempted: %v", rec.calls)
	}
}

// A container that exits immediately should fail fast rather than burn every
// retry probing a process that is gone.
func TestStartFailsFastWhenTheContainerExits(t *testing.T) {
	rec := &recorder{
		responses: map[string]response{
			"run":     {stdout: "gone9999\n"},
			"inspect": {stdout: "false\n"},
			"logs":    {stdout: "flag provided but not defined: -bogus\n"},
		},
		fallback: response{err: fmt.Errorf("exit status 1")},
	}
	d := NewDocker(rec.runner())
	_, err := d.Start(context.Background(), hardenedConfig(t, ""))
	if err == nil {
		t.Fatal("an exited container was reported as started")
	}
	if !strings.Contains(err.Error(), "exited") {
		t.Errorf("error does not say the container exited: %v", err)
	}
	if !strings.Contains(err.Error(), "not defined") {
		t.Errorf("error does not carry the container logs: %v", err)
	}
	if probes := rec.count("exec"); probes > 1 {
		t.Errorf("probed an exited container %d times", probes)
	}
}

// The startup budget has to be enforced by wall clock, not only by retry count:
// a probe that blocks would otherwise wait forever.
func TestStartStopsAtTheStartupTimeout(t *testing.T) {
	rec := &recorder{
		responses: map[string]response{
			"run":     {stdout: "slow5555\n"},
			"inspect": {stdout: "true\n"},
			"logs":    {stdout: "still starting\n"},
		},
		fallback: response{err: fmt.Errorf("exit status 1")},
	}
	d := NewDocker(rec.runner())
	// retries is high enough that only the deadline can end the loop.
	cfg := hardenedConfig(t, "")
	cfg.StartupTimeout = 30 * time.Millisecond
	cfg.Health.Retries = 100000
	cfg.Health.Interval = time.Millisecond
	started := time.Now()
	_, err := d.Start(context.Background(), cfg)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("start ignored its timeout")
	}
	if !strings.Contains(err.Error(), "timeout") && !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error does not mention the timeout: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("start took %v; the timeout was not enforced", elapsed)
	}
}

// Shutdown is deterministic: the container is asked to stop with a grace period
// and only then removed. `rm -f` sends SIGKILL immediately, which gives a
// service no chance to flush.
func TestStopAsksBeforeKilling(t *testing.T) {
	rec := &recorder{fallback: response{stdout: "ok"}}
	d := NewDocker(rec.runner())
	cfg := hardenedConfig(t, "")
	if err := d.Stop(context.Background(), cfg); err != nil {
		t.Fatalf("stop: %v", err)
	}
	stop := rec.argv("stop")
	if stop == nil {
		t.Fatalf("no stop was issued: %v", rec.calls)
	}
	if !strings.Contains(strings.Join(stop, " "), "--time 3") {
		t.Errorf("stop did not pass the grace period: %v", stop)
	}
	rm := rec.argv("rm")
	if rm == nil {
		t.Fatalf("container was not removed: %v", rec.calls)
	}
	if strings.Contains(strings.Join(rm, " "), "-f") {
		t.Errorf("rm still forces a kill: %v", rm)
	}
	// Order matters: stop before rm.
	stopIndex, rmIndex := -1, -1
	for i, call := range rec.calls {
		switch call[1] {
		case "stop":
			stopIndex = i
		case "rm":
			if rmIndex == -1 {
				rmIndex = i
			}
		}
	}
	if stopIndex > rmIndex {
		t.Errorf("rm came before stop: %v", rec.calls)
	}
}

// Status has to survive both shapes docker produces: a container whose image
// declares a HEALTHCHECK, and one whose State carries no Health key at all. The
// second is what broke the first real-daemon run.
func TestStatusReportsRunningAndHealth(t *testing.T) {
	withHealth := &recorder{responses: map[string]response{
		"inspect": {stdout: `abc123 {"Running":true,"Health":{"Status":"healthy"}}` + "\n"},
	}}
	handle, err := NewDocker(withHealth.runner()).Status(context.Background(), "twinwright-probe")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if handle.ID != "abc123" || !handle.Running || !handle.Healthy {
		t.Fatalf("handle=%+v", handle)
	}

	noHealthKey := &recorder{responses: map[string]response{
		"inspect": {stdout: `def456 {"Running":true,"Status":"running"}` + "\n"},
	}}
	handle, err = NewDocker(noHealthKey.runner()).Status(context.Background(), "twinwright-probe")
	if err != nil {
		t.Fatalf("status without a declared health check: %v", err)
	}
	if handle.ID != "def456" || !handle.Running || !handle.Healthy {
		t.Fatalf("handle=%+v; a running container with no declared health check is the best evidence available", handle)
	}

	unhealthy := &recorder{responses: map[string]response{
		"inspect": {stdout: `ghi789 {"Running":true,"Health":{"Status":"starting"}}` + "\n"},
	}}
	handle, err = NewDocker(unhealthy.runner()).Status(context.Background(), "twinwright-probe")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if handle.Healthy {
		t.Errorf("a container reporting health status starting was called healthy: %+v", handle)
	}
}

// The host-side HTTP probe needs no daemon to test: point a publish mapping at a
// local test server and the probe either reaches it or does not. This is the form
// a scratch image requires, since `docker exec` there fails with exit 127 however
// healthy the process is.
func TestStartProbesThePublishedPortFromTheHost(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		// Unready for the first two attempts, then serving.
		if requests < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	port := server.URL[strings.LastIndex(server.URL, ":")+1:]

	rec := &recorder{responses: map[string]response{
		"run":     {stdout: "scratch01\n"},
		"inspect": {stdout: "true\n"},
	}}
	cfg, err := Parse([]byte(`
version: 1
name: twinwright-scratch
runtime: docker
image: hashicorp/http-echo:1.0
network: bridge
publish: ["` + port + `:5678"]
health:
  http_path: /
  interval: 1ms
  retries: 10
label: experimental
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	d := NewDocker(rec.runner())
	handle, err := d.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !handle.Healthy {
		t.Error("handle does not report the container as healthy")
	}
	if requests < 3 {
		t.Errorf("probe requests=%d; the probe should have retried past the 500s", requests)
	}
	// An HTTP probe must not shell into the container at all.
	if rec.count("exec") != 0 {
		t.Errorf("an http probe ran docker exec %d times", rec.count("exec"))
	}
}

// An exec probe against an image without the command fails with exit 127. The
// error has to say that, because the container logs will show a perfectly healthy
// service and the reader would otherwise blame it.
func TestExecProbeExplainsExit127(t *testing.T) {
	rec := &recorder{
		responses: map[string]response{
			"run":     {stdout: "scratch02\n"},
			"inspect": {stdout: "true\n"},
			"logs":    {stdout: "[INFO] server is listening on :5678\n"},
		},
		fallback: response{err: fmt.Errorf("exit status 127")},
	}
	cfg := hardenedConfig(t, "")
	cfg.Health.Retries = 2
	d := NewDocker(rec.runner())
	_, err := d.Start(context.Background(), cfg)
	if err == nil {
		t.Fatal("start reported success")
	}
	if !strings.Contains(err.Error(), "does not contain that command") {
		t.Errorf("error does not explain exit 127: %v", err)
	}
	if !strings.Contains(err.Error(), "http_path") {
		t.Errorf("error does not point at the working alternative: %v", err)
	}
}

// Config validation. Each of these would otherwise produce a container whose
// real configuration differs from what the file says.
func TestParseRejectsUnsafeAndContradictoryConfig(t *testing.T) {
	base := `
version: 1
name: probe
runtime: docker
image: img:1
label: experimental
`
	for name, spec := range map[string]struct{ extra, want string }{
		"missing network": {"publish: [\"1:1\"]\n", "network is required"},
		"publish without a network": {"network: none\npublish: [\"1:1\"]\n",
			"publish requires a network"},
		"bad network name": {"network: \"has space\"\n", "network"},
		"bad memory":       {"network: bridge\nresources:\n  memory: 256mb; rm -rf /\n", "memory"},
		"bad cpus":         {"network: bridge\nresources:\n  cpus: \"0.5 --privileged\"\n", "cpus"},
		"negative pids":    {"network: bridge\nresources:\n  pids: -1\n", "pids"},
		"health without a command": {"network: bridge\nhealth:\n  interval: 1s\n  retries: 3\n",
			"health command"},
		"empty health argument": {"network: bridge\nhealth:\n  command: [\"wget\", \"\"]\n", "health command"},
		"zero retries":          {"network: bridge\nhealth:\n  command: [\"true\"]\n  retries: 0\n", "retries"},
		"unparsable duration":   {"network: bridge\nstartup_timeout: soon\n", "startup_timeout"},
		"absurd timeout":        {"network: bridge\nstartup_timeout: 4h\n", "startup_timeout"},
		"negative stop timeout": {"network: bridge\nstop_timeout: -1s\n", "stop_timeout"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(base + spec.extra))
			if err == nil {
				t.Fatalf("accepted:\n%s", base+spec.extra)
			}
			if !strings.Contains(err.Error(), spec.want) {
				t.Fatalf("err=%q want it to mention %q", err, spec.want)
			}
		})
	}
}

// Defaults have to be safe and stated. An unset startup timeout that means
// "wait forever" would hang a CI job rather than fail it.
func TestParseAppliesBoundedDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`
version: 1
name: probe
runtime: docker
image: img:1
network: bridge
label: experimental
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StartupTimeout <= 0 || cfg.StartupTimeout > 10*time.Minute {
		t.Errorf("startup timeout default=%v", cfg.StartupTimeout)
	}
	if cfg.StopTimeout <= 0 {
		t.Errorf("stop timeout default=%v", cfg.StopTimeout)
	}
	if len(cfg.Health.Command) != 0 {
		t.Errorf("health invented a command: %v", cfg.Health.Command)
	}
}
