package sqlengine

import (
	"context"
	"database/sql"
	"database/sql/driver"

	"github.com/jackc/pgx/v5/stdlib"
)

// The wrapper fixes the isolation level of read-only transactions. SQL and
// bound parameters, including native host SQL, pass through intact.
//
// It does not serialize writes. It used to take one advisory lock for every
// write transaction in the database, which is where that lock could be taken
// without knowing anything — and knowing nothing is exactly the problem: a
// driver cannot tell which dataset a transaction is for, so it serialized
// writes to unrelated datasets against each other. The engine takes the lock
// now, per dataset, where the dataset is known.
type pgDriver struct{}

func (pgDriver) Open(dsn string) (driver.Conn, error) {
	c, e := stdlib.GetDefaultDriver().Open(dsn)
	if e != nil {
		return nil, e
	}
	return &pgConn{Conn: c}, nil
}

type pgConn struct{ driver.Conn }

func (c *pgConn) Prepare(q string) (driver.Stmt, error) { return c.Conn.Prepare(q) }
func (c *pgConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, q)
}
func (c *pgConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
}
func (c *pgConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}
func (c *pgConn) Ping(ctx context.Context) error { return c.Conn.(driver.Pinger).Ping(ctx) }
func (c *pgConn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}
func (c *pgConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}
func (c *pgConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if opts.ReadOnly {
		opts.Isolation = driver.IsolationLevel(sql.LevelRepeatableRead)
	}
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
func init() { sql.Register("tessera-postgres", pgDriver{}) }
