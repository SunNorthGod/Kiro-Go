package proxy

import (
	"strings"
	"testing"
)

// Regressions for the hole in the truncation convergence pipeline: it only ever
// shrank TEXT, so images and structured tool results were untouchable. Production
// logged four payloads that came out of "full convergence" at 2,942,373 /
// 5,512,495 / 5,579,454 / 6,126,631 bytes against a 921,600 limit — every one a
// guaranteed upstream 400 "Input is too long." — and Caddy recorded request bodies
// up to 46.26 MiB. `currentLen=1` in those log lines is the second half of the
// bug: the text stages derive their budget from the payload with the bodies
// emptied, so multi-megabyte attachments drove the budget to zero and ground the
// client's instruction down to a single byte while the attachments stayed put.

// fakeImage builds an image whose base64 payload is n bytes of the base64
// alphabet — no JSON escape expansion, so the serialized weight is n plus ~40
// bytes of scaffolding.
func fakeImage(n int) KiroImage {
	img := KiroImage{Format: "png"}
	img.Source.Bytes = strings.Repeat("A", n)
	return img
}

func userTurn(content string, images ...KiroImage) KiroHistoryMessage {
	return KiroHistoryMessage{UserInputMessage: &KiroUserInputMessage{
		Content: content,
		ModelID: "claude-opus-4.8",
		Origin:  "AI_EDITOR",
		Images:  images,
	}}
}

func assistantTurn(content string) KiroHistoryMessage {
	return KiroHistoryMessage{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: content}}
}

// newTestPayload assembles a payload the way ClaudeToKiro does, without going
// through request parsing.
func newTestPayload(current string, history ...KiroHistoryMessage) *KiroPayload {
	payload := &KiroPayload{}
	payload.ConversationState.ChatTriggerType = "MANUAL"
	payload.ConversationState.ConversationID = "conv-test"
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: current,
		ModelID: "claude-opus-4.8",
		Origin:  "AI_EDITOR",
	}
	payload.ConversationState.History = history
	return payload
}

// ---- stage 1: history images are the cheapest bytes in the payload ----

// TestHistoryImageIsReclaimedBeforeConversationTextIsDestroyed is the ordering
// regression. A stale screenshot is worth less than every word of the
// conversation, but the old pipeline could not drop it, so it collapsed all four
// retained turns to the placeholder to make room for bytes it was never going to
// send anyway — and still went upstream oversized.
func TestHistoryImageIsReclaimedBeforeConversationTextIsDestroyed(t *testing.T) {
	olderText := strings.Repeat("the migration ran in 153 seconds. ", 1200) // ~40 KB
	recentText := strings.Repeat("account failover reported transient. ", 1100)
	instruction := "Based on the earlier diagram, write the migration plan."

	payload := newTestPayload(instruction,
		userTurn("please look at this diagram", fakeImage(3*1024*1024)),
		assistantTurn("I see a diagram of the deployment."),
		userTurn(olderText),
		assistantTurn(recentText),
	)

	before := payloadByteSize(payload)
	if before <= maxPayloadBytes {
		t.Fatalf("test payload is not oversized: %d", before)
	}

	truncatePayloadToLimit(payload, false)

	if size := payloadByteSize(payload); size > maxPayloadBytes {
		t.Fatalf("payload %d still exceeds limit %d after convergence (history image never reclaimed)", size, maxPayloadBytes)
	}

	history := payload.ConversationState.History
	if len(history) != 4 {
		t.Fatalf("history turns = %d, want 4 (no turn should need dropping)", len(history))
	}
	if imgs := history[0].UserInputMessage.Images; len(imgs) != 0 {
		t.Fatalf("history image survived: %d image(s) still attached", len(imgs))
	}
	if !strings.Contains(history[0].UserInputMessage.Content, "were removed to fit") {
		t.Fatalf("image elision not noted, history[0] content = %q", history[0].UserInputMessage.Content)
	}

	// The point of the ordering: reclaiming the image frees enough that the text
	// stages never run, so the conversation survives verbatim.
	if got := history[2].UserInputMessage.Content; got != olderText {
		t.Fatalf("conversation text was destroyed to make room for a dropped image: got %d bytes, want %d", len(got), len(olderText))
	}
	if got := history[3].AssistantResponseMessage.Content; got != recentText {
		t.Fatalf("assistant text was destroyed to make room for a dropped image: got %d bytes, want %d", len(got), len(recentText))
	}
	if got := payload.ConversationState.CurrentMessage.UserInputMessage.Content; got != instruction {
		t.Fatalf("instruction altered: %q", got)
	}
}

