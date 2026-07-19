package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// APIKey is the db-layer DTO for a client-facing API key ("卡密"). It mirrors the
// persisted fields of config.ApiKeyEntry and adds the card-key fields requested
// for the PG schema: ExpiresAt, BoundAccountIDs and ParentKeyID.
//
// Limits with value 0 mean "unlimited" (same convention as config.ApiKeyEntry).
// TokensUsed / CreditsUsed / RequestsCount on this row are a convenience mirror of
// the key's own quota accounting; the authoritative per-(credential,model)
// breakdown lives in usage_counters.
type APIKey struct {
	ID       string
	Name     string
	Key      string
	Enabled  bool
	Migrated bool

	// Limits (0 == unlimited).
	TokenLimit  int64
	CreditLimit float64

	// Cumulative usage mirror (never auto-reset).
	TokensUsed    int64
	CreditsUsed   float64
	RequestsCount int64

	// Card-key fields.
	ExpiresAt       int64    // 0 == never expires
	BoundAccountIDs []string // account ids this key may use; empty == any
	ParentKeyID     string   // id of the key that minted this one; "" == root

	CreatedAt  int64
	LastUsedAt int64

	// CreditsGranted is the "进账" side of the unified ledger: the running total
	// of credits ever recharged onto this key (see db.RechargeAPIKey). The key's
	// spendable balance is CreditsGranted - CreditsUsed. Like CreditsUsed it is
	// monotonic and never auto-reset.
	CreditsGranted float64
}

// apiKeyColumns is the canonical column order for api_keys reads/writes. "key" is
// quoted throughout because it is a keyword in some SQL dialects.
const apiKeyColumns = `id, name, "key", enabled, migrated, token_limit, credit_limit, ` +
	`tokens_used, credits_used, requests_count, expires_at, bound_account_ids, ` +
	`parent_key_id, created_at, last_used_at, credits_granted`

const apiKeyUpsert = `
INSERT INTO api_keys (` + apiKeyColumns + `)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
ON CONFLICT (id) DO UPDATE SET
    name              = EXCLUDED.name,
    "key"             = EXCLUDED."key",
    enabled           = EXCLUDED.enabled,
    migrated          = EXCLUDED.migrated,
    token_limit       = EXCLUDED.token_limit,
    credit_limit      = EXCLUDED.credit_limit,
    tokens_used       = EXCLUDED.tokens_used,
    credits_used      = EXCLUDED.credits_used,
    requests_count    = EXCLUDED.requests_count,
    expires_at        = EXCLUDED.expires_at,
    bound_account_ids = EXCLUDED.bound_account_ids,
    parent_key_id     = EXCLUDED.parent_key_id,
    created_at        = EXCLUDED.created_at,
    last_used_at      = EXCLUDED.last_used_at,
    credits_granted   = EXCLUDED.credits_granted`

// marshalBoundIDs encodes the id slice as a JSON array. nil/empty becomes "[]" so
// the jsonb column never holds SQL NULL or the JSON literal null. Passing the
// bytes explicitly (rather than the []string) keeps encoding independent of the
// pgx query-exec mode.
func marshalBoundIDs(ids []string) ([]byte, error) {
	if len(ids) == 0 {
		return []byte("[]"), nil
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return nil, fmt.Errorf("db: encode bound_account_ids: %w", err)
	}
	return b, nil
}

// scanAPIKey reads one api_keys row (columns in apiKeyColumns order), decoding the
// bound_account_ids jsonb into a []string.
func scanAPIKey(row scannable) (APIKey, error) {
	var k APIKey
	var bound []byte
	err := row.Scan(
		&k.ID, &k.Name, &k.Key, &k.Enabled, &k.Migrated,
		&k.TokenLimit, &k.CreditLimit, &k.TokensUsed, &k.CreditsUsed, &k.RequestsCount,
		&k.ExpiresAt, &bound, &k.ParentKeyID, &k.CreatedAt, &k.LastUsedAt, &k.CreditsGranted,
	)
	if err != nil {
		return APIKey{}, err
	}
	if len(bound) > 0 && string(bound) != "null" {
		if err := json.Unmarshal(bound, &k.BoundAccountIDs); err != nil {
			return APIKey{}, fmt.Errorf("db: decode bound_account_ids: %w", err)
		}
	}
	return k, nil
}

// UpsertAPIKey inserts a new key or updates the existing one with the same id.
// It rejects empty id/key values up front. A duplicate `key` on a different id
// surfaces as the underlying unique-violation error.
func UpsertAPIKey(ctx context.Context, q Querier, k APIKey) error {
	if k.ID == "" {
		return errors.New("db: api key id must not be empty")
	}
	if strings.TrimSpace(k.Key) == "" {
		return errors.New("db: api key value must not be empty")
	}
	bound, err := marshalBoundIDs(k.BoundAccountIDs)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, apiKeyUpsert,
		k.ID, k.Name, k.Key, k.Enabled, k.Migrated,
		k.TokenLimit, k.CreditLimit, k.TokensUsed, k.CreditsUsed, k.RequestsCount,
		k.ExpiresAt, bound, k.ParentKeyID, k.CreatedAt, k.LastUsedAt, k.CreditsGranted,
	)
	if err != nil {
		return fmt.Errorf("db: upsert api key: %w", err)
	}
	return nil
}

// GetAPIKey returns the key with the given id, or (nil, nil) when not found.
func GetAPIKey(ctx context.Context, q Querier, id string) (*APIKey, error) {
	row := q.QueryRow(ctx, "SELECT "+apiKeyColumns+" FROM api_keys WHERE id = $1", id)
	k, err := scanAPIKey(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get api key: %w", err)
	}
	return &k, nil
}

