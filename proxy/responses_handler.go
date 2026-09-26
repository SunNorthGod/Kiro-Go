package proxy

import (
	"kiro-go/logger"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/pool"
	"net/http"
	"strings"
	"time"
)

const defaultResponsesModel = "claude-sonnet-4.5"

func (h *Handler) handleOpenAIResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	// 低余额串行化闸门(保守透支缓解,见 low_balance.go)。
	releaseLowBalance, ok := gateLowBalance(r)
	if !ok {
		h.sendOpenAIError(w, 429, "rate_limit_error", lowBalanceRejectMessage)
		return
	}
	if releaseLowBalance != nil {
		defer releaseLowBalance()
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}

	if strings.TrimSpace(req.Model) == "" {
		req.Model = defaultResponsesModel
	}

	storedInputCopy := append(json.RawMessage(nil), req.Input...)

	storeResponse := true
	if req.Store != nil {
		storeResponse = *req.Store
	}

	var historyMessages []OpenAIMessage
	if req.PreviousResponseID != "" {
		prev, loadErr := loadResponse(req.PreviousResponseID)
		if loadErr != nil {
			h.sendOpenAIError(w, 404, "invalid_request_error",
				fmt.Sprintf("previous_response_id not found: %v", loadErr))
			return
		}
		historyMessages = expandPreviousResponseHistory(prev)
	}

	inputMessages, err := parseResponsesInput(req.Input)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", err.Error())
		return
	}

	finalMessages := make([]OpenAIMessage, 0, len(historyMessages)+len(inputMessages)+1)
	finalMessages = append(finalMessages, historyMessages...)
	if strings.TrimSpace(req.Instructions) != "" {
		// New instructions on this turn always take effect, even when
		// continuing from previous_response_id. Place them after the
		// expanded history so they apply to the current and future turns,
		// while ancestor instructions (re-emitted by expandPreviousResponseHistory)
		// stay in scope for the historical exchanges they shaped.
		finalMessages = append(finalMessages, OpenAIMessage{
			Role:    "system",
			Content: req.Instructions,
		})
	}
	finalMessages = append(finalMessages, inputMessages...)

	if len(finalMessages) == 0 {
		h.sendOpenAIError(w, 400, "invalid_request_error", "input must contain at least one message")
		return
	}

	hasUser := false
	for _, m := range finalMessages {
		if m.Role == "user" {
			hasUser = true
			break
		}
	}
	if !hasUser {
		h.sendOpenAIError(w, 400, "invalid_request_error", "input must contain at least one user message")
		return
	}

	openaiReq := &OpenAIRequest{
		Model:    req.Model,
		Messages: finalMessages,
		Stream:   req.Stream,
		Tools:    req.Tools,
	}
	if req.Temperature != nil {
		openaiReq.Temperature = *req.Temperature
	}
	if req.MaxOutputTokens != nil {
		openaiReq.MaxTokens = *req.MaxOutputTokens
	}

	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)
	openaiReq.Model = actualModel

	estimatedInputTokens := estimateOpenAIRequestInputTokens(openaiReq)
	cacheProfile := h.promptCache.BuildOpenAIProfile(openaiReq, estimatedInputTokens)
	kiroPayload := OpenAIToKiro(openaiReq, thinking)

	apiKeyID := apiKeyIDFromContext(r.Context())
	respID := generateResponseID()

	if req.Stream {
		h.handleResponsesStream(r.Context(), w, kiroPayload, actualModel, thinking, estimatedInputTokens,
			cacheProfile, apiKeyID, respID, &req, storedInputCopy, storeResponse)
		return
	}

	h.handleResponsesNonStream(r.Context(), w, kiroPayload, actualModel, thinking, estimatedInputTokens,
		cacheProfile, apiKeyID, respID, &req, storedInputCopy, storeResponse)
}