// ---- stage 2: tool results shrink, they never disappear ----

// TestCurrentToolResultsAreShrunkNotDropped covers the active tool turn: the
// current message answers the last assistant turn's toolUses with a
// multi-megabyte result. The body has to shrink, but the KiroToolResult entry
// must stay — its toolUseId is what answers the assistant turn, and an
// unanswered tool use is its own upstream rejection.
func TestCurrentToolResultsAreShrunkNotDropped(t *testing.T) {
	huge := strings.Repeat("grep hit at proxy/handler.go:1690 -> ErrNoAccount\n", 110000) // ~5.5 MB

	payload := newTestPayload(minimalFallbackUserContent,
		userTurn("find every 503 source"),
		KiroHistoryMessage{AssistantResponseMessage: &KiroAssistantResponseMessage{
			Content:  "searching",
			ToolUses: []KiroToolUse{{ToolUseID: "t1", Name: "grepSearch", Input: map[string]interface{}{"q": "503"}}},
		}},
	)
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{
		ToolResults: []KiroToolResult{{
			ToolUseID: "t1",
			Content:   []KiroResultContent{{Text: huge}},
			Status:    "success",
		}},
	}

	truncatePayloadToLimit(payload, false)

	if size := payloadByteSize(payload); size > maxPayloadBytes {
		t.Fatalf("payload %d still exceeds limit %d (tool result never shrunk)", size, maxPayloadBytes)
	}

	ctx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if ctx == nil || len(ctx.ToolResults) != 1 {
		t.Fatal("the tool result entry was dropped; its toolUseId no longer answers the assistant turn")
	}
	result := ctx.ToolResults[0]
	if result.ToolUseID != "t1" {
		t.Fatalf("toolUseId rewritten: %q", result.ToolUseID)
	}
	if len(result.Content) != 1 {
		t.Fatalf("tool result content blocks = %d, want 1", len(result.Content))
	}
	body := result.Content[0].Text
	if strings.TrimSpace(body) == "" {
		t.Fatal("tool result body emptied; the model will report the tool as having returned nothing")
	}
	if len(body) < toolResultMinPreservedBytes {
		t.Fatalf("tool result body %d bytes, below the %d floor", len(body), toolResultMinPreservedBytes)
	}
	if !strings.Contains(body, toolResultTruncationPlaceholder) {
		t.Fatal("tool output elision not marked inside the tool result body")
	}
	// The conversation-history wording must not leak into a command's output.
	if strings.Contains(body, truncationPlaceholder) {
		t.Fatal("tool result annotated with the conversation-history placeholder")
	}
}

// ---- stage 3: the user's own attachment is the last thing to go ----

// TestCurrentImagesSurviveWhenTextIsTheProblem pins the guard on the last-resort
// stage: an oversized conversation carrying one small screenshot must shrink the
// text and keep the screenshot. Without the floor test this stage would discard
// what the user just attached on every oversized request.
func TestCurrentImagesSurviveWhenTextIsTheProblem(t *testing.T) {
	bulk := strings.Repeat("z", 400*1024)
	instruction := "compare this screenshot with the log above"

	payload := newTestPayload(instruction,
		userTurn(bulk),
		assistantTurn(bulk),
		userTurn(bulk),
		assistantTurn(bulk),
	)
	payload.ConversationState.CurrentMessage.UserInputMessage.Images = []KiroImage{fakeImage(100 * 1024)}

	truncatePayloadToLimit(payload, false)

	if size := payloadByteSize(payload); size > maxPayloadBytes {
		t.Fatalf("payload %d still exceeds limit %d", size, maxPayloadBytes)
	}
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if len(cur.Images) != 1 {
		t.Fatalf("the user's attachment was discarded even though shrinking text was enough (images left: %d)", len(cur.Images))
	}
	if strings.Contains(cur.Content, "were removed to fit") {
		t.Fatalf("spurious image-removal note: %q", cur.Content)
	}
}

