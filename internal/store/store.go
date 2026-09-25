package store

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	mrand "math/rand/v2"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ DB *sql.DB }
type World struct {
	ID     string `json:"id"`
	Seed   int64  `json:"seed"`
	Digest string `json:"digest"`
}
type Run struct {
	ID             string `json:"id"`
	WorldID        string `json:"world_id"`
	Scenario       string `json:"scenario"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Task           string `json:"task"`
	Status         string `json:"status"`
	Step           int    `json:"step"`
	Transcript     string `json:"transcript"`
	FaultOperation string `json:"fault_operation"`
}
type Event struct {
	ID         string          `json:"id"`
	RunID      string          `json:"run_id"`
	Seq        int             `json:"seq"`
	RecordedAt string          `json:"recorded_at"`
	WorldAt    string          `json:"world_at"`
	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload"`
}
type ForkLineage struct {
	ChildRunID     string `json:"child_run_id"`
	ParentRunID    string `json:"parent_run_id"`
	ForkEventSeq   int    `json:"fork_event_seq"`
	CheckpointID   string `json:"checkpoint_id"`
	FormatVersion  int    `json:"format_version"`
	ManifestDigest string `json:"manifest_digest"`
	PrefixDigest   string `json:"prefix_digest"`
	ParentProvider string `json:"parent_provider"`
	ParentModel    string `json:"parent_model"`
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{"PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000", "PRAGMA journal_mode=WAL"} {
		if _, err = db.Exec(stmt); err != nil {
			db.Close()
			return nil, err
		}
	}
	schema := []string{
		`CREATE TABLE IF NOT EXISTS worlds (id TEXT PRIMARY KEY, seed INTEGER NOT NULL, digest TEXT NOT NULL, base_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS customers (world_id TEXT NOT NULL, id TEXT NOT NULL, name TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS invoices (world_id TEXT NOT NULL, id TEXT NOT NULL, customer_id TEXT NOT NULL, amount_cents INTEGER NOT NULL, subscription_id TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS charges (world_id TEXT NOT NULL, id TEXT NOT NULL, invoice_id TEXT NOT NULL, amount_cents INTEGER NOT NULL, refunded_cents INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS refunds (world_id TEXT NOT NULL, id TEXT NOT NULL, charge_id TEXT NOT NULL, amount_cents INTEGER NOT NULL, reason TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS subscriptions (world_id TEXT NOT NULL, id TEXT NOT NULL, customer_id TEXT NOT NULL, status TEXT NOT NULL, plan TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS crm_accounts (world_id TEXT NOT NULL, id TEXT NOT NULL, customer_id TEXT NOT NULL, status TEXT NOT NULL, representative_id TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS crm_contacts (world_id TEXT NOT NULL, id TEXT NOT NULL, account_id TEXT NOT NULL, name TEXT NOT NULL, email TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS crm_notes (world_id TEXT NOT NULL, id TEXT NOT NULL, account_id TEXT NOT NULL, body TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS ticket_projects (world_id TEXT NOT NULL, id TEXT NOT NULL, key TEXT NOT NULL, name TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS ticket_issues (world_id TEXT NOT NULL, id TEXT NOT NULL, project_id TEXT NOT NULL, account_id TEXT NOT NULL, title TEXT NOT NULL, status TEXT NOT NULL, priority TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS ticket_comments (world_id TEXT NOT NULL, id TEXT NOT NULL, issue_id TEXT NOT NULL, body TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS message_workspaces (world_id TEXT NOT NULL, id TEXT NOT NULL, name TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS message_channels (world_id TEXT NOT NULL, id TEXT NOT NULL, workspace_id TEXT NOT NULL, name TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS message_members (world_id TEXT NOT NULL, channel_id TEXT NOT NULL, principal_id TEXT NOT NULL, PRIMARY KEY(world_id,channel_id,principal_id))`,
		`CREATE TABLE IF NOT EXISTS message_messages (world_id TEXT NOT NULL, id TEXT NOT NULL, channel_id TEXT NOT NULL, body TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY(world_id,id))`,
		`CREATE TABLE IF NOT EXISTS runs (id TEXT PRIMARY KEY, world_id TEXT NOT NULL, scenario TEXT NOT NULL, provider TEXT NOT NULL, model TEXT NOT NULL, task TEXT NOT NULL, status TEXT NOT NULL, step INTEGER NOT NULL DEFAULT 0, transcript TEXT NOT NULL DEFAULT '[]', fault_operation TEXT NOT NULL DEFAULT '', fault_consumed INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS events (run_id TEXT NOT NULL, seq INTEGER NOT NULL, id TEXT NOT NULL UNIQUE, recorded_at TEXT NOT NULL, world_at TEXT NOT NULL, type TEXT NOT NULL, payload TEXT NOT NULL, PRIMARY KEY(run_id,seq))`,
		`CREATE TABLE IF NOT EXISTS tool_results (run_id TEXT NOT NULL, call_id TEXT NOT NULL, operation_id TEXT NOT NULL, arguments TEXT NOT NULL, status INTEGER NOT NULL, body TEXT NOT NULL, PRIMARY KEY(run_id,call_id))`,
		`CREATE TABLE IF NOT EXISTS checkpoints (id TEXT PRIMARY KEY, run_id TEXT NOT NULL, event_seq INTEGER NOT NULL, format_version INTEGER NOT NULL, manifest_digest TEXT NOT NULL, prefix_digest TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS fork_lineage (child_run_id TEXT PRIMARY KEY, parent_run_id TEXT NOT NULL, fork_event_seq INTEGER NOT NULL, checkpoint_id TEXT NOT NULL, format_version INTEGER NOT NULL, manifest_digest TEXT NOT NULL, prefix_digest TEXT NOT NULL, parent_provider TEXT NOT NULL, parent_model TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS run_chaos (run_id TEXT PRIMARY KEY, policy_json TEXT NOT NULL, digest TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS chaos_rule_state (run_id TEXT NOT NULL, rule_id TEXT NOT NULL, matching_calls INTEGER NOT NULL DEFAULT 0, injections INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(run_id,rule_id))`,
		`CREATE TABLE IF NOT EXISTS chaos_snapshots (run_id TEXT NOT NULL, rule_id TEXT NOT NULL, arguments_digest TEXT NOT NULL, status INTEGER NOT NULL, body TEXT NOT NULL, PRIMARY KEY(run_id,rule_id,arguments_digest))`,
		`CREATE TABLE IF NOT EXISTS chaos_hidden_outcomes (run_id TEXT NOT NULL, call_id TEXT NOT NULL, rule_id TEXT NOT NULL, status INTEGER NOT NULL, body TEXT NOT NULL, PRIMARY KEY(run_id,call_id))`,
	}
	for _, stmt := range schema {
		if _, err = db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("schema: %w", err)
		}
	}
	if err = ensureModelColumn(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema migration: %w", err)
	}
	return &Store{DB: db}, nil
}
func ensureModelColumn(db *sql.DB) error {
	rows, err := db.Query("PRAGMA table_info(runs)")
	if err != nil {
		return err
	}
	hasModel := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue sql.NullString
		if err = rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "model" {
			hasModel = true
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if hasModel {
		return nil
	}
	_, err = db.Exec("ALTER TABLE runs ADD COLUMN model TEXT NOT NULL DEFAULT ''")
	return err
}
func OpenReadOnly(path string) (*Store, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uriPath := filepath.ToSlash(absolute)
	if filepath.VolumeName(absolute) != "" {
		uriPath = "/" + uriPath
	}
	uri := url.URL{Scheme: "file", Path: uriPath, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: db}, nil
}
func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) Seed(ctx context.Context, seed int64, digest string) (World, error) {
	return s.SeedScenario(ctx, seed, digest, "duplicate-charge")
}

