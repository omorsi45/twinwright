# 0033. The agent protocol and container isolation

Date: 2026-10-28

Status: accepted; extends ADR 0013, ADR 0024 and ADR 0027

## Context

A run needs a decision maker, and until now there were two: a scripted fixture in
this repository, and an adapter for a hosted model. An agent that already exists
as a program, in any language, had to be rewritten as a Go adapter before it could
be put in front of a world. That is the wrong cost for the thing this project is
for, which is testing agents someone else wrote.

Putting a stranger's program in the loop raises questions the adapters never did.
What does it get to see? What can it do if it misbehaves, by crashing, hanging,
flooding its output or printing a secret it was given? Can a run it took part in
be replayed without it? And if it is run in a container to limit the damage, what
does the container actually limit?

This ADR records the protocol, the bounds around the process, the container mode,
and what each is and is not shown to do.

## Decision

### A provider that is a process

`--agent exec` selects `agent.ExecProvider`. `--agent-command` is the command line
and `--model` is required and names the agent and its version, for example
`billing-agent@1`. It is the only thing the run records about who decided, and an
empty one would record two different programs as the same nothing.

The command line is split on blanks with single or double quotes grouping a word,
and nothing else: no escapes, variables, globs, substitutions or operators. The
result is an argv that is executed directly and never reaches a shell, so what the
line says is what runs.

The flags are registered by the same helper as the other provider flags, so `run`,
`resume`, `fork`, `counterfactual`, `bench`, `shadow` and `worker` all accept them.
`--agent-command` or `--agent-env` with any other provider is refused by name, and
so are `--record-cassette` and `--cassette` with `exec`: it makes no HTTP calls, so
there is nothing to record or serve.

The agent process is started once per turn. Nothing survives between turns except
the history it is sent, so a run can be resumed by a different process on a
different machine, and nothing of the run lives in the agent.

### The wire protocol, version 1

`twinwright.agent.v1`, documented in `docs/protocol/agent-v1.md` with JSON Schema
files for both documents.

- **Request.** One JSON object on stdin, then end of file: `protocol`, `task`,
  `history` (the messages as the ledger records them) and `operations`
  (`operation_id`, `method`, `path`, `description` and a JSON Schema for
  `parameters`, built from the compiled operation the way the hosted adapters build
  a tool definition).
- **Response.** One JSON object on stdout: `protocol`, `content` and `tool_calls`
  are required, `usage` is optional. A call needs `id`, `operation_id` and
  `arguments`.
- **Strict in what it reads.** Unknown fields (at any level), a missing or
  different `protocol`, a missing `content`, `tool_calls` or call field, data after
  the object, and a response with neither content nor calls are errors. A missing
  field is told from an empty one, so `tool_calls: []` is a final answer and an
  absent `tool_calls` is a mistake. The request is written from a goroutine, so an
  agent that never reads cannot block the turn.
- **Schemas are held to the code.** A test decodes the schema files as JSON and
  requires each `required` list to equal the non-`omitempty` JSON fields of the Go
  struct it describes, and each `properties` list to equal all of them. It then
  validates the request an agent really receives and the answers the helper agent
  really gives against the schemas, and checks that the response schema refuses six
  malformed documents.

### Bounds and process lifetime

- stdout is read through a 1 MiB limit. Past it the turn fails and the process is
  killed.
- stderr keeps its last 4 KiB. That tail goes into error text, which is what makes
  a crashed agent diagnosable, and it is bounded because an agent that logs without
  end must be harmless.
- The deadline is the provider timeout, 90 seconds by default. The agent is started
  in its own process group. On a timeout the group gets SIGTERM, then SIGKILL after
  500 ms whatever happened, and the child is always waited for. The turn returns a
  `timeout` interruption, so the runner's existing single retry applies and the
  attempt is recorded. Every other failure (a non-zero exit, malformed output, a
  protocol mismatch) is an ordinary provider error that is not retried: asking the
  same program the same question is not expected to change the answer.
- The process-group calls live in `internal/agent/exec_unix.go`, with a plain kill
  in `internal/agent/exec_other.go`, so the module still builds for Windows.

### Environment, secrets, and what is stored

