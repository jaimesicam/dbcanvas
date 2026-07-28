package sim

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
)

// maxTxnRetries bounds how many times a single logical operation retries
// after a 1213/1205. Retrying is required for correctness under Galera (the
// server does not retry a multi-statement transaction on the client's
// behalf), and is harmless overhead against a standalone/replication target.
const maxTxnRetries = 5

// withRetry runs fn inside a transaction against db, retrying the whole
// thing on a MySQL 1213 (deadlock, or a Galera certification conflict on a
// PXC target) or 1205 (lock wait timeout) with a small jittered backoff so
// concurrent retriers don't immediately re-collide. Takes an explicit db
// rather than always using Engine.Store.DB — MarketChaos, unlike every prior
// sim, sometimes runs agents against a specific PXC member's own pool (see
// pxc.go), and that connection needs the exact same retry semantics as the
// primary one.
func withRetry(ctx context.Context, db *sql.DB, retries *counters, fn func(tx *sql.Tx) error) error {
	var lastErr error
	for attempt := 0; attempt < maxTxnRetries; attempt++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin: %w", err)
		}
		err = fn(tx)
		if err == nil {
			if cerr := tx.Commit(); cerr != nil {
				err = cerr
			} else {
				return nil
			}
		}
		tx.Rollback()
		if !isRetryable(err) {
			return err
		}
		lastErr = err
		if retries != nil {
			retries.txnRetries.Add(1)
		}
		time.Sleep(time.Duration(5+rand.Intn(20)) * time.Millisecond * time.Duration(attempt+1))
	}
	return fmt.Errorf("giving up after %d retries: %w", maxTxnRetries, lastErr)
}

func isRetryable(err error) bool {
	var me *mysqldriver.MySQLError
	if errors.As(err, &me) {
		return me.Number == 1213 || me.Number == 1205
	}
	return false
}
