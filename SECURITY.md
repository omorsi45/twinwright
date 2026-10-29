# Security

## Reporting a vulnerability

Report privately through GitHub's [Report a vulnerability](https://github.com/omorsi45/twinwright/security/advisories/new)
form rather than opening a public issue. Include what you did, what happened, and
what you expected. Expect an acknowledgement within a week.

## What Twinwright is, for threat-modelling purposes

Twinwright runs **simulated** services against which an agent is tested. The
worlds are fictional: the billing, CRM, ticketing and messaging services are
in-process handlers over a local database. Twinwright does not call a customer's
real systems, and shadow mode is observe-only by design - a configuration that
asks for production writes is rejected rather than honoured.

The interesting boundaries are therefore internal, and they are enforced in code.
`docs/security-review.md` is the review pass over each of them: what was examined,
what was found, and the test that holds each claim below, including the places
where no test holds one.

**Authorization.** Principal policies are evaluated in the tool transaction,
before any handler runs, and the decision is recorded in the ledger. Enforcement
never depends on prompt text, because an agent's prompt is exactly what an
attacker gets to influence.

**Prompt injection.** Injected instructions reaching the agent through retrieved
data (a ticket comment, for instance) cannot widen its permissions: the
authorization check does not read the transcript. A scenario in
`examples/security/` exercises this, and the security report distinguishes an
*attempted* boundary crossing from a *successful* one.

**Ownership.** In distributed mode a worker cannot commit to a run it no longer
owns. The fence check happens inside the same transaction as the write, so a
stalled worker cannot pass the check and then write.

**Secret redaction.** Configured secrets and provider-key patterns are redacted
from traces, including from error bodies that echo a key back. Provider errors
are recorded without credentials.

**Replay isolation.** Replay reconstructs into a throwaway local database and
opens the source read-only. On PostgreSQL that is
`default_transaction_read_only=on`, so the server refuses a write rather than
this process merely intending not to attempt one.

**Identifier handling.** All runtime SQL uses bind parameters. The one place an
identifier is interpolated is the PostgreSQL `search_path` schema name in
`CREATE SCHEMA`, which cannot be a bind parameter; it is validated against a
plain-identifier pattern first.

**Agent processes.** `--agent exec` starts a program of yours once per turn. That
program is not trusted: it can only propose tool calls, and the dispatcher
authorizes and executes them, so the run's policy holds whatever it proposes. Its
environment is `PATH`, `HOME` and the variables named with `--agent-env`, so it
never receives the database address or a provider key unless you name that
variable, and `NAME=value` is refused so no secret is typed on a command line.
Without `--agent-container` it runs with your privileges and Twinwright does not
stop it reading files or opening connections. With `--agent-container IMAGE` it
runs under a read-only root, with all capabilities dropped, no-new-privileges, a
non-root user, resource caps and no network by default. The limits of that mode:
the Docker daemon is trusted, there is no custom seccomp profile, a network you
allow is unrestricted, on macOS and Windows the boundary is the Docker Desktop VM,
and a container is left running if Twinwright itself is killed mid-turn (a turn that
times out, is cancelled or floods its output has its container removed). Section 7 of `docs/security-review.md` has the
test behind each claim, and ADR 0033 records the decision.

**Recordings.** `run --record-cassette` writes the provider's HTTP exchanges to a
file. Request headers are never written, a URL loses its userinfo and query, and
the configured key is masked in both bodies. The file still holds the prompts and
the model's answers in full, and only the key you configured is masked, so
**do not commit a cassette or a live-model report**. `*.cassette.jsonl` and
`live-model-report.json` are in `.gitignore`. ADR 0031 records the decision.

## Deployment notes

- **The metrics endpoint is unauthenticated** and off by default. It exposes
  queue depth, run counts and ownership topology. Bind it to loopback, or put it
  behind something that authenticates.
- **The observability stack has no TLS and no authentication.** The collector,
  Jaeger and Prometheus in `docker-compose.yml` are for development: Compose
  publishes every port on loopback only, so they are not reachable from other
  machines. Do not expose them. Trace export is best effort and its failures are
  logged with credentials scrubbed. ADR 0032 records the decision.
- **`make demo-stack` opens two listeners on all interfaces while it runs.** Its
  workers serve metrics on `0.0.0.0:9095` and `0.0.0.0:9096`, without
  authentication, because the Prometheus container reaches the host through the
  bridge gateway and a loopback listener does not answer there. They exist only
  while the demo runs. Do not run it on a machine whose network you do not trust.
- **Container execution of world sidecars is experimental.** It runs a sidecar for
  a world and is not a sandbox for untrusted code. Do not rely on it to contain a
  hostile workload. Agent containers (`--agent-container`) are covered under
  "Agent processes" above.
- **The PostgreSQL DSN carries credentials.** Pass it through the environment or
  a secret manager, not a shell history.
- **Provider API keys** are only needed for live-provider runs. Every
  deterministic example, fixture and benchmark case runs on scripted providers
  with no credentials at all.

## Out of scope

- The fictional world services are not hardened against their own simulated
  data; they are test fixtures.
- Twinwright makes no claim about the safety of an agent you test with it. That
  is what the assertions, chaos policies and security scenarios are for.
