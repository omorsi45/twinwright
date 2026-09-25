package chaos

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

var runStateTables = []struct {
	name    string
	columns []string
}{
	{"run_chaos", []string{"policy_json", "digest"}},
	{"chaos_rule_state", []string{"rule_id", "matching_calls", "injections"}},
	{"chaos_snapshots", []string{"rule_id", "arguments_digest", "status", "body"}},
	{"chaos_hidden_outcomes", []string{"call_id", "rule_id", "status", "body"}},
}

// CopyRun copies only the reconstructed prefix's chaos state into a new child.
func CopyRun(ctx context.Context, source *sql.DB, target *sql.Tx, from, to string) error {
	for _, table := range runStateTables {
		columns := strings.Join(table.columns, ",")
		rows, err := source.QueryContext(ctx, "SELECT "+columns+" FROM "+table.name+" WHERE run_id=?", from)
		if err != nil {
			return fmt.Errorf("read %s: %w", table.name, err)
		}
		query := "INSERT INTO " + table.name + "(run_id," + columns + ") VALUES(" + strings.TrimSuffix(strings.Repeat("?,", len(table.columns)+1), ",") + ")"
		for rows.Next() {
			values := make([]any, len(table.columns))
			ptrs := make([]any, len(values))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err = rows.Scan(ptrs...); err != nil {
				break
			}
			_, err = target.ExecContext(ctx, query, append([]any{to}, values...)...)
			if err != nil {
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
		closeErr := rows.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("copy %s: %w", table.name, err)
		}
	}
	return nil
}

// CopyRunInTx copies state when parent and child are in one database transaction.
func CopyRunInTx(ctx context.Context, tx *sql.Tx, from, to string) error {
	for _, table := range runStateTables {
		columns := strings.Join(table.columns, ",")
		query := "INSERT INTO " + table.name + "(run_id," + columns + ") SELECT ?," + columns + " FROM " + table.name + " WHERE run_id=?"
		if _, err := tx.ExecContext(ctx, query, to, from); err != nil {
			return fmt.Errorf("copy %s: %w", table.name, err)
		}
	}
	return nil
}
