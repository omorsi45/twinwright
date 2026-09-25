package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"twinwright/internal/behavior"
	"twinwright/internal/chaos"
	"twinwright/internal/compiler"
	"twinwright/internal/store"
)

type Result struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}
type Dispatcher struct {
	Store    *store.Store
	Manifest compiler.Manifest
	Registry *behavior.Registry
}

func (d *Dispatcher) Invoke(ctx context.Context, runID, callID, operationID string, args map[string]any) (Result, error) {
	tx, err := d.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()
	arguments, err := json.Marshal(args)
	if err != nil {
		return Result{}, err
	}
	var savedOp, savedArgs, body string
	var status int
	err = tx.QueryRowContext(ctx, "SELECT operation_id,arguments,status,body FROM tool_results WHERE run_id=? AND call_id=?", runID, callID).Scan(&savedOp, &savedArgs, &status, &body)
	if err == nil {
		if savedOp != operationID || savedArgs != string(arguments) {
			return Result{}, fmt.Errorf("call ID %s reused with different request", callID)
		}
		return Result{status, json.RawMessage(body)}, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Result{}, err
	}
	op := d.Manifest.Operation(operationID)
	if op == nil {
		return Result{}, fmt.Errorf("operation %s is not compiled", operationID)
	}
	var worldID, transcript, fault string
	var consumed int
	if err = tx.QueryRowContext(ctx, "SELECT world_id,transcript,fault_operation,fault_consumed FROM runs WHERE id=?", runID).Scan(&worldID, &transcript, &fault, &consumed); err != nil {
		return Result{}, err
	}
	if err = store.AppendEventTx(ctx, tx, runID, "tool.request", map[string]any{"call_id": callID, "operation_id": operationID, "arguments": args}); err != nil {
		return Result{}, err
	}
	var response any
	var mutation any
	var decision chaos.Decision
	var chaosPayload map[string]any
	var postEffect, actorPending, handlerRan bool
	status, response = validate(*op, args)
	skipHandler := status != 0
	if !skipHandler {
		var decideErr error
		decision, decideErr = chaos.Decide(ctx, tx, runID, operationID, arguments)
		if decideErr != nil {
			return Result{}, decideErr
		}
		if decision.RuleID != "" {
			chaosPayload = map[string]any{"rule_id": decision.RuleID, "type": decision.Rule.Type, "call_id": callID,
				"operation_id": operationID, "matching_call": decision.MatchNumber}
			switch decision.Rule.Type {
			case "http_error":
				status, response, skipHandler = decision.Rule.Status, map[string]string{"error": "simulated HTTP failure"}, true
				chaosPayload["status"] = status
			case "timeout":
				status, response, skipHandler = 0, map[string]string{"error": "simulated transport timeout"}, true
			case "rate_limit":
				status, response, skipHandler = 429, map[string]string{"error": "simulated rate limit"}, true
				chaosPayload["status"] = status
			case "permission_revocation":
				status, response, skipHandler = 403, map[string]string{"error": "simulated permission revocation"}, true
				chaosPayload["status"] = status
			case "partial_service_outage":
				status, response, skipHandler = 503, map[string]string{"error": "simulated service outage"}, true
				chaosPayload["status"] = status
			case "latency":
				chaosPayload["duration_ms"] = decision.Rule.DurationMS
			case "stale_read":
				var staleBody []byte
				status, staleBody, err = chaos.LoadSnapshot(ctx, tx, runID, decision.RuleID, arguments)
				if err != nil {
					return Result{}, err
				}
				response, skipHandler = json.RawMessage(staleBody), true
			case "timeout_after_commit", "malformed_response":
				postEffect = true
			case "concurrent_mutation":
				actorPending = true
			default:
				return Result{}, fmt.Errorf("chaos effect %s is not implemented", decision.Rule.Type)
			}
			if !postEffect {
				if err = store.AppendEventTx(ctx, tx, runID, "chaos.injected", chaosPayload); err != nil {
					return Result{}, err
				}
			}
			if actorPending {
				if err := d.invokeActor(ctx, tx, worldID, runID, decision); err != nil {
					return Result{}, err
				}
			}
		}
	}
	if !skipHandler {
		if fault == operationID && consumed == 0 {
			status = 503
			response = map[string]string{"error": "injected temporary unavailability"}
			if _, err = tx.ExecContext(ctx, "UPDATE runs SET fault_consumed=1 WHERE id=?", runID); err != nil {
				return Result{}, err
			}
			if err = store.AppendEventTx(ctx, tx, runID, "error", map[string]any{"call_id": callID, "kind": "injected_503"}); err != nil {
				return Result{}, err
			}
		} else {
			if fault == operationID && consumed == 1 {
				var retries int
				if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE run_id=? AND type='retry'", runID).Scan(&retries); err != nil {
					return Result{}, err
				}
				if retries == 0 {
					if err = store.AppendEventTx(ctx, tx, runID, "retry", map[string]any{"call_id": callID, "operation_id": operationID}); err != nil {
						return Result{}, err
					}
				}
			}
			registry := d.Registry
			if registry == nil {
				registry = behavior.Builtin()
			}
			handler, ok := registry.Lookup(op.Behavior)
			if !ok {
				return Result{}, fmt.Errorf("unsupported behavior %q", op.Behavior)
			}
			status, response, mutation, err = handler(ctx, tx, worldID, runID, callID, op.Behavior, args)
			if err != nil {
				return Result{}, err
			}
			handlerRan = true
		}
	}
	if handlerRan && status >= 200 && status < 300 && op.Method == "GET" {
		actual, marshalErr := json.Marshal(response)
		if marshalErr != nil {
			return Result{}, marshalErr
		}
		for _, ruleID := range decision.CaptureRules {
			if err := chaos.SaveSnapshot(ctx, tx, runID, ruleID, arguments, status, actual); err != nil {
				return Result{}, err
			}
		}
	}
	if postEffect {
		if !handlerRan {
			return Result{}, fmt.Errorf("chaos rule %s handler did not run", decision.RuleID)
		}
		if decision.Rule.Type == "timeout_after_commit" && (status < 200 || status >= 300 || mutation == nil) {
			if _, err := tx.ExecContext(ctx, "UPDATE chaos_rule_state SET injections=injections-1 WHERE run_id=? AND rule_id=?", runID, decision.RuleID); err != nil {
				return Result{}, err
			}
			postEffect = false
		}
	}
	if postEffect {
		actual, marshalErr := json.Marshal(response)
		if marshalErr != nil {
			return Result{}, marshalErr
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO chaos_hidden_outcomes(run_id,call_id,rule_id,status,body) VALUES(?,?,?,?,?)", runID, callID, decision.RuleID, status, string(actual)); err != nil {
			return Result{}, err
		}
		if decision.Rule.Type == "timeout_after_commit" {
			status, response = 0, map[string]string{"error": "simulated transport timeout"}
		} else {
			response = decision.Rule.Body
		}
		if err := store.AppendEventTx(ctx, tx, runID, "chaos.injected", chaosPayload); err != nil {
			return Result{}, err
		}
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return Result{}, err
	}
	result := Result{Status: status, Body: encoded}
	if mutation != nil {
		if err = store.AppendEventTx(ctx, tx, runID, "state.mutation", mutation); err != nil {
			return Result{}, err
		}
	}
	if err = store.AppendEventTx(ctx, tx, runID, "tool.response", map[string]any{"call_id": callID, "operation_id": operationID, "status": status, "body": response}); err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO tool_results(run_id,call_id,operation_id,arguments,status,body) VALUES(?,?,?,?,?,?)", runID, callID, operationID, string(arguments), status, string(encoded)); err != nil {
		return Result{}, err
	}
	var messages []map[string]any
	if err = json.Unmarshal([]byte(transcript), &messages); err != nil {
		return Result{}, err
	}
	messages = append(messages, map[string]any{"role": "tool", "call_id": callID, "operation_id": operationID, "status": status, "content": string(encoded)})
	next, err := json.Marshal(messages)
	if err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE runs SET transcript=? WHERE id=?", string(next), runID); err != nil {
		return Result{}, err
	}
	if err = tx.Commit(); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (d *Dispatcher) invokeActor(ctx context.Context, tx *sql.Tx, worldID, runID string, decision chaos.Decision) error {
	actor := decision.Rule.Actor
	if actor == nil {
		return fmt.Errorf("chaos rule %s has no actor", decision.RuleID)
	}
	op := d.Manifest.Operation(actor.Operation)
	if op == nil || op.Method != "POST" {
		return fmt.Errorf("chaos actor operation %q is unavailable", actor.Operation)
	}
	registry := d.Registry
	if registry == nil {
		registry = behavior.Builtin()
	}
	handler, ok := registry.Lookup(op.Behavior)
	if !ok {
		return fmt.Errorf("chaos actor behavior %q is unavailable", op.Behavior)
	}
	actorCallID := decision.RuleID + "-" + strconv.Itoa(decision.MatchNumber)
	status, _, mutation, err := handler(ctx, tx, worldID, runID+"-chaos-actor", actorCallID, op.Behavior, actor.Arguments)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 || mutation == nil {
		return fmt.Errorf("chaos actor %s did not mutate successfully: HTTP %d", actor.Operation, status)
	}
	return store.AppendEventTx(ctx, tx, runID, "chaos.actor_mutation", map[string]any{"rule_id": decision.RuleID, "operation_id": actor.Operation, "status": status, "mutation": mutation})
}

func validate(op compiler.Operation, args map[string]any) (int, any) {
	for _, name := range op.Required {
		if _, ok := args[name]; !ok {
			return 400, map[string]string{"error": "missing " + name}
		}
	}
	names := make([]string, 0, len(args))
	for name := range args {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := args[name]
		typ, ok := op.Properties[name]
		if !ok {
			return 400, map[string]string{"error": "unexpected " + name}
		}
		switch typ {
		case "string":
			s, ok := value.(string)
			if !ok || s == "" {
				return 400, map[string]string{"error": "invalid " + name}
			}
		case "integer":
			switch n := value.(type) {
			case int, int64, json.Number:
			case float64:
				if n != float64(int64(n)) {
					return 400, map[string]string{"error": "invalid " + name}
				}
			default:
				return 400, map[string]string{"error": "invalid " + name}
			}
		}
	}
	return 0, nil
}
