package fork

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"twinwright/internal/agent"
	"twinwright/internal/authz"
	"twinwright/internal/chaos"
	"twinwright/internal/checkpoint"
	"twinwright/internal/compiler"
	"twinwright/internal/store"
)

type Options struct {
	Provider       string
	Model          string
	FaultOperation *string
	ChaosPolicyRaw []byte
	// AuthPolicyRaw replaces the inherited authorization policy; the child
	// then starts at call zero.
	AuthPolicyRaw []byte
	// Observation replaces what the child saw as the response of the call
	// whose tool response is the fork checkpoint. World state is unchanged.
	Observation *store.Observation
}

type Result struct {
	Run        store.Run             `json:"run"`
	World      store.World           `json:"world"`
	Checkpoint checkpoint.Checkpoint `json:"checkpoint"`
}

// ValidateOptions checks a fork's future execution settings without writing.
func ValidateOptions(parent store.Run, manifest compiler.Manifest, options Options) error {
	if options.FaultOperation != nil && *options.FaultOperation != "" && manifest.Operation(*options.FaultOperation) == nil {
		return fmt.Errorf("unknown fault operation %q", *options.FaultOperation)
	}
	if len(options.ChaosPolicyRaw) > 0 {
		if options.FaultOperation != nil && *options.FaultOperation != "" {
			return fmt.Errorf("fork fault and chaos policy cannot be combined")
		}
		if _, err := chaos.Parse(options.ChaosPolicyRaw, manifest); err != nil {
			return err
		}
	}
	if len(options.AuthPolicyRaw) > 0 {
		if _, err := authz.Parse(options.AuthPolicyRaw, manifest); err != nil {
			return err
		}
	}
	if options.Provider != "" && !agent.KnownProvider(options.Provider) {
		return fmt.Errorf("unsupported fork provider %q", options.Provider)
	}
	if options.Provider != "" && options.Provider != parent.Provider && options.Model == "" {
		return fmt.Errorf("changing fork provider requires an explicit model")
	}
	provider, model := parent.Provider, parent.Model
	if options.Provider != "" {
		provider = options.Provider
	}
	if options.Model != "" {
		model = options.Model
	}
	if provider == "" || model == "" {
		return fmt.Errorf("fork provider and model are required")
	}
	return nil
}

