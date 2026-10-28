package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"twinwright/internal/compiler"
	"twinwright/internal/redact"
)

// KnownProvider reports the provider names the CLI and forks accept.
func KnownProvider(name string) bool {
	switch name {
	case "scripted", "openai", "openai-compatible", "anthropic", "exec":
		return true
	}
	return false
}

func guidanceFor(ops []compiler.Operation) string {
	guidance := "You are testing a fictional billing service. Use tools to investigate. Refund only a justified duplicate charge. A 503 is temporary; retry if needed. Never claim success without checking tool results."
	for _, op := range ops {
		if strings.HasPrefix(op.ID, "crm") || strings.HasPrefix(op.Behavior, "crm.") {
			return "You are testing fictional billing, CRM, ticketing, and messaging services. Investigate with tool results, refund only a justified duplicate charge, and record a CRM note. Open an engineering issue and notify support only when the account contains incident evidence. A 503 is temporary; retry with a new tool call ID. Check results before claiming success."
		}
	}
	return guidance
}

// MaxResponseBytes is the most of a provider response body that is read.
const MaxResponseBytes = 4 << 20

// ErrResponseTooLarge reports a response body over MaxResponseBytes.
var ErrResponseTooLarge = errors.New("provider response exceeds 4 MiB")

// readBody reads a response body up to MaxResponseBytes. One byte more is read
// so a body that is exactly at the limit is accepted and a longer one is told
// apart from it: silently cutting it would hand the decoder half a document.
// The size error is deliberately not an interruption, so it is not retried.
func readBody(res *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(res.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxResponseBytes {
		return nil, ErrResponseTooLarge
	}
	return body, nil
}

// transportError is a failed request with its URL and text made safe to print.
type transportError struct {
	op, url, text string
	err           error
}

func (e *transportError) Error() string {
	return fmt.Sprintf("%s %s: %s", e.op, strconv.Quote(e.url), e.text)
}

// Unwrap keeps errors.Is and errors.As working for the cause, such as a
// deadline, without keeping the original URL reachable from the error text.
func (e *transportError) Unwrap() error { return e.err }

// safeTransportError rewrites the *url.Error that http.Client.Do returns. The
// standard library prints the full request URL, and an OpenAI-compatible base
// URL may carry userinfo or a key in its query. The cause text is also masked,
// since a proxy or dialer can echo a credential back.
func safeTransportError(err error, secrets ...string) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	return &transportError{op: urlErr.Op, url: redact.URL(urlErr.URL), text: redact.String(urlErr.Err.Error(), secrets...), err: urlErr.Err}
}

// redactOutput masks the configured secret in a model's own output items. It
// runs before the items are interpreted, so the content, the tool arguments and
// the raw items kept for replay all derive from the masked text and none of them
// can carry the key into the ledger.
func redactOutput(raws []json.RawMessage, secrets ...string) []json.RawMessage {
	out := make([]json.RawMessage, len(raws))
	for i, raw := range raws {
		out[i] = json.RawMessage(redact.Secrets(string(raw), secrets...))
	}
	return out
}
