// Package proxy is the core proxy layer for the Kiro API.
// It handles streaming API calls to the Kiro backend and parses AWS Event Stream responses.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Endpoint configuration (auto-fallback on quota exhaustion).
type kiroEndpoint struct {
	URL       string
	Origin    string
	AmzTarget string
	Name      string
}

// maxEventFrameBytes caps a single AWS event-stream frame. The declared 4-byte
// length is untrusted (corruption / a hostile upstream), so it must not drive an
// unbounded allocation. 16 MiB is far above any real Kiro event.
const maxEventFrameBytes = 16 << 20

var kiroEndpoints = []kiroEndpoint{
	{
		URL:       "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "",
		Name:      "Kiro IDE",
	},
	{
		URL:       "https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
		Name:      "CodeWhisperer",
	},
	{
		URL:       "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "AmazonQDeveloperStreamingService.SendMessage",
		Name:      "AmazonQ",
	},
}

// Global HTTP clients, swappable at runtime to apply proxy reconfiguration without restart.
var kiroHttpStore atomic.Pointer[http.Client]
var kiroRestHttpStore atomic.Pointer[http.Client]

// proxyClientCache caches http.Client instances keyed by proxy URL for per-account proxy support.
var proxyClientCache sync.Map

// streamIdleTimeout bounds how long a streaming response may go WITHOUT producing
// any bytes before the request is aborted. This replaces the old whole-request
// 5-minute client Timeout, which hard-cut any long stream regardless of progress
// (a legitimate multi-minute agentic answer would be truncated). A per-read idle
// deadline instead lets a stream run arbitrarily long as long as it keeps
// producing, while still killing a genuinely hung upstream. Override via
// KIRO_STREAM_IDLE_TIMEOUT_SECONDS. Mirrors the Rust v2026.1.42 fix (overall
// .timeout → connect/read timeouts).
var streamIdleTimeout = func() time.Duration {
	if v := os.Getenv("KIRO_STREAM_IDLE_TIMEOUT_SECONDS"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return 120 * time.Second
}()

func init() {
	InitKiroHttpClient("")
}

// GetClientForProxy returns the streaming http.Client for the given proxy URL.
// The streaming client has NO whole-request timeout — long-stream cutting is
// avoided; connect/TLS/response-header limits live on the Transport, and a
// per-read idle deadline (streamIdleTimeout) is applied around the response body
// in CallKiroAPI. If proxyURL is empty, returns the global kiro HTTP client.
func GetClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return kiroHttpStore.Load()
	}
	if cached, ok := proxyClientCache.Load(proxyURL); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		Transport: buildKiroTransport(proxyURL),
	}
	proxyClientCache.Store(proxyURL, client)
	return client
}

// GetRestClientForProxy returns a rest http.Client (30s timeout) for the given proxy URL.
// If proxyURL is empty, returns the global kiro REST HTTP client.
func GetRestClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return kiroRestHttpStore.Load()
	}
	cacheKey := "rest:" + proxyURL
	if cached, ok := proxyClientCache.Load(cacheKey); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildKiroTransport(proxyURL),
	}
	proxyClientCache.Store(cacheKey, client)
	return client
}

// ResolveAccountProxyURL returns the effective proxy URL for an account.
// Falls back to global config.GetProxyURL() if the account has no per-account proxy.
func ResolveAccountProxyURL(account *config.Account) string {
	if account != nil && account.ProxyURL != "" {
		return account.ProxyURL
	}
	return config.GetProxyURL()
}

// buildKiroTransport constructs an HTTP Transport with optional outbound proxy
// support. Transport-level timeouts bound connection setup and time-to-headers
// WITHOUT capping total stream duration (unlike a whole-request client Timeout):
// dial/TLS/response-header limits catch a dead or unresponsive upstream, while a
// live long stream is only bounded by the per-read idle deadline in CallKiroAPI.
func buildKiroTransport(proxyURL string) *http.Transport {
	t := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    false,
		ForceAttemptHTTP2:     true,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			t.Proxy = http.ProxyURL(u)
			// Proxied connections cannot negotiate HTTP/2.
			t.ForceAttemptHTTP2 = false
		}
	} else {
		t.Proxy = http.ProxyFromEnvironment
	}
	return t
}

// InitKiroHttpClient initializes (or reinitializes) the HTTP clients used for Kiro API requests.
//
// The streaming client has NO whole-request Timeout (long streams must not be
// hard-cut); it relies on the Transport's connect/TLS/response-header limits plus
// the per-read idle deadline applied in CallKiroAPI. The REST client keeps its
// short 30s whole-request timeout — those calls are single short round-trips.
func InitKiroHttpClient(proxyURL string) {
	client := &http.Client{
		Transport: buildKiroTransport(proxyURL),
	}
	kiroHttpStore.Store(client)

	restClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildKiroTransport(proxyURL),
	}
	kiroRestHttpStore.Store(restClient)
}

