package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNoWork means the queue held nothing claimable. It is an ordinary outcome
// of an idle poll, not a failure.
var ErrNoWork = errors.New("no runnable work")

// Work queue states.
const (
	QueueRunnable = "runnable"
	QueueLeased   = "leased"
	QueueFinished = "finished"
	QueueFailed   = "failed"
)

// QueueEntry is one run's place in the work queue.
type QueueEntry struct {
	RunID       string `json:"run_id"`
	State       string `json:"state"`
	MaxSteps    int    `json:"max_steps"`
	Attempts    int    `json:"attempts"`
	EnqueuedAt  string `json:"enqueued_at"`
	AvailableAt string `json:"available_at"`
	LastError   string `json:"last_error,omitempty"`
	// Takeover is set by ClaimRun, never stored. It is true only when the claim
	// displaced a lease that had expired, the state a dead worker leaves behind.
	// A run that was requeued after an error is claimed from the runnable state
	// and is a retry, not a takeover, however many attempts it has had.
	Takeover bool `json:"takeover"`
}

// Enqueue makes a run claimable by any worker. Enqueueing a run that is
// already queued resets it to runnable, clears its backoff and its delivery
// count, which is how an operator retries a run that failed. The count is reset
// because an explicit retry deserves a fresh delivery budget: left in place, a
// run that failed at the delivery limit would be refused again on the next claim
// without executing. Which workers held the run before stays in
// run_ownership_log.
func (s *Store) Enqueue(ctx context.Context, runID string, maxSteps int, now time.Time) error {
	if maxSteps < 1 {
		return fmt.Errorf("maxSteps must be positive")
	}
	var runs int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM runs WHERE id=?", runID).Scan(&runs); err != nil {
		return err
	}
	if runs == 0 {
		return fmt.Errorf("run %s does not exist", runID)
	}
	stamp := CoordinationTime(now)
	_, err := s.DB.ExecContext(ctx, `
INSERT INTO work_queue(run_id,state,max_steps,attempts,enqueued_at,available_at,last_error)
VALUES(?,?,?,0,?,?,'')
ON CONFLICT(run_id) DO UPDATE SET state=excluded.state, max_steps=excluded.max_steps, attempts=0, available_at=excluded.available_at, last_error=''`,
		runID, QueueRunnable, maxSteps, stamp, stamp)
	return err
}

// ClaimRun takes the oldest claimable run and returns a fenced lease for it.
//
// A run is claimable when it is runnable, or when it is marked leased but its
// lease has expired - which is exactly the state a crashed worker leaves
// behind. Recovery therefore needs no separate janitor process: the next poll
// picks the run up, and the fence it gets is strictly higher than the dead
// worker's, so the dead worker cannot commit if it ever wakes up.
//
// A run whose lease is still held by another worker is never claimable, even
// when the queue row says runnable. That combination is reachable: Enqueue resets
// the state without touching the lease, so an operator retrying a run that is
// currently being worked on produces exactly it. Losing that race is contention,
// not a failure, so such a run is skipped and the next candidate is tried. A
// worker that reported an error here would stop over one busy run while other
// runs sat claimable.
//
// On PostgreSQL the candidate row is locked with FOR UPDATE ... SKIP LOCKED, so
// several workers polling at the same instant each take a different run instead
// of serialising or colliding. SQLite has one writer, so the transaction itself
// provides that exclusion.
func (s *Store) ClaimRun(ctx context.Context, owner string, ttl time.Duration, now time.Time) (RunLease, QueueEntry, error) {
	if owner == "" {
		return RunLease{}, QueueEntry{}, fmt.Errorf("worker owner is required")
	}
	// The SQL below already excludes a live foreign lease. This loop covers the
	// residual case where the row looked claimable when it was selected and the
	// lease turned out to be held anyway: on PostgreSQL a concurrent claim that
	// commits mid-statement can be re-checked against the selecting snapshot, so
	// the row can pass the filter and still be taken. Skipping the run by id
	// rather than retrying blindly is what keeps the poll making progress.
	var skip []string
	for attempt := 0; attempt < maxClaimAttempts; attempt++ {
		lease, entry, err := s.claimOnce(ctx, owner, ttl, now, skip)
		if errors.Is(err, ErrLeaseHeld) {
			skip = append(skip, entry.RunID)
			continue
		}
		return lease, entry, err
	}
	// Every candidate this poll could see is held by someone else, which is an
	// idle tick rather than a problem.
	return RunLease{}, QueueEntry{}, ErrNoWork
}

// maxClaimAttempts bounds how many contended runs one poll will skip before
// reporting no work. A poll is cheap and runs again shortly, so there is no value
// in walking a long queue of busy runs inside a single call.
const maxClaimAttempts = 8

