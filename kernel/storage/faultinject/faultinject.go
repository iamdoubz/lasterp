// SPDX-License-Identifier: AGPL-3.0-only

// Package faultinject opens a SQLite storage.DB whose reads can be made to
// fail partway through a scan, so the WP-3.3d retry-aliasing property is
// directly assertable instead of raced for.
//
// The property under test is "a retried read returns what one read returns".
// tenancy.WithTenant retries the whole callback on SQLITE_BUSY, so a callback
// that accumulates into a variable it captured from the enclosing scope hands
// the caller the first attempt's rows plus the second's. Reproducing that with
// real contention needs a BUSY to land in the window between the first
// rows.Next and the last — timing nobody can schedule. Injecting it instead
// makes each site a deterministic table row.
//
// It is a package rather than a test helper because the sites it proves are
// spread across kernel/changefeed, kernel/eventstore and kernel/metadata, and
// a helper cannot be shared between three packages' tests. Same reason and
// same shape as kernel/storage/conformance.
package faultinject

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"

	_ "modernc.org/sqlite" // the driver being wrapped

	"github.com/iamdoubz/lasterp/kernel/storage"
)

// busyMessage is what modernc.org/sqlite reports for SQLITE_BUSY and what
// storage.IsBusy matches on. Injecting anything else would exercise a retry
// path that does not exist.
const busyMessage = "database is locked"

// Injector arms and observes one fault. The zero value is disarmed.
type Injector struct {
	mu    sync.Mutex
	match string
	after int
	armed bool
	fired bool
}

// FailScan arms a single fault: the next query whose SQL contains match will
// fail after yielding afterRows rows, with the error SQLITE_BUSY produces.
// The fault disarms as it fires, so the callback's retry sees a healthy DB —
// which is the point: the retry succeeds, and what the caller receives is then
// entirely a question of whether the callback accumulated across attempts.
//
// afterRows must be at least 1. A fault before the first row leaves nothing
// accumulated and so proves nothing.
func (i *Injector) FailScan(match string, afterRows int) {
	if afterRows < 1 {
		panic("faultinject: afterRows must be >= 1; a fault before the first row proves nothing")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.match, i.after, i.armed, i.fired = match, afterRows, true, false
}

// Fired reports whether the armed fault actually fired. Every test asserts it:
// a fault that never fired makes the assertion that follows measure nothing
// (docs/19's non-vacuity rule).
func (i *Injector) Fired() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.fired
}

// consume reports whether this Next call, on a query with this SQL, is the one
// that fails.
func (i *Injector) consume(query string, rowsSoFar int) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.armed || rowsSoFar < i.after || !strings.Contains(query, i.match) {
		return false
	}
	i.armed, i.fired = false, true
	return true
}

// Open opens a SQLite DB at dsn behind the fault-injecting wrapper. The
// returned DB is an ordinary *storage.DB: callers migrate it and use it
// through the same package APIs as any other.
func Open(dsn string) (*storage.DB, *Injector, error) {
	base, err := baseDriver()
	if err != nil {
		return nil, nil, err
	}
	inj := &Injector{}
	db := sql.OpenDB(&connector{dsn: dsn, base: base, inj: inj})
	return &storage.DB{DB: db, Dialect: storage.SQLite}, inj, nil
}

// baseDriver returns the registered modernc.org/sqlite driver. Asking
// database/sql for it beats constructing sqlite.Driver{} directly: the zero
// value's unexported fields are not ours to reason about.
func baseDriver() (driver.Driver, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("faultinject: locate sqlite driver: %w", err)
	}
	d := db.Driver()
	_ = db.Close()
	return d, nil
}

type connector struct {
	dsn  string
	base driver.Driver
	inj  *Injector
}

func (c *connector) Connect(context.Context) (driver.Conn, error) {
	inner, err := c.base.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &conn{Conn: inner, inj: c.inj}, nil
}

func (c *connector) Driver() driver.Driver { return c.base }

// conn forwards everything and wraps the two paths that can produce rows:
// QueryerContext, which database/sql prefers, and Prepare, which it falls back
// to. Optional interfaces are forwarded only when the wrapped conn has them,
// so the wrapper cannot claim a capability the driver lacks.
type conn struct {
	driver.Conn
	inj *Injector
}

var (
	_ driver.QueryerContext     = (*conn)(nil)
	_ driver.ExecerContext      = (*conn)(nil)
	_ driver.ConnBeginTx        = (*conn)(nil)
	_ driver.ConnPrepareContext = (*conn)(nil)
)

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	r, err := q.QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	return &rows{Rows: r, query: query, inj: c.inj}, nil
}

func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	e, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return e.ExecContext(ctx, query, args)
}

func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	b, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return c.Conn.Begin() //nolint:staticcheck // the pre-context fallback is the whole point
	}
	return b.BeginTx(ctx, opts)
}

func (c *conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	p, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		return c.Prepare(query)
	}
	s, err := p.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return &stmt{Stmt: s, query: query, inj: c.inj}, nil
}

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	s, err := c.Conn.Prepare(query)
	if err != nil {
		return nil, err
	}
	return &stmt{Stmt: s, query: query, inj: c.inj}, nil
}

type stmt struct {
	driver.Stmt
	query string
	inj   *Injector
}

var (
	_ driver.StmtQueryContext = (*stmt)(nil)
	_ driver.StmtExecContext  = (*stmt)(nil)
)

func (s *stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := s.Stmt.(driver.StmtQueryContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	r, err := q.QueryContext(ctx, args)
	if err != nil {
		return nil, err
	}
	return &rows{Rows: r, query: s.query, inj: s.inj}, nil
}

func (s *stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	e, ok := s.Stmt.(driver.StmtExecContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return e.ExecContext(ctx, args)
}

func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	r, err := s.Stmt.Query(args) //nolint:staticcheck // the pre-context fallback is the whole point
	if err != nil {
		return nil, err
	}
	return &rows{Rows: r, query: s.query, inj: s.inj}, nil
}

// rows counts what it has yielded so the fault can land mid-scan, which is the
// only place it is interesting: rows already appended, more still to come.
type rows struct {
	driver.Rows
	query string
	inj   *Injector
	seen  int
}

func (r *rows) Next(dest []driver.Value) error {
	if r.inj.consume(r.query, r.seen) {
		return errors.New(busyMessage)
	}
	if err := r.Rows.Next(dest); err != nil {
		return err
	}
	r.seen++
	return nil
}