// idleTimeoutReader wraps a streaming response body with a per-read idle
// deadline: every Read that returns data renews the timer, and if no bytes
// arrive within the timeout the onIdle callback (the request-context cancel)
// fires, aborting the in-flight upstream request so the blocked Read unblocks
// with an error. This bounds a hung stream without capping a healthy long one.
type idleTimeoutReader struct {
	body    io.ReadCloser
	timer   *time.Timer
	timeout time.Duration
	stop    sync.Once
}

func newIdleTimeoutReader(body io.ReadCloser, timeout time.Duration, onIdle func()) *idleTimeoutReader {
	return &idleTimeoutReader{
		body:    body,
		timeout: timeout,
		// AfterFunc timer: Reset on an AfterFunc timer needs no channel drain.
		// A benign race at the exact deadline can call onIdle (cancel) as a Read
		// completes; cancel is idempotent and aborting a stream that just hit its
		// idle window is the intended safety behavior.
		timer: time.AfterFunc(timeout, onIdle),
	}
}

func (r *idleTimeoutReader) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if n > 0 {
		r.timer.Reset(r.timeout)
	}
	return n, err
}

func (r *idleTimeoutReader) Close() error {
	r.stop.Do(func() { r.timer.Stop() })
	return r.body.Close()
}

// ==================== Request Structs ====================

// KiroPayload is the top-level request body sent to the Kiro API.
type KiroPayload struct {
	ConversationState struct {
		AgentContinuationId string `json:"agentContinuationId,omitempty"`
		AgentTaskType       string `json:"agentTaskType,omitempty"`
		ChatTriggerType     string `json:"chatTriggerType"`
		ConversationID      string `json:"conversationId"`
		CurrentMessage      struct {
			UserInputMessage KiroUserInputMessage `json:"userInputMessage"`
		} `json:"currentMessage"`
		History []KiroHistoryMessage `json:"history,omitempty"`
	} `json:"conversationState"`
	ProfileArn      string           `json:"profileArn,omitempty"`
	InferenceConfig *InferenceConfig `json:"inferenceConfig,omitempty"`

	// AdditionalModelRequestFields 承载 reasoning/output_config 的 effort 档位透传
	// (按模型家族 schema 驱动)。为空则不序列化——空 schema 模型(sonnet-4.5/haiku 等)
	// 发了会被上游 400 "additionalModelRequestFields is not supported for this model"。
	AdditionalModelRequestFields map[string]interface{} `json:"additionalModelRequestFields,omitempty"`

	// ToolNameMap maps sanitized tool names (sent to Kiro) back to the
	// original names supplied by the client. Used to restore original names
	// in tool_use responses so the client can match them to its tool registry.
	// Not serialized to the Kiro API request body.
	ToolNameMap map[string]string `json:"-"`
}

type KiroUserInputMessage struct {
	Content                 string                   `json:"content"`
	ModelID                 string                   `json:"modelId,omitempty"`
	Origin                  string                   `json:"origin"`
	Images                  []KiroImage              `json:"images,omitempty"`
	UserInputMessageContext *UserInputMessageContext `json:"userInputMessageContext,omitempty"`
}

type UserInputMessageContext struct {
	Tools       []KiroToolWrapper `json:"tools,omitempty"`
	ToolResults []KiroToolResult  `json:"toolResults,omitempty"`
}

type KiroToolWrapper struct {
	ToolSpecification struct {
		Name        string      `json:"name"`
		Description string      `json:"description"`
		InputSchema InputSchema `json:"inputSchema"`
	} `json:"toolSpecification"`
}

type InputSchema struct {
	JSON interface{} `json:"json"`
}

type KiroToolResult struct {
	ToolUseID string              `json:"toolUseId"`
	Content   []KiroResultContent `json:"content"`
	Status    string              `json:"status"`
}

type KiroResultContent struct {
	Text string `json:"text"`
}

type KiroImage struct {
	Format string `json:"format"`
	Source struct {
		Bytes string `json:"bytes"`
	} `json:"source"`
}