The agent's environment is exactly `PATH` and `HOME` from the Twinwright process
plus the variables the operator names with the repeatable `--agent-env NAME`. The
value comes from the CLI's own environment. A `NAME=value` is refused, without
repeating the value, because a value typed on a command line is visible in process
listings and shell history, which is what passing a secret by environment variable
exists to avoid. A name this process does not have is an error, not a silent gap.

The passed values are masked in the stderr tail, in every error text, and in the
agent's own stdout before it is read, so an agent that echoes one back does not put
it in the ledger. This matches a literal value and its JSON-string form; it does
not match a transformed one. In the stderr tail the match is made on the text the
agent wrote, before the tail is quoted and before it is cut to 4 KiB, and it also
covers the Go quoted form.

The command line and the environment names are not stored. The ledger records the
provider name `exec` and the `--model`, and the turns, as for any provider. That
keeps the ledger what ADR 0029 says it is, the agent's trajectory: nothing about
the machine, the operator's setup or who executed a run enters it. The cost is that continuing an exec run, with `resume` or
a worker, needs `--agent-command` again, and the error names the flag.

### The agent proposes; the dispatcher decides

An exec agent is untrusted in the same way a model is. It never executes anything.
It returns tool calls, and they go through the runner's validation and the
dispatcher's authorization inside the tool transaction, as any provider's do. An
operation the principal's policy hides is not offered to the agent, and a call to
one anyway is answered 403 with nothing written. The agent never receives the
database address or any provider key unless the operator names that variable with
`--agent-env`.

### Container mode

`--agent-container IMAGE` runs the same process behind this command, built by
`ContainerOptions.Wrap` and asserted whole by a test:

```
docker run --rm -i --read-only --cap-drop ALL --security-opt no-new-privileges
  --user 65534:65534 --network <net> --memory <m> --cpus <c> --pids-limit <p>
  --tmpfs /tmp:rw,size=16m [-v <mount>:/agent:ro] [-e NAME ...] <image> <argv...>
```

The root filesystem is read-only with a 16 MiB `/tmp`, every capability is dropped,
privilege gain is off, and the user is `nobody`. The network defaults to `none`,
memory to 512m, CPUs to 1 and processes to 128 (`--agent-network`, `--agent-memory`,
`--agent-cpus`, `--agent-pids`). `--agent-mount DIR` makes a host directory visible
read-only at `/agent`; the CLI makes it absolute first, because docker reads a
relative host path as a volume name. `-e NAME` passes a variable from `--agent-env`
by name, and docker takes the value from the environment of the docker client,
which `ExecProvider.Env` supplies, so a value is never an argument.

`Validate` runs before anything starts. It refuses an empty image and anything that
is not an image reference, because the image sits among docker's own options and a
value of `--privileged` would otherwise become one; `host` as a network, by name,
because it removes the isolation this mode exists for; any other invalid network
name (the pattern already refuses `container:...`); a memory or CPU value that fails
the patterns a sidecar config is held to; fewer than one process; an environment
entry that is not a bare name; and a mount that is relative, missing, not a
directory, or contains a colon, which would end the host path early and let the
rest be read as the container path or the mode. The memory, CPU and network
patterns are the sidecar's, exported from `internal/container/limits.go` and
checked against `Parse` so the two cannot disagree.

### Where the helper agent lives

The end-to-end test must call `replay.Verify`, and `internal/agent` tests cannot
import `internal/replay`. The agent that tests run is therefore a non-test package,
`internal/agent/exectest`, which test binaries of several packages re-execute
themselves into through a `TestMain`. It does not import `internal/agent`: it
decodes the request with its own minimal types, so the protocol is a contract
between two independent readings of the document and a drift shows as a failing
test.

## What is proven

By tests that run on every `go test ./...`:

- A separate process completes the billing run across a pause and a resume, passes
  the evaluation, and the run replays. The standard-library Python agent does the
  same when `python3` is present.
- The fourteen misbehaviours in `TestExecProviderRejectsMisbehavingAgents` are
  errors, a hang is killed within its deadline with its process reaped, a child that
  ignores SIGTERM is killed with the group, and the stdout limit holds.
- The environment is exactly `HOME`, `PATH` and the passed names.
- An agent that asks for an operation its principal may not call is refused by the
  dispatcher.
