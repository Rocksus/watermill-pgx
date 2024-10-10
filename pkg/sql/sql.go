package sql

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"strings"
)

// ContextExecutor can perform SQL queries with context
type ContextExecutor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (commandTag pgconn.CommandTag, err error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Beginner begins transactions.
type Beginner interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
	ContextExecutor
}

// sqlArgsToLog is used for "lazy" generating sql args strings to logger
type sqlArgsToLog []interface{}

func (s sqlArgsToLog) String() string {
	var strArgs []string
	for _, arg := range s {
		strArgs = append(strArgs, fmt.Sprintf("%v", arg))
	}

	return strings.Join(strArgs, ",")
}

type Scanner interface {
	Scan(dest ...any) error
}

type Query struct {
	Query string
	Args  []any
}

func (q Query) IsZero() bool {
	return q.Query == ""
}

func (q Query) String() string {
	return fmt.Sprintf("%s %s", q.Query, sqlArgsToLog(q.Args))
}
