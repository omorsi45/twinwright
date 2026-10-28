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
Where the operator names the provider (`run`, `bench`, `shadow`, `fork --agent`),
`--agent-command` or `--agent-env` with any other provider is refused by name. Where
the provider comes from the run's own record (`worker`, `resume`, and the run that
`fork` and `counterfactual` start from), the flags apply to a run whose provider is
`exec` and are not applied to any other, so a worker started with `--agent-command`
still serves the scripted and hosted runs in its queue. `--record-cassette` and
`--cassette` with `exec` are refused: it makes no HTTP calls, so there is nothing to
record or serve.

The agent process is started once per turn. It carries nothing between turns
except the history it is sent, so a run can be resumed by a different process on a
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
  are required, `usage` is optional but, when present, must carry all three counts.
  A call needs `id`, `operation_id` and `arguments`.
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
  killed. In container mode "the process" is the docker client, and the container
  is removed as well (see Container mode).
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
- Every turn ends by sending SIGKILL to the agent's process group once the agent
  has been waited for, on success and on every failure, so a process the agent
  started and left in its group does not outlive the turn. The group outlives its
  reaped leader while any member runs, which is why this reaches a helper the
  agent left behind. A process that left the group (`setsid`, a daemon) is not
  reached.
- On Linux the agent is started with a parent-death signal of SIGKILL, so it dies
  if Twinwright is killed or crashes mid-turn. The kernel ties that signal to the
  thread that started the child, so the starting goroutine is pinned to its
  thread for the length of the turn. This covers the direct child on Linux only:
  grandchildren of a SIGKILLed Twinwright, and every other platform, are not
  covered. `TestExecAgentDiesWithTheProcessThatStartedIt` kills a Twinwright
  mid-turn with SIGKILL and requires the agent to be gone.

### Environment, secrets, and what is stored

The agent's environment is exactly `PATH` and `HOME` from the Twinwright process
plus the variables the operator names with the repeatable `--agent-env NAME`. The
value comes from the CLI's own environment. A `NAME=value` is refused, without
repeating the value, because a value typed on a command line is visible in process
listings and shell history, which is what passing a secret by environment variable
exists to avoid. A name this process does not have is an error, not a silent gap.

The passed values are masked in the stderr tail, in every error text, and in the
strings of the agent's answer, so an agent that echoes one back does not put it in
the ledger. The answer is decoded first and only its decoded strings are masked
(the content, and the id, operation and argument names and values of each call, at
any depth), never its bytes: that keeps a value from breaking a valid document and
makes the agent's own escaping irrelevant. A value that the agent uses as an
argument is masked before the call is dispatched, so the ledger and the tool both
get the mask and the tool never receives the value. This matches a literal value,
its JSON-string form and its Go-quoted form; it does not match a transformed one,
and a value under eight characters is never masked. In the stderr tail the match is
made on the text the agent wrote, before the tail is quoted and before it is cut to
4 KiB.

The command line and the environment names are not stored. The ledger records the
provider name `exec` and the `--model`, and the turns, as for any provider. That
keeps the ledger what ADR 0029 says it is, the agent's trajectory: on a run that
succeeds, nothing about the machine, the operator's setup or who executed a run
enters it. An agent that cannot be started fails the run with the operating
system's reason ("no such file or directory") and not the command that was tried.
An agent that fails after it started is quoted from its own stderr, which is its
words and not Twinwright's: a traceback that prints its script path puts that path
in the error, and the masking covers the passed values and key-like tokens, not
paths. The cost is that continuing an exec run, with `resume` or
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
docker run --rm -i --name <tw-agent-random> --read-only --cap-drop ALL
  --security-opt no-new-privileges
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

Each turn's container is named `tw-agent-` and 16 random hexadecimal digits, fresh
on every `Wrap` call. Docker does not stop a container when its client dies, so
when a turn is ended by a timeout, a cancellation or an output overflow, and the
docker client's process group has been killed, `ContainerOptions.Cleanup` runs
`docker rm -f <name>`. It accepts only a name of that shape, so it can only remove
a container this package named. It is best effort and bounded to 10 seconds: a
failure is logged and the turn's own error is the one returned, and a container
that is already gone is not a failure. It does not run for a turn that ended by
itself, where `--rm` removes the container, and it cannot run if Twinwright is
killed mid-turn. The name does not include the run or the turn: a provider is not
told either.

The docker client finds its daemon through its own environment, so it is given
`DOCKER_HOST`, `DOCKER_CONTEXT`, `DOCKER_CONFIG`, `DOCKER_CERT_PATH` and
`DOCKER_TLS_VERIFY` from the Twinwright process when they are set
(`ExecProvider.ClientEnv`). Those go to the client and never into the container:
the container gets only the names `--agent-env` gave. Their values are masked in
error text like the passed values.

`Validate` runs before anything starts. It refuses an empty image and anything that
is not an image reference, because the image sits among docker's own options and a
value of `--privileged` would otherwise become one; `host` as a network, by name,
because it removes the isolation this mode exists for; any other invalid network
name (the pattern already refuses `container:...`); a memory or CPU value that fails
the patterns a sidecar config is held to, or that is zero, which docker reads as no
limit (checked on Docker Engine 29.9.2: `--memory 0`, `0b`, `0m`, `00` and `--cpus 0`,
`0.0` all leave the cgroup limit at `max`); fewer than one process; an environment
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
- The seventeen misbehaviours in `TestExecProviderRejectsMisbehavingAgents` are
  errors, a hang is killed within its deadline with its process reaped, a child that
  ignores SIGTERM is killed with the group, and the stdout limit holds.
- The environment is exactly `HOME`, `PATH` and the passed names, and empty when
  there are none to pass.
