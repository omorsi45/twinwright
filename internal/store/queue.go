package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
}

// Enqueue makes a run claimable by any worker. Enqueueing a run that is
// already queued resets it to runnable and clears its backoff, which is how an
// operator retries a run that failed.
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
	stamp := now.UTC().Format(time.RFC3339Nano)
	_, err := s.DB.ExecContext(ctx, `
INSERT INTO work_queue(run_id,state,max_steps,attempts,enqueued_at,available_at,last_error)
VALUES(?,?,?,0,?,?,'')
ON CONFLICT(run_id) DO UPDATE SET state=excluded.state, max_steps=excluded.max_steps, available_at=excluded.available_at, last_error=''`,
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
// On PostgreSQL the candidate row is locked with FOR UPDATE ... SKIP LOCKED, so
// several workers polling at the same instant each take a different run instead
// of serialising or colliding. SQLite has one writer, so the transaction itself
// provides that exclusion.
func (s *Store) ClaimRun(ctx context.Context, owner string, ttl time.Duration, now time.Time) (RunLease, QueueEntry, error) {
	if owner == "" {
		return RunLease{}, QueueEntry{}, fmt.Errorf("worker owner is required")
	}
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
	stamp := now.UTC().Format(time.RFC3339Nano)
	var entry QueueEntry
	err = tx.QueryRowContext(ctx, `
SELECT q.run_id, q.state, q.max_steps, q.attempts, q.enqueued_at, q.available_at
FROM work_queue q
LEFT JOIN run_leases l ON l.run_id = q.run_id
WHERE q.available_at <= ?
  AND ( q.state = '`+QueueRunnable+`'
        OR ( q.state = '`+QueueLeased+`' AND (l.run_id IS NULL OR l.expires_at <= ?) ) )
ORDER BY q.enqueued_at, q.run_id
LIMIT 1`+lock, stamp, stamp).
		Scan(&entry.RunID, &entry.State, &entry.MaxSteps, &entry.Attempts, &entry.EnqueuedAt, &entry.AvailableAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RunLease{}, QueueEntry{}, ErrNoWork
	}
	if err != nil {
		return RunLease{}, QueueEntry{}, err
	}

	takeover := entry.State == QueueLeased
	lease, err := acquireRunLeaseTx(ctx, tx, s.Dialect, entry.RunID, owner, ttl, now, takeover)
	if err != nil {
		return RunLease{}, QueueEntry{}, err
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

func (s *Store) completeQueueEntry(ctx context.Context, lease RunLease, now time.Time, state, kind, reason string, availableAt time.Time) error {
	if availableAt.IsZero() {
		availableAt = now
	}
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
	if _, err = tx.ExecContext(ctx, "UPDATE work_queue SET state=?, available_at=?, last_error=? WHERE run_id=?",
		state, availableAt.UTC().Format(time.RFC3339Nano), reason, lease.RunID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE run_leases SET owner='', expires_at=?, heartbeat_at=? WHERE run_id=? AND owner=? AND fence=?",
		now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano), lease.RunID, lease.Owner, lease.Token); err != nil {
		return err
	}
	detail := map[string]any{"state": state}
	if reason != "" {
		detail["reason"] = reason
	}
	if err = appendOwnershipTx(ctx, tx, lease.RunID, kind, lease.Owner, lease.Token, now, detail); err != nil {
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
	stamp := now.UTC().Format(time.RFC3339Nano)
	_, err := s.DB.ExecContext(ctx, `
INSERT INTO workers(id,host,started_at,last_seen_at,claims) VALUES(?,?,?,?,0)
ON CONFLICT(id) DO UPDATE SET host=excluded.host, last_seen_at=excluded.last_seen_at`,
		id, host, stamp, stamp)
	return err
}

// TouchWorker updates a worker's liveness stamp.
func (s *Store) TouchWorker(ctx context.Context, id string, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, "UPDATE workers SET last_seen_at=? WHERE id=?", now.UTC().Format(time.RFC3339Nano), id)
	return err
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
