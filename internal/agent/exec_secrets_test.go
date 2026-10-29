package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"twinwright/internal/agent/exectest"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

// secretCase is a passed value with a character that every encoder rewrites.
// fragment is a stretch of the value that holds none of them, so it appears
// unchanged in any spelling of the value and in any quoting of the text around
// it: finding it in an error text means the value, or the end of it, got out.
type secretCase struct {
	name, value, fragment string
}

var escapableSecrets = []secretCase{
	{"double quote", `quote"in-the-secret-1234`, "in-the-secret-1234"},
	{"backslash", `back\slash-secret-1234`, "slash-secret-1234"},
	{"newline", "line-one\nline-two-secret", "line-two-secret"},
	{"tab", "tab\there-secret-1234", "here-secret-1234"},
	{"non-ASCII", "p\u00e4ssw\u00f6rd-secret-1", "rd-secret-1"},
	{"JSON-escaped markup", `a&b<c>d-secret-1234`, "d-secret-1234"},
	{"slash", `slash/in-the-secret-1234`, "in-the-secret-1234"},
}

// spellings lists the ways a value can be written in a text: as it is, with the
// escapes of strconv.Quote, and with the escapes of a JSON string.
func spellings(t testing.TB, value string) []string {
	t.Helper()
	quoted := strconv.Quote(value)
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return []string{value, quoted[1 : len(quoted)-1], string(encoded[1 : len(encoded)-1])}
}

// requireNoSecret fails if text holds the value in any spelling or the fragment.
func requireNoSecret(t testing.TB, where, text string, c secretCase) {
	t.Helper()
	for _, spelling := range append(spellings(t, c.value), c.fragment) {
		if strings.Contains(text, spelling) {
			t.Errorf("LEAK in %s: passed value %q appears as %q in: %.300s", where, c.value, spelling, text)
		}
	}
}

// cutPad is the filler that leaves the 4 KiB window of stderr starting three
// bytes into a value that is written first, with a newline after it and another
// after the filler. Every fragment above starts after the third byte, so it is
// inside the window and is the part that survives a cut made before masking.
func cutPad(value string) int { return maxAgentStderrBytes + 3 - 2 - len(value) }

// The tail of stderr is quoted for the error text, and quoting rewrites a quote,
// a backslash or a control character. The mask has to see the value as the agent
// wrote it, whichever way the agent spelled it.
func TestExecStderrMasksValuesWithEscapedCharacters(t *testing.T) {
	for _, c := range escapableSecrets {
		for _, mode := range []string{"exit3", "exit3-spellings"} {
			t.Run(c.name+"/"+mode, func(t *testing.T) {
				_, err := helperProvider(t, mode, exectest.Secret+"="+c.value).Next(context.Background(), "task", nil, nil)
				if err == nil {
					t.Fatal("a failing agent was accepted")
				}
				if !strings.Contains(err.Error(), "exited with status 3") || !strings.Contains(err.Error(), "[REDACTED]") {
					t.Fatalf("error = %.300q, want the status and a mask", err)
				}
				requireNoSecret(t, "the error text", err.Error(), c)
			})
		}
	}
}

// A value that the 4 KiB window cuts in two must not leave its end behind: the
// mask runs before the cut, so the part inside the window is still a whole value
// to it.
func TestExecStderrMasksAValueCutByTheTail(t *testing.T) {
	for _, c := range escapableSecrets {
		t.Run(c.name, func(t *testing.T) {
			_, err := helperProvider(t, "exit3-padded", exectest.Secret+"="+c.value, exectest.Pad+"="+strconv.Itoa(cutPad(c.value))).
				Next(context.Background(), "task", nil, nil)
			if err == nil {
				t.Fatal("a failing agent was accepted")
			}
			requireNoSecret(t, "the error text", err.Error(), c)
			if !strings.Contains(err.Error(), "xxxx") {
				t.Fatalf("the filler after the value is missing, so the test cut nothing: %.200q", err)
			}
		})
	}
}

