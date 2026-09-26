package pgsql

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"testing"
)

// recordingDriver is a minimal base driver that records the statement text it
// is handed, so the wrapper's translation can be asserted without a server.
type recordingDriver struct{ seen *[]string }

func (d recordingDriver) Open(string) (driver.Conn, error) { return recordingConn{seen: d.seen}, nil }

type recordingConn struct{ seen *[]string }

func (c recordingConn) Prepare(query string) (driver.Stmt, error) {
	*c.seen = append(*c.seen, query)
	return recordingStmt{}, nil
}
func (c recordingConn) Close() error              { return nil }
func (c recordingConn) Begin() (driver.Tx, error) { return nil, errors.New("not used") }
func (c recordingConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	*c.seen = append(*c.seen, query)
	return driver.RowsAffected(1), nil
}
func (c recordingConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	*c.seen = append(*c.seen, query)
	return emptyRows{}, nil
}
func (c recordingConn) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	*c.seen = append(*c.seen, query)
	return recordingStmt{}, nil
}

type recordingStmt struct{}

func (recordingStmt) Close() error                               { return nil }
func (recordingStmt) NumInput() int                              { return -1 }
func (recordingStmt) Exec([]driver.Value) (driver.Result, error) { return driver.RowsAffected(0), nil }
func (recordingStmt) Query([]driver.Value) (driver.Rows, error)  { return emptyRows{}, nil }

type emptyRows struct{}

func (emptyRows) Columns() []string         { return nil }
func (emptyRows) Close() error              { return nil }
func (emptyRows) Next([]driver.Value) error { return io.EOF }

func TestWrapDriverTranslatesPreparedStatements(t *testing.T) {
	var seen []string
	conn, err := WrapDriver(recordingDriver{seen: &seen}).Open("ignored")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err = conn.Prepare("SELECT id FROM runs WHERE id=?"); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(seen) != 1 || seen[0] != "SELECT id FROM runs WHERE id=$1" {
		t.Fatalf("prepare did not translate: %q", seen)
	}
}

func TestWrapDriverTranslatesExecAndQueryContext(t *testing.T) {
	var seen []string
	conn, err := WrapDriver(recordingDriver{seen: &seen}).Open("ignored")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	execer, ok := conn.(driver.ExecerContext)
	if !ok {
		t.Fatal("wrapped conn must forward ExecerContext when the base supports it")
	}
	if _, err = execer.ExecContext(context.Background(), "UPDATE runs SET status=? WHERE id=?", nil); err != nil {
		t.Fatalf("exec: %v", err)
	}
	queryer, ok := conn.(driver.QueryerContext)
	if !ok {
		t.Fatal("wrapped conn must forward QueryerContext when the base supports it")
	}
	rows, err := queryer.QueryContext(context.Background(), "SELECT 1 WHERE a=?", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	_ = rows.Close()
	want := []string{"UPDATE runs SET status=$1 WHERE id=$2", "SELECT 1 WHERE a=$1"}
	if len(seen) != len(want) {
		t.Fatalf("unexpected statements: %q", seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("statement %d\n got  %q\n want %q", i, seen[i], want[i])
		}
	}
}

func TestWrapDriverTranslatesPrepareContext(t *testing.T) {
	var seen []string
	conn, err := WrapDriver(recordingDriver{seen: &seen}).Open("ignored")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	preparer, ok := conn.(driver.ConnPrepareContext)
	if !ok {
		t.Fatal("wrapped conn must forward ConnPrepareContext when the base supports it")
	}
	if _, err = preparer.PrepareContext(context.Background(), "INSERT INTO events VALUES(?,?)"); err != nil {
		t.Fatalf("prepare context: %v", err)
	}
	if len(seen) != 1 || seen[0] != "INSERT INTO events VALUES($1,$2)" {
		t.Fatalf("prepare context did not translate: %q", seen)
	}
}

// A base conn without the context interfaces must not gain them: database/sql
// probes with type assertions and would otherwise call through to nothing.
type legacyOnlyDriver struct{ seen *[]string }

func (d legacyOnlyDriver) Open(string) (driver.Conn, error) {
	return legacyOnlyConn{seen: d.seen}, nil
}

type legacyOnlyConn struct{ seen *[]string }

func (c legacyOnlyConn) Prepare(query string) (driver.Stmt, error) {
	*c.seen = append(*c.seen, query)
	return recordingStmt{}, nil
}
func (c legacyOnlyConn) Close() error              { return nil }
func (c legacyOnlyConn) Begin() (driver.Tx, error) { return nil, errors.New("not used") }

func TestWrapDriverDoesNotFabricateCapabilities(t *testing.T) {
	var seen []string
	conn, err := WrapDriver(legacyOnlyDriver{seen: &seen}).Open("ignored")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, ok := conn.(driver.ExecerContext); ok {
		t.Fatal("wrapper advertised ExecerContext for a base conn that lacks it")
	}
	if _, ok := conn.(driver.QueryerContext); ok {
		t.Fatal("wrapper advertised QueryerContext for a base conn that lacks it")
	}
	if _, err = conn.Prepare("DELETE FROM runs WHERE id=?"); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(seen) != 1 || seen[0] != "DELETE FROM runs WHERE id=$1" {
		t.Fatalf("legacy prepare did not translate: %q", seen)
	}
}
