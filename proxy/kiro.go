// Package proxy is the core proxy layer for the Kiro API.
// It handles streaming API calls to the Kiro backend and parses AWS Event Stream responses.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
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

const (
	// streamRetryBackoff spaces out a retry of a stream that died before
	// delivering any output callback. Upstream drops cluster in time, so an
	// immediate retry tends to hit the same blip.
	streamRetryBackoff           = 700 * time.Millisecond
	maxStreamAttemptsPerEndpoint = 2
	maxEventStreamMessageSize    = 16 * 1024 * 1024
)

var (
	errEmptyKiroStream         = errors.New("upstream stream ended before any output")
	errIncompleteKiroToolInput = errors.New("upstream stream ended with incomplete tool input")
	errInvalidKiroEventStream  = errors.New("invalid upstream event stream")
	errKiroEventStreamUpstream = errors.New("upstream event stream error")
	streamRetryWait            = waitForStreamRetry
	resolveKiroEndpoints       = endpointsForAccount
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

// kiroCLIEndpoint is the headless / API Key path used by Kiro CLI:
// POST https://runtime.{region}.kiro.dev/ with AWS JSON 1.0 protocol.
var kiroCLIEndpoint = kiroEndpoint{
	URL:       "https://runtime.us-east-1.kiro.dev/",
	Origin:    "KIRO_CLI",
	AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
	Name:      "Kiro CLI",
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

// How hard to retry when every endpoint reports an upstream capacity problem
// (HTTP 500 MODEL_TEMPORARILY_UNAVAILABLE / "please try again"). Kept small on
// purpose: the point is to ride out a brief model hiccup, not to hide a sustained
// outage from the client. Two retries means at most three endpoint cycles, and the
// backoff is linear (1.2 s, then 2.4 s), so the worst case adds ~3.6 s before the
// client is told. Waiting also respects the request context, so a client that has
// already disconnected releases its account slot immediately.
//
// Override is deliberately not exposed as an env var: unlike the resize budget
// these numbers are a property of the upstream's behaviour, not of the host.
const (
	upstreamOverloadRetries = 2
	upstreamOverloadBackoff = 1200 * time.Millisecond
)

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
	// 嵌套 wire format {reasoningText:{text,signature}};仅当带模型真实签名且该签名由**当前
	// 服务账号**产出时才发(见 signature_provenance.go)——签名跨账号回放必 400
	// (THINKING_SIGNATURE_INVALID)。ClaudeToKiro 只填下面两个 transient 候选字段;真正是否
	// 下发由 applyThinkingProvenance(payload, 选中账号) 在选号后裁决。
	ReasoningContent *KiroReasoningContent `json:"reasoningContent,omitempty"`

	// ---- 以下 transient 字段不参与序列化(json:"-"),仅承载"换号剥 thinking"的证据 ----
	// ReasoningCandidate 是候选历史推理块(已解包出真实签名);applyThinkingProvenance 依据
	// ReasoningProducer 与当前账号是否一致,决定把它赋给 ReasoningContent(保留)还是置 nil(剥离)。
	ReasoningCandidate *KiroReasoningContent `json:"-"`
	// ReasoningProducer 是该历史推理签名的产出账号 token(accountSignatureToken)。空串=来源未知
	// (无我方 provenance 标记的外来/历史签名)——一律按剥离处理。
	ReasoningProducer string `json:"-"`
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
	// OnStreamStart fires as soon as an upstream endpoint returned 2xx and the
	// event stream is about to be consumed — i.e. this request WILL stream (or
	// die trying). Streaming handlers use it to flush response headers early
	// and start the SSE keepalive before the first content event, killing the
	// pre-first-token silent window (CF 524). May fire again on a later
	// account/self-heal retry, so it must be idempotent. Synchronous, on the
	// handler goroutine.
	OnStreamStart func()
	OnText        func(text string, isThinking bool)
	OnToolUse     func(toolUse KiroToolUse)
	OnComplete    func(inputTokens, outputTokens int)
	OnError       func(err error)
	// ToolSchemas maps the WIRE tool name (post ToolNameMap) to its declared
	// input schema. Populated by CallKiroAPI from the payload; finishToolUse
	// uses it to coerce stringified argument values back to their declared
	// types before they reach the client (see tool_input_coerce.go).
	ToolSchemas map[string]map[string]interface{}
	// OnException fires on a mid-stream `exception` frame that is NOT a hard
	// failure — specifically the model hitting its output-token cap
	// (ContentLengthExceededException). The content streamed so far is valid but
	// truncated; handlers use this to report stop_reason=max_tokens (Claude) /
	// finish_reason=length (OpenAI) instead of a normal end_turn/stop, so agentic
	// clients know the answer was cut short rather than genuinely finished.
	OnException    func(exceptionType, message string)
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
	OnStopReason    func(reason string)
}

// ==================== API Call ====================

func setPayloadProfileArnForAccount(payload *KiroPayload, account *config.Account) {
	if payload == nil {
		return
	}

	// API Key credentials must not carry IDE/profile semantics.
	if config.IsAPIKeyAccount(account) {
		payload.ProfileArn = ""
		return
	}

	payload.ProfileArn = strings.TrimSpace(payload.ProfileArn)
	if account != nil {
		if profileArn := strings.TrimSpace(account.ProfileArn); profileArn != "" {
			payload.ProfileArn = profileArn
		}
	}
}

// endpointsForAccount returns the upstream endpoint list for a credential.
// API Key accounts always use the CLI runtime protocol; OAuth accounts keep
// the configured preferred-endpoint fallback chain.
func endpointsForAccount(account *config.Account) []kiroEndpoint {
	if config.IsAPIKeyAccount(account) {
		return []kiroEndpoint{kiroCLIEndpoint}
	}
	return getSortedEndpoints(config.GetPreferredEndpoint())
}

// cliRuntimeURL builds the regional Kiro CLI runtime URL.
func cliRuntimeURL(account *config.Account) string {
	region := "us-east-1"
	if account != nil {
		if r := strings.TrimSpace(account.Region); r != "" {
			region = r
		}
	}
	return fmt.Sprintf("https://runtime.%s.kiro.dev/", region)
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
	if callback != nil && callback.ToolSchemas == nil && payload != nil {
		var kiroTools []KiroToolWrapper
		if ctx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext; ctx != nil {
			kiroTools = ctx.Tools
		}
		if len(kiroTools) > 0 {
			m := make(map[string]map[string]interface{}, len(kiroTools))
			for i := range kiroTools {
				spec := &kiroTools[i].ToolSpecification
				if m[spec.Name] == nil {
					if sm, ok := spec.InputSchema.JSON.(map[string]interface{}); ok {
						m[spec.Name] = sm
					}
				}
			}
			callback.ToolSchemas = m
		}
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

	if payload != nil && strings.TrimSpace(payload.ProfileArn) == "" && !config.IsAPIKeyAccount(account) {
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
	// 也无 AWS 三端点回退);API Key 账号由 resolveKiroEndpoints 路由到 Kiro CLI runtime
	// 协议单端点;其余账号(idc/social)走 AWS q.{region}.amazonaws.com 并按配置回退。
	var endpoints []kiroEndpoint
	if auth.IsExternalIdpAccount(account) {
		endpoints = []kiroEndpoint{{
			URL:    "https://" + auth.ExternalIdpRuntimeHost(account.Region) + "/generateAssistantResponse",
			Origin: "AI_EDITOR",
			Name:   "ExternalIdP",
		}}
	} else {
		endpoints = resolveKiroEndpoints(account)
	}
	isAPIKey := config.IsAPIKeyAccount(account)

	// Retry the WHOLE endpoint cycle — not the next endpoint — when the upstream
	// reports a capacity problem. The three endpoints are separate API surfaces in
	// front of the same model backend, so rotating between them inside a few
	// milliseconds cannot help: production showed all three answering HTTP 500
	// MODEL_TEMPORARILY_UNAVAILABLE for the same request, after which the client
	// got "empty upstream response" (286 times in 24 h) even though the upstream
	// had asked it to try again. Waiting a moment and re-running the cycle is what
	// it asks for. Bounded, because an overload that outlives a few seconds should
	// surface to the client rather than be hidden behind unbounded retries.
	var lastErr error
	for cycle := 0; ; cycle++ {
		lastErr = nil
		for epIndex, ep := range endpoints {
			// Update the origin field for the selected endpoint.
			payload.ConversationState.CurrentMessage.UserInputMessage.Origin = ep.Origin

			// Target the profile's data-plane region; endpoint URLs are declared for
			// us-east-1. API Key accounts use the CLI runtime host instead of IDE/Q
			// hosts.
			epURL := regionalizeURLForProfile(ep.URL, account, payload.ProfileArn)
			if isAPIKey {
				epURL = cliRuntimeURL(account)
			}

			reqBody, mErr := json.Marshal(payload)
			if mErr != nil {
				lastErr = mErr
				continue
			}
			if debugPayload {
				logger.Debugf("[KiroAPI] Request payload: %s", string(reqBody))
			}

			host := ""
			if parsedURL, parseErr := url.Parse(epURL); parseErr == nil {
				host = parsedURL.Host
			}
			headerValues := buildStreamingHeaderValues(account, host)
			// One invocation id per endpoint, shared across the stream attempts below.
			invocationID := uuid.New().String()

			// Per-endpoint attempt in a closure so the derived cancel is always
			// released (defer), whether we fail fast or stream to completion.
			// A stream that died before any output reached the client (#143) is
			// retried on the SAME endpoint, bounded by maxStreamAttemptsPerEndpoint,
			// before the loop falls back to the next endpoint.
		var terminal, retrySame bool
		var err error
		for streamAttempt := 1; ; streamAttempt++ {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			terminal, retrySame, err = func() (terminal, retrySame bool, err error) {
				reqCtx, cancel := context.WithCancel(ctx)
				defer cancel()

				// Requests and bodies cannot be reused after an HTTP attempt.
				req, err := http.NewRequestWithContext(reqCtx, "POST", epURL, bytes.NewReader(reqBody))
				if err != nil {
					return false, false, err
				}

				req.Header.Set("Content-Type", "application/json")
				if isAPIKey {
					req.Header.Set("Content-Type", "application/x-amz-json-1.0")
				}
				req.Header.Set("Accept", "*/*")
				if ep.AmzTarget != "" {
					req.Header.Set("X-Amz-Target", ep.AmzTarget)
				}
				applyKiroBaseHeaders(req, account, headerValues)
				if !isAPIKey {
					req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
				}
				// CLI captures use optout=false; IDE path keeps true.
				if isAPIKey {
					req.Header.Set("x-amzn-codewhisperer-optout", "false")
				} else {
					req.Header.Set("x-amzn-codewhisperer-optout", "true")
				}
				req.Header.Set("Amz-Sdk-Request", fmt.Sprintf("attempt=%d; max=%d", streamAttempt, maxStreamAttemptsPerEndpoint))
				req.Header.Set("Amz-Sdk-Invocation-Id", invocationID)

				resp, err := GetClientForProxy(ResolveAccountProxyURL(account)).Do(req)
				if err != nil {
					logger.Warnf("[KiroAPI] Endpoint %s failed: %v", ep.Name, err)
					// A deadline that expired before the request could complete is
					// terminal: the caller's budget is gone, falling back to another
					// endpoint cannot help.
					if !isRetryableStreamError(err) {
						return true, false, err
					}
					return false, false, err
				}

					if resp.StatusCode == 429 {
						resp.Body.Close()
						logger.Warnf("[KiroAPI] Endpoint %s quota exhausted (429), trying next...", ep.Name)
						return false, false, fmt.Errorf("quota exhausted on %s", ep.Name)
					}

					if resp.StatusCode != 200 {
						errBody, _ := io.ReadAll(resp.Body)
						resp.Body.Close()
						e := fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, ep.Name, string(errBody))
						// Authentication and payment errors are not retried across endpoints.
						if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 402 {
							return true, false, e
						}
						// Neither are rejections of the payload itself: every endpoint will
						// reject the same bytes, so falling back only burns round-trips before
						// the caller's self-heal / error path can even run. Logged with the
						// serialized request size, which is the one number needed to tell an
						// oversized-input 400 apart from a malformed-payload 400.
						if isRequestShapeErrorMessage(e.Error()) {
							logger.Warnf("[KiroAPI] Endpoint %s rejected the request itself (no endpoint fallback): reqBytes=%d history=%d model=%s err=%v",
								ep.Name, len(reqBody), len(payload.ConversationState.History),
								payload.ConversationState.CurrentMessage.UserInputMessage.ModelID, e)
							return true, false, e
						}
						logger.Warnf("[KiroAPI] Endpoint %s error: %v", ep.Name, e)
						return false, false, e
					}

					// The upstream stream is established: let the handler flush its
					// response headers / start its keepalive before the first content
					// event arrives.
					if callback != nil && callback.OnStreamStart != nil {
						callback.OnStreamStart()
					}

					// Success: stream with a per-read idle deadline. On idle, cancel
					// aborts the upstream request so the blocked Read returns an error.
					body := newIdleTimeoutReader(resp.Body, streamIdleTimeout, cancel)
					defer body.Close()
					emitted, perr := parseEventStream(reqCtx, body, callback)
					if perr == nil {
						return true, false, nil
					}
					// Once any output callback ran, the attempt must stand or fail as a
					// whole: retrying would duplicate caller-visible state. A dead stream
					// with zero output is the one safe retry, and only a retryable error
					// (transport blip, empty stream) qualifies - client cancellation and
					// idle timeouts surface immediately.
					if emitted || !isRetryableStreamError(perr) {
						return true, false, perr
					}
					return false, true, perr
			}()

			if terminal {
				return err
			}
			if !retrySame {
				// This endpoint failed the ordinary way (transport, quota, non-200):
				// no dead-stream wait, just fall back to the next endpoint.
				break
			}
			// A cancelled client must not trigger another wait or retry.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			hasSameEndpointRetry := streamAttempt < maxStreamAttemptsPerEndpoint
			hasEndpointFallback := epIndex+1 < len(endpoints)
			if !hasSameEndpointRetry && !hasEndpointFallback {
				return err
			}
			logger.Warnf("[KiroAPI] Endpoint %s stream died before any output (attempt %d/%d); retrying in %s: %v",
				ep.Name, streamAttempt, maxStreamAttemptsPerEndpoint, streamRetryBackoff, err)
			// Honours cancellation while waiting: a client that has already
			// gone away must not hold an account slot for another backoff.
			if waitErr := streamRetryWait(ctx, streamRetryBackoff); waitErr != nil {
				return waitErr
			}
			if !hasSameEndpointRetry {
				break // same-endpoint attempts exhausted: try the next endpoint
			}
		}
		lastErr = err
		}

		if lastErr == nil || cycle >= upstreamOverloadRetries || !isUpstreamOverloadErrorMessage(lastErr.Error()) {
			break
		}
		wait := upstreamOverloadBackoff * time.Duration(cycle+1)
		logger.Warnf("[KiroAPI] all endpoints reported upstream capacity trouble; retrying the cycle in %s (attempt %d/%d): %v",
			wait, cycle+2, upstreamOverloadRetries+1, lastErr)
		// Honour cancellation while waiting: a client that has already gone away must
		// not hold an account slot for another second and a half.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}

	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("all endpoints failed")
}

// CallKiroAPIContext is CallKiroAPI under the upstream-facing name. The stream
// integrity retry (runKiroWithIntegrityRetry) and the handlers call it; the
// context-first signature is the single entry point for both.
func CallKiroAPIContext(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	return CallKiroAPI(ctx, account, payload, callback)
}

func isRetryableStreamError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	return !errors.As(err, &netErr) || !netErr.Timeout()
}

func accountEmailForLog(account *config.Account) string {
	if account == nil {
		return "<nil>"
	}
	return account.Email
}

func waitForStreamRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ==================== Event Stream Parsing ====================
// parseEventStream decodes an AWS binary Event Stream response body. ctx lets a
// read error be reported as a clean cancellation (client disconnect or idle
// timeout) instead of a raw transport error; either way an error return means
// the handler's failure path runs and the request is NOT billed.
//
// It also reports whether any output callback ran. A failure before the first
// output callback is safe to retry (the dead-stream retry in CallKiroAPI), and
// a stream that ends without any output at all is an error, not success.
func parseEventStream(ctx context.Context, body io.Reader, callback *KiroStreamCallback) (emitted bool, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if callback == nil {
		callback = &KiroStreamCallback{}
	}

	// Read directly without bufio to avoid buffering latency in streaming responses.
	var inputTokens, outputTokens int
	var totalCredits float64
	var meteringCacheRead, meteringCacheCreation int
	var hasCacheMetering bool
	var sawOutput bool
	pending := &pendingToolUses{}

	// Track tool-use output through the callback wrapper so sawOutput/emitted
	// stay true regardless of how a tool frame arrives.
	trackedCallback := *callback
	originalOnToolUse := trackedCallback.OnToolUse
	trackedCallback.OnToolUse = func(toolUse KiroToolUse) {
		sawOutput = true
		if originalOnToolUse != nil {
			emitted = true
			originalOnToolUse(toolUse)
		}
	}
	callback = &trackedCallback

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
				return emitted, ctxErr
			}
			return emitted, err
		}

		totalLength := int(prelude[0])<<24 | int(prelude[1])<<16 | int(prelude[2])<<8 | int(prelude[3])
		headersLength := int(prelude[4])<<24 | int(prelude[5])<<16 | int(prelude[6])<<8 | int(prelude[7])

		// Lower bound on totalLength: below 16 there's no room for headers + the
		// 4-byte trailing CRC. (On a 32-bit int build the top byte could also make
		// this negative; the < 16 check rejects that too.)
		if totalLength < 16 {
			continue
		}
		// Cap a single frame's declared size: the 4-byte totalLength is attacker/
		// corruption-influenced, and make([]byte, totalLength-12) on a bogus large
		// value would try to allocate up to ~4 GiB and OOM the process. Legit Kiro
		// frames are far below 16 MiB.
		if totalLength > maxEventFrameBytes {
			return emitted, fmt.Errorf("event stream frame too large: %d bytes (cap %d)", totalLength, maxEventFrameBytes)
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
				return emitted, ctxErr
			}
			return emitted, err
		}

		// headersLength is parsed from untrusted bytes: a negative value (high bit
		// set on a 32-bit int build, or a corrupt frame) would slip past a bare
		// upper-bound check and then panic on the msgBuf[0:headersLength] slice.
		// Guard both ends before slicing.
		if headersLength < 0 || headersLength > len(msgBuf)-4 {
			continue
		}

		messageType, eventType, exceptionType, errorCode := extractFrameMeta(msgBuf[0:headersLength])
		payloadBytes := msgBuf[headersLength : len(msgBuf)-4]

		// Mid-stream error/exception frames: the Kiro backend can emit these AFTER
		// a 200 + partial stream. The event switch below only matches `event`
		// frames, so previously these were silently dropped - truncated answers
		// looked complete and transient upstream faults got billed as success with
		// no failover. Surface them here.
		if messageType == "exception" || messageType == "error" {
			kind := exceptionType
			if kind == "" {
				kind = errorCode
			}
			msg := extractFrameErrorMessage(payloadBytes)
			// Output-cap truncation is NOT a failure: the streamed content is valid,
			// only cut short. Signal it so the handler reports max_tokens / length
			// and finalizes normally with the partial content already sent.
			if isOutputTruncationException(kind) {
				if callback.OnException != nil {
					callback.OnException(kind, msg)
				}
				continue
			}
			// Any other mid-stream exception/error is fatal for this attempt.
			// Returning an error runs the handler's failure path (SSE error frame if
			// the stream already committed, else account failover) - like a non-200.
			if kind == "" {
				kind = "UpstreamError"
			}
			return emitted, fmt.Errorf("%s: %s", kind, msg)
		}

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
			// Content is relayed verbatim: upstream deltas that repeat or extend
			// earlier text are legitimate model output, and collapsing them here
			// silently dropped real content from the response stream.
			if content, ok := event["content"].(string); ok && content != "" {
				sawOutput = true
				if callback.OnText != nil {
					emitted = true
					callback.OnText(content, false)
				}
			}
		case "reasoningContentEvent":
			if text, ok := event["text"].(string); ok && text != "" {
				sawOutput = true
				if callback.OnText != nil {
					emitted = true
					callback.OnText(text, true)
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
			if toolErr := handleToolUseEvent(event, pending, callback); toolErr != nil {
				return emitted, toolErr
			}
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
		case "metadataEvent":
			// stopReason rides inside metadataEvent on the wire; there is no
			// standalone stop reason event type. Its absence after content is
			// how callers detect a truncated stream.
			if reason := firstStringField(event, "stopReason", "stop_reason"); reason != "" && callback.OnStopReason != nil {
				callback.OnStopReason(reason)
			}
		}
	}

	// Flush tools that never received a stop frame, in arrival order.
	if err := pending.flushAll(callback); err != nil {
		return emitted, err
	}
	// A stream that ends without producing anything is not a success: surface it
	// so the caller can retry or fail over instead of billing an empty response.
	// Streams that carried upstream accounting (credits / token counts / prompt
	// cache metering) did real work even with no assistant output — usage-only
	// responses are a legitimate shape and must not be rejected as empty.
	if !sawOutput && totalCredits == 0 && inputTokens == 0 && outputTokens == 0 && !hasCacheMetering {
		return emitted, errEmptyKiroStream
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
	return emitted, nil
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
// 取值优先级:
//  1. Kiro ListAvailableModels 透出的 tokenLimits.maxInputTokens(权威值,新模型
//     上线即生效,见 model_registry.go)。
//  2. 按模型名的版本号推断(冷启动 / 回源失败时兜底)。
//
// This value is used to convert the upstream contextUsagePercentage into an
// absolute input-token count that clients rely on to decide when to compact; an
// undersized window under-reports tokens and prevents clients from compacting
// in time.
func getContextWindowSize(model string) int {
	if meta, ok := lookupModelMeta(model); ok && meta.maxInputTokens > 0 {
		return meta.maxInputTokens
	}
	if isLargeContextModel(model) {
		return 1_000_000
	}
	return 200_000
}

// claudeVersionExtractor matches "claude-<family>-<major>[.<minor>]" (dot or dash
// form) and is used to classify models by version.
//
// minor 是**可选**的:Claude 5 代起模型名不再带小版本号(claude-opus-5 /
// claude-sonnet-5),写死两段数字会让它们整个匹配不上而误判成小窗口模型。
// minor 限 1~2 位并加 \b 边界,避免把日期快照(claude-sonnet-4-20250514)误当小版本。
var claudeVersionExtractor = regexp.MustCompile(`claude-(?:opus|sonnet|haiku|fable|mythos)-(\d+)(?:[.-](\d{1,2}))?\b`)

// parseClaudeVersion 从模型名(客户端别名 / kiro_id / 带 -thinking 后缀均可)里解析
// Claude 版本号。无小版本号时 minor 返回 0(如 claude-opus-5 → 5, 0)。
func parseClaudeVersion(model string) (major int, minor int, ok bool) {
	match := claudeVersionExtractor.FindStringSubmatch(strings.ToLower(model))
	if match == nil {
		return 0, 0, false
	}
	major, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, 0, false
	}
	if match[2] != "" {
		if minor, err = strconv.Atoi(match[2]); err != nil {
			return 0, 0, false
		}
	}
	return major, minor, true
}

