package checkpoint

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"twinwright/internal/agent"
	"twinwright/internal/authz"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

// Reconstruct verifies a recorded prefix and reproduces its state in memory.
// The caller owns the returned store and must close it.
func Reconstruct(ctx context.Context, source *store.Store, runID string, selected Checkpoint, manifest compiler.Manifest) (*store.Store, store.Run, error) {
	points, err := List(ctx, source, runID, manifest)
	if err != nil {
		return nil, store.Run{}, err
	}
	current, err := Select(points, selected.EventSeq)
	if err != nil {
		return nil, store.Run{}, err
	}
	if current != selected {
		return nil, store.Run{}, fmt.Errorf("checkpoint %s no longer matches source prefix", selected.ID)
	}
	original, err := source.Run(ctx, runID)
	if err != nil {
		return nil, store.Run{}, err
	}
	var seed int64
	var digest string
	if err := source.DB.QueryRowContext(ctx, "SELECT seed,digest FROM worlds WHERE id=?", original.WorldID).Scan(&seed, &digest); err != nil {
		return nil, store.Run{}, err
	}
	events, err := source.Events(ctx, runID)
	if err != nil {
		return nil, store.Run{}, err
	}
	target, err := store.Open(":memory:")
	if err != nil {
		return nil, store.Run{}, err
	}
	success := false
	defer func() {
		if !success {
			target.Close()
		}
	}()
	world, err := target.SeedScenario(ctx, seed, digest, original.Scenario)
	if err != nil {
		return nil, store.Run{}, err
	}
	if _, err := target.CreateReplayRun(ctx, original, world.ID); err != nil {
		return nil, store.Run{}, err
	}
	policyJSON, policyDigest, policyErr := source.ChaosPolicy(ctx, runID)
	if policyErr != nil && policyErr != sql.ErrNoRows {
		return nil, store.Run{}, policyErr
	}
	if policyErr == nil {
		if err := target.AttachChaos(ctx, runID, policyJSON, policyDigest); err != nil {
			return nil, store.Run{}, err
		}
	}
	authJSON, authDigest, authErr := source.AuthPolicy(ctx, runID)
	if authErr != nil && authErr != sql.ErrNoRows {
		return nil, store.Run{}, authErr
	}
	if authErr == nil {
		if err := target.AttachAuth(ctx, runID, authJSON, authDigest); err != nil {
			return nil, store.Run{}, err
		}
	}
	d := dispatch.Dispatcher{Store: target, Manifest: manifest}
	var history []agent.Message
	step := 0
	for i := 1; i < selected.EventSeq; i++ {
		event := events[i]
		switch event.Type {
		case "model.request":
			current, err := target.Run(ctx, runID)
			if err != nil {
				return nil, store.Run{}, err
			}
			var requestHistory []agent.Message
			if err := json.Unmarshal([]byte(current.Transcript), &requestHistory); err != nil {
				return nil, store.Run{}, err
			}
			operations, err := authz.Exposed(ctx, target.DB, runID, manifest.Operations)
			if err != nil {
				return nil, store.Run{}, err
			}
			request := map[string]any{"task": current.Task, "provider": current.Provider, "model": current.Model, "history": requestHistory, "operations": operations}
			encoded, err := json.Marshal(request)
			if err != nil {
				return nil, store.Run{}, err
			}
			if !equalPayload(event.Payload, encoded) {
				return nil, store.Run{}, fmt.Errorf("model request differs from reconstructed context at event %d", event.Seq)
			}
			if err := target.StartModelCall(ctx, runID, request); err != nil {
				return nil, store.Run{}, err
			}
		case "model.response":
			var message agent.Message
			if err := decodeInto(event.Payload, &message); err != nil {
				return nil, store.Run{}, err
			}
			history = append(history, message)
			transcript, err := json.Marshal(history)
			if err != nil {
				return nil, store.Run{}, err
			}
			step++
			if err := target.SaveTurn(ctx, runID, step, string(transcript), message); err != nil {
				return nil, store.Run{}, err
			}
		case "tool.request":
			var request struct {
				CallID      string         `json:"call_id"`
				OperationID string         `json:"operation_id"`
				Arguments   map[string]any `json:"arguments"`
			}
			if err := decodeInto(event.Payload, &request); err != nil {
				return nil, store.Run{}, err
			}
			if _, err := d.Invoke(ctx, runID, request.CallID, request.OperationID, request.Arguments); err != nil {
				return nil, store.Run{}, fmt.Errorf("replaying tool call %s: %w", request.CallID, err)
			}
			for i+1 < selected.EventSeq && events[i].Type != "tool.response" {
				i++
			}
			if events[i].Type != "tool.response" {
				return nil, store.Run{}, fmt.Errorf("tool call %s has no response before checkpoint", request.CallID)
			}
			currentRun, err := target.Run(ctx, runID)
			if err != nil {
				return nil, store.Run{}, err
			}
			if err := decodeInto([]byte(currentRun.Transcript), &history); err != nil {
				return nil, store.Run{}, err
			}
		case "execution.paused", "execution.completed":
			payload, err := decodePayload(event.Payload)
			if err != nil {
				return nil, store.Run{}, err
			}
			status := "paused"
			if event.Type == "execution.completed" {
				status = "completed"
			}
			if err := target.SaveStatus(ctx, runID, status, event.Type, payload); err != nil {
				return nil, store.Run{}, err
			}
		default:
			return nil, store.Run{}, fmt.Errorf("unexpected standalone event %q at %d", event.Type, event.Seq)
		}
	}
	generated, err := target.Events(ctx, runID)
	if err != nil {
		return nil, store.Run{}, err
	}
	if len(generated) != selected.EventSeq {
		return nil, store.Run{}, fmt.Errorf("checkpoint event count differs: recorded %d, replayed %d", selected.EventSeq, len(generated))
	}
	for i := 1; i < selected.EventSeq; i++ {
		left, right := events[i], generated[i]
		if left.Type != right.Type || left.WorldAt != right.WorldAt || !equalPayload(left.Payload, right.Payload) {
			return nil, store.Run{}, fmt.Errorf("checkpoint replay diverged at event %d", i+1)
		}
		if left.Type == "tool.request" {
			var request struct {
				CallID string `json:"call_id"`
			}
			if err := json.Unmarshal(left.Payload, &request); err != nil {
				return nil, store.Run{}, err
			}
			if err := compareToolResult(ctx, source.DB, target.DB, runID, request.CallID); err != nil {
				return nil, store.Run{}, err
			}
		}
	}
	rebuilt, err := target.Run(ctx, runID)
	if err != nil {
		return nil, store.Run{}, err
	}
	if err := compareTranscriptPrefix(original.Transcript, rebuilt.Transcript); err != nil {
		return nil, store.Run{}, err
	}
	success = true
	return target, rebuilt, nil
}

