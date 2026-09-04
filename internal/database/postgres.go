package database

import (
	"context"
	"database/sql/driver"
	"strconv"
	"strings"

	"github.com/uptrace/bun/driver/pgdriver"
)

// The app's SQL is written with ? placeholders, which SQLite understands but
// Postgres does not (it wants $1, $2, …). The rebind connector wraps pgdriver
// and rewrites every query on its way to the server, so one set of store SQL
// runs on both engines. Queries without a ? (including pgdriver's own $n
// bookkeeping and goose's) pass through untouched.

// pgConnector wraps a pgdriver.Connector so every connection it hands out
// rebinds placeholders.
type pgConnector struct {
	parent *pgdriver.Connector
}

func (c *pgConnector) Connect(ctx context.Context) (driver.Conn, error) {
	cn, err := c.parent.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return rebindConn{Conn: cn}, nil
}

func (c *pgConnector) Driver() driver.Driver {
	return rebindDriver{}
}

type rebindDriver struct{}

func (rebindDriver) Open(name string) (driver.Conn, error) {
	cn, err := (pgdriver.Driver{}).Open(name)
	if err != nil {
		return nil, err
	}
	return rebindConn{Conn: cn}, nil
}

// rebindConn delegates everything to the pgdriver connection, rewriting
// queries first. database/sql only calls the ExecContext/QueryContext paths
// when the connection implements them, which pgdriver's does; PrepareContext
// is kept in sync for explicit prepares.
type rebindConn struct {
	driver.Conn
}

func (c rebindConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	ec, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		// database/sql falls back to the (also rebinding) prepare path.
		return nil, driver.ErrSkip
	}
	return ec.ExecContext(ctx, rebind(query), args)
}

func (c rebindConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	qc, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return qc.QueryContext(ctx, rebind(query), args)
}

func (c rebindConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if pc, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return pc.PrepareContext(ctx, rebind(query))
	}
	return c.Conn.Prepare(rebind(query))
}

// rebind replaces each ? placeholder with $n, numbered in order of
// appearance. String literals, quoted identifiers and comments are copied
// verbatim so a question mark inside them is never mistaken for a
// placeholder.
func rebind(query string) string {
	if !strings.ContainsRune(query, '?') {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 16)
	n := 0
	for i := 0; i < len(query); {
		switch c := query[i]; c {
		case '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			i++
		case '\'', '"':
			// Copy the quoted region verbatim; a doubled quote character
			// ('' or "") is an escape, not the terminator.
			j := i + 1
			for j < len(query) {
				if query[j] == c {
					if j+1 < len(query) && query[j+1] == c {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			b.WriteString(query[i:j])
			i = j
		case '-':
			if i+1 < len(query) && query[i+1] == '-' {
				j := strings.IndexByte(query[i:], '\n')
				if j < 0 {
					j = len(query) - i
				} else {
					j++
				}
				b.WriteString(query[i : i+j])
				i += j
			} else {
				b.WriteByte(c)
				i++
			}
		case '/':
			if i+1 < len(query) && query[i+1] == '*' {
				j := strings.Index(query[i+2:], "*/")
				end := len(query)
				if j >= 0 {
					end = i + 2 + j + 2
				}
				b.WriteString(query[i:end])
				i = end
			} else {
				b.WriteByte(c)
				i++
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}
