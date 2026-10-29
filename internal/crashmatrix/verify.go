package crashmatrix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"twinwright/internal/checkpoint"
	"twinwright/internal/dispatch"
	"twinwright/internal/replay"
	"twinwright/internal/store"
	"twinwright/internal/worker"
)

// Result is the verdict for one crashed-and-recovered case. It carries the
// measurements as well as the verdict so a failing case can be diagnosed from
// the report alone.
type Result struct {
	Case                      Case     `json:"case"`
	RunID                     string   `json:"run_id"`
	Crashed                   bool     `json:"crashed"`
	CrashedFence              int64    `json:"crashed_fence"`
	RecoveryFence             int64    `json:"recovery_fence"`
	Takeover                  bool     `json:"takeover"`
	Deliveries                int      `json:"deliveries"`
	Mutations                 int      `json:"mutations"`
	DuplicateCallIDs          []string `json:"duplicate_call_ids,omitempty"`
	ReplayVerified            bool     `json:"replay_verified"`
	CheckpointsReconstructed  int      `json:"checkpoints_reconstructed"`
	StaleNewCallRejected      bool     `json:"stale_new_call_rejected"`
	StaleExistingCallRejected bool     `json:"stale_existing_call_rejected"`
	Violations                []string `json:"violations,omitempty"`
}

// Passed reports that every check held.
func (r Result) Passed() bool { return len(r.Violations) == 0 }

// worldTables are the per-world tables in the base schema: every table that
// carries a world_id. Their row counts are the world's whole observable state, so
// comparing them against the reference catches an effect that was repeated, lost,
// or landed in the wrong place, including effects no judge looks at.
var worldTables = []string{
	"customers", "invoices", "charges", "refunds", "subscriptions",
	"crm_accounts", "crm_contacts", "crm_notes",
	"ticket_projects", "ticket_issues", "ticket_comments",
	"message_workspaces", "message_channels", "message_members", "message_messages",
}

// worldRows counts each per-world table's rows for one world. The table names
// come from the fixed list above, never from input.
func worldRows(ctx context.Context, s *store.Store, worldID string) (map[string]int, error) {
	rows := make(map[string]int, len(worldTables))
	for _, table := range worldTables {
		var n int
		if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE world_id=?", worldID).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		rows[table] = n
	}
	return rows, nil
}

// mutations is the agent's own committed state changes, by tool call.
type mutations struct {
	total      int
	byCall     map[string]int
	unattached int
}

// attributeMutations walks the ledger in order and charges each state.mutation to
// the tool call whose request most recently opened. A chaos actor's write is a
// different event type and is not counted: it is the world changing under the
// agent, not the agent acting, and a correct recovery cannot control it.
func attributeMutations(events []store.Event) mutations {
	m := mutations{byCall: map[string]int{}}
	current := ""
	for _, event := range events {
		switch event.Type {
		case "tool.request":
			var request struct {
				CallID string `json:"call_id"`
			}
			if json.Unmarshal(event.Payload, &request) == nil {
				current = request.CallID
			}
		case "state.mutation":
			m.total++
			if current == "" {
				m.unattached++
				continue
			}
			m.byCall[current]++
		}
	}
	return m
}