// claimableWhere is the condition under which a worker can claim a queue entry,
// over work_queue as q and its lease as l (a LEFT JOIN on run_id). It is the one
// definition of "claimable": claimOnce selects with it, and OldestClaimableSince
// reads the same text, so the metrics gauge for the age of the oldest claimable
// entry cannot disagree with what ClaimRun can take.
//
// An entry is claimable when its available_at has passed (a requeued run waits
// out its backoff) and either it is runnable, or it is marked leased and its lease
// is missing or expired. A lease that is live and held by someone other than the
// claimant makes the entry unclaimable whatever the queue row says. The four
// placeholders are bound by claimableArgs.
const claimableWhere = `q.available_at <= ?
  AND ( q.state = '` + QueueRunnable + `'
        OR ( q.state = '` + QueueLeased + `' AND (l.run_id IS NULL OR l.expires_at <= ?) ) )
  AND ( l.run_id IS NULL OR l.owner = '' OR l.owner = ? OR l.expires_at <= ? )`

// claimableArgs binds claimableWhere for a claimant at the instant stamp. An
// empty owner is nobody, so every live lease counts as someone else's.
func claimableArgs(stamp, owner string) []any {
	return []any{stamp, stamp, owner, stamp}
}

// OldestClaimableSince reports when the entry that has been claimable longest
// became claimable, as a CoordinationLayout stamp, and false when no worker could
// claim anything at now. It uses claimableWhere, so it answers for the entries
// ClaimRun would take: a runnable entry counts from its available_at, and an entry
// whose lease expired (a crashed worker's) counts from the later of its
// available_at and the expiry, because it was not claimable before the lease ran
// out. A run another worker's live lease still holds is not counted. It reads
// only, and takes no lock.
func (s *Store) OldestClaimableSince(ctx context.Context, now time.Time) (string, bool, error) {
	var since string
	err := s.DB.QueryRowContext(ctx, `
SELECT CASE WHEN l.owner <> '' AND l.expires_at > q.available_at THEN l.expires_at ELSE q.available_at END AS claimable_since
FROM work_queue q
LEFT JOIN run_leases l ON l.run_id = q.run_id
WHERE `+claimableWhere+`
ORDER BY claimable_since
LIMIT 1`, claimableArgs(CoordinationTime(now), "")...).Scan(&since)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return since, true, nil
}

