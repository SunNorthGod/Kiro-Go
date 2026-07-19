package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Usage is the input to RecordUsage: one (or, if Requests > 1, a batch of)
// billable event(s) attributed to a specific api key / credential / model.
//
// CredentialID may be "" when the originating account is unknown; it then folds
// into the "unknown credential" counter row for that (api_key, model), which is
// still counted toward the key's total. This is deliberate: usage is never
// dropped just because the credential is unattributed.
type Usage struct {
	APIKeyID     string
	CredentialID string
	Model        string
	Requests     int64 // number of requests this event represents; <= 0 is treated as 1
	InputTokens  int64
	OutputTokens int64
	Credits      float64
	ClientIP     string // only used by the optional detail log; ignored by RecordUsage
	// Prompt-cache accounting for the optional detail log ONLY (display: cache
	// hit-rate panel). RecordUsage never reads these; billing stays in InputTokens.
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
}

// UsageCounter is one row of the authoritative usage_counters ledger: the
// monotonic totals for a single (api key, credential, model) triple.
type UsageCounter struct {
	APIKeyID                 string
	CredentialID             string
	Model                    string
	TotalRequests            int64
	TotalInputTokens         int64
	TotalOutputTokens        int64
	TotalCacheReadTokens     int64
	TotalCacheCreationTokens int64
	TotalCredits             float64
	UpdatedAt                int64
}

// UsageSummary is the api-key-level aggregate across all its counter rows.
type UsageSummary struct {
	APIKeyID                 string
	TotalRequests            int64
	TotalInputTokens         int64
	TotalOutputTokens        int64
	TotalCacheReadTokens     int64
	TotalCacheCreationTokens int64
	TotalCredits             float64
}

// UsageRecord is one row of the OPTIONAL usage_records detail log (display only).
type UsageRecord struct {
	ID                       int64
	APIKeyID                 string
	CredentialID             string
	Model                    string
	InputTokens              int64
	OutputTokens             int64
	Credits                  float64
	CreatedAt                int64
	ClientIP                 string
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
}

// recordUsageSQL is the heart of the billing fix: an atomic UPSERT that, on
// conflict, ADDS the new event to the existing totals. Every field is
// incremented as existing + EXCLUDED (the row we tried to insert), so the counter
// is monotonic and lock-free under concurrency — Postgres serializes the conflict
// resolution per row. This never reads or depends on the detail log, so pruning
// usage_records can never move these totals (the root cause of the old
// trim-then-rescan roll-back bug).
const recordUsageSQL = `
INSERT INTO usage_counters (
    api_key_id, credential_id, model,
    total_requests, total_input_tokens, total_output_tokens,
    total_cache_read_tokens, total_cache_creation_tokens, total_credits, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (api_key_id, credential_id, model) DO UPDATE SET
    total_requests              = usage_counters.total_requests              + EXCLUDED.total_requests,
    total_input_tokens          = usage_counters.total_input_tokens          + EXCLUDED.total_input_tokens,
    total_output_tokens         = usage_counters.total_output_tokens         + EXCLUDED.total_output_tokens,
    total_cache_read_tokens     = usage_counters.total_cache_read_tokens     + EXCLUDED.total_cache_read_tokens,
    total_cache_creation_tokens = usage_counters.total_cache_creation_tokens + EXCLUDED.total_cache_creation_tokens,
    total_credits               = usage_counters.total_credits               + EXCLUDED.total_credits,
    updated_at                  = EXCLUDED.updated_at`

// RecordUsage folds one usage event into the authoritative usage_counters ledger
// with a single atomic UPSERT + INCREMENT. This is the billing source of truth
// and the only function quota checks should feed. It does NOT write the detail
// log; use AddUsageRecord or RecordUsageWithDetail for that.
//
// q may be a *pgxpool.Pool or a pgx.Tx.
func RecordUsage(ctx context.Context, q Querier, u Usage) error {
	if u.APIKeyID == "" {
		return fmt.Errorf("db: record usage: api_key_id must not be empty")
	}
	req := u.Requests
	if req <= 0 {
		req = 1
	}
	_, err := q.Exec(ctx, recordUsageSQL,
		u.APIKeyID, u.CredentialID, u.Model,
		req, u.InputTokens, u.OutputTokens,
		u.CacheReadInputTokens, u.CacheCreationInputTokens, u.Credits, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("db: record usage: %w", err)
	}
	return nil
}

