// Package lease provides time-bounded ownership with fencing tokens.
//
// Delivery for a future multi-worker Twinwright is at-least-once: a worker may
// see the same unit of work more than once after a crash. Handlers must stay
// idempotent by call ID. This package does not claim exactly-once delivery.
package lease

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Lease is one owned resource.
type Lease struct {
	Name      string    `json:"name"`
	Owner     string    `json:"owner"`
	Token     int64     `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Store persists leases.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) a SQLite lease database.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	for _, stmt := range []string{"PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000", "PRAGMA journal_mode=WAL"} {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS leases (
  name TEXT PRIMARY KEY NOT NULL,
  owner TEXT NOT NULL,
  token INTEGER NOT NULL,
  expires_at TEXT NOT NULL
)`)
	return err
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Acquire takes ownership of name for owner until now+ttl.
// If an unexpired lease exists for another owner, Acquire fails.
// If the lease is missing or expired, Acquire creates a new fencing token.
func (s *Store) Acquire(ctx context.Context, name, owner string, ttl time.Duration, now time.Time) (Lease, error) {
	if name == "" || owner == "" {
		return Lease{}, fmt.Errorf("lease name and owner are required")
	}
	if ttl < time.Second {
		return Lease{}, fmt.Errorf("lease ttl must be at least one second")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var current Lease
	var expires string
	err = tx.QueryRowContext(ctx, `SELECT name, owner, token, expires_at FROM leases WHERE name=?`, name).Scan(&current.Name, &current.Owner, &current.Token, &expires)
	switch {
	case err == sql.ErrNoRows:
		current.Token = 0
	case err != nil:
		return Lease{}, err
	default:
		current.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires)
		if err != nil {
			return Lease{}, err
		}
		if current.ExpiresAt.After(now) && current.Owner != "" && current.Owner != owner {
			return Lease{}, fmt.Errorf("lease %q held by %q until %s", name, current.Owner, current.ExpiresAt.UTC().Format(time.RFC3339Nano))
		}
	}
	next := Lease{Name: name, Owner: owner, Token: current.Token + 1, ExpiresAt: now.Add(ttl).UTC()}
	if _, err = tx.ExecContext(ctx, `
INSERT INTO leases(name, owner, token, expires_at) VALUES(?,?,?,?)
ON CONFLICT(name) DO UPDATE SET owner=excluded.owner, token=excluded.token, expires_at=excluded.expires_at
`, next.Name, next.Owner, next.Token, next.ExpiresAt.Format(time.RFC3339Nano)); err != nil {
		return Lease{}, err
	}
	if err = tx.Commit(); err != nil {
		return Lease{}, err
	}
	return next, nil
}

// Renew extends a lease only when the owner and fencing token still match.
func (s *Store) Renew(ctx context.Context, name, owner string, token int64, ttl time.Duration, now time.Time) (Lease, error) {
	if ttl < time.Second {
		return Lease{}, fmt.Errorf("lease ttl must be at least one second")
	}
	expires := now.Add(ttl).UTC()
	res, err := s.db.ExecContext(ctx, `
UPDATE leases SET expires_at=? WHERE name=? AND owner=? AND token=? AND expires_at > ?
`, expires.Format(time.RFC3339Nano), name, owner, token, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return Lease{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Lease{}, err
	}
	if n != 1 {
		return Lease{}, fmt.Errorf("lease renew rejected for %q (owner/token mismatch or expired)", name)
	}
	return Lease{Name: name, Owner: owner, Token: token, ExpiresAt: expires}, nil
}

// Release clears ownership while keeping the fencing token sequence.
func (s *Store) Release(ctx context.Context, name, owner string, token int64, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `
UPDATE leases SET owner='', expires_at=? WHERE name=? AND owner=? AND token=?
`, now.UTC().Format(time.RFC3339Nano), name, owner, token)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("lease release rejected for %q (owner/token mismatch)", name)
	}
	return nil
}

// Get returns the current lease row, including expired ones.
func (s *Store) Get(ctx context.Context, name string) (Lease, error) {
	var lease Lease
	var expires string
	err := s.db.QueryRowContext(ctx, `SELECT name, owner, token, expires_at FROM leases WHERE name=?`, name).Scan(&lease.Name, &lease.Owner, &lease.Token, &expires)
	if err != nil {
		return Lease{}, err
	}
	lease.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires)
	return lease, err
}
