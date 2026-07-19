// Package db implements the PostgreSQL persistence layer for the Kiro API proxy.
//
// It is a self-contained data-access layer: it deliberately does NOT import the
// config package. Instead it defines its own DTO structs (Account, APIKey,
// UsageCounter, UsageRecord) that mirror the persisted fields of
// config.Account / config.ApiKeyEntry. Keeping the DTOs local avoids a coupling
// (and potential import cycle) between storage and configuration, and lets the
// schema evolve independently of the on-disk JSON format.
//
// # Billing correctness
//
// The single most important design decision in this package is how per-key usage
// is tracked. The predecessor (a JSON/in-memory implementation) stored every
// request as a row in an append-only slice, capped the slice at a fixed number of
// rows per key (MAX_RECORDS_PER_KEY = 10000) by discarding the OLDEST rows, and
// then computed "credits used so far" by scanning and summing those detail rows.
//
// That is a latent billing bug: once a key crosses the cap, trimming the oldest
// detail rows makes the scanned sum go DOWN, so the key's measured usage silently
// rolls back. Quota checks that rely on the scanned sum then let the key keep
// spending past its limit — under-billing and, in the worst case, giving away
// paid capacity for free.
//
// This package fixes the root cause by separating the two concerns:
//
//   - usage_counters is the authoritative, monotonically-increasing ledger. Every
//     request is folded into it with an atomic UPSERT + INCREMENT. Values only ever
//     go up (except for an explicit, operator-initiated reset). Quota/credit checks
//     read ONLY this table (see GetTotalCredits / GetUsageCounters).
//
//   - usage_records is an OPTIONAL, purely-cosmetic detail log used for the "recent
//     requests" pagination in the admin UI. It may be pruned freely; billing never
//     reads it. Pruning it can never move the counters, so the roll-back bug cannot
//     recur.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is the subset of methods shared by *pgxpool.Pool and pgx.Tx. Every CRUD
// helper in this package accepts a Querier instead of a concrete pool so callers
// can run an operation either directly against the pool or inside a transaction
// (e.g. RecordUsageWithDetail wraps two writes in one pgx.Tx).
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Compile-time proof that both the pool and a transaction satisfy Querier.
var (
	_ Querier = (*pgxpool.Pool)(nil)
	_ Querier = (pgx.Tx)(nil)
)

// scannable is satisfied by both pgx.Row (QueryRow) and pgx.Rows (Query), letting
// the row-scan helpers work for both single-row and multi-row reads.
type scannable interface {
	Scan(dest ...any) error
}

// InitPool parses the DSN, applies conservative pool defaults (unless the DSN
// overrides them), opens the connection pool and verifies connectivity with a
// Ping. The returned pool is safe for concurrent use; the caller owns it and must
// Close it on shutdown.
//
// dsn accepts either the keyword/value form ("host=... user=... dbname=...") or a
// URL ("postgres://user:pass@host:5432/dbname?sslmode=disable"). Pool tuning
// parameters such as pool_max_conns may be supplied in the DSN and take priority
// over the defaults set here.
func InitPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: parse dsn: %w", err)
	}

	// Only apply defaults the DSN did not already specify.
	if cfg.MaxConns == 0 {
		cfg.MaxConns = 10
	}
	if cfg.MinConns == 0 {
		cfg.MinConns = 1
	}
	if cfg.MaxConnLifetime == 0 {
		cfg.MaxConnLifetime = time.Hour
	}
	if cfg.MaxConnIdleTime == 0 {
		cfg.MaxConnIdleTime = 30 * time.Minute
	}
	if cfg.HealthCheckPeriod == 0 {
		cfg.HealthCheckPeriod = time.Minute
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// Migrate creates every table and index used by this package. Each statement uses
// IF NOT EXISTS, so Migrate is idempotent and safe to run on every startup. The
// statements are executed in dependency order (parents before the tables that
// reference their ids).
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	for _, stmt := range schemaStatements {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("db: migrate: %w", err)
		}
	}
	return nil
}

// WithTx runs fn inside a single transaction, committing on success and rolling
// back on error or panic. It is a convenience wrapper for callers that want to
// group several CRUD helpers atomically.
func WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) (err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return
		}
		err = tx.Commit(ctx)
	}()
	err = fn(tx)
	return err
}
