package fork

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"twinwright/internal/checkpoint"
	"twinwright/internal/store"
)

// prepareObservation checks that the override replaces the observation at the
// checkpoint itself and returns its canonical form and audit event payload.
func prepareObservation(events []store.Event, selected checkpoint.Checkpoint, o store.Observation) (store.Observation, map[string]any, error) {
	if o.CallID == "" {
		return store.Observation{}, nil, fmt.Errorf("observation override requires a call ID")
	}
	if o.Status != 0 && (o.Status < 100 || o.Status > 599) {
		return store.Observation{}, nil, fmt.Errorf("observation override status %d must be 0 or 100 to 599", o.Status)
	}
	body, err := canonicalBody(o.Body)
	if err != nil {
		return store.Observation{}, nil, fmt.Errorf("observation override body: %w", err)
	}
	if selected.EventType != "tool.response" || selected.EventSeq < 1 || selected.EventSeq > len(events) {
		return store.Observation{}, nil, fmt.Errorf("observation override requires a tool response checkpoint")
	}
	var response struct {
		CallID      string `json:"call_id"`
		OperationID string `json:"operation_id"`
		Status      int    `json:"status"`
	}
	if err := json.Unmarshal(events[selected.EventSeq-1].Payload, &response); err != nil {
		return store.Observation{}, nil, err
	}
	if response.CallID != o.CallID {
		return store.Observation{}, nil, fmt.Errorf("checkpoint %d is the response to call %s, not %s", selected.EventSeq, response.CallID, o.CallID)
	}
	canonical := store.Observation{CallID: o.CallID, Status: o.Status, Body: body}
	payload := map[string]any{"call_id": o.CallID, "operation_id": response.OperationID, "original_status": response.Status, "status": o.Status, "body": body}
	return canonical, payload, nil
}

func canonicalBody(raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("multiple JSON values")
	}
	return json.Marshal(value)
}

// applyObservation rewrites the transcript's final tool message, which at a
// tool response checkpoint is the overridden call's observation.
func applyObservation(transcript string, o store.Observation) (string, error) {
	var messages []json.RawMessage
	if err := json.Unmarshal([]byte(transcript), &messages); err != nil {
		return "", err
	}
	if len(messages) == 0 {
		return "", fmt.Errorf("transcript has no observation for call %s", o.CallID)
	}
	var last map[string]json.RawMessage
	if err := json.Unmarshal(messages[len(messages)-1], &last); err != nil {
		return "", err
	}
	var role, callID string
	if json.Unmarshal(last["role"], &role) != nil || json.Unmarshal(last["call_id"], &callID) != nil || role != "tool" || callID != o.CallID {
		return "", fmt.Errorf("transcript does not end with the observation for call %s", o.CallID)
	}
	content, err := json.Marshal(string(o.Body))
	if err != nil {
		return "", err
	}
	last["content"] = content
	delete(last, "status")
	if o.Status != 0 {
		last["status"] = json.RawMessage(strconv.Itoa(o.Status))
	}
	if messages[len(messages)-1], err = json.Marshal(last); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(messages)
	return string(encoded), err
}
