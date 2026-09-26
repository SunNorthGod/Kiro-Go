package proxy

import "testing"

func TestAccountFailureClassifiers(t *testing.T) {
	tests := []struct {
		name string
		fn   func(string) bool
		msg  string
	}{
		{name: "quota", fn: isQuotaErrorMessage, msg: "HTTP 429: quota exhausted"},
		{name: "overage", fn: isOverageErrorMessage, msg: "HTTP 402 from Kiro IDE: OVERAGE limit exceeded"},
		{name: "suspension", fn: isSuspensionErrorMessage, msg: "Your User ID temporarily is suspended"},
		{name: "profile", fn: isProfileUnavailableErrorMessage, msg: "no available Kiro profile"},
		{name: "auth", fn: isAuthErrorMessage, msg: "Authentication failed - token invalid or expired"},
	}

	for _, tc := range tests {
		if !tc.fn(tc.msg) {
			t.Fatalf("%s classifier did not match %q", tc.name, tc.msg)
		}
	}
}

// TestRequestShapeErrorClassifier pins which upstream rejections are about the
// payload we sent rather than the account or endpoint serving it. These must not
// trigger endpoint fallback or account rotation: the same bytes are rejected
// everywhere, so a failover turned one client request into up to nine identical
// upstream 400s and marked three innocent accounts as failing.
func TestRequestShapeErrorClassifier(t *testing.T) {
	shape := []string{
		`HTTP 400 from Kiro IDE: {"message":"Input is too long."}`,
		`HTTP 400 from AmazonQ: {"reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD","message":"..."}`,
		`HTTP 400 from Kiro IDE: {"message":"THINKING_SIGNATURE_INVALID"}`,
		`HTTP 400: additionalModelRequestFields is not supported for this model`,
		`HTTP 400: The `+"`signature` in `thinking`"+` block is invalid`,
	}
	for _, msg := range shape {
		if !isRequestShapeErrorMessage(msg) {
			t.Fatalf("expected a request-shape classification for %q", msg)
		}
	}

	// Account- or endpoint-scoped failures must keep their existing failover.
	notShape := []string{
		`HTTP 429: quota exhausted`,
		`HTTP 401 from Kiro IDE: unauthorized`,
		`HTTP 402 from Kiro IDE: OVERAGE limit exceeded`,
		`HTTP 500 from AmazonQ: internal error`,
		`quota exhausted on Kiro IDE`,
		`context canceled`,
	}
	for _, msg := range notShape {
		if isRequestShapeErrorMessage(msg) {
			t.Fatalf("%q must stay eligible for endpoint/account failover", msg)
		}
	}
}

// TestOversizedInputClassifier covers the compaction-specific rejection on its
// own, since it is the one the truncation convergence is meant to prevent.
func TestOversizedInputClassifier(t *testing.T) {
	if !isOversizedInputErrorMessage(`{"message":"Input is too long."}`) {
		t.Fatal("expected the oversized-input classifier to match the plain message form")
	}
	if !isOversizedInputErrorMessage(`{"reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`) {
		t.Fatal("expected the oversized-input classifier to match the reason-code form")
	}
	if isOversizedInputErrorMessage(`{"message":"THINKING_SIGNATURE_INVALID"}`) {
		t.Fatal("signature rejections are not an oversized-input error")
	}
}

// TestRequestShapeErrorsCoverSelfHealableSet guards the duplication between
// isRequestShapeErrorMessage and isSelfHealableKiroError: every error the
// self-heal path knows how to strip must also be treated as request-shape, or it
// would still burn a full endpoint + account failover before self-healing.
func TestRequestShapeErrorsCoverSelfHealableSet(t *testing.T) {
	selfHealable := []string{
		"THINKING_SIGNATURE_INVALID",
		"additionalModelRequestFields is not supported",
		"not supported for this model",
	}
	for _, msg := range selfHealable {
		if !isSelfHealableKiroError(errString(msg)) {
			t.Fatalf("test premise broken: %q is not self-healable", msg)
		}
		if !isRequestShapeErrorMessage(msg) {
			t.Fatalf("self-healable error %q must also classify as request-shape", msg)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// TestImageRejectionIsRequestShape pins the 2026-07-26 finding: image rejections
// are a property of the bytes we forwarded, so no endpoint or account can ever
// accept them. Measured before the fix: 12 IMAGE_DIMENSION_EXCEEDED requests each
// burned Kiro IDE -> CodeWhisperer -> AmazonQ in turn and marked the serving
// accounts as failing.
func TestImageRejectionIsRequestShape(t *testing.T) {
	rejections := []string{
		`HTTP 400 from AmazonQ: {"message":"messages.2.content.1.image.source.base64.data: At least one of the image dimensions exceed max allowed size: 8000 pixels","reason":"IMAGE_DIMENSION_EXCEEDED"}`,
		`HTTP 400 from AmazonQ: {"message":"Could not process image","reason":"IMAGE_FORMAT_UNSUPPORTED"}`,
		`HTTP 400 from Kiro IDE: {"reason":"IMAGE_MIME_MISMATCH"}`,
	}
	for _, msg := range rejections {
		if !isImageRejectionErrorMessage(msg) {
			t.Fatalf("expected an image-rejection classification for %q", msg)
		}
		if !isRequestShapeErrorMessage(msg) {
			t.Fatalf("image rejection must also be request-shape (no endpoint/account retry): %q", msg)
		}
	}

	// Must not swallow unrelated failures that DO deserve a failover.
	for _, msg := range []string{
		`HTTP 429: quota exhausted`,
		`HTTP 500 from AmazonQ: internal error`,
		`HTTP 400 from AmazonQ: {"message":"Input is too long."}`, // oversized, not image
		`context canceled`,
	} {
		if isImageRejectionErrorMessage(msg) {
			t.Fatalf("%q must not be classified as an image rejection", msg)
		}
	}
}
