# Agent protocol, version 1

`--agent exec` lets a program of your own make the agent's decisions. Twinwright
starts it for every turn, tells it the task, the history so far and the
operations it may call, and reads back either calls it proposes or a final
answer. The protocol is `twinwright.agent.v1`. A working example is
`examples/agents/python/billing_agent.py`; it is a protocol example, not a model.

```sh
twinwright run duplicate-charge \
  --agent exec \
  --model billing-agent@1 \
  --agent-command 'python3 examples/agents/python/billing_agent.py' \
  --agent-env MY_AGENT_TOKEN \
  --fail-on-eval
```

| Flag | Meaning |
|---|---|
| `--agent exec` | use an agent process |
| `--model NAME` | required. Names the agent and its version (for example `billing-agent@1`); it is what the run records about who decided |
| `--agent-command LINE` | required. How to start the agent, split into arguments without a shell (below) |
| `--agent-env NAME` | repeatable. Pass the CLI's own value of `NAME` to the agent. A `NAME=value` is refused so that no value is typed on a command line |
| `--provider-timeout D` | the deadline of one turn, default 90 seconds |

The flags are accepted by every command that can reach a provider (`run`,
`resume`, `fork`, `counterfactual`, `bench`, `shadow`, `worker`). Where the
operator names the provider (`run`, `bench`, `shadow`, and `fork --agent`),
`--agent-command` and `--agent-env` with any provider other than `exec` are
refused. Where the provider comes from the run's own record (`worker`, `resume`,
and `fork` and `counterfactual` for the run they start from), the agent flags apply
to the runs whose provider is `exec` and are not applied to the others, so one
worker started with `--agent-command` serves scripted, hosted and exec runs from
the same queue. `--record-cassette` and `--cassette` are refused with `exec`, which
makes no HTTP calls.

## Starting the agent

`--agent-command` is split on blanks. A single or double quoted span is part of
one word and the quotes are removed; text and quoted spans that touch make one
word. There are no escapes, variables, globs, substitutions or operators, and the
result is executed directly, never through a shell. An unterminated quote or an
empty command is an error.

The agent is started once per turn and carries nothing between turns except the
history it is sent. Its environment is exactly `PATH` and `HOME` from the
Twinwright process, plus the variables named with `--agent-env`. Provider keys,
database addresses and the rest of the operator's environment are not passed. If
Twinwright has neither `PATH` nor `HOME` and nothing is named, the environment is
empty, not inherited.
Its working directory is the one Twinwright was started in.

## Framing

One request, one response.

1. Twinwright writes **one JSON object** to the agent's stdin and closes stdin.
   The write does not block the turn: an agent that never reads its input is
   not waited on for it.
2. The agent writes **one JSON object** to stdout and exits with status 0.
   Whitespace around the object is fine. Anything after it is an error.
3. stderr is free for diagnostics.

## Request

Schema: [`agent-v1-request.schema.json`](agent-v1-request.schema.json).

```json
{
  "protocol": "twinwright.agent.v1",
  "task": "Customer C-104 was charged twice. Refund only the duplicate.",
  "history": [],
  "operations": [
    {
      "operation_id": "getCustomer",
      "method": "GET",
      "path": "/customers/{id}",
      "description": "Get a customer by ID.",
      "parameters": {
        "type": "object",
        "properties": { "id": { "type": "string" } },
        "required": ["id"],
        "additionalProperties": false
      }
    }
  ]
}
```

- `history` is the conversation as the run's ledger records it: the agent's own
  earlier turns, and one `tool` message per executed call with the call's
  `call_id`, `operation_id`, HTTP `status` and response body as `content`. It is
  `[]` on the first turn.
- `operations` lists only what the run's policy exposes. `parameters` is a JSON
  Schema object, the same one the hosted-model adapters send as a tool definition.
- An agent should ignore request fields it does not recognise.

## Response

Schema: [`agent-v1-response.schema.json`](agent-v1-response.schema.json).

```json
{
  "protocol": "twinwright.agent.v1",
  "content": "",
  "tool_calls": [
    { "id": "step-1", "operation_id": "getCustomer", "arguments": { "id": "C-104" } }
  ]
}
```

- `protocol`, `content` and `tool_calls` are required. `usage` (`input_tokens`,
  `output_tokens`, `total_tokens`) is optional and is recorded with the turn.
- With one or more `tool_calls` the calls are proposed. The run executes them and
  the next turn is the agent started again with their results in `history`.
- With `tool_calls: []` the turn is the final answer and `content` is it. The run
  ends.
- A response with an empty `content` and no tool calls is refused.
- Each call's `id` must be unique within the run: the runner refuses an id that an
  earlier turn already used. `operation_id` must be one of the request's
  operations.
- Unknown fields are refused, in the response and in each call and in `usage`.

## Errors and limits

All of these are enforced by the provider and covered by tests in
`internal/agent/exec_test.go`.

| Condition | Result |
|---|---|
| Exit status other than 0 | provider error naming the status, with the last 4 KiB of stderr |
| stdout is empty, is not one JSON object, has data after the object, has an unknown field, or lacks `protocol`, `content` or `tool_calls` | provider error |
| `protocol` is missing or is not `twinwright.agent.v1` | provider error (protocol mismatch) |
| stdout over 1 MiB (1,048,576 bytes) | provider error, and the process is killed |
| No answer within the turn deadline | the process group gets SIGTERM, then SIGKILL after 500 ms, and the turn is recorded as an interrupted attempt of kind `timeout` |
| The command cannot be started | provider error |

