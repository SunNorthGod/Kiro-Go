package proxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const tokenRefreshSkewSeconds int64 = 120

// maxAPIBodyBytes caps request-body size on public API endpoints (32 MiB) to
// prevent memory exhaustion from oversized payloads.
const maxAPIBodyBytes = 32 << 20

// RequestLog stores details about a single API request (success or failure).
type RequestLog struct {
	Time      int64  `json:"time"`      // Unix timestamp
	Endpoint  string `json:"endpoint"`  // claude/openai/responses
	Model     string `json:"model"`     // Requested model
	AccountID string `json:"accountId"` // Account used
	Status    string `json:"status"`    // "success" or "error"
	Error     string `json:"error"`     // Error message (empty on success)
	ErrorType string `json:"errorType"` // Error category (empty on success)
	Tokens    int    `json:"tokens"`    // Total tokens (input+output, 0 on failure)
	Credits   float64 `json:"credits"`  // Credits consumed (0 on failure)
	Duration  int64  `json:"duration"`  // Request duration in ms
}

const requestLogsMaxSize = 500

// Handler HTTP 处理器
type Handler struct {
	pool *pool.AccountPool
	// 运行时统计 (使用原子操作)
	totalRequests   int64
	successRequests int64
	failedRequests  int64
	// clientDisconnects 计数被客户端断开中断、因此未走到上游 metering 计费的请求
	// (仅观测:计费策略不变,断开是否计费留待产品决策)。
	clientDisconnects int64
	totalTokens       int64
	totalCredits    float64 // float64 需要用锁保护
	creditsMu       sync.RWMutex
	startTime       int64
	stopRefresh     chan struct{}
	stopStatsSaver  chan struct{}
	// 模型缓存
	cachedModels    []ModelInfo
	modelsCacheMu   sync.RWMutex
	modelsCacheTime int64
	// Cold-cache stampede guard: modelsColdMu serializes the expensive
	// all-accounts refresh so a burst of /v1/models on an empty cache triggers ONE
	// refresh (others coalesce), and modelsLastColdAttempt is a short negative
	// cache so a failing refresh isn't retried on every request.
	modelsColdMu         sync.Mutex
	modelsLastColdAttempt int64
	promptCache     *promptCacheTracker
	// tokenRefreshLocks serializes token refreshes PER ACCOUNT (not globally): a
	// single shared mutex meant a slow refresh for one account blocked every
	// other account's requests from even checking their own token. Keyed by
	// account id; entries are created on demand and kept (bounded by account count).
	tokenRefreshMu    sync.Mutex // guards the tokenRefreshLocks map only
	tokenRefreshLocks map[string]*sync.Mutex
	// 请求日志 (环形缓冲区，包含成功和失败)
	requestLogs   []RequestLog
	requestLogsMu sync.RWMutex
	// 每日统计 (最近 ~30 天;JSON 模式落盘 daily_stats.json 持久化,DB 模式从 usage_records 聚合;零值可用,惰性初始化)
	dailyMu      sync.Mutex
	dailyStats   map[string]*dayBucket // "2006-01-02"(CST) → 当日累计
	dailySavedAt int64                 // 上次落盘的 unix 秒(节流用)
	// /admin/api/overview 重聚合结果的进程内 TTL 缓存(见 dashboard.go overviewHeavyTTL)。
	// 只缓存 daily 趋势序列(按 days 分 key)与 promptCache 统计块;实时计数不经过它。
	ovHeavyMu     sync.Mutex
	ovDailyCache  map[int]ovDailyEntry
	ovPromptCache ovPromptEntry
	// 统计落盘去抖:记录上次已持久化的计数快照,无变化时跳过 30s 定时写(避免空转重写整份配置文件)。
	lastSavedStats savedStatsSnapshot
	lastSavedMu    sync.Mutex
}

type thinkingStreamSource int

const (
	thinkingSourceUnknown thinkingStreamSource = iota
	thinkingSourceReasoningEvent
	thinkingSourceTagBlock
)

func allowReasoningSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceTagBlock {
		return false
	}
	*source = thinkingSourceReasoningEvent
	return true
}

func allowTagSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceReasoningEvent {
		return false
	}
	if *source == thinkingSourceUnknown {
		*source = thinkingSourceTagBlock
	}
	return *source == thinkingSourceTagBlock
}

func validateClaudeRequestShape(req *ClaudeRequest) string {
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}
	if msg := validateClaudeThinkingConfig(req.Thinking, req.MaxTokens); msg != "" {
		return msg
	}

	hasUserContext := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		lastRole = role
		if role != "user" {
			continue
		}

		text, images, toolResults := extractClaudeUserContent(msg.Content)
		if normalizeUserContent(text, len(images) > 0) != "" || len(toolResults) > 0 {
			hasUserContext = true
		}
	}

	if lastRole == "assistant" {
		return "assistant-prefill final message is not supported; last message must be user"
	}
	if !hasUserContext {
		return "at least one non-empty user message is required"
	}
	return ""
}

func validateClaudeThinkingConfig(thinking *ClaudeThinkingConfig, maxTokens int) string {
	if thinking == nil {
		return ""
	}

	kind := strings.ToLower(strings.TrimSpace(thinking.Type))
	switch kind {
	case "enabled":
		if maxTokens == 0 {
			return "thinking.type enabled cannot be used with max_tokens=0"
		}
		if thinking.BudgetTokens <= 0 {
			return "thinking.budget_tokens is required when thinking.type is enabled"
		}
		if thinking.BudgetTokens < 1024 {
			return "thinking.budget_tokens must be at least 1024"
		}
		if maxTokens > 0 && thinking.BudgetTokens >= maxTokens {
			return "thinking.budget_tokens must be less than max_tokens"
		}
	case "adaptive":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is not supported when thinking.type is adaptive"
		}
	case "disabled":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is not supported when thinking.type is disabled"
		}
	default:
		return "thinking.type must be one of: enabled, adaptive, disabled"
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	if display != "" && display != "summarized" && display != "omitted" {
		return "thinking.display must be one of: summarized, omitted"
	}
	if kind == "disabled" && display != "" {
		return "thinking.display is not supported when thinking.type is disabled"
	}

	return ""
}

type claudeThinkingResponseOptions struct {
	Format      string
	OmitDisplay bool
}

func resolveClaudeThinkingResponseOptions(thinking *ClaudeThinkingConfig, defaultFormat string) claudeThinkingResponseOptions {
	opts := claudeThinkingResponseOptions{Format: defaultFormat}
	if opts.Format == "" {
		opts.Format = "thinking"
	}
	if thinking == nil {
		return opts
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	switch display {
	case "summarized":
		opts.Format = "thinking"
	case "omitted":
		opts.Format = "thinking"
		opts.OmitDisplay = true
	}

	return opts
}

func validateOpenAIRequestShape(req *OpenAIRequest) string {
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}

	hasNonSystem := false
	hasUserContext := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		if role != "system" {
			hasNonSystem = true
			lastRole = role
		}

		if role != "user" {
			continue
		}
		text, images := extractOpenAIUserContent(msg.Content)
		if normalizeUserContent(text, len(images) > 0) != "" {
			hasUserContext = true
		}
	}

	if !hasNonSystem {
		return "at least one non-system message is required"
	}
	if lastRole == "assistant" {
		return "assistant-prefill final message is not supported; last message must be user or tool"
	}
	if !hasUserContext {
		return "at least one non-empty user message is required"
	}
	return ""
}

func NewHandler() *Handler {
	// 启动时应用代理配置
	applyProxyConfig(config.GetProxyURL())

	totalReq, successReq, failedReq, totalTokens, totalCredits := config.GetStats()
	h := &Handler{
		pool:            pool.GetPool(),
		totalRequests:   int64(totalReq),
		successRequests: int64(successReq),
		failedRequests:  int64(failedReq),
		totalTokens:     int64(totalTokens),
		totalCredits:    totalCredits,
		startTime:       time.Now().Unix(),
		stopRefresh:       make(chan struct{}),
		stopStatsSaver:    make(chan struct{}),
		promptCache:       newPromptCacheTracker(defaultPromptCacheTTL),
		tokenRefreshLocks: make(map[string]*sync.Mutex),
	}
	// 启动后台刷新
	go h.backgroundRefresh()
	// 启动后台统计保存 (每30秒保存一次)
	go h.backgroundStatsSaver()
	// 清理过期的 stored responses（>30 天）
	go purgeExpiredResponses(responsesDefaultTTL)
	return h
}

// accountRefreshInterval 账号信息(额度/订阅/overage)的后台刷新周期。管理页已
// 去掉所有手动刷新按钮,数据新鲜度完全靠这里,故从 30min 收紧到 5min。
const accountRefreshInterval = 5 * time.Minute

// maintenanceEveryNTicks 低频维护(模型缓存刷新/日志清理/粘性会话淘汰等)按
// N 个账号刷新周期跑一次:5min × 6 = 30min,与旧节奏一致(模型列表变化极少,
// 无需 5min 一轮全账号 ListAvailableModels)。
const maintenanceEveryNTicks = 6

// backgroundRefresh 后台定时刷新账户信息
func (h *Handler) backgroundRefresh() {
	ticker := time.NewTicker(accountRefreshInterval)
	defer ticker.Stop()

	// 启动时延迟 10 秒后执行一次
	time.Sleep(10 * time.Second)
	h.refreshModelsCache()
	h.refreshAllAccounts()
	pruneUsageRecordsRetention()

	tick := 0
	for {
		select {
		case <-ticker.C:
			// 每 tick(5min):账号额度/订阅/overage 刷新——前端只读渲染,新鲜度全在这。
			h.refreshAllAccounts()
			tick++
			if tick%maintenanceEveryNTicks != 0 {
				continue
			}
			// 每 6 tick(30min):低频维护,保持旧节奏。
			h.refreshModelsCache()
			pruneUsageRecordsRetention()
			sweepLowBalanceGates(time.Hour)
			// Periodic maintenance that previously ran only once at startup or
			// never: expire stored responses on every tick (not just the single
			// startup goroutine), and evict idle sticky conversation bindings so
			// the sticky map stays proportional to active conversations.
			purgeExpiredResponses(responsesDefaultTTL)
			if n := h.pool.EvictStaleStickySessions(); n > 0 {
				logger.Infof("[Maintenance] evicted %d stale sticky sessions", n)
			}
		case <-h.stopRefresh:
			return
		}
	}
}

// refreshAllAccounts 刷新所有账户信息
func (h *Handler) refreshAllAccounts() {
	accounts := config.GetAccounts()
	for i := range accounts {
		account := &accounts[i]
		if !account.Enabled || account.AccessToken == "" {
			continue
		}

		// 检查 token 是否需要刷新
		if account.ExpiresAt > 0 && time.Now().Unix() > account.ExpiresAt-tokenRefreshSkewSeconds {
			newAccessToken, newRefreshToken, newExpiresAt, profileArn, err := auth.RefreshToken(account)
			if err != nil {
				logger.Warnf("[BackgroundRefresh] Token refresh failed for %s: %v", account.Email, err)
				h.handleAccountFailure(account, err)
				continue
			}
			account.AccessToken = newAccessToken
			if newRefreshToken != "" {
				account.RefreshToken = newRefreshToken
			}
			account.ExpiresAt = newExpiresAt
			config.UpdateAccountToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt)
			h.pool.UpdateToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt)
			if profileArn != "" {
				account.ProfileArn = profileArn
				config.UpdateAccountProfileArn(account.ID, profileArn)
			}
		}

		// 刷新账户信息(RefreshAccountInfo 内部会顺带把 getUsageLimits 响应里的
		// overage 状态写回,零额外上游调用;见 kiro_api.go / extractOverageSnapshot)
		info, err := RefreshAccountInfo(account)
		if err != nil {
			logger.Warnf("[BackgroundRefresh] Failed to refresh %s: %v", account.Email, err)
			continue
		}

		config.UpdateAccountInfo(account.ID, *info)

		// 存量账号 OverageStatus 从未定型("")的,在这里补一次探测+自动开启
		// (与新账号建号后的 maybeAutoEnableOverage 同一套 ""-guard 逻辑,定型后
		// 不再触发;此前若建号时探测失败会永远漏掉,这里兜底闭环)。
		if strings.TrimSpace(account.OverageStatus) == "" {
			h.maybeAutoEnableOverage(account)
		}
		logger.Infof("[BackgroundRefresh] Refreshed %s: %s %.1f/%.1f", account.Email, info.SubscriptionType, info.UsageCurrent, info.UsageLimit)
	}
	h.pool.Reload()
}

// validateApiKey 验证 API Key（Bool 包装，旧签名仍被部分调用方使用）
func (h *Handler) validateApiKey(r *http.Request) bool {
	_, err := h.authenticate(r)
	return err == nil
}

// authenticateForClaude runs authenticate and writes a Claude-style error on failure.
// Returns the request with the matched API key injected into context, or nil if auth failed.
func (h *Handler) authenticateForClaude(w http.ResponseWriter, r *http.Request) *http.Request {
	entry, err := h.authenticate(r)
	if err != nil {
		ae, _ := err.(*authError)
		if ae == nil {
			ae = newAuthError(http.StatusUnauthorized, "authentication_error", err.Error())
		}
		h.sendClaudeError(w, ae.status, ae.code, ae.message)
		return nil
	}
	return withApiKeyContext(r, entry)
}

// authenticateForOpenAI runs authenticate and writes an OpenAI-style error on failure.
func (h *Handler) authenticateForOpenAI(w http.ResponseWriter, r *http.Request) *http.Request {
	entry, err := h.authenticate(r)
	if err != nil {
		ae, _ := err.(*authError)
		if ae == nil {
			ae = newAuthError(http.StatusUnauthorized, "authentication_error", err.Error())
		}
		h.sendOpenAIError(w, ae.status, ae.code, ae.message)
		return nil
	}
	return withApiKeyContext(r, entry)
}