func (s *Store) SeedScenario(ctx context.Context, seed int64, digest, scenario string) (World, error) {
	switch scenario {
	case "duplicate-charge", "company-incident", "company-routine", "company-no-duplicate":
	default:
		return World{}, fmt.Errorf("unknown scenario %q", scenario)
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", digest, seed)))
	prefix := "W-" + hex.EncodeToString(h[:8])
	w := World{Seed: seed, Digest: digest}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return World{}, err
	}
	defer tx.Rollback()
	var count int
	err = tx.QueryRowContext(ctx, "SELECT count(*) FROM worlds WHERE seed=? AND digest=?", seed, digest).Scan(&count)
	if err != nil {
		return World{}, err
	}
	w.ID = fmt.Sprintf("%s-%06d", prefix, count+1)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seed%365) * 24 * time.Hour)
	rng := mrand.New(mrand.NewPCG(uint64(seed), uint64(seed)^0x9e3779b97f4a7c15))
	amount := int64(2500 + rng.IntN(7500))
	type entry struct {
		q    string
		args []any
	}
	entries := []entry{
		{"INSERT INTO worlds VALUES(?,?,?,?)", []any{w.ID, seed, digest, base.Format(time.RFC3339)}},
		{"INSERT INTO customers VALUES(?,?,?)", []any{w.ID, "C-104", "Morgan Vale"}},
		{"INSERT INTO customers VALUES(?,?,?)", []any{w.ID, "C-205", "Taylor Reed"}},
		{"INSERT INTO invoices VALUES(?,?,?,?,?)", []any{w.ID, "INV-104", "C-104", amount, "SUB-104"}},
		{"INSERT INTO invoices VALUES(?,?,?,?,?)", []any{w.ID, "INV-205", "C-205", 4100, "SUB-205"}},
		{"INSERT INTO charges VALUES(?,?,?,?,?,?)", []any{w.ID, "CH-1001", "INV-104", amount, 0, base.Add(time.Hour).Format(time.RFC3339)}},
	}
	if scenario != "company-no-duplicate" {
		entries = append(entries, entry{"INSERT INTO charges VALUES(?,?,?,?,?,?)", []any{w.ID, "CH-1002", "INV-104", amount, 0, base.Add(2 * time.Hour).Format(time.RFC3339)}})
	}
	entries = append(entries,
		entry{"INSERT INTO charges VALUES(?,?,?,?,?,?)", []any{w.ID, "CH-2001", "INV-205", 4100, 0, base.Add(3 * time.Hour).Format(time.RFC3339)}},
		entry{"INSERT INTO subscriptions VALUES(?,?,?,?,?)", []any{w.ID, "SUB-104", "C-104", "active", "standard"}},
		entry{"INSERT INTO subscriptions VALUES(?,?,?,?,?)", []any{w.ID, "SUB-205", "C-205", "active", "standard"}},
	)
	if scenario != "duplicate-charge" {
		entries = append(entries,
			entry{"INSERT INTO crm_accounts VALUES(?,?,?,?,?)", []any{w.ID, "A-104", "C-104", "active", "REP-1"}},
			entry{"INSERT INTO crm_accounts VALUES(?,?,?,?,?)", []any{w.ID, "A-205", "C-205", "active", "REP-1"}},
			entry{"INSERT INTO crm_contacts VALUES(?,?,?,?,?)", []any{w.ID, "CT-104", "A-104", "Morgan Vale", "morgan.vale@example.test"}},
			entry{"INSERT INTO crm_contacts VALUES(?,?,?,?,?)", []any{w.ID, "CT-205", "A-205", "Taylor Reed", "taylor.reed@example.test"}},
			entry{"INSERT INTO ticket_projects VALUES(?,?,?,?)", []any{w.ID, "PROJ-ENG", "ENG", "Engineering"}},
			entry{"INSERT INTO message_workspaces VALUES(?,?,?)", []any{w.ID, "WS-1", "Company"}},
			entry{"INSERT INTO message_channels VALUES(?,?,?,?)", []any{w.ID, "CH-SUPPORT", "WS-1", "support"}},
			entry{"INSERT INTO message_members VALUES(?,?,?)", []any{w.ID, "CH-SUPPORT", "agent"}},
		)
		if scenario == "company-incident" {
			entries = append(entries, entry{"INSERT INTO crm_notes VALUES(?,?,?,?,?)", []any{w.ID, "SEED-INCIDENT-104", "A-104", "Billing retry worker retried C-104 invoice after a timeout; investigate duplicate charge incident.", base.Add(4 * time.Hour).Format(time.RFC3339)}})
		}
	}
	for _, e := range entries {
		if _, err = tx.ExecContext(ctx, e.q, e.args...); err != nil {
			return World{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return World{}, err
	}
	return w, nil
}

func (s *Store) Snapshot(ctx context.Context, worldID string) (string, error) {
	type row struct {
		Table  string `json:"table"`
		ID     string `json:"id"`
		Detail string `json:"detail"`
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT 'customer',id,name FROM customers WHERE world_id=? UNION ALL SELECT 'invoice',id,customer_id||':'||amount_cents FROM invoices WHERE world_id=? UNION ALL SELECT 'charge',id,invoice_id||':'||amount_cents||':'||refunded_cents FROM charges WHERE world_id=? UNION ALL SELECT 'refund',id,charge_id||':'||amount_cents FROM refunds WHERE world_id=? ORDER BY 1,2`, worldID, worldID, worldID, worldID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var out []row
	for rows.Next() {
		var r row
		if err = rows.Scan(&r.Table, &r.ID, &r.Detail); err != nil {
			return "", err
		}
		out = append(out, r)
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	b, err := json.Marshal(out)
	return string(b), err
}

func (s *Store) CreateRun(ctx context.Context, worldID, scenario, provider, model, task, faultOperation string) (Run, error) {
	return s.createRun(ctx, worldID, scenario, provider, model, task, faultOperation, nil, "")
}

// CreateRunWithChaos persists a validated policy in the same transaction as the run.
func (s *Store) CreateRunWithChaos(ctx context.Context, worldID, scenario, provider, model, task, faultOperation string, policyJSON []byte, digest string) (Run, error) {
	if faultOperation != "" {
		return Run{}, fmt.Errorf("legacy fault and chaos policy cannot be combined")
	}
	if !json.Valid(policyJSON) {
		return Run{}, fmt.Errorf("invalid chaos policy JSON")
	}
	hash := sha256.Sum256(policyJSON)
	if hex.EncodeToString(hash[:]) != digest {
		return Run{}, fmt.Errorf("chaos policy digest mismatch")
	}
	return s.createRun(ctx, worldID, scenario, provider, model, task, faultOperation, policyJSON, digest)
}

func (s *Store) createRun(ctx context.Context, worldID, scenario, provider, model, task, faultOperation string, policyJSON []byte, digest string) (Run, error) {
	var random [12]byte
	if _, err := crand.Read(random[:]); err != nil {
		return Run{}, err
	}
	r := Run{ID: "R-" + hex.EncodeToString(random[:]), WorldID: worldID, Scenario: scenario, Provider: provider, Model: model, Task: task, Status: "running", Transcript: "[]", FaultOperation: faultOperation}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO runs(id,world_id,scenario,provider,model,task,status,transcript,fault_operation) VALUES(?,?,?,?,?,?,?,?,?)", r.ID, r.WorldID, r.Scenario, r.Provider, r.Model, r.Task, r.Status, r.Transcript, r.FaultOperation); err != nil {
		return Run{}, err
	}
	if len(policyJSON) > 0 {
		if _, err = tx.ExecContext(ctx, "INSERT INTO run_chaos(run_id,policy_json,digest) VALUES(?,?,?)", r.ID, string(policyJSON), digest); err != nil {
			return Run{}, err
		}
	}
	if err = AppendEventTx(ctx, tx, r.ID, "execution.started", map[string]any{"scenario": scenario, "provider": provider, "model": model, "world_id": worldID}); err != nil {
		return Run{}, err
	}
	if err = tx.Commit(); err != nil {
		return Run{}, err
	}
	return r, nil
}

// ChaosPolicy returns sql.ErrNoRows for old databases or runs without a policy.
func (s *Store) ChaosPolicy(ctx context.Context, runID string) ([]byte, string, error) {
	var exists int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='run_chaos'").Scan(&exists); err != nil {
		return nil, "", err
	}
	if exists == 0 {
		return nil, "", sql.ErrNoRows
	}
	var encoded, digest string
	if err := s.DB.QueryRowContext(ctx, "SELECT policy_json,digest FROM run_chaos WHERE run_id=?", runID).Scan(&encoded, &digest); err != nil {
		return nil, "", err
	}
	return []byte(encoded), digest, nil
}

func (s *Store) Run(ctx context.Context, id string) (Run, error) {
	var r Run
	err := s.DB.QueryRowContext(ctx, "SELECT id,world_id,scenario,provider,model,task,status,step,transcript,fault_operation FROM runs WHERE id=?", id).Scan(&r.ID, &r.WorldID, &r.Scenario, &r.Provider, &r.Model, &r.Task, &r.Status, &r.Step, &r.Transcript, &r.FaultOperation)
	return r, err
}

func (s *Store) Lineage(ctx context.Context, childRunID string) (ForkLineage, error) {
	var lineage ForkLineage
	var exists int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='fork_lineage'").Scan(&exists); err != nil {
		return lineage, err
	}
	if exists == 0 {
		return lineage, sql.ErrNoRows
	}
	err := s.DB.QueryRowContext(ctx, `SELECT child_run_id,parent_run_id,fork_event_seq,checkpoint_id,format_version,manifest_digest,prefix_digest,parent_provider,parent_model FROM fork_lineage WHERE child_run_id=?`, childRunID).Scan(
		&lineage.ChildRunID, &lineage.ParentRunID, &lineage.ForkEventSeq, &lineage.CheckpointID, &lineage.FormatVersion, &lineage.ManifestDigest, &lineage.PrefixDigest, &lineage.ParentProvider, &lineage.ParentModel)
	return lineage, err
}

func (s *Store) Append(ctx context.Context, runID, typ string, payload any) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = AppendEventTx(ctx, tx, runID, typ, payload); err != nil {
		return err
	}
	return tx.Commit()
}

func AppendEventTx(ctx context.Context, tx *sql.Tx, runID, typ string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var seq int
	var base string
	err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(e.seq),0)+1,w.base_at FROM runs r JOIN worlds w ON w.id=r.world_id LEFT JOIN events e ON e.run_id=r.id WHERE r.id=?", runID).Scan(&seq, &base)
	if err != nil {
		return err
	}
	baseTime, err := time.Parse(time.RFC3339, base)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO events(run_id,seq,id,recorded_at,world_at,type,payload) VALUES(?,?,?,?,?,?,?)", runID, seq, fmt.Sprintf("%s/%d", runID, seq), time.Now().UTC().Format(time.RFC3339Nano), baseTime.Add(time.Duration(seq)*time.Second).Format(time.RFC3339), typ, string(data))
	return err
}

func (s *Store) Events(ctx context.Context, runID string) ([]Event, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT id,run_id,seq,recorded_at,world_at,type,payload FROM events WHERE run_id=? ORDER BY seq", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var payload string
		if err = rows.Scan(&e.ID, &e.RunID, &e.Seq, &e.RecordedAt, &e.WorldAt, &e.Type, &payload); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) StartModelCall(ctx context.Context, runID string, request any) error {
	return s.Append(ctx, runID, "model.request", request)
}

func (s *Store) SaveTurn(ctx context.Context, runID string, step int, transcript string, response any) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = AppendEventTx(ctx, tx, runID, "model.response", response); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE runs SET step=?,transcript=?,status='running' WHERE id=?", step, transcript, runID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FailModelTurn(ctx context.Context, runID string, response any, message string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = AppendEventTx(ctx, tx, runID, "model.response", response); err != nil {
		return err
	}
	if err = AppendEventTx(ctx, tx, runID, "error", map[string]any{"kind": "provider", "message": message}); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE runs SET status='failed' WHERE id=?", runID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SaveStatus(ctx context.Context, runID, status, typ string, payload any) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE runs SET status=? WHERE id=?", status, runID); err != nil {
		return err
	}
	if err = AppendEventTx(ctx, tx, runID, typ, payload); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateReplayRun starts a run with a preserved ID in an isolated store.
func (s *Store) CreateReplayRun(ctx context.Context, original Run, worldID string) (Run, error) {
	r := Run{
		ID: original.ID, WorldID: worldID, Scenario: original.Scenario,
		Provider: original.Provider, Model: original.Model, Task: original.Task,
		Status: "running", Transcript: "[]", FaultOperation: original.FaultOperation,
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO runs(id,world_id,scenario,provider,model,task,status,transcript,fault_operation) VALUES(?,?,?,?,?,?,?,?,?)", r.ID, r.WorldID, r.Scenario, r.Provider, r.Model, r.Task, r.Status, r.Transcript, r.FaultOperation); err != nil {
		return Run{}, err
	}
	if err = AppendEventTx(ctx, tx, r.ID, "execution.started", map[string]any{"scenario": r.Scenario, "provider": r.Provider, "model": r.Model, "world_id": worldID}); err != nil {
		return Run{}, err
	}
	if err = tx.Commit(); err != nil {
		return Run{}, err
	}
	return r, nil
}
