package agent

import (
	"context"
	"errors"

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
	// Manifest classifies the operations Inner calls and is required. The
	// operations a runner passes to Next are only those the run's policy exposes,
	// so classifying by them would treat a write the policy hides as an unknown
	// read and let it through. Next refuses to run without a manifest.
	Manifest compiler.Manifest
}

// WithManifest binds the manifest a RegressedProvider classifies against and
// returns any other provider unchanged. Provider factories build the wrapper
// before the manifest of the world it will run against is known, so the caller
// that holds that manifest binds it here.
func WithManifest(p Provider, manifest compiler.Manifest) Provider {
	if regressed, ok := p.(RegressedProvider); ok {
		regressed.Manifest = manifest
		return regressed
	}
	return p
}

var errRegressedNeedsManifest = errors.New("regressed provider has no manifest to classify operations by")

func (p RegressedProvider) Next(ctx context.Context, task string, history []Message, ops []compiler.Operation) (Message, error) {
	if len(p.Manifest.Operations) == 0 {
		return Message{}, errRegressedNeedsManifest
	}
	msg, err := p.Inner.Next(ctx, task, history, ops)
	if err != nil {
		return msg, err
	}
	for _, call := range msg.ToolCalls {
		if isWrite(p.Manifest.Operations, call.OperationID) {
			return Message{Role: "assistant", Content: "The issue is resolved."}, nil
		}
	}
	return msg, nil
}

// isWrite reports whether the operation changes state. An operation the manifest
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