type KiroHistoryMessage struct {
	UserInputMessage         *KiroUserInputMessage         `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *KiroAssistantResponseMessage `json:"assistantResponseMessage,omitempty"`
}

type KiroAssistantResponseMessage struct {
	Content  string        `json:"content"`
	ToolUses []KiroToolUse `json:"toolUses,omitempty"`
	// ReasoningContent 回传历史带签名的思考(延续工具循环的 interleaved thinking)。
	// 嵌套 wire format {reasoningText:{text,signature}};仅当带模型真实签名时才发,
	// 无签名/伪造签名一律省略(否则 Kiro 400 REQUEST_BODY_INVALID / THINKING_SIGNATURE_INVALID)。
	ReasoningContent *KiroReasoningContent `json:"reasoningContent,omitempty"`
}

// KiroReasoningContent 是 Kiro 后端接受的历史推理 wire 格式(嵌套,非扁平;扁平会 400)。
type KiroReasoningContent struct {
	ReasoningText KiroReasoningText `json:"reasoningText"`
}

// KiroReasoningText 承载一段历史推理文本及其签名。
type KiroReasoningText struct {
	Text      string `json:"text"`
	Signature string `json:"signature"`
}

type KiroToolUse struct {
	ToolUseID string                 `json:"toolUseId"`
	Name      string                 `json:"name"`
	Input     map[string]interface{} `json:"input"`
}

type InferenceConfig struct {
	MaxTokens   int     `json:"maxTokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	TopP        float64 `json:"topP,omitempty"`
}

// ==================== Stream Callbacks ====================

// KiroStreamCallback stream response callbacks
type KiroStreamCallback struct {
	OnText         func(text string, isThinking bool)
	OnToolUse      func(toolUse KiroToolUse)
	OnComplete     func(inputTokens, outputTokens int)
	OnError        func(err error)
	OnCredits      func(credits float64)
	OnContextUsage func(percentage float64)
	// OnReasoningSignature fires when the model emits a native reasoning
	// signature (reasoningContentEvent.signature, usually once at reasoning end).
	// The signature must be relayed to the client as a signature_delta so the
	// client can send it back on the next turn; Kiro rejects unsigned thinking.
	OnReasoningSignature func(signature string)
	// OnCacheMetering fires once at stream end when the upstream meteringEvent
	// carried prompt-cache token fields (cacheReadInputTokens /
	// cacheWriteInputTokens). These are the upstream truth: when present they
	// take precedence over the local prompt-cache simulation.
	OnCacheMetering func(readTokens, creationTokens int)
}

// ==================== API Call ====================

func setPayloadProfileArnForAccount(payload *KiroPayload, account *config.Account) {
	if payload == nil {
		return
	}

	payload.ProfileArn = strings.TrimSpace(payload.ProfileArn)
	if account != nil {
		if profileArn := strings.TrimSpace(account.ProfileArn); profileArn != "" {
			payload.ProfileArn = profileArn
		}
	}
}

// getSortedEndpoints returns endpoints ordered by user preference, with optional fallback.
func getSortedEndpoints(preferred string) []kiroEndpoint {
	fallback := config.GetEndpointFallback()

	var primary int
	switch preferred {
	case "kiro":
		primary = 0
	case "codewhisperer":
		primary = 1
	case "amazonq":
		primary = 2
	default:
		// "auto": Kiro first, then fallback to others
		return []kiroEndpoint{kiroEndpoints[0], kiroEndpoints[1], kiroEndpoints[2]}
	}

	if !fallback {
		// No fallback: only use the selected endpoint
		return []kiroEndpoint{kiroEndpoints[primary]}
	}

	// With fallback: selected first, then others in order
	result := []kiroEndpoint{kiroEndpoints[primary]}
	for i, ep := range kiroEndpoints {
		if i != primary {
			result = append(result, ep)
		}
	}
	return result
}

