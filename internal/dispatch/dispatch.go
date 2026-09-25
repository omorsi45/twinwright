package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"twinwright/internal/billing"
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
	var mutation *billing.Mutation
	if status, response = validate(*op, args); status == 0 {
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
			status, response, mutation, err = billing.Handle(ctx, tx, worldID, runID, callID, op.Behavior, args)
			if err != nil {
				return Result{}, err
			}
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

func validate(op compiler.Operation, args map[string]any) (int, any) {
	for _, name := range op.Required {
		if _, ok := args[name]; !ok {
			return 400, map[string]string{"error": "missing " + name}
		}
	}
	for name, value := range args {
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
