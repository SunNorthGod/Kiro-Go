package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RechargeRecord is one row of the recharge_records "进账" ledger: a single
// top-up applied to an api key. It is the symmetric counterpart of UsageRecord
// on the consumption side.
//
//   - Amount is the credits added by this one recharge (always > 0).
//   - BalanceAfter snapshots api_keys.credits_granted immediately AFTER this
//     recharge was folded in, so a page of records reads as a running statement.
//
// A key's spendable balance is credits_granted - credits_used (see GetKeyBalance),
// i.e. the difference of the two monotonic ledgers this table and usage_counters
// maintain.
type RechargeRecord struct {
	ID           int64
	APIKeyID     string
	Amount       float64
	Operator     string
	Note         string
	BalanceAfter float64
	CreatedAt    int64
}

// RechargeAPIKey credits amount onto the key and logs the recharge atomically.
//
// In one transaction it (1) increments api_keys.credits_granted by amount,
// reading back the new running total, then (2) appends a recharge_records row
// whose balance_after is that new total. Wrapping both writes in a single
// transaction guarantees the ledger row can never disagree with the counter it
// snapshots. amount must be strictly positive; a missing key is an error.
//
// It takes a *pgxpool.Pool (not a Querier) because it owns its own transaction.
func RechargeAPIKey(ctx context.Context, pool *pgxpool.Pool, apiKeyID string, amount float64, operator, note string) (RechargeRecord, error) {
	if apiKeyID == "" {
		return RechargeRecord{}, errors.New("db: recharge: api key id must not be empty")
	}
	if amount <= 0 {
		return RechargeRecord{}, fmt.Errorf("db: recharge: amount must be positive, got %g", amount)
	}

	rec := RechargeRecord{
		APIKeyID:  apiKeyID,
		Amount:    amount,
		Operator:  operator,
		Note:      note,
		CreatedAt: time.Now().Unix(),
	}

	err := WithTx(ctx, pool, func(tx pgx.Tx) error {
		// (1) Add the credits and read back the new running grant total.
		err := tx.QueryRow(ctx, `
UPDATE api_keys
   SET credits_granted = credits_granted + $2
 WHERE id = $1
RETURNING credits_granted`, apiKeyID, amount).Scan(&rec.BalanceAfter)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("db: recharge: api key %q not found", apiKeyID)
		}
		if err != nil {
			return fmt.Errorf("db: recharge: increment credits_granted: %w", err)
		}

		// (2) Log the recharge, snapshotting the post-increment total.
		if err := tx.QueryRow(ctx, `
INSERT INTO recharge_records (api_key_id, amount, operator, note, balance_after, created_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id`,
			rec.APIKeyID, rec.Amount, rec.Operator, rec.Note, rec.BalanceAfter, rec.CreatedAt,
		).Scan(&rec.ID); err != nil {
			return fmt.Errorf("db: recharge: insert record: %w", err)
		}
		return nil
	})
	if err != nil {
		return RechargeRecord{}, err
	}
	return rec, nil
}

// ListRechargeRecords returns a page of recharge rows for a key (newest first)
// plus the total row count for that key, for building paginated admin views. It
// mirrors ListUsageRecords: page is 1-based and page/pageSize < 1 are clamped to
// sane defaults.
func ListRechargeRecords(ctx context.Context, q Querier, apiKeyID string, page, pageSize int) (records []RechargeRecord, total int64, err error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 50
	}

	if err = q.QueryRow(ctx, `SELECT COUNT(*) FROM recharge_records WHERE api_key_id = $1`, apiKeyID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("db: count recharge records: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	offset := (page - 1) * pageSize
	rows, err := q.Query(ctx, `
SELECT id, api_key_id, amount, operator, note, balance_after, created_at
  FROM recharge_records
 WHERE api_key_id = $1
 ORDER BY created_at DESC, id DESC
 LIMIT $2 OFFSET $3`, apiKeyID, pageSize, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("db: list recharge records: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var r RechargeRecord
		if err := rows.Scan(
			&r.ID, &r.APIKeyID, &r.Amount, &r.Operator, &r.Note, &r.BalanceAfter, &r.CreatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("db: scan recharge record: %w", err)
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("db: list recharge records: %w", err)
	}
	return records, total, nil
}

// GetTotalRecharged returns the lifetime sum of credits recharged onto a key
// (SUM over recharge_records.amount). Zero rows yields 0, not an error. This is a
// reporting helper; the authoritative grant total lives in api_keys.credits_granted
// (see GetKeyBalance) and the two should agree barring an explicit adjustment.
func GetTotalRecharged(ctx context.Context, q Querier, apiKeyID string) (float64, error) {
	var total float64
	err := q.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount), 0) FROM recharge_records WHERE api_key_id = $1`,
		apiKeyID,
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("db: get total recharged: %w", err)
	}
	return total, nil
}