// CallKiroAPI calls the Kiro streaming API, trying each configured endpoint with
// automatic fallback. ctx ties the upstream request lifetime to the caller
// (typically the client's r.Context()): when the client disconnects, ctx is
// cancelled, the in-flight upstream request is aborted, its slot released and no
// success is recorded (a cancelled stream returns an error, so the handler's
// failure path runs and never bills). ctx also carries the per-read idle
// deadline (see idleTimeoutReader).
func CallKiroAPI(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	if ctx == nil {
		ctx = context.Background()
	}
	originalProfileArn := ""
	if payload != nil {
		originalProfileArn = payload.ProfileArn
		defer func() {
			payload.ProfileArn = originalProfileArn
		}()
	}
	setPayloadProfileArnForAccount(payload, account)

	// Payload serialization now happens once per endpoint attempt below (its error
	// is the validation, and DEBUG logging reuses that same bytes). This drops the
	// previous 3x marshal (standalone validate + unconditional debug dump + body)
	// to a single marshal on the common single-endpoint path.
	debugPayload := logger.GetLevel() <= logger.LevelDebug

	// Wrap OnToolUse to restore original tool names for the client.
	if callback != nil && callback.OnToolUse != nil && len(payload.ToolNameMap) > 0 {
		originalOnToolUse := callback.OnToolUse
		nameMap := payload.ToolNameMap
		wrapped := *callback
		wrapped.OnToolUse = func(tu KiroToolUse) {
			if original, ok := nameMap[tu.Name]; ok {
				tu.Name = original
			}
			originalOnToolUse(tu)
		}
		callback = &wrapped
	}

	if payload != nil && strings.TrimSpace(payload.ProfileArn) == "" {
		if profileArn, err := ResolveProfileArn(account); err == nil {
			payload.ProfileArn = profileArn
		} else if isProfileArnResolutionSoftError(err) {
			logger.Debugf("[ProfileArn] Skipped profile ARN resolution for %s: %v", accountEmailForLog(account), err)
		} else {
			logger.Warnf("[ProfileArn] Failed to resolve profile ARN for %s: %v", accountEmailForLog(account), err)
		}
	}

	// Build endpoint list. external_idp(Microsoft Entra / Kiro 企业版)账号走 Kiro 数据面
	// runtime.{region}.kiro.dev 单端点(其 Bearer 是客户 IdP 直签 token,不能走 AWS 直连,
	// 也无 AWS 三端点回退);其余账号(idc/social/api_key)走 AWS q.{region}.amazonaws.com 并按配置回退。
	var endpoints []kiroEndpoint
	if auth.IsExternalIdpAccount(account) {
		endpoints = []kiroEndpoint{{
			URL:    "https://" + auth.ExternalIdpRuntimeHost(account.Region) + "/generateAssistantResponse",
			Origin: "AI_EDITOR",
			Name:   "ExternalIdP",
		}}
	} else {
		endpoints = getSortedEndpoints(config.GetPreferredEndpoint())
	}

	var lastErr error
	for _, ep := range endpoints {
		// Update the origin field for the selected endpoint.
		payload.ConversationState.CurrentMessage.UserInputMessage.Origin = ep.Origin

		// Target the profile's data-plane region; endpoint URLs are declared for us-east-1.
		epURL := regionalizeURLForProfile(ep.URL, account, payload.ProfileArn)
		reqBody, mErr := json.Marshal(payload)
		if mErr != nil {
			lastErr = mErr
			continue
		}
		if debugPayload {
			logger.Debugf("[KiroAPI] Request payload: %s", string(reqBody))
		}

		// Per-endpoint attempt in a closure so the derived cancel is always
		// released (defer), whether we fail fast or stream to completion.
		terminal, err := func() (terminal bool, err error) {
			reqCtx, cancel := context.WithCancel(ctx)
			defer cancel()

			req, err := http.NewRequestWithContext(reqCtx, "POST", epURL, bytes.NewReader(reqBody))
			if err != nil {
				return false, err
			}

			host := ""
			if parsedURL, parseErr := url.Parse(epURL); parseErr == nil {
				host = parsedURL.Host
			}
			headerValues := buildStreamingHeaderValues(account, host)

			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "*/*")
			if ep.AmzTarget != "" {
				req.Header.Set("X-Amz-Target", ep.AmzTarget)
			}
			applyKiroBaseHeaders(req, account, headerValues)
			req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
			req.Header.Set("x-amzn-codewhisperer-optout", "true")
			req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
			req.Header.Set("Amz-Sdk-Invocation-Id", uuid.New().String())

			resp, err := GetClientForProxy(ResolveAccountProxyURL(account)).Do(req)
			if err != nil {
				logger.Warnf("[KiroAPI] Endpoint %s failed: %v", ep.Name, err)
				return false, err
			}

			if resp.StatusCode == 429 {
				resp.Body.Close()
				logger.Warnf("[KiroAPI] Endpoint %s quota exhausted (429), trying next...", ep.Name)
				return false, fmt.Errorf("quota exhausted on %s", ep.Name)
			}

			if resp.StatusCode != 200 {
				errBody, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				e := fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, ep.Name, string(errBody))
				// Authentication and payment errors are not retried across endpoints.
				if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 402 {
					return true, e
				}
				logger.Warnf("[KiroAPI] Endpoint %s error: %v", ep.Name, e)
				return false, e
			}

			// Success: stream with a per-read idle deadline. On idle, cancel
			// aborts the upstream request so the blocked Read returns an error.
			body := newIdleTimeoutReader(resp.Body, streamIdleTimeout, cancel)
			defer body.Close()
			return true, parseEventStream(reqCtx, body, callback)
		}()

		if terminal {
			return err
		}
		lastErr = err
	}

	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("all endpoints failed")
}

