package proxy

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// The bug these cover: an upstream HTTP 500 about the MODEL's capacity was counted
// against whichever account happened to serve the request. OutcomeTransient bumps a
// per-account error counter that only resets on a success, and three bumps cool the
// account for a minute — so a burst of upstream 500s cooled the whole pool and the
// next clients were told "No available accounts" (HTTP 503, 208 times in four hours)
// while every account was healthy.
//
// The strings below are verbatim from production logs on 2026-07-28.

func TestUpstreamOverloadIsRecognised(t *testing.T) {
	overloaded := []string{
		`HTTP 500 from AmazonQ: {"message":"Encountered unexpectedly high load when processing the request, please try again.","reason":"MODEL_TEMPORARILY_UNAVAILABLE"}`,
		`HTTP 500 from Kiro IDE: {"message":"Encountered an unexpected error when processing the request, please try again."}`,
		`HTTP 500 from CodeWhisperer: {"message":"Encountered an unexpected error when processing the request, please try again."}`,
		`error: Encountered an unexpected error when processing the request, please try again.`,
	}
	for _, msg := range overloaded {
		if !isUpstreamOverloadErrorMessage(msg) {
			t.Errorf("expected an upstream capacity error, got false for %q", msg)
		}
	}
}

func TestUpstreamOverloadDoesNotSwallowAccountProblems(t *testing.T) {
	// Every one of these says something about the ACCOUNT or the REQUEST and must keep
	// reaching its own branch in handleAccountFailure. If any of them started matching
	// the overload classifier, a genuinely broken account would never be cooled or
	// disabled — the opposite failure, and a worse one.
	notOverload := []string{
		`HTTP 401 from Kiro IDE: token expired`,
		`HTTP 403 from Kiro IDE: {"message":"Your User ID (722571076411) temporarily is suspended"}`,
		`HTTP 402 from AmazonQ: overage limit reached`,
		`HTTP 429 from Kiro IDE: quota exhausted`,
		`HTTP 400 from Kiro IDE: {"message":"Input is too long.","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`,
		`HTTP 400 from Kiro IDE: {"reason":"IMAGE_DIMENSION_EXCEEDED"}`,
		`HTTP 400 from AmazonQ: {"message":"Invalid model. Please select a different model"}`,
		`context canceled`,
		`empty upstream response`,
		`no available kiro profile`,
	}
	for _, msg := range notOverload {
		if isUpstreamOverloadErrorMessage(msg) {
			t.Errorf("must NOT be classified as upstream capacity: %q", msg)
		}
	}
}

func TestUpstreamOverloadNeedsBothAFiveHundredAndARetryHint(t *testing.T) {
	// The classifier requires the message to look like a 5xx or to carry the
	// upstream's own "please try again", so a stray mention of high load in some other
	// context cannot silently exempt an account from being blamed.
	if isUpstreamOverloadErrorMessage(`HTTP 400 from Kiro IDE: model_temporarily_unavailable is not a valid field`) {
		t.Error("a 400 mentioning the reason code must not count as a capacity error")
	}
	if !isUpstreamOverloadErrorMessage(`HTTP 503 from AmazonQ: MODEL_TEMPORARILY_UNAVAILABLE`) {
		t.Error("a 5xx carrying the reason code should count")
	}
}

// Guard the ordering inside handleAccountFailure's switch: the overload case must not
// shadow, or be shadowed by, the classifiers that came before it.
func TestOverloadAndOtherClassifiersDoNotOverlap(t *testing.T) {
	overload := `HTTP 500 from AmazonQ: {"message":"Encountered unexpectedly high load when processing the request, please try again.","reason":"MODEL_TEMPORARILY_UNAVAILABLE"}`
	for name, matched := range map[string]bool{
		"quota":              isQuotaErrorMessage(overload),
		"overage":            isOverageErrorMessage(overload),
		"suspension":         isSuspensionErrorMessage(overload),
		"auth":               isAuthErrorMessage(overload),
		"requestShape":       isRequestShapeErrorMessage(overload),
		"profileUnavailable": isProfileUnavailableErrorMessage(overload),
	} {
		if matched {
			t.Errorf("%s classifier also matches the overload message; the switch would take the wrong branch", name)
		}
	}
}

// ---- cancelled requests must never be charged to an account ----