func (m mutations) duplicates() []string {
	var ids []string
	for id, n := range m.byCall {
		if n > 1 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// Verify checks the recovery invariants of one crashed-and-recovered run and
// reports each broken one as a violation. ref may be nil, which skips the checks
// that compare against an uncrashed run.
//
// Crashed is true for after-completion too: that point is simulated by the
// harness rather than reached in code, but the death it models did happen.
func Verify(ctx context.Context, s *store.Store, t Target, ref *Reference, runID string, crash CrashOutcome, outcome worker.Outcome) Result {
	result := Result{
		RunID: runID, Crashed: crash.Crashed, CrashedFence: crash.Doomed.Token,
		RecoveryFence: outcome.Fence, Takeover: outcome.Takeover, Deliveries: outcome.Attempt,
	}
	fail := func(format string, args ...any) {
		result.Violations = append(result.Violations, fmt.Sprintf(format, args...))
	}

	if !crash.Crashed {
		fail("the crash point was never reached, so nothing was tested")
	}
	if outcome.Disposition != worker.Finished {
		fail("recovery ended %q (%s), not finished", outcome.Disposition, outcome.Err)
	}
	if outcome.Fence <= crash.Doomed.Token {
		fail("recovery fence %d does not exceed the crashed worker's fence %d", outcome.Fence, crash.Doomed.Token)
	}
	if !outcome.Takeover {
		fail("recovery was not a takeover of an expired lease")
	}

	run, err := s.Run(ctx, runID)
	if err != nil {
		fail("read run: %v", err)
		return result
	}
	if run.Status != "completed" {
		fail("run status is %q, not completed", run.Status)
	}

	events, err := s.Events(ctx, runID)
	if err != nil {
		fail("read ledger: %v", err)
		return result
	}
	committed := attributeMutations(events)
	result.Mutations = committed.total
	result.DuplicateCallIDs = committed.duplicates()
	for _, id := range result.DuplicateCallIDs {
		fail("call %s committed %d mutations", id, committed.byCall[id])
	}
	if committed.unattached > 0 {
		fail("%d mutation(s) are not inside any tool call", committed.unattached)
	}
	if ref != nil && committed.total != ref.Mutations {
		fail("agent mutation count is %d, an uncrashed run commits %d", committed.total, ref.Mutations)
	}

	if ref != nil {
		rows, err := worldRows(ctx, s, run.WorldID)
		if err != nil {
			fail("%v", err)
		} else {
			for _, table := range worldTables {
				if rows[table] != ref.TableRows[table] {
					fail("table %s has %d rows, an uncrashed run has %d", table, rows[table], ref.TableRows[table])
				}
			}
		}
	}

	verifyReplay(ctx, s, t, runID, &result, fail)
	verifyCheckpoints(ctx, s, t, runID, crash.CrashSeq, &result, fail)

	requests, responses := 0, 0
	for _, event := range events {
		switch event.Type {
		case "model.request":
			requests++
		case "model.response":
			responses++
		}
	}
	if requests != responses {
		fail("ledger has %d model requests and %d model responses", requests, responses)
	}

	verifyStaleWorker(ctx, s, t, runID, crash, events, &result, fail)
	return result
}

func verifyReplay(ctx context.Context, s *store.Store, t Target, runID string, result *Result, fail func(string, ...any)) {
	report, err := replay.Verify(ctx, s, runID, t.Manifest)
	switch {
	case err != nil:
		fail("replay of the recovered run failed: %v", err)
	case !report.Verified:
		fail("replay of the recovered run diverged: %s", report.Divergence)
	default:
		result.ReplayVerified = true
	}
}

// verifyCheckpoints proves the recovered ledger is still restorable. The
// checkpoint at the crash is the one a human would fork from to ask "what if the
// worker had not died here", and the last one is the finished run.
func verifyCheckpoints(ctx context.Context, s *store.Store, t Target, runID string, crashSeq int, result *Result, fail func(string, ...any)) {
	points, err := checkpoint.List(ctx, s, runID, t.Manifest)
	if err != nil {
		fail("checkpoint list failed: %v", err)
		return
	}
	if len(points) == 0 {
		fail("the recovered run has no checkpoints")
		return
	}
	chosen := []checkpoint.Checkpoint{points[len(points)-1]}
	for i := len(points) - 1; i >= 0; i-- {
		if points[i].EventSeq <= crashSeq {
			if points[i].EventSeq != chosen[0].EventSeq {
				chosen = append(chosen, points[i])
			}
			break
		}
	}
	for _, point := range chosen {
		rebuilt, _, err := checkpoint.Reconstruct(ctx, s, runID, point, t.Manifest)
		if err != nil {
			fail("checkpoint at event %d does not reconstruct: %v", point.EventSeq, err)
			continue
		}
		rebuilt.Close()
		result.CheckpointsReconstructed++
	}
}

// verifyStaleWorker wakes the crashed worker up and has it write. Fencing exists
// for this moment, so it is exercised rather than assumed. Two calls are tried: a
// new one, which would add an effect, and one the run already committed, which
// would otherwise be answered from the idempotency cache and so show a fenced-out
// worker the run's saved results.
func verifyStaleWorker(ctx context.Context, s *store.Store, t Target, runID string, crash CrashOutcome, events []store.Event, result *Result, fail func(string, ...any)) {
	fence := crash.Doomed.Fence()
	stale := &dispatch.Dispatcher{Store: s, Manifest: t.Manifest, Fence: &fence}
	before := attributeMutations(events).total

	_, err := stale.Invoke(ctx, runID, "crashmatrix-zombie", t.MutatingOperation, t.ZombieArguments)
	switch {
	case errors.Is(err, store.ErrFenced):
		result.StaleNewCallRejected = true
	case err == nil:
		fail("a stale worker committed a new call after losing the run")
	default:
		fail("a stale worker's new call failed for the wrong reason: %v", err)
	}

	callID, operation, arguments, ok := firstCommittedCall(ctx, s, runID, events)
	if !ok {
		fail("the run has no committed tool call to replay as a stale worker")
	} else {
		_, err = stale.Invoke(ctx, runID, callID, operation, arguments)
		switch {
		case errors.Is(err, store.ErrFenced):
			result.StaleExistingCallRejected = true
		case err == nil:
			fail("a stale worker was answered for committed call %s after losing the run", callID)
		default:
			fail("a stale worker's replay of call %s failed for the wrong reason: %v", callID, err)
		}
	}

	after, err := s.Events(ctx, runID)
	if err != nil {
		fail("read ledger after stale calls: %v", err)
		return
	}
	if now := attributeMutations(after).total; now != before {
		fail("agent mutation count changed from %d to %d after stale calls", before, now)
	}
	verifyStaleRunnerWrites(ctx, s, runID, fence, len(after), fail)
}

// verifyStaleRunnerWrites has the doomed worker make the two writes the runner
// itself makes (a model turn, which overwrites the transcript and step, and a
// status change), which the dispatcher calls above do not reach. A run's most
// damaging stale write is a transcript overwrite, and the dispatcher check alone
// would not notice a runner that stopped presenting its fence. Both must be
// refused and leave the run row and the ledger as they were.
func verifyStaleRunnerWrites(ctx context.Context, s *store.Store, runID string, fence store.Fence, eventsBefore int, fail func(string, ...any)) {
	before, err := s.Run(ctx, runID)
	if err != nil {
		fail("read run before the stale runner writes: %v", err)
		return
	}
	turn := map[string]any{"role": "assistant", "content": "written by a stale worker"}
	if err = s.SaveTurnGuarded(ctx, &fence, nil, runID, before.Step+1, `[{"role":"assistant","content":"stale"}]`, turn, nil); !errors.Is(err, store.ErrFenced) {
		fail("a stale worker's model turn was not refused as fenced: %v", err)
	}
	if err = s.SaveStatusGuarded(ctx, &fence, nil, runID, "failed", "error", map[string]any{"kind": "stale"}); !errors.Is(err, store.ErrFenced) {
		fail("a stale worker's status change was not refused as fenced: %v", err)
	}
	after, err := s.Run(ctx, runID)
	if err != nil {
		fail("read run after the stale runner writes: %v", err)
		return
	}
	if after.Status != before.Status || after.Step != before.Step || after.Transcript != before.Transcript {
		fail("a stale worker's runner writes changed the run: status %q to %q, step %d to %d, transcript changed %v",
			before.Status, after.Status, before.Step, after.Step, after.Transcript != before.Transcript)
	}
	events, err := s.Events(ctx, runID)
	if err != nil {
		fail("read ledger after the stale runner writes: %v", err)
		return
	}
	if len(events) != eventsBefore {
		fail("a stale worker's runner writes added %d ledger event(s)", len(events)-eventsBefore)
	}
}

// firstCommittedCall returns the earliest tool call in the ledger that has a
// saved result, with the operation and arguments it was recorded under.
func firstCommittedCall(ctx context.Context, s *store.Store, runID string, events []store.Event) (string, string, map[string]any, bool) {
	for _, event := range events {
		if event.Type != "tool.request" {
			continue
		}
		var request struct {
			CallID string `json:"call_id"`
		}
		if json.Unmarshal(event.Payload, &request) != nil || request.CallID == "" {
			continue
		}
		var operation, encoded string
		err := s.DB.QueryRowContext(ctx, "SELECT operation_id,arguments FROM tool_results WHERE run_id=? AND call_id=?", runID, request.CallID).Scan(&operation, &encoded)
		if err != nil {
			continue
		}
		var arguments map[string]any
		if json.Unmarshal([]byte(encoded), &arguments) != nil {
			continue
		}
		return request.CallID, operation, arguments, true
	}
	return "", "", nil, false
}
