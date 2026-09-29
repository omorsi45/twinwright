# ADR 0022: Shadow observation connectors

Status: accepted, 2026-09-28; extends ADR 0015

ADR 0015 shipped shadow mode as observe-only with a single hardcoded source: a
JSONL file in Twinwright's own observation shape, under `examples/`. That was the
right first step, and it is not a boundary. It means the only way to shadow an
agent against real recorded behaviour is to hand-transform an export into
Twinwright's shape first, which is where fidelity quietly gets lost and where a
reviewer loses the ability to check that the comparison saw what the source said.

This ADR adds the adaptor layer: `Connector`, a registry, and three
implementations.

## The boundary

A connector does one thing: turn recorded bytes into `[]Observation`. It is
deliberately the narrowest useful interface - name, description, decode - and
decoding is pure. A connector opens no socket, resolves no host, holds no
credential and executes nothing.

That purity is the security property, not a style preference. Shadow mode exists
to evaluate an agent against behaviour recorded elsewhere, so its input is by
definition data someone else produced. If decoding could reach the network, a
crafted observation file would turn shadow mode into a request forwarder running
inside the operator's trust boundary. Keeping decode a pure function of bytes is
what makes that impossible rather than merely discouraged.

The transport stays what ADR 0015 made it: a file resolved through `confinePath`
under the examples root. Confinement is now enforced in `Load` as well as in
`Parse`, because `Load` accepts a `SourceConfig` a caller can construct
directly, and a guard that only runs on the config-file path has a hole in it.

Sources are size-capped at 8 MiB. A shadow source is someone else's export, so
its size is not this project's choice, and reading it whole is what every
connector wants; the cap is what turns an oversized dump into an error instead of
memory pressure.

## The three connectors

- `file` / `jsonl` - Twinwright's native observation shape. Registered under both
  names: `file` is what existing configs already say, `jsonl` is what it is.
- `audit_log` - a sanitized audit export, where the operation is an `action` and
  its inputs are `parameters`. This is the shape compliance and admin logs
  usually arrive in.
- `recorded_http` - captured HTTP interactions, with method, path, query and
  body.

Three rather than one is the point. A boundary with a single implementation is an
interface someone hopes is general; three force the normalisation to be real, and
a test asserts it: the same support session recorded three ways decodes to
byte-identical `(operation_id, arguments)` pairs.

### Two decisions inside the connectors worth stating

**An operation ID is required, never inferred.** `recorded_http` will not guess
the operation from method and path. A recorder that captured the call knew which
operation it was; deriving it here from a path pattern would silently
mis-attribute an action, and a report that names the wrong operation is worse
than one that refuses to load.

**An attempt that did not take effect is not an observed action.** A denied audit
entry and a 4xx or 5xx HTTP response are dropped. They are evidence that a human
tried something and was stopped, and counting them as observed behaviour would
make the comparison accuse the agent of missing a step that never happened. The
`outcome` and `status` fields are how each connector recognises this.

**Unknown fields are rejected.** Lenient decoding is the dangerous default for a
shadow source: a misspelled key would drop real observed behaviour, the
observation would load with empty arguments, and the report would look clean
because the evidence never arrived. Strict decoding turns a silent hole into a
loud failure at a named line number.

## Comparison determinism, fixed here

`Compare` assembled its matched and residual lists by ranging over maps keyed on
operation and canonical arguments, so Go's randomised map iteration decided the
order of a report. Two comparisons over byte-identical input disagreed on
ordering, which makes a report impossible to diff between runs or commit as a
fixture. Every other package in the runtime that returns a collection sorts for
exactly this reason; shadow was the exception. Output is now sorted by operation
and then canonical arguments.

The test repeats the comparison 200 times and compares against the first result.
It fails against the previous implementation on the first repetition.

## Deliberately not implemented, with reasons

- **A webhook or event-stream transport.** The roadmap lists it, and it is not a
  fourth registry entry. It means a listening socket accepting unauthenticated
  input, which is a materially different security posture from reading a file and
  needs its own threat model and ADR. Adding it as a convenience alongside three
  file adaptors would smuggle in an attack surface under the heading of a format
  change.
- **Twinwright's own exported traces as a source.** An obvious connector to want,
  and the reason to wait is concrete: the trace format is owned by
  `internal/trace`, and pinning a second consumer to it now would freeze a shape
  that is still moving.
- **Write mode.** Unchanged from ADR 0015. `allow_writes: true` is still
  rejected, and a write-capable shadow path still needs its own ADR.

## No live external integration has been tested

Every test in this package runs against fixture bytes. No connector here has been
pointed at a live audit system, a real HTTP recorder, or any external service,
and the CLI's shadow output carries `live_external: false` so a report cannot be
mistaken for evidence of one. The connectors are structured so that a live
ingestion path would be a separate transport feeding the same decode functions,
which is the part that is designed rather than demonstrated.

## Testing

Adversarial tests cover: cross-connector parity on one recorded session; denied
and failed attempts excluded; a misspelled field rejected for all three
connectors; `recorded_http` refusing a record with no operation ID; an unknown
connector name rejected with the available names listed; path traversal and
absolute paths refused by every connector; an oversized source refused; and
comparison determinism across 200 repetitions.

Two checks were mutation-tested by deliberately removing them. Removing
`DisallowUnknownFields` fails the misspelled-field test for all three connectors.
Removing confinement from `Load` fails the traversal test - but only after that
test was strengthened. Its first version pointed at `go.mod` and asserted merely
that an error occurred, so it passed with the guard removed, because reading
`go.mod` fails at JSON parsing. The test now places valid JSONL outside the root,
proves the same bytes load from inside it, and requires the refusal to name the
escape. With confinement removed it now reports reading the outside file and
returning its contents.