// ServeHTTP 路由分发
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Debug-level request trace for fine-grained visibility. Use the real client
	// IP (behind Cloudflare/Caddy) rather than the proxy's RemoteAddr.
	logger.Debugf("[HTTP] %s %s from %s", r.Method, path, clientIP(r))

	// CORS - 完整的头部支持
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key, anthropic-version, anthropic-beta, x-api-key, x-stainless-os, x-stainless-lang, x-stainless-package-version, x-stainless-runtime, x-stainless-runtime-version, x-stainless-arch")
	w.Header().Set("Access-Control-Expose-Headers", "x-request-id, x-ratelimit-limit-requests, x-ratelimit-limit-tokens, x-ratelimit-remaining-requests, x-ratelimit-remaining-tokens, x-ratelimit-reset-requests, x-ratelimit-reset-tokens")

	// Behind a CDN (Cloudflare): dynamic / auth-scoped responses are keyed only by
	// URL at the edge, so they must never be cached or shared across API keys.
	// Static assets and /__down are excluded so they stay cacheable. SSE/HTML
	// handlers set their own (no-cache) which harmlessly overrides this.
	if isEdgeUncacheablePath(path) {
		w.Header().Set("Cache-Control", "no-store")
	}

	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}

	// 对读取请求体的公共 API 端点限制体积,防止超大负载导致内存耗尽。
	// 管理端点/Web 路径豁免(凭证导入可能更大,且单独鉴权)。
	switch path {
	case "/v1/messages", "/messages", "/anthropic/v1/messages",
		"/v1/messages/count_tokens", "/messages/count_tokens",
		"/v1/chat/completions", "/chat/completions",
		"/v1/responses", "/responses":
		r.Body = http.MaxBytesReader(w, r.Body, maxAPIBodyBytes)
	}

	// 路由
	switch {
	// API 端点（需要验证 API Key）
	case path == "/v1/messages" || path == "/messages" || path == "/anthropic/v1/messages":
		ar := h.authenticateForClaude(w, r)
		if ar == nil {
			return
		}
		h.handleClaudeMessages(w, ar)
	case path == "/v1/messages/count_tokens" || path == "/messages/count_tokens":
		ar := h.authenticateForClaude(w, r)
		if ar == nil {
			return
		}
		h.handleCountTokens(w, ar)
	case path == "/v1/chat/completions" || path == "/chat/completions":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		h.handleOpenAIChat(w, ar)
	case path == "/v1/responses" || path == "/responses":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		h.handleOpenAIResponses(w, ar)
	case path == "/v1/models" || path == "/models":
		h.handleModels(w, r)
	case path == "/api/event_logging/batch":
		// Claude Code 遥测端点 - 直接返回 200 OK
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(`{"status":"ok"}`))

	// 管理端点
	case path == "/admin" || path == "/admin/":
		h.serveAdminPage(w, r)
	case strings.HasPrefix(path, "/admin/api/"):
		h.handleAdminAPI(w, r)
	case strings.HasPrefix(path, "/admin/"):
		h.serveStaticFile(w, r)

	// 用户自助门户(客户用自己的 API Key 查看用量/余额/充值记录,只读且只能看自己)
	case path == "/user" || path == "/user/":
		h.serveUserPage(w, r)
	case strings.HasPrefix(path, "/user/api/"):
		h.handleUserAPI(w, r)

	// 健康检查
	case path == "/health" || path == "/":
		h.handleHealth(w, r)

	// 轻量存活探针：CloudflareSpeedTest 优选 exe 在筛选候选 IP 时会对
	// https://<域名>:<端口>/v1/ping 发 HTTPS 请求，要求返回 200 才认定该 CF 边缘 IP
	// 真正能服务本域名(借此排除 TLS 握手失败 / Edge IP Restricted 1034 的坏 IP)。
	// 必须无鉴权、恒定 200、可被 CF 缓存,否则所有候选 IP 都会被误判为不可用。
	case path == "/v1/ping" || path == "/ping":
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(`{"status":"ok"}`))

	// CDN 测速下载端点（无鉴权、可被 CF 边缘缓存）——给 CloudflareSpeedTest 等
	// 工具一个稳定的自建测速 URL，替代不可靠的默认 cf.xiu2.xyz/url。
	case path == "/__down":
		h.handleSpeedTestDownload(w, r)

	// 统计端点（需要 API Key 鉴权）
	case path == "/v1/stats":
		if !h.validateApiKey(r) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid or missing API key"})
			return
		}
		h.handleStats(w, r)

	default:
		http.Error(w, "Not Found", 404)
	}
}

// handleHealth 健康检查（不暴露统计数据）
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"version": config.Version,
		"uptime":  time.Now().Unix() - h.startTime,
	})
}

// handleStats 统计数据（需要 API Key 鉴权）
func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "ok",
		"version":         config.Version,
		"accounts":        h.pool.Count(),
		"available":       h.pool.AvailableCount(),
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

// modelsColdNegativeCacheSec bounds how often a cold-cache refresh is retried
// when it keeps failing, so /v1/models falls back cheaply instead of hammering
// every account on every request.
const modelsColdNegativeCacheSec = 15

// ensureModelsCache populates the model cache when empty, coalescing concurrent
// cold-cache callers behind modelsColdMu (only one runs the expensive
// all-accounts refresh) with a short negative cache on repeated failure.
func (h *Handler) ensureModelsCache() {
	h.modelsCacheMu.RLock()
	have := len(h.cachedModels)
	h.modelsCacheMu.RUnlock()
	if have > 0 {
		return
	}

	h.modelsColdMu.Lock()
	defer h.modelsColdMu.Unlock()
	// Re-check: a coalesced caller may have populated the cache while we waited.
	h.modelsCacheMu.RLock()
	have = len(h.cachedModels)
	h.modelsCacheMu.RUnlock()
	if have > 0 {
		return
	}
	// Negative cache: skip re-refreshing (fall back) if we just tried and failed.
	now := time.Now().Unix()
	if now-h.modelsLastColdAttempt < modelsColdNegativeCacheSec {
		return
	}
	h.modelsLastColdAttempt = now
	h.refreshModelsCache()
}

// handleModels 模型列表
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	// 尝试用缓存的真实模型列表
	h.modelsCacheMu.RLock()
	cached := h.cachedModels
	h.modelsCacheMu.RUnlock()
	if len(cached) == 0 {
		h.ensureModelsCache()
		h.modelsCacheMu.RLock()
		cached = h.cachedModels
		h.modelsCacheMu.RUnlock()
	}

	thinkingSuffix := config.GetThinkingConfig().Suffix

	models := buildAnthropicModelsResponse(cached, thinkingSuffix)
	if len(models) == 0 {
		models = fallbackAnthropicModels(thinkingSuffix)
	}

	// 添加别名模型（仅当官方模型列表里没有同名 id 时才补，避免与 Kiro
	// ListAvailableModels 已返回的模型重复。auto 官方已提供，通常不会再追加）。
	existingIDs := make(map[string]bool, len(models))
	for _, m := range models {
		if id, ok := m["id"].(string); ok {
			existingIDs[id] = true
		}
	}
	autoModel := buildModelInfo("auto", "kiro-proxy", true)
	autoModel["display_name"] = "Auto"
	autoModel["description"] = "Automatically selects the best available model."
	for _, alias := range []map[string]interface{}{
		autoModel,
		buildModelInfo("gpt-4o", "kiro-proxy", true),
		buildModelInfo("gpt-4", "kiro-proxy", true),
	} {
		id, _ := alias["id"].(string)
		if id == "" || existingIDs[id] {
			continue
		}
		existingIDs[id] = true
		models = append(models, alias)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   models,
	})
	return
}

func buildAnthropicModelsResponse(cached []ModelInfo, thinkingSuffix string) []map[string]interface{} {
	if len(cached) == 0 {
		return nil
	}

	models := make([]map[string]interface{}, 0, len(cached)*2)
	for i := range cached {
		m := cached[i]
		supportsImage := modelSupportsImage(m.InputTypes)
		// Base model carries Kiro's full metadata (effort/reasoning schema, token
		// limits, description) so the list faithfully mirrors ListAvailableModels.
		base := buildModelInfo(m.ModelId, "anthropic", supportsImage)
		enrichModelInfo(base, &m, true)
		models = append(models, base)
		// Auto-generated thinking variant: same token limits/description, but no
		// effort schema (thinking is budget-driven for the variant).
		variant := buildModelInfo(m.ModelId+thinkingSuffix, "anthropic", supportsImage)
		enrichModelInfo(variant, &m, false)
		models = append(models, variant)
	}
	return models
}

// enrichModelInfo augments a /v1/models entry with Kiro's per-model metadata so
// the relay list mirrors ListAvailableModels. When includeEffort is true and the
// model exposes an effort schema, the effort levels / reasoning modes / schema
// path / defaults are attached (plus the raw schema) so clients (and the plugin's
// CPS) can reconstruct Kiro's exact per-model thinking options.
func enrichModelInfo(info map[string]interface{}, m *ModelInfo, includeEffort bool) {
	if m.ModelName != "" {
		info["display_name"] = m.ModelName
	}
	if m.Description != "" {
		info["description"] = m.Description
	}
	if m.RateMultiplier > 0 {
		info["rate_multiplier"] = m.RateMultiplier
	}
	if m.TokenLimits != nil {
		if m.TokenLimits.MaxInputTokens > 0 {
			info["context_window"] = m.TokenLimits.MaxInputTokens
			info["max_input_tokens"] = m.TokenLimits.MaxInputTokens
		}
		if m.TokenLimits.MaxOutputTokens > 0 {
			info["max_output_tokens"] = m.TokenLimits.MaxOutputTokens
		}
	}
	if !includeEffort {
		return
	}
	eff := m.EffortInfo()
	if eff == nil {
		return
	}
	if len(eff.Levels) > 0 {
		info["effort_levels"] = eff.Levels
	}
	if eff.SchemaPath != "" {
		info["effort_schema_path"] = eff.SchemaPath
	}
	if eff.DefaultLevel != "" {
		info["default_effort_level"] = eff.DefaultLevel
	}
	if len(eff.Modes) > 0 {
		info["reasoning_modes"] = eff.Modes
	}
	if eff.DefaultMode != "" {
		info["default_reasoning_mode"] = eff.DefaultMode
	}
	if len(m.AdditionalModelRequestFieldsSchema) > 0 {
		info["additionalModelRequestFieldsSchema"] = m.AdditionalModelRequestFieldsSchema
	}
}

func fallbackAnthropicModels(thinkingSuffix string) []map[string]interface{} {
	return []map[string]interface{}{
		buildModelInfo("claude-sonnet-4.6", "anthropic", true),
		buildModelInfo("claude-sonnet-4.6"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.6", "anthropic", true),
		buildModelInfo("claude-opus-4.6"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.7", "anthropic", true),
		buildModelInfo("claude-opus-4.7"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-sonnet-4.5", "anthropic", true),
		buildModelInfo("claude-sonnet-4.5"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-sonnet-4", "anthropic", true),
		buildModelInfo("claude-sonnet-4"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-haiku-4.5", "anthropic", true),
		buildModelInfo("claude-haiku-4.5"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.5", "anthropic", true),
		buildModelInfo("claude-opus-4.5"+thinkingSuffix, "anthropic", true),
	}
}

func modelSupportsImage(inputTypes []string) bool {
	for _, t := range inputTypes {
		lt := strings.ToLower(t)
		if strings.Contains(lt, "image") || strings.Contains(lt, "vision") {
			return true
		}
	}
	return false
}

func buildModelInfo(id, ownedBy string, supportsImage bool) map[string]interface{} {
	modalities := []string{"text"}
	if supportsImage {
		modalities = append(modalities, "image")
	}
	modalitiesMap := map[string][]string{
		"input":  modalities,
		"output": []string{"text"},
	}

	return map[string]interface{}{
		"id":               id,
		"object":           "model",
		"owned_by":         ownedBy,
		"supports_image":   supportsImage,
		"input_modalities": modalities,
		"modalities":       modalitiesMap,
		"capabilities": map[string]bool{
			"vision":       supportsImage,
			"image":        supportsImage,
			"image_vision": supportsImage,
		},
		"info": map[string]interface{}{
			"meta": map[string]interface{}{
				"capabilities": map[string]bool{
					"vision":       supportsImage,
					"image_vision": supportsImage,
				},
			},
		},
	}
}

// refreshModelsCache 从 Kiro API 拉取模型列表并缓存
func (h *Handler) refreshModelsCache() {
	accounts := config.GetEnabledAccounts()
	if len(accounts) == 0 {
		return
	}

	aggregated := make([]ModelInfo, 0)
	for i := range accounts {
		account := &accounts[i]
		if err := h.ensureValidToken(account); err != nil {
			logger.Warnf("[ModelsCache] Skip %s token refresh failed: %v", account.Email, err)
			h.handleAccountFailure(account, err)
			continue
		}

		models, err := ListAvailableModels(account)
		if err != nil {
			logger.Warnf("[ModelsCache] Failed to refresh for %s: %v", account.Email, err)
			h.handleAccountFailure(account, err)
			continue
		}
		// 缓存每账号可用模型，用于路由时过滤
		modelIDs := make([]string, 0, len(models))
		for _, m := range models {
			modelIDs = append(modelIDs, m.ModelId)
		}
		h.pool.SetModelList(account.ID, modelIDs)
		aggregated = mergeUniqueModels(aggregated, models)
	}

	if len(aggregated) > 0 {
		h.modelsCacheMu.Lock()
		h.cachedModels = aggregated
		h.modelsCacheTime = time.Now().Unix()
		h.modelsCacheMu.Unlock()
		logger.Infof("[ModelsCache] Cached %d models", len(aggregated))
	}
}

// fetchAndCacheAccountModels 为单个账号拉取并写入模型缓存。
// 同时更新 pool 的路由缓存与全局聚合模型列表。
func (h *Handler) fetchAndCacheAccountModels(account *config.Account) error {
	if err := h.ensureValidToken(account); err != nil {
		return fmt.Errorf("token refresh failed: %w", err)
	}
	models, err := ListAvailableModels(account)
	if err != nil {
		return err
	}
	modelIDs := make([]string, 0, len(models))
	for _, m := range models {
		modelIDs = append(modelIDs, m.ModelId)
	}
	h.pool.SetModelList(account.ID, modelIDs)

	// 合并到聚合缓存
	h.modelsCacheMu.Lock()
	h.cachedModels = mergeUniqueModels(h.cachedModels, models)
	h.modelsCacheTime = time.Now().Unix()
	h.modelsCacheMu.Unlock()

	logger.Infof("[ModelsCache] Refreshed %d models for account %s", len(models), account.Email)
	// Auto-enable the upstream Overages switch for freshly added, capable accounts.
	h.maybeAutoEnableOverage(account)
	return nil
}

func mergeUniqueModels(existing []ModelInfo, incoming []ModelInfo) []ModelInfo {
	if len(incoming) == 0 {
		return existing
	}

	indexByID := make(map[string]int, len(existing))
	merged := make([]ModelInfo, len(existing))
	copy(merged, existing)
	for i, model := range merged {
		indexByID[strings.ToLower(strings.TrimSpace(model.ModelId))] = i
	}

	for _, model := range incoming {
		key := strings.ToLower(strings.TrimSpace(model.ModelId))
		if key == "" {
			continue
		}
		if idx, ok := indexByID[key]; ok {
			merged[idx] = mergeModelInfo(merged[idx], model)
			continue
		}
		indexByID[key] = len(merged)
		merged = append(merged, model)
	}

	return merged
}

func mergeModelInfo(base ModelInfo, extra ModelInfo) ModelInfo {
	if base.ModelName == "" {
		base.ModelName = extra.ModelName
	}
	if base.Description == "" {
		base.Description = extra.Description
	}
	if base.RateMultiplier == 0 {
		base.RateMultiplier = extra.RateMultiplier
	}
	if base.TokenLimits == nil {
		base.TokenLimits = extra.TokenLimits
	}
	base.InputTypes = mergeStringLists(base.InputTypes, extra.InputTypes)
	return base
}

func mergeStringLists(base []string, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base)+len(extra))
	merged := make([]string, 0, len(base)+len(extra))
	for _, item := range base {
		key := strings.ToLower(strings.TrimSpace(item))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, item)
	}
	for _, item := range extra {
		key := strings.ToLower(strings.TrimSpace(item))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, item)
	}
	return merged
}

// handleCountTokens Token 计数（Claude Code 会调用）
func (h *Handler) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	if msg := validateClaudeThinkingConfig(req.Thinking, req.MaxTokens); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}

	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := resolveClaudeThinkingMode(req.Model, req.Thinking, thinkingCfg.Suffix)
	req.Model = actualModel
	effectiveReq := cloneClaudeRequestForThinking(&req, thinking)

	estimatedTokens := estimateClaudeRequestInputTokens(effectiveReq)
	if estimatedTokens < 1 {
		estimatedTokens = 1
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]int{"input_tokens": estimatedTokens})
}

