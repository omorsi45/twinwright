package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
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
	// InterruptRateLimited is an HTTP 429: the provider asked the caller to slow
	// down, usually saying for how long in Retry-After.
	InterruptRateLimited = "rate_limited"
	// InterruptServerError is an HTTP 500, 502, 503 or 504: the provider failed
	// to answer, which is routinely gone a moment later. Other 5xx codes are not
	// in the list on purpose; 501 and 505 say the request itself is unsupported.
	InterruptServerError = "server_error"
	// InterruptTransport is a request that failed before any response arrived
	// while neither the caller nor the attempt deadline had given up: a refused
	// or dropped connection.
	InterruptTransport = "transport"
)

// MaxRetryAfter bounds how long a server's Retry-After is honoured and recorded.
// A run that sleeps for an hour on one header is a hung run, so a server asking
// for more is waited on for this long and no longer.
const MaxRetryAfter = 30 * time.Second

// defaultRetryWait is the pause before the second attempt when the server named
// none. Retrying at once into a limit or an outage that has only just begun
// mostly spends the one retry there is.
const defaultRetryWait = time.Second

// maxParsedRetryAfter is the largest Retry-After in seconds that is kept as
// sent. It is about 34 years: far past MaxRetryAfter, and small enough that
// converting to a time.Duration cannot overflow into a negative wait.
const maxParsedRetryAfter = 1 << 30

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
	// Status is the HTTP status of a rate limit or a server error. A transport
	// failure never got one.
	Status int
	// RetryAfter is the wait the server asked for, as sent: zero when it sent
	// none, or sent something unusable such as a zero, a negative or a date that
	// has passed. It is not capped here; Record and the runner cap it.
	RetryAfter time.Duration
	Err        error
}

func (e *CallInterrupted) Error() string {
	switch {
	case e.Kind == InterruptTimeout:
		return fmt.Sprintf("provider call passed its %s deadline: %v", e.Deadline, e.Err)
	case e.Kind == InterruptRateLimited || e.Kind == InterruptServerError:
		// Err already says what the provider answered, with its body redacted.
		if e.Err != nil {
			return e.Err.Error()
		}
		return fmt.Sprintf("provider answered HTTP %d", e.Status)
	case e.Kind == InterruptTransport:
		// Err is the text safeTransportError left, so it is safe to print and to
		// record: no URL query, userinfo or key.
		if e.Err != nil {
			return fmt.Sprintf("provider request failed: %v", e.Err)
		}
		return "provider request failed"
	case e.Reason != "":
		return fmt.Sprintf("provider cut the completion short (%s)", e.Reason)
	default:
		return "provider cut the completion short"
	}
}

func (e *CallInterrupted) Unwrap() error { return e.Err }

// Interruption is the recorded form of an interrupted attempt.
//
// Nothing in it is measured while the ledger is written. The payload has to be
// reproducible, because a replayed run appends the same event from the recorded
// turn and the two ledgers are compared field for field. RetryAfterMS is worked
// out once, when the attempt fails, and replay reads the recorded value back.
type Interruption struct {
	Kind       string `json:"kind"`
	Attempt    int    `json:"attempt"`
	Reason     string `json:"reason,omitempty"`
	DeadlineMS int    `json:"deadline_ms,omitempty"`
	// Status is the HTTP status of a rate limit or a server error, and is absent
	// for a transport failure.
	Status int `json:"status,omitempty"`
	// RetryAfterMS is the server's Retry-After capped at MaxRetryAfter, so what
	// the ledger says the server asked for is what the run could honour. It is
	// absent when the server sent none.
	RetryAfterMS int `json:"retry_after_ms,omitempty"`
}

// Record names which attempt this interruption was.
func (e *CallInterrupted) Record(attempt int) Interruption {
	return Interruption{
		Kind: e.Kind, Attempt: attempt, Reason: e.Reason,
		DeadlineMS: int(e.Deadline / time.Millisecond),
		Status:     e.Status,
		// max first: a negative RetryAfter must not become a negative record.
		RetryAfterMS: int(min(max(e.RetryAfter, 0), MaxRetryAfter) / time.Millisecond),
	}
}

// wait is how long the runner pauses before the attempt after this one.
//
// Only the kinds that mean "the other side is busy or briefly gone" wait. A
// timeout already spent its whole deadline, and a truncated completion is not
// rate pressure, so both retry at once as they always did.
func (e *CallInterrupted) wait() time.Duration {
	switch e.Kind {
	case InterruptRateLimited, InterruptServerError, InterruptTransport:
		if e.RetryAfter > 0 {
			return min(e.RetryAfter, MaxRetryAfter)
		}
		return defaultRetryWait
	}
	return 0
}

// parseRetryAfter reads a Retry-After header: a whole number of seconds or an
// HTTP date. Anything that does not name a wait in the future returns zero, which
// callers treat as "not sent". Seconds too large for a duration are kept as a
// very long wait, not dropped, because "wait a very long time" is what the
// server said.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		if err != nil {
			seconds = maxParsedRetryAfter
		}
		return time.Duration(min(seconds, maxParsedRetryAfter)) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(at.Sub(now), 0)
	}
	return 0
}

// statusInterruption classifies a non-2xx response. Only a 429 and the four
// transient 5xx codes are interruptions; every other status is nil, and the
// caller returns its ordinary error, because a rejected key or a malformed
// request is not something a retry can fix. cause is that ordinary error, kept
// as the interruption's text so the provider's own message is not lost.
func statusInterruption(res *http.Response, cause error) *CallInterrupted {
	var kind string
	switch res.StatusCode {
	case http.StatusTooManyRequests:
		kind = InterruptRateLimited
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		kind = InterruptServerError
	default:
		return nil
	}
	return &CallInterrupted{
		Kind: kind, Status: res.StatusCode, Err: cause,
		RetryAfter: parseRetryAfter(res.Header.Get("Retry-After"), time.Now()),
	}
}

// sleepContext waits for d or until ctx ends, whichever is first, and returns the
// context's error in the second case. An already-ended context returns at once.
func sleepContext(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
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

// requestFailed classifies an error from http.Client.Do, which is the only place
// a transport failure is recognised. Past the deadline it is a timeout, as in
// interrupted. Otherwise, while the caller is still live, the connection failed
// on its own and the attempt is a transport interruption. A cancelled caller is
// the run going away, so its error is returned as it is.
//
// The error must already be the redacted one from safeTransportError, because
// this is the text the ledger records if both attempts fail.
func requestFailed(caller, attempt context.Context, deadline time.Duration, err error) error {
	err = interrupted(caller, attempt, deadline, err)
	if _, already := err.(*CallInterrupted); already || caller.Err() != nil {
		return err
	}
	return &CallInterrupted{Kind: InterruptTransport, Err: err}
}

// truncatedCompletion reports a response the provider cut short, carrying the
// vendor's verbatim reason.
func truncatedCompletion(reason string) error {
	return &CallInterrupted{Kind: InterruptTruncated, Reason: reason}
}
