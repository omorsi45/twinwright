package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestStorageCommandsNeverPrintAMalformedDSN runs two commands that open the
// store, one through the migrating open and one through the read-only open, with
// a PostgreSQL DSN whose password holds an unencoded "/", "#" or "?". url.Parse
// rejects such a DSN and quotes a piece of the password in its message, and every
// command except perf used to print it.
func TestStorageCommandsNeverPrintAMalformedDSN(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dsn   string
		leaks []string
	}{
		{"slash", "postgres://perf:hunter2/secret@127.0.0.1:1/perf?sslmode=disable", []string{"hunter2", "secret"}},
		{"hash", "postgres://perf:hunter2#secret@127.0.0.1:1/perf", []string{"hunter2", "secret"}},
		{"digits then slash", "postgres://perf:12345/secret@127.0.0.1:1/perf", []string{"12345", "secret"}},
	} {
		for command, args := range map[string][]string{
			"enqueue": {"enqueue", "R-1", "--db", tc.dsn},
			"queue":   {"queue", "--db", tc.dsn},
			"doctor":  {"doctor", "--db", tc.dsn},
		} {
			t.Run(tc.name+"/"+command, func(t *testing.T) {
				var out bytes.Buffer
				err := runCLI(args, &out)
				text := out.String()
				if err != nil {
					text += "\n" + err.Error()
				} else if command != "doctor" {
					t.Fatalf("%s accepted a malformed DSN", command)
				}
				for _, leak := range append([]string{tc.dsn}, tc.leaks...) {
					if strings.Contains(text, leak) {
						t.Errorf("%q reached the output of %s: %s", leak, command, text)
					}
				}
				if !strings.Contains(text, "not a usable postgres URL") {
					t.Errorf("the output of %s does not say what is wrong: %s", command, text)
				}
			})
		}
	}
}
