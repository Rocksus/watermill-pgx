package sql

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
)

func runInTx(
	ctx context.Context,
	db Beginner,
	fn func(ctx context.Context, tx pgx.Tx) error,
) (err error) {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("could not begin transaction: %w", err)
	}

	defer func() {
		if err != nil {
			if rollbackErr := tx.Rollback(ctx); rollbackErr != nil {
				err = errors.Join(err, rollbackErr)
			}
			return
		}

		err = tx.Commit(ctx)
	}()

	return fn(ctx, tx)
}