func accountEmailForLog(account *config.Account) string {
	if account == nil {
		return "<nil>"
	}
	return account.Email
}

// ==================== Event Stream Parsing ====================

// parseEventStream decodes an AWS binary Event Stream response body. ctx lets a
// read error be reported as a clean cancellation (client disconnect or idle
// timeout) instead of a raw transport error; either way an error return means
// the handler's failure path runs and the request is NOT billed.
func parseEventStream(ctx context.Context, body io.Reader, callback *KiroStreamCallback) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if callback == nil {
		callback = &KiroStreamCallback{}
	}

	// Read directly without bufio to avoid buffering latency in streaming responses.
	var inputTokens, outputTokens int
	var totalCredits float64
	var currentToolUse *toolUseState
	var lastAssistantContent string
	var lastReasoningContent string
	var meteringCacheRead, meteringCacheCreation int
	var hasCacheMetering bool

	// Reused across frames so a long stream doesn't allocate a fresh prelude +
	// message buffer per event.
	prelude := make([]byte, 12)
	var msgBuf []byte

	for {
		// Prelude: 12 bytes (total_len + headers_len + crc)
		_, err := io.ReadFull(body, prelude)
		if err == io.EOF {
			break
		}
		if err != nil {
			// A cancelled context (client disconnect / idle timeout) surfaces as
			// a read error; report it as the context error so the caller can see
			// the interruption clearly. Not billed either way (early return skips
			// OnCredits/OnComplete below).
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		}

		totalLength := int(prelude[0])<<24 | int(prelude[1])<<16 | int(prelude[2])<<8 | int(prelude[3])
		headersLength := int(prelude[4])<<24 | int(prelude[5])<<16 | int(prelude[6])<<8 | int(prelude[7])

		if totalLength < 16 {
			continue
		}
		// Cap a single frame's declared size: the 4-byte totalLength is attacker/
		// corruption-influenced, and make([]byte, totalLength-12) on a bogus large
		// value would try to allocate up to ~4 GiB and OOM the process. Legit Kiro
		// frames are far below 16 MiB.
		if totalLength > maxEventFrameBytes {
			return fmt.Errorf("event stream frame too large: %d bytes (cap %d)", totalLength, maxEventFrameBytes)
		}

		// Read the remaining message bytes into the reused buffer (grown on demand).
		remaining := totalLength - 12
		if cap(msgBuf) < remaining {
			msgBuf = make([]byte, remaining)
		} else {
			msgBuf = msgBuf[:remaining]
		}
		_, err = io.ReadFull(body, msgBuf)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		}

		if headersLength > len(msgBuf)-4 {
			continue
		}

		eventType := extractEventType(msgBuf[0:headersLength])
		payloadBytes := msgBuf[headersLength : len(msgBuf)-4]
		if len(payloadBytes) == 0 {
			continue
		}

		var event map[string]interface{}
		if err := json.Unmarshal(payloadBytes, &event); err != nil {
			continue
		}

		inputTokens, outputTokens = updateTokensFromEvent(event, inputTokens, outputTokens)

		// Dispatch by event type.
		switch eventType {
		case "assistantResponseEvent":
			if content, ok := event["content"].(string); ok && content != "" {
				normalized := normalizeChunk(content, &lastAssistantContent)
				if normalized != "" && callback.OnText != nil {
					callback.OnText(normalized, false)
				}
			}
		case "reasoningContentEvent":
			if text, ok := event["text"].(string); ok && text != "" {
				normalized := normalizeChunk(text, &lastReasoningContent)
				if normalized != "" && callback.OnText != nil {
					callback.OnText(normalized, true)
				}
			}
			// Native reasoning signature (opus-4.8 等原生思考模型在推理结束时下发一次)。
			// 必须收集并透传给客户端,否则思考签名丢失、下一轮无法带回、Kiro 拒收。
			if sig, ok := event["signature"].(string); ok && sig != "" {
				if callback.OnReasoningSignature != nil {
					callback.OnReasoningSignature(sig)
				}
			}
		case "toolUseEvent":
			currentToolUse = handleToolUseEvent(event, currentToolUse, callback)
		case "meteringEvent":
			if usage, ok := event["usage"].(float64); ok {
				totalCredits += usage
			}
			// Upstream prompt-cache truth, when Kiro chooses to transmit it.
			// (Rust 侧实测 2026-07 版本不透传；这里保留探测 + INFO 日志,一旦
			// 上游开始返回就自动切到真值口径。)
			if read, creation, ok := extractCacheMeteringFromEvent(event); ok {
				meteringCacheRead, meteringCacheCreation = read, creation
				hasCacheMetering = true
				logger.Infof("[Metering] upstream cache fields present: read=%d creation=%d", read, creation)
			}
		case "contextUsageEvent":
			if pct, ok := event["contextUsagePercentage"].(float64); ok {
				if callback.OnContextUsage != nil {
					callback.OnContextUsage(pct)
				}
			}
		}
	}

	if currentToolUse != nil {
		finishToolUse(currentToolUse, callback)
	}

	if callback.OnCredits != nil && totalCredits > 0 {
		callback.OnCredits(totalCredits)
	}

	// Only surface upstream cache metering when it carries information: an
	// explicit all-zero pair is indistinguishable from "not implemented" and
	// must not silence the local simulation.
	if callback.OnCacheMetering != nil && hasCacheMetering && (meteringCacheRead > 0 || meteringCacheCreation > 0) {
		callback.OnCacheMetering(meteringCacheRead, meteringCacheCreation)
	}

	if callback.OnComplete != nil {
		callback.OnComplete(inputTokens, outputTokens)
	}
	return nil
}