// TestHistoryImagesSurviveWhenTextIsTheProblem is the regression for the first
// version of this fix, which dropped history images whenever the payload was
// oversized, before giving the text stages a chance. On live traffic that fired
// about three times a minute — far more requests than were actually failing — so
// screenshots were being discarded to save kilobytes that trimming text covered
// anyway. Images are unrecoverable once dropped, so the bar has to be "text cannot
// do it", not "the payload is over".
func TestHistoryImagesSurviveWhenTextIsTheProblem(t *testing.T) {
	bulk := strings.Repeat("w", 400*1024)

	payload := newTestPayload("summarize the discussion",
		userTurn("here is the diagram", fakeImage(150*1024)),
		assistantTurn(bulk),
		userTurn(bulk),
		assistantTurn(bulk),
	)

	if size := payloadByteSize(payload); size <= maxPayloadBytes {
		t.Fatalf("fixture is not oversized (%d); the test proves nothing", size)
	}

	truncatePayloadToLimit(payload, false)

	if size := payloadByteSize(payload); size > maxPayloadBytes {
		t.Fatalf("payload %d still exceeds limit %d", size, maxPayloadBytes)
	}
	imgs := payload.ConversationState.History[0].UserInputMessage.Images
	if len(imgs) != 1 {
		t.Fatalf("history image discarded even though shrinking text was enough (images left: %d)", len(imgs))
	}
	if c := payload.ConversationState.History[0].UserInputMessage.Content; strings.Contains(c, "were removed to fit") {
		t.Fatalf("spurious image-removal note: %q", c)
	}
}

// TestCurrentImagesDroppedWhenUnavoidable is the other side: the attachments
// themselves are over budget, so no amount of text shrinking can save the
// request. Dropping them with a note beats a guaranteed 400.
func TestCurrentImagesDroppedWhenUnavoidable(t *testing.T) {
	instruction := "review these three screenshots"
	payload := newTestPayload(instruction,
		userTurn("earlier question"),
		assistantTurn("earlier answer"),
	)
	payload.ConversationState.CurrentMessage.UserInputMessage.Images = []KiroImage{
		fakeImage(2 * 1024 * 1024), fakeImage(2 * 1024 * 1024), fakeImage(2 * 1024 * 1024),
	}

	truncatePayloadToLimit(payload, false)

	if size := payloadByteSize(payload); size > maxPayloadBytes {
		t.Fatalf("payload %d still exceeds limit %d (current images never reclaimed)", size, maxPayloadBytes)
	}
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if len(cur.Images) != 0 {
		t.Fatalf("images left: %d, want 0", len(cur.Images))
	}
	if !strings.Contains(cur.Content, instruction) {
		t.Fatalf("instruction lost: %q", cur.Content)
	}
	if !strings.Contains(cur.Content, "3 attached image(s) were removed") {
		t.Fatalf("image elision not noted for the model: %q", cur.Content)
	}
}

// ---- the production log lines, reproduced ----