// claimOnce selects one candidate and leases it, skipping the given run ids.
func (s *Store) claimOnce(ctx context.Context, owner string, ttl time.Duration, now time.Time, skip []string) (RunLease, QueueEntry, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return RunLease{}, QueueEntry{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// FOR UPDATE OF q, not plain FOR UPDATE: PostgreSQL refuses to lock the
	// nullable side of an outer join, and the queue row is the one that needs
	// locking anyway.
	lock := ""
	if s.Dialect == DialectPostgres {
		lock = " FOR UPDATE OF q SKIP LOCKED"
	}
	stamp := CoordinationTime(now)
	// The first placeholder belongs to the displaced column of the SELECT list;
	// the rest bind claimableWhere.
	args := append([]any{stamp}, claimableArgs(stamp, owner)...)
	// Placeholders are generated; the ids themselves are always bound values.
	exclude := ""
	if len(skip) > 0 {
		marks := make([]string, len(skip))
		for i, id := range skip {
			marks[i] = "?"
			args = append(args, id)
		}
		exclude = "\n  AND q.run_id NOT IN (" + strings.Join(marks, ",") + ")"
	}
	var entry QueueEntry
	// displaced is read from the lease row and not from the queue state. Enqueue
	// resets a live run to runnable without touching its lease, so a claim that
	// takes over from a dead owner can arrive through the runnable branch.
	var displaced int
	err = tx.QueryRowContext(ctx, `
SELECT q.run_id, q.state, q.max_steps, q.attempts, q.enqueued_at, q.available_at,
       CASE WHEN l.owner <> '' AND l.expires_at <= ? THEN 1 ELSE 0 END
FROM work_queue q
LEFT JOIN run_leases l ON l.run_id = q.run_id
WHERE `+claimableWhere+exclude+`
ORDER BY q.enqueued_at, q.run_id
LIMIT 1`+lock, args...).
		Scan(&entry.RunID, &entry.State, &entry.MaxSteps, &entry.Attempts, &entry.EnqueuedAt, &entry.AvailableAt, &displaced)
	if errors.Is(err, sql.ErrNoRows) {
		return RunLease{}, QueueEntry{}, ErrNoWork
	}
	if err != nil {
		return RunLease{}, QueueEntry{}, err
	}

	// The same value goes to the ownership log and to the caller, so the two
	// cannot disagree about whether this claim displaced a previous owner.
	takeover := displaced == 1
	lease, err := acquireRunLeaseTx(ctx, tx, s.Dialect, entry.RunID, owner, ttl, now, takeover)
	if err != nil {
		// entry carries the run id so the caller can skip this run and look at
		// the next candidate rather than selecting it again.
		return RunLease{}, entry, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE work_queue SET state=?, attempts=attempts+1 WHERE run_id=?", QueueLeased, entry.RunID); err != nil {
		return RunLease{}, QueueEntry{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE workers SET claims=claims+1, last_seen_at=? WHERE id=?", stamp, owner); err != nil {
		return RunLease{}, QueueEntry{}, err
	}
	if err = tx.Commit(); err != nil {
		return RunLease{}, QueueEntry{}, err
	}
	entry.State = QueueLeased
	entry.Attempts++
	entry.Takeover = takeover
	return lease, entry, nil
}

// FinishRun marks a claimed run complete and gives up its lease, in one
// transaction gated on the worker still owning the run.
func (s *Store) FinishRun(ctx context.Context, lease RunLease, now time.Time) error {
	return s.completeQueueEntry(ctx, lease, now, QueueFinished, OwnershipFinished, "", time.Time{})
}

// FailRun marks a claimed run failed without rescheduling it. Used when a
// failure cannot be fixed by running again, so retrying would only burn work.
func (s *Store) FailRun(ctx context.Context, lease RunLease, now time.Time, reason string) error {
	return s.completeQueueEntry(ctx, lease, now, QueueFailed, OwnershipFailed, reason, time.Time{})
}

// RequeueRun returns a claimed run to the queue, available again at
// availableAt. This is the at-least-once path: the run will be attempted again,
// and idempotent tool effects are what keep the repeat safe.
func (s *Store) RequeueRun(ctx context.Context, lease RunLease, now, availableAt time.Time, reason string) error {
	return s.completeQueueEntry(ctx, lease, now, QueueRunnable, OwnershipReleased, reason, availableAt)
}

// ReleaseClaim hands a claimed run back to the queue, runnable at once, and gives
// back the delivery that claim counted. It is for a worker that was told to stop
// while the run was executing: the work was cut short by the worker's own
// shutdown, which says nothing about the run, so it must not count toward the
// delivery bound or wait out a backoff. A few rolling restarts during one long run
// would otherwise fail it at the limit.
//
// The release is fenced like every completion, in one transaction with the queue
// update, the lease release and the ownership row, so only the current owner can
// undo its own delivery. A worker that was killed instead of stopped never gets
// here, and its delivery stays counted: that is the path a crash takes.
func (s *Store) ReleaseClaim(ctx context.Context, lease RunLease, now time.Time, reason string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = GuardFenceTx(ctx, tx, s.Dialect, lease.Fence(), now); err != nil {
		return err
	}
	if err = completeQueueEntryTx(ctx, tx, lease, now, QueueRunnable, OwnershipReleased, reason, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE work_queue SET attempts=CASE WHEN attempts>0 THEN attempts-1 ELSE 0 END WHERE run_id=?", lease.RunID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) completeQueueEntry(ctx context.Context, lease RunLease, now time.Time, state, kind, reason string, availableAt time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Gated on ownership: a fenced-out worker must not be able to declare a
	// run finished or failed after another worker took it over.
	if err = GuardFenceTx(ctx, tx, s.Dialect, lease.Fence(), now); err != nil {
		return err
	}
	if err = completeQueueEntryTx(ctx, tx, lease, now, state, kind, reason, availableAt); err != nil {
		return err
	}
	return tx.Commit()
}

// completeQueueEntryTx records a claim's end inside a transaction that has
// already passed GuardFenceTx: the queue state, the released lease, and the
// ownership log row.
func completeQueueEntryTx(ctx context.Context, tx *sql.Tx, lease RunLease, now time.Time, state, kind, reason string, availableAt time.Time) error {
	if availableAt.IsZero() {
		availableAt = now
	}
	if _, err := tx.ExecContext(ctx, "UPDATE work_queue SET state=?, available_at=?, last_error=? WHERE run_id=?",
		state, CoordinationTime(availableAt), reason, lease.RunID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE run_leases SET owner='', expires_at=?, heartbeat_at=? WHERE run_id=? AND owner=? AND fence=?",
		CoordinationTime(now), CoordinationTime(now), lease.RunID, lease.Owner, lease.Token); err != nil {
		return err
	}
	detail := map[string]any{"state": state}
	if reason != "" {
		detail["reason"] = reason
	}
	return appendOwnershipTx(ctx, tx, lease.RunID, kind, lease.Owner, lease.Token, now, detail)
}

// DeliveryLimitReason is the queue entry's last_error, and the worker's outcome
// text, for a run that used up its deliveries without completing.
func DeliveryLimitReason(deliveries, maxAttempts int) string {
	return fmt.Sprintf("delivery limit: %d deliveries without completion (max attempts %d)", deliveries, maxAttempts)
}

// FailRunAtDeliveryLimit fails a run that was delivered more times than its
// attempt bound allows without ever completing.
//
// MaxAttempts used to be checked only when execution returned an error. A worker
// that dies reports nothing, so a run that kills its worker every time was taken
// over forever. The claim count is the one signal a crash does leave behind.
//
// The run status, the ledger event, the queue entry, the released lease and the
// ownership row commit together after GuardFenceTx, so a worker that lost the
// run to a takeover cannot fail it. The ledger payload carries counts only: it
// is the agent trajectory and never names a worker or a lease.
func (s *Store) FailRunAtDeliveryLimit(ctx context.Context, lease RunLease, now time.Time, deliveries, maxAttempts int) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = GuardFenceTx(ctx, tx, s.Dialect, lease.Fence(), now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE runs SET status='failed' WHERE id=?", lease.RunID); err != nil {
		return err
	}
	payload := map[string]any{"kind": "delivery_limit", "deliveries": deliveries, "max_attempts": maxAttempts}
	if err = AppendEventTx(ctx, tx, lease.RunID, "error", payload); err != nil {
		return err
	}
	reason := DeliveryLimitReason(deliveries, maxAttempts)
	if err = completeQueueEntryTx(ctx, tx, lease, now, QueueFailed, OwnershipFailed, reason, time.Time{}); err != nil {
		return err
	}
	return tx.Commit()
}

// QueueEntryFor reports a run's queue state.
func (s *Store) QueueEntryFor(ctx context.Context, runID string) (QueueEntry, error) {
	var entry QueueEntry
	err := s.DB.QueryRowContext(ctx,
		"SELECT run_id,state,max_steps,attempts,enqueued_at,available_at,last_error FROM work_queue WHERE run_id=?", runID).
		Scan(&entry.RunID, &entry.State, &entry.MaxSteps, &entry.Attempts, &entry.EnqueuedAt, &entry.AvailableAt, &entry.LastError)
	return entry, err
}

// QueueDepth counts queue entries by state, for the metrics endpoint and the
// CLI's queue view.
func (s *Store) QueueDepth(ctx context.Context) (map[string]int, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT state, count(*) FROM work_queue GROUP BY state")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	depth := map[string]int{}
	for rows.Next() {
		var state string
		var count int
		if err = rows.Scan(&state, &count); err != nil {
			return nil, err
		}
		depth[state] = count
	}
	return depth, rows.Err()
}

// RegisterWorker records a worker's presence so operators can see the fleet.
func (s *Store) RegisterWorker(ctx context.Context, id, host string, now time.Time) error {
	stamp := CoordinationTime(now)
	_, err := s.DB.ExecContext(ctx, `
INSERT INTO workers(id,host,started_at,last_seen_at,claims) VALUES(?,?,?,?,0)
ON CONFLICT(id) DO UPDATE SET host=excluded.host, last_seen_at=excluded.last_seen_at`,
		id, host, stamp, stamp)
	return err
}

// TouchWorker updates a worker's liveness stamp.
func (s *Store) TouchWorker(ctx context.Context, id string, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, "UPDATE workers SET last_seen_at=? WHERE id=?", CoordinationTime(now), id)
	return err
}

// RemoveWorker takes a worker's row out of the fleet table when the worker stops
// cleanly, and reports whether it removed one. A worker that still owns a live
// lease at now keeps its row: the lease is an unfinished run, and the row is how an
// operator finds who holds it. The row of a worker that was killed instead of
// stopped stays, and the metrics leave it out once it has not been seen for their
// window. Removing a row that is not there is not an error.
func (s *Store) RemoveWorker(ctx context.Context, id string, now time.Time) (bool, error) {
	result, err := s.DB.ExecContext(ctx, `
DELETE FROM workers
WHERE id=? AND NOT EXISTS (SELECT 1 FROM run_leases WHERE owner=? AND expires_at > ?)`,
		id, id, CoordinationTime(now))
	if err != nil {
		return false, err
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return removed > 0, nil
}

// WorkerRecord is one registered worker.
type WorkerRecord struct {
	ID         string `json:"id"`
	Host       string `json:"host"`
	StartedAt  string `json:"started_at"`
	LastSeenAt string `json:"last_seen_at"`
	Claims     int    `json:"claims"`
}

// Workers lists registered workers, most recently seen first.
func (s *Store) Workers(ctx context.Context) ([]WorkerRecord, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT id,host,started_at,last_seen_at,claims FROM workers ORDER BY last_seen_at DESC, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkerRecord
	for rows.Next() {
		var record WorkerRecord
		if err = rows.Scan(&record.ID, &record.Host, &record.StartedAt, &record.LastSeenAt, &record.Claims); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}