// extractCacheMeteringFromEvent looks for prompt-cache token fields on a
// meteringEvent payload (flat or inside any nested usage map). ok is true when
// at least one of the fields is present; an absent field reads as 0.
func extractCacheMeteringFromEvent(event map[string]interface{}) (readTokens, creationTokens int, ok bool) {
	candidates := []map[string]interface{}{event}
	collectUsageMaps(event, &candidates)
	for _, m := range candidates {
		if m == nil {
			continue
		}
		read, readOK := readTokenNumber(m, "cacheReadInputTokens", "cache_read_input_tokens")
		creation, creationOK := readTokenNumber(m, "cacheWriteInputTokens", "cache_write_input_tokens", "cacheCreationInputTokens", "cache_creation_input_tokens")
		if readOK || creationOK {
			return read, creation, true
		}
	}
	return 0, 0, false
}

func updateTokensFromEvent(event map[string]interface{}, currentInputTokens, currentOutputTokens int) (int, int) {
	candidates := []map[string]interface{}{event}
	collectUsageMaps(event, &candidates)

	inputTokens := currentInputTokens
	outputTokens := currentOutputTokens

	for _, usage := range candidates {
		if usage == nil {
			continue
		}

		if v, ok := readTokenNumber(usage,
			"outputTokens", "completionTokens", "totalOutputTokens",
			"output_tokens", "completion_tokens", "total_output_tokens",
		); ok {
			outputTokens = v
		}

		if v, ok := readTokenNumber(usage,
			"inputTokens", "promptTokens", "totalInputTokens",
			"input_tokens", "prompt_tokens", "total_input_tokens",
		); ok {
			inputTokens = v
			continue
		}

		uncached, _ := readTokenNumber(usage, "uncachedInputTokens", "uncached_input_tokens")
		cacheRead, _ := readTokenNumber(usage, "cacheReadInputTokens", "cache_read_input_tokens")
		cacheWrite, _ := readTokenNumber(usage, "cacheWriteInputTokens", "cache_write_input_tokens", "cacheCreationInputTokens", "cache_creation_input_tokens")
		if uncached+cacheRead+cacheWrite > 0 {
			inputTokens = uncached + cacheRead + cacheWrite
			continue
		}

		total, ok := readTokenNumber(usage, "totalTokens", "total_tokens")
		if ok && total > 0 {
			candidateOutput := outputTokens
			if v, vok := readTokenNumber(usage,
				"outputTokens", "completionTokens", "totalOutputTokens",
				"output_tokens", "completion_tokens", "total_output_tokens",
			); vok {
				candidateOutput = v
			}
			if total-candidateOutput > 0 {
				inputTokens = total - candidateOutput
			}
		}
	}

	return inputTokens, outputTokens
}

// getContextWindowSize returns the context window size (in tokens) for a model.
//
// Per Kiro's ListAvailableModels, the 1M-token context window applies to
// Claude 4.6 and newer (sonnet-4.6, opus-4.6, opus-4.7, opus-4.8, and future
// 4.x releases), while 4.5 and earlier (opus-4.5, sonnet-4.5, sonnet-4,
// haiku-4.5) use a 200K window. This value is used to convert the upstream
// contextUsagePercentage into an absolute input-token count that clients rely
// on to decide when to compact; an undersized window under-reports tokens and
// prevents clients from compacting in time.
func getContextWindowSize(model string) int {
	if isLargeContextModel(model) {
		return 1_000_000
	}
	return 200_000
}

