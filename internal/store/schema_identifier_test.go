package store

import (
	"strings"
	"testing"
)

// The PostgreSQL search_path schema is the one identifier in this codebase that
// reaches a statement as text: CREATE SCHEMA cannot take a bind parameter. Both
// SECURITY.md and the comment above schemaIdentifier say the name is validated
// against a plain-identifier pattern before that interpolation happens, and
// nothing held that claim.
//
// No server is needed, which is the point: the validation runs while parsing the
// DSN, before a connection is opened, so a hostile search_path never reaches a
// statement at all.
func TestSearchPathSchemaRejectsAnythingButAPlainIdentifier(t *testing.T) {
	hostile := []string{
		`x";DROP SCHEMA public CASCADE;--`,
		`public"`,
		`a b`,
		`1schema`,
		`sch-ema`,
		`sch.ema`,
		strings.Repeat("s", 64),
	}
	for _, value := range hostile {
		dsn := "postgres://host/db?search_path=" + queryEscape(value)
		schema, err := searchPathSchema(dsn)
		if err == nil {
			t.Errorf("search_path %q was accepted as schema %q", value, schema)
			continue
		}
		if !strings.Contains(err.Error(), "not a plain SQL identifier") {
			t.Errorf("search_path %q failed for the wrong reason: %v", value, err)
		}
	}

	// Controls, so the test cannot pass by rejecting everything.
	for _, value := range []string{"twinwright", "_private", "Run_42", strings.Repeat("s", 63)} {
		schema, err := searchPathSchema("postgres://host/db?search_path=" + value)
		if err != nil || schema != value {
			t.Errorf("legal schema %q was refused: schema=%q err=%v", value, schema, err)
		}
	}
	// An absent search_path is not an error; it means the server's default.
	if schema, err := searchPathSchema("postgres://host/db"); err != nil || schema != "" {
		t.Errorf("absent search_path: schema=%q err=%v", schema, err)
	}
}

// queryEscape keeps the hostile values readable above while still producing a DSN
// url.Parse accepts, so the test exercises the identifier check rather than the
// URL parser.
func queryEscape(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch r {
		case '"':
			b.WriteString("%22")
		case ';':
			b.WriteString("%3B")
		case ' ':
			b.WriteString("%20")
		case '#':
			b.WriteString("%23")
		case '&':
			b.WriteString("%26")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
