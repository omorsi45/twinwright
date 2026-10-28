package bench

import (
	"context"
	"encoding/json"
	"fmt"

	"twinwright/internal/store"
)

// Step is one tool call's outcome in a run's trajectory. Status is the HTTP
// status the service answered with, or 0 when the call timed out and no
// response arrived.
type Step struct {
	Operation string `json:"operation"`
	Status    int    `json:"status"`
}

// Fingerprint reads what a run did from its ledger: the tool trajectory in
// ledger order and the number of state mutations the agent's own calls
// committed.
//
// The trajectory is built from tool.response events, so a call that failed,
// was denied or timed out (status 0) is part of it: a regression gate that only
// saw successful calls could not notice an agent that started failing.
//
// Mutations counts state.mutation events only. A chaos actor's write is a
// chaos.actor_mutation event and is left out, because it is the scenario's
// doing and not the agent's. Filtering by run ID matters when the database also
// holds fork runs, as a counterfactual case's does.
func Fingerprint(ctx context.Context, s *store.Store, runID string) (trajectory []Step, mutations int, err error) {
	events, err := s.Events(ctx, runID)
	if err != nil {
		return nil, 0, fmt.Errorf("fingerprint run %s: %w", runID, err)
	}
	for _, e := range events {
		switch e.Type {
		case "tool.response":
			var response struct {
				OperationID string `json:"operation_id"`
				Status      int    `json:"status"`
			}
			if err := json.Unmarshal(e.Payload, &response); err != nil {
				return nil, 0, fmt.Errorf("fingerprint run %s: event %s: %w", runID, e.ID, err)
			}
			trajectory = append(trajectory, Step{Operation: response.OperationID, Status: response.Status})
		case "state.mutation":
			mutations++
		}
	}
	return trajectory, mutations, nil
}
