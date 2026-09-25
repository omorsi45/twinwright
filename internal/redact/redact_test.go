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
