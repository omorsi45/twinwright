package store

import "time"

// CoordinationLayout is the one text form for every time stored in the
// coordination tables (run_leases, work_queue, workers).
//
// Those columns are compared and sorted inside SQL, as text, on both SQLite and
// PostgreSQL. time.RFC3339Nano trims trailing zeros, so a whole second renders
// as "12:00:01Z" while half a second later renders as "12:00:01.5Z". Compared
// as text, "." sorts before "Z", which puts the later time first: a lease that
// expired at 12:00:01.000 looked live at 12:00:01.500. A fixed nine-digit
// fraction makes lexical order equal time order. The trailing Z is a literal
// because CoordinationTime always converts to UTC first.
//
// Ledger event timestamps are not coordination times and keep their own form.
const CoordinationLayout = "2006-01-02T15:04:05.000000000Z"

// CoordinationTime renders t in CoordinationLayout, in UTC.
func CoordinationTime(t time.Time) string {
	return t.UTC().Format(CoordinationLayout)
}