// The part of a value that lies before everything the buffer kept cannot be
// masked, because the mask never sees the rest of it. It must not be shown
// either, even when the masked text is short enough for the whole kept region to
// fit the window: the window is chosen in the original offsets, after the extra
// that is kept for the mask.
func TestTailBufferNeverShowsTheEndOfAValueItCutOff(t *testing.T) {
	const passed = "passed-value-abcd"
	const window = 64
	// 2 bytes of text and thirty copies of the value: 512 bytes. The buffer
	// keeps window + len(passed) = 81, which starts 4 bytes into a copy.
	stream := "ab" + strings.Repeat(passed, 30)
	buffer := newTailBuffer(window, secretSpellings([]string{passed}))
	for i := 0; i < len(stream); i += 7 {
		buffer.Write([]byte(stream[i:min(i+7, len(stream))]))
	}
	tail := buffer.Tail()
	if tail == "" || strings.ReplaceAll(tail, redacted, "") != "" {
		t.Fatalf("tail = %q, want nothing but masks", tail)
	}
}

// The check that matters is the stored one: the run's error event, read back
// from SQLite after a real Runner has failed on the agent.
func TestExecStderrSecretsNeverReachTheLedger(t *testing.T) {
	ctx := context.Background()
	manifest := execBillingManifest(t)
	for _, c := range escapableSecrets {
		for _, mode := range []string{"exit3", "exit3-spellings", "exit3-padded"} {
			t.Run(c.name+"/"+mode, func(t *testing.T) {
				s, err := store.Open(filepath.Join(t.TempDir(), "world.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				world, err := s.Seed(ctx, 42, manifest.Digest)
				if err != nil {
					t.Fatal(err)
				}
				run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "exec", "leaky@test", "task", "listCharges")
				if err != nil {
					t.Fatal(err)
				}
				provider := helperProvider(t, mode, exectest.Secret+"="+c.value, exectest.Pad+"="+strconv.Itoa(cutPad(c.value)))
				runner := Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest, Provider: provider}
				if _, err := runner.Execute(ctx, run.ID, 5); err == nil {
					t.Fatal("the run did not fail")
				}
				events, err := s.Events(ctx, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				seen := false
				for _, event := range events {
					if event.Type != "error" {
						continue
					}
					seen = true
					var payload struct {
						Message string `json:"message"`
					}
					if err := json.Unmarshal(event.Payload, &payload); err != nil {
						t.Fatal(err)
					}
					requireNoSecret(t, "ledger event "+event.ID+" (error), message", payload.Message, c)
					requireNoSecret(t, "ledger event "+event.ID+" (error), payload", string(event.Payload), c)
				}
				if !seen {
					t.Fatal("no error event was recorded")
				}
			})
		}
	}
}

// How the agent was started is not part of a run. An agent that cannot be started
// fails the run with the operating system's reason, and not with the command that
// was tried, whose path and arguments are the operator's.
func TestExecStartFailureNeverPutsTheCommandInTheLedger(t *testing.T) {
	cases := []struct {
		name    string
		command []string
		reason  string
	}{
		{"a path that does not exist", []string{"/home/operator-name/private-agents/billing-agent-v7", "--private-flag"}, "no such file"},
		{"a name that is not on PATH", []string{"private-agent-billing-v7", "--private-flag"}, "not found"},
	}
	ctx := context.Background()
	manifest := execBillingManifest(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := store.Open(filepath.Join(t.TempDir(), "world.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			world, err := s.Seed(ctx, 42, manifest.Digest)
			if err != nil {
				t.Fatal(err)
			}
			run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "exec", "missing@test", "task", "listCharges")
			if err != nil {
				t.Fatal(err)
			}
			runner := Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest, Provider: ExecProvider{Command: tc.command, Timeout: time.Second}}
			if _, err := runner.Execute(ctx, run.ID, 5); err == nil {
				t.Fatal("the run did not fail")
			}
			events, err := s.Events(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			seen := false
			for _, event := range events {
				payload := string(event.Payload)
				for _, word := range []string{tc.command[0], "operator-name", "private-agents", "--private-flag"} {
					if strings.Contains(payload, word) {
						t.Errorf("ledger event %s (%s) holds %q: %s", event.ID, event.Type, word, payload)
					}
				}
				if event.Type == "error" {
					seen = true
					if !strings.Contains(strings.ToLower(payload), tc.reason) || !strings.Contains(payload, "start") {
						t.Errorf("the error event gives no reason (%q): %s", tc.reason, payload)
					}
				}
			}
			if !seen {
				t.Fatal("no error event was recorded")
			}
		})
	}
}
