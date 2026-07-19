package config

import "sync"

// persist_worker.go persists per-account runtime stats off the request hot path
// on a SINGLE background goroutine, replacing the previous
// "go config.UpdateAccountStats(...)" spawned on every successful request. That
// old pattern had two defects:
//   - unbounded goroutines: a burst of successes spawned a goroutine each, any
//     of which could pile up behind cfgLock;
//   - out-of-order writes: two goroutines for the same account could persist in
//     the wrong order, so an older cumulative snapshot could overwrite a newer
//     one (stats going backwards).
//
// The worker coalesces by account id (latest snapshot wins) and applies updates
// sequentially, so writes are ordered and the goroutine count is fixed at one.
// The pending map is naturally bounded by the number of distinct accounts.
// These are display-only stats (never billing), so dropping intermediate
// snapshots via coalescing is fine.

type accountStatsSnapshot struct {
	requestCount int
	errorCount   int
	totalTokens  int
	totalCredits float64
	lastUsed     int64
}

var (
	statsWorkerOnce sync.Once
	statsMu         sync.Mutex
	pendingStats    map[string]accountStatsSnapshot
	statsSignal     chan struct{}
)

func startStatsWorker() {
	pendingStats = make(map[string]accountStatsSnapshot)
	statsSignal = make(chan struct{}, 1)
	go statsWorkerLoop()
}

// EnqueueAccountStats records the latest cumulative stats snapshot for an account
// and wakes the worker. It never blocks the caller and never spawns a goroutine
// per call. Absolute (cumulative) values mean the latest snapshot always
// supersedes older ones, so coalescing is correct.
func EnqueueAccountStats(id string, requestCount, errorCount, totalTokens int, totalCredits float64, lastUsed int64) {
	if id == "" {
		return
	}
	statsWorkerOnce.Do(startStatsWorker)
	statsMu.Lock()
	pendingStats[id] = accountStatsSnapshot{
		requestCount: requestCount,
		errorCount:   errorCount,
		totalTokens:  totalTokens,
		totalCredits: totalCredits,
		lastUsed:     lastUsed,
	}
	statsMu.Unlock()
	select {
	case statsSignal <- struct{}{}:
	default: // a wake is already pending; the worker will see the new snapshot
	}
}

func statsWorkerLoop() {
	for range statsSignal {
		for {
			statsMu.Lock()
			if len(pendingStats) == 0 {
				statsMu.Unlock()
				break
			}
			var id string
			var snap accountStatsSnapshot
			for k, v := range pendingStats {
				id, snap = k, v
				break
			}
			delete(pendingStats, id)
			statsMu.Unlock()

			// UpdateAccountStats updates the in-memory mirror under cfgLock and,
			// in DB mode, performs the persistence OUTSIDE that lock.
			_ = UpdateAccountStats(id, snap.requestCount, snap.errorCount, snap.totalTokens, snap.totalCredits, snap.lastUsed)
		}
	}
}