// handleClaudeMessages Claude API 处理
func (h *Handler) handleClaudeMessages(w http.ResponseWriter, r *http.Request) {
	h.handleClaudeMessagesInternal(w, r)
}

func (h *Handler) handleClaudeMessagesInternal(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	// 低余额串行化闸门(保守透支缓解:余额将尽的卡强制并发=1,见 low_balance.go)。
	releaseLowBalance, ok := gateLowBalance(r)
	if !ok {
		h.sendClaudeError(w, 429, "rate_limit_error", lowBalanceRejectMessage)
		return
	}
	if releaseLowBalance != nil {
		defer releaseLowBalance()
	}

	// 读取请求
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON: "+err.Error())
		return
	}
	if msg := validateClaudeRequestShape(&req); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}

	// 解析模型和 thinking 模式
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := resolveClaudeThinkingMode(req.Model, req.Thinking, thinkingCfg.Suffix)
	req.Model = actualModel
	effectiveReq := cloneClaudeRequestForThinking(&req, thinking)
	thinkingResponseOpts := resolveClaudeThinkingResponseOptions(req.Thinking, thinkingCfg.ClaudeFormat)
	estimatedInputTokens := estimateClaudeRequestInputTokens(effectiveReq)
	cacheProfile := h.promptCache.BuildClaudeProfile(effectiveReq, estimatedInputTokens)

	// 转换请求
	kiroPayload := ClaudeToKiro(&req, thinking)

	// 响应侧推理门:是否把 AWS 的 reasoningContentEvent 转发给客户端。
	// 请求侧的 thinking(系统 <thinking_mode> 标签注入)只认后缀/thinking字段;
	// 但响应门必须**额外**认 output_config.effort —— 原生 Kiro/插件默认 auto 档只发 effort,
	// 若响应门只看 thinking,后端产出的思考会被整个丢弃(实测 effort-only=0 段思考)。
	// 对齐旧 Rust 反代 `thinking_enabled = thinking.is_enabled() || output_config.is_some()`。
	forwardReasoning := thinking || claudeEffortRequested(&req)

	// Stream or non-stream. r.Context() is threaded down to the upstream call so
	// a client disconnect cancels the upstream request (see CallKiroAPI).
	apiKeyID := apiKeyIDFromContext(r.Context())
	if req.Stream {
		h.handleClaudeStream(r.Context(), w, kiroPayload, req.Model, forwardReasoning, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID)
	} else {
		h.handleClaudeNonStream(r.Context(), w, kiroPayload, req.Model, forwardReasoning, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID)
	}
}

// handleClaudeStream Claude 流式响应
func (h *Handler) handleClaudeStream(ctx context.Context, w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendClaudeError(w, 500, "api_error", "Streaming not supported")
		return
	}

	// 获取 thinking 输出格式配置
	thinkingFormat := thinkingOpts.Format

	reqStart := time.Now()
	msgID := "msg_" + uuid.New().String()
	startInputTokens := estimatedInputTokens
	excluded := make(map[string]bool)
	var lastErr error
	messageStarted := false
	var messageStartUsage promptCacheUsage

	ensureMessageStart := func() {
		if messageStarted {
			return
		}
		h.sendSSE(w, flusher, "message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":            msgID,
				"type":          "message",
				"role":          "assistant",
				"content":       []interface{}{},
				"model":         model,
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         buildClaudeUsageMap(startInputTokens, 0, messageStartUsage, cacheProfile != nil),
			},
		})
		messageStarted = true
	}

	// conversationID(确定性 agentContinuationId)= 会话粘性缓存键,让同一会话稳定命中同一账号。
	conversationID := payload.ConversationState.AgentContinuationId
	// 无 API Key(公网/无鉴权模式)时不施加每卡密公平限制。
	bypassFairness := apiKeyID == ""
	// 每卡密并发下限(公平准入基线):取该卡密配置的 MaxConcurrency,未配置则 0(用池默认)。
	keyFloor := 0
	var boundAccountIDs []string
	if e := config.GetApiKeyEntry(apiKeyID); e != nil {
		keyFloor = e.MaxConcurrency
		boundAccountIDs = e.BoundAccountIDs
	}
	// Panic-safety net: guarantee the acquired concurrency slot is released even
	// if a handler/callback panics. releaseSlot is idempotent (sync.Once), so the
	// explicit release on the normal path and this deferred guard never
	// double-release; on Acquire failure releaseSlot is nil (no-op guard).
	var activeRelease func()
	defer func() {
		if activeRelease != nil {
			activeRelease()
		}
	}()
	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account, releaseSlot, aerr := h.pool.Acquire(apiKeyID, keyFloor, bypassFairness, conversationID, model, excluded, boundAccountIDs)
		activeRelease = releaseSlot
		if aerr == pool.ErrTooBusy {
			// 本卡密并发已达公平上限且池子饱和 → 429(软限制:空载时不会到这里)。
			h.sendClaudeError(w, 429, "rate_limit_error", "Too many concurrent requests for this key; retry shortly")
			return
		}
		if aerr != nil {
			break // ErrNoAccount → 无可用账号
		}
		if err := h.ensureValidToken(&account); err != nil {
			releaseSlot()
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(&account, err)
			continue
		}
		cacheUsage := h.promptCache.Compute(account.ID, cacheProfile)
		messageStartUsage = cacheUsage

		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var contextFull bool
		var toolUses []KiroToolUse
		var nextContentIndex int
		var rawContentBuilder strings.Builder
		var rawThinkingBuilder strings.Builder
		var meteringCacheRead, meteringCacheCreation int
		var hasCacheMetering bool
		activeBlockIndex := -1
		activeBlockType := ""

		// 原生推理签名:reasoningContentEvent.signature 收集到这里,thinking 块关闭前
		// 作为 signature_delta 注入(优先真实签名,缺失则兜底伪造)。签名只发一次。
		var nativeSignature string
		signatureSent := false
		emitSignatureDelta := func(thinkingIdx int) {
			if signatureSent {
				return
			}
			signatureSent = true
			sig := nativeSignature
			if sig == "" {
				sig = generateFakeSignature()
			}
			// 按 40 字节切块发送 signature_delta(对齐 Anthropic 分块惯例)
			for i := 0; i < len(sig); i += 40 {
				end := i + 40
				if end > len(sig) {
					end = len(sig)
				}
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": thinkingIdx,
					"delta": map[string]string{"type": "signature_delta", "signature": sig[i:end]},
				})
			}
		}

		closeActiveBlock := func() {
			if activeBlockIndex < 0 {
				return
			}
			// thinking 块关闭前必须先发 signature_delta(Anthropic 协议:签名在 content_block_stop 之前)
			if activeBlockType == "thinking" {
				emitSignatureDelta(activeBlockIndex)
			}
			h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": activeBlockIndex,
			})
			activeBlockIndex = -1
			activeBlockType = ""
		}

		startContentBlock := func(blockType string) {
			if activeBlockType == blockType {
				return
			}
			ensureMessageStart()
			closeActiveBlock()

			idx := nextContentIndex
			nextContentIndex++

			if blockType == "thinking" {
				h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]string{
						"type":     "thinking",
						"thinking": "",
					},
				})
			} else {
				h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]string{
						"type": "text",
						"text": "",
					},
				})
			}

			activeBlockIndex = idx
			activeBlockType = blockType
		}

		var textBuffer string
		var inThinkingBlock bool
		var dropTagThinking bool
		var thinkingSource thinkingStreamSource
		var thinkingStarted bool
		var eventThinkingOpen bool

		sendText := func(text string, thinkingState int) {
			if thinkingState == 0 {
				if text == "" {
					return
				}
				startContentBlock("text")
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": text},
				})
				return
			}

			if !thinking {
				return
			}

			switch thinkingFormat {
			case "think":
				var outputText string
				switch thinkingState {
				case 1:
					outputText = "<think>" + text
				case 2:
					outputText = text
				case 3:
					outputText = text + "</think>"
				}
				if outputText == "" {
					return
				}
				startContentBlock("text")
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": outputText},
				})
			case "reasoning_content":
				if text == "" {
					return
				}
				startContentBlock("text")
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": text},
				})
			default:
				if thinkingOpts.OmitDisplay {
					if thinkingState == 1 {
						startContentBlock("thinking")
						return
					}
					if thinkingState == 3 {
						if activeBlockType != "thinking" {
							startContentBlock("thinking")
						}
						closeActiveBlock()
					}
					return
				}
				if thinkingState == 3 && text == "" {
					if activeBlockType == "thinking" {
						closeActiveBlock()
					}
					return
				}
				if text != "" {
					startContentBlock("thinking")
					h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": activeBlockIndex,
						"delta": map[string]string{"type": "thinking_delta", "thinking": text},
					})
				}
				if thinkingState == 3 && activeBlockType == "thinking" {
					closeActiveBlock()
				}
			}
		}

		processClaudeText := func(text string, isThinking bool, forceFlush bool) {
			if isThinking && !thinking {
				return
			}

			if isThinking {
				if !allowReasoningSource(&thinkingSource) {
					return
				}
				if !thinkingStarted {
					sendText(text, 1)
					thinkingStarted = true
					eventThinkingOpen = true
				} else {
					sendText(text, 2)
				}
				return
			}

			if eventThinkingOpen {
				sendText("", 3)
				eventThinkingOpen = false
				thinkingStarted = false
			}

			textBuffer += text

			for {
				if !inThinkingBlock {
					thinkingStart := strings.Index(textBuffer, "<thinking>")
					if thinkingStart != -1 {
						if thinkingStart > 0 {
							sendText(textBuffer[:thinkingStart], 0)
						}
						textBuffer = textBuffer[thinkingStart+10:]
						inThinkingBlock = true
						dropTagThinking = !allowTagSource(&thinkingSource)
						thinkingStarted = false
					} else if forceFlush || len([]rune(textBuffer)) > 50 {
						runes := []rune(textBuffer)
						safeLen := len(runes)
						if !forceFlush {
							safeLen = max(0, len(runes)-15)
						}
						if safeLen > 0 {
							sendText(string(runes[:safeLen]), 0)
							textBuffer = string(runes[safeLen:])
						}
						break
					} else {
						break
					}
				} else {
					thinkingEnd := strings.Index(textBuffer, "</thinking>")
					if thinkingEnd != -1 {
						content := textBuffer[:thinkingEnd]
						if !dropTagThinking {
							if !thinkingStarted {
								sendText(content, 1)
								sendText("", 3)
							} else {
								sendText(content, 3)
							}
						}
						textBuffer = textBuffer[thinkingEnd+11:]
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
					} else if forceFlush {
						if textBuffer != "" {
							if !dropTagThinking {
								if !thinkingStarted {
									sendText(textBuffer, 1)
									sendText("", 3)
								} else {
									sendText(textBuffer, 3)
								}
							}
							textBuffer = ""
						}
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
						break
					} else {
						runes := []rune(textBuffer)
						if len(runes) > 20 {
							safeLen := len(runes) - 15
							if safeLen > 0 {
								if !dropTagThinking {
									if !thinkingStarted {
										sendText(string(runes[:safeLen]), 1)
										thinkingStarted = true
									} else {
										sendText(string(runes[:safeLen]), 2)
									}
								}
								textBuffer = string(runes[safeLen:])
							}
						}
						break
					}
				}
			}
		}

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				if isThinking {
					rawThinkingBuilder.WriteString(text)
				} else {
					rawContentBuilder.WriteString(text)
				}
				processClaudeText(text, isThinking, false)
			},
			OnToolUse: func(tu KiroToolUse) {
				processClaudeText("", false, true)
				rawContentBuilder.WriteString(tu.Name)
				if b, err := json.Marshal(tu.Input); err == nil {
					rawContentBuilder.Write(b)
				}

				toolUses = append(toolUses, tu)
				ensureMessageStart()
				closeActiveBlock()

				idx := nextContentIndex
				nextContentIndex++

				h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]interface{}{
						"type":  "tool_use",
						"id":    tu.ToolUseID,
						"name":  tu.Name,
						"input": map[string]interface{}{},
					},
				})

				inputJSON, _ := json.Marshal(tu.Input)
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": idx,
					"delta": map[string]interface{}{
						"type":         "input_json_delta",
						"partial_json": string(inputJSON),
					},
				})

				h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
					"type":  "content_block_stop",
					"index": idx,
				})
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
				if pct >= 100 {
					contextFull = true
				}
			},
			OnReasoningSignature: func(sig string) {
				if sig != "" {
					nativeSignature = sig
				}
			},
			OnCacheMetering: func(read, creation int) {
				meteringCacheRead, meteringCacheCreation = read, creation
				hasCacheMetering = true
			},
		}

		err := callKiroWithSelfHeal(ctx, &account, payload, callback)
		if err != nil {
			// Client disconnected: the error is just our own cancellation, not an
			// account fault. Release and return silently — no exclude/retry, no
			// account-failure signal, no failure stat (observability only).
			if clientGone(ctx) {
				releaseSlot()
				h.noteClientDisconnect("claude", model, apiKeyID,
					estimateApproxTokens(rawContentBuilder.String())+estimateApproxTokens(rawThinkingBuilder.String()))
				return
			}
			releaseSlot()
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(&account, err)
			if !messageStarted {
				continue
			}
			h.recordFailureWithDetails("claude", model, account.ID, err)
			h.sendSSE(w, flusher, "error", map[string]interface{}{
				"type":  "error",
				"error": map[string]string{"type": "api_error", "message": err.Error()},
			})
			return
		}

		processClaudeText("", false, true)
		if eventThinkingOpen {
			sendText("", 3)
		}
		closeActiveBlock()

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		// Final cache accounting: upstream metering truth wins over the local
		// simulation; both get clamped to the final input so the value sent to
		// the client, shown in panels and written to the ledgers is identical.
		cacheUsage = resolvePromptCacheUsage(cacheUsage, hasCacheMetering, meteringCacheRead, meteringCacheCreation, inputTokens)
		outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
		thinkingOutput := rawThinkingBuilder.String()
		if thinking && thinkingOutput == "" && extractedReasoning != "" {
			thinkingOutput = extractedReasoning
		}
		if !thinking {
			thinkingOutput = ""
		}
		outputTokens = estimateClaudeOutputTokens(outputContent, thinkingOutput, toolUses)

		// 空响应检测: 上游成功但零内容零工具(或近空且上下文过大) → 不静默发 end_turn,
		// 而是给客户端一个 error 事件(过大→提示压缩、偏小→可重试),避免 agentic 客户端卡死。
		if isEmptyKiroResponse(outputContent, thinkingOutput, len(toolUses), outputTokens, inputTokens, model) {
			h.pool.RecordSuccess(account.ID)
			releaseSlot()
			errType, errMsg := emptyResponseErrorInfo(emptyResponseIsOversizedContext(inputTokens, model))
			h.recordFailureWithDetails("claude", model, account.ID, fmt.Errorf("empty upstream response"))
			ensureMessageStart()
			h.sendSSE(w, flusher, "error", map[string]interface{}{
				"type":  "error",
				"error": map[string]string{"type": errType, "message": errMsg},
			})
			return
		}

		h.recordSuccessForApiKeyWithCache(apiKeyID, model, inputTokens, outputTokens, cacheUsage.CacheReadInputTokens, cacheUsage.CacheCreationInputTokens, credits)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		releaseSlot()
		h.promptCache.Update(account.ID, cacheProfile)
		h.recordSuccessLog("claude", model, account.ID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

		stopReason := resolveClaudeStopReason(len(toolUses) > 0, contextFull, inputTokens, model)

		ensureMessageStart()
		h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
			"type": "message_delta",
			"delta": map[string]interface{}{
				"stop_reason": stopReason,
			},
			"usage": buildClaudeUsageMap(inputTokens, outputTokens, cacheUsage, cacheProfile != nil || hasCacheMetering),
		})

		h.sendSSE(w, flusher, "message_stop", map[string]interface{}{
			"type": "message_stop",
		})
		return
	}

	if lastErr == nil {
		h.sendClaudeError(w, 503, "api_error", "No available accounts")
		return
	}

	h.recordFailureWithDetails("claude", model, "", lastErr)
	h.sendClaudeError(w, 500, "api_error", lastErr.Error())
}

