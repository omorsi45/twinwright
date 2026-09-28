package store

import (
	"context"
	"database/sql"
	"time"
)

// Guarded writes.
//
// Every durable write a worker makes on behalf of a run goes through one of
// these. Each takes an optional *Fence: nil means "no ownership checking",
// which is how a single-node local run keeps its original behaviour exactly,
// and a non-nil fence makes the write conditional on still owning the run.
//
// The check runs inside the same transaction as the write, so a takeover cannot
// land between proving ownership and committing. A rejected write rolls back
// whole: a fenced-out worker leaves no partial state and no ledger entry.

// Clock lets tests drive lease expiry deterministically instead of sleeping.
// Production code leaves it nil and gets time.Now.
type Clock func() time.Time

// Now returns the clock's current time, defaulting to time.Now in UTC.
func (c Clock) Now() time.Time {
	if c == nil {
		return time.Now().UTC()
	}
	return c()
}

func (c Clock) now() time.Time { return c.Now() }

// guarded runs body in a transaction, asserting ownership first when fence is
// set.
func (s *Store) guarded(ctx context.Context, fence *Fence, clock Clock, body func(*sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if fence != nil {
		if err = GuardFenceTx(ctx, tx, s.Dialect, *fence, clock.now()); err != nil {
			return err
		}
	}
	if err = body(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// AppendGuarded appends one ledger event under an ownership check.
func (s *Store) AppendGuarded(ctx context.Context, fence *Fence, clock Clock, runID, typ string, payload any) error {
	return s.guarded(ctx, fence, clock, func(tx *sql.Tx) error {
		return AppendEventTx(ctx, tx, runID, typ, payload)
	})
}

// SaveTurnGuarded persists a model turn under an ownership check.
func (s *Store) SaveTurnGuarded(ctx context.Context, fence *Fence, clock Clock, runID string, step int, transcript string, response any) error {
	return s.guarded(ctx, fence, clock, func(tx *sql.Tx) error {
		if err := AppendEventTx(ctx, tx, runID, "model.response", response); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE runs SET step=?,transcript=?,status='running' WHERE id=?", step, transcript, runID)
		return err
	})
}

// FailModelTurnGuarded records a provider failure under an ownership check.
func (s *Store) FailModelTurnGuarded(ctx context.Context, fence *Fence, clock Clock, runID string, response any, message string) error {
	return s.guarded(ctx, fence, clock, func(tx *sql.Tx) error {
		if err := AppendEventTx(ctx, tx, runID, "model.response", response); err != nil {
			return err
		}
		if err := AppendEventTx(ctx, tx, runID, "error", map[string]any{"kind": "provider", "message": message}); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE runs SET status='failed' WHERE id=?", runID)
		return err
	})
}

// SaveStatusGuarded moves a run's status under an ownership check.
func (s *Store) SaveStatusGuarded(ctx context.Context, fence *Fence, clock Clock, runID, status, typ string, payload any) error {
	return s.guarded(ctx, fence, clock, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE runs SET status=? WHERE id=?", status, runID); err != nil {
			return err
		}
		return AppendEventTx(ctx, tx, runID, typ, payload)
	})
}
