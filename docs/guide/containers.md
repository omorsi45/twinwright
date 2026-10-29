# Optional containers

This page was moved out of the project README when the README was rewritten as the Twinwright Cloud overview. It holds the README section "Optional containers", unchanged except where noted. Paths in backticks are relative to the repository root. [Back to the guide index](README.md).

## Optional containers

**Experimental.** Default worlds stay in-process. When a scenario needs an isolated sidecar, use a version 1 container config:

```bash
go run ./cmd/twinwright container start --config examples/container/local.yaml
go run ./cmd/twinwright container status --config examples/container/local.yaml
go run ./cmd/twinwright container stop --config examples/container/local.yaml
```

`runtime: local` is a no-op handle for in-process work. `runtime: docker` shells to the docker CLI and requires a running daemon.

`Start` does not return until the container is ready. There are two probe forms. `health.command` runs inside the container with `docker exec`, which suits an image that carries the tool it needs. `health.http_path` is requested from the host against the first published port, which is the only form that works for a scratch or distroless image: those contain no shell and no client, so an exec probe fails with exit 127 however healthy the process is. Any status below 500 counts as ready. Without a `health` block, readiness means only that docker still reports the container running, which is weaker evidence. Every wait is bounded by `startup_timeout` (default 30s), enforced by a deadline rather than a retry count, and a container that exits during startup fails in one probe instead of burning every retry.

A failed start cleans up after itself: the container is stopped and removed, the name is released, and the error carries the last 50 lines of the container's own output, which is usually where the reason is. Shutdown asks before killing: `docker stop --time N` with the `stop_timeout` grace period, then `docker rm`.

`network` is required for the docker runtime rather than defaulted, because a default would quietly choose either host reachability or a broken `publish`. Publishing a port with `network: none` is refused at parse time, since docker refuses it too. `resources.memory`, `resources.cpus` and `resources.pids` become command-line arguments and are validated against strict patterns, so a value that cannot be a limit never reaches docker. Secrets belong in a relative `--env-file`, never on the command line.

Kubernetes is not supported. Container identity is deliberately absent from traces: a sidecar has no association with a run, and putting a container ID on a span would assert a relationship the runtime does not enforce.

The unit tests use an injected runner. The claims above that a fake cannot prove are covered by `internal/container/integration_test.go` against a real daemon, gated on `TWINWRIGHT_TEST_DOCKER=1` and run by a job in the Integration workflow: the shipped example starts and its published port answers an HTTP request the moment `Start` returns, and a container that exits immediately leaves nothing behind. Those tests have not been run on a Windows development host without a daemon; CI is where that evidence comes from. See `docs/adr/0016-optional-containers.md` and `docs/adr/0024-container-hardening.md`.
