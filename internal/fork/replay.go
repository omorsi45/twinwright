package fork

import (
	"context"
	"fmt"
	"strings"

	"twinwright/internal/chaos"
	"twinwright/internal/checkpoint"
	"twinwright/internal/compiler"
	"twinwright/internal/store"
)

// ReconstructForReplay verifies the parent prefix and prepares an isolated child
// at the exact state from which its recorded suffix began. The caller closes it.
func ReconstructForReplay(ctx context.Context, source *store.Store, child store.Run, lineage store.ForkLineage, manifest compiler.Manifest) (*store.Store, error) {
	if lineage.ChildRunID != child.ID || lineage.FormatVersion != checkpoint.FormatVersion || lineage.ManifestDigest != manifest.Digest {
		return nil, fmt.Errorf("fork lineage format or manifest mismatch")
	}
	parent, err := source.Run(ctx, lineage.ParentRunID)
	if err != nil {
		return nil, err
	}
	if parent.Provider != lineage.ParentProvider || parent.Model != lineage.ParentModel || parent.Scenario != child.Scenario || parent.Task != child.Task {
		return nil, fmt.Errorf("fork parent metadata differs from lineage")
	}
	points, err := checkpoint.List(ctx, source, parent.ID, manifest)
	if err != nil {
		return nil, err
	}
	selected, err := checkpoint.Select(points, lineage.ForkEventSeq)
	if err != nil {
		return nil, err
	}
	if selected.ID != lineage.CheckpointID || selected.PrefixDigest != lineage.PrefixDigest || selected.FormatVersion != lineage.FormatVersion {
		return nil, fmt.Errorf("fork lineage checkpoint differs from parent prefix")
	}
	var childSeed int64
	var childDigest, childBase string
	if err := source.DB.QueryRowContext(ctx, "SELECT seed,digest,base_at FROM worlds WHERE id=?", child.WorldID).Scan(&childSeed, &childDigest, &childBase); err != nil {
		return nil, err
	}
	if childDigest != manifest.Digest {
		return nil, fmt.Errorf("fork child manifest mismatch")
	}
	target, rebuilt, err := checkpoint.Reconstruct(ctx, source, parent.ID, selected, manifest)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			target.Close()
		}
	}()
	var parentSeed int64
	var parentDigest, parentBase string
	if err := target.DB.QueryRowContext(ctx, "SELECT seed,digest,base_at FROM worlds WHERE id=?", rebuilt.WorldID).Scan(&parentSeed, &parentDigest, &parentBase); err != nil {
		return nil, err
	}
	if childSeed != parentSeed || childDigest != parentDigest || childBase != parentBase {
		return nil, fmt.Errorf("fork child world metadata differs from checkpoint")
	}
	var consumed int
	if err := target.DB.QueryRowContext(ctx, "SELECT fault_consumed FROM runs WHERE id=?", rebuilt.ID).Scan(&consumed); err != nil {
		return nil, err
	}
	if child.FaultOperation != parent.FaultOperation {
		consumed = 0
	}
	tx, err := target.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO worlds(id,seed,digest,base_at) VALUES(?,?,?,?)", child.WorldID, childSeed, childDigest, childBase); err != nil {
		return nil, err
	}
	for _, table := range worldTables {
		columns := strings.Join(table.columns, ",")
		query := "INSERT INTO " + table.name + "(world_id," + columns + ") SELECT ?," + columns + " FROM " + table.name + " WHERE world_id=?"
		if _, err := tx.ExecContext(ctx, query, child.WorldID, rebuilt.WorldID); err != nil {
			return nil, fmt.Errorf("copy %s: %w", table.name, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs(id,world_id,scenario,provider,model,task,status,step,transcript,fault_operation,fault_consumed) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		child.ID, child.WorldID, child.Scenario, child.Provider, child.Model, child.Task, "paused", rebuilt.Step, rebuilt.Transcript, child.FaultOperation, consumed); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tool_results(run_id,call_id,operation_id,arguments,status,body) SELECT ?,call_id,operation_id,arguments,status,body FROM tool_results WHERE run_id=?`, child.ID, parent.ID); err != nil {
		return nil, err
	}
	if lineage.ChaosReplaced {
		encoded, digest, err := source.ChaosPolicy(ctx, child.ID)
		if err != nil {
			return nil, err
		}
		policy, err := chaos.ValidateStored(encoded, digest, manifest)
		if err != nil || policy.Digest() != digest {
			return nil, fmt.Errorf("fork replacement chaos policy differs from lineage")
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO run_chaos(run_id,policy_json,digest) VALUES(?,?,?)", child.ID, string(encoded), digest); err != nil {
			return nil, err
		}
	} else if err := chaos.CopyRunInTx(ctx, tx, parent.ID, child.ID); err != nil {
		return nil, err
	}
	if err := store.AppendEventTx(ctx, tx, child.ID, "execution.forked", map[string]any{
		"parent_run_id": parent.ID, "fork_event_seq": selected.EventSeq, "checkpoint_id": selected.ID, "manifest_digest": manifest.Digest,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	ok = true
	return target, nil
}
