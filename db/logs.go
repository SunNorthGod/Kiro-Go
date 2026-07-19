package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RequestLog is one audit row in request_logs: a single proxied request (success
// or error) with its cost, latency and error classification.
type RequestLog struct {
	Time       int64
	Endpoint   string
	Model      string
	AccountID  string
	APIKeyID   string
	Status     string
	Error      string
	ErrorType  string
	Tokens     int64
	Credits    float64
	DurationMs int64
}

// AddRequestLog appends one audit row. Cheap single INSERT; callers should invoke
// it off the request hot path (async) so logging never adds latency.
func AddRequestLog(ctx context.Context, q Querier, r RequestLog) error {
	_, err := q.Exec(ctx, `
INSERT INTO request_logs
    (ts, endpoint, model, account_id, api_key_id, status, error, error_type, tokens, credits, duration_ms)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		r.Time, r.Endpoint, r.Model, r.AccountID, r.APIKeyID, r.Status, r.Error, r.ErrorType, r.Tokens, r.Credits, r.DurationMs)
	if err != nil {
		return fmt.Errorf("db: add request log: %w", err)
	}
	return nil
}

// ListRequestLogs returns the newest `limit` audit rows (newest first). Optional
// filters: status ("success"/"error"/"") and endpoint ("claude"/"openai"/"").
func ListRequestLogs(ctx context.Context, q Querier, limit int, status, endpoint string) ([]RequestLog, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	rows, err := q.Query(ctx, `
SELECT ts, endpoint, model, account_id, api_key_id, status, error, error_type, tokens, credits, duration_ms
  FROM request_logs
 WHERE ($1 = '' OR status = $1)
   AND ($2 = '' OR endpoint = $2)
 ORDER BY id DESC
 LIMIT $3`, status, endpoint, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list request logs: %w", err)
	}
	defer rows.Close()
	var out []RequestLog
	for rows.Next() {
		var r RequestLog
		if err := rows.Scan(&r.Time, &r.Endpoint, &r.Model, &r.AccountID, &r.APIKeyID,
			&r.Status, &r.Error, &r.ErrorType, &r.Tokens, &r.Credits, &r.DurationMs); err != nil {
			return nil, fmt.Errorf("db: scan request log: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: request log rows: %w", err)
	}
	return out, nil
}

// CountRequestLogs returns total / success / error counts for the summary line.
func CountRequestLogs(ctx context.Context, q Querier) (total, success, errored int64, err error) {
	row := q.QueryRow(ctx, `
SELECT COUNT(*),
       COUNT(*) FILTER (WHERE status = 'success'),
       COUNT(*) FILTER (WHERE status = 'error')
  FROM request_logs`)
	if err = row.Scan(&total, &success, &errored); err != nil {
		return 0, 0, 0, fmt.Errorf("db: count request logs: %w", err)
	}
	return total, success, errored, nil
}

// ClearRequestLogs empties the audit trail (explicit operator action only).
func ClearRequestLogs(ctx context.Context, q Querier) error {
	if _, err := q.Exec(ctx, `DELETE FROM request_logs`); err != nil {
		return fmt.Errorf("db: clear request logs: %w", err)
	}
	return nil
}

// PruneRequestLogs keeps only the newest `keep` rows so the trail stays bounded
// without losing recent history. keep <= 0 is treated as a large default.
//
// Implementation: find the id of the keep-th newest row (the watermark) and
// delete everything below it — a range delete on the primary key. The previous
// `id NOT IN (SELECT ... LIMIT keep)` built a large in-list and scanned it per
// candidate row; `id < watermark` uses the pk index directly.
func PruneRequestLogs(ctx context.Context, q Querier, keep int) error {
	if keep <= 0 {
		keep = 100000
	}
	var watermark int64
	err := q.QueryRow(ctx, `
SELECT id FROM request_logs ORDER BY id DESC LIMIT 1 OFFSET $1`, keep).Scan(&watermark)
	if err != nil {
		// Fewer than `keep` rows exist (no watermark) → nothing to prune.
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("db: prune request logs (watermark): %w", err)
	}
	if _, err := q.Exec(ctx, `DELETE FROM request_logs WHERE id <= $1`, watermark); err != nil {
		return fmt.Errorf("db: prune request logs: %w", err)
	}
	return nil
}