// RecordUsageWithDetail atomically increments the authoritative counter AND
// appends a row to the optional detail log, in one transaction. If the detail
// insert fails the counter increment is rolled back too, so the two stay
// consistent. Callers that don't need the detail log should prefer RecordUsage.
func RecordUsageWithDetail(ctx context.Context, pool *pgxpool.Pool, u Usage) error {
	return WithTx(ctx, pool, func(tx pgx.Tx) error {
		if err := RecordUsage(ctx, tx, u); err != nil {
			return err
		}
		_, err := AddUsageRecord(ctx, tx, UsageRecord{
			APIKeyID:                 u.APIKeyID,
			CredentialID:             u.CredentialID,
			Model:                    u.Model,
			InputTokens:              u.InputTokens,
			OutputTokens:             u.OutputTokens,
			Credits:                  u.Credits,
			CreatedAt:                time.Now().Unix(),
			ClientIP:                 u.ClientIP,
			CacheReadInputTokens:     u.CacheReadInputTokens,
			CacheCreationInputTokens: u.CacheCreationInputTokens,
		})
		return err
	})
}

// GetTotalCredits returns the total credits a key has consumed, read DIRECTLY
// from the monotonic ledger (SUM over usage_counters). It never scans the detail
// log, so the value only ever rises with real usage and is immune to detail-log
// pruning. This is the function credit-limit enforcement must call.
func GetTotalCredits(ctx context.Context, q Querier, apiKeyID string) (float64, error) {
	var total float64
	err := q.QueryRow(ctx,
		`SELECT COALESCE(SUM(total_credits), 0) FROM usage_counters WHERE api_key_id = $1`,
		apiKeyID,
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("db: get total credits: %w", err)
	}
	return total, nil
}

// GetKeyBalance returns a key's unified-ledger balance in one row read: granted is
// api_keys.credits_granted (the "进账" total from RechargeAPIKey), used is
// api_keys.credits_used, and balance is granted - used. It reads only the two
// monotonic mirror columns on the api_keys row — it does NOT scan usage_counters
// or the detail logs — so it is cheap enough for the request hot path. A missing
// key is reported as an error rather than a silent zero balance.
func GetKeyBalance(ctx context.Context, q Querier, apiKeyID string) (granted, used, balance float64, err error) {
	err = q.QueryRow(ctx,
		`SELECT credits_granted, credits_used FROM api_keys WHERE id = $1`,
		apiKeyID,
	).Scan(&granted, &used)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, 0, fmt.Errorf("db: get key balance: api key %q not found", apiKeyID)
	}
	if err != nil {
		return 0, 0, 0, fmt.Errorf("db: get key balance: %w", err)
	}
	return granted, used, granted - used, nil
}

// GetUsageSummary returns the api-key-level aggregate (SUM across every
// credential/model counter row). Like GetTotalCredits it reads only the ledger.
// Zero rows yields an all-zero summary, not an error.
func GetUsageSummary(ctx context.Context, q Querier, apiKeyID string) (UsageSummary, error) {
	s := UsageSummary{APIKeyID: apiKeyID}
	err := q.QueryRow(ctx, `
SELECT COALESCE(SUM(total_requests), 0),
       COALESCE(SUM(total_input_tokens), 0),
       COALESCE(SUM(total_output_tokens), 0),
       COALESCE(SUM(total_cache_read_tokens), 0),
       COALESCE(SUM(total_cache_creation_tokens), 0),
       COALESCE(SUM(total_credits), 0)
  FROM usage_counters
 WHERE api_key_id = $1`, apiKeyID).Scan(
		&s.TotalRequests, &s.TotalInputTokens, &s.TotalOutputTokens,
		&s.TotalCacheReadTokens, &s.TotalCacheCreationTokens, &s.TotalCredits,
	)
	if err != nil {
		return UsageSummary{}, fmt.Errorf("db: get usage summary: %w", err)
	}
	return s, nil
}

// GlobalCacheStats is the deployment-wide prompt-cache aggregate (all keys).
type GlobalCacheStats struct {
	CacheReadTokens     int64
	CacheCreationTokens int64
	InputTokens         int64
}

// GetGlobalCacheStats sums the prompt-cache token counters across every key in
// one cheap aggregate query, for the admin overview's global cache-hit-rate tile.
// hit rate = CacheReadTokens / InputTokens (InputTokens already includes cached
// prompt tokens). Display-only; never touches billing.
func GetGlobalCacheStats(ctx context.Context, q Querier) (GlobalCacheStats, error) {
	var s GlobalCacheStats
	err := q.QueryRow(ctx, `
SELECT COALESCE(SUM(total_cache_read_tokens), 0),
       COALESCE(SUM(total_cache_creation_tokens), 0),
       COALESCE(SUM(total_input_tokens), 0)
  FROM usage_counters`).Scan(&s.CacheReadTokens, &s.CacheCreationTokens, &s.InputTokens)
	if err != nil {
		return GlobalCacheStats{}, fmt.Errorf("db: get global cache stats: %w", err)
	}
	return s, nil
}

