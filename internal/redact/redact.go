// Package redact masks credentials in text before it is stored or printed.
package redact

import (
	"regexp"
	"strings"
)

const mask = "[REDACTED]"

var (
	apiKey = regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`)
	bearer = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+`)
)

// String masks the given secrets, API keys that start with "sk-", and bearer
// tokens. Secrets shorter than eight characters are ignored so that ordinary
// words are never masked.
func String(s string, secrets ...string) string {
	for _, secret := range secrets {
		if len(secret) >= 8 {
			s = strings.ReplaceAll(s, secret, mask)
		}
	}
	s = apiKey.ReplaceAllString(s, mask)
	return bearer.ReplaceAllString(s, "${1}"+mask)
}