func (h *Handler) handleResponsesNonStream(
	ctx context.Context, w http.ResponseWriter, payload *KiroPayload, model string, thinking bool,
	estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID, respID string,
	req *ResponsesRequest, storedInput json.RawMessage, storeResponse bool,
) {
	excluded := make(map[string]bool)
	var lastErr error
	reqStart := time.Now()

	conversationID := payload.ConversationState.AgentContinuationId
	bypassFairness := apiKeyID == ""
	var keyConcurrency *int
	var boundAccountIDs []string
	if e := config.GetApiKeyEntry(apiKeyID); e != nil {
		keyConcurrency = e.MaxConcurrency
		boundAccountIDs = e.BoundAccountIDs
	}
	// Panic-safety net: release the acquired slot even on a handler/callback panic.
	// releaseSlot is idempotent, so it never double-releases with the explicit calls.
	var activeRelease func()
	defer func() {
		if activeRelease != nil {
			activeRelease()
		}
	}()
	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account, releaseSlot, aerr := h.pool.Acquire(apiKeyID, keyConcurrency, bypassFairness, conversationID, model, excluded, boundAccountIDs)
		activeRelease = releaseSlot
		if aerr == pool.ErrTooBusy {
			h.sendOpenAIError(w, 429, "rate_limit_exceeded", "Too many concurrent requests for this key; retry shortly")
			return
		}
		if aerr != nil {
			break
		}
		fulllogNoteAccount(ctx, account.ID)
		if err := h.ensureValidToken(&account); err != nil {
			releaseSlot()
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(&account, err)
			continue
		}
		cacheUsage := h.promptCache.Compute(account.ID, cacheProfile)

		var content, reasoningContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var meteringCacheRead, meteringCacheCreation int
		var hasCacheMetering bool
		var upstreamStopReason string

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					reasoningContent += text
				} else {
					content += text
				}
			},
			OnToolUse:  func(tu KiroToolUse) { toolUses = append(toolUses, tu) },
			OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnCredits:  func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
			OnCacheMetering: func(read, creation int) {
				meteringCacheRead, meteringCacheCreation = read, creation
				hasCacheMetering = true
			},
			OnStopReason: func(reason string) {
				upstreamStopReason = reason
			},
		}

		measure := func() (int, int, string, bool) {
			return len(content), len(toolUses), upstreamStopReason, reasoningContent != ""
		}

		reset := func() {
			content = ""
			reasoningContent = ""
			toolUses = nil
			inputTokens = 0
			outputTokens = 0
			credits = 0
			realInputTokens = 0
			upstreamStopReason = ""
		}

		// Fully buffered path: nothing reaches the client until the response is
		// encoded, so a retry can never duplicate output. (#143/#146 上游并入,
		// 内层保留本地 callKiroWithSelfHeal 的 400 自愈。)
		err := runKiroWithSelfHealAndIntegrity(ctx, &account, payload, callback, measure, reset, nil)
		if err != nil {
			// Client disconnected → release and return silently (see clientGone).
			if clientGone(ctx) {
				releaseSlot()
				// 非流式:尚未向客户端写任何字节,ctx 取消只能来自客户端读端断开。
				h.noteClientDisconnect("responses", model, apiKeyID,
					estimateApproxTokens(content)+estimateApproxTokens(reasoningContent), err, false)
				return
			}
			releaseSlot()
			lastErr = err
			excluded[account.ID] = true
			// #146(上游): 完整性错误(截断/空流)是上游抖动,不记账号故障,仅换号。
			if !isStreamIntegrityError(err) {
				h.handleAccountFailure(&account, err)
			}
			if isRequestShapeErrorMessage(err.Error()) {
				break // 见 isRequestShapeErrorMessage:换号发同一份 payload 必然同样失败
			}
			continue
		}

		finalContent, _ := extractThinkingFromContent(content)
		if !thinking {
			reasoningContent = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		cacheUsage = resolvePromptCacheUsage(cacheUsage, hasCacheMetering, meteringCacheRead, meteringCacheCreation, inputTokens)
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)

		// 空响应护栏(对齐 Claude 路径):上游成功但零内容零工具 → 回错误而非静默空 completed,
		// 避免 agentic 客户端卡死。
		if isEmptyKiroResponse(finalContent, reasoningContent, len(toolUses), outputTokens, inputTokens, model) {
			h.pool.RecordSuccess(account.ID)
			releaseSlot()
			oversized := emptyResponseIsOversizedContext(inputTokens, model)
			errType, errMsg := emptyResponseErrorInfo(oversized)
			h.recordFailureWithDetails("responses", model, account.ID, fmt.Errorf("empty upstream response"))
			code := 503
			if oversized {
				code = 400
			}
			h.sendOpenAIError(w, code, errType, errMsg)
			return
		}

		h.recordSuccessForApiKeyWithCache(apiKeyID, model, inputTokens, outputTokens, cacheUsage.CacheReadInputTokens, cacheUsage.CacheCreationInputTokens, credits)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		releaseSlot()
		h.promptCache.Update(account.ID, cacheProfile)
		h.recordSuccessLog("responses", model, account.ID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

		respObj := buildResponsesObject(respID, model, finalContent, toolUses, inputTokens, outputTokens, req, upstreamStopReason)
		respObj.Usage.InputTokensDetails = &ResponsesInputTokensDetails{CachedTokens: cacheUsage.CacheReadInputTokens}
		respObj.StoredInput = storedInput
		respObj.Instructions = req.Instructions

		if storeResponse {
			if saveErr := saveResponse(respObj); saveErr != nil {
				logResponsesPersistFailure(respObj.ID, saveErr)
			}
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(respObj)
		return
	}

	if lastErr == nil {
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}
	h.recordFailureWithDetails("responses", model, "", lastErr)
	h.sendOpenAIError(w, 500, "server_error", lastErr.Error())
}