// largeContextMinor matches "claude-<family>-<major>.<minor>" (dot or dash form)
// and is used to classify 1M-window models by version.
var claudeVersionExtractor = regexp.MustCompile(`claude-(?:opus|sonnet|haiku)-(\d+)[.-](\d+)`)

func isLargeContextModel(model string) bool {
	m := strings.ToLower(model)
	if match := claudeVersionExtractor.FindStringSubmatch(m); match != nil {
		major, errMaj := strconv.Atoi(match[1])
		minor, errMin := strconv.Atoi(match[2])
		if errMaj == nil && errMin == nil {
			// 1M window for Claude >= 4.6 (4.6, 4.7, 4.8, ...) and any major >= 5.
			if major > 4 {
				return true
			}
			if major == 4 && minor >= 6 {
				return true
			}
			return false
		}
	}
	// Fallback substring checks for non-standard identifiers.
	for _, tag := range []string{"4.6", "4-6", "4.7", "4-7", "4.8", "4-8", "4.9", "4-9"} {
		if strings.Contains(m, tag) {
			return true
		}
	}
	return false
}

func collectUsageMaps(v interface{}, out *[]map[string]interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			lk := strings.ToLower(k)
			if lk == "usage" || lk == "tokenusage" || lk == "token_usage" {
				if m, ok := child.(map[string]interface{}); ok {
					*out = append(*out, m)
				}
			}
			collectUsageMaps(child, out)
		}
	case []interface{}:
		for _, child := range t {
			collectUsageMaps(child, out)
		}
	}
}

func normalizeChunk(chunk string, previous *string) string {
	if chunk == "" {
		return ""
	}

	prev := *previous
	if prev == "" {
		*previous = chunk
		return chunk
	}

	if chunk == prev {
		return ""
	}

	if strings.HasPrefix(chunk, prev) {
		delta := chunk[len(prev):]
		*previous = chunk
		return delta
	}

	if strings.HasPrefix(prev, chunk) {
		return ""
	}

	maxOverlap := 0
	maxLen := len(prev)
	if len(chunk) < maxLen {
		maxLen = len(chunk)
	}
	for i := maxLen; i > 0; i-- {
		if strings.HasSuffix(prev, chunk[:i]) {
			maxOverlap = i
			break
		}
	}

	*previous = chunk
	if maxOverlap > 0 {
		return chunk[maxOverlap:]
	}

	return chunk
}

func readTokenNumber(m map[string]interface{}, keys ...string) (int, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return int(n), true
		case int:
			return n, true
		case int64:
			return int(n), true
		case json.Number:
			if parsed, err := n.Int64(); err == nil {
				return int(parsed), true
			}
		case string:
			if parsed, err := strconv.Atoi(n); err == nil {
				return parsed, true
			}
			if parsed, err := strconv.ParseFloat(n, 64); err == nil {
				return int(parsed), true
			}
		}
	}
	return 0, false
}

// ==================== Tool Use Handling ====================

type toolUseState struct {
	ToolUseID   string
	Name        string
	InputBuffer strings.Builder
	GeneratedID bool
}

func handleToolUseEvent(event map[string]interface{}, current *toolUseState, callback *KiroStreamCallback) *toolUseState {
	toolUseID := firstStringField(event, "toolUseId", "toolUseID", "tool_use_id", "id")
	name := firstStringField(event, "name", "toolName", "tool_name")
	isStop := firstBoolField(event, "stop", "isStop", "done")

	if toolUseID != "" && name != "" {
		if current == nil {
			current = &toolUseState{ToolUseID: toolUseID, Name: name}
		} else if current.ToolUseID != toolUseID {
			if current.GeneratedID && current.Name == name {
				current.ToolUseID = toolUseID
				current.GeneratedID = false
			} else {
				finishToolUse(current, callback)
				current = &toolUseState{ToolUseID: toolUseID, Name: name}
			}
		}
	} else if name != "" && current == nil {
		current = &toolUseState{ToolUseID: "toolu_" + uuid.New().String(), Name: name, GeneratedID: true}
	} else if name != "" && current != nil && current.Name != name {
		finishToolUse(current, callback)
		current = &toolUseState{ToolUseID: "toolu_" + uuid.New().String(), Name: name, GeneratedID: true}
	}

	if current != nil {
		if input, ok := event["input"].(string); ok {
			current.InputBuffer.WriteString(input)
		} else if inputObj, ok := event["input"].(map[string]interface{}); ok {
			data, _ := json.Marshal(inputObj)
			current.InputBuffer.Reset()
			current.InputBuffer.Write(data)
		}
	}

	if isStop && current != nil {
		finishToolUse(current, callback)
		return nil
	}

	return current
}