A timeout is the only failure the runner retries: it makes up to two attempts at
a turn and records each interrupted one. Every other failure ends the run as
`failed`, with the error in the ledger.

On unix the agent runs in a process group of its own, and every turn ends by
killing that group, whether the turn succeeded, failed or timed out (a timeout
sends SIGTERM first). A process the agent started and left in its group does not
outlive the turn, and the child is always waited for, so no zombie is left. What
this does not reach: a process that left the group (`setsid`, a daemon, a double
fork into a new session), and on platforms other than unix anything the agent
started, since there only the agent itself is killed.

If Twinwright itself dies mid-turn, the agent is killed too on Linux, where the
kernel is asked to send it SIGKILL when its parent goes. That covers the agent
process only: what the agent started is not reached when Twinwright is killed
rather than stopped, and on other platforms a hung agent outlives a dead
Twinwright.

stderr is kept to its last 4 KiB. The error text, including that tail, is
redacted: the values of the variables passed with `--agent-env` are masked, as are
key-like tokens. A passed value is masked in the tail as the agent wrote it, as a
JSON string and as a Go quoted string, before the tail is quoted for the error text
and before it is cut to 4 KiB, so a value holding a quote, a backslash or a newline
is masked, and so is one that the 4 KiB window would have cut in two. When stderr is
longer than the window, the tail starts at a line boundary: the first, partial line
is dropped.

Passed values are also masked in what the agent answers, after the response is
decoded and not in its bytes: in `content` and in every string of each call (the
`id`, the `operation_id`, and the names and values of `arguments` at any depth).
Numbers, booleans and `null` are not strings and are never masked, so a passed
value cannot make a valid response invalid, and the agent's own escaping (`\u00e4`
for `ä` as Python's `json.dumps` writes it, `\"`, `\/`) does not matter. The
masking is done before the runner records or dispatches a call, so a passed value
that the agent uses as an argument is recorded in the ledger as `[REDACTED]` and the
tool receives `[REDACTED]` too, never the value: that changes what the tool
receives, and it is the only way the tool does not receive the secret.

A passed value shorter than 8 characters is never masked, in stderr or in the
answer, so that ordinary words are not; do not pass a secret that short.

## Trust boundary

The agent **proposes** calls. It does not execute them. Every proposed call goes
through the same dispatcher as a call from any other provider: the principal's
policy authorizes it inside the tool transaction, and only then does a handler
run. An agent that ignores the policy, or that was talked into something by the
content of a simulated service, gets the same denial a model would.

The agent never receives the database address or any provider key unless the
operator names it with `--agent-env`. It reads and writes only its own stdin,
stdout and stderr, and whatever the operating system lets its process reach: by
itself `exec` is not a sandbox. Container mode puts the process in a hardened
container.

## Container mode

`--agent-container IMAGE` runs the agent command inside a hardened `docker run`
instead of directly. The image must already contain what the command needs; the
command line is then the command inside the container.

| Flag | Default | Meaning |
|---|---|---|
| `--agent-container IMAGE` | none | run the agent in this image |
| `--agent-network` | `none` | docker network; `host` is refused |
| `--agent-memory` | `512m` | memory limit |
| `--agent-cpus` | `1` | CPU limit |
| `--agent-pids` | `128` | process count limit |
| `--agent-mount DIR` | none | host directory mounted read-only at `/agent`; made absolute by the CLI |

The network, memory, CPU and process flags apply only with `--agent-container`;
`--agent-container` with a provider other than `exec` is refused, and so is
`--agent-mount` without a container. Variables named with `--agent-env` are passed
into the container by name.

```sh
twinwright run duplicate-charge --agent exec --model billing-agent@1 \
  --agent-container python:3.12-alpine \
  --agent-mount examples/agents/python \
  --agent-command 'python3 /agent/billing_agent.py' \
  --fail-on-eval
```

The container runs with a read-only root filesystem (plus a 16 MiB `/tmp`), every
capability dropped, privilege gain off and the user `65534`. The files you mount
must be readable by that user. What this does not do: the Docker daemon is trusted,
there is no custom seccomp profile, a network other than `none` is unrestricted, on
macOS and Windows the boundary is the Docker Desktop VM, and a timeout kills the
docker client but Docker does not stop a container when its client dies, so a hung
agent's container can keep running until its process exits. See
`docs/adr/0033-agent-protocol-and-isolation.md`.

## What a run records

The run records the provider name `exec`, the `--model` you gave, and the turns,
exactly as for any other provider. It does **not** record the command, its
arguments or the environment names. Replay verifies a run from its recorded turns
and needs no agent. Continuing an exec run, with `resume` or a `worker`, needs
`--agent-command` again, and an exec run started elsewhere fails with an error
that names that flag.

## Versioning

`protocol` carries the version. A change that an agent written for version 1 would
misread gets a new version string, and a runner that does not know the string an
agent answers with refuses the response instead of guessing.
