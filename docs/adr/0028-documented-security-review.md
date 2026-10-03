# 0028. A security review that is evidence, and a test that keeps it honest

Date: 2026-10-02

Status: accepted

## Context

`SECURITY.md` existed and was doing its job: where to report a vulnerability, what
Twinwright is for threat-modelling purposes, four deployment notes, two
out-of-scope items. What it is not, and was never meant to be, is a record of
anyone having looked.

There was no review anywhere under `docs/`: nothing matching secur, audit, review
or threat, and no ADR. So the policy existed and the pass did not, and
`SECURITY.md` carried six positive claims with nothing saying which test holds
each.

Two of those claims turned out to be weaker than they read.

- "Provider errors are recorded without credentials" is a claim about the ledger.
  The provider adapters do redact, but the only tests asserted on the returned
  error and on `Message.RawBody`; the store redacts nothing, and no test opened a
  ledger and looked. `TestBuildFailedProviderTurnIsRedacted` actually demonstrates
  the opposite case working as designed: a raw bearer token reaches the ledger and
  is masked only when a trace is built.
- "The one place an identifier is interpolated ... is validated against a
  plain-identifier pattern first" was true of the code and untested. The regex
  existed, the comment explained why it mattered, and `postgres_test.go` never
  supplied a hostile `search_path`.

## Decision

`docs/security-review.md` is the review, written as evidence rather than
reassurance. Seven surfaces, and for each one: what was examined, what was found,
and the file and test that hold each property. A claim with no test is written down
as a known limit under the surface it belongs to rather than collected into a
footnote nobody reads.

Three choices inside that shape are worth recording.

**Citations are to tests, with what the test asserts.** Naming a test is cheap and
a reader cannot tell a real citation from a plausible one. Each citation therefore
says in a clause what the test actually does, which is also what makes a stale
citation visible when someone reads the test.

**Gaps that contradict an existing claim get a test; the rest get a sentence.**
Both of the claims above are now held:
`internal/agent/redaction_ledger_test.go` runs a real provider against a server
that echoes the configured key into a 401, executes the run so the failure path
writes, and asserts no event payload and no stored transcript carries the key.
`internal/store/schema_identifier_test.go` refuses seven hostile `search_path`
values with legal controls beside them. A third, smaller gap -
`internal/container/env_file_test.go` - covers a traversal written in the
unnormalised form the existing cases had already normalised away. Everything else
is recorded as a limit, because a stated absence is not a defect and padding the
test suite to make a document look better would be the wrong trade.

**Properties held by structure are named as such.** The shadow connector's "opens
no socket" and "nothing is executed" are true by inspection of the current code and
no test would fail if a decoder dialled. Saying so is more useful than either
pretending a fixture-bytes test covers it or inventing an import-level assertion
nobody asked for.

The review is gated. `cmd/twinwright/security_review_test.go` requires every cited
file to exist, every cited test name to be a real test function in this
repository, and every surface to be present, and each check fails outright if it
parses nothing - the same shape as `cmd/twinwright/changelog_test.go`, for the same
reason: a document full of claims about the build should fail the build when it
stops being true.

## Consequences

Renaming a cited test now breaks CI. That is the intended cost: the alternative is
a review that quietly stops pointing at anything.

What the gate cannot check is the part that matters most - whether a cited test
still asserts what the sentence beside it claims. No mechanical check can, which is
why the document says so in its own closing section and dates itself. A review that
was accurate on 2026-10-02 is not evidence about a later build.

`SECURITY.md` keeps its job and gains one pointer to the review. The two documents
are deliberately different kinds of thing: a policy a reporter reads, and a record
a maintainer re-establishes.

One finding is worth carrying forward in its own right. The container surface
enforces less than the word "hardening" in ADR 0024 suggests: the label, the
explicitly configured network, and the memory, cpu and pids limits. `Start` passes
no `--read-only`, no `--cap-drop`, no `--security-opt no-new-privileges`, no
`--user` and no seccomp profile, and does not force `--network none`. That is
consistent with `SECURITY.md`'s "not a sandbox for untrusted code" rather than a
contradiction of it, but the review states the specific flags rather than leaving a
reader to infer which ones. Adding any of them is a change to what the feature
claims and needs its own ADR.