// GetAPIKeyByValue returns the key whose value matches, or (nil, nil) when not
// found. Used on the request hot path to authenticate an incoming key.
func GetAPIKeyByValue(ctx context.Context, q Querier, key string) (*APIKey, error) {
	row := q.QueryRow(ctx, "SELECT "+apiKeyColumns+` FROM api_keys WHERE "key" = $1`, key)
	k, err := scanAPIKey(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get api key by value: %w", err)
	}
	return &k, nil
}

// ListAPIKeys returns every key ordered by created_at (oldest first).
func ListAPIKeys(ctx context.Context, q Querier) ([]APIKey, error) {
	rows, err := q.Query(ctx, "SELECT "+apiKeyColumns+" FROM api_keys ORDER BY created_at ASC, id ASC")
	if err != nil {
		return nil, fmt.Errorf("db: list api keys: %w", err)
	}
	defer rows.Close()

	var keys []APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, fmt.Errorf("db: list api keys: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list api keys: %w", err)
	}
	return keys, nil
}

// ListChildAPIKeys returns the keys minted by a given parent key id.
func ListChildAPIKeys(ctx context.Context, q Querier, parentID string) ([]APIKey, error) {
	rows, err := q.Query(ctx, "SELECT "+apiKeyColumns+" FROM api_keys WHERE parent_key_id = $1 ORDER BY created_at ASC, id ASC", parentID)
	if err != nil {
		return nil, fmt.Errorf("db: list child api keys: %w", err)
	}
	defer rows.Close()

	var keys []APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, fmt.Errorf("db: list child api keys: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list child api keys: %w", err)
	}
	return keys, nil
}

// GetChildrenAggregateUsage sums the per-key credit mirrors across every child of
// parentID: totalUsed is Σ credits_used and totalGranted is Σ credits_granted over
// the child rows. It powers the parent key's "额度池" (shared-pool) view — how much
// of the pool the children have collectively consumed vs. been granted. A parent
// with no children yields (0, 0, nil), not an error.
//
// Note this reads the api_keys row mirrors (kept in step by TouchAPIKeyUsage /
// RechargeAPIKey), not the usage_counters ledger; it is an aggregate display
// helper, not the billing source of truth.
func GetChildrenAggregateUsage(ctx context.Context, q Querier, parentID string) (totalUsed float64, totalGranted float64, err error) {
	err = q.QueryRow(ctx, `
SELECT COALESCE(SUM(credits_used), 0), COALESCE(SUM(credits_granted), 0)
  FROM api_keys
 WHERE parent_key_id = $1`, parentID).Scan(&totalUsed, &totalGranted)
	if err != nil {
		return 0, 0, fmt.Errorf("db: get children aggregate usage: %w", err)
	}
	return totalUsed, totalGranted, nil
}

// TouchAPIKeyUsage bumps the key's own quota mirror: adds tokens/credits, bumps
// requests_count and updates last_used_at. This keeps the per-key totals in step
// with the authoritative usage_counters ledger, but is not itself the billing
// source of truth. Negative deltas are ignored (mirror is monotonic).
func TouchAPIKeyUsage(ctx context.Context, q Querier, id string, tokens int64, credits float64, lastUsedAt int64) error {
	if tokens < 0 {
		tokens = 0
	}
	if credits < 0 {
		credits = 0
	}
	_, err := q.Exec(ctx, `
UPDATE api_keys
   SET tokens_used    = tokens_used  + $2,
       credits_used   = credits_used + $3,
       requests_count = requests_count + 1,
       last_used_at   = $4
 WHERE id = $1`, id, tokens, credits, lastUsedAt)
	if err != nil {
		return fmt.Errorf("db: touch api key usage: %w", err)
	}
	return nil
}

// ResetAPIKeyUsage clears the key's own quota mirror (tokens/credits/requests).
// last_used_at is preserved. This is an explicit operator action; it does NOT
// touch usage_counters.
func ResetAPIKeyUsage(ctx context.Context, q Querier, id string) error {
	_, err := q.Exec(ctx, `
UPDATE api_keys
   SET tokens_used = 0, credits_used = 0, requests_count = 0
 WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("db: reset api key usage: %w", err)
	}
	return nil
}

// DeleteAPIKey removes the key with the given id. Idempotent.
func DeleteAPIKey(ctx context.Context, q Querier, id string) error {
	if _, err := q.Exec(ctx, `DELETE FROM api_keys WHERE id = $1`, id); err != nil {
		return fmt.Errorf("db: delete api key: %w", err)
	}
	return nil
}

// DeleteAPIKeyWithSettlement deletes a sub-card and, in the same transaction,
// folds its consumed credits into the parent's credits_used mirror. This keeps
// the shared-pool invariant across the delete: the child's real spend stays
// deducted from the parent's budget forever, only its unused grant is freed.
func DeleteAPIKeyWithSettlement(ctx context.Context, pool *pgxpool.Pool, childID, parentID string, settleCredits float64) error {
	if settleCredits < 0 {
		settleCredits = 0
	}
	return WithTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE api_keys SET credits_used = credits_used + $2 WHERE id = $1`,
			parentID, settleCredits); err != nil {
			return fmt.Errorf("db: settle child usage into parent: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM api_keys WHERE id = $1`, childID); err != nil {
			return fmt.Errorf("db: delete api key: %w", err)
		}
		return nil
	})
}