func finishToolUse(state *toolUseState, callback *KiroStreamCallback) {
	if state == nil || state.Name == "" || callback == nil || callback.OnToolUse == nil {
		return
	}
	if state.ToolUseID == "" {
		state.ToolUseID = "toolu_" + uuid.New().String()
	}
	var input map[string]interface{}
	if state.InputBuffer.Len() > 0 {
		json.Unmarshal([]byte(state.InputBuffer.String()), &input)
	}
	if input == nil {
		input = make(map[string]interface{})
	}
	callback.OnToolUse(KiroToolUse{
		ToolUseID: state.ToolUseID,
		Name:      state.Name,
		Input:     input,
	})
}

// FakeSignatureMarker 是兜底伪造签名的前缀标记。
// 当模型未下发真实签名但流中产生了 thinking 块时,用带此标记的伪造签名占位透传给客户端。
// 下一轮请求把历史 thinking 带回时,请求侧据此前缀识别并剥离伪造签名——绝不能把伪造签名
// 发回 Kiro,否则触发 THINKING_SIGNATURE_INVALID(400)。真实签名不带此标记,正常回传。
const FakeSignatureMarker = "kirogofakesig"

// LegacyRustFakeSignatureMarker 是旧 Rust 反代(kiro2cc-proxy)兜底伪造签名的前缀。
// 从 Rust 反代迁移过来的老会话,历史 thinking 块里带的正是这个前缀的假签名。走 kirogo 时
// 若不识别,会被当真签名发回 AWS → THINKING_SIGNATURE_INVALID(400) → SelfHeal 触发,把
// reasoningContent + additionalModelRequestFields(含 effort)一并剥掉重试,导致该轮不思考。
// 必须一并识别并剥离。来源:Rust stream.rs FAKE_SIGNATURE_MARKER。
const LegacyRustFakeSignatureMarker = "FAKESIGk2ccPROXY"

// generateFakeSignature 生成兜底思考签名(带 FakeSignatureMarker 前缀)。
// Anthropic 客户端要求 thinking 块的 signature_delta 总长度 >= 100 字符才通过校验;
// 当模型未下发真实签名(native_signature 缺失)但流中确实产生了 thinking 块时,
// 用此兜底签名占位,避免客户端因签名缺失/过短而报错。
func generateFakeSignature() string {
	var sb strings.Builder
	sb.WriteString(FakeSignatureMarker)
	for sb.Len() < 120 {
		sb.WriteString(strings.ReplaceAll(uuid.New().String(), "-", ""))
	}
	return sb.String()
}

// isFakeSignature 判断签名是否为兜底伪造(本代理 FakeSignatureMarker 前缀,或旧 Rust
// 反代 LegacyRustFakeSignatureMarker 前缀)。请求侧回传历史推理时用它剥离伪造签名,
// 避免把无效签名发回 Kiro 触发 THINKING_SIGNATURE_INVALID(400)。真实模型签名不带任何
// 已知前缀,正常回传。
func isFakeSignature(sig string) bool {
	return strings.HasPrefix(sig, FakeSignatureMarker) ||
		strings.HasPrefix(sig, LegacyRustFakeSignatureMarker)
}

func firstStringField(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func firstBoolField(m map[string]interface{}, keys ...string) bool {
	for _, key := range keys {
		if v, ok := m[key].(bool); ok {
			return v
		}
	}
	return false
}

// extractEventType extracts the event type string from AWS Event Stream message headers.
func extractEventType(headers []byte) string {
	offset := 0
	for offset < len(headers) {
		if offset >= len(headers) {
			break
		}
		nameLen := int(headers[offset])
		offset++
		if offset+nameLen > len(headers) {
			break
		}
		name := string(headers[offset : offset+nameLen])
		offset += nameLen
		if offset >= len(headers) {
			break
		}
		valueType := headers[offset]
		offset++

		if valueType == 7 { // String
			if offset+2 > len(headers) {
				break
			}
			valueLen := int(headers[offset])<<8 | int(headers[offset+1])
			offset += 2
			if offset+valueLen > len(headers) {
				break
			}
			value := string(headers[offset : offset+valueLen])
			offset += valueLen
			if name == ":event-type" {
				return value
			}
			continue
		}

		// Skip other value types by their fixed byte widths.
		skipSizes := map[byte]int{0: 0, 1: 0, 2: 1, 3: 2, 4: 4, 5: 8, 8: 8, 9: 16}
		if valueType == 6 {
			if offset+2 > len(headers) {
				break
			}
			l := int(headers[offset])<<8 | int(headers[offset+1])
			offset += 2 + l
		} else if skip, ok := skipSizes[valueType]; ok {
			offset += skip
		} else {
			break
		}
	}
	return ""
}
