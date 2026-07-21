package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// signature_provenance.go implements "换号主动剥历史 thinking"（v1.1.9）。
//
// 背景（2026-07-20 严查 2 实锤，read-only 实验 + 代码级追踪）:
//   - opus-4.8 原生下发真实思考签名,kirogo 逐字节保真回传/回收(实验证明同账号回放被接受)。
//   - 但 Kiro/AWS 的思考签名是**账号(profile)绑定**的:同账号回放 → 200;跨账号回放 →
//     THINKING_SIGNATURE_INVALID(400)。生产 400 风暴恒定报 messages.3.content.0(会话里
//     最早那条 assistant 思考块)——它由首轮服务账号产出,一旦后续轮次因 429 退避/粘性淘汰/
//     账号封禁被派到别的账号,旧签名即失效。**这解释了为何 400 数(≈167/h)能超过换号数**:
//     巨会话只要早期换过一次号,之后每一轮(哪怕之后一直粘同一新账号)都在回放那条外来签名。
//   - 旧行为:发上游 → 400 →(跨 3 个端点各撞一次 400)→ SelfHeal 剥 reasoning 同账号重试。
//     一次 400 事件 ≈ 4 个上游往返。本模块把它前移到"发上游前"就剥掉必失效的签名。
//
// 机制:signature 里自带产出账号的短 token(wrapProvenanceSignature,回传给客户端;下一轮
// parseProvenanceSignature 取出)。选号后 applyThinkingProvenance 逐块比对:
//   - 签名产出账号 == 当前服务账号 → 保留(有效,还能命中上游前缀缓存 / 延续 interleaved thinking)。
//   - 跨账号 / 无 provenance 标记(外来签名:Kiro IDE 原生、真 Anthropic、旧 Rust、v1.1.9 前的
//     kirogo)→ 剥离(判定不了就宁可剥;剥了 SelfHeal 已证上游接受,代价仅是丢一点缓存收益)。
// SelfHeal 仍作兜底(剥后仍 400 的其它场景继续自愈)。

// RealSignatureProvenancePrefix 标记"由 kirogo 回传、带产出账号 token 的真实签名"。
// 形如 "<prefix><token>:<realSignature>"。选用不与 base64 真实签名冲突、也不与
// FakeSignatureMarker/LegacyRustFakeSignatureMarker 冲突的前缀。
const RealSignatureProvenancePrefix = "kgsig1_"

// accountSignatureToken 为账号 id 派生一个短、稳定、不可逆的 token(sha256 前 4 字节 = 8 hex),
// 只用于比对签名产出账号,绝不把原始账号 id 暴露给客户端。
func accountSignatureToken(accountID string) string {
	if accountID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("kirogo-sig-acct:" + accountID))
	return hex.EncodeToString(sum[:4])
}

// wrapProvenanceSignature 把真实签名连同其产出账号 token 打包(回传客户端)。
// 空账号 token 或空签名时原样返回(不打包)。
func wrapProvenanceSignature(realSig, accountID string) string {
	tok := accountSignatureToken(accountID)
	if tok == "" || realSig == "" {
		return realSig
	}
	return RealSignatureProvenancePrefix + tok + ":" + realSig
}

// parseProvenanceSignature 把 provenance 标记的签名拆回(真实签名, 产出账号 token, ok)。
// 非本代理标记(真 Anthropic base64 / 伪造签名 / 空)返回 ok=false。
func parseProvenanceSignature(sig string) (realSig, token string, ok bool) {
	if !strings.HasPrefix(sig, RealSignatureProvenancePrefix) {
		return "", "", false
	}
	rest := sig[len(RealSignatureProvenancePrefix):]
	idx := strings.IndexByte(rest, ':')
	if idx <= 0 || idx >= len(rest)-1 {
		return "", "", false
	}
	return rest[idx+1:], rest[:idx], true
}

// classifyHistorySignature 归类客户端回传的历史思考签名,给 ClaudeToKiro 决定如何填充
// transient 候选字段:
//   - tagged:  本代理带 provenance 的真实签名 → 解包出真实签名 + 产出账号 token。
//   - foreign: 无标记、也非伪造的签名(外来真实签名,来源未知)→ 原样候选,producer 置空。
//   - fake:    伪造占位签名(本代理或旧 Rust)→ 不作候选(直接丢弃)。
func classifyHistorySignature(sig string) (realSig, producer string, kind string) {
	if realSig, token, ok := parseProvenanceSignature(sig); ok {
		return realSig, token, "tagged"
	}
	if isFakeSignature(sig) {
		return "", "", "fake"
	}
	return sig, "", "foreign"
}

// applyThinkingProvenance 在选号之后(账号已确定)裁决每条历史思考块是否发给上游。
// 只有"签名产出账号 == 当前服务账号"才保留,其余(跨账号 / 来源未知)一律剥离——把必然的
// 400 + SelfHeal 往返消灭在发上游之前。可安全地在每次换号重试时重复调用(候选块被保留,
// 每次从候选按当前账号重新裁决,幂等且可恢复)。
func applyThinkingProvenance(payload *KiroPayload, accountID string) {
	if payload == nil {
		return
	}
	token := accountSignatureToken(accountID)
	for i := range payload.ConversationState.History {
		am := payload.ConversationState.History[i].AssistantResponseMessage
		if am == nil {
			continue
		}
		if am.ReasoningCandidate != nil && token != "" && am.ReasoningProducer == token {
			// 同账号:签名有效,保留(顺带命中上游前缀缓存、延续 interleaved thinking)。
			am.ReasoningContent = am.ReasoningCandidate
		} else {
			// 跨账号或来源未知:提前剥离,避免 THINKING_SIGNATURE_INVALID(400)+SelfHeal 往返。
			am.ReasoningContent = nil
		}
	}
}

// applyResponseThinkingSignature 给非流式 Claude 响应的**第一个** thinking 块透传上游
// 真实签名(#5)。与流式路径 emitSignatureDelta 完全对齐:真实签名经 wrapProvenanceSignature
// 打上产出账号 provenance 标记后回传客户端,下一轮回传时 applyThinkingProvenance 据此判定
// 同账号(保留)/跨账号(剥离),不引入跨账号回放 400 风险。
//
// 关键差异:无真实签名时**不兜底伪造**(流式路径缺签名会 generateFakeSignature 占位)。
// 非流式保持既有语义——无真实签名即空签名,靠客户端 SelfHeal 恢复;仅在上游确实下发真实
// 签名时才透传,是纯增量、不破坏现有兜底。realSig 为空或无 thinking 块时为 no-op。
func applyResponseThinkingSignature(blocks []ClaudeContentBlock, realSig, accountID string) {
	if realSig == "" {
		return
	}
	wrapped := wrapProvenanceSignature(realSig, accountID)
	for i := range blocks {
		if blocks[i].Type == "thinking" {
			blocks[i].Signature = wrapped
			return
		}
	}
}
