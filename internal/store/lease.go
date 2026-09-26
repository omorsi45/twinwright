package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrFenced is returned when a worker tries to commit work for a run it no
// longer owns. Callers treat it as "stop, someone else has this run" rather
// than as a retryable failure: retrying cannot help, because the run has moved.
var ErrFenced = errors.New("fenced out: this worker no longer owns the run")

// RunLease is time-bounded, fenced ownership of one run's execution.
//
// Delivery in the multi-worker runtime is at-least-once: a crashed worker's run
// is taken over and the same unit of work may be attempted twice. Two
// mechanisms keep that safe. Tool effects are idempotent by call ID, so a
// repeated tool call returns the recorded result instead of committing a second
// effect. Durable writes are fenced, so a worker whose lease lapsed cannot
// commit after a newer worker has taken over. Exactly-once delivery is not
// claimed anywhere.
type RunLease struct {
	RunID     string    `json:"run_id"`
	Owner     string    `json:"owner"`
	Token     int64     `json:"fence"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Fence returns the token a worker presents with every durable write.
func (l RunLease) Fence() Fence { return Fence{RunID: l.RunID, Owner: l.Owner, Token: l.Token} }

// Fence identifies the holder of a run at a point in the fence sequence. A nil
// *Fence means "no ownership checking", which is how single-node local runs
// keep their original behaviour.
type Fence struct {
	RunID string `json:"run_id"`
	Owner string `json:"owner"`
	Token int64  `json:"fence"`
}

// forUpdate locks the lease row for the duration of the transaction on
// PostgreSQL, where several workers hold real concurrent connections. SQLite
// serialises writers with a single connection, so the clause is unnecessary
// there and is not valid SQLite syntax in this position.
func (s *Store) forUpdate() string {
	if s.Dialect == DialectPostgres {
		return " FOR UPDATE"
	}
	return ""
}

// AcquireRunLease takes fenced ownership of a run until now+ttl.
//
// Every successful acquire increments the fence, including re-acquisition by
// the current owner: a worker that lost contact and reconnected must not be
// able to keep using its previous token, because the run may have been taken
// over and handed back in between.
func (s *Store) AcquireRunLease(ctx context.Context, runID, owner string, ttl time.Duration, now time.Time) (RunLease, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return RunLease{}, err
	}
	defer func() { _ = tx.Rollback() }()
	lease, err := acquireRunLeaseTx(ctx, tx, s.Dialect, runID, owner, ttl, now, false)
	if err != nil {
		return RunLease{}, err
	}
	if err = tx.Commit(); err != nil {
		return RunLease{}, err
	}
	return lease, nil
}

// acquireRunLeaseTx is the transaction-scoped form, so claiming a queued run
// and taking its lease commit together. A claim that acquired a lease and then
// failed to update the queue would strand the run under a dead owner.
//
// takeover records that this acquisition displaced a previous holder, which is
// what distinguishes a crash recovery from a normal first claim in the audit
// log and in metrics.
func acquireRunLeaseTx(ctx context.Context, tx *sql.Tx, dialect Dialect, runID, owner string, ttl time.Duration, now time.Time, takeover bool) (RunLease, error) {
	if runID == "" || owner == "" {
		return RunLease{}, fmt.Errorf("run ID and owner are required")
	}
	if ttl < time.Second {
		return RunLease{}, fmt.Errorf("lease ttl must be at least one second")
	}
	lock := ""
	if dialect == DialectPostgres {
		lock = " FOR UPDATE"
	}

	var currentOwner, expiresText string
	var fence, committed int64
	err := tx.QueryRowContext(ctx, `SELECT owner,fence,committed_fence,expires_at FROM run_leases WHERE run_id=?`+lock, runID).
		Scan(&currentOwner, &fence, &committed, &expiresText)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		var runs int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM runs WHERE id=?", runID).Scan(&runs); err != nil {
			return RunLease{}, err
		}
		if runs == 0 {
			return RunLease{}, fmt.Errorf("run %s does not exist", runID)
		}
		fence, committed = 0, 0
	case err != nil:
		return RunLease{}, err
	default:
		expires, parseErr := time.Parse(time.RFC3339Nano, expiresText)
		if parseErr != nil {
			return RunLease{}, fmt.Errorf("lease expiry for %s: %w", runID, parseErr)
		}
		if currentOwner != "" && currentOwner != owner && expires.After(now) {
			return RunLease{}, fmt.Errorf("run %s is leased by %q until %s", runID, currentOwner, expires.UTC().Format(time.RFC3339Nano))
		}
	}

	next := RunLease{RunID: runID, Owner: owner, Token: fence + 1, ExpiresAt: now.Add(ttl).UTC()}
	if _, err = tx.ExecContext(ctx, `
INSERT INTO run_leases(run_id,owner,fence,committed_fence,expires_at,acquired_at,heartbeat_at) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(run_id) DO UPDATE SET owner=excluded.owner, fence=excluded.fence, expires_at=excluded.expires_at, acquired_at=excluded.acquired_at, heartbeat_at=excluded.heartbeat_at`,
		runID, owner, next.Token, committed, next.ExpiresAt.Format(time.RFC3339Nano),
		now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano)); err != nil {
		return RunLease{}, err
	}
	if err = appendOwnershipTx(ctx, tx, runID, OwnershipAcquired, owner, next.Token, now, map[string]any{
		"expires_at":     next.ExpiresAt.Format(time.RFC3339Nano),
		"previous_owner": currentOwner,
		"takeover":       takeover || (currentOwner != "" && currentOwner != owner),
	}); err != nil {
		return RunLease{}, err
	}
	return next, nil
}

// RenewRunLease extends a live lease held by the presented owner and fence.
// An expired lease cannot be renewed: the run may already belong to a newer
// worker, so the holder has to go back through AcquireRunLease and get a new
// fence, which is exactly the signal it needs that it lost the run.
func (s *Store) RenewRunLease(ctx context.Context, lease RunLease, ttl time.Duration, now time.Time) (RunLease, error) {
	if ttl < time.Second {
		return RunLease{}, fmt.Errorf("lease ttl must be at least one second")
	}
	expires := now.Add(ttl).UTC()
	result, err := s.DB.ExecContext(ctx, `
UPDATE run_leases SET expires_at=?, heartbeat_at=? WHERE run_id=? AND owner=? AND fence=? AND expires_at > ?`,
		expires.Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano),
		lease.RunID, lease.Owner, lease.Token, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return RunLease{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return RunLease{}, err
	}
	if affected != 1 {
		return RunLease{}, fmt.Errorf("%w: renew rejected for run %s (owner, fence or expiry no longer match)", ErrFenced, lease.RunID)
	}
	renewed := lease
	renewed.ExpiresAt = expires
	return renewed, nil
}

// ReleaseRunLease gives up ownership without rewinding the fence sequence, so
// a released holder still cannot write and the next owner still gets a higher
// token.
func (s *Store) ReleaseRunLease(ctx context.Context, lease RunLease, now time.Time) error {
	result, err := s.DB.ExecContext(ctx, `
UPDATE run_leases SET owner='', expires_at=?, heartbeat_at=? WHERE run_id=? AND owner=? AND fence=?`,
		now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano),
		lease.RunID, lease.Owner, lease.Token)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return fmt.Errorf("%w: release rejected for run %s", ErrFenced, lease.RunID)
	}
	return s.AppendOwnership(ctx, lease.RunID, OwnershipReleased, lease.Owner, lease.Token, now, nil)
}

// RunLease reports current ownership, including an expired row, so operators
// can see who held a run last. It returns sql.ErrNoRows for a run that has
// never been leased.
func (s *Store) RunLease(ctx context.Context, runID string) (RunLease, error) {
	var lease RunLease
	var expiresText string
	err := s.DB.QueryRowContext(ctx, "SELECT run_id,owner,fence,expires_at FROM run_leases WHERE run_id=?", runID).
		Scan(&lease.RunID, &lease.Owner, &lease.Token, &expiresText)
	if err != nil {
		return RunLease{}, err
	}
	lease.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresText)
	return lease, err
}

// GuardFenceTx asserts, inside an existing transaction, that fence still owns
// the run, and records the fence as the highest one that has committed.
//
// This is the single choke point for every durable write a worker makes. Two
// independent conditions must hold, and both matter:
//
//   - The lease row must still name this owner and token, and must not have
//     expired. Expiry alone is disqualifying, because the moment a lease lapses
//     another worker may take the run.
//   - The presented token must be at least the highest token that has already
//     committed. This one does not depend on any clock, so it stays correct
//     under clock skew between workers and the database, which is precisely the
//     case a TTL check alone gets wrong.
//
// Recording committed_fence inside the caller's transaction is what makes the
// check atomic with the work it guards: a takeover cannot slip between the
// check and the write.
func GuardFenceTx(ctx context.Context, tx *sql.Tx, dialect Dialect, fence Fence, now time.Time) error {
	if fence.RunID == "" || fence.Owner == "" {
		return fmt.Errorf("fence requires a run ID and owner")
	}
	lock := ""
	if dialect == DialectPostgres {
		lock = " FOR UPDATE"
	}
	var owner, expiresText string
	var current, committed int64
	err := tx.QueryRowContext(ctx, `SELECT owner,fence,committed_fence,expires_at FROM run_leases WHERE run_id=?`+lock, fence.RunID).
		Scan(&owner, &current, &committed, &expiresText)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: run %s has no lease", ErrFenced, fence.RunID)
	}
	if err != nil {
		return err
	}
	if fence.Token < committed {
		return fmt.Errorf("%w: fence %d for run %s was superseded by %d", ErrFenced, fence.Token, fence.RunID, committed)
	}
	if owner != fence.Owner || current != fence.Token {
		return fmt.Errorf("%w: run %s is now held by %q at fence %d, not %q at fence %d", ErrFenced, fence.RunID, owner, current, fence.Owner, fence.Token)
	}
	expires, err := time.Parse(time.RFC3339Nano, expiresText)
	if err != nil {
		return fmt.Errorf("lease expiry for %s: %w", fence.RunID, err)
	}
	if !expires.After(now) {
		return fmt.Errorf("%w: lease on run %s expired at %s", ErrFenced, fence.RunID, expires.UTC().Format(time.RFC3339Nano))
	}
	if committed != fence.Token {
		if _, err = tx.ExecContext(ctx, "UPDATE run_leases SET committed_fence=? WHERE run_id=?", fence.Token, fence.RunID); err != nil {
			return err
		}
	}
	return nil
}

// GuardFence checks ownership in its own transaction, for callers that only
// need the assertion and have no write to pair it with.
func (s *Store) GuardFence(ctx context.Context, fence Fence, now time.Time) error {
	return s.withGuardedTx(ctx, fence, now, func(*sql.Tx) error { return nil })
}

// withGuardedTx runs body in a transaction that first proves ownership. A
// rejected guard rolls the whole transaction back, so an out-of-date worker
// leaves no trace of the write it attempted.
func (s *Store) withGuardedTx(ctx context.Context, fence Fence, now time.Time, body func(*sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = GuardFenceTx(ctx, tx, s.Dialect, fence, now); err != nil {
		return err
	}
	if err = body(tx); err != nil {
		return err
	}
	return tx.Commit()
}
