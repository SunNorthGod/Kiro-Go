package proxy

import (
	"errors"
	"testing"
)

// The 2026-11-05 incident: 2072 upstream 429s in one morning on a single-account
// pool. Every 429 was treated as instant endpoint rotation with no wait, so
// millisecond-apart attempts on three endpoints of the SAME account all landed
// in the same rate window. The split below separates RATE throttling (retryable
// after a real wait) from USAGE-quota exhaustion (retrying until reset is waste).

func TestUsageQuota429BodyClassifier(t *testing.T) {
	// Usage wording (allowance problems) must classify as usage even without
	// throttling vocabulary, and must not be shadowed by mixed bodies.
	usage := []string{
		`{"message":"Monthly limit reached for this account"}`,
		`quota exhausted: no credits remaining`,
		`usage limit exceeded for this subscription`,
		`plan limit reached`,
		`You have used all of your allowance for this period`,
	}
	for _, body := range usage {
		if !isUsageQuota429Body(body) {
			t.Errorf("usage-quota body not recognized: %q", body)
		}
	}

	// Rate vocabulary wins over anything else: a rate 429 is retryable, and a
	// mistaken usage classification would freeze a healthy account until reset.
	rate := []string{
		`{"__type":"ThrottlingException","message":"rate exceeded"}`,
		`Too many requests, please wait before trying again.`,
		`Slow down and try again`,
		`{"message":"quota exhausted on Kiro IDE"}`, // rate-quota wording, no usage words
		``,                                           // empty body: default to the retryable interpretation
		`{"message":"quota exhausted on AmazonQ"}`,
	}
	for _, body := range rate {
		if isUsageQuota429Body(body) {
			t.Errorf("rate-throttle body misclassified as usage quota: %q", body)
		}
	}
}

func TestUsageQuotaErrorMessageSplit(t *testing.T) {
	usageErr := `HTTP 429 from Kiro IDE: usage quota exhausted: {"message":"Monthly limit reached"}`
	rateErr := `HTTP 429 rate throttled on Kiro IDE`

	if !isUsageQuotaErrorMessage(usageErr) {
		t.Error("usage-quota error message not recognized")
	}
	if isUsageQuotaErrorMessage(rateErr) {
		t.Error("rate-throttle error message misclassified as usage quota")
	}
	// The generic quota classifier still catches BOTH (its short-backoff branch
	// is the fallback for rate failures); the usage branch merely runs first.
	if !isQuotaErrorMessage(usageErr) || !isQuotaErrorMessage(rateErr) {
		t.Error("generic quota classifier lost coverage of a 429 variant")
	}
}

func TestRateThrottleFailureTextForFailoverLoop(t *testing.T) {
	if !isRateThrottleFailureText(errors.New(`HTTP 429 rate throttled on Kiro IDE`)) {
		t.Error("rate-throttle failure text not recognized")
	}
	// Waiting cannot heal a usage exhaustion, so it must not count as throttle.
	if isRateThrottleFailureText(errors.New(`HTTP 429 from Kiro IDE: usage quota exhausted: {"message":"Monthly limit reached"}`)) {
		t.Error("usage exhaustion misclassified as waitable rate throttling")
	}
	if isRateThrottleFailureText(errors.New(`HTTP 401 from Kiro IDE: token expired`)) {
		t.Error("auth failure misclassified as rate throttling")
	}
	if isRateThrottleFailureText(nil) {
		t.Error("nil error must not classify as throttling")
	}
}

// Branch-order guard, same spirit as TestModelRestrictedDoesNotOverlapEarlierClassifiers:
// the usage-quota branch sits BEFORE the generic quota branch in
// handleAccountFailure, so a usage message reaching only the generic branch
// would silently get the seconds-long rate cooldown instead of the quota one.
func TestUsageQuotaMessageReachesUsageClassifierBeforeQuotaFallback(t *testing.T) {
	msg := `HTTP 429 from Kiro IDE: usage quota exhausted: {"message":"Monthly limit reached"}`
	if !isUsageQuotaErrorMessage(msg) {
		t.Fatal("usage message must hit its own branch")
	}
	// No earlier branch may steal it.
	if isCancellationError(errors.New(msg)) ||
		isOverageErrorMessage(msg) ||
		isSuspensionErrorMessage(msg) ||
		isProfileUnavailableErrorMessage(msg) ||
		isAuthErrorMessage(msg) ||
		isRequestShapeErrorMessage(msg) ||
		isUpstreamOverloadErrorMessage(msg) ||
		isModelRestrictedFailureMessage(msg) {
		t.Error("an earlier handleAccountFailure branch also matches the usage-quota message")
	}
}
