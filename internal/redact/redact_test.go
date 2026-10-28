package redact

import (
	"strings"
	"testing"
)

func TestStringMasksSecrets(t *testing.T) {
	// Assembled at run time so the secret scanner does not flag the fixture.
	fakeKey := "sk-" + "proj-" + "AbC_123-xyz987654"
	cases := map[string]struct{ in, want string }{
		"exact secret":    {"key was abcdef123456 here", "key was [REDACTED] here"},
		"openai key":      {"Incorrect API key provided: " + fakeKey + ".", "Incorrect API key provided: [REDACTED]."},
		"bearer token":    {"Authorization: Bearer eyJhbGciOi.J9-x_y", "Authorization: Bearer [REDACTED]"},
		"lowercase auth":  {"authorization: bearer tok3n-value", "authorization: bearer [REDACTED]"},
		"ordinary text":   {"refund RF-1 on CH-1002 for 500 cents; task-based", "refund RF-1 on CH-1002 for 500 cents; task-based"},
		"short sk prefix": {"sk-1 is not a key", "sk-1 is not a key"},
	}
	for name, tc := range cases {
		if got := String(tc.in, "abcdef123456"); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}

func TestStringIgnoresShortOrEmptySecrets(t *testing.T) {
	if got := String("a b c", "", "a"); got != "a b c" {
		t.Fatalf("short secret masked ordinary text: %q", got)
	}
}

// Secrets masks only what it was given. The pattern masks in String exist for
// unknown text such as an error body; applying them to a model's own output
// would rewrite legitimate text that merely looks like a key.
func TestSecretsMasksOnlyTheGivenValues(t *testing.T) {
	fakeKey := "sk-" + "proj-" + "AbC_123-xyz987654"
	cases := map[string]struct{ in, want string }{
		"given secret":     {"echo abcdef123456 back", "echo [REDACTED] back"},
		"repeated":         {"abcdef123456 abcdef123456", "[REDACTED] [REDACTED]"},
		"unrelated sk key": {"model said " + fakeKey, "model said " + fakeKey},
		"unrelated bearer": {"Bearer eyJhbGciOi", "Bearer eyJhbGciOi"},
		"ordinary text":    {"refund RF-1 on CH-1002", "refund RF-1 on CH-1002"},
	}
	for name, tc := range cases {
		if got := Secrets(tc.in, "abcdef123456"); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
	if got := Secrets("a b c", "", "a", "short"); got != "a b c" {
		t.Fatalf("short or empty secret masked ordinary text: %q", got)
	}
}

func TestURLStripsUserinfoAndTheWholeQuery(t *testing.T) {
	cases := map[string]string{
		"http://user:pw@127.0.0.1:1/v1?key=abc":    "http://127.0.0.1:1/v1",
		"https://api.example.com/v1/messages":      "https://api.example.com/v1/messages",
		"https://tok@api.example.com/v1?a=1&b=2":   "https://api.example.com/v1",
		"https://api.example.com/v1?":              "https://api.example.com/v1",
		"http://user:***@127.0.0.1:1/v1?key=abc/x": "http://127.0.0.1:1/v1",
		"": "",
		// A fragment is never sent to a server, but it is part of what the string
		// carries, and a password with an unencoded "#" ends up in it.
		"https://api.example.com/v1#frag": "https://api.example.com/v1",
		"http://[::1]:8080/v1?key=abc":    "http://[::1]:8080/v1",
		// Surrounding whitespace is not part of the URL. A leading blank used to
		// make the hand-cut fallback miss the scheme and keep the userinfo.
		" http://user:userinfo-pw@127.0.0.1:1/v1?key=query-token":  "http://127.0.0.1:1/v1",
		"http://user:userinfo-pw@127.0.0.1:1/v1?key=query-token\n": "http://127.0.0.1:1/v1",
		" \t ": "",
	}
	for in, want := range cases {
		if got := URL(in); got != want {
			t.Errorf("URL(%q) = %q, want %q", in, got, want)
		}
	}
}

// A string url.Parse rejects is exactly when an error message is most likely to
// print it, and a hand-cut guess at which part is the userinfo fails on the
// passwords that break the parse: an unencoded "/", "#" or "?" ends the
// authority early, so the rest of the password lands in the path, the query or
// the fragment. URL therefore keeps only what is certainly safe, and nothing at
// all whenever an "@" could be the end of userinfo.
func TestURLFailsClosedOnAnUnparseableURL(t *testing.T) {
	const placeholder = "[unparseable URL]"
	cases := []struct {
		name, in, want string
		// leaks are fragments of the credential that must not survive.
		leaks []string
	}{
		{"unencoded percent in the password", "http://user:pa%ss@127.0.0.1:1/v1?key=query-token/chat/completions", placeholder, []string{"pa%ss", "query-token", "user"}},
		{"control character after the query", "http://user:userinfo-pw@127.0.0.1:1/v1?key=query-token\n/chat/completions", placeholder, []string{"userinfo-pw", "query-token"}},
		{"slash in the password", "postgres://perf:hunter2/secret@127.0.0.1:5432/perf", placeholder, []string{"hunter2", "secret"}},
		{"hash in the password", "postgres://perf:hunter2#secret@127.0.0.1:5432/perf", placeholder, []string{"hunter2", "secret"}},
		{"question mark in the password", "postgres://perf:hunter2?secret@127.0.0.1:5432/perf", placeholder, []string{"hunter2", "secret"}},
		{"bad port with userinfo", "http://user:pw-secret@host:notaport/v1?key=abc", placeholder, []string{"pw-secret", "user", "abc"}},
		{"bad escape in the password and a query", "https://user:p%zz@api.example.com/v1?api_key=abc", placeholder, []string{"p%zz", "abc"}},
		// These parse, with the front of the password as the port and the rest in
		// the path, the query or the fragment, so the parse alone cannot be trusted.
		{"digits then slash", "postgres://perf:12345/secret@127.0.0.1:5432/perf", placeholder, []string{"12345", "secret"}},
		{"digits then hash", "postgres://perf:12345#secret@127.0.0.1:5432/perf", placeholder, []string{"12345", "secret"}},
		{"digits then question mark", "postgres://perf:12345?secret@127.0.0.1:5432/perf", placeholder, []string{"12345", "secret"}},
		{"no userinfo, bad port", "http://host:notaport/v1?key=abc", "http://host", []string{"notaport", "abc", "v1"}},
		{"no userinfo, bad escape in the path", "https://api.example.com:8443/%zz/key-abcdef?x=1", "https://api.example.com:8443", []string{"zz", "abcdef", "x=1"}},
		{"no userinfo, bad escape and a fragment", "https://api.example.com/%zz#frag-abcdef", "https://api.example.com", []string{"abcdef"}},
		{"host that is not plain", "http://ho st:80/%zz", placeholder, []string{"ho st"}},
		{"bracketed host is not recovered", "http://[::1]:99/%zz", placeholder, nil},
		{"not a URL", "not a url %zz", placeholder, []string{"not a url"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := URL(tc.in)
			if got != tc.want {
				t.Errorf("URL(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for _, leak := range tc.leaks {
				if strings.Contains(got, leak) {
					t.Errorf("URL(%q) = %q still carries %q", tc.in, got, leak)
				}
			}
		})
	}
}
