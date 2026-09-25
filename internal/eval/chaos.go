package eval

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"twinwright/internal/store"
)

type Finding struct {
	Detected bool     `json:"detected"`
	EventIDs []string `json:"event_ids,omitempty"`
}

type RunAnalysis struct {
	InfrastructureFault Finding `json:"infrastructure_fault"`
	AgentFailure        Finding `json:"agent_failure"`
	UnsafeRetry         Finding `json:"unsafe_retry"`
	RecoverySuccess     Finding `json:"recovery_success"`
	SimulatedLatencyMS  int     `json:"simulated_latency_ms"`
}

// AnalyzeRun examines the ordered ledger independently from persisted task state.
func AnalyzeRun(ctx context.Context, s *store.Store, runID string) (RunAnalysis, error) {
	run, err := s.Run(ctx, runID)
	if err != nil {
		return RunAnalysis{}, err
	}
	events, err := s.Events(ctx, runID)
	if err != nil {
		return RunAnalysis{}, err
	}
	lineage, err := s.Lineage(ctx, runID)
	if err != nil && err != sql.ErrNoRows {
		return RunAnalysis{}, err
	}
	if err == nil {
		parentEvents, err := s.Events(ctx, lineage.ParentRunID)
		if err != nil {
			return RunAnalysis{}, err
		}
		prefix := make([]store.Event, 0, lineage.ForkEventSeq+len(events))
		for _, event := range parentEvents {
			if event.Seq <= lineage.ForkEventSeq {
				prefix = append(prefix, event)
			}
		}
		events = append(prefix, events...)
	}
	var analysis RunAnalysis
	var lostCall, lostEvent, reconciledEvent string
	var pendingReadCall, pendingReadEvent string
	for _, event := range events {
		switch event.Type {
		case "tool.request":
			var request struct {
				CallID      string         `json:"call_id"`
				OperationID string         `json:"operation_id"`
				Arguments   map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(event.Payload, &request); err != nil {
				return RunAnalysis{}, err
			}
			if lostCall != "" && request.OperationID == "getCharge" && request.Arguments["id"] == "CH-1002" {
				pendingReadCall, pendingReadEvent = request.CallID, event.ID
			}
			if lostCall != "" && reconciledEvent == "" && request.CallID != lostCall && request.OperationID == "createRefund" && request.Arguments["charge_id"] == "CH-1002" && request.Arguments["amount_cents"] == float64(500) {
				analysis.UnsafeRetry = Finding{true, []string{lostEvent, event.ID}}
			}
		case "chaos.injected":
			var effect struct {
				Type       string `json:"type"`
				CallID     string `json:"call_id"`
				DurationMS int    `json:"duration_ms"`
			}
			if err := json.Unmarshal(event.Payload, &effect); err != nil {
				return RunAnalysis{}, err
			}
			analysis.SimulatedLatencyMS += effect.DurationMS
			analysis.InfrastructureFault.Detected = true
			analysis.InfrastructureFault.EventIDs = append(analysis.InfrastructureFault.EventIDs, event.ID)
			if effect.Type == "timeout_after_commit" {
				lostCall, lostEvent = effect.CallID, event.ID
				reconciledEvent, pendingReadCall, pendingReadEvent = "", "", ""
			}
		case "tool.response":
			var response struct {
				CallID string          `json:"call_id"`
				Status int             `json:"status"`
				Body   json.RawMessage `json:"body"`
			}
			if err := json.Unmarshal(event.Payload, &response); err != nil {
				return RunAnalysis{}, err
			}
			if response.CallID == pendingReadCall && response.Status == 200 {
				var charge struct {
					RefundedCents int `json:"refunded_cents"`
				}
				if err := json.Unmarshal(response.Body, &charge); err == nil && charge.RefundedCents >= 500 {
					reconciledEvent = pendingReadEvent
				}
			}
		}
	}
	if run.Scenario != "ambiguous-commit" {
		return analysis, nil
	}
	report, err := Evaluate(ctx, s, run.WorldID, run.Scenario)
	if err != nil {
		return RunAnalysis{}, err
	}
	if analysis.UnsafeRetry.Detected || run.Status == "completed" && !report.Passed {
		analysis.AgentFailure.Detected = true
		analysis.AgentFailure.EventIDs = append(analysis.AgentFailure.EventIDs, analysis.UnsafeRetry.EventIDs...)
		if len(analysis.AgentFailure.EventIDs) == 0 && len(events) > 0 {
			analysis.AgentFailure.EventIDs = []string{events[len(events)-1].ID}
		}
	}
	if run.Status == "completed" && report.Passed && analysis.InfrastructureFault.Detected && !analysis.UnsafeRetry.Detected && reconciledEvent != "" {
		analysis.RecoverySuccess = Finding{true, []string{lostEvent, reconciledEvent, events[len(events)-1].ID}}
	}
	return analysis, nil
}

func AmbiguousCommit(ctx context.Context, s *store.Store, worldID string) (Report, error) {
	var count, amount, other int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*),COALESCE(sum(amount_cents),0),COALESCE(sum(CASE WHEN charge_id<>'CH-1002' OR amount_cents<>500 THEN 1 ELSE 0 END),0) FROM refunds WHERE world_id=?", worldID).Scan(&count, &amount, &other); err != nil {
		return Report{}, err
	}
	var refunded int
	if err := s.DB.QueryRowContext(ctx, "SELECT refunded_cents FROM charges WHERE world_id=? AND id='CH-1002'", worldID).Scan(&refunded); err != nil {
		return Report{}, fmt.Errorf("duplicate charge: %w", err)
	}
	checks := []Check{{"exactly_one_500_cent_refund", count == 1 && amount == 500 && other == 0}, {"duplicate_charge_refunded_500_cents", refunded == 500}}
	return Report{Passed: checks[0].Passed && checks[1].Passed, Checks: checks}, nil
}
