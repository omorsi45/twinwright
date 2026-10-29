package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"twinwright/internal/redact"
)

// otelHeadersEnv is the standard OpenTelemetry variable for the headers an OTLP
// exporter sends with every request.
const otelHeadersEnv = "OTEL_EXPORTER_OTLP_HEADERS"

// headerName is an HTTP header field name: an RFC 9110 token.
var headerName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// resolveOTLPHeaders returns the collector headers for the trace and worker
// commands: --otlp-header when it is given, and otherwise the standard
// OTEL_EXPORTER_OTLP_HEADERS variable.
//
// The variable exists because a worker is long-running, and a credential in its
// arguments is visible in the process listing for as long as it runs. The flag
// still works and wins when both are given, so an existing command line behaves
// as it did.
func resolveOTLPHeaders(flagValue string) (map[string]string, error) {
	if flagValue != "" {
		return parseOTLPHeader(flagValue)
	}
	return parseOTLPHeadersEnv(os.Getenv(otelHeadersEnv))
}

// parseOTLPHeadersEnv reads the OpenTelemetry format: comma-separated name=value
// pairs, with each value percent-decoded. Empty entries are skipped, whitespace
// around a name or a value is dropped, and only the first "=" of a pair splits it,
// so a value may end in "=".
//
// A refusal names the entry and, where it helps, the header name, and never the
// value: the value is a credential. A value that decodes to a control character
// is refused too, since a decoded line break would otherwise start another header.
func parseOTLPHeadersEnv(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	headers := map[string]string{}
	for i, entry := range strings.Split(raw, ",") {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		name, value, found := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		switch {
		case !found || name == "":
			return nil, fmt.Errorf("%s entry %d is not name=value", otelHeadersEnv, i+1)
		case !headerName.MatchString(name):
			return nil, fmt.Errorf("%s entry %d has an invalid header name", otelHeadersEnv, i+1)
		}
		decoded, err := url.PathUnescape(strings.TrimSpace(value))
		if err != nil || strings.ContainsAny(decoded, "\r\n\x00") {
			return nil, fmt.Errorf("%s entry %d (%s) has an invalid percent-encoded value", otelHeadersEnv, i+1, name)
		}
		headers[name] = decoded
	}
	if len(headers) == 0 {
		return nil, nil
	}
	return headers, nil
}

// exportErrorScrubber returns the function that makes an export error safe to
// print or log. The exporter already removes the endpoint and the header values
// from its own errors; this is the second layer both callers share, and it adds
// what only the command knows, the configured provider keys. The endpoint can
// carry userinfo or a query token, and neither the header values nor a provider
// key may appear in a log line or on the terminal.
func exportErrorScrubber(endpoint string, headers map[string]string) func(error) error {
	secrets := configuredSecrets()
	for _, value := range headers {
		secrets = append(secrets, value)
	}
	rawEndpoint := strings.TrimRight(endpoint, "/")
	safeEndpoint := strings.TrimRight(redact.URL(endpoint), "/")
	return func(err error) error {
		text := redact.String(err.Error(), secrets...)
		return errors.New(strings.ReplaceAll(text, rawEndpoint, safeEndpoint))
	}
}
