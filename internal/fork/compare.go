package fork

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"twinwright/internal/eval"
	"twinwright/internal/store"
)

type Availability struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

type ToolStep struct {
	CallID      string          `json:"call_id"`
	OperationID string          `json:"operation_id"`
	Arguments   json.RawMessage `json:"arguments"`
}

type RunOutcome struct {
	RunID          string            `json:"run_id"`
	Status         string            `json:"status"`
	Evaluation     *eval.Report      `json:"evaluation,omitempty"`
	ToolTrajectory []ToolStep        `json:"tool_trajectory"`
	Mutations      []json.RawMessage `json:"mutations"`
}

type StateDifference struct {
	Table      string            `json:"table"`
	ParentOnly []json.RawMessage `json:"parent_only"`
	ChildOnly  []json.RawMessage `json:"child_only"`
}

type Comparison struct {
	ParentRunID      string            `json:"parent_run_id"`
	ChildRunID       string            `json:"child_run_id"`
	ForkEventSeq     int               `json:"fork_event_seq"`
	Parent           RunOutcome        `json:"parent"`
	Child            RunOutcome        `json:"child"`
	StateDifferences []StateDifference `json:"state_differences"`
	Latency          Availability      `json:"latency"`
	ModelUsage       Availability      `json:"model_usage"`
}

// Compare reports the parent's suffix and the child's new execution after a fork.
func Compare(ctx context.Context, s *store.Store, parentRunID, childRunID string) (Comparison, error) {
	lineage, err := s.Lineage(ctx, childRunID)
	if err != nil {
		return Comparison{}, err
	}
	if lineage.ParentRunID != parentRunID {
		return Comparison{}, fmt.Errorf("run %s is not a fork of %s", childRunID, parentRunID)
	}
	parent, err := s.Run(ctx, parentRunID)
	if err != nil {
		return Comparison{}, err
	}
	child, err := s.Run(ctx, childRunID)
	if err != nil {
		return Comparison{}, err
	}
	parentOutcome, err := outcome(ctx, s, parent, lineage.ForkEventSeq)
	if err != nil {
		return Comparison{}, err
	}
	childOutcome, err := outcome(ctx, s, child, 1)
	if err != nil {
		return Comparison{}, err
	}
	report := Comparison{
		ParentRunID: parentRunID, ChildRunID: childRunID, ForkEventSeq: lineage.ForkEventSeq,
		Parent: parentOutcome, Child: childOutcome,
		Latency: Availability{Reason: "latency not recorded"}, ModelUsage: Availability{Reason: "model usage not recorded"},
	}
	for _, table := range worldTables {
		parentRows, err := compareRows(ctx, s.DB, table, parent.WorldID)
		if err != nil {
			return Comparison{}, err
		}
		childRows, err := compareRows(ctx, s.DB, table, child.WorldID)
		if err != nil {
			return Comparison{}, err
		}
		leftOnly, rightOnly := rowDifference(parentRows, childRows)
		if len(leftOnly) > 0 || len(rightOnly) > 0 {
			report.StateDifferences = append(report.StateDifferences, StateDifference{Table: table.name, ParentOnly: leftOnly, ChildOnly: rightOnly})
		}
	}
	return report, nil
}

func outcome(ctx context.Context, s *store.Store, run store.Run, afterSeq int) (RunOutcome, error) {
	result := RunOutcome{RunID: run.ID, Status: run.Status, ToolTrajectory: []ToolStep{}, Mutations: []json.RawMessage{}}
	if run.Status == "completed" {
		report, err := eval.Evaluate(ctx, s, run.WorldID, run.Scenario)
		if err != nil {
			return RunOutcome{}, err
		}
		result.Evaluation = &report
	}
	events, err := s.Events(ctx, run.ID)
	if err != nil {
		return RunOutcome{}, err
	}
	for _, event := range events {
		if event.Seq <= afterSeq {
			continue
		}
		switch event.Type {
		case "tool.request":
			var step ToolStep
			if err := json.Unmarshal(event.Payload, &step); err != nil {
				return RunOutcome{}, err
			}
			result.ToolTrajectory = append(result.ToolTrajectory, step)
		case "state.mutation":
			result.Mutations = append(result.Mutations, append(json.RawMessage(nil), event.Payload...))
		}
	}
	return result, nil
}

func compareRows(ctx context.Context, db *sql.DB, table tableSpec, worldID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT "+strings.Join(table.columns, ",")+" FROM "+table.name+" WHERE world_id=?", worldID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		values := make([]any, len(table.columns))
		pointers := make([]any, len(values))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		for i, value := range values {
			if bytes, ok := value.([]byte); ok {
				values[i] = string(bytes)
			}
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			return nil, err
		}
		result = append(result, string(encoded))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(result)
	return result, nil
}

func rowDifference(parent, child []string) ([]json.RawMessage, []json.RawMessage) {
	var parentOnly, childOnly []json.RawMessage
	i, j := 0, 0
	for i < len(parent) || j < len(child) {
		switch {
		case j == len(child) || (i < len(parent) && parent[i] < child[j]):
			parentOnly = append(parentOnly, json.RawMessage(parent[i]))
			i++
		case i == len(parent) || child[j] < parent[i]:
			childOnly = append(childOnly, json.RawMessage(child[j]))
			j++
		default:
			i++
			j++
		}
	}
	return parentOnly, childOnly
}