func (h *Handler) sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonData))
	flusher.Flush()
}

// backgroundStatsSaver 后台定时保存统计数据
func (h *Handler) backgroundStatsSaver() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			h.saveStats()
		case <-h.stopStatsSaver:
			h.saveStats() // 退出前保存一次
			return
		}
	}
}

// savedStatsSnapshot is the set of global counters saveStats persists; it is
// compared against the last-persisted values to skip no-op writes.
type savedStatsSnapshot struct {
	totalRequests   int64
	successRequests int64
	failedRequests  int64
	totalTokens     int64
	totalCredits    float64
	valid           bool
}

// saveStats 保存统计到配置文件。若自上次落盘以来计数没有变化则跳过——空闲时
// 后台每 30s 一次的定时保存不再无谓重写整份配置文件。
func (h *Handler) saveStats() {
	cur := savedStatsSnapshot{
		totalRequests:   atomic.LoadInt64(&h.totalRequests),
		successRequests: atomic.LoadInt64(&h.successRequests),
		failedRequests:  atomic.LoadInt64(&h.failedRequests),
		totalTokens:     atomic.LoadInt64(&h.totalTokens),
		totalCredits:    h.getCredits(),
		valid:           true,
	}
	h.lastSavedMu.Lock()
	prev := h.lastSavedStats
	if prev.valid &&
		prev.totalRequests == cur.totalRequests &&
		prev.successRequests == cur.successRequests &&
		prev.failedRequests == cur.failedRequests &&
		prev.totalTokens == cur.totalTokens &&
		prev.totalCredits == cur.totalCredits {
		h.lastSavedMu.Unlock()
		return
	}
	h.lastSavedStats = cur
	h.lastSavedMu.Unlock()

	config.UpdateStats(
		int(cur.totalRequests),
		int(cur.successRequests),
		int(cur.failedRequests),
		int(cur.totalTokens),
		cur.totalCredits,
	)
}

// getCredits 线程安全获取 credits
func (h *Handler) getCredits() float64 {
	h.creditsMu.RLock()
	defer h.creditsMu.RUnlock()
	return h.totalCredits
}

// addCredits 线程安全增加 credits
func (h *Handler) addCredits(credits float64) {
	h.creditsMu.Lock()
	h.totalCredits += credits
	h.creditsMu.Unlock()
}

// 统计记录 (使用原子操作)
func (h *Handler) recordSuccess(inputTokens, outputTokens int, credits float64) {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.successRequests, 1)
	atomic.AddInt64(&h.totalTokens, int64(inputTokens+outputTokens))
	h.addCredits(credits)
	h.recordDaily(inputTokens+outputTokens, credits)
}

// recordSuccessForApiKey is recordSuccess + per-API-key usage attribution.
// When apiKeyID is empty (legacy single-key path or unauthenticated path), only the
// global counters are updated. Persistence errors are logged but do not propagate.
func (h *Handler) recordSuccessForApiKey(apiKeyID, model string, inputTokens, outputTokens int, credits float64) {
	h.recordSuccess(inputTokens, outputTokens, credits)
	if apiKeyID == "" {
		return
	}
	// Carry the model through so usage_counters/usage_records get per-model
	// attribution, and split input/output so both ledgers are accurate.
	if err := config.RecordApiKeyUsage(apiKeyID, model, int64(inputTokens), int64(outputTokens), credits); err != nil {
		logger.Warnf("[ApiKey] failed to record usage for key %s: %v", apiKeyID, err)
	}
}

// recordSuccessForApiKeyWithCache is recordSuccessForApiKey plus prompt-cache
// token attribution (cache read / creation input tokens) for the display-only
// detail log — the Claude paths know these, and the usage panels use them to
// compute cache hit rate. Billing is unaffected (cache tokens never touch the
// authoritative counter).
func (h *Handler) recordSuccessForApiKeyWithCache(apiKeyID, model string, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int, credits float64) {
	h.recordSuccess(inputTokens, outputTokens, credits)
	if apiKeyID == "" {
		return
	}
	if err := config.RecordApiKeyUsageWithCache(apiKeyID, model, int64(inputTokens), int64(outputTokens), int64(cacheReadTokens), int64(cacheCreationTokens), credits); err != nil {
		logger.Warnf("[ApiKey] failed to record usage for key %s: %v", apiKeyID, err)
	}
}

// clientGone reports whether the client's request context is already done, i.e.
// the caller disconnected. It is checked against the OUTER request context
// (r.Context(), threaded into each handler as ctx), which distinguishes a real
// client disconnect from an idle-timeout abort: the idle timeout cancels only
// the inner per-request context created inside CallKiroAPI and leaves this outer
// ctx untouched. On a client disconnect there is no one left to serve, so the
// handler must release its slot and return silently — NOT blame the account
// (handleAccountFailure), NOT exclude/retry it, and NOT record a failure.
func clientGone(ctx context.Context) bool {
	return ctx != nil && ctx.Err() != nil
}

// noteClientDisconnect records (WARN + counter) that a request was interrupted
// by a client disconnect before the upstream metering event, so it was NOT
// billed. Observability ONLY — billing policy is intentionally unchanged
// (whether to bill on disconnect is a product decision). apiKeyID is the
// internal card id (a UUID, not the secret key value); it is shortened for the
// log. approxOutputTokens is a best-effort estimate of what had already streamed.
func (h *Handler) noteClientDisconnect(endpoint, model, apiKeyID string, approxOutputTokens int) {
	n := atomic.AddInt64(&h.clientDisconnects, 1)
	logger.Warnf("[Billing] client disconnect before metering; request not billed: endpoint=%s model=%s apiKey=%s approxOutputTokens=%d totalClientDisconnects=%d",
		endpoint, model, shortID(apiKeyID), approxOutputTokens, n)
}

// shortID returns a log-friendly prefix of an internal id (UUID). Not a secret,
// but shortened to keep logs tidy.
func shortID(id string) string {
	if id == "" {
		return "(none)"
	}
	if len(id) <= 8 {
		return id
	}
	return id[:8] + "…"
}

// recordFailureWithDetails records a failure and stores it in the request logs.
func (h *Handler) recordFailureWithDetails(endpoint, model, accountID string, err error) {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.failedRequests, 1)

	if err == nil {
		return
	}

	errMsg := err.Error()
	errType := classifyError(errMsg)

	entry := RequestLog{
		Time:      time.Now().Unix(),
		Endpoint:  endpoint,
		Model:     model,
		AccountID: accountID,
		Status:    "error",
		Error:     errMsg,
		ErrorType: errType,
	}

	h.appendRequestLog(entry)
}

// recordSuccessLog records a successful request in the request logs.
func (h *Handler) recordSuccessLog(endpoint, model, accountID string, tokens int, credits float64, durationMs int64) {
	entry := RequestLog{
		Time:      time.Now().Unix(),
		Endpoint:  endpoint,
		Model:     model,
		AccountID: accountID,
		Status:    "success",
		Tokens:    tokens,
		Credits:   credits,
		Duration:  durationMs,
	}

	h.appendRequestLog(entry)
}

func (h *Handler) appendRequestLog(entry RequestLog) {
	h.requestLogsMu.Lock()
	if h.requestLogs == nil {
		h.requestLogs = make([]RequestLog, 0, requestLogsMaxSize)
	}
	if len(h.requestLogs) >= requestLogsMaxSize {
		h.requestLogs = h.requestLogs[1:]
	}
	h.requestLogs = append(h.requestLogs, entry)
	h.requestLogsMu.Unlock()
	// Persist to the DB audit trail (async, no-op in JSON mode).
	enqueueRequestLogDB(entry)
}

// classifyError categorizes an error message into a type for display.
func classifyError(msg string) string {
	switch {
	case isQuotaErrorMessage(msg):
		return "quota"
	case isOverageErrorMessage(msg):
		return "overage"
	case isSuspensionErrorMessage(msg):
		return "suspended"
	case isAuthErrorMessage(msg):
		return "auth"
	case isProfileUnavailableErrorMessage(msg):
		return "profile"
	default:
		return "unknown"
	}
}

// getRequestLogs returns request logs (newest first). Prefers the durable DB audit
// trail when the PostgreSQL backend is active; falls back to the in-memory ring.
func (h *Handler) getRequestLogs() []RequestLog {
	if logs, ok := listRequestLogsDB(1000); ok {
		return logs
	}
	h.requestLogsMu.RLock()
	defer h.requestLogsMu.RUnlock()
	if len(h.requestLogs) == 0 {
		return []RequestLog{}
	}
	result := make([]RequestLog, len(h.requestLogs))
	for i, e := range h.requestLogs {
		result[len(h.requestLogs)-1-i] = e
	}
	return result
}

// handleClaudeNonStream Claude 非流式响应
func (h *Handler) handleClaudeNonStream(ctx context.Context, w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string) {
	excluded := make(map[string]bool)
	var lastErr error
	reqStart := time.Now()

	conversationID := payload.ConversationState.AgentContinuationId
	bypassFairness := apiKeyID == ""
	keyFloor := 0
	var boundAccountIDs []string
	if e := config.GetApiKeyEntry(apiKeyID); e != nil {
		keyFloor = e.MaxConcurrency
		boundAccountIDs = e.BoundAccountIDs
	}
	// Panic-safety net: guarantee the acquired concurrency slot is released even
	// if a handler/callback panics between Acquire and the explicit release.
	// releaseSlot is idempotent (sync.Once), so the normal path's explicit
	// release and this deferred guard never double-release. On Acquire failure
	// releaseSlot is nil, so activeRelease stays nil and the guard is a no-op.
	var activeRelease func()
	defer func() {
		if activeRelease != nil {
			activeRelease()
		}
	}()
	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account, releaseSlot, aerr := h.pool.Acquire(apiKeyID, keyFloor, bypassFairness, conversationID, model, excluded, boundAccountIDs)
		activeRelease = releaseSlot
		if aerr == pool.ErrTooBusy {
			h.sendClaudeError(w, 429, "rate_limit_error", "Too many concurrent requests for this key; retry shortly")
			return
		}
		if aerr != nil {
			break // ErrNoAccount → 无可用账号
		}
		if err := h.ensureValidToken(&account); err != nil {
			releaseSlot()
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(&account, err)
			continue
		}
		cacheUsage := h.promptCache.Compute(account.ID, cacheProfile)

		var content string
		var thinkingContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var meteringCacheRead, meteringCacheCreation int
		var hasCacheMetering bool

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					thinkingContent += text
				} else {
					content += text
				}
			},
			OnToolUse: func(tu KiroToolUse) {
				toolUses = append(toolUses, tu)
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
			OnCacheMetering: func(read, creation int) {
				meteringCacheRead, meteringCacheCreation = read, creation
				hasCacheMetering = true
			},
		}

		err := callKiroWithSelfHeal(ctx, &account, payload, callback)
		if err != nil {
			// Client disconnected → release and return silently (see clientGone).
			if clientGone(ctx) {
				releaseSlot()
				h.noteClientDisconnect("claude", model, apiKeyID,
					estimateApproxTokens(content)+estimateApproxTokens(thinkingContent))
				return
			}
			releaseSlot()
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(&account, err)
			continue
		}

		thinkingFormat := thinkingOpts.Format
		finalContent, extractedReasoning := extractThinkingFromContent(content)
		rawThinkingContent := thinkingContent
		if thinking && rawThinkingContent == "" && extractedReasoning != "" {
			rawThinkingContent = extractedReasoning
		}
		if !thinking {
			rawThinkingContent = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		// Final cache accounting (metering truth > local simulation, clamped).
		cacheUsage = resolvePromptCacheUsage(cacheUsage, hasCacheMetering, meteringCacheRead, meteringCacheCreation, inputTokens)
		outputTokens = estimateClaudeOutputTokens(finalContent, rawThinkingContent, toolUses)

		// 空响应检测: 零内容零工具(或近空且上下文过大) → 回错误而非静默的空 end_turn。
		if isEmptyKiroResponse(finalContent, rawThinkingContent, len(toolUses), outputTokens, inputTokens, model) {
			h.pool.RecordSuccess(account.ID)
			releaseSlot()
			oversized := emptyResponseIsOversizedContext(inputTokens, model)
			errType, errMsg := emptyResponseErrorInfo(oversized)
			h.recordFailureWithDetails("claude", model, account.ID, fmt.Errorf("empty upstream response"))
			status := 503
			if oversized {
				status = 400
			}
			h.sendClaudeError(w, status, errType, errMsg)
			return
		}

		h.recordSuccessForApiKeyWithCache(apiKeyID, model, inputTokens, outputTokens, cacheUsage.CacheReadInputTokens, cacheUsage.CacheCreationInputTokens, credits)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		releaseSlot()
		h.promptCache.Update(account.ID, cacheProfile)
		h.recordSuccessLog("claude", model, account.ID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

		responseThinkingContent := rawThinkingContent
		includeEmptyThinkingBlock := thinking && thinkingOpts.OmitDisplay && rawThinkingContent != ""
		if includeEmptyThinkingBlock {
			responseThinkingContent = ""
		}

		if thinking && responseThinkingContent != "" {
			switch thinkingFormat {
			case "think":
				finalContent = "<think>" + responseThinkingContent + "</think>" + finalContent
				responseThinkingContent = ""
			case "reasoning_content":
				finalContent = responseThinkingContent + finalContent
				responseThinkingContent = ""
			default:
			}
		}

		resp := KiroToClaudeResponse(finalContent, responseThinkingContent, includeEmptyThinkingBlock, toolUses, inputTokens, outputTokens, model)
		// 上下文写满时用更准确的 stop_reason(否则恒 end_turn/tool_use)。
		resp.StopReason = resolveClaudeStopReason(len(toolUses) > 0, false, inputTokens, model)
		resp.Usage.InputTokens = billedClaudeInputTokens(inputTokens, cacheUsage)
		resp.Usage.CacheCreationInputTokens = cacheUsage.CacheCreationInputTokens
		resp.Usage.CacheReadInputTokens = cacheUsage.CacheReadInputTokens
		if cacheProfile != nil || hasCacheMetering {
			resp.Usage.CacheCreation = &ClaudeCacheCreationUsage{
				Ephemeral5mInputTokens: cacheUsage.CacheCreation5mInputTokens,
				Ephemeral1hInputTokens: cacheUsage.CacheCreation1hInputTokens,
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	if lastErr == nil {
		h.sendClaudeError(w, 503, "api_error", "No available accounts")
		return
	}

	h.recordFailureWithDetails("claude", model, "", lastErr)
	h.sendClaudeError(w, 500, "api_error", lastErr.Error())
}

func (h *Handler) sendClaudeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}

// handleOpenAIChat OpenAI API 处理
func (h *Handler) handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
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

	var req OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	if msg := validateOpenAIRequestShape(&req); msg != "" {
		h.sendOpenAIError(w, 400, "invalid_request_error", msg)
		return
	}

	// 解析模型和 thinking 模式
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)
	req.Model = actualModel
	estimatedInputTokens := estimateOpenAIRequestInputTokens(&req)
	cacheProfile := h.promptCache.BuildOpenAIProfile(&req, estimatedInputTokens)

	kiroPayload := OpenAIToKiro(&req, thinking)

	apiKeyID := apiKeyIDFromContext(r.Context())
	if req.Stream {
		h.handleOpenAIStream(r.Context(), w, kiroPayload, req.Model, thinking, estimatedInputTokens, cacheProfile, apiKeyID)
	} else {
		h.handleOpenAINonStream(r.Context(), w, kiroPayload, req.Model, thinking, estimatedInputTokens, cacheProfile, apiKeyID)
	}
}

