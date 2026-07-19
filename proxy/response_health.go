package proxy

import (
	"context"
	"kiro-go/config"
	"kiro-go/logger"
	"strings"
)

// response_health.go 集中承载三类"响应健康"逻辑，移植自 Rust 参照实现
// (anthropic/handlers.rs、anthropic/stream.rs):
//
//  1. 400 自愈重试 (callKiroWithSelfHeal): 上游在请求校验期(流式开始前)返回的两类
//     可安全重试的 400 —— 历史 thinking 签名无效、模型不支持 additionalModelRequestFields
//     —— 剥掉冒犯字段后用同一账号重试一次。
//  2. 空响应检测 (isEmptyKiroResponse): 上游成功但零内容零工具时判定为退化响应，
//     交由各端点回一个错误信号而非静默 end_turn，避免 agentic 客户端卡死。
//  3. stop_reason 语义 (resolveClaudeStopReason / openAIFinishReason): 上下文写满时
//     用更准确的收尾原因。

// ---- 400 自愈重试 ----

// isSelfHealableKiroError 判断上游错误是否为"请求校验期可自愈的 400"。
// 命中的两类(均发生在流式响应开始之前，重试安全):
//   - THINKING_SIGNATURE_INVALID / signature in thinking: 历史带签名思考块被上游拒绝。
//   - additionalModelRequestFields is not supported / not supported for this model:
//     空 schema 模型(sonnet-4.5/haiku/deepseek/glm/qwen 等)不接受该字段。
func isSelfHealableKiroError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	if strings.Contains(msg, "THINKING_SIGNATURE_INVALID") ||
		strings.Contains(lower, "signature` in `thinking") ||
		strings.Contains(lower, "signature in thinking") {
		return true
	}
	if strings.Contains(lower, "additionalmodelrequestfields") {
		return true
	}
	if strings.Contains(lower, "not supported for this model") {
		return true
	}
	return false
}

// stripSelfHealFields 剥离两类冒犯字段:
//   - payload.AdditionalModelRequestFields 顶层字段(空 schema 模型不支持)。
//   - payload.ConversationState.History 中各 assistant 消息的 ReasoningContent
//     (历史思考签名被拒时剥掉，让上游重新裁决)。
//
// 返回是否真的剥掉了东西 —— 没有可剥的就不必重试(重试还是同样的错)。
func stripSelfHealFields(payload *KiroPayload) bool {
	if payload == nil {
		return false
	}
	changed := false
	if payload.AdditionalModelRequestFields != nil {
		payload.AdditionalModelRequestFields = nil
		changed = true
	}
	for i := range payload.ConversationState.History {
		if am := payload.ConversationState.History[i].AssistantResponseMessage; am != nil && am.ReasoningContent != nil {
			am.ReasoningContent = nil
			changed = true
		}
	}
	return changed
}

// callKiroWithSelfHeal 包一层 CallKiroAPI: 命中可自愈的 400 时剥掉冒犯字段用同一账号
// 重试一次;其余错误(或无字段可剥)原样返回，交由常规多账号故障转移处理。
//
// 自愈只针对流式开始前的请求校验 400(非 200 → CallKiroAPI 在调用 parseEventStream 之前
// 就返回错误，callback 尚未触发)，故重试不会造成流内容重复。
func callKiroWithSelfHeal(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	err := CallKiroAPI(ctx, account, payload, callback)
	if err == nil || !isSelfHealableKiroError(err) {
		return err
	}
	if !stripSelfHealFields(payload) {
		return err
	}
	logger.Warnf("[SelfHeal] upstream rejected request (%v); stripped reasoningContent/additionalModelRequestFields, retrying once on same account", err)
	return CallKiroAPI(ctx, account, payload, callback)
}

// ---- 空响应检测 ----

// nearEmptyOutputThreshold: output < 此值且无工具调用时视为"近似空"(仅在上下文极大时触发)。
// 对齐 Rust NEAR_EMPTY_OUTPUT_THRESHOLD。
const nearEmptyOutputThreshold = 30

// emptyResponseOversizedThreshold 返回"上下文过大"判定阈值(模型窗口的 28%)。
// 对齐 Rust empty_response_oversized_threshold。
func emptyResponseOversizedThreshold(model string) int {
	return int(float64(getContextWindowSize(model)) * 0.28)
}

// isEmptyKiroResponse 判断一次成功的上游调用是否零产出(无文本、无 thinking、无工具)。
// 对齐 Rust is_empty_response 的路径 1(完全空)与路径 2(近似空 + 上下文过大)。
//
// 只在"零内容零工具"或"近空且上下文极大"时返回 true —— 正常的短文本回复(有文本)
// 一律不误伤。
func isEmptyKiroResponse(text, thinking string, toolUses, outputTokens, inputTokens int, model string) bool {
	if toolUses > 0 {
		return false
	}
	if strings.TrimSpace(text) != "" || strings.TrimSpace(thinking) != "" {
		return false
	}
	// 路径 1: 完全空(无任何内容)。
	if outputTokens <= 0 {
		return true
	}
	// 路径 2: 近似空(极少 output)且上下文已超阈值 → 视为上下文过大导致的退化响应。
	if outputTokens < nearEmptyOutputThreshold && inputTokens >= emptyResponseOversizedThreshold(model) {
		return true
	}
	return false
}

// emptyResponseIsOversizedContext 空响应是否由上下文过大导致(决定错误语义:
// 过大 → 提示压缩、不建议原样重试;偏小 → 视为偶发、可重试)。
func emptyResponseIsOversizedContext(inputTokens int, model string) bool {
	return inputTokens >= emptyResponseOversizedThreshold(model)
}

// emptyResponseErrorInfo 返回空响应对应的 (errType, message)。对齐 Rust empty_response_error_event:
//   - 上下文过大: invalid_request_error,提示压缩上下文,不鼓励原样重试。
//   - 偶发(偏小): overloaded_error,客户端可重试。
func emptyResponseErrorInfo(oversizedContext bool) (string, string) {
	if oversizedContext {
		return "invalid_request_error", "Upstream returned an empty response, likely because the context is too large. Reduce conversation history (e.g. /compact), system prompt, or tools, then retry."
	}
	return "overloaded_error", "Upstream returned an empty response. Please retry."
}

// ---- stop_reason 语义 ----

// resolveClaudeStopReason 计算 Claude 端点收尾的 stop_reason(优先级):
//  1. 有工具调用 → "tool_use"(最高优先，不能被上下文满盖掉，否则客户端只渲染工具块不执行)。
//  2. 上下文写满(contextUsage>=100 或输入触顶模型窗口) → "model_context_window_exceeded"。
//  3. 否则 → "end_turn"。
func resolveClaudeStopReason(hasToolUse, contextFull bool, inputTokens int, model string) string {
	if hasToolUse {
		return "tool_use"
	}
	if contextFull || inputTokens >= getContextWindowSize(model) {
		return "model_context_window_exceeded"
	}
	return "end_turn"
}

// openAIFinishReason 计算 OpenAI 端点收尾的 finish_reason(优先级同上):
//  1. 有工具调用 → "tool_calls"。
//  2. 上下文写满 → "length"。
//  3. 否则 → "stop"。
func openAIFinishReason(hasToolCall, contextFull bool, inputTokens int, model string) string {
	if hasToolCall {
		return "tool_calls"
	}
	if contextFull || inputTokens >= getContextWindowSize(model) {
		return "length"
	}
	return "stop"
}