// TestCancellationIsNotAnAccountFault pins the classifier for the largest error
// class in production (458 in 24h): a cancelled request. Only the Claude streaming
// handler guarded for it; the other ten handleAccountFailure call sites blamed the
// serving account, and three strikes cool it for a minute — which is how a pool of
// healthy accounts ends up empty and answering HTTP 503.
func TestCancellationIsNotAnAccountFault(t *testing.T) {
	cancelled := []error{
		context.Canceled,
		fmt.Errorf("post kiro: %w", context.Canceled),
		errors.New("context canceled"),
		errors.New("Post \"https://codewhisperer/generateAssistantResponse\": context canceled"),
		errors.New("stream read: context cancelled"),
		errors.New("Get \"https://x\": request canceled while waiting for connection"),
		errors.New("client disconnected before the first event"),
	}
	for _, err := range cancelled {
		if !isCancellationError(err) {
			t.Fatalf("cancellation not recognized, account would be penalised: %v", err)
		}
	}
}

// Reverse assertion: real faults must still be classified as faults, or this fix
// would hide genuine account and upstream problems.
func TestCancellationClassifierIgnoresRealFaults(t *testing.T) {
	real := []error{
		errors.New("HTTP 401 from Kiro IDE: unauthorized"),
		errors.New("HTTP 403 from Kiro IDE: {\"message\":\"temporarily_suspended\"}"),
		errors.New("HTTP 429 from Kiro IDE: too many requests"),
		errors.New("HTTP 402 from Kiro IDE: overage limit reached"),
		errors.New("HTTP 500 from Kiro IDE: {\"reason\":\"MODEL_TEMPORARILY_UNAVAILABLE\"}"),
		errors.New("HTTP 400 from Kiro IDE: {\"message\":\"Input is too long.\"}"),
		errors.New("empty upstream response"),
		errors.New("http2: server sent GOAWAY and closed the connection"),
		errors.New("stream error: stream ID 1267; INTERNAL_ERROR; received from peer"),
		context.DeadlineExceeded,
		fmt.Errorf("upstream: %w", context.DeadlineExceeded),
	}
	for _, err := range real {
		if isCancellationError(err) {
			t.Fatalf("real fault misclassified as a cancellation, it would stop being tracked: %v", err)
		}
	}
}

// A deadline is not a cancellation: it keeps its transient outcome, because a
// timeout is at least weak evidence that the path is unhealthy.
func TestDeadlineExceededIsStillTreatedAsAFault(t *testing.T) {
	if isCancellationError(context.DeadlineExceeded) {
		t.Fatal("context.DeadlineExceeded must not be excused as a cancellation")
	}
	if isUpstreamOverloadErrorMessage(context.DeadlineExceeded.Error()) {
		t.Fatal("context.DeadlineExceeded must not be classified as upstream overload either")
	}
}

// ---- an unknown model name is a request fault, not an account fault ----

// TestUnknownModelIsARequestShapeError guards the consequence of letting the pool
// route models it does not advertise: the upstream now answers "Invalid model", and
// that must terminate the request instead of being retried on every account and
// cooling each one. Strings are verbatim from production.
func TestUnknownModelIsARequestShapeError(t *testing.T) {
	unknown := []string{
		`HTTP 400 from AmazonQ: {"message":"Invalid model. Please select a different model."}`,
		`HTTP 400 from Kiro IDE: {"message":"Bedrock error message: This model doesn't support the requested feature"}`,
		`HTTP 400 from Kiro IDE: {"message":"unsupported model: simple-task"}`,
	}
	for _, msg := range unknown {
		if !isUnknownModelErrorMessage(msg) {
			t.Fatalf("unknown-model rejection not recognized: %s", msg)
		}
		if !isRequestShapeErrorMessage(msg) {
			t.Fatalf("unknown-model rejection would cascade across accounts: %s", msg)
		}
	}
}

// A 5xx that merely mentions a model must stay with the overload branch, which
// retries, rather than being written off as a permanent client error.
func TestUnknownModelClassifierRequiresA400(t *testing.T) {
	capacity := `HTTP 500 from Kiro IDE: {"message":"Encountered unexpectedly high load when processing the request, please try again.","reason":"MODEL_TEMPORARILY_UNAVAILABLE"}`
	if isUnknownModelErrorMessage(capacity) {
		t.Fatal("a capacity 500 was classified as an unknown model; it would stop being retried")
	}
	if !isUpstreamOverloadErrorMessage(capacity) {
		t.Fatal("the capacity 500 lost its overload classification")
	}

	for _, msg := range []string{
		`HTTP 401 from Kiro IDE: unauthorized`,
		`HTTP 403 from Kiro IDE: {"message":"temporarily_suspended"}`,
		`empty upstream response`,
	} {
		if isUnknownModelErrorMessage(msg) {
			t.Fatalf("misclassified as an unknown model: %s", msg)
		}
	}
}