// handleOpenAIStream OpenAI 流式响应
func (h *Handler) handleOpenAIStream(ctx context.Context, w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	// 获取 thinking 输出格式配置
	thinkingFormat := config.GetThinkingConfig().OpenAIFormat

	chatID := "chatcmpl-" + uuid.New().String()
	excluded := make(map[string]bool)
	var lastErr error
	reqStart := time.Now()

	conversationID := payload.ConversationState.AgentContinuationId
	bypassFairness := apiKeyID == ""
	keyFloor := 0
	var boundAccountIDs []string
	if e := config.GetApiKeyEntry(apiKeyID); e != nil {
		keyFloor = e.MaxConcurrency
		boundAccountIDs = e.BoundAccountIDs
	}
	// Panic-safety net: guarantee the acquired concurrency slot is released even
	// if a handler/callback panics between Acquire and the explicit release.
	// releaseSlot is idempotent (sync.Once), so the normal path's explicit
	// release and this deferred guard never double-release. On Acquire failure
	// releaseSlot is nil, so activeRelease stays nil and the guard is a no-op.
	var activeRelease func()
	defer func() {
		if activeRelease != nil {
			activeRelease()
		}
	}()
	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account, releaseSlot, aerr := h.pool.Acquire(apiKeyID, keyFloor, bypassFairness, conversationID, model, excluded, boundAccountIDs)
		activeRelease = releaseSlot
		if aerr == pool.ErrTooBusy {
			h.sendOpenAIError(w, 429, "rate_limit_error", "Too many concurrent requests for this key; retry shortly")
			return
		}
		if aerr != nil {
			break // ErrNoAccount → 无可用账号
		}
		if err := h.ensureValidToken(&account); err != nil {
			releaseSlot()
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(&account, err)
			continue
		}
		cacheUsage := h.promptCache.Compute(account.ID, cacheProfile)

		var toolCalls []ToolCall
		var toolCallIndex int
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var meteringCacheRead, meteringCacheCreation int
		var hasCacheMetering bool
		var rawContentBuilder strings.Builder
		var rawReasoningBuilder strings.Builder
		var textBuffer string
		var inThinkingBlock bool
		var dropTagThinking bool
		var thinkingSource thinkingStreamSource
		var thinkingStarted bool
		var eventThinkingOpen bool
		responseStarted := false

		sendChunk := func(content string, thinkingState int) {
			if content == "" && thinkingState == 2 {
				return
			}

			var chunk map[string]interface{}

			if thinkingState > 0 {
				if !thinking {
					return
				}
				switch thinkingFormat {
				case "thinking":
					var text string
					switch thinkingState {
					case 1:
						text = "<thinking>" + content
					case 2:
						text = content
					case 3:
						text = content + "</thinking>"
					}
					if text == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"content": text},
							"finish_reason": nil,
						}},
					}
				case "think":
					var text string
					switch thinkingState {
					case 1:
						text = "<think>" + content
					case 2:
						text = content
					case 3:
						text = content + "</think>"
					}
					if text == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"content": text},
							"finish_reason": nil,
						}},
					}
				default:
					if content == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"reasoning_content": content},
							"finish_reason": nil,
						}},
					}
				}
			} else {
				if content == "" {
					return
				}
				chunk = map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{{
						"index":         0,
						"delta":         map[string]string{"content": content},
						"finish_reason": nil,
					}},
				}
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			flusher.Flush()
			responseStarted = true
		}

		processText := func(text string, isThinking bool, forceFlush bool) {
			if isThinking && !thinking {
				return
			}

			if isThinking {
				if !allowReasoningSource(&thinkingSource) {
					return
				}
				if !thinkingStarted {
					sendChunk(text, 1)
					thinkingStarted = true
					eventThinkingOpen = true
				} else {
					sendChunk(text, 2)
				}
				return
			}

			if eventThinkingOpen {
				sendChunk("", 3)
				eventThinkingOpen = false
				thinkingStarted = false
			}

			textBuffer += text

			for {
				if !inThinkingBlock {
					thinkingStart := strings.Index(textBuffer, "<thinking>")
					if thinkingStart != -1 {
						if thinkingStart > 0 {
							sendChunk(textBuffer[:thinkingStart], 0)
						}
						textBuffer = textBuffer[thinkingStart+10:]
						inThinkingBlock = true
						dropTagThinking = !allowTagSource(&thinkingSource)
						thinkingStarted = false
					} else if forceFlush || len([]rune(textBuffer)) > 50 {
						runes := []rune(textBuffer)
						safeLen := len(runes)
						if !forceFlush {
							safeLen = max(0, len(runes)-15)
						}
						if safeLen > 0 {
							sendChunk(string(runes[:safeLen]), 0)
							textBuffer = string(runes[safeLen:])
						}
						break
					} else {
						break
					}
				} else {
					thinkingEnd := strings.Index(textBuffer, "</thinking>")
					if thinkingEnd != -1 {
						content := textBuffer[:thinkingEnd]
						if !dropTagThinking {
							if !thinkingStarted {
								sendChunk(content, 1)
								sendChunk("", 3)
							} else {
								sendChunk(content, 3)
							}
						}
						textBuffer = textBuffer[thinkingEnd+11:]
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
					} else if forceFlush {
						if textBuffer != "" {
							if !dropTagThinking {
								if !thinkingStarted {
									sendChunk(textBuffer, 1)
									sendChunk("", 3)
								} else {
									sendChunk(textBuffer, 3)
								}
							}
							textBuffer = ""
						}
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
						break
					} else {
						runes := []rune(textBuffer)
						if len(runes) > 20 {
							safeLen := len(runes) - 15
							if safeLen > 0 {
								if !dropTagThinking {
									if !thinkingStarted {
										sendChunk(string(runes[:safeLen]), 1)
										thinkingStarted = true
									} else {
										sendChunk(string(runes[:safeLen]), 2)
									}
								}
								textBuffer = string(runes[safeLen:])
							}
						}
						break
					}
				}
			}
		}

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				if isThinking {
					rawReasoningBuilder.WriteString(text)
				} else {
					rawContentBuilder.WriteString(text)
				}
				processText(text, isThinking, false)
			},
			OnToolUse: func(tu KiroToolUse) {
				processText("", false, true)

				args, _ := json.Marshal(tu.Input)
				rawContentBuilder.WriteString(tu.Name)
				rawContentBuilder.Write(args)
				tc := ToolCall{ID: tu.ToolUseID, Type: "function"}
				tc.Function.Name = tu.Name
				tc.Function.Arguments = string(args)
				toolCalls = append(toolCalls, tc)

				chunk := map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{{
						"index": 0,
						"delta": map[string]interface{}{
							"tool_calls": []map[string]interface{}{{
								"index": toolCallIndex,
								"id":    tu.ToolUseID,
								"type":  "function",
								"function": map[string]string{
									"name":      tu.Name,
									"arguments": string(args),
								},
							}},
						},
						"finish_reason": nil,
					}},
				}
				toolCallIndex++
				data, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", string(data))
				flusher.Flush()
				responseStarted = true
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
			OnCacheMetering: func(read, creation int) {
				meteringCacheRead, meteringCacheCreation = read, creation
				hasCacheMetering = true
			},
		}

		err := callKiroWithSelfHeal(ctx, &account, payload, callback)
		if err != nil {
			// Client disconnected → release and return silently (see clientGone).
			if clientGone(ctx) {
				releaseSlot()
				h.noteClientDisconnect("openai", model, apiKeyID,
					estimateApproxTokens(rawContentBuilder.String())+estimateApproxTokens(rawReasoningBuilder.String()))
				return
			}
			releaseSlot()
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(&account, err)
			if !responseStarted {
				continue
			}
			h.recordFailureWithDetails("openai", model, account.ID, err)
			return
		}

		processText("", false, true)
		if eventThinkingOpen {
			sendChunk("", 3)
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		cacheUsage = resolvePromptCacheUsage(cacheUsage, hasCacheMetering, meteringCacheRead, meteringCacheCreation, inputTokens)
		outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
		reasoningOutput := rawReasoningBuilder.String()
		if thinking && reasoningOutput == "" && extractedReasoning != "" {
			reasoningOutput = extractedReasoning
		}
		if !thinking {
			reasoningOutput = ""
		}
		outputTokens = estimateApproxTokens(outputContent) + estimateApproxTokens(reasoningOutput)
		for _, tc := range toolCalls {
			outputTokens += estimateApproxTokens(tc.Function.Name)
			outputTokens += estimateApproxTokens(tc.Function.Arguments)
		}

		h.recordSuccessForApiKeyWithCache(apiKeyID, model, inputTokens, outputTokens, cacheUsage.CacheReadInputTokens, cacheUsage.CacheCreationInputTokens, credits)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		// 修复既有并发名额泄漏:此前成功路径从不 releaseSlot,inflight 只增不减,
		// 卡密公平准入迟早被顶死到 429。
		releaseSlot()
		h.promptCache.Update(account.ID, cacheProfile)
		h.recordSuccessLog("openai", model, account.ID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

		finishReason := openAIFinishReason(len(toolCalls) > 0, false, inputTokens, model)

		chunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]interface{}{{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": finishReason,
			}},
			"usage": buildOpenAIUsageMap(inputTokens, outputTokens, cacheUsage),
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", string(data))
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	if lastErr == nil {
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}

	h.recordFailureWithDetails("openai", model, "", lastErr)
	h.sendOpenAIError(w, 500, "server_error", lastErr.Error())
}

// handleOpenAINonStream OpenAI 非流式响应
//
// 与其它五条路径同构:走 pool.Acquire(公平准入/并发计数/粘性/RPM,拿到的是账号值拷贝,
// 修复了旧 GetNextForModelExcluding 返回池内指针后 ensureValidToken 无锁写 token 与
// 调度器读写竞态的问题)+ callKiroWithSelfHeal(可自愈 400 同账号重试一次)。
func (h *Handler) handleOpenAINonStream(ctx context.Context, w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string) {
	excluded := make(map[string]bool)
	var lastErr error
	reqStart := time.Now()

	conversationID := payload.ConversationState.AgentContinuationId
	bypassFairness := apiKeyID == ""
	keyFloor := 0
	var boundAccountIDs []string
	if e := config.GetApiKeyEntry(apiKeyID); e != nil {
		keyFloor = e.MaxConcurrency
		boundAccountIDs = e.BoundAccountIDs
	}
	// Panic-safety net: guarantee the acquired concurrency slot is released even
	// if a handler/callback panics between Acquire and the explicit release.
	// releaseSlot is idempotent (sync.Once), so the normal path's explicit
	// release and this deferred guard never double-release. On Acquire failure
	// releaseSlot is nil, so activeRelease stays nil and the guard is a no-op.
	var activeRelease func()
	defer func() {
		if activeRelease != nil {
			activeRelease()
		}
	}()
	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account, releaseSlot, aerr := h.pool.Acquire(apiKeyID, keyFloor, bypassFairness, conversationID, model, excluded, boundAccountIDs)
		activeRelease = releaseSlot
		if aerr == pool.ErrTooBusy {
			h.sendOpenAIError(w, 429, "rate_limit_error", "Too many concurrent requests for this key; retry shortly")
			return
		}
		if aerr != nil {
			break // ErrNoAccount → 无可用账号
		}
		if err := h.ensureValidToken(&account); err != nil {
			releaseSlot()
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(&account, err)
			continue
		}
		cacheUsage := h.promptCache.Compute(account.ID, cacheProfile)

		var content string
		var reasoningContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var meteringCacheRead, meteringCacheCreation int
		var hasCacheMetering bool

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
		}

		err := callKiroWithSelfHeal(ctx, &account, payload, callback)
		if err != nil {
			// Client disconnected → release and return silently (see clientGone).
			if clientGone(ctx) {
				releaseSlot()
				h.noteClientDisconnect("openai", model, apiKeyID,
					estimateApproxTokens(content)+estimateApproxTokens(reasoningContent))
				return
			}
			releaseSlot()
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(&account, err)
			continue
		}

		finalContent, extractedReasoning := extractThinkingFromContent(content)
		if thinking && reasoningContent == "" && extractedReasoning != "" {
			reasoningContent = extractedReasoning
		} else if !thinking {
			reasoningContent = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		cacheUsage = resolvePromptCacheUsage(cacheUsage, hasCacheMetering, meteringCacheRead, meteringCacheCreation, inputTokens)
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)

		h.recordSuccessForApiKeyWithCache(apiKeyID, model, inputTokens, outputTokens, cacheUsage.CacheReadInputTokens, cacheUsage.CacheCreationInputTokens, credits)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		releaseSlot()
		h.promptCache.Update(account.ID, cacheProfile)
		h.recordSuccessLog("openai", model, account.ID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

		thinkingFormat := config.GetThinkingConfig().OpenAIFormat
		resp := KiroToOpenAIResponseWithReasoning(finalContent, reasoningContent, toolUses, inputTokens, outputTokens, model, thinkingFormat, cacheUsage)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	if lastErr == nil {
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}

	h.recordFailureWithDetails("openai", model, "", lastErr)
	h.sendOpenAIError(w, 500, "server_error", lastErr.Error())
}

