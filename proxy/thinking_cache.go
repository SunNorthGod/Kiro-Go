package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// ==================== 思考重注入缓存 ====================
//
// OpenAI 协议线(插件→NewAPI→kirogo)上,客户端结构性无法回传上一轮 assistant 的
// 思考块(OpenAI 消息没有 reasoning 字段),interleaved thinking 因此断链。原生
// Kiro 客户端则会把 reasoning+signature 原样带回。本缓存在服务端补上这一环:
//
//   写入:一轮成功响应完成后,rememberThinkingForReplay 记下该会话最后一条
//        assistant 回复的内容哈希、思考全文与(溯源包装后的)签名。
//   注入:ClaudeToKiro 构建上游历史时,若请求历史里最后一条 assistant 消息的
//        文本与缓存哈希命中(=客户端原样回传了上一轮回复,且没带思考),把缓存
//        的思考块作为 ReasoningCandidate 挂回去。此后走既有管线:选号后由
//        applyThinkingProvenance 按"签名产出账号==当前服务账号"裁决,跨账号
//        自动剥离;若注入的签名过期被上游 400,SelfHeal 会剥思考重试(SelfHeal
//        StillStripsKeptReasoning 锁定的既有兜底)。
//
// 客户端截断/改写历史(上下文压缩)会让哈希不命中,注入自然跳过——宁缺毋滥。

const (
	thinkingReplayTTL      = time.Hour
	thinkingReplayCapacity = 1024
)

type thinkingReplayEntry struct {
	contentHash string
	reasoning   string
	wrappedSig  string // kgsig1-包装后的签名,注入即用
	producerTok string // 产出账号的签名指纹(ReasoningProducer)
	ts          time.Time
}

var (
	thinkingReplayMu sync.Mutex
	thinkingReplay   = map[string]thinkingReplayEntry{}
)

func hashAssistantContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// rememberThinkingForReplay stores the reasoning block of a completed assistant
// turn. Empty reasoning or empty signature is not stored: replaying a signature-
// less reasoning block would be stripped downstream anyway.
func rememberThinkingForReplay(conversationID, assistantContent, reasoning, realSig, accountID string) {
	if conversationID == "" || reasoning == "" || realSig == "" || accountID == "" || assistantContent == "" {
		return
	}
	wrapped := wrapProvenanceSignature(realSig, accountID)
	if wrapped == realSig { // wrap failed (empty token) - nothing useful to replay
		return
	}
	thinkingReplayMu.Lock()
	defer thinkingReplayMu.Unlock()
	// Bounded: on overflow drop expired first, then arbitrary entries. Exact LRU
	// is not worth the bookkeeping - misses degrade to the status quo.
	if len(thinkingReplay) >= thinkingReplayCapacity {
		now := time.Now()
		for k, v := range thinkingReplay {
			if now.Sub(v.ts) > thinkingReplayTTL {
				delete(thinkingReplay, k)
			}
		}
		for k := range thinkingReplay {
			if len(thinkingReplay) < thinkingReplayCapacity {
				break
			}
			delete(thinkingReplay, k)
		}
	}
	thinkingReplay[conversationID] = thinkingReplayEntry{
		contentHash: hashAssistantContent(assistantContent),
		reasoning:   reasoning,
		wrappedSig:  wrapped,
		producerTok: accountSignatureToken(accountID),
		ts:          time.Now(),
	}
}

// replayThinkingCandidate returns the cached reasoning block when the request's
// last assistant message matches what this conversation last produced.
func replayThinkingCandidate(conversationID, lastAssistantContent string) (reasoning, wrappedSig, producerTok string, ok bool) {
	if conversationID == "" || lastAssistantContent == "" {
		return "", "", "", false
	}
	thinkingReplayMu.Lock()
	defer thinkingReplayMu.Unlock()
	e, hit := thinkingReplay[conversationID]
	if !hit {
		return "", "", "", false
	}
	if time.Since(e.ts) > thinkingReplayTTL {
		delete(thinkingReplay, conversationID)
		return "", "", "", false
	}
	if e.contentHash != hashAssistantContent(lastAssistantContent) {
		return "", "", "", false
	}
	return e.reasoning, e.wrappedSig, e.producerTok, true
}
