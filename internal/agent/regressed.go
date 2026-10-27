package agent

import (
	"context"

	"twinwright/internal/compiler"
)

// RegressedModel is the scripted model name that selects RegressedProvider.
const RegressedModel = "fixture-regressed-v1"

// RegressedProvider is a deliberately broken fixture. It behaves like Inner
// until Inner first asks for a write, then answers that the issue is resolved
// without making the call.
//
// It exists to prove the regression gate can fail. A gate that has only ever
// passed has not been shown to catch anything, so CI benches this fixture and
// requires the gate to block it. A regression that stops before the first write
// is the realistic shape of the failure: the agent investigates correctly, then
// reports success without acting.
//
// The decision depends only on the operation being called, never on the
// scenario, so the same wrapper degrades every scripted fixture alike.
type RegressedProvider struct {
	Inner Provider
	// Manifest classifies the operations Inner calls. When it carries none, the
	// operations the runner passes to Next are used instead, so a caller that has
	// no manifest in hand still gets the wrapper.
	Manifest compiler.Manifest
}

func (p RegressedProvider) Next(ctx context.Context, task string, history []Message, ops []compiler.Operation) (Message, error) {
	msg, err := p.Inner.Next(ctx, task, history, ops)
	if err != nil {
		return msg, err
	}
	known := p.Manifest.Operations
	if len(known) == 0 {
		known = ops
	}
	for _, call := range msg.ToolCalls {
		if isWrite(known, call.OperationID) {
			return Message{Role: "assistant", Content: "The issue is resolved."}, nil
		}
	}
	return msg, nil
}

// isWrite reports whether the operation changes state. An operation the list
// does not know is treated as a read: the dispatcher rejects it with its own
// error, which is the behaviour of the healthy fixture too.
func isWrite(ops []compiler.Operation, id string) bool {
	for _, op := range ops {
		if op.ID == id {
			return op.Method != "GET"
		}
	}
	return false
}