// TestProductionResidueSizesConverge rebuilds the four payload sizes that appeared
// in the "[Truncate] payload still oversized after full convergence" warnings and
// requires each one to end up inside the limit.
func TestProductionResidueSizesConverge(t *testing.T) {
	for _, total := range []int{2942373, 5512495, 5579454, 6126631} {
		t.Run(map[bool]string{true: "large", false: "small"}[total > 4<<20]+"-"+itoa(total), func(t *testing.T) {
			// Distribute the mass across all three untouchable shapes at once:
			// a stale history image, an active tool result, and fresh attachments.
			share := total / 3
			payload := newTestPayload("summarize everything above",
				userTurn("here is the architecture diagram", fakeImage(share)),
				assistantTurn("noted"),
				KiroHistoryMessage{AssistantResponseMessage: &KiroAssistantResponseMessage{
					Content:  "running the tool",
					ToolUses: []KiroToolUse{{ToolUseID: "t1", Name: "readFile"}},
				}},
			)
			payload.ConversationState.CurrentMessage.UserInputMessage.Images = []KiroImage{fakeImage(share / 2), fakeImage(share / 2)}
			payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{
				ToolResults: []KiroToolResult{{
					ToolUseID: "t1",
					Content:   []KiroResultContent{{Text: strings.Repeat("k", share)}},
					Status:    "success",
				}},
			}

			before := payloadByteSize(payload)
			if before < total*3/4 {
				t.Fatalf("test payload only %d bytes, meant to approximate %d", before, total)
			}

			truncatePayloadToLimit(payload, false)

			if size := payloadByteSize(payload); size > maxPayloadBytes {
				t.Fatalf("payload %d still exceeds limit %d (was %d) — this is the production 400", size, maxPayloadBytes, before)
			}
			// The tool result must still answer the tool use.
			ctx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
			if ctx == nil || len(ctx.ToolResults) != 1 || ctx.ToolResults[0].ToolUseID != "t1" {
				t.Fatal("the active tool result was dropped instead of shrunk")
			}
		})
	}
}

// ---- the floor predicate the last-resort stage relies on ----

// TestShrinkableFloorSizePredictsTextStageFailure pins the contract: when the
// floor is over budget, running the text stages really cannot bring the payload
// under the limit — which is what justifies discarding the user's attachment.
func TestShrinkableFloorSizePredictsTextStageFailure(t *testing.T) {
	build := func(imageBytes, textBytes int) *KiroPayload {
		payload := newTestPayload(strings.Repeat("i", 4096),
			userTurn(strings.Repeat("h", textBytes)),
			assistantTurn(strings.Repeat("a", textBytes)),
		)
		payload.ConversationState.CurrentMessage.UserInputMessage.Images = []KiroImage{fakeImage(imageBytes)}
		return payload
	}

	// Attachment-dominated: floor over budget, and the text stages provably fail.
	heavy := build(5*1024*1024, 64*1024)
	if floor := shrinkableFloorSize(heavy); floor <= payloadBudget() {
		t.Fatalf("floor %d should exceed budget %d for an attachment-dominated payload", floor, payloadBudget())
	}
	truncateCurrentMessage(heavy)
	shrinkHistoryEntries(heavy)
	if size := payloadByteSize(heavy); size <= maxPayloadBytes {
		t.Fatalf("text stages unexpectedly fit the payload (%d): the floor predicate is wrong", size)
	}

	// Text-dominated: floor inside budget, and the text stages really do suffice.
	light := build(64*1024, 700*1024)
	if floor := shrinkableFloorSize(light); floor > payloadBudget() {
		t.Fatalf("floor %d should fit budget %d for a text-dominated payload", floor, payloadBudget())
	}
	truncateCurrentMessage(light)
	shrinkHistoryEntries(light)
	if size := payloadByteSize(light); size > maxPayloadBytes {
		t.Fatalf("text stages left %d bytes, above the limit %d: the floor predicate is wrong", size, maxPayloadBytes)
	}
}

// TestShrinkableFloorSizeIsALowerBound checks the arithmetic never claims savings
// the shrink stages will not actually make — an over-optimistic floor would keep
// attachments that have to go and ship a 400.
func TestShrinkableFloorSizeIsALowerBound(t *testing.T) {
	payload := newTestPayload(strings.Repeat("c", 600*1024),
		userTurn(strings.Repeat("u", 600*1024)),
		assistantTurn(strings.Repeat("a", 600*1024)),
	)
	floor := shrinkableFloorSize(payload)

	truncateCurrentMessage(payload)
	shrinkHistoryEntries(payload)

	if size := payloadByteSize(payload); size < floor {
		t.Fatalf("payload shrank to %d, below the claimed floor %d: the floor over-states what the stages preserve", size, floor)
	}
}

// ---- reverse assertions: nothing is touched without cause ----

