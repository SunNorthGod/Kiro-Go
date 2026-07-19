package proxy

import (
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"strings"
	"sync"
	"time"
)

const maxAccountRetryAttempts = 3

// overageRefreshInflight dedupes background overage-status refreshes so repeated
// 402s for the same account spawn at most one in-flight refresh goroutine.
var overageRefreshInflight sync.Map // accountID -> struct{}

func isQuotaErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "429") || strings.Contains(msg, "quota")
}

func isOverageErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "402") && strings.Contains(msg, "overage")
}

func isSuspensionErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "temporarily_suspended") ||
		strings.Contains(msg, "temporarily is suspended") ||
		strings.Contains(msg, "account suspended")
}

func isProfileUnavailableErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "no available kiro profile")
}

func isAuthErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "http 401") ||
		strings.Contains(msg, "http 403") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "forbidden") ||
		strings.Contains(msg, "authentication failed") ||
		strings.Contains(msg, "token invalid") ||
		strings.Contains(msg, "token expired") ||
		strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "access token expired") ||
		strings.Contains(msg, "refresh token expired")
}

func (h *Handler) disableAccount(account *config.Account, banStatus, banReason string) {
	if account == nil {
		return
	}

	updatedAccount := *account
	if !updatedAccount.Enabled && updatedAccount.BanStatus == banStatus && updatedAccount.BanReason == banReason {
		return
	}

	updatedAccount.Enabled = false
	updatedAccount.BanStatus = banStatus
	updatedAccount.BanReason = banReason
	updatedAccount.BanTime = time.Now().Unix()

	if err := config.UpdateAccount(account.ID, updatedAccount); err != nil {
		logger.Warnf("[AccountFailover] Failed to disable %s: %v", account.Email, err)
		return
	}

	logger.Warnf("[AccountFailover] Disabled %s: %s", account.Email, banReason)
	h.pool.Reload()
}

// refreshOverageAsync refreshes an account's upstream overage status off the
// request path, deduped so concurrent 402s for the same account share one
// refresh. A value copy is handed to the goroutine so it never races the caller.
func (h *Handler) refreshOverageAsync(account *config.Account) {
	if account == nil {
		return
	}
	id := account.ID
	if _, inflight := overageRefreshInflight.LoadOrStore(id, struct{}{}); inflight {
		return
	}
	acc := *account
	go func() {
		defer overageRefreshInflight.Delete(id)
		h.disableAccountOverage(&acc)
	}()
}

func (h *Handler) disableAccountOverage(account *config.Account) {
	if account == nil {
		return
	}

	snap, fetchErr := FetchOverageStatus(account)
	if fetchErr != nil {
		logger.Warnf("[AccountFailover] Failed to refresh overage status for %s: %v", account.Email, fetchErr)
		return
	}
	if persistErr := PersistOverageSnapshot(account.ID, snap); persistErr != nil {
		logger.Warnf("[AccountFailover] Failed to persist overage snapshot for %s: %v", account.Email, persistErr)
		return
	}

	logger.Warnf("[AccountFailover] Refreshed overage status for %s after upstream overage limit error: %s", account.Email, snap.Status)
	h.pool.Reload()
}

func (h *Handler) handleAccountFailure(account *config.Account, err error) {
	if account == nil || err == nil {
		return
	}

	errMsg := err.Error()
	switch {
	case isOverageErrorMessage(errMsg):
		// Cool the account down immediately (fast, in-memory), then refresh the
		// authoritative upstream overage status in the BACKGROUND. Previously the
		// blocking getUsageLimits round-trip ran inline in the request's failover
		// loop, adding upstream latency to every 402.
		h.pool.ReportOutcome(account.ID, "", pool.OutcomeQuotaExhausted)
		h.refreshOverageAsync(account)
	case isSuspensionErrorMessage(errMsg):
		// Suspension is a definitive upstream ban → disable immediately.
		h.disableAccount(account, "BANNED", "AWS temporarily suspended - unusual user activity detected")
	case isQuotaErrorMessage(errMsg):
		// 429 = Kiro rate-limiting (transient), NOT monthly quota (that is handled
		// by usage tracking / isQuotaBlocked). Apply a short exponential backoff and
		// rotate — never a 1h cooldown.
		h.pool.ReportOutcome(account.ID, "", pool.OutcomeRateLimited)
	case isProfileUnavailableErrorMessage(errMsg):
		// Profile ARN may be transiently unresolvable; soft transient, never auto-disable.
		h.pool.ReportOutcome(account.ID, "", pool.OutcomeTransient)
	case isAuthErrorMessage(errMsg):
		// A single 401/403 no longer hard-bans (could be a transient blip or, before
		// the pool race fix, a torn-read Bearer). Disable only after N consecutive
		// auth failures with no success in between (see pool.ReportOutcome).
		h.pool.ReportOutcome(account.ID, "", pool.OutcomeAuthError)
	default:
		h.pool.ReportOutcome(account.ID, "", pool.OutcomeTransient)
	}
}