// GetRecentCacheStats sums the prompt-cache token counters over the DISPLAY-ONLY
// usage_records detail log for events at/after sinceUnix. Unlike GetGlobalCacheStats
// (which reads the lifetime billing ledger), this yields a rolling recent-window
// hit rate that reflects CURRENT cache behaviour, so a large historical base of
// un-cached input can't drag the number down forever. input_tokens here has the
// same semantics as the ledger (already includes cached prompt tokens), because
// both are written from the same Usage event. Display-only; never touches billing.
func GetRecentCacheStats(ctx context.Context, q Querier, sinceUnix int64) (GlobalCacheStats, error) {
	var s GlobalCacheStats
	err := q.QueryRow(ctx, `
SELECT COALESCE(SUM(cache_read_input_tokens), 0),
       COALESCE(SUM(cache_creation_input_tokens), 0),
       COALESCE(SUM(input_tokens), 0)
  FROM usage_records
 WHERE created_at >= $1`, sinceUnix).Scan(&s.CacheReadTokens, &s.CacheCreationTokens, &s.InputTokens)
	if err != nil {
		return GlobalCacheStats{}, fmt.Errorf("db: get recent cache stats: %w", err)
	}
	return s, nil
}

// GetUsageCounters returns the per-(credential, model) counter rows for a key,
// ordered by credits desc. This is the detailed breakdown behind GetUsageSummary.
func GetUsageCounters(ctx context.Context, q Querier, apiKeyID string) ([]UsageCounter, error) {
	rows, err := q.Query(ctx, `
SELECT api_key_id, credential_id, model,
       total_requests, total_input_tokens, total_output_tokens,
       total_cache_read_tokens, total_cache_creation_tokens, total_credits, updated_at
  FROM usage_counters
 WHERE api_key_id = $1
 ORDER BY total_credits DESC, model ASC`, apiKeyID)
	if err != nil {
		return nil, fmt.Errorf("db: get usage counters: %w", err)
	}
	return scanUsageCounters(rows)
}

// GetAllUsageCounters returns every counter row across all keys (admin overview).
func GetAllUsageCounters(ctx context.Context, q Querier) ([]UsageCounter, error) {
	rows, err := q.Query(ctx, `
SELECT api_key_id, credential_id, model,
       total_requests, total_input_tokens, total_output_tokens,
       total_cache_read_tokens, total_cache_creation_tokens, total_credits, updated_at
  FROM usage_counters
 ORDER BY api_key_id ASC, total_credits DESC`)
	if err != nil {
		return nil, fmt.Errorf("db: get all usage counters: %w", err)
	}
	return scanUsageCounters(rows)
}

