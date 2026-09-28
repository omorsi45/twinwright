package pgsql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync"

	"github.com/jackc/pgx/v5/stdlib"
)

// DriverName is the database/sql driver name Twinwright's PostgreSQL store
// opens. It is pgx with statement-text translation layered on top.
const DriverName = "twinwright-pgx"

var registerOnce sync.Once

// Register installs the translating driver. It is safe to call repeatedly;
// database/sql panics on a duplicate registration, so the work happens once.
func Register() {
	registerOnce.Do(func() {
		sql.Register(DriverName, WrapDriver(stdlib.GetDefaultDriver()))
	})
}

// WrapDriver returns a driver that rewrites `?` placeholders to $N before
// delegating to base.
//
// database/sql discovers driver capabilities with type assertions, so the
// wrapper must advertise exactly what the base supports and no more: a
// wrapper that claimed ExecerContext over a base without it would send
// statements into a method that cannot run them. Three concrete shapes cover
// the cases that occur in practice - a full pgx connection, a connection with
// only the context-aware SQL methods, and a legacy Prepare-only connection.
func WrapDriver(base driver.Driver) driver.Driver {
	if ctxBase, ok := base.(driver.DriverContext); ok {
		return contextDriver{base: base, ctxBase: ctxBase}
	}
	return plainDriver{base: base}
}

type plainDriver struct{ base driver.Driver }

func (d plainDriver) Open(dsn string) (driver.Conn, error) { return openWrapped(d.base, dsn) }

type contextDriver struct {
	base    driver.Driver
	ctxBase driver.DriverContext
}

func (d contextDriver) Open(dsn string) (driver.Conn, error) { return openWrapped(d.base, dsn) }

func (d contextDriver) OpenConnector(dsn string) (driver.Connector, error) {
	inner, err := d.ctxBase.OpenConnector(dsn)
	if err != nil {
		return nil, err
	}
	return wrappedConnector{inner: inner, driver: d}, nil
}

type wrappedConnector struct {
	inner  driver.Connector
	driver driver.Driver
}

func (c wrappedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return wrapConn(conn), nil
}

func (c wrappedConnector) Driver() driver.Driver { return c.driver }

func openWrapped(base driver.Driver, dsn string) (driver.Conn, error) {
	conn, err := base.Open(dsn)
	if err != nil {
		return nil, err
	}
	return wrapConn(conn), nil
}

// wrapConn selects the narrowest wrapper shape that matches the base.
func wrapConn(conn driver.Conn) driver.Conn {
	execer, hasExecer := conn.(driver.ExecerContext)
	queryer, hasQueryer := conn.(driver.QueryerContext)
	preparer, hasPreparer := conn.(driver.ConnPrepareContext)
	if !hasExecer || !hasQueryer || !hasPreparer {
		return baseConn{conn: conn}
	}
	sqlLevel := sqlConn{
		baseConn: baseConn{conn: conn},
		execer:   execer,
		queryer:  queryer,
		preparer: preparer,
	}
	beginner, hasBeginner := conn.(driver.ConnBeginTx)
	pinger, hasPinger := conn.(driver.Pinger)
	resetter, hasResetter := conn.(driver.SessionResetter)
	validator, hasValidator := conn.(driver.Validator)
	checker, hasChecker := conn.(driver.NamedValueChecker)
	if hasBeginner && hasPinger && hasResetter && hasValidator && hasChecker {
		return fullConn{
			sqlConn:   sqlLevel,
			beginner:  beginner,
			pinger:    pinger,
			resetter:  resetter,
			validator: validator,
			checker:   checker,
		}
	}
	return sqlLevel
}

// baseConn covers the mandatory driver.Conn surface.
type baseConn struct{ conn driver.Conn }

func (c baseConn) Prepare(query string) (driver.Stmt, error) { return c.conn.Prepare(Rewrite(query)) }
func (c baseConn) Close() error                              { return c.conn.Close() }
func (c baseConn) Begin() (driver.Tx, error)                 { return c.conn.Begin() } //nolint:staticcheck // required by driver.Conn

// sqlConn adds the context-aware statement methods, which are the only places
// statement text crosses the boundary.
type sqlConn struct {
	baseConn
	execer   driver.ExecerContext
	queryer  driver.QueryerContext
	preparer driver.ConnPrepareContext
}

func (c sqlConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.execer.ExecContext(ctx, Rewrite(query), args)
}

func (c sqlConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.queryer.QueryContext(ctx, Rewrite(query), args)
}

func (c sqlConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	return c.preparer.PrepareContext(ctx, Rewrite(query))
}

// fullConn is the shape pgx presents. Its extra methods carry no statement
// text, so they forward unchanged; they are advertised only because
// database/sql needs them for transaction options, health checks and
// connection reuse.
type fullConn struct {
	sqlConn
	beginner  driver.ConnBeginTx
	pinger    driver.Pinger
	resetter  driver.SessionResetter
	validator driver.Validator
	checker   driver.NamedValueChecker
}

func (c fullConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.beginner.BeginTx(ctx, opts)
}
func (c fullConn) Ping(ctx context.Context) error             { return c.pinger.Ping(ctx) }
func (c fullConn) ResetSession(ctx context.Context) error     { return c.resetter.ResetSession(ctx) }
func (c fullConn) IsValid() bool                              { return c.validator.IsValid() }
func (c fullConn) CheckNamedValue(v *driver.NamedValue) error { return c.checker.CheckNamedValue(v) }