// isLargeContextModel 判断模型是否为 1M 上下文窗口。
//
// 对齐 Kiro ListAvailableModels:Claude 4.6 及更新(sonnet-4.6、opus-4.6/4.7/4.8)
// 以及 5 代及以后(claude-opus-5、claude-sonnet-5 等,无小版本号)为 1M;
// 4.5 及更早(opus-4.5、sonnet-4.5、sonnet-4、haiku-4.5)为 200K。
func isLargeContextModel(model string) bool {
	if major, minor, ok := parseClaudeVersion(model); ok {
		if major > 4 {
			return true
		}
		return major == 4 && minor >= 6
	}
	// Fallback substring checks for non-standard identifiers.
	m := strings.ToLower(model)
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

// pendingToolUses tracks the tool calls in flight for one stream. Entries are
// keyed by toolUseId so interleaved parallel frames accumulate independently,
// and `order` preserves arrival sequence: tool call order is semantic for
// clients, so a bare map range (randomised in Go) must never decide it.
type pendingToolUses struct {
	byID   map[string]*toolUseState
	order  []string
	lastID string
}

func (p *pendingToolUses) get(id string) *toolUseState {
	if p.byID == nil {
		return nil
	}
	return p.byID[id]
}

func (p *pendingToolUses) add(state *toolUseState) {
	if p.byID == nil {
		p.byID = make(map[string]*toolUseState)
	}
	p.byID[state.ToolUseID] = state
	p.order = append(p.order, state.ToolUseID)
	p.lastID = state.ToolUseID
}

// rekey moves an entry from a locally generated id to the real id from
// upstream, keeping its position in the arrival order.
func (p *pendingToolUses) rekey(state *toolUseState, newID string) {
	oldID := state.ToolUseID
	delete(p.byID, oldID)
	for i, id := range p.order {
		if id == oldID {
			p.order[i] = newID
			break
		}
	}
	state.ToolUseID = newID
	state.GeneratedID = false
	p.byID[newID] = state
	if p.lastID == oldID {
		p.lastID = newID
	}
}

func (p *pendingToolUses) remove(id string) {
	delete(p.byID, id)
	for i, existing := range p.order {
		if existing == id {
			p.order = append(p.order[:i], p.order[i+1:]...)
			break
		}
	}
	if p.lastID == id {
		p.lastID = ""
	}
}

// flushAll emits every tool still open when the stream ended, in arrival order.
// It stops at the first incomplete tool so the caller can retry the request
// rather than deliver a call with missing arguments.
func (p *pendingToolUses) flushAll(callback *KiroStreamCallback) error {
	order := p.order
	byID := p.byID
	p.byID = nil
	p.order = nil
	p.lastID = ""
	for _, id := range order {
		state := byID[id]
		if state == nil {
			continue
		}
		if err := finishToolUse(state, callback); err != nil {
			return err
		}
	}
	return nil
}

// handleToolUseEvent accumulates one toolUseEvent frame into pending.
// Frames for different toolUseIds stay open concurrently so interleaved
// parallel tool calls reassemble correctly; a fragment that omits toolUseId
// continues the most recently seen tool.
func handleToolUseEvent(event map[string]interface{}, pending *pendingToolUses, callback *KiroStreamCallback) error {
	toolUseID := firstStringField(event, "toolUseId", "toolUseID", "tool_use_id", "id")
	name := firstStringField(event, "name", "toolName", "tool_name")
	isStop := firstBoolField(event, "stop", "isStop", "done")

	var state *toolUseState

	switch {
	case toolUseID != "":
		state = pending.get(toolUseID)
		if state == nil && pending.lastID != "" {
			// Upstream may send the opening fragment without an id and only
			// reveal the real one later. Adopt the synthetic entry we opened
			// for it instead of splitting one logical call across two entries
			// (which also splits its argument JSON, so neither half parses).
			if prev := pending.get(pending.lastID); prev != nil && prev.GeneratedID && (name == "" || prev.Name == name) {
				pending.rekey(prev, toolUseID)
				state = prev
			}
		}
		if state == nil {
			if name == "" {
				// Orphan fragment: an id never seen before and no name to open with.
				return nil
			}
			state = &toolUseState{ToolUseID: toolUseID, Name: name}
			pending.add(state)
		} else {
			if name != "" && state.Name == "" {
				state.Name = name
			}
			pending.lastID = state.ToolUseID
		}
	case pending.lastID != "" && pending.get(pending.lastID) != nil:
		state = pending.get(pending.lastID)
		if name != "" && state.Name != name {
			// Name changed with no id: close the tool in flight rather than
			// appending foreign arguments to it, then open a new one.
			if err := finishToolUse(state, callback); err != nil {
				return err
			}
			pending.remove(state.ToolUseID)
			state = &toolUseState{ToolUseID: "toolu_" + uuid.New().String(), Name: name, GeneratedID: true}
			pending.add(state)
		}
	case name != "":
		state = &toolUseState{ToolUseID: "toolu_" + uuid.New().String(), Name: name, GeneratedID: true}
		pending.add(state)
	default:
		return nil
	}

	if input, ok := event["input"].(string); ok {
		state.InputBuffer.WriteString(input)
	} else if inputObj, ok := event["input"].(map[string]interface{}); ok {
		data, _ := json.Marshal(inputObj)
		state.InputBuffer.Reset()
		state.InputBuffer.Write(data)
	}

	if isStop {
		if err := finishToolUse(state, callback); err != nil {
			return err
		}
		pending.remove(state.ToolUseID)
	}
	return nil
}

func finishToolUse(state *toolUseState, callback *KiroStreamCallback) error {
	if state == nil || state.Name == "" {
		return nil
	}
	if state.ToolUseID == "" {
		state.ToolUseID = "toolu_" + uuid.New().String()
	}
	var input map[string]interface{}
	if state.InputBuffer.Len() > 0 {
		if err := json.Unmarshal([]byte(state.InputBuffer.String()), &input); err != nil {
			return fmt.Errorf("%w: %v", errIncompleteKiroToolInput, err)
		}
	}
	if input == nil {
		input = make(map[string]interface{})
	}
	// Schema-aware coercion: a model occasionally emits a non-string argument
	// as a JSON-encoded string ({"todos": "[...]"}, "limit": "25" — buyer
	// ticket 2026-09-30). Unwrap when the declared schema types the field as
	// that non-string kind; fields the schema types as string are never
	// touched. Also breaks the imitation loop: the coerced call is what the
	// client validates, replays and the model next sees.
	if callback != nil && len(callback.ToolSchemas) > 0 {
		if ps := callback.ToolSchemas[state.Name]; ps != nil {
			before, _ := json.Marshal(input)
			input = coerceInputToSchema(input, ps, 0)
			after, _ := json.Marshal(input)
			if string(before) != string(after) {
				logger.Warnf("[ToolInput] coerced stringified arguments for tool %q", state.Name)
			}
		}
	}
	if callback == nil || callback.OnToolUse == nil {
		return nil
	}
	callback.OnToolUse(KiroToolUse{
		ToolUseID: state.ToolUseID,
		Name:      state.Name,
		Input:     input,
	})
	return nil
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

func eventStreamUint32(data []byte) uint32 {
	return uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
}

func eventStreamChecksum(prelude, message []byte) uint32 {
	checksum := crc32.Update(0, crc32.IEEETable, prelude)
	return crc32.Update(checksum, crc32.IEEETable, message)
}

func parseEventStreamHeaders(data []byte) (map[string]string, error) {
	headers := make(map[string]string)
	for offset := 0; offset < len(data); {
		nameLength := int(data[offset])
		offset++
		if nameLength == 0 || offset+nameLength >= len(data) {
			return nil, errors.New("malformed event header name")
		}
		name := string(data[offset : offset+nameLength])
		offset += nameLength
		valueType := data[offset]
		offset++

		valueLength := 0
		switch valueType {
		case 0, 1:
			continue
		case 2:
			valueLength = 1
		case 3:
			valueLength = 2
		case 4:
			valueLength = 4
		case 5, 8:
			valueLength = 8
		case 9:
			valueLength = 16
		case 6, 7:
			if offset+2 > len(data) {
				return nil, errors.New("malformed variable event header")
			}
			valueLength = int(data[offset])<<8 | int(data[offset+1])
			offset += 2
		default:
			return nil, fmt.Errorf("unsupported event header type %d", valueType)
		}
		if offset+valueLength > len(data) {
			return nil, errors.New("truncated event header value")
		}
		if valueType == 7 {
			headers[name] = string(data[offset : offset+valueLength])
		}
		offset += valueLength
	}
	return headers, nil
}

// extractFrameMeta extracts the AWS Event Stream routing headers in a single
// pass: :message-type ("event" / "exception" / "error"), :event-type (the event
// name for event frames), :exception-type, and :error-code. Absent headers come
// back as "". Unlike extractEventType (which only reads :event-type and thus
// makes exception/error frames indistinguishable from an unknown event), this
// lets the parser route non-event frames to the failure path.
func extractFrameMeta(headers []byte) (messageType, eventType, exceptionType, errorCode string) {
	offset := 0
	for offset < len(headers) {
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
			switch name {
			case ":message-type":
				messageType = value
			case ":event-type":
				eventType = value
			case ":exception-type":
				exceptionType = value
			case ":error-code":
				errorCode = value
			}
			continue
		}

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
	return
}

// extractFrameErrorMessage pulls a human-readable message out of an
// error/exception frame payload, tolerating the common JSON field name variants
// and falling back to the raw (bounded) body.
func extractFrameErrorMessage(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var m map[string]interface{}
	if err := json.Unmarshal(payload, &m); err == nil {
		for _, k := range []string{"message", "Message", "errorMessage", "error", "reason"} {
			if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	s := strings.TrimSpace(string(payload))
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}

// isOutputTruncationException reports whether a mid-stream exception means the
// model hit its output-token cap (content is valid but truncated) rather than a
// hard failure. Mirrors the Rust reference, which maps ContentLengthExceededException
// to stop_reason=max_tokens instead of aborting.
func isOutputTruncationException(kind string) bool {
	k := strings.ToLower(kind)
	return strings.Contains(k, "contentlengthexceeded") || strings.Contains(k, "maxtokens")
}
