package chaos

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

type Decision struct {
	RuleID       string
	Rule         Rule
	MatchNumber  int
	CaptureRules []string
}

// Decide advances each matching rule once and selects the first active effect.
// Call it only after validating arguments and checking saved call-ID results.
func Decide(ctx context.Context, tx *sql.Tx, runID, operationID string, arguments []byte) (Decision, error) {
	var encoded, digest string
	err := tx.QueryRowContext(ctx, "SELECT policy_json,digest FROM run_chaos WHERE run_id=?", runID).Scan(&encoded, &digest)
	if err == sql.ErrNoRows {
		return Decision{}, nil
	}
	if err != nil {
		return Decision{}, err
	}
	hash := sha256.Sum256([]byte(encoded))
	if hex.EncodeToString(hash[:]) != digest {
		return Decision{}, fmt.Errorf("chaos policy digest mismatch for run %s", runID)
	}
	var policy Policy
	if err := json.Unmarshal([]byte(encoded), &policy); err != nil {
		return Decision{}, err
	}
	if policy.Version != 1 {
		return Decision{}, fmt.Errorf("unsupported stored chaos policy version %d", policy.Version)
	}
	var selected Decision
	var captureRules []string
	for _, rule := range policy.Rules {
		matched := false
		for _, id := range rule.Operations {
			matched = matched || id == operationID
		}
		if !matched {
			continue
		}
		if rule.Type == "stale_read" {
			captureRules = append(captureRules, rule.ID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chaos_rule_state(run_id,rule_id,matching_calls,injections) VALUES(?,?,1,0) ON CONFLICT(run_id,rule_id) DO UPDATE SET matching_calls=matching_calls+1`, runID, rule.ID); err != nil {
			return Decision{}, err
		}
		var calls, injections int
		if err := tx.QueryRowContext(ctx, "SELECT matching_calls,injections FROM chaos_rule_state WHERE run_id=? AND rule_id=?", runID, rule.ID).Scan(&calls, &injections); err != nil {
			return Decision{}, err
		}
		active := selected.RuleID == "" && calls > rule.AfterCalls && (rule.Times == 0 || injections < rule.Times)
		if active && rule.Type == "stale_read" {
			_, _, snapshotErr := LoadSnapshot(ctx, tx, runID, rule.ID, arguments)
			if snapshotErr == sql.ErrNoRows {
				active = false
			} else if snapshotErr != nil {
				return Decision{}, snapshotErr
			}
		}
		if active {
			selected = Decision{RuleID: rule.ID, Rule: rule, MatchNumber: calls}
			if _, err := tx.ExecContext(ctx, "UPDATE chaos_rule_state SET injections=injections+1 WHERE run_id=? AND rule_id=?", runID, rule.ID); err != nil {
				return Decision{}, err
			}
		}
	}
	selected.CaptureRules = captureRules
	return selected, nil
}

func argumentDigest(arguments []byte) string {
	hash := sha256.Sum256(arguments)
	return hex.EncodeToString(hash[:])
}

// SaveSnapshot records the first successful observation for one argument set.
func SaveSnapshot(ctx context.Context, tx *sql.Tx, runID, ruleID string, arguments []byte, status int, body []byte) error {
	if !json.Valid(arguments) || !json.Valid(body) {
		return fmt.Errorf("invalid chaos snapshot JSON")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO chaos_snapshots(run_id,rule_id,arguments_digest,status,body) VALUES(?,?,?,?,?) ON CONFLICT(run_id,rule_id,arguments_digest) DO NOTHING`,
		runID, ruleID, argumentDigest(arguments), status, string(body))
	return err
}

func LoadSnapshot(ctx context.Context, tx *sql.Tx, runID, ruleID string, arguments []byte) (int, []byte, error) {
	if !json.Valid(arguments) {
		return 0, nil, fmt.Errorf("invalid chaos arguments JSON")
	}
	var status int
	var body string
	err := tx.QueryRowContext(ctx, "SELECT status,body FROM chaos_snapshots WHERE run_id=? AND rule_id=? AND arguments_digest=?", runID, ruleID, argumentDigest(arguments)).Scan(&status, &body)
	return status, []byte(body), err
}
