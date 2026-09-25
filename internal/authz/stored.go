package authz

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"twinwright/internal/compiler"
)

// ValidateStored accepts only canonical JSON that Parse would produce for this
// world, with a matching digest.
func ValidateStored(encoded []byte, digest string, manifest compiler.Manifest) (Policy, error) {
	policy, err := Parse(encoded, manifest)
	if err != nil {
		return Policy{}, fmt.Errorf("stored authorization policy fails validation: %w", err)
	}
	canonical, err := policy.CanonicalJSON()
	if err != nil || !bytes.Equal(canonical, encoded) || policy.Digest() != digest {
		return Policy{}, fmt.Errorf("stored authorization policy is not canonical or digest differs")
	}
	return policy, nil
}

// CopyRun gives a fork child the policy and call counter of a reconstructed prefix.
func CopyRun(ctx context.Context, source querier, target *sql.Tx, from, to string) error {
	var encoded, digest string
	err := source.QueryRowContext(ctx, "SELECT policy_json,digest FROM run_auth WHERE run_id=?", from).Scan(&encoded, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := target.ExecContext(ctx, "INSERT INTO run_auth(run_id,policy_json,digest) VALUES(?,?,?)", to, encoded, digest); err != nil {
		return err
	}
	var call int
	err = source.QueryRowContext(ctx, "SELECT call_index FROM auth_state WHERE run_id=?", from).Scan(&call)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = target.ExecContext(ctx, "INSERT INTO auth_state(run_id,call_index) VALUES(?,?)", to, call)
	return err
}
