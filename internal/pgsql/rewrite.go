// Package pgsql adapts Twinwright's SQLite-flavoured SQL to PostgreSQL.
//
// Twinwright's runtime packages hand a *sql.Tx to behavior handlers, chaos,
// authorization, checkpoint reconstruction and replay. Those handlers write
// ordinary `?`-placeholder SQL. Rather than duplicate every statement per
// dialect, the PostgreSQL store installs a database/sql driver that wraps
// pgx and translates the statement text on its way to the server. The
// translation is purely lexical and happens once per prepare/exec/query.
//
// Scope and limitation: every `?` outside a string literal, a quoted
// identifier, a dollar-quoted body or a comment is treated as a bind
// placeholder. Twinwright never uses PostgreSQL's jsonb `?` existence
// operator, so this is unambiguous for this codebase; a caller that needs
// that operator must write dialect-specific SQL with native $N parameters,
// which Rewrite leaves untouched.
package pgsql

import "strings"

// Rewrite converts `?` bind placeholders into PostgreSQL's $N form.
//
// Statements that contain no translatable placeholder are returned unchanged,
// so native PostgreSQL SQL can be mixed freely with translated SQL.
func Rewrite(query string) string {
	if !strings.ContainsRune(query, '?') {
		return query
	}
	var out strings.Builder
	out.Grow(len(query) + 8)
	next := 1
	for i := 0; i < len(query); {
		switch c := query[i]; c {
		case '\'', '"':
			end := scanQuoted(query, i, c)
			out.WriteString(query[i:end])
			i = end
		case '-':
			if i+1 < len(query) && query[i+1] == '-' {
				end := scanLineComment(query, i)
				out.WriteString(query[i:end])
				i = end
				continue
			}
			out.WriteByte(c)
			i++
		case '/':
			if i+1 < len(query) && query[i+1] == '*' {
				end := scanBlockComment(query, i)
				out.WriteString(query[i:end])
				i = end
				continue
			}
			out.WriteByte(c)
			i++
		case '$':
			if tag, ok := dollarTag(query, i); ok {
				end := scanDollarQuoted(query, i, tag)
				out.WriteString(query[i:end])
				i = end
				continue
			}
			out.WriteByte(c)
			i++
		case '?':
			out.WriteByte('$')
			out.WriteString(itoa(next))
			next++
			i++
		default:
			out.WriteByte(c)
			i++
		}
	}
	return out.String()
}

// scanQuoted returns the index just past a single- or double-quoted run that
// starts at open. SQL escapes the quote character by doubling it. An
// unterminated literal consumes the rest of the statement, which the server
// then rejects with its own syntax error rather than us guessing.
func scanQuoted(query string, open int, quote byte) int {
	for i := open + 1; i < len(query); i++ {
		if query[i] != quote {
			continue
		}
		if i+1 < len(query) && query[i+1] == quote {
			i++
			continue
		}
		return i + 1
	}
	return len(query)
}

func scanLineComment(query string, open int) int {
	if idx := strings.IndexByte(query[open:], '\n'); idx >= 0 {
		return open + idx + 1
	}
	return len(query)
}

// scanBlockComment honours PostgreSQL's nested /* */ comments.
func scanBlockComment(query string, open int) int {
	depth := 0
	for i := open; i < len(query)-1; i++ {
		switch {
		case query[i] == '/' && query[i+1] == '*':
			depth++
			i++
		case query[i] == '*' && query[i+1] == '/':
			depth--
			i++
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(query)
}

// dollarTag reports the full $tag$ opener at index i, if there is one. A
// digit immediately after `$` means a native positional parameter, not a
// dollar-quoted string.
func dollarTag(query string, i int) (string, bool) {
	if query[i] != '$' {
		return "", false
	}
	for j := i + 1; j < len(query); j++ {
		c := query[j]
		if c == '$' {
			return query[i : j+1], true
		}
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			continue
		}
		if c >= '0' && c <= '9' && j > i+1 {
			continue
		}
		return "", false
	}
	return "", false
}

func scanDollarQuoted(query string, open int, tag string) int {
	body := open + len(tag)
	if idx := strings.Index(query[body:], tag); idx >= 0 {
		return body + idx + len(tag)
	}
	return len(query)
}

// itoa avoids pulling strconv into the hot rewrite path for small integers.
func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
