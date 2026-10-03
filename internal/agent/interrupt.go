package agent

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DefaultCallTimeout is the per-attempt deadline a provider uses when nothing
// configured one.
//
// It is the same 90 seconds the providers used to hard-code into their own
// http.Client, kept so that behaviour does not change for a caller that sets
// nothing. What changed is everything else about it: it is per attempt, it is
// configurable, it arrives on the context so the request is actually cancelled,
// and passing it produces a recorded outcome rather than an opaque transport
// error 90 seconds into a run.
const DefaultCallTimeout = 90 * time.Second

// Interruption kinds. A kind is the shape of the failure, not the vendor's
// spelling of it; the vendor's own value is kept verbatim in Reason.
const (
	// InterruptTimeout is an attempt that passed its deadline with no answer.
	InterruptTimeout = "timeout"
	// InterruptTruncated is a completion the provider cut short. The response is
	// HTTP 200 and well-formed; only a metadata field says it is a fragment.
	InterruptTruncated = "truncated"
)

// CallInterrupted reports one provider attempt that produced no usable turn.
//
// It is a distinct type because the runner has to tell it apart from an ordinary
// provider error: an interrupted attempt is retried once and recorded, while a
// bad request or a rejected key is not something a retry can fix.
type CallInterrupted struct {
	Kind string
	// Reason is the provider's own stop reason, verbatim: "length" on an
	// OpenAI-compatible chat completion, "max_tokens" on Anthropic's Messages
	// API, "max_output_tokens" inside the Responses API's incomplete_details.
	// Normalising it away would lose the only thing the provider told us.
	Reason string
	// Deadline is the per-attempt deadline a timeout passed.
	Deadline time.Duration
	Err      error
}

func (e *CallInterrupted) Error() string {
	switch {
	case e.Kind == InterruptTimeout:
		return fmt.Sprintf("provider call passed its %s deadline: %v", e.Deadline, e.Err)
	case e.Reason != "":
		return fmt.Sprintf("provider cut the completion short (%s)", e.Reason)
	default:
		return "provider cut the completion short"
	}
}

func (e *CallInterrupted) Unwrap() error { return e.Err }

// Interruption is the recorded form of an interrupted attempt.
//
// It carries nothing measured from the wall clock. The payload has to be
// reproducible, because a replayed run appends the same event from the recorded
// turn and the two ledgers are compared field for field.
type Interruption struct {
	Kind       string `json:"kind"`
	Attempt    int    `json:"attempt"`
	Reason     string `json:"reason,omitempty"`
	DeadlineMS int    `json:"deadline_ms,omitempty"`
}

// Record names which attempt this interruption was.
func (e *CallInterrupted) Record(attempt int) Interruption {
	return Interruption{
		Kind: e.Kind, Attempt: attempt, Reason: e.Reason,
		DeadlineMS: int(e.Deadline / time.Millisecond),
	}
}

// callDeadline resolves a provider's configured per-attempt deadline.
func callDeadline(configured time.Duration) time.Duration {
	if configured <= 0 {
		return DefaultCallTimeout
	}
	return configured
}

// interrupted classifies a failed attempt.
//
// Only the attempt's own deadline makes this a provider timeout. A caller whose
// context went away is the run being cancelled, and retrying a call nobody is
// waiting for is worse than reporting the cancellation.
func interrupted(caller, attempt context.Context, deadline time.Duration, err error) error {
	if caller.Err() == nil && errors.Is(attempt.Err(), context.DeadlineExceeded) {
		return &CallInterrupted{Kind: InterruptTimeout, Deadline: deadline, Err: err}
	}
	return err
}

// truncatedCompletion reports a response the provider cut short, carrying the
// vendor's verbatim reason.
func truncatedCompletion(reason string) error {
	return &CallInterrupted{Kind: InterruptTruncated, Reason: reason}
}
