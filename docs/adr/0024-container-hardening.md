# ADR 0024: Container lifecycle hardening

- Status: accepted
- Date: 2026-09-29
- Extends: ADR 0016 (optional local or Docker sidecars)

## Context

The Docker executor did the minimum: `docker run -d`, `docker rm -f`, `docker
inspect`. It worked, and every one of its tests used an injected fake runner, so
nothing had ever proved the daemon accepts the command line it builds.

Four gaps mattered more than the missing flags.

**A started container is not a ready container.** `Start` returned as soon as
docker printed an ID. The caller got a published address with nothing listening
on it yet, and the only way to use a sidecar safely was to sleep and hope.

**A failed start left the name taken.** `docker run` can fail after creating the
container. The next start then failed on a name collision, reporting that instead
of the original cause.

**A failure said nothing about why.** The error carried docker's stderr, which
for a container that started and then died says nothing. The reason was in the
container's own output, which was never read.

**Shutdown was a kill.** `docker rm -f` sends SIGKILL immediately.

## Decision

### Readiness is part of starting

`Start` does not return until the container is ready, or until it has been
cleaned up. With a `health` block configured, readiness means a probe executed
inside the container succeeded. Without one, readiness means docker still reports
the container running, which is weaker evidence and is documented as weaker
rather than presented as a health check.

The probe runs via `docker exec` rather than relying on the image's own
`HEALTHCHECK`, because most images declare none. `Status` still reports the
image's declared health when there is one.

Between probes the executor re-checks that the container is running, so a
container that exits immediately fails in one probe instead of spending every
retry on a process that is gone.

### Every wait is bounded

`startup_timeout` defaults to 30 seconds and caps at 10 minutes. An unbounded
wait turns a broken image into a hung CI job rather than a failed one. The budget
is enforced by a context deadline, not only by the retry count, so a probe that
blocks cannot outlast it.

### Failure cleans up and explains

A failed `docker run` is followed by a best-effort `docker rm -f` of the name, so
the next start fails on its own merits. A container that starts but never becomes
ready is stopped and removed with the same grace period a normal stop uses, and
the error carries the last 50 lines of the container's output.

### Shutdown asks before killing

`Stop` issues `docker stop --time N` and then `docker rm`. The grace period comes
from `stop_timeout`, default 5 seconds.

This changed the `Executor` interface: `Stop` takes the whole `Config` rather than
a name, because the grace period is configuration and a name lookup cannot supply
it.

### Network is required, not defaulted

For the docker runtime, `network` must be stated. A default would be a surprising
choice either way: `bridge` quietly gives a sidecar host reachability, and `none`
quietly breaks `publish`.

Publishing a port on `network: none` is refused at parse time. Docker itself
refuses that combination, so catching it in the config turns a runtime surprise
into a config error with an explanation.

### Resource limits are validated as argv

`memory`, `cpus` and `pids` become command-line arguments, so each is matched
against a strict pattern (`^[0-9]+(b|k|m|g|kb|mb|gb)?$`, `^[0-9]+(\.[0-9]+)?$`,
positive integer) rather than passed through. `resources.memory: "256m; rm -rf /"`
is a parse error. Nothing is passed to a shell, so this is defence in depth rather
than the only barrier, but a value that cannot be a limit should not reach docker
at all.

Secrets keep their existing rule: `env_file` only, never argv, because a process
list is readable by any user on the host.

## Integration tests, and what they are for

The unit tests assert the argv the executor builds. That is what a fake can
prove, and it is not enough: a fake will happily accept a flag docker rejects.

`internal/container/integration_test.go` runs against a real daemon, gated on
`TWINWRIGHT_TEST_DOCKER=1` exactly as the PostgreSQL tests are gated on a DSN, so
`go test ./...` never reaches for a daemon or pulls an image on a contributor's
machine. A new Integration workflow job sets the variable.

Two things are proved there that no fake can:

- The shipped example config starts, and an HTTP request to its published port
  succeeds the moment `Start` returns. Without that assertion the health gate is
  a claim rather than a guarantee. The test parses `examples/container/http-echo.yaml`
  instead of an inline config, so the documented example is what gets verified.
- A container that exits immediately fails the start and leaves no container
  behind, checked with `docker ps -a`.

The CI job also drives `container start|status|stop` through the CLI, so the
documented commands are covered and not just the package API.

## Rejected

**Kubernetes.** ADR 0016 declined it and nothing here changes that. A sidecar is
one optional process for one world; a scheduler would add an operational
dependency to a feature explicitly labelled experimental.

**Container identity in traces.** This was on the hardening list and is
deliberately not implemented. A sidecar today is a CLI-managed process with no
association to a run: no world's services talk to one, and nothing in the ledger
references one. Putting a container ID on a span would assert a relationship the
runtime does not enforce, which is exactly the kind of claim this project refuses
to make. It becomes worth doing when a world can declare a sidecar it actually
depends on, and that is a different change.

**Auto-detecting Docker instead of an opt-in variable.** Detection would mean
that a developer with a running daemon gets images pulled by an ordinary
`go test ./...`. Explicit opt-in matches the PostgreSQL tests and keeps the
default suite hermetic.

## Consequences

`network` is now required for docker configs, so an existing config without it
fails to parse with a message naming the options. The shipped example is updated
and gains health, timeouts and resource limits, which makes it a better starting
point than it was.

The container feature remains labelled experimental, and the label is still
enforced at parse time. Hardening a feature is not the same as promoting it: no
world depends on a sidecar, so the honest description is an optional process with
a careful lifecycle.
