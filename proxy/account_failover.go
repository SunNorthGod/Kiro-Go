package proxy

import (
	"context"
	"errors"
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

// isOversizedInputErrorMessage matches the upstream's rejection of a too-large
// request body. Kiro answers HTTP 400 with either "Input is too long." or
// {"reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}.
func isOversizedInputErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "input is too long") ||
		strings.Contains(msg, "content_length_exceeds_threshold")
}

// isImageRejectionErrorMessage matches the upstream's rejection of an image we
// forwarded: dimensions beyond its 8000-pixel ceiling, or a container it cannot
// decode. Both are properties of the bytes themselves.
func isImageRejectionErrorMessage(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "image_dimension_exceeded") ||
		strings.Contains(lower, "image_format_unsupported") ||
		strings.Contains(lower, "image_mime_mismatch") ||
		strings.Contains(lower, "could not process image") ||
		strings.Contains(lower, "image dimensions exceed")
}

// isRequestShapeErrorMessage reports whether an upstream error is about the
// REQUEST WE SENT rather than the account or the endpoint serving it.
//
// These must not be retried by rotating endpoints or accounts: the same payload
// is rejected everywhere, so a failover only multiplies the cost (3 endpoints x 3
// accounts = up to 9 identical 400s per client request) while marking innocent
// accounts as failing. The self-healable subset is still healed in place by
// callKiroWithSelfHeal, which strips the offending fields and retries on the SAME
// account (see response_health.go) — that path is unaffected, and it now runs
// after one upstream round-trip instead of three.
func isRequestShapeErrorMessage(msg string) bool {
	if isOversizedInputErrorMessage(msg) {
		return true
	}
	// Image rejections are payload properties too. Measured on 2026-07-26: 12
	// IMAGE_DIMENSION_EXCEEDED requests each burned all three endpoints in turn
	// (identical error from Kiro IDE, CodeWhisperer and AmazonQ within seconds)
	// and marked the serving accounts as failing, for a request no endpoint could
	// ever accept.
	if isImageRejectionErrorMessage(msg) {
		return true
	}
	// An unknown model name is a property of the request, and every account will
	// reject it identically. This matters more since the pool stopped hard-blocking
	// models that no account advertises (pool/scheduler.go): such a request now
	// reaches the upstream and comes back "Invalid model", which is the truthful
	// answer — but without this branch it would land in the default case, cascade
	// through every account, and cool each one on the way. Measured beforehand:
	// "Invalid model" 8 times and "This model doesn't support..." 25 times in 24h,
	// all of them charged to accounts.
	if isUnknownModelErrorMessage(msg) {
		return true
	}
	lower := strings.ToLower(msg)
	// Payload-level 400s that callKiroWithSelfHeal knows how to strip: a thinking
	// signature the upstream rejects, and additionalModelRequestFields on a model
	// with no schema for it. Kept in sync with isSelfHealableKiroError.
	return strings.Contains(msg, "THINKING_SIGNATURE_INVALID") ||
		strings.Contains(lower, "signature` in `thinking") ||
		strings.Contains(lower, "signature in thinking") ||
		strings.Contains(lower, "additionalmodelrequestfields") ||
		strings.Contains(lower, "not supported for this model")
}

// isUnknownModelErrorMessage matches an upstream rejection of the MODEL NAME.
//
// Requires a 400 alongside the phrase so that a 500 whose body happens to mention
// a model is not swallowed here — a capacity 500 must stay with the overload
// branch, which retries, rather than being treated as a permanent client error.
func isUnknownModelErrorMessage(msg string) bool {
	lower := strings.ToLower(msg)
	if !strings.Contains(lower, "400") {
		return false
	}
	return strings.Contains(lower, "invalid model") ||
		strings.Contains(lower, "this model doesn't support") ||
		strings.Contains(lower, "this model does not support") ||
		strings.Contains(lower, "model is not supported") ||
		strings.Contains(lower, "unsupported model")
}