func (h *Handler) sendOpenAIError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"type":    errType,
			"message": message,
		},
	})
}

// accountRefreshLock returns the per-account refresh mutex, creating it on first
// use. The tiny map guard is held only to fetch/insert the mutex, never across
// the refresh itself.
func (h *Handler) accountRefreshLock(id string) *sync.Mutex {
	h.tokenRefreshMu.Lock()
	defer h.tokenRefreshMu.Unlock()
	if h.tokenRefreshLocks == nil {
		h.tokenRefreshLocks = make(map[string]*sync.Mutex)
	}
	mu := h.tokenRefreshLocks[id]
	if mu == nil {
		mu = &sync.Mutex{}
		h.tokenRefreshLocks[id] = mu
	}
	return mu
}

// ensureValidToken 确保 token 有效
func (h *Handler) ensureValidToken(account *config.Account) error {
	if account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds {
		return nil
	}

	// Serialize refreshes for THIS account only, so a slow refresh of one account
	// never blocks requests to other accounts (the old single global mutex did).
	mu := h.accountRefreshLock(account.ID)
	mu.Lock()
	defer mu.Unlock()

	// Another concurrent request may have refreshed this account while we waited.
	if latest := h.pool.GetByID(account.ID); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
		if account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds {
			return nil
		}
	}

	accessToken, refreshToken, expiresAt, profileArn, err := auth.RefreshToken(account)
	if err != nil {
		return err
	}

	// 更新内存
	h.pool.UpdateToken(account.ID, accessToken, refreshToken, expiresAt)
	account.AccessToken = accessToken
	if refreshToken != "" {
		account.RefreshToken = refreshToken
	}
	account.ExpiresAt = expiresAt
	if profileArn != "" {
		account.ProfileArn = profileArn
		config.UpdateAccountProfileArn(account.ID, profileArn)
	}

	// 持久化
	config.UpdateAccountToken(account.ID, accessToken, refreshToken, expiresAt)

	return nil
}

// ==================== 管理 API ====================

func (h *Handler) handleAdminAPI(w http.ResponseWriter, r *http.Request) {
	// 验证密码
	password := r.Header.Get("X-Admin-Password")
	if password == "" {
		cookie, _ := r.Cookie("admin_password")
		if cookie != nil {
			password = cookie.Value
		}
	}

	// Brute-force throttle: block an IP that has failed admin auth too many times.
	ip := clientIP(r)
	if ok, retryAfter := adminAuthAllowed(ip); !ok {
		secs := int(retryAfter.Seconds())
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		w.WriteHeader(429)
		json.NewEncoder(w).Encode(map[string]string{"error": "too many failed attempts; try again later"})
		return
	}

	if stored := config.GetPassword(); stored == "" || subtle.ConstantTimeCompare([]byte(password), []byte(stored)) != 1 {
		adminAuthRecordFailure(ip)
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
		return
	}
	adminAuthRecordSuccess(ip)

	path := strings.TrimPrefix(r.URL.Path, "/admin/api")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	switch {
	case path == "/accounts" && r.Method == "GET":
		h.apiGetAccounts(w, r)
	case path == "/accounts" && r.Method == "POST":
		h.apiAddAccount(w, r)
	case path == "/accounts/batch" && r.Method == "POST":
		h.apiBatchAccounts(w, r)
	// 手动刷新端点(账号/模型/超额)已删除:数据新鲜度由后台定时刷新(5min)全权负责,
	// 管理网页只读渲染。见 backgroundRefresh / RefreshAccountInfo。
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/test") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/test")
		h.apiTestAccount(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models/cached") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models/cached")
		h.apiGetAccountModelsCached(w, r, id)

	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/overage") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/overage")
		h.apiSetAccountOverage(w, r, id)

	// 单账号详情(与 /accounts 列表项同 shape;弹窗打开时拉取)。必须在所有
	// /accounts/{id}/xxx 子路由之后匹配:仅接受不含 "/" 的裸 id。
	case strings.HasPrefix(path, "/accounts/") && r.Method == "GET" && !strings.Contains(strings.TrimPrefix(path, "/accounts/"), "/"):
		h.apiGetAccount(w, r, strings.TrimPrefix(path, "/accounts/"))
	case strings.HasPrefix(path, "/accounts/") && r.Method == "DELETE":
		h.apiDeleteAccount(w, r, strings.TrimPrefix(path, "/accounts/"))
	case strings.HasPrefix(path, "/accounts/") && r.Method == "PUT":
		h.apiUpdateAccount(w, r, strings.TrimPrefix(path, "/accounts/"))
	case path == "/auth/iam-sso/start" && r.Method == "POST":
		h.apiStartIamSso(w, r)
	case path == "/auth/iam-sso/complete" && r.Method == "POST":
		h.apiCompleteIamSso(w, r)
	case path == "/auth/builderid/start" && r.Method == "POST":
		h.apiStartBuilderIdLogin(w, r)
	case path == "/auth/builderid/poll" && r.Method == "POST":
		h.apiPollBuilderIdAuth(w, r)
	case path == "/auth/sso-token" && r.Method == "POST":
		h.apiImportSsoToken(w, r)
	case path == "/auth/credentials" && r.Method == "POST":
		h.apiImportCredentials(w, r)
	// Social 登录(app.kiro.dev,Google/GitHub/Microsoft/Amazon/邮箱):复用 Builder ID 授权码流程,
	// 产出 idc 账号,后续走标准 OIDC 刷新与 AWS 数据面。
	case path == "/auth/social/start" && r.Method == "POST":
		h.apiStartSocialLogin(w, r)
	case path == "/auth/social/complete" && r.Method == "POST":
		h.apiCompleteSocialLogin(w, r)
	// Kiro API Key(ksk_)headless 账号:直接作 Bearer,不刷新。
	case path == "/auth/api-key" && r.Method == "POST":
		h.apiImportApiKey(w, r)
	case path == "/overview" && r.Method == "GET":
		h.apiGetOverview(w, r)
	case path == "/status" && r.Method == "GET":
		h.apiGetStatus(w, r)
	case path == "/settings" && r.Method == "GET":
		h.apiGetSettings(w, r)
	case path == "/settings" && r.Method == "POST":
		h.apiUpdateSettings(w, r)
	case path == "/stats" && r.Method == "GET":
		h.apiGetStats(w, r)
	case path == "/logs" && r.Method == "GET":
		h.apiGetLogs(w, r)
	case path == "/logs" && r.Method == "DELETE":
		h.apiClearLogs(w, r)
	case path == "/generate-machine-id" && r.Method == "GET":
		h.apiGenerateMachineId(w, r)
	case path == "/thinking" && r.Method == "GET":
		h.apiGetThinkingConfig(w, r)
	case path == "/thinking" && r.Method == "POST":
		h.apiUpdateThinkingConfig(w, r)
	case path == "/endpoint" && r.Method == "GET":
		h.apiGetEndpointConfig(w, r)
	case path == "/endpoint" && r.Method == "POST":
		h.apiUpdateEndpointConfig(w, r)
	case path == "/proxy" && r.Method == "GET":
		h.apiGetProxy(w, r)
	case path == "/proxy" && r.Method == "POST":
		h.apiUpdateProxy(w, r)
	case path == "/prompt-filter" && r.Method == "GET":
		h.apiGetPromptFilter(w, r)
	case path == "/prompt-filter" && r.Method == "POST":
		h.apiUpdatePromptFilter(w, r)
	case path == "/version" && r.Method == "GET":
		h.apiGetVersion(w, r)
	case path == "/export" && r.Method == "POST":
		h.apiExportAccounts(w, r)
	case path == "/api-keys" && r.Method == "GET":
		h.apiListApiKeys(w, r)
	case path == "/api-keys" && r.Method == "POST":
		h.apiCreateApiKey(w, r)
	case strings.HasPrefix(path, "/api-keys/") && strings.HasSuffix(path, "/reset-usage") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api-keys/"), "/reset-usage")
		h.apiResetApiKeyUsage(w, r, id)
	case strings.HasPrefix(path, "/api-keys/") && strings.HasSuffix(path, "/topup") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api-keys/"), "/topup")
		h.apiTopupApiKey(w, r, id)
	case strings.HasPrefix(path, "/api-keys/") && strings.HasSuffix(path, "/recharges") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api-keys/"), "/recharges")
		h.apiApiKeyRecharges(w, r, id)
	case strings.HasPrefix(path, "/api-keys/") && strings.HasSuffix(path, "/usage/records") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api-keys/"), "/usage/records")
		h.apiApiKeyUsageRecords(w, r, id)
	case strings.HasPrefix(path, "/api-keys/") && strings.HasSuffix(path, "/usage") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api-keys/"), "/usage")
		h.apiApiKeyUsage(w, r, id)
	case strings.HasPrefix(path, "/api-keys/") && strings.HasSuffix(path, "/children") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api-keys/"), "/children")
		h.apiApiKeyChildren(w, r, id)
	case path == "/concurrency" && r.Method == "GET":
		h.apiConcurrency(w, r)
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "GET":
		h.apiGetApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "PUT":
		h.apiUpdateApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "DELETE":
		h.apiDeleteApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	default:
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Not Found"})
	}
}

// accountSummaryMap builds the JSON shape shared by the /accounts list and the
// single-account GET /accounts/{id} (detail modal): persisted fields + runtime
// stats + live RPM. `stats` carries the runtime counters (pool copy when the
// account is schedulable, else the persisted account itself — disabled/over-quota
// accounts are not in the pool and would otherwise show zeros).
func accountSummaryMap(a *config.Account, stats *config.Account, rpm int) map[string]interface{} {
	return map[string]interface{}{
		"id":         a.ID,
		"email":      a.Email,
		"userId":     a.UserId,
		"nickname":   a.Nickname,
		"authMethod": a.AuthMethod,
		"provider":   a.Provider,
		"region":     a.Region,
		"createdAt":  a.CreatedAt,
		"enabled":    a.Enabled,
		"banStatus":  a.BanStatus,
		"banReason":  a.BanReason,
		"banTime":    a.BanTime,
		"expiresAt":  a.ExpiresAt,
		"hasToken":   a.AccessToken != "",
		// canRefresh: an expired access token is normal and self-healing for
		// accounts that can renew it — OAuth/IdC accounts with a refresh token,
		// and api_key (ksk_) accounts whose key is itself the long-lived bearer
		// (never expires). The UI uses this to avoid flashing a false "expired"
		// badge in the brief window between token TTL and the next refresh.
		"canRefresh":        a.RefreshToken != "" || auth.IsApiKeyAccount(a),
		"machineId":         a.MachineId,
		"weight":            a.Weight,
		"overageStatus":     a.OverageStatus,
		"overageCapability": a.OverageCapability,
		"overageCap":        a.OverageCap,
		"overageRate":       a.OverageRate,
		"currentOverages":   a.CurrentOverages,
		"overageCheckedAt":  a.OverageCheckedAt,
		"proxyURL":          a.ProxyURL,
		"subscriptionType":  a.SubscriptionType,
		"subscriptionTitle": a.SubscriptionTitle,
		"daysRemaining":     a.DaysRemaining,
		"usageCurrent":      a.UsageCurrent,
		"usageLimit":        a.UsageLimit,
		"usagePercent":      a.UsagePercent,
		"nextResetDate":     a.NextResetDate,
		"lastRefresh":       a.LastRefresh,
		"trialUsageCurrent": a.TrialUsageCurrent,
		"trialUsageLimit":   a.TrialUsageLimit,
		"trialUsagePercent": a.TrialUsagePercent,
		"trialStatus":       a.TrialStatus,
		"trialExpiresAt":    a.TrialExpiresAt,
		"requestCount":      stats.RequestCount,
		"errorCount":        stats.ErrorCount,
		"totalTokens":       stats.TotalTokens,
		"totalCredits":      stats.TotalCredits,
		"lastUsed":          stats.LastUsed,
		"rpm":               rpm,
	}
}

func (h *Handler) apiGetAccounts(w http.ResponseWriter, r *http.Request) {
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()

	// 合并运行时统计
	statsMap := make(map[string]config.Account)
	for _, a := range poolAccounts {
		statsMap[a.ID] = a
	}
	rpmByAcct, _, _ := h.pool.RPMSnapshot()

	// 运行时统计 + 实时 RPM
	result := make([]map[string]interface{}, len(accounts))
	for i := range accounts {
		a := &accounts[i]
		// 获取运行时统计。池子只装可调度账号(启用且未被配额挡住),被禁用/超额
		// 的账号不在池里 → 回退 config 里持久化的累计值,统计不再显示为 0。
		stats, inPool := statsMap[a.ID]
		if !inPool {
			stats = *a
		}
		result[i] = accountSummaryMap(a, &stats, rpmByAcct[a.ID])
	}
	json.NewEncoder(w).Encode(result)
}

// apiGetAccount GET /admin/api/accounts/{id} — one account in exactly the same
// shape as one /accounts list item. The detail modal fetches this on open so it
// renders CURRENT data (auto-refresh polling pauses while a modal is open, so
// the in-memory list snapshot could be stale).
func (h *Handler) apiGetAccount(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	stats := *account
	if inPool := h.pool.GetByID(id); inPool != nil {
		stats = *inPool
	}
	json.NewEncoder(w).Encode(accountSummaryMap(account, &stats, h.pool.AccountRPM(id)))
}

func (h *Handler) apiAddAccount(w http.ResponseWriter, r *http.Request) {
	var account config.Account
	if err := json.NewDecoder(r.Body).Decode(&account); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if account.ID == "" {
		account.ID = auth.GenerateAccountID()
	}
	if account.Region == "" {
		account.Region = "us-east-1"
	}

	if err := config.AddOrReplaceAccount(&account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// 新账号若已启用且有 token，立即拉取并缓存模型列表
	if account.Enabled && account.AccessToken != "" {
		go func(acc config.Account) {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for new account %s: %v", acc.Email, err)
			}
		}(account)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": account.ID})
}

func (h *Handler) apiDeleteAccount(w http.ResponseWriter, r *http.Request, id string) {
	if err := config.DeleteAccount(id); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiUpdateAccount(w http.ResponseWriter, r *http.Request, id string) {
	var updates map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 获取现有账号
	accounts := config.GetAccounts()
	var existing *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			existing = &accounts[i]
			break
		}
	}
	if existing == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// 只更新传入的字段
	oldEnabled := existing.Enabled
	if v, ok := updates["enabled"].(bool); ok {
		existing.Enabled = v
	}
	if v, ok := updates["nickname"].(string); ok {
		existing.Nickname = v
	}
	if v, ok := updates["machineId"].(string); ok {
		existing.MachineId = v
	}
	if v, ok := updates["weight"].(float64); ok {
		existing.Weight = int(v)
	}
	if v, ok := updates["proxyURL"].(string); ok {
		existing.ProxyURL = v
	}

	if err := config.UpdateAccount(id, *existing); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// 账号从禁用→启用时，自动拉取并缓存模型列表
	if !oldEnabled && existing.Enabled && existing.AccessToken != "" {
		go func(acc config.Account) {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for re-enabled account %s: %v", acc.Email, err)
			}
		}(*existing)
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiSetAccountOverage 翻转单个账号的上游 Overages 开关，并刷新缓存。
// Body: {"enabled": true|false}
func (h *Handler) apiSetAccountOverage(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	snap, err := SetOverageStatus(account, body.Enabled)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if persistErr := PersistOverageSnapshot(id, snap); persistErr != nil {
		logger.Warnf("[Overage] persist SET overage failed for %s: %v", account.Email, persistErr)
	}
	h.pool.Reload()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"overageStatus":     snap.Status,
		"overageCapability": snap.Capability,
		"subscriptionTitle": snap.SubscriptionTitle,
		"overageCap":        snap.OverageCap,
		"overageRate":       snap.OverageRate,
		"currentOverages":   snap.CurrentOverages,
		"overageCheckedAt":  snap.CheckedAt,
	})
}

