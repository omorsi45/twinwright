package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// OwnershipEvent is one entry in a run's ownership history: who held it, at
// which fence, and what happened.
//
// This log is separate from the run ledger on purpose. The ledger is the
// agent's trajectory and is digested by fork lineage and checkpoint
// reconstruction, so it must not depend on which worker executed the run.
// Ownership history is infrastructure: useful for operators and traces, and
// irrelevant to replay.
type OwnershipEvent struct {
	RunID  string          `json:"run_id"`
	Seq    int             `json:"seq"`
	At     string          `json:"at"`
	Kind   string          `json:"kind"`
	Owner  string          `json:"owner"`
	Fence  int64           `json:"fence"`
	Detail json.RawMessage `json:"detail,omitempty"`
}

// Ownership event kinds.
const (
	OwnershipAcquired = "lease.acquired"
	OwnershipReleased = "lease.released"
	OwnershipRenewed  = "lease.renewed"
	OwnershipFenced   = "fence.rejected"
	OwnershipFailed   = "run.failed"
	OwnershipFinished = "run.finished"
)

func appendOwnershipTx(ctx context.Context, tx *sql.Tx, runID, kind, owner string, fence int64, now time.Time, detail any) error {
	encoded := "{}"
	if detail != nil {
		raw, err := json.Marshal(detail)
		if err != nil {
			return err
		}
		encoded = string(raw)
	}
	var seq int
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0)+1 FROM run_ownership_log WHERE run_id=?", runID).Scan(&seq); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		"INSERT INTO run_ownership_log(run_id,seq,at,kind,owner,fence,detail) VALUES(?,?,?,?,?,?,?)",
		runID, seq, now.UTC().Format(time.RFC3339Nano), kind, owner, fence, encoded)
	return err
}

// AppendOwnership records an ownership transition in its own transaction.
func (s *Store) AppendOwnership(ctx context.Context, runID, kind, owner string, fence int64, now time.Time, detail any) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = appendOwnershipTx(ctx, tx, runID, kind, owner, fence, now, detail); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordFenceRejection notes that a worker was fenced out.
//
// The rejected worker has no authority to append to the run's ledger - that is
// exactly what being fenced means - but recording the attempt in the ownership
// audit log is safe and is the only durable evidence that a stale worker tried
// to commit. It is best-effort: a failure here must never mask the rejection
// itself, so callers log and continue.
func (s *Store) RecordFenceRejection(ctx context.Context, fence Fence, now time.Time, reason string) error {
	return s.AppendOwnership(ctx, fence.RunID, OwnershipFenced, fence.Owner, fence.Token, now, map[string]any{"reason": reason})
}

// Ownership returns a run's ownership history in order.
func (s *Store) Ownership(ctx context.Context, runID string) ([]OwnershipEvent, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT run_id,seq,at,kind,owner,fence,detail FROM run_ownership_log WHERE run_id=? ORDER BY seq", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OwnershipEvent
	for rows.Next() {
		var event OwnershipEvent
		var detail string
		if err = rows.Scan(&event.RunID, &event.Seq, &event.At, &event.Kind, &event.Owner, &event.Fence, &detail); err != nil {
			return nil, err
		}
		event.Detail = json.RawMessage(detail)
		out = append(out, event)
	}
	return out, rows.Err()
}