func decodeInto(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(value)
}

func decodePayload(raw []byte) (any, error) {
	var value any
	err := decodeInto(raw, &value)
	return value, err
}

func equalPayload(left, right []byte) bool {
	a, errA := canonicalJSON(left)
	b, errB := canonicalJSON(right)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

func compareTranscriptPrefix(source, rebuilt string) error {
	var sourceMessages, rebuiltMessages []json.RawMessage
	if err := json.Unmarshal([]byte(source), &sourceMessages); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(rebuilt), &rebuiltMessages); err != nil {
		return err
	}
	if len(sourceMessages) < len(rebuiltMessages) {
		return fmt.Errorf("source transcript is shorter than checkpoint transcript")
	}
	for i := range rebuiltMessages {
		if !equalPayload(sourceMessages[i], rebuiltMessages[i]) {
			return fmt.Errorf("source transcript differs at message %d", i+1)
		}
	}
	return nil
}

func compareToolResult(ctx context.Context, source, target *sql.DB, runID, callID string) error {
	read := func(db *sql.DB) (string, string, int, string, error) {
		var operation, arguments, body string
		var status int
		err := db.QueryRowContext(ctx, "SELECT operation_id,arguments,status,body FROM tool_results WHERE run_id=? AND call_id=?", runID, callID).Scan(&operation, &arguments, &status, &body)
		return operation, arguments, status, body, err
	}
	leftOp, leftArgs, leftStatus, leftBody, err := read(source)
	if err != nil {
		return err
	}
	rightOp, rightArgs, rightStatus, rightBody, err := read(target)
	if err != nil {
		return err
	}
	if leftOp != rightOp || leftStatus != rightStatus || !equalPayload([]byte(leftArgs), []byte(rightArgs)) || !equalPayload([]byte(leftBody), []byte(rightBody)) {
		return fmt.Errorf("checkpoint tool result differs for call %s", callID)
	}
	return nil
}
