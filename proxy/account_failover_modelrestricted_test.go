package proxy

import (
	"errors"
	"testing"
)

// Model-scoped restrictions must reach their own branch in handleAccountFailure,
// not the transient one that freezes the account. Production evidence 2026-09-28:
// an opus-5.5 empty-stream restriction drove both healthy accounts into rolling
// 1-minute cooldowns (whole-pool 503s, cooling=2) while opus-4.8 on the same
// accounts kept serving.

func TestModelRestrictedClassifierMatchesProductionSignatures(t *testing.T) {
	mustMatch := []string{
		errEmptyKiroStream.Error(),
		"upstream stream ended before any output",
		"Kiro IDE stream died before any output",
		`Bedrock error message: temporarily restricted due to excessive content refusals`,
		"temporarily restricted",
	}
	for _, msg := range mustMatch {
		if !isModelRestrictedFailureMessage(msg) {
			t.Errorf("model-restricted signature not recognized: %q", msg)
		}
	}

	// Strings that belong to other branches — a false positive here would exempt a
	// genuinely account-scoped failure from every counter.
	mustNotMatch := []string{
		`HTTP 401 from Kiro IDE: unauthorized`,
		`HTTP 429 from Kiro IDE: quota exhausted`,
		`HTTP 402 from AmazonQ: overage limit reached`,
		`HTTP 500 from AmazonQ: {"message":"Encountered unexpectedly high load when processing the request, please try again.","reason":"MODEL_TEMPORARILY_UNAVAILABLE"}`,
		`HTTP 400 from Kiro IDE: {"message":"Input is too long.","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`,
		"context canceled",
		"no available kiro profile",
		"",
	}
	for _, msg := range mustNotMatch {
		if isModelRestrictedFailureMessage(msg) {
			t.Errorf("message from another branch classified as model-restricted: %q", msg)
		}
	}
}

// The model-restricted message must not trip any classifier whose branch precedes
// it in handleAccountFailure's switch — orderings are load-bearing here.
func TestModelRestrictedDoesNotOverlapEarlierClassifiers(t *testing.T) {
	empty := errEmptyKiroStream.Error()
	if isCancellationError(errors.New(empty)) {
		t.Error("cancellation classifier also matches the empty-stream message")
	}
	if isOverageErrorMessage(empty) {
		t.Error("overage classifier also matches the empty-stream message")
	}
	if isQuotaErrorMessage(empty) {
		t.Error("quota classifier also matches the empty-stream message")
	}
	if isAuthErrorMessage(empty) {
		t.Error("auth classifier also matches the empty-stream message")
	}
	if isRequestShapeErrorMessage(empty) {
		t.Error("request-shape classifier also matches the empty-stream message")
	}
	if isUpstreamOverloadErrorMessage(empty) {
		t.Error("overload classifier also matches the empty-stream message")
	}
	if isProfileUnavailableErrorMessage(empty) {
		t.Error("profile-unavailable classifier also matches the empty-stream message")
	}
}
