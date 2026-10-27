// Package redact masks credentials in text before it is stored or printed.
package redact

import (
	"net/url"
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
	s = Secrets(s, secrets...)
	s = apiKey.ReplaceAllString(s, mask)
	return bearer.ReplaceAllString(s, "${1}"+mask)
}

// Secrets masks only the given secret values, with the same eight character
// floor as String and none of its patterns. It is for text the caller trusts to
// be ordinary, such as a model's own output, where rewriting anything that merely
// looks like a key would corrupt legitimate content.
func Secrets(s string, secrets ...string) string {
	for _, secret := range secrets {
		if len(secret) >= 8 {
			s = strings.ReplaceAll(s, secret, mask)
		}
	}
	return s
}

var userinfo = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*://)[^/?#@]*@`)

// URL removes what a URL can carry besides its address: the userinfo and the
// whole query string, where API keys and signed tokens are usually passed. A
// string that does not parse is cut by hand rather than returned whole, because
// a malformed URL is exactly when an error message is most likely to print it.
func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		raw, _, _ = strings.Cut(raw, "?")
		return userinfo.ReplaceAllString(raw, "${1}")
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	return u.String()
}