func scanUsageCounters(rows pgx.Rows) ([]UsageCounter, error) {
	defer rows.Close()
	var out []UsageCounter
	for rows.Next() {
		var c UsageCounter
		if err := rows.Scan(
			&c.APIKeyID, &c.CredentialID, &c.Model,
			&c.TotalRequests, &c.TotalInputTokens, &c.TotalOutputTokens,
			&c.TotalCacheReadTokens, &c.TotalCacheCreationTokens, &c.TotalCredits, &c.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan usage counter: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: scan usage counters: %w", err)
	}
	return out, nil
}

// ResetUsageCounters deletes all counter rows for a key. This is an explicit,
// operator-initiated action (e.g. an admin "reset usage" button) — the ONLY
// sanctioned way the monotonic totals may go down. It is intentionally separate
// from any automatic path so routine operations can never roll usage back.
func ResetUsageCounters(ctx context.Context, q Querier, apiKeyID string) error {
	if _, err := q.Exec(ctx, `DELETE FROM usage_counters WHERE api_key_id = $1`, apiKeyID); err != nil {
		return fmt.Errorf("db: reset usage counters: %w", err)
	}
	return nil
}

// ---- Optional detail log (display only; never used for billing) ----

// AddUsageRecord appends a detail row and returns its generated id. Safe to skip
// entirely; billing does not depend on these rows.
func AddUsageRecord(ctx context.Context, q Querier, r UsageRecord) (int64, error) {
	if r.CreatedAt == 0 {
		r.CreatedAt = time.Now().Unix()
	}
	var id int64
	err := q.QueryRow(ctx, `
INSERT INTO usage_records (
    api_key_id, credential_id, model, input_tokens, output_tokens, credits, created_at, client_ip,
    cache_read_input_tokens, cache_creation_input_tokens
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING id`,
		r.APIKeyID, r.CredentialID, r.Model, r.InputTokens, r.OutputTokens, r.Credits, r.CreatedAt, r.ClientIP,
		r.CacheReadInputTokens, r.CacheCreationInputTokens,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("db: add usage record: %w", err)
	}
	return id, nil
}

// ListUsageRecords returns a page of detail rows for a key (newest first) plus the
// total row count for that key, for building paginated admin views. page is
// 1-based; page/pageSize < 1 are clamped to sane values.
func ListUsageRecords(ctx context.Context, q Querier, apiKeyID string, page, pageSize int) (records []UsageRecord, total int64, err error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 50
	}

	if err = q.QueryRow(ctx, `SELECT COUNT(*) FROM usage_records WHERE api_key_id = $1`, apiKeyID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("db: count usage records: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	offset := (page - 1) * pageSize
	rows, err := q.Query(ctx, `
SELECT id, api_key_id, credential_id, model, input_tokens, output_tokens, credits, created_at, client_ip,
       cache_read_input_tokens, cache_creation_input_tokens
  FROM usage_records
 WHERE api_key_id = $1
 ORDER BY created_at DESC, id DESC
 LIMIT $2 OFFSET $3`, apiKeyID, pageSize, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("db: list usage records: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var r UsageRecord
		if err := rows.Scan(
			&r.ID, &r.APIKeyID, &r.CredentialID, &r.Model,
			&r.InputTokens, &r.OutputTokens, &r.Credits, &r.CreatedAt, &r.ClientIP,
			&r.CacheReadInputTokens, &r.CacheCreationInputTokens,
		); err != nil {
			return nil, 0, fmt.Errorf("db: scan usage record: %w", err)
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("db: list usage records: %w", err)
	}
	return records, total, nil
}

// PruneUsageRecords deletes detail rows for a key beyond the newest keep rows and
// returns how many were removed. This is the SAFE analogue of the old
// MAX_RECORDS_PER_KEY trimming: because billing reads usage_counters (not these
// rows), pruning here can never affect quota or credit totals. keep <= 0 deletes
// all detail rows for the key.
func PruneUsageRecords(ctx context.Context, q Querier, apiKeyID string, keep int) (int64, error) {
	if keep <= 0 {
		ct, err := q.Exec(ctx, `DELETE FROM usage_records WHERE api_key_id = $1`, apiKeyID)
		if err != nil {
			return 0, fmt.Errorf("db: prune usage records: %w", err)
		}
		return ct.RowsAffected(), nil
	}
	ct, err := q.Exec(ctx, `
DELETE FROM usage_records
 WHERE api_key_id = $1
   AND id NOT IN (
       SELECT id FROM usage_records
        WHERE api_key_id = $1
        ORDER BY created_at DESC, id DESC
        LIMIT $2
   )`, apiKeyID, keep)
	if err != nil {
		return 0, fmt.Errorf("db: prune usage records: %w", err)
	}
	return ct.RowsAffected(), nil
}

// PruneUsageRecordsBefore deletes detail rows older than cutoffUnix across ALL
// keys and returns how many were removed. This is the time-based retention for
// the display-only log: billing reads usage_counters, so pruning here can never
// affect quota or credit totals. Callers must keep at least the 90 days that
// GetDailyUsage can be asked for.
func PruneUsageRecordsBefore(ctx context.Context, q Querier, cutoffUnix int64) (int64, error) {
	ct, err := q.Exec(ctx, `DELETE FROM usage_records WHERE created_at < $1`, cutoffUnix)
	if err != nil {
		return 0, fmt.Errorf("db: prune usage records before: %w", err)
	}
	return ct.RowsAffected(), nil
}

// DailyUsage is one calendar day's aggregate over usage_records, for the admin
// overview trend chart.
type DailyUsage struct {
	Date         string
	Requests     int64
	Credits      float64
	InputTokens  int64
	OutputTokens int64
}

// GetDailyUsage aggregates usage_records by CST (UTC+8) calendar day for the last
// `days` days (default 14, capped 90), oldest→newest. This is the authoritative,
// persistent daily history (it survives restarts), matching the Rust panel which
// derives daily stats by scanning persisted usage records. Days without records
// are simply absent; the caller fills the gaps for a continuous x-axis.
func GetDailyUsage(ctx context.Context, q Querier, days int) ([]DailyUsage, error) {
	if days <= 0 {
		days = 14
	}
	if days > 90 {
		days = 90
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	rows, err := q.Query(ctx, `
SELECT to_char(to_timestamp(created_at) AT TIME ZONE 'Asia/Shanghai', 'YYYY-MM-DD') AS day,
       COUNT(*)                         AS requests,
       COALESCE(SUM(credits), 0)        AS credits,
       COALESCE(SUM(input_tokens), 0)   AS input_tokens,
       COALESCE(SUM(output_tokens), 0)  AS output_tokens
  FROM usage_records
 WHERE created_at >= $1
 GROUP BY day
 ORDER BY day ASC`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("db: get daily usage: %w", err)
	}
	defer rows.Close()
	var out []DailyUsage
	for rows.Next() {
		var d DailyUsage
		if err := rows.Scan(&d.Date, &d.Requests, &d.Credits, &d.InputTokens, &d.OutputTokens); err != nil {
			return nil, fmt.Errorf("db: scan daily usage: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: daily usage rows: %w", err)
	}
	return out, nil
}