func mapResponsesCompletion(reason string) (status, incompleteReason string) {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "max_tokens", "max_output_tokens", "length", "model_context_window_exceeded", "context_window_exceeded":
		return "incomplete", "max_output_tokens"
	case "refusal", "content_filter", "content_filtered", "guardrail_intervened":
		return "incomplete", "content_filter"
	default:
		return "completed", ""
	}
}

func buildResponsesObject(
	id, model, content string, toolUses []KiroToolUse,
	inputTokens, outputTokens int, req *ResponsesRequest, upstreamStopReason string,
) *ResponsesObject {
	output := make([]ResponseOutputItem, 0, 1+len(toolUses))

	if strings.TrimSpace(content) != "" {
		output = append(output, ResponseOutputItem{
			ID:     generateOutputItemID("msg"),
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []ResponseContentPart{{
				Type: "output_text",
				Text: content,
			}},
		})
	}

	for _, tu := range toolUses {
		args, _ := json.Marshal(tu.Input)
		output = append(output, ResponseOutputItem{
			ID:        generateOutputItemID("fc"),
			Type:      "function_call",
			Status:    "completed",
			CallID:    tu.ToolUseID,
			Name:      tu.Name,
			Arguments: string(args),
		})
	}

	if len(output) == 0 {
		output = append(output, ResponseOutputItem{
			ID:     generateOutputItemID("msg"),
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []ResponseContentPart{{
				Type: "output_text",
				Text: "",
			}},
		})
	}

	status, incompleteReason := mapResponsesCompletion(upstreamStopReason)
	var incompleteDetails *ResponsesIncompleteDetails
	if incompleteReason != "" {
		incompleteDetails = &ResponsesIncompleteDetails{Reason: incompleteReason}
	}

	return &ResponsesObject{
		ID:                 id,
		Object:             "response",
		CreatedAt:          time.Now().Unix(),
		Status:             status,
		Model:              model,
		Output:             output,
		Usage:              ResponsesUsage{InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: inputTokens + outputTokens},
		PreviousResponseID: req.PreviousResponseID,
		Metadata:           req.Metadata,
		IncompleteDetails:  incompleteDetails,
	}
}