// isUpstreamOverloadErrorMessage matches an upstream 5xx that is about the
// SERVICE's capacity, not about this account or this request.
//
// Measured on 2026-07-28 over 24h of production traffic: 263 endpoint-level
// MODEL_TEMPORARILY_UNAVAILABLE responses, plus 77 client requests failing with
// "Encountered an unexpected error when processing the request, please try again."
// Both are HTTP 500 from AWS and both explicitly ask the caller to retry.
//
// Two things were wrong with how these were handled. They fell through to the
// default branch of handleAccountFailure and were reported as OutcomeTransient,
// which bumps the account's error counter — and three of those (the counter only
// resets on a SUCCESS) cool the account for a minute. With five accounts and a
// burst of upstream 500s the whole pool sat in rolling cooldowns, so the pool
// reported "no available account" and clients got HTTP 503: 208 of them in four
// hours, for accounts that were perfectly healthy. Second, nothing retried, even
// though the upstream said to: the three endpoints are different API surfaces in
// front of the SAME model backend, so failing over between them within
// milliseconds cannot help, and the request ended as "empty upstream response"
// (286 in 24h).
//
// A genuinely broken account produces 401/403/402/429, each of which has its own
// branch above. A 500 is by definition the server's problem, so the account is
// not blamed here — same reasoning already applied to request-shape errors.
func isUpstreamOverloadErrorMessage(msg string) bool {
	lower := strings.ToLower(msg)
	if !strings.Contains(lower, "http 5") && !strings.Contains(lower, "please try again") {
		return false
	}
	return strings.Contains(lower, "model_temporarily_unavailable") ||
		strings.Contains(lower, "unexpectedly high load") ||
		strings.Contains(lower, "encountered an unexpected error when processing the request")
}

// isCancellationError reports whether a failure is a cancelled request rather than
// an account or upstream fault — almost always the client hanging up mid-stream.
//
// This is the LARGEST single error class in production: 458 in 24 hours, ahead of
// every genuine upstream problem. Only the Claude streaming handler guarded for it
// (handler.go checks clientGone(ctx) before blaming the account); the other ten
// handleAccountFailure call sites — non-streaming Claude, both OpenAI paths and the
// responses API — did not, so a customer pressing Ctrl-C bumped the serving
// account's error counter. Three of those with no success in between cool the
// account for a minute (pool/scheduler.go:373), and once enough accounts are
// cooling the eligible set is empty and the next requests get HTTP 503 "No
// available accounts" — returned in 0.00s with nothing logged, which is exactly the
// fingerprint measured: 299 of 370 such 503s were instant, and every account
// reported error_count 0 because the pool's counter is in-memory only.
//
// Classifying here rather than at the call sites is deliberate: eleven guards are
// eleven chances to forget one, and the next handler added would forget it too.
//
// context.DeadlineExceeded is deliberately NOT included. A timeout is at least weak
// evidence that this path is unhealthy, so it keeps its transient outcome.
func isCancellationError(err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	lower := strings.ToLower(err.Error())
	// String forms too: the error crosses HTTP client and stream-parser layers that
	// format rather than wrap, so errors.Is alone misses it.
	return strings.Contains(lower, "context canceled") ||
		strings.Contains(lower, "context cancelled") ||
		strings.Contains(lower, "request canceled") ||
		strings.Contains(lower, "client disconnected")
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
	case isCancellationError(err):
		// The request was cancelled — the client went away, or we cancelled our own
		// context. Nothing about the account is implicated. Checked FIRST because a
		// cancellation can surface with a message that also mentions HTTP status text
		// from a partially-read response, which would otherwise land in a branch that
		// penalises the account. Deliberately NO ReportOutcome.
		logger.Warnf("[AccountFailover] request cancelled (account not blamed) for %s: %v", account.Email, err)
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
	case isRequestShapeErrorMessage(errMsg):
		// The request we sent is at fault, not the account. Reporting a transient
		// outcome here punished innocent accounts: every account tried during the
		// failover got an error-count bump, and three in a row earned a one-minute
		// cooldown — so one malformed/oversized request could cool down a third of
		// the pool. Deliberately NO ReportOutcome; just make it visible.
		logger.Warnf("[AccountFailover] request-shape upstream rejection (account not blamed) for %s: %v", account.Email, err)
	case isUpstreamOverloadErrorMessage(errMsg):
		// The upstream MODEL is busy, which says nothing about this account. Same
		// reasoning as the request-shape case above, and the same consequence when it
		// was missing: these 500s were counted against every account the failover
		// touched, three strikes cooled it for a minute, and with the pool cooled the
		// next requests were answered with HTTP 503 "No available accounts" — 208 of
		// them in four hours while every account was healthy. Deliberately NO
		// ReportOutcome. CallKiroAPI retries the endpoint cycle after a short
		// backoff, which is what the upstream asks for.
		logger.Warnf("[AccountFailover] upstream capacity error (account not blamed) for %s: %v", account.Email, err)
	default:
		h.pool.ReportOutcome(account.ID, "", pool.OutcomeTransient)
	}
}