- The container argv is exactly the one above, and each bad option is refused.

By tests that run only with `TWINWRIGHT_TEST_DOCKER=1` and `python:3.12-alpine`:
`TestPythonAgentInHardenedContainer` runs the reference agent through the hardened
command against a real daemon and requires the billing run to pass, and has an
agent in `--network none` try an outbound connection and requires that it did not
connect. This is CI evidence: the `docker` job of `.github/workflows/integration.yml`
pulls the image and runs it, and the coordinator records that run. It was also run
once on the development machine, against Docker Engine 29.9.2 in WSL2: both
subtests passed, the billing run in about 2.8 s and the probe answering `blocked:
[Errno 101] Network unreachable`. A control run by hand, which is not part of the
test, printed `connected` for the same probe on the default bridge network.

## Limits

- **`exec` alone is not a sandbox.** The agent runs with the operator's privileges.
  The environment is minimal; the filesystem and the network are not.
- **The Docker daemon is trusted.** Anything that can talk to it can undo the
  container.
- **No custom seccomp profile.** Docker's default applies. A network other than
  `none` is unrestricted, and Twinwright does not limit the network access an agent
  needs for its own model calls unless the operator chooses `--agent-network none`.
- **On macOS and Windows the boundary is the Docker Desktop VM**, not the host
  kernel.
- **A timeout may leave a container running.** On a timeout the docker client's
  process group is killed. Docker does not stop a container when its client dies,
  and a Python agent running as PID 1 ignores SIGTERM, so a hung containerized agent
  can keep running until its own process exits. No test covers this and it is not
  fixed here.
- **Masking is literal.** A secret that an agent encodes, splits or hashes before
  printing is not recognised.
- **Process groups are unix only.** Elsewhere the agent alone is killed, and
  anything it started is left to the operating system.
- **The hardening flags are asserted, not each observed.** The argv test holds that
  they are passed; the daemon test shows it accepts the command and that the
  network is off. Read-only root, dropped capabilities and the unprivileged user
  were looked at once by hand and are not in a test.
- **One scenario, one reference agent.** The Python agent solves `duplicate-charge`
  and is a protocol example, not a model.

## Rejected

**A long-lived agent process.** Keeping one process for a run saves a start per
turn, and ties the run to a process: it cannot be resumed elsewhere, a crash loses
state the ledger does not have, and a hang has no clean boundary. Per-turn start
makes the history the only state.

**Running the command through a shell.** It would allow pipelines and variables
and make the command line a place where an untrusted string becomes code.

**Accepting `NAME=value` for `--agent-env`.** Convenient, and it puts a secret in
the process list.

**Allowing `--network host`, with a warning.** A warning is not a boundary. The
option that removes the isolation is the option that cannot be chosen.

**A custom seccomp profile.** It would be a second, unreviewed policy to maintain
against Docker's own; the container mode claims what the flags above give and no
more.

## Consequences

- Any program that can read stdin and write stdout can be tested in a world, with
  its own libraries and runtime, and a run it takes part in replays without it.
- The cost of that is operational: resuming an exec run needs the command again, and
  a misconfigured agent shows as a provider error with its stderr tail.
- Container mode is opt-in and changes nothing for the other providers. It adds one
  CI step and one image pull to the `docker` job.
- The protocol is versioned by its `protocol` string. A change that an agent written
  for version 1 would misread gets a new string, and a runner that does not know the
  string an agent answers with refuses the response.

## Corrections

- 2026-10-28: the stderr tail was quoted before it was masked and cut to 4 KiB before it was masked, so a passed value holding a quote, a backslash or a control character, and the end of a value that the window cut in two, reached the error text and the ledger (EXEC-1); the tail is now masked, in the value's raw, JSON-escaped and Go-quoted spellings, before it is quoted and before it is cut, and the first partial line of a cut tail is dropped.
- 2026-10-28: a child environment with neither `PATH` nor `HOME` and no passed variable was a nil slice, which `os/exec` reads as "inherit the parent's environment", so the stated "exactly `PATH` and `HOME` plus the named variables" did not hold (EXEC-2); it is now an empty slice that stays empty.
