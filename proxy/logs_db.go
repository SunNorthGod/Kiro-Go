package proxy

// logs_db.go persists the request audit trail to PostgreSQL when the DB backend
// is active. Writes are queued and drained by a single background goroutine so
// request handling never blocks on logging; a full queue drops the DB write (the
// in-memory ring still has the entry). Reads for the admin logs view come from the
// DB when available, giving a durable, auditable history far beyond the in-memory
// ring. In JSON mode all of this is a no-op and the in-memory ring is used.

import (
	"context"
	"sync"
	"time"

	"kiro-go/config"
	"kiro-go/db"
	"kiro-go/logger"
)

const (
	requestLogsDBKeep    = 100000 // audit retention (rows) before pruning oldest
	requestLogsPruneStep = 500    // prune every N inserts
	logQueueCap          = 2048
)

var (
	logQueue     chan db.RequestLog
	logQueueOnce sync.Once
)

func startLogWriter() {
	logQueue = make(chan db.RequestLog, logQueueCap)
	go func() {
		inserts := 0
		for e := range logQueue {
			pool := config.DatabasePool()
			if pool == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := db.AddRequestLog(ctx, pool, e); err == nil {
				inserts++
			}
			cancel()
			if inserts >= requestLogsPruneStep {
				inserts = 0
				pctx, pcancel := context.WithTimeout(context.Background(), 15*time.Second)
				_ = db.PruneRequestLogs(pctx, pool, requestLogsDBKeep)
				pcancel()
			}
		}
	}()
}

// enqueueRequestLogDB fire-and-forget queues one entry for async DB persistence.
// No-op in JSON mode. Never blocks the caller.
func enqueueRequestLogDB(e RequestLog) {
	if config.DatabasePool() == nil {
		return
	}
	logQueueOnce.Do(startLogWriter)
	select {
	case logQueue <- toDBRequestLog(e):
	default: // queue saturated → drop; in-memory ring still retains it
	}
}

func toDBRequestLog(e RequestLog) db.RequestLog {
	return db.RequestLog{
		Time: e.Time, Endpoint: e.Endpoint, Model: e.Model, AccountID: e.AccountID,
		Status: e.Status, Error: e.Error, ErrorType: e.ErrorType,
		Tokens: int64(e.Tokens), Credits: e.Credits, DurationMs: e.Duration,
	}
}

// listRequestLogsDB reads recent audit rows from PostgreSQL (newest first).
// Returns (nil,false) in JSON mode or on error so the caller falls back to memory.
func listRequestLogsDB(limit int) ([]RequestLog, bool) {
	pool := config.DatabasePool()
	if pool == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := db.ListRequestLogs(ctx, pool, limit, "", "")
	if err != nil {
		return nil, false
	}
	out := make([]RequestLog, 0, len(rows))
	for _, r := range rows {
		out = append(out, RequestLog{
			Time: r.Time, Endpoint: r.Endpoint, Model: r.Model, AccountID: r.AccountID,
			Status: r.Status, Error: r.Error, ErrorType: r.ErrorType,
			Tokens: int(r.Tokens), Credits: r.Credits, Duration: r.DurationMs,
		})
	}
	return out, true
}

// clearRequestLogsDB empties the DB audit trail (operator action). No-op in JSON mode.
func clearRequestLogsDB() {
	pool := config.DatabasePool()
	if pool == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = db.ClearRequestLogs(ctx, pool)
}

// usageRecordsRetentionDays bounds the display-only usage_records log. 90 days
// covers everything the UI can request (GetDailyUsage caps at 90; the cache
// window is 7); beyond that the rows only cost storage and scan time.
const usageRecordsRetentionDays = 90

// pruneUsageRecordsRetention trims usage_records to the retention window.
// No-op in JSON mode. Called from the periodic background refresh; billing is
// unaffected (it reads usage_counters, never this log).
func pruneUsageRecordsRetention() {
	pool := config.DatabasePool()
	if pool == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cutoff := time.Now().AddDate(0, 0, -usageRecordsRetentionDays).Unix()
	if n, err := db.PruneUsageRecordsBefore(ctx, pool, cutoff); err != nil {
		logger.Warnf("[Retention] prune usage_records failed: %v", err)
	} else if n > 0 {
		logger.Infof("[Retention] pruned %d usage_records rows older than %d days", n, usageRecordsRetentionDays)
	}
}