// apiBatchAccounts 批量操作账号（启用/禁用）。批量"刷新"action 已随前端手动
// 刷新触发一并删除——额度/订阅/overage 由后台每 5min 定时刷新兜底。
func (h *Handler) apiBatchAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"` // "enable", "disable"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if len(req.IDs) == 0 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "No account IDs provided"})
		return
	}

	switch req.Action {
	case "enable", "disable":
		enabled := req.Action == "enable"
		accounts := config.GetAccounts()
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var toRefreshModels []config.Account
		for _, a := range accounts {
			if idSet[a.ID] {
				// 记录本次从禁用→启用、且有 token 的账号
				if enabled && !a.Enabled && a.AccessToken != "" {
					toRefreshModels = append(toRefreshModels, a)
				}
				a.Enabled = enabled
				if enabled && a.BanStatus != "" && a.BanStatus != "ACTIVE" {
					a.BanStatus = "ACTIVE"
					a.BanReason = ""
					a.BanTime = 0
				}
				config.UpdateAccount(a.ID, a)
			}
		}
		h.pool.Reload()
		// 为本次新启用的账号异步拉取模型缓存
		for _, acc := range toRefreshModels {
			go func(a config.Account) {
				a.Enabled = true
				if err := h.fetchAndCacheAccountModels(&a); err != nil {
					logger.Warnf("[ModelsCache] Auto-refresh failed for batch-enabled account %s: %v", a.Email, err)
				}
			}(acc)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "count": len(req.IDs)})

	default:
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid action: " + req.Action})
	}
}

func (h *Handler) apiStartIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StartUrl string `json:"startUrl"`
		Region   string `json:"region"`
		Name     string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.StartUrl == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "startUrl is required"})
		return
	}

	// region 留空时后端自动探测门户所属区域（含跨区门户）。name 为用户填写的备注/用户名。
	sessionID, authorizeUrl, expiresIn, err := auth.StartIamSsoLogin(req.StartUrl, req.Region, req.Name)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":    sessionID,
		"authorizeUrl": authorizeUrl,
		"expiresIn":    expiresIn,
	})
}

func (h *Handler) apiCompleteIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		CallbackUrl string `json:"callbackUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, label, expiresIn, err := auth.CompleteIamSsoLogin(req.SessionID, req.CallbackUrl)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 账号标签：备注/用户名(label) 作为 nickname；列表主展示优先真实 email，取不到则回退 label。
	email, _, _ := auth.GetUserInfo(accessToken)
	if email == "" {
		email = label
	}

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		Nickname:     label,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Provider:     "Enterprise",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddOrReplaceAccount(&account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 服务端同步拉取额度/订阅并自动开 Overages（取代前端建完再调 /refresh）。
	h.hydrateNewAccount(&account)
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiStartBuilderIdLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region string `json:"region"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	session, err := auth.StartBuilderIdLogin(req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":       session.ID,
		"userCode":        session.UserCode,
		"verificationUri": session.VerificationUri,
		"interval":        session.Interval,
	})
}

func (h *Handler) apiPollBuilderIdAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, status, err := auth.PollBuilderIdAuth(req.SessionID)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	if status == "pending" || status == "slow_down" {
		// 获取当前间隔
		interval := 5
		if session := auth.GetBuilderIdSession(req.SessionID); session != nil {
			interval = session.Interval
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"completed": false,
			"status":    status,
			"interval":  interval,
		})
		return
	}

	// 授权完成，获取用户信息
	email, _, _ := auth.GetUserInfo(accessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Provider:     "BuilderId",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddOrReplaceAccount(&account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"completed": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiImportSsoToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BearerToken string `json:"bearerToken"`
		Region      string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.BearerToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "bearerToken is required"})
		return
	}

	// 支持批量导入，按行分割
	tokens := strings.Split(strings.TrimSpace(req.BearerToken), "\n")
	var imported []map[string]interface{}
	var errors []string

	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}

		accessToken, refreshToken, clientID, clientSecret, expiresIn, err := auth.ImportFromSsoToken(token, req.Region)
		if err != nil {
			errors = append(errors, err.Error())
			continue
		}

		// 获取用户信息
		email, _, _ := auth.GetUserInfo(accessToken)

		// 创建账号
		account := config.Account{
			ID:           auth.GenerateAccountID(),
			Email:        email,
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			ClientID:     clientID,
			ClientSecret: clientSecret,
			AuthMethod:   "idc",
			Region:       req.Region,
			ExpiresAt:    time.Now().Unix() + int64(expiresIn),
			Enabled:      true,
			MachineId:    config.GenerateMachineId(),
		}

		if err := config.AddOrReplaceAccount(&account); err != nil {
			errors = append(errors, err.Error())
			continue
		}

		imported = append(imported, map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		})
	}

	h.pool.Reload()

	if len(imported) == 0 && len(errors) > 0 {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   strings.Join(errors, "; "),
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"accounts": imported,
		"errors":   errors,
	})
}

func (h *Handler) apiImportCredentials(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
		AuthMethod   string `json:"authMethod"`
		Provider     string `json:"provider"`
		Region       string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.RefreshToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "refreshToken is required"})
		return
	}

	// 设置默认值
	if req.Region == "" {
		req.Region = "us-east-1"
	}
	if req.AuthMethod == "" {
		if req.ClientID != "" {
			req.AuthMethod = "idc"
		} else {
			req.AuthMethod = "social"
		}
	}
	// 标准化 authMethod
	switch strings.ToLower(req.AuthMethod) {
	case "idc", "builderid", "enterprise":
		req.AuthMethod = "idc"
	case "social", "google", "github":
		req.AuthMethod = "social"
	default:
		if req.ClientID != "" && req.ClientSecret != "" {
			req.AuthMethod = "idc"
		} else {
			req.AuthMethod = "social"
		}
	}

	// 用 refreshToken 刷新获取新的 accessToken。导入必须以一次成功的刷新为前提：
	// 本地缓存里的 accessToken 不携带可信的过期时间，盲猜短 TTL 会让账号在选号时
	// 永远被跳过，导致后台/按需刷新都无法触发（详见 ensureValidToken 与 Pick 的过期判定）。
	tempAccount := &config.Account{
		RefreshToken: req.RefreshToken,
		ClientID:     req.ClientID,
		ClientSecret: req.ClientSecret,
		AuthMethod:   req.AuthMethod,
		Region:       req.Region,
	}
	accessToken, newRefreshToken, expiresAt, newProfileArn, err := auth.RefreshToken(tempAccount)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}
	if newRefreshToken != "" {
		req.RefreshToken = newRefreshToken
	}

	// 获取用户信息
	email, _, _ := auth.GetUserInfo(accessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: req.RefreshToken,
		ClientID:     req.ClientID,
		ClientSecret: req.ClientSecret,
		AuthMethod:   req.AuthMethod,
		Provider:     req.Provider,
		Region:       req.Region,
		ExpiresAt:    expiresAt,
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
		ProfileArn:   newProfileArn,
	}

	if err := config.AddOrReplaceAccount(&account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.hydrateNewAccount(&account)
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

// apiStartSocialLogin 发起 Social 登录(app.kiro.dev:Google/GitHub/Microsoft/Amazon/邮箱)。
// 底层复用 AWS Builder ID 授权码流程(PKCE),返回 authorizeUrl 供用户浏览器打开授权。
func (h *Handler) apiStartSocialLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region string `json:"region"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	sessionID, authorizeUrl, expiresIn, err := auth.StartSocialLogin(req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":    sessionID,
		"authorizeUrl": authorizeUrl,
		"expiresIn":    expiresIn,
	})
}

// apiCompleteSocialLogin 用回调 URL(含授权码)换取 token,完成 Social 登录建号。
// 产出账号 AuthMethod=idc(后续走标准 OIDC 刷新)、Provider=Social(便于前端区分来源)。
func (h *Handler) apiCompleteSocialLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		CallbackUrl string `json:"callbackUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, err := auth.CompleteSocialLogin(req.SessionID, req.CallbackUrl)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	email, _, _ := auth.GetUserInfo(accessToken)

	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   auth.SocialAuthMethod, // "idc"
		Provider:     auth.SocialProvider,   // "Social"
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddOrReplaceAccount(&account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.hydrateNewAccount(&account)
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

// apiImportApiKey 导入 Kiro API Key(ksk_)账号(headless,authMethod=api_key,不刷新)。
// 支持批量:按行分割多个 key。key 直接作 Bearer,调用后端时带 tokentype: API_KEY 头。
func (h *Handler) apiImportApiKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ApiKey   string `json:"apiKey"`
		Region   string `json:"region"`
		Nickname string `json:"nickname"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if strings.TrimSpace(req.ApiKey) == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "apiKey is required"})
		return
	}

	keys := strings.Split(strings.TrimSpace(req.ApiKey), "\n")
	var imported []map[string]interface{}
	var errors []string

	for _, k := range keys {
		k = auth.NormalizeKiroApiKey(k)
		if k == "" {
			continue
		}
		if !strings.HasPrefix(k, auth.KiroApiKeyPrefix) {
			errors = append(errors, fmt.Sprintf("invalid key (expect %s prefix): %s", auth.KiroApiKeyPrefix, auth.MaskKiroApiKey(k)))
			continue
		}

		// 备注/标签直接用完整 API Key（不脱敏、无需手填），同时作为列表主展示。
		account := auth.NewApiKeyAccount(k, req.Region, k)
		account.Email = k
		if err := config.AddOrReplaceAccount(&account); err != nil {
			errors = append(errors, err.Error())
			continue
		}
		h.hydrateNewAccount(&account)
		imported = append(imported, map[string]interface{}{
			"id":     account.ID,
			"apiKey": k,
		})
	}

	h.pool.Reload()

	if len(imported) == 0 && len(errors) > 0 {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   strings.Join(errors, "; "),
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"accounts": imported,
		"errors":   errors,
	})
}

// apiImportExternalIdp 导入 External IdP(Microsoft Entra / Kiro 企业版)账号。
// 客户 IdP 直签 token,数据面走 runtime.{region}.kiro.dev、控制面走 management.{region}.kiro.dev,
// 调用后端时带 tokentype: EXTERNAL_IDP 头。导入以一次成功刷新为前提(校验凭证有效)。
func (h *Handler) apiImportExternalIdp(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken  string `json:"refreshToken"`
		ClientID      string `json:"clientId"`
		TokenEndpoint string `json:"tokenEndpoint"`
		IssuerUrl     string `json:"issuerUrl"`
		Scopes        string `json:"scopes"`
		Region        string `json:"region"`
		ProfileArn    string `json:"profileArn"`
		Nickname      string `json:"nickname"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if strings.TrimSpace(req.RefreshToken) == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "refreshToken is required"})
		return
	}
	if strings.TrimSpace(req.ClientID) == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "clientId is required"})
		return
	}
	if strings.TrimSpace(req.TokenEndpoint) == "" && strings.TrimSpace(req.IssuerUrl) == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "tokenEndpoint or issuerUrl is required"})
		return
	}
	if req.Region == "" {
		req.Region = "us-east-1"
	}

	// 以一次成功刷新校验凭证:本地 accessToken 无可信过期时间,盲存会让账号选号时被永远跳过。
	tempAccount := &config.Account{
		AuthMethod:    auth.ExternalIdpAuthMethod,
		RefreshToken:  req.RefreshToken,
		ClientID:      req.ClientID,
		TokenEndpoint: req.TokenEndpoint,
		IssuerUrl:     req.IssuerUrl,
		Scopes:        req.Scopes,
		Region:        req.Region,
	}
	accessToken, newRefreshToken, expiresAt, _, err := auth.RefreshToken(tempAccount)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}
	if newRefreshToken != "" {
		req.RefreshToken = newRefreshToken
	}

	account := config.Account{
		ID:            auth.GenerateAccountID(),
		Nickname:      req.Nickname,
		AccessToken:   accessToken,
		RefreshToken:  req.RefreshToken,
		ClientID:      req.ClientID,
		AuthMethod:    auth.ExternalIdpAuthMethod,
		Provider:      "ExternalIdP",
		Region:        req.Region,
		TokenEndpoint: req.TokenEndpoint,
		IssuerUrl:     req.IssuerUrl,
		Scopes:        req.Scopes,
		ProfileArn:    strings.TrimSpace(req.ProfileArn),
		ExpiresAt:     expiresAt,
		Enabled:       true,
		MachineId:     config.GenerateMachineId(),
	}

	if err := config.AddOrReplaceAccount(&account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":       account.ID,
			"nickname": account.Nickname,
		},
	})
}

func (h *Handler) apiGetStatus(w http.ResponseWriter, r *http.Request) {
	// Counters are mutated concurrently by request handlers; read them atomically
	// (and credits through its mutex) rather than racing on the plain fields.
	json.NewEncoder(w).Encode(map[string]interface{}{
		"version":         config.Version,
		"accounts":        h.pool.Count(),
		"available":       h.pool.AvailableCount(),
		"totalRequests":     atomic.LoadInt64(&h.totalRequests),
		"successRequests":   atomic.LoadInt64(&h.successRequests),
		"failedRequests":    atomic.LoadInt64(&h.failedRequests),
		"clientDisconnects": atomic.LoadInt64(&h.clientDisconnects),
		"totalTokens":       atomic.LoadInt64(&h.totalTokens),
		"totalCredits":      h.getCredits(),
		"uptime":            time.Now().Unix() - h.startTime,
	})
}

func (h *Handler) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"apiKey":         config.GetApiKey(),
		"requireApiKey":  config.IsApiKeyRequired(),
		"port":           config.GetPort(),
		"host":           config.GetHost(),
	})
}

func (h *Handler) apiGetPromptFilter(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(config.GetPromptFilterConfig())
}

