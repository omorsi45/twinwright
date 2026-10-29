package store

import (
	"context"
	"strings"
	"testing"
)

// malformedDSNs are PostgreSQL URLs that url.Parse rejects or that parse into
// something other than what their author meant, because the password holds an
// unencoded "/", "#" or "?", or a space or control character surrounds the URL.
// url.Parse quotes what it rejected in its error, so the old
// "parse postgres DSN: %w" printed a piece of the password, or all of it.
//
// leaks are the pieces that must never reach an error. Each is part of the
// password, not of the host or the user, which an error may legitimately name.
var malformedDSNs = []struct {
	name  string
	dsn   string
	leaks []string
}{
	{"slash", "postgres://perf:hunter2/secret@127.0.0.1:1/perf?sslmode=disable", []string{"hunter2", "secret"}},
	{"question mark", "postgres://perf:hunter2?secret@127.0.0.1:1/perf", []string{"hunter2", "secret"}},
	{"hash", "postgres://perf:hunter2#secret@127.0.0.1:1/perf", []string{"hunter2", "secret"}},
	{"digits then slash", "postgres://perf:12345/secret@127.0.0.1:1/perf", []string{"12345", "secret"}},
	{"bad escape", "postgres://perf:hun%ter2@127.0.0.1:1/perf", []string{"hun%ter2", "ter2"}},
	{"trailing newline", "postgres://perf:hunter2@127.0.0.1:1/perf\n", []string{"hunter2"}},
	{"control character", "postgres://perf:hunter2@127.0.0.1:1/pe\x7frf", []string{"hunter2"}},
}

func requireNoLeak(t *testing.T, where string, err error, dsn string, leaks []string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s accepted a malformed DSN", where)
	}
	text := err.Error()
	for _, leak := range append([]string{dsn}, leaks...) {
		if strings.Contains(text, leak) {
			t.Errorf("%s: %q reached the error: %s", where, leak, text)
		}
	}
	if !strings.Contains(text, "not a usable postgres URL") {
		t.Errorf("%s: the error does not say what is wrong: %s", where, text)
	}
}

func TestOpeningAMalformedDSNNeverPrintsIt(t *testing.T) {
	ctx := context.Background()
	for _, tc := range malformedDSNs {
		t.Run(tc.name, func(t *testing.T) {
			s, err := OpenDSN(ctx, tc.dsn)
			if s != nil {
				s.Close()
			}
			requireNoLeak(t, "OpenDSN", err, tc.dsn, tc.leaks)

			s, err = OpenPostgres(ctx, tc.dsn)
			if s != nil {
				s.Close()
			}
			requireNoLeak(t, "OpenPostgres", err, tc.dsn, tc.leaks)

			s, err = OpenReadOnlyDSN(ctx, tc.dsn)
			if s != nil {
				s.Close()
			}
			requireNoLeak(t, "OpenReadOnlyDSN", err, tc.dsn, tc.leaks)
		})
	}
}

// The check must not turn a DSN that is merely unreachable into a "malformed"
// one: an operator with a wrong port needs the connection error, and that error
// (pgx's) names the user and database but not the password.
func TestOpeningAWellFormedDSNReportsTheConnectionNotTheFormat(t *testing.T) {
	ctx := context.Background()
	const dsn = "postgres://perf:hunter2@127.0.0.1:1/perf?sslmode=disable"
	for name, open := range map[string]func(context.Context, string) (*Store, error){
		"OpenDSN":         OpenDSN,
		"OpenReadOnlyDSN": OpenReadOnlyDSN,
	} {
		s, err := open(ctx, dsn)
		if s != nil {
			s.Close()
		}
		if err == nil {
			t.Fatalf("%s: connected to a closed port", name)
		}
		if strings.Contains(err.Error(), "not a usable postgres URL") {
			t.Errorf("%s refused a well-formed DSN as malformed: %v", name, err)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: the password reached the connection error: %v", name, err)
		}
	}
}