func (h *Handler) handleResponsesStream(
	ctx context.Context, w http.ResponseWriter, payload *KiroPayload, model string, thinking bool,
	estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID, respID string,
	req *ResponsesRequest, storedInput json.RawMessage, storeResponse bool,
) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	// SSE keepalive(注释行心跳,同 OpenAI chat 路径)。responses 流一进来就发
	// response.created(隐式提交 200 头并 flush),所以这里无需 commitStream 挂
	// OnStreamStart——首个 send 之后直接 Start,心跳顺带覆盖换号重试的等待期。
	kw := newSSEKeepaliveWriter(w, flusher, commentKeepalivePing, sseKeepaliveInterval, &h.keepalivePings, "responses")
	defer kw.Stop()
	w = kw
	flusher = kw

	send := func(eventName string, payload interface{}) {
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, string(data))
		flusher.Flush()
	}

	createdAt := time.Now().Unix()
	initial := &ResponsesObject{
		ID:                 respID,
		Object:             "response",
		CreatedAt:          createdAt,
		Status:             "in_progress",
		Model:              model,
		Output:             []ResponseOutputItem{},
		Usage:              ResponsesUsage{},
		PreviousResponseID: req.PreviousResponseID,
		Metadata:           req.Metadata,
	}
	send("response.created", map[string]interface{}{
		"type":     "response.created",
		"response": initial,
	})
	// 200 头已随首个事件提交,从此心跳安全。
	kw.Start()

	excluded := make(map[string]bool)
	var lastErr error
	reqStart := time.Now()

	conversationID := payload.ConversationState.AgentContinuationId
	bypassFairness := apiKeyID == ""
	var keyConcurrency *int
	var boundAccountIDs []string
	if e := config.GetApiKeyEntry(apiKeyID); e != nil {
		keyConcurrency = e.MaxConcurrency
		boundAccountIDs = e.BoundAccountIDs
	}
	// Panic-safety net: release the acquired slot even on a handler/callback panic.
	// releaseSlot is idempotent, so it never double-releases with the explicit calls.
	var activeRelease func()
	defer func() {
		if activeRelease != nil {
			activeRelease()
		}
	}()
	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account, releaseSlot, aerr := h.pool.Acquire(apiKeyID, keyConcurrency, bypassFairness, conversationID, model, excluded, boundAccountIDs)
		activeRelease = releaseSlot
		if aerr == pool.ErrTooBusy {
			send("response.failed", map[string]interface{}{
				"type": "response.failed",
				"response": map[string]interface{}{
					"id":     respID,
					"status": "failed",
					"error": map[string]string{
						"type":    "rate_limit_exceeded",
						"message": "Too many concurrent requests for this key; retry shortly",
					},
				},
			})
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
		if aerr != nil {
			break
		}
		fulllogNoteAccount(ctx, account.ID)
		if err := h.ensureValidToken(&account); err != nil {
			releaseSlot()
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(&account, err)
			continue
		}

		send("response.in_progress", map[string]interface{}{
			"type":     "response.in_progress",
			"response": initial,
		})
		cacheUsage := h.promptCache.Compute(account.ID, cacheProfile)

		var (
			fullText              strings.Builder
			reasoningText         strings.Builder
			toolUses              []KiroToolUse
			inputTokens           int
			outputTokens          int
			credits               float64
			realInputTokens       int
			meteringCacheRead     int
			meteringCacheCreation int
			hasCacheMetering      bool
			upstreamStopReason    string
		)

		messageItemID := generateOutputItemID("msg")
		messageStarted := false
		outputIndex := 0
		contentIndex := 0

		ensureMessageStarted := func() {
			if messageStarted {
				return
			}
			messageStarted = true
			send("response.output_item.added", map[string]interface{}{
				"type":         "response.output_item.added",
				"output_index": outputIndex,
				"item": map[string]interface{}{
					"id":      messageItemID,
					"type":    "message",
					"role":    "assistant",
					"status":  "in_progress",
					"content": []map[string]interface{}{},
				},
			})
			send("response.content_part.added", map[string]interface{}{
				"type":          "response.content_part.added",
				"item_id":       messageItemID,
				"output_index":  outputIndex,
				"content_index": contentIndex,
				"part": map[string]interface{}{
					"type": "output_text",
					"text": "",
				},
			})
		}

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				if isThinking {
					reasoningText.WriteString(text)
					return
				}
				fullText.WriteString(text)
				ensureMessageStarted()
				send("response.output_text.delta", map[string]interface{}{
					"type":          "response.output_text.delta",
					"item_id":       messageItemID,
					"output_index":  outputIndex,
					"content_index": contentIndex,
					"delta":         text,
				})
			},
			OnToolUse: func(tu KiroToolUse) {
				if messageStarted {
					send("response.content_part.done", map[string]interface{}{
						"type":          "response.content_part.done",
						"item_id":       messageItemID,
						"output_index":  outputIndex,
						"content_index": contentIndex,
						"part": map[string]interface{}{
							"type": "output_text",
							"text": fullText.String(),
						},
					})
					send("response.output_item.done", map[string]interface{}{
						"type":         "response.output_item.done",
						"output_index": outputIndex,
						"item": map[string]interface{}{
							"id":     messageItemID,
							"type":   "message",
							"role":   "assistant",
							"status": "completed",
							"content": []map[string]interface{}{{
								"type": "output_text",
								"text": fullText.String(),
							}},
						},
					})
					messageStarted = false
					outputIndex++
				}

				toolUses = append(toolUses, tu)
				args, _ := json.Marshal(tu.Input)
				fcID := generateOutputItemID("fc")
				send("response.output_item.added", map[string]interface{}{
					"type":         "response.output_item.added",
					"output_index": outputIndex,
					"item": map[string]interface{}{
						"id":        fcID,
						"type":      "function_call",
						"status":    "in_progress",
						"call_id":   tu.ToolUseID,
						"name":      tu.Name,
						"arguments": "",
					},
				})
				send("response.function_call_arguments.delta", map[string]interface{}{
					"type":         "response.function_call_arguments.delta",
					"item_id":      fcID,
					"output_index": outputIndex,
					"delta":        string(args),
				})
				send("response.output_item.done", map[string]interface{}{
					"type":         "response.output_item.done",
					"output_index": outputIndex,
					"item": map[string]interface{}{
						"id":        fcID,
						"type":      "function_call",
						"status":    "completed",
						"call_id":   tu.ToolUseID,
						"name":      tu.Name,
						"arguments": string(args),
					},
				})
				outputIndex++
			},
			OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnCredits:  func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
			OnCacheMetering: func(read, creation int) {
				meteringCacheRead, meteringCacheCreation = read, creation
				hasCacheMetering = true
			},
			OnStopReason: func(reason string) {
				upstreamStopReason = reason
			},
		}

		// #146(上游并入,检测半): responses 流在进入账号循环前就已发 response.created
		// (200 头已提交),本地设计为不再换号/同账号重跑(见下方错误路径注释),
		// 故这里只做完整性检测:传输成功但流被截断(有内容/思考、无终止信号、无工具)
		// → 按 response.failed 收尾,不再伪造 response.completed。
		// (#143 的同账号重试预算只用于非流式/未提交路径,见 handler.go。)
		err := callKiroWithSelfHeal(ctx, &account, payload, callback)
		if err == nil {
			if ierr := classifyStreamIntegrity(fullText.Len(), len(toolUses), upstreamStopReason, reasoningText.Len() > 0); ierr != nil && ctx.Err() == nil {
				err = ierr
				logger.Warnf("[StreamIntegrity] %v on %s; signaling response.failed (stream already committed)", ierr, account.Email)
			}
		}
		if err != nil {
			// Client disconnected → release and return silently (see clientGone):
			// no exclude/retry, no account-failure signal, no response.failed event
			// (there is no client to receive it).
			if clientGone(ctx) {
				releaseSlot()
				h.noteClientDisconnect("responses", model, apiKeyID,
					estimateApproxTokens(fullText.String())+estimateApproxTokens(reasoningText.String()),
					err, kw.WriteFailed())
				return
			}
			// responses 流在进入本循环前就已发 response.created(提交 200 头)。
			// 因此上游一旦返回错误,任何换号重跑都会把第二轮内容续到同一个已提交的流里
			// → 客户端看到"两份回答"。这里不再按 responseStarted 决定是否重试(该标志
			// 只在首个 delta 后才置位,存在提交后未出首字的窗口);200 已提交即收尾。
			// (CallKiroAPI 之前的 ensureValidToken 失败仍走上面的 continue,不受影响。)
			releaseSlot()
			lastErr = err
			excluded[account.ID] = true
			// #146(上游): 截断流是上游抖动,不记账号故障;仅传输类错误才拉黑账号。
			if !isStreamIntegrityError(err) {
				h.handleAccountFailure(&account, err)
			}
			send("response.failed", map[string]interface{}{
				"type": "response.failed",
				"response": map[string]interface{}{
					"id":     respID,
					"status": "failed",
					"error": map[string]string{
						"type":    "server_error",
						"message": err.Error(),
					},
				},
			})
			h.recordFailureWithDetails("responses", model, account.ID, err)
			return
		}

		finalContent, _ := extractThinkingFromContent(fullText.String())
		reasoning := reasoningText.String()
		if !thinking {
			reasoning = ""
		}

		if messageStarted {
			send("response.content_part.done", map[string]interface{}{
				"type":          "response.content_part.done",
				"item_id":       messageItemID,
				"output_index":  outputIndex,
				"content_index": contentIndex,
				"part": map[string]interface{}{
					"type": "output_text",
					"text": finalContent,
				},
			})
			send("response.output_item.done", map[string]interface{}{
				"type":         "response.output_item.done",
				"output_index": outputIndex,
				"item": map[string]interface{}{
					"id":     messageItemID,
					"type":   "message",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]interface{}{{
						"type": "output_text",
						"text": finalContent,
					}},
				},
			})
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		cacheUsage = resolvePromptCacheUsage(cacheUsage, hasCacheMetering, meteringCacheRead, meteringCacheCreation, inputTokens)
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoning, toolUses)

		// Empty-response guard (parity with the Claude/OpenAI paths): upstream
		// succeeded but produced no text/tools. A fully empty response never
		// started a message item (ensureMessageStarted only fires on non-empty
		// text), so emit response.failed instead of a hollow response.completed —
		// otherwise agentic clients treat the empty turn as done and can hang.
		if isEmptyKiroResponse(finalContent, reasoning, len(toolUses), outputTokens, inputTokens, model) {
			h.pool.RecordSuccess(account.ID)
			releaseSlot()
			_, errMsg := emptyResponseErrorInfo(emptyResponseIsOversizedContext(inputTokens, model))
			h.recordFailureWithDetails("responses", model, account.ID, fmt.Errorf("empty upstream response"))
			send("response.failed", map[string]interface{}{
				"type": "response.failed",
				"response": map[string]interface{}{
					"id":     respID,
					"status": "failed",
					"error":  map[string]string{"type": "server_error", "message": errMsg},
				},
			})
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}

		h.recordSuccessForApiKeyWithCache(apiKeyID, model, inputTokens, outputTokens, cacheUsage.CacheReadInputTokens, cacheUsage.CacheCreationInputTokens, credits)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		releaseSlot()
		h.promptCache.Update(account.ID, cacheProfile)
		h.recordSuccessLog("responses", model, account.ID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

		respObj := buildResponsesObject(respID, model, finalContent, toolUses, inputTokens, outputTokens, req, upstreamStopReason)
		respObj.Usage.InputTokensDetails = &ResponsesInputTokensDetails{CachedTokens: cacheUsage.CacheReadInputTokens}
		respObj.CreatedAt = createdAt
		respObj.StoredInput = storedInput
		respObj.Instructions = req.Instructions

		if storeResponse {
			if saveErr := saveResponse(respObj); saveErr != nil {
				logResponsesPersistFailure(respObj.ID, saveErr)
			}
		}

		send("response.completed", map[string]interface{}{
			"type":     "response.completed",
			"response": respObj,
		})
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		// 上游正常收尾但写出已失败过 → 客户端只收到截断的流(见 noteStreamWriteFailure)。
		if kw.WriteFailed() {
			h.noteStreamWriteFailure("responses", model, apiKeyID)
		}
		return
	}

	if lastErr == nil {
		send("response.failed", map[string]interface{}{
			"type": "response.failed",
			"response": map[string]interface{}{
				"id":     respID,
				"status": "failed",
				"error": map[string]string{
					"type":    "server_error",
					"message": "No available accounts",
				},
			},
		})
		return
	}
	h.recordFailureWithDetails("responses", model, "", lastErr)
	send("response.failed", map[string]interface{}{
		"type": "response.failed",
		"response": map[string]interface{}{
			"id":     respID,
			"status": "failed",
			"error": map[string]string{
				"type":    "server_error",
				"message": lastErr.Error(),
			},
		},
	})
}
