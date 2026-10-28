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

// URL removes what a URL can carry besides its address: the userinfo, the whole
// query string, where API keys and signed tokens are usually passed, and the
// fragment.
//
// A string that does not parse is not cut by hand around a guess at where the
// userinfo ends. A malformed URL is exactly when an error message is most likely
// to print it, and the passwords that break the parse (an unencoded "/", "#" or
// "?") are the ones a guess gets wrong: the authority ends early, so the rest of
// the password is in what looks like the path. Such a string keeps only what is
// certainly safe, which is its scheme and host when it holds no "@" at all (so
// it cannot hold userinfo), and is otherwise replaced by a fixed placeholder.
//
// The same reasoning applies to a string that parses but holds an "@" without
// any userinfo: the "@" ended the userinfo in the author's mind, and an early
// delimiter moved the front of the password into the host and the rest into the
// path, the query or the fragment. It is treated as unparseable too.
// Surrounding whitespace is dropped first, since an environment variable
// routinely carries a trailing newline.
func URL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, ok := parseURL(raw)
	if !ok {
		return unparsedURL(raw)
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}

// WellFormedURL reports whether URL can trust its own parse of raw: the string
// parses, and any "@" in it ends a userinfo. A caller that holds a URL which is
// not well formed knows that an error built from it may quote part of a
// credential, and can drop that error's text rather than try to clean it.
func WellFormedURL(raw string) bool {
	_, ok := parseURL(strings.TrimSpace(raw))
	return ok
}

// parseURL parses raw, and refuses a result it cannot trust. See URL for why a
// string that parses but holds an "@" without userinfo is not trusted.
func parseURL(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.User == nil && strings.Contains(raw, "@")) {
		return nil, false
	}
	return u, true
}

// unparseableURL stands in for a URL of which nothing is certainly safe to show.
const unparseableURL = "[unparseable URL]"

var (
	urlScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*$`)
	plainHost = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)
	allDigits = regexp.MustCompile(`^[0-9]+$`)
)

// unparsedURL is what is safe to keep of a string that could not be trusted to
// parse: scheme://host, plus the port when it is plain digits. A host that is not
// a plain name or an IPv4 address, or any "@" anywhere in the string, gives the
// placeholder instead.
func unparsedURL(raw string) string {
	scheme, rest, found := strings.Cut(raw, "://")
	if !found || !urlScheme.MatchString(scheme) || strings.Contains(rest, "@") {
		return unparseableURL
	}
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		rest = rest[:end]
	}
	host, port, hasPort := strings.Cut(rest, ":")
	if !plainHost.MatchString(host) {
		return unparseableURL
	}
	if hasPort && allDigits.MatchString(port) {
		host += ":" + port
	}
	return scheme + "://" + host
}