// TestFittingPayloadKeepsItsAttachments guards against an over-eager reclaim path
// stripping images or tool results from requests that were never oversized.
func TestFittingPayloadKeepsItsAttachments(t *testing.T) {
	body := strings.Repeat("s", 4096)
	payload := newTestPayload("what is in this picture",
		userTurn(body),
		assistantTurn(body),
	)
	payload.ConversationState.CurrentMessage.UserInputMessage.Images = []KiroImage{fakeImage(10 * 1024)}
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{
		ToolResults: []KiroToolResult{{
			ToolUseID: "t1",
			Content:   []KiroResultContent{{Text: body}},
			Status:    "success",
		}},
	}

	if size := payloadByteSize(payload); size > maxPayloadBytes {
		t.Fatalf("fixture is oversized (%d); the test proves nothing", size)
	}

	truncatePayloadToLimit(payload, false)

	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if len(cur.Images) != 1 {
		t.Fatalf("image dropped from a payload that already fit (images left: %d)", len(cur.Images))
	}
	if cur.Content != "what is in this picture" {
		t.Fatalf("current content altered: %q", cur.Content)
	}
	if got := cur.UserInputMessageContext.ToolResults[0].Content[0].Text; got != body {
		t.Fatalf("tool result shrunk in a payload that already fit: %d bytes, want %d", len(got), len(body))
	}
	if got := payload.ConversationState.History[0].UserInputMessage.Content; got != body {
		t.Fatalf("history text altered: %d bytes, want %d", len(got), len(body))
	}
}

// TestNoteDroppedImagesReplacesBodilessTurns keeps the elision note from stacking
// on top of a synthesized body that only existed to describe the image.
func TestNoteDroppedImagesReplacesBodilessTurns(t *testing.T) {
	cases := []struct {
		in      string
		count   int
		wantHas string
		wantNot string
	}{
		{imageOnlyUserContent, 1, "1 attached image(s) were removed", imageOnlyUserContent},
		{minimalFallbackUserContent, 2, "2 attached image(s) were removed", ""},
		{"", 1, "1 attached image(s) were removed", ""},
		{"look at this", 1, "look at this", ""},
	}
	for _, tc := range cases {
		got := noteDroppedImages(tc.in, tc.count)
		if !strings.Contains(got, tc.wantHas) {
			t.Fatalf("noteDroppedImages(%q, %d) = %q, want it to contain %q", tc.in, tc.count, got, tc.wantHas)
		}
		if tc.wantNot != "" && strings.Contains(got, tc.wantNot) {
			t.Fatalf("noteDroppedImages(%q, %d) = %q, should not keep %q", tc.in, tc.count, got, tc.wantNot)
		}
	}
	if got := noteDroppedImages("unchanged", 0); got != "unchanged" {
		t.Fatalf("noteDroppedImages with count 0 rewrote the body: %q", got)
	}
}

// ---- end-to-end through the Claude request path ----

// TestClaudeRequestWithOversizedImageConverges proves the reclaim stages are
// actually wired into ClaudeToKiro, using the wire shape a decoded request has
// (maps, not typed blocks).
func TestClaudeRequestWithOversizedImageConverges(t *testing.T) {
	bigImage := "data:image/png;base64," + strings.Repeat("A", 4*1024*1024)
	instruction := "describe the screenshot and the earlier discussion"

	payload := ClaudeToKiro(&ClaudeRequest{
		Model:     "claude-opus-4.8",
		MaxTokens: 512,
		System:    "You are helpful.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "here is a screenshot"},
				map[string]interface{}{"type": "image", "source": map[string]interface{}{
					"type": "base64", "media_type": "image/png", "data": bigImage,
				}},
			}},
			{Role: "assistant", Content: "I see it."},
			{Role: "user", Content: instruction},
		},
	}, false)

	if size := payloadByteSize(payload); size > maxPayloadBytes {
		t.Fatalf("payload %d exceeds limit %d: reclaim stages are not reached from ClaudeToKiro", size, maxPayloadBytes)
	}
	if got := payload.ConversationState.CurrentMessage.UserInputMessage.Content; !strings.Contains(got, instruction) {
		t.Fatalf("instruction lost: %q", got)
	}
	if bytes := payloadImageBytes(payload); bytes > maxPayloadBytes {
		t.Fatalf("%d bytes of images still attached", bytes)
	}
}

// itoa avoids pulling strconv in just for subtest names.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}