- A process the agent leaves running in its group is killed when the turn ends,
  after a clean exit and after the failure where it holds the output open; on
  Linux a Twinwright killed mid-turn takes the agent with it.
- An agent that asks for an operation its principal may not call is refused by the
  dispatcher.
- The container argv is exactly the one above, and each bad option is refused.

By tests that run only with `TWINWRIGHT_TEST_DOCKER=1` and `python:3.12-alpine`:
`TestPythonAgentInHardenedContainer` runs the reference agent through the hardened
command against a real daemon and requires the billing run to pass, and has an
agent in `--network none` try an outbound connection and requires that it was
blocked with errno 101 (network unreachable), which a firewall that drops or
refuses packets would not give. A third subtest is the control: the same probe on
the `bridge` network must connect, and it skips with a logged reason on a runner
with no outbound network, since the isolation test then has nothing to be compared
with. `TestHardenedContainerIsGoneAfterOverflowAndTimeout` runs a hanging agent and
a flooding agent through the hardened command, requires the container to be gone
when the turn returns, and has a control without the removal showing that the
container is still running after its client was killed. This is CI evidence: the
`docker` job of `.github/workflows/integration.yml` pulls the image and runs them,
and the coordinator records that run. The job requires the passes by name in the
`-v` output, so a renamed test, a missing image or an image check that failed does
not leave a green step. They were also run on the development machine, against
Docker Engine 29.9.2 in WSL2: every subtest passed, the billing run in about 3 s,
the isolation probe answering `blocked: [Errno 101] Network unreachable`, the same
probe answering `connected` on the bridge network, and both containers running after
their client was killed without the removal and absent with it.

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
- **A container is removed only if Twinwright is alive to remove it.** On a timeout,
  a cancellation or an output overflow the docker client's process group is killed
  and then `docker rm -f` is run for the turn's container, because Docker does not
  stop a container when its client dies and a Python agent running as PID 1 ignores
  the SIGTERM the client proxies. If Twinwright is killed mid-turn, the client dies
  with it and the container is left running until its own process exits; the
  parent-death signal covers the client, not the container. The removal is best
  effort and its failure is only logged.
- **Masking is literal.** A secret that an agent encodes, splits or hashes before
  printing is not recognised. A value of fewer than eight characters is not masked
  at all.
- **Process groups are unix only.** Elsewhere the agent alone is killed, and
  anything it started is left to the operating system. On unix a process that
  leaves the agent's group is not killed with it, and the parent-death signal is
  Linux only and covers the direct child.
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
- 2026-10-28: "nothing survives between turns" was false for what an agent started: the process group was signalled only on a timeout or an output overflow, so a helper left running by a turn that succeeded, or by one that failed because it held the output open, outlived it (EXEC-3). Every turn now ends by killing the group (unix), and on Linux the agent also gets a parent-death SIGKILL so it does not outlive a Twinwright that dies mid-turn (EXEC-12); the claim is now stated as "carries nothing between turns except the history", and the cases it does not cover (a process that left the group, grandchildren of a killed Twinwright, non-Linux platforms) are listed under Bounds and Limits.
- 2026-10-28: the passed values were masked in the agent's stdout bytes before the response was decoded, so a value that occurs as the digits of a number made a valid response invalid and blamed the agent, one inside an argument rewrote the call, and a value the agent encoded its own way (`\u00e4` as Python writes it, a raw `&`) was not recognised although this ADR said the JSON-string form was matched (EXEC-4, EXEC-8); the response is now decoded first and only its decoded strings are masked, and the sentences above state what is matched, the 8-character floor and the effect on arguments.
- 2026-10-28: "`--agent-command` or `--agent-env` with any other provider is refused" made a worker started with the agent flags fail every scripted or hosted run it claimed, so one fleet could not serve exec runs and others from one queue (EXEC-5); the refusal now holds where the operator names the provider (`run`, `bench`, `shadow`, `fork --agent`), and where the provider comes from the run's record (`worker`, `resume`, and the run `fork` and `counterfactual` start from) the agent flags apply to exec runs only.
- 2026-10-28: the stdout limit and the deadline killed only the docker client, so in container mode the container kept running and writing, and a timed out turn left one behind that its retry then duplicated, although "the process is killed" was stated without that qualification (EXEC-7); each turn's container is now named and `docker rm -f` is run for it after the client is killed, the Limit about timeouts is rewritten, and a Docker test shows the container running without the removal and gone with it.
- 2026-10-28: `--agent-memory 0` and `--agent-cpus 0` passed `Validate` and, on Docker Engine 29.9.2, leave the memory and CPU limits at `max` (EXEC-9); both are refused, with every spelling of zero.
- 2026-10-28: the docker client was given only `PATH` and `HOME`, so a daemon configured through `DOCKER_HOST` and its relatives was unreachable, and the only workaround, `--agent-env DOCKER_HOST`, also put the daemon address in the untrusted container (EXEC-14); the five connection variables now go to the client process only.
- 2026-10-28: the Docker evidence for container mode could pass without running: a renamed test matched nothing, a failed image check skipped, and the network probe accepted any "blocked" answer (EXEC-13); the CI step requires the passes by name, the isolation test requires errno 101, and a bridge-network control shows the probe can connect.
- 2026-10-28: the decoder accepted a `usage` that was `null`, `{}` or missing counts and recorded zeros, although the response schema requires an object with all three (EXEC-10); a `usage` that is there is now held to the schema.
- 2026-10-28: "the ledger records nothing about how the agent was started" was false for a failed turn: the error for an agent that could not be started carried the command's path (`fork/exec /home/operator/...`), and a traceback in the stderr tail can carry a script path (EXEC-11); a start failure now gives the operating system's reason without the command, and the sentence above says what holds for a run that succeeds and what a failed agent's own stderr can still contain.
