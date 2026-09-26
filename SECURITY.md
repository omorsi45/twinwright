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

The interesting boundaries are therefore internal, and they are enforced in code:

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

## Deployment notes

- **The metrics endpoint is unauthenticated** and off by default. It exposes
  queue depth, run counts and ownership topology. Bind it to loopback, or put it
  behind something that authenticates.
- **Container execution is experimental.** It runs a sidecar for a world and is
  not a sandbox for untrusted code. Do not rely on it to contain a hostile
  workload.
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