// Create reconstructs the chosen prefix, then writes an isolated child atomically.
// sourceReadOnly and destination must refer to the same database file.
func Create(ctx context.Context, sourceReadOnly, destination *store.Store, selected checkpoint.Checkpoint, manifest compiler.Manifest, options Options) (Result, error) {
	if selected.FormatVersion != checkpoint.FormatVersion || selected.ManifestDigest != manifest.Digest {
		return Result{}, fmt.Errorf("checkpoint format or manifest mismatch")
	}
	parent, err := sourceReadOnly.Run(ctx, selected.RunID)
	if err != nil {
		return Result{}, err
	}
	if err := ValidateOptions(parent, manifest, options); err != nil {
		return Result{}, err
	}
	originalEvents, err := sourceReadOnly.Events(ctx, parent.ID)
	if err != nil {
		return Result{}, err
	}
	initialTip := len(originalEvents)
	var observation store.Observation
	var observationEvent map[string]any
	if options.Observation != nil {
		if observation, observationEvent, err = prepareObservation(originalEvents, selected, *options.Observation); err != nil {
			return Result{}, err
		}
	}
	rebuiltStore, rebuilt, err := checkpoint.Reconstruct(ctx, sourceReadOnly, parent.ID, selected, manifest)
	if err != nil {
		return Result{}, err
	}
	defer rebuiltStore.Close()
	transcript := rebuilt.Transcript
	if observationEvent != nil {
		if transcript, err = applyObservation(transcript, observation); err != nil {
			return Result{}, err
		}
	}
	provider, model := parent.Provider, parent.Model
	if options.Provider != "" {
		provider = options.Provider
	}
	if options.Model != "" {
		model = options.Model
	}
	if provider == "" || model == "" {
		return Result{}, fmt.Errorf("fork provider and model are required")
	}
	fault := parent.FaultOperation
	if options.FaultOperation != nil {
		fault = *options.FaultOperation
	}
	if len(options.ChaosPolicyRaw) > 0 {
		fault = ""
	}
	var consumed int
	if err := rebuiltStore.DB.QueryRowContext(ctx, "SELECT fault_consumed FROM runs WHERE id=?", rebuilt.ID).Scan(&consumed); err != nil {
		return Result{}, err
	}
	if fault != parent.FaultOperation {
		consumed = 0
	}
	var seed int64
	var digest, baseAt string
	if err := rebuiltStore.DB.QueryRowContext(ctx, "SELECT seed,digest,base_at FROM worlds WHERE id=?", rebuilt.WorldID).Scan(&seed, &digest, &baseAt); err != nil {
		return Result{}, err
	}
	worldID, err := randomID("W-F-")
	if err != nil {
		return Result{}, err
	}
	runID, err := randomID("R-")
	if err != nil {
		return Result{}, err
	}
	tx, err := destination.DB.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()
	if err := verifySourceInTransaction(ctx, tx, parent, selected, initialTip); err != nil {
		return Result{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO worlds(id,seed,digest,base_at) VALUES(?,?,?,?)", worldID, seed, digest, baseAt); err != nil {
		return Result{}, err
	}
	for _, table := range worldTables {
		if err := copyWorldTable(ctx, rebuiltStore.DB, tx, table, rebuilt.WorldID, worldID); err != nil {
			return Result{}, fmt.Errorf("copy %s: %w", table.name, err)
		}
	}
	principal := parent.PrincipalID
	var authPolicy authz.Policy
	if len(options.AuthPolicyRaw) > 0 {
		if authPolicy, err = authz.Parse(options.AuthPolicyRaw, manifest); err != nil {
			return Result{}, err
		}
		principal = authPolicy.Principal.ID
	}
	child := store.Run{ID: runID, WorldID: worldID, Scenario: parent.Scenario, Provider: provider, Model: model, Task: parent.Task,
		Status: "paused", Step: rebuilt.Step, Transcript: transcript, FaultOperation: fault, PrincipalID: principal}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs(id,world_id,scenario,provider,model,task,status,step,transcript,fault_operation,fault_consumed,principal_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		child.ID, child.WorldID, child.Scenario, child.Provider, child.Model, child.Task, child.Status, child.Step, child.Transcript, child.FaultOperation, consumed, child.PrincipalID); err != nil {
		return Result{}, err
	}
	if err := copyToolResults(ctx, rebuiltStore.DB, tx, parent.ID, child.ID); err != nil {
		return Result{}, err
	}
	if observationEvent != nil {
		if err := overrideToolResult(ctx, tx, child.ID, observation); err != nil {
			return Result{}, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO fork_observations(child_run_id,call_id,status,body) VALUES(?,?,?,?)", child.ID, observation.CallID, observation.Status, string(observation.Body)); err != nil {
			return Result{}, err
		}
	}
	if len(options.ChaosPolicyRaw) > 0 {
		policy, err := chaos.Parse(options.ChaosPolicyRaw, manifest)
		if err != nil {
			return Result{}, err
		}
		encoded, err := policy.CanonicalJSON()
		if err != nil {
			return Result{}, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO run_chaos(run_id,policy_json,digest) VALUES(?,?,?)", child.ID, string(encoded), policy.Digest()); err != nil {
			return Result{}, err
		}
	} else if err := chaos.CopyRun(ctx, rebuiltStore.DB, tx, parent.ID, child.ID); err != nil {
		return Result{}, err
	}
	if len(options.AuthPolicyRaw) > 0 {
		encoded, err := authPolicy.CanonicalJSON()
		if err != nil {
			return Result{}, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO run_auth(run_id,policy_json,digest) VALUES(?,?,?)", child.ID, string(encoded), authPolicy.Digest()); err != nil {
			return Result{}, err
		}
	} else if err := authz.CopyRun(ctx, rebuiltStore.DB, tx, parent.ID, child.ID); err != nil {
		return Result{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoints(id,run_id,event_seq,format_version,manifest_digest,prefix_digest) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`,
		selected.ID, parent.ID, selected.EventSeq, selected.FormatVersion, selected.ManifestDigest, selected.PrefixDigest); err != nil {
		return Result{}, err
	}
	// chaos_replaced and auth_replaced are integer columns on both backends.
	// SQLite silently accepts a Go bool there; PostgreSQL refuses to encode one
	// into BIGINT, so the flag is converted explicitly rather than relying on
	// one driver's leniency.
	if _, err := tx.ExecContext(ctx, `INSERT INTO fork_lineage(child_run_id,parent_run_id,fork_event_seq,checkpoint_id,format_version,manifest_digest,prefix_digest,parent_provider,parent_model,chaos_replaced,auth_replaced) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		child.ID, parent.ID, selected.EventSeq, selected.ID, selected.FormatVersion, selected.ManifestDigest, selected.PrefixDigest, parent.Provider, parent.Model,
		flagInt(len(options.ChaosPolicyRaw) > 0), flagInt(len(options.AuthPolicyRaw) > 0)); err != nil {
		return Result{}, err
	}
	if err := store.AppendEventTx(ctx, tx, child.ID, "execution.forked", map[string]any{
		"parent_run_id": parent.ID, "fork_event_seq": selected.EventSeq, "checkpoint_id": selected.ID, "manifest_digest": manifest.Digest,
	}); err != nil {
		return Result{}, err
	}
	if observationEvent != nil {
		if err := store.AppendEventTx(ctx, tx, child.ID, "observation.overridden", observationEvent); err != nil {
			return Result{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Result{}, err
	}
	return Result{Run: child, World: store.World{ID: worldID, Seed: seed, Digest: digest}, Checkpoint: selected}, nil
}

func overrideToolResult(ctx context.Context, tx *sql.Tx, runID string, o store.Observation) error {
	result, err := tx.ExecContext(ctx, "UPDATE tool_results SET status=?,body=? WHERE run_id=? AND call_id=?", o.Status, string(o.Body), runID, o.CallID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("observation override found no saved result for call %s", o.CallID)
	}
	return nil
}

func randomID(prefix string) (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

func verifySourceInTransaction(ctx context.Context, tx *sql.Tx, parent store.Run, selected checkpoint.Checkpoint, initialTip int) error {
	var status, worldID, digest string
	if err := tx.QueryRowContext(ctx, `SELECT r.status,r.world_id,w.digest FROM runs r JOIN worlds w ON w.id=r.world_id WHERE r.id=?`, parent.ID).Scan(&status, &worldID, &digest); err != nil {
		return err
	}
	if (status != "paused" && status != "completed") || worldID != parent.WorldID || digest != selected.ManifestDigest {
		return fmt.Errorf("source run changed during fork")
	}
	rows, err := tx.QueryContext(ctx, `SELECT seq,id,recorded_at,world_at,type,payload FROM events WHERE run_id=? ORDER BY seq`, parent.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var prefix []store.Event
	tip := 0
	for rows.Next() {
		var event store.Event
		var payload string
		if err := rows.Scan(&event.Seq, &event.ID, &event.RecordedAt, &event.WorldAt, &event.Type, &payload); err != nil {
			return err
		}
		event.RunID, event.Payload = parent.ID, json.RawMessage(payload)
		tip++
		if event.Seq <= selected.EventSeq {
			prefix = append(prefix, event)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if tip != initialTip || len(prefix) != selected.EventSeq {
		return fmt.Errorf("source ledger changed during fork")
	}
	digestNow, err := checkpoint.PrefixDigest(parent.ID, selected.ManifestDigest, prefix)
	if err != nil || digestNow != selected.PrefixDigest {
		return fmt.Errorf("source checkpoint changed during fork")
	}
	return nil
}

type tableSpec struct {
	name    string
	columns []string
}

var worldTables = []tableSpec{
	{"customers", []string{"id", "name"}},
	{"invoices", []string{"id", "customer_id", "amount_cents", "subscription_id"}},
	{"charges", []string{"id", "invoice_id", "amount_cents", "refunded_cents", "created_at"}},
	{"refunds", []string{"id", "charge_id", "amount_cents", "reason", "created_at"}},
	{"subscriptions", []string{"id", "customer_id", "status", "plan"}},
	{"crm_accounts", []string{"id", "customer_id", "status", "representative_id"}},
	{"crm_contacts", []string{"id", "account_id", "name", "email"}},
	{"crm_notes", []string{"id", "account_id", "body", "created_at"}},
	{"ticket_projects", []string{"id", "key", "name"}},
	{"ticket_issues", []string{"id", "project_id", "account_id", "title", "status", "priority"}},
	{"ticket_comments", []string{"id", "issue_id", "body", "created_at"}},
	{"message_workspaces", []string{"id", "name"}},
	{"message_channels", []string{"id", "workspace_id", "name"}},
	{"message_members", []string{"channel_id", "principal_id"}},
	{"message_messages", []string{"id", "channel_id", "body", "created_at"}},
}

func copyWorldTable(ctx context.Context, source *sql.DB, target *sql.Tx, table tableSpec, fromWorldID, toWorldID string) error {
	columns := strings.Join(table.columns, ",")
	rows, err := source.QueryContext(ctx, "SELECT "+columns+" FROM "+table.name+" WHERE world_id=?", fromWorldID)
	if err != nil {
		return err
	}
	defer rows.Close()
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(table.columns)+1), ",")
	insert := "INSERT INTO " + table.name + "(world_id," + columns + ") VALUES(" + placeholders + ")"
	for rows.Next() {
		values := make([]any, len(table.columns))
		ptrs := make([]any, len(values))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		args := append([]any{toWorldID}, values...)
		if _, err := target.ExecContext(ctx, insert, args...); err != nil {
			return err
		}
	}
	return rows.Err()
}

func copyToolResults(ctx context.Context, source *sql.DB, target *sql.Tx, parentRunID, childRunID string) error {
	rows, err := source.QueryContext(ctx, `SELECT call_id,operation_id,arguments,status,body FROM tool_results WHERE run_id=? ORDER BY call_id`, parentRunID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var callID, operationID, arguments, body string
		var status int
		if err := rows.Scan(&callID, &operationID, &arguments, &status, &body); err != nil {
			return err
		}
		if _, err := target.ExecContext(ctx, `INSERT INTO tool_results(run_id,call_id,operation_id,arguments,status,body) VALUES(?,?,?,?,?,?)`, childRunID, callID, operationID, arguments, status, body); err != nil {
			return err
		}
	}
	return rows.Err()
}

// flagInt renders a boolean for an integer column. Both backends store these
// flags as integers; only SQLite would accept a Go bool for one.
func flagInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
