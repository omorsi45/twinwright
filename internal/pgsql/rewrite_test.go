package pgsql

import "testing"

func TestRewritePositionalPlaceholders(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"none", "SELECT 1", "SELECT 1"},
		{"single", "SELECT id FROM runs WHERE id=?", "SELECT id FROM runs WHERE id=$1"},
		{"numbered in order", "INSERT INTO runs(id,world_id) VALUES(?,?)", "INSERT INTO runs(id,world_id) VALUES($1,$2)"},
		{
			"many",
			"INSERT INTO tool_results(run_id,call_id,operation_id,arguments,status,body) VALUES(?,?,?,?,?,?)",
			"INSERT INTO tool_results(run_id,call_id,operation_id,arguments,status,body) VALUES($1,$2,$3,$4,$5,$6)",
		},
		{"single quoted literal is not a placeholder", "SELECT '?' FROM runs WHERE id=?", "SELECT '?' FROM runs WHERE id=$1"},
		{"escaped quote inside literal", "SELECT 'it''s ? here' , ?", "SELECT 'it''s ? here' , $1"},
		{"double quoted identifier", `SELECT "weird?column" FROM t WHERE a=?`, `SELECT "weird?column" FROM t WHERE a=$1`},
		{"line comment", "SELECT 1 -- ? not a placeholder\nWHERE a=?", "SELECT 1 -- ? not a placeholder\nWHERE a=$1"},
		{"block comment", "SELECT /* ? ignored */ 1 WHERE a=?", "SELECT /* ? ignored */ 1 WHERE a=$1"},
		{"nested block comment", "SELECT /* a /* ? */ ? */ 1 WHERE a=?", "SELECT /* a /* ? */ ? */ 1 WHERE a=$1"},
		{"dollar quoted body", "SELECT $$ ? $$ , ?", "SELECT $$ ? $$ , $1"},
		{"tagged dollar quoted body", "SELECT $tag$ ? $tag$ , ?", "SELECT $tag$ ? $tag$ , $1"},
		{
			"multiline statement",
			"UPDATE runs SET status=?\nWHERE id=? AND owner_fence <= ?",
			"UPDATE runs SET status=$1\nWHERE id=$2 AND owner_fence <= $3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Rewrite(tc.query); got != tc.want {
				t.Fatalf("Rewrite(%q)\n got  %q\n want %q", tc.query, got, tc.want)
			}
		})
	}
}

func TestRewriteLeavesExistingDollarParametersAlone(t *testing.T) {
	// Already-native Postgres SQL must survive untouched so hand-written
	// dialect-specific statements can be mixed with translated ones.
	query := "SELECT id FROM runs WHERE id=$1 AND status=$2"
	if got := Rewrite(query); got != query {
		t.Fatalf("Rewrite rewrote native SQL: %q", got)
	}
}

func TestRewriteIsStableWhenNothingChanges(t *testing.T) {
	query := "SELECT count(*) FROM information_schema.tables"
	if got := Rewrite(query); got != query {
		t.Fatalf("expected identical string, got %q", got)
	}
}