func (h *Handler) apiUpdatePromptFilter(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FilterClaudeCode      *bool                      `json:"filterClaudeCode,omitempty"`
		FilterEnvNoise        *bool                      `json:"filterEnvNoise,omitempty"`
		FilterStripBoundaries *bool                      `json:"filterStripBoundaries,omitempty"`
		Rules                 *[]config.PromptFilterRule `json:"rules,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// Read current config to fill in any fields not provided in the request.
	current := config.GetPromptFilterConfig()
	fcc := current.FilterClaudeCode
	fen := current.FilterEnvNoise
	fsb := current.FilterStripBoundaries
	rules := current.Rules
	if req.FilterClaudeCode != nil {
		fcc = *req.FilterClaudeCode
	}
	if req.FilterEnvNoise != nil {
		fen = *req.FilterEnvNoise
	}
	if req.FilterStripBoundaries != nil {
		fsb = *req.FilterStripBoundaries
	}
	if req.Rules != nil {
		rules = *req.Rules
	}
	if err := config.UpdatePromptFilterConfig(fcc, fen, fsb, rules); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ApiKey         *string `json:"apiKey,omitempty"`
		RequireApiKey  *bool   `json:"requireApiKey,omitempty"`
		Password       string  `json:"password,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if err := config.UpdateSettingsPatch(req.ApiKey, req.RequireApiKey, req.Password); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiGetStats(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"totalRequests":     atomic.LoadInt64(&h.totalRequests),
		"successRequests":   atomic.LoadInt64(&h.successRequests),
		"failedRequests":    atomic.LoadInt64(&h.failedRequests),
		"clientDisconnects": atomic.LoadInt64(&h.clientDisconnects),
		"totalTokens":       atomic.LoadInt64(&h.totalTokens),
		"totalCredits":      h.getCredits(),
		"uptime":            time.Now().Unix() - h.startTime,
	})
}

func (h *Handler) apiGetLogs(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"logs": h.getRequestLogs(),
	})
}

func (h *Handler) apiClearLogs(w http.ResponseWriter, r *http.Request) {
	h.requestLogsMu.Lock()
	h.requestLogs = h.requestLogs[:0]
	h.requestLogsMu.Unlock()
	clearRequestLogsDB()
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGenerateMachineId 生成新的机器码
func (h *Handler) apiGenerateMachineId(w http.ResponseWriter, r *http.Request) {
	machineId := config.GenerateMachineId()
	json.NewEncoder(w).Encode(map[string]string{"machineId": machineId})
}

// apiTestAccount tests a specific account by sending a real model request through its proxy.
func (h *Handler) apiTestAccount(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	if err := h.ensureValidToken(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}

	// Parse test model from request body (optional)
	var req struct {
		Model string `json:"model"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Model == "" {
		req.Model = "claude-sonnet-4"
	}

	// Build a minimal chat payload
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)

	openaiReq := &OpenAIRequest{
		Model:     actualModel,
		Messages:  []OpenAIMessage{{Role: "user", Content: "say ok"}},
		MaxTokens: 5,
		Stream:    false,
	}
	kiroPayload := OpenAIToKiro(openaiReq, thinking)

	var content string
	callback := &KiroStreamCallback{
		OnText:         func(text string, isThinking bool) { content += text },
		OnToolUse:      func(tu KiroToolUse) {},
		OnComplete:     func(inTok, outTok int) {},
		OnError:        func(err error) {},
		OnCredits:      func(c float64) {},
		OnContextUsage: func(pct float64) {},
	}

	err := CallKiroAPI(r.Context(), account, kiroPayload, callback)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"reply":   content,
		"model":   req.Model,
	})
}

// hydrateNewAccount 在新账号建好后，服务端同步拉取一次额度/订阅信息并写回，
// 同时对 overage-capable 账号自动打开上游 Overages 开关。best-effort：失败仅记日志，
// 不影响建号结果。取代旧的「前端建完再调 /accounts/{id}/refresh」链路。
func (h *Handler) hydrateNewAccount(account *config.Account) {
	if account == nil {
		return
	}
	if info, err := RefreshAccountInfo(account); err != nil {
		logger.Warnf("[NewAccount] 拉取额度/订阅失败 %s: %v", account.Email, err)
	} else if info != nil {
		if err := config.UpdateAccountInfo(account.ID, *info); err != nil {
			logger.Warnf("[NewAccount] 写回账号信息失败 %s: %v", account.ID, err)
		}
	}
	// 对 capable 且未设置过 Overages 的账号自动开启（内部自带能力/状态判断）。
	h.maybeAutoEnableOverage(account)
}

// apiGetAccountModelsCached 返回账号已缓存的模型列表（不实时拉取上游）。
// pool 里只存 modelId 集合;这里与聚合模型缓存(cachedModels,后台每 30min 刷新)
// join 出名称/倍率/描述等元数据,让详情弹窗无需实时端点也能渲染完整模型卡。
// join 不到的 id(极短暂的缓存不同步窗口)退化为只含 modelId 的条目。
func (h *Handler) apiGetAccountModelsCached(w http.ResponseWriter, r *http.Request, id string) {
	ids := h.pool.GetModelList(id)

	h.modelsCacheMu.RLock()
	metaByID := make(map[string]*ModelInfo, len(h.cachedModels))
	for i := range h.cachedModels {
		metaByID[strings.ToLower(strings.TrimSpace(h.cachedModels[i].ModelId))] = &h.cachedModels[i]
	}
	models := make([]map[string]interface{}, 0, len(ids))
	for _, mid := range ids {
		entry := map[string]interface{}{"modelId": mid}
		if meta := metaByID[strings.ToLower(strings.TrimSpace(mid))]; meta != nil {
			entry["modelId"] = meta.ModelId // 原样大小写
			if meta.ModelName != "" {
				entry["modelName"] = meta.ModelName
			}
			if meta.Description != "" {
				entry["description"] = meta.Description
			}
			if meta.RateMultiplier > 0 {
				entry["rateMultiplier"] = meta.RateMultiplier
			}
		}
		models = append(models, entry)
	}
	h.modelsCacheMu.RUnlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"models":  models,
	})
}

// ==================== 静态文件服务 ====================

func (h *Handler) serveAdminPage(w http.ResponseWriter, r *http.Request) {
	// HTML must always revalidate: it carries the current page structure and the
	// ?v-busted <script>/<link> loader. JS/CSS themselves stay cacheable via ?v.
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	http.ServeFile(w, r, "web/index.html")
}

func (h *Handler) serveStaticFile(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/")
	// Never let the browser serve stale HTML (structure changes must land
	// immediately); versioned JS/CSS keep their heuristic/?v caching.
	if strings.HasSuffix(path, ".html") {
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	}
	http.ServeFile(w, r, "web/"+path)
}

// apiGetThinkingConfig 获取 thinking 配置
func (h *Handler) apiGetThinkingConfig(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetThinkingConfig()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"suffix":       cfg.Suffix,
		"openaiFormat": cfg.OpenAIFormat,
		"claudeFormat": cfg.ClaudeFormat,
	})
}

// apiUpdateThinkingConfig 更新 thinking 配置
func (h *Handler) apiUpdateThinkingConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Suffix       string `json:"suffix"`
		OpenAIFormat string `json:"openaiFormat"`
		ClaudeFormat string `json:"claudeFormat"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证格式
	validFormats := map[string]bool{"reasoning_content": true, "thinking": true, "think": true}
	if req.OpenAIFormat != "" && !validFormats[req.OpenAIFormat] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid openaiFormat, must be: reasoning_content, thinking, or think"})
		return
	}
	if req.ClaudeFormat != "" && !validFormats[req.ClaudeFormat] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid claudeFormat, must be: reasoning_content, thinking, or think"})
		return
	}

	if err := config.UpdateThinkingConfig(req.Suffix, req.OpenAIFormat, req.ClaudeFormat); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetEndpointConfig 获取端点配置
func (h *Handler) apiGetEndpointConfig(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"preferredEndpoint": config.GetPreferredEndpoint(),
		"endpointFallback":  config.GetEndpointFallback(),
	})
}

// apiUpdateEndpointConfig 更新端点配置
func (h *Handler) apiUpdateEndpointConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PreferredEndpoint string `json:"preferredEndpoint"`
		EndpointFallback  *bool  `json:"endpointFallback"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	valid := map[string]bool{"auto": true, "kiro": true, "codewhisperer": true, "amazonq": true}
	if !valid[req.PreferredEndpoint] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid endpoint, must be: auto, kiro, codewhisperer, or amazonq"})
		return
	}

	if err := config.UpdatePreferredEndpoint(req.PreferredEndpoint); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if req.EndpointFallback != nil {
		config.UpdateEndpointFallback(*req.EndpointFallback)
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// applyProxyConfig 将代理配置应用到所有出站 HTTP 客户端（Kiro API + auth 模块）
func applyProxyConfig(proxyURL string) {
	InitKiroHttpClient(proxyURL)
	auth.InitHttpClient(proxyURL)
}

// apiGetProxy 获取当前代理配置
func (h *Handler) apiGetProxy(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"proxyURL": config.GetProxyURL(),
	})
}

// apiUpdateProxy 更新代理配置并立即生效
func (h *Handler) apiUpdateProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProxyURL string `json:"proxyURL"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证代理 URL 格式（非空时）
	if req.ProxyURL != "" {
		if !strings.HasPrefix(req.ProxyURL, "http://") &&
			!strings.HasPrefix(req.ProxyURL, "https://") &&
			!strings.HasPrefix(req.ProxyURL, "socks5://") &&
			!strings.HasPrefix(req.ProxyURL, "socks5h://") {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "proxyURL must start with http://, https://, socks5://, or socks5h://"})
			return
		}
	}

	if err := config.UpdateProxySettings(req.ProxyURL); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 立即应用新的代理配置
	applyProxyConfig(req.ProxyURL)

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetVersion 获取版本信息
func (h *Handler) apiGetVersion(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"version": config.Version,
	})
}

// apiExportAccounts 导出账号凭证
func (h *Handler) apiExportAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"` // 为空则导出全部
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// 如果 body 为空或解析失败，导出全部
		req.IDs = nil
	}

	accounts := config.GetAccounts()

	// 如果指定了 ID，只导出指定的
	if len(req.IDs) > 0 {
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var filtered []config.Account
		for _, a := range accounts {
			if idSet[a.ID] {
				filtered = append(filtered, a)
			}
		}
		accounts = filtered
	}

	// 构建兼容 Kiro Account Manager 的导出格式
	type ExportCredentials struct {
		AccessToken  string `json:"accessToken"`
		CsrfToken    string `json:"csrfToken"`
		RefreshToken string `json:"refreshToken"`
		ClientID     string `json:"clientId,omitempty"`
		ClientSecret string `json:"clientSecret,omitempty"`
		Region       string `json:"region,omitempty"`
		ExpiresAt    int64  `json:"expiresAt"`
		AuthMethod   string `json:"authMethod,omitempty"`
		Provider     string `json:"provider,omitempty"`
		// Auth-method-specific fields so every account type round-trips: idc (AWS
		// IdC / IAM Identity Center) needs StartUrl; external_idp (Entra / Kiro
		// Enterprise) needs TokenEndpoint + IssuerUrl; api_key needs KiroApiKey.
		StartUrl      string `json:"startUrl,omitempty"`
		ProfileArn    string `json:"profileArn,omitempty"`
		TokenEndpoint string `json:"tokenEndpoint,omitempty"`
		IssuerUrl     string `json:"issuerUrl,omitempty"`
		KiroApiKey    string `json:"kiroApiKey,omitempty"`
	}

	type ExportSubscription struct {
		Type  string `json:"type"`
		Title string `json:"title,omitempty"`
	}

	type ExportUsage struct {
		Current     float64 `json:"current"`
		Limit       float64 `json:"limit"`
		PercentUsed float64 `json:"percentUsed"`
		LastUpdated int64   `json:"lastUpdated"`
	}

	type ExportAccount struct {
		ID           string             `json:"id"`
		Email        string             `json:"email"`
		Nickname     string             `json:"nickname,omitempty"`
		Idp          string             `json:"idp"`
		UserId       string             `json:"userId,omitempty"`
		MachineId    string             `json:"machineId,omitempty"`
		Credentials  ExportCredentials  `json:"credentials"`
		Subscription ExportSubscription `json:"subscription"`
		Usage        ExportUsage        `json:"usage"`
		Tags         []string           `json:"tags"`
		Status       string             `json:"status"`
		CreatedAt    int64              `json:"createdAt"`
		LastUsedAt   int64              `json:"lastUsedAt"`
	}

	type ExportData struct {
		Version    string          `json:"version"`
		ExportedAt int64           `json:"exportedAt"`
		Accounts   []ExportAccount `json:"accounts"`
		Groups     []interface{}   `json:"groups"`
		Tags       []interface{}   `json:"tags"`
	}

	exportAccounts := make([]ExportAccount, 0, len(accounts))
	for _, a := range accounts {
		// 映射 provider 到 idp
		idp := a.Provider
		if idp == "" {
			if a.AuthMethod == "social" {
				idp = "Google"
			} else {
				idp = "BuilderId"
			}
		}

		// 映射 authMethod
		authMethod := a.AuthMethod
		if authMethod == "idc" {
			authMethod = "IdC"
		}

		// 映射订阅类型
		subType := "Free"
		rawType := strings.ToUpper(a.SubscriptionType)
		if strings.Contains(rawType, "PRO_PLUS") || strings.Contains(rawType, "PROPLUS") {
			subType = "Pro_Plus"
		} else if strings.Contains(rawType, "PRO") {
			subType = "Pro"
		} else if strings.Contains(rawType, "POWER") {
			subType = "Pro_Plus"
		}

		exportAccounts = append(exportAccounts, ExportAccount{
			ID:        a.ID,
			Email:     a.Email,
			Nickname:  a.Nickname,
			Idp:       idp,
			UserId:    a.UserId,
			MachineId: a.MachineId,
			Credentials: ExportCredentials{
				AccessToken:   a.AccessToken,
				CsrfToken:     "",
				RefreshToken:  a.RefreshToken,
				ClientID:      a.ClientID,
				ClientSecret:  a.ClientSecret,
				Region:        a.Region,
				ExpiresAt:     a.ExpiresAt * 1000, // 转为毫秒时间戳
				AuthMethod:    authMethod,
				Provider:      a.Provider,
				StartUrl:      a.StartUrl,
				ProfileArn:    a.ProfileArn,
				TokenEndpoint: a.TokenEndpoint,
				IssuerUrl:     a.IssuerUrl,
				KiroApiKey:    a.KiroApiKey,
			},
			Subscription: ExportSubscription{
				Type:  subType,
				Title: a.SubscriptionTitle,
			},
			Usage: ExportUsage{
				Current:     a.UsageCurrent,
				Limit:       a.UsageLimit,
				PercentUsed: a.UsagePercent,
				LastUpdated: time.Now().UnixMilli(),
			},
			Tags:       []string{},
			Status:     "active",
			CreatedAt:  time.Now().UnixMilli(),
			LastUsedAt: time.Now().UnixMilli(),
		})
	}

	data := ExportData{
		Version:    config.Version,
		ExportedAt: time.Now().UnixMilli(),
		Accounts:   exportAccounts,
		Groups:     []interface{}{},
		Tags:       []interface{}{},
	}

	json.NewEncoder(w).Encode(data)
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
