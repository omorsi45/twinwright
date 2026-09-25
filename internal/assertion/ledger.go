package assertion

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"twinwright/internal/store"
)

func (m EventMatch) matches(event store.Event) (bool, error) {
	if event.Type != m.Type {
		return false, nil
	}
	if len(m.Where) == 0 {
		return true, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return false, nil
	}
	for key, want := range m.Where {
		got, ok := payload[key]
		if !ok {
			return false, nil
		}
		left, err := json.Marshal(got)
		if err != nil {
			return false, err
		}
		right, err := json.Marshal(want)
		if err != nil {
			return false, err
		}
		if !bytes.Equal(left, right) {
			return false, nil
		}
	}
	return true, nil
}

func eventCount(events []store.Event, a Assertion, result *Result) error {
	for _, event := range events {
		ok, err := a.Event.matches(event)
		if err != nil {
			return err
		}
		if ok {
			result.EventIDs = append(result.EventIDs, event.ID)
		}
	}
	a.Count.judge(len(result.EventIDs), result)
	return nil
}

// mutationForbidden attributes each state.mutation to the tool call it was
// recorded inside, so a write hidden behind a timeout still counts.
func mutationForbidden(events []store.Event, set Set, a Assertion, result *Result) error {
	operation := ""
	for _, event := range events {
		switch event.Type {
		case "tool.request":
			var request struct {
				OperationID string `json:"operation_id"`
			}
			if err := json.Unmarshal(event.Payload, &request); err != nil {
				return err
			}
			operation = request.OperationID
		case "tool.response":
			operation = ""
		case "state.mutation":
			if forbidden(set, a, operation) {
				result.EventIDs = append(result.EventIDs, event.ID)
			}
		}
	}
	result.Passed = len(result.EventIDs) == 0
	if !result.Passed {
		result.Detail = fmt.Sprintf("%d mutation(s) by forbidden %s%s", len(result.EventIDs), a.Service, a.Operation)
	}
	return nil
}

func forbidden(set Set, a Assertion, operationID string) bool {
	if a.Operation != "" {
		return operationID == a.Operation
	}
	op := set.manifest.Operation(operationID)
	return op != nil && strings.SplitN(op.Behavior, ".", 2)[0] == a.Service
}

func eventOrder(events []store.Event, a Assertion, result *Result) error {
	seenFirst := false
	for _, event := range events {
		isThen, err := a.Then.matches(event)
		if err != nil {
			return err
		}
		if isThen && !seenFirst {
			result.EventIDs = append(result.EventIDs, event.ID)
		}
		isFirst, err := a.First.matches(event)
		if err != nil {
			return err
		}
		seenFirst = seenFirst || isFirst
	}
	result.Passed = len(result.EventIDs) == 0
	if !result.Passed {
		result.Detail = fmt.Sprintf("%d event before any %s", len(result.EventIDs), a.First.Type)
	}
	return nil
}
