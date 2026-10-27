package redact

import "testing"

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
		"http://user:pw@host:notaport/v1?key=abc": "http://host:notaport/v1",
	}
	for in, want := range cases {
		if got := URL(in); got != want {
			t.Errorf("URL(%q) = %q, want %q", in, got, want)
		}
	}
}
