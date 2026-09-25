package assertion

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"twinwright/internal/store"
)

// Result is one assertion's outcome with ledger evidence when relevant.
type Result struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Passed   bool     `json:"passed"`
	Detail   string   `json:"detail,omitempty"`
	EventIDs []string `json:"event_ids,omitempty"`
}

type Report struct {
	Passed  bool     `json:"passed"`
	Digest  string   `json:"digest"`
	Results []Result `json:"results"`
}

// Check evaluates every assertion against one run's world and ledger. It only reads.
func Check(ctx context.Context, s *store.Store, runID string, set Set) (Report, error) {
	run, err := s.Run(ctx, runID)
	if err != nil {
		return Report{}, err
	}
	report := Report{Passed: true, Digest: set.Digest()}
	for _, a := range set.Assertions {
		result := Result{ID: a.ID, Type: a.Type}
		switch a.Type {
		case "row_count":
			err = rowCount(ctx, s.DB, run.WorldID, a, &result)
		case "field_equals":
			err = fieldEquals(ctx, s.DB, run.WorldID, a, &result)
		case "relationship":
			err = relationship(ctx, s.DB, run.WorldID, a, &result)
		default:
			err = fmt.Errorf("assertion type %s is not implemented", a.Type)
		}
		if err != nil {
			return Report{}, fmt.Errorf("assertion %s: %w", a.ID, err)
		}
		report.Passed = report.Passed && result.Passed
		report.Results = append(report.Results, result)
	}
	return report, nil
}

func (c Count) holds(n int) bool {
	switch c.Op {
	case "equals":
		return n == c.N
	case "at_least":
		return n >= c.N
	}
	return n <= c.N
}

func (c Count) judge(n int, result *Result) {
	result.Passed = c.holds(n)
	if !result.Passed {
		result.Detail = fmt.Sprintf("count %d, want %s %d", n, c.Op, c.N)
	}
}

func rowCount(ctx context.Context, db *sql.DB, worldID string, a Assertion, result *Result) error {
	query := "SELECT count(*) FROM " + a.Table + " WHERE world_id=?"
	args := []any{worldID}
	columns := make([]string, 0, len(a.Where))
	for column := range a.Where {
		columns = append(columns, column)
	}
	sort.Strings(columns)
	for _, column := range columns {
		condition := a.Where[column]
		if condition.Contains != "" {
			query += " AND instr(" + column + ",?)>0"
			args = append(args, condition.Contains)
		} else {
			query += " AND " + column + "=?"
			args = append(args, condition.Equals)
		}
	}
	var n int
	if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return err
	}
	a.Count.judge(n, result)
	return nil
}

func lookup(ctx context.Context, db *sql.DB, worldID string, r Ref, field string) (any, bool, error) {
	var value any
	err := db.QueryRowContext(ctx, "SELECT "+field+" FROM "+r.Table+" WHERE world_id=? AND id=?", worldID, r.ID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return value, err == nil, err
}

func fieldEquals(ctx context.Context, db *sql.DB, worldID string, a Assertion, result *Result) error {
	actual, found, err := lookup(ctx, db, worldID, *a.Entity, a.Field)
	if err != nil {
		return err
	}
	if !found {
		result.Detail = fmt.Sprintf("%s.%s not found", a.Entity.Table, a.Entity.ID)
		return nil
	}
	want := a.Value
	if a.ValueFrom != nil {
		if want, found, err = lookup(ctx, db, worldID, *a.ValueFrom, a.ValueFrom.Field); err != nil {
			return err
		}
		if !found {
			result.Detail = fmt.Sprintf("%s.%s not found", a.ValueFrom.Table, a.ValueFrom.ID)
			return nil
		}
	}
	result.Passed = fmt.Sprint(actual) == fmt.Sprint(want)
	if !result.Passed {
		result.Detail = fmt.Sprintf("got %q, want %q", fmt.Sprint(actual), fmt.Sprint(want))
	}
	return nil
}

func relationship(ctx context.Context, db *sql.DB, worldID string, a Assertion, result *Result) error {
	rows, err := db.QueryContext(ctx, "SELECT DISTINCT t."+a.Field+" FROM "+a.Table+" t WHERE t.world_id=? AND NOT EXISTS (SELECT 1 FROM "+a.References+" r WHERE r.world_id=t.world_id AND r.id=t."+a.Field+") ORDER BY 1", worldID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var dangling []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return err
		}
		dangling = append(dangling, value)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	result.Passed = len(dangling) == 0
	if !result.Passed {
		result.Detail = fmt.Sprintf("%s.%s values missing from %s: %s", a.Table, a.Field, a.References, strings.Join(dangling, ", "))
	}
	return nil
}
