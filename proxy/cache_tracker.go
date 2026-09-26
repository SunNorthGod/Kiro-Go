package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"kiro-go/logger"
)

const defaultPromptCacheTTL = 5 * time.Minute

// The local simulation never sends cache_control upstream, so Anthropic's
// "minimum cacheable prefix" API limits (2048/4096) do NOT apply here — those
// gate an API feature; this tracker only records repeat prefixes for the usage
// panels. The threshold therefore lives in the ESTIMATED token space (the same
// estimateApproxTokens space the breakpoints are compared in) and is kept small:
// its only job is to skip bookkeeping for trivially short prompts. The former
// opus-specific 4096 threshold was a category error: the estimate runs ~15%
// below real tokenizer counts, so real ~4.5k-token opus prefixes estimated
// under 4096 and NEVER hit — measured 0/5 on production with a 7.6k-token
// (real) prefix.
const defaultMinCacheableTokens = 1024

type promptCacheUsage struct {
	CacheCreationInputTokens   int
	CacheReadInputTokens       int
	CacheCreation5mInputTokens int
	CacheCreation1hInputTokens int
}

type promptCacheBreakpoint struct {
	Fingerprint      [32]byte
	CumulativeTokens int
	TTL              time.Duration
}

type promptCacheProfile struct {
	Breakpoints      []promptCacheBreakpoint
	TotalInputTokens int
	Model            string
}

func minCacheableTokensForModel(string) int {
	return defaultMinCacheableTokens
}

type promptCacheEntry struct {
	ExpiresAt time.Time
	TTL       time.Duration
}

// promptCachePruneInterval throttles the full-map expiry sweep. Compute/Update
// run on every request and each previously did a full O(accounts×entries) prune;
// under load that repeated scan dominated the lock hold. Expired entries are
// still skipped lazily on read (ExpiresAt check), so bounding the sweep to at
// most once per interval only delays reclaiming memory, never correctness.
const promptCachePruneInterval = 60 * time.Second

type promptCacheTracker struct {
	mu               sync.Mutex
	entriesByAccount map[string]map[[32]byte]promptCacheEntry
	maxSupportedTTL  time.Duration
	lastPrune        time.Time
}

func newPromptCacheTracker(maxTTL time.Duration) *promptCacheTracker {
	if maxTTL <= 0 {
		maxTTL = defaultPromptCacheTTL
	}
	return &promptCacheTracker{
		entriesByAccount: make(map[string]map[[32]byte]promptCacheEntry),
		maxSupportedTTL:  maxTTL,
	}
}

func (t *promptCacheTracker) BuildClaudeProfile(req *ClaudeRequest, totalInputTokens int) *promptCacheProfile {
	return buildPromptCacheProfile(flattenClaudeCacheBlocks(req), totalInputTokens, req.Model)
}

// BuildOpenAIProfile builds the same account-level prefix profile for the
// OpenAI-compatible paths (/v1/chat/completions and /v1/responses). OpenAI
// clients never send cache_control, so all breakpoints come from the automatic
// structural boundaries (end of tools / each message end) with the default TTL,
// mirroring OpenAI's own automatic prompt caching.
func (t *promptCacheTracker) BuildOpenAIProfile(req *OpenAIRequest, totalInputTokens int) *promptCacheProfile {
	if req == nil {
		return nil
	}
	return buildPromptCacheProfile(flattenOpenAICacheBlocks(req), totalInputTokens, req.Model)
}

// buildPromptCacheProfile hashes the flattened prompt blocks into cumulative
// prefix fingerprints and derives the breakpoint chain.
//
// Breakpoint policy (simulating an optimally cache-annotated client, so plain
// clients get the same realistic accounting as e.g. Claude Code):
//   - every structural boundary (end of tools, end of system, end of each
//     message) is an implicit breakpoint with the default 5m TTL;
//   - an explicit cache_control block is a breakpoint too and its TTL (e.g. 1h)
//     carries over to the boundaries that follow it.
func buildPromptCacheProfile(blocks []cacheablePromptBlock, totalInputTokens int, model string) *promptCacheProfile {
	if len(blocks) == 0 {
		return nil
	}

	hasher := sha256.New()
	breakpoints := make([]promptCacheBreakpoint, 0)
	cumulativeTokens := 0
	activeTTL := defaultPromptCacheTTL

	for _, block := range blocks {
		// Reuse the canonical string computed at block construction (no second
		// serialization of the same value).
		writeHashChunk(hasher, block.Canonical)
		cumulativeTokens += block.Tokens

		breakpointTTL := time.Duration(0)
		if block.TTL > 0 {
			breakpointTTL = block.TTL
			activeTTL = block.TTL
		} else if block.IsBoundary {
			breakpointTTL = activeTTL
		}

		if breakpointTTL <= 0 {
			continue
		}

		var fingerprint [32]byte
		copy(fingerprint[:], hasher.Sum(nil))
		breakpoints = append(breakpoints, promptCacheBreakpoint{
			Fingerprint:      fingerprint,
			CumulativeTokens: cumulativeTokens,
			TTL:              breakpointTTL,
		})
	}

	if len(breakpoints) == 0 {
		return nil
	}

	if totalInputTokens < cumulativeTokens {
		totalInputTokens = cumulativeTokens
	}

	return &promptCacheProfile{
		Breakpoints:      breakpoints,
		TotalInputTokens: totalInputTokens,
		Model:            model,
	}
}

// Compute simulates one request against the account's stored prefix cache with
// Anthropic semantics: the longest still-live stored prefix counts as
// cache_read, everything cacheable beyond it counts as cache_creation, and a
// hit refreshes the entry's TTL. Prompts below the model's minimum cacheable
// size are not cached at all (all-zero usage). The output always satisfies
// read+creation <= TotalInputTokens and 5m+1h == creation.
func (t *promptCacheTracker) Compute(accountID string, profile *promptCacheProfile) promptCacheUsage {
	if t == nil || profile == nil || len(profile.Breakpoints) == 0 || accountID == "" {
		bps := 0
		if profile != nil {
			bps = len(profile.Breakpoints)
		}
		logger.Debugf("[CacheProbe] compute skip: nilT=%v nilProfile=%v bps=%d acctEmpty=%v", t == nil, profile == nil, bps, accountID == "")
		return promptCacheUsage{}
	}

	minTokens := minCacheableTokensForModel(profile.Model)
	last := profile.Breakpoints[len(profile.Breakpoints)-1]
	lastTokens := minInt(last.CumulativeTokens, profile.TotalInputTokens)
	if lastTokens < minTokens {
		logger.Debugf("[CacheProbe] compute skip small: lastTokens=%d min=%d", lastTokens, minTokens)
		return promptCacheUsage{}
	}
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneExpiredLocked(now)

	matchedTokens := 0
	entries := t.entriesByAccount[accountID]
	for i := len(profile.Breakpoints) - 1; i >= 0; i-- {
		breakpoint := profile.Breakpoints[i]
		// Breakpoints below the minimum cacheable size were never stored.
		if breakpoint.CumulativeTokens < minTokens {
			continue
		}
		entry, ok := entries[breakpoint.Fingerprint]
		if !ok || entry.ExpiresAt.Before(now) {
			continue
		}
		// A cache hit refreshes the entry's TTL, like the real cache.
		entry.ExpiresAt = now.Add(entry.TTL)
		entries[breakpoint.Fingerprint] = entry
		matchedTokens = minInt(breakpoint.CumulativeTokens, lastTokens)
		break
	}

	creation := maxInt(lastTokens-matchedTokens, 0)
	cache5m, cache1h := computePromptCacheTTLBreakdown(profile, matchedTokens, lastTokens)
	return promptCacheUsage{
		CacheCreationInputTokens:   creation,
		CacheReadInputTokens:       matchedTokens,
		CacheCreation5mInputTokens: cache5m,
		CacheCreation1hInputTokens: cache1h,
	}
}

func (t *promptCacheTracker) Update(accountID string, profile *promptCacheProfile) {
	if t == nil || profile == nil || len(profile.Breakpoints) == 0 || accountID == "" {
		bps := 0
		if profile != nil {
			bps = len(profile.Breakpoints)
		}
		logger.Debugf("[CacheProbe] update skip: nilT=%v nilProfile=%v bps=%d acctEmpty=%v", t == nil, profile == nil, bps, accountID == "")
		return
	}

	minTokens := minCacheableTokensForModel(profile.Model)
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneExpiredLocked(now)

	entries := t.entriesByAccount[accountID]
	if entries == nil {
		entries = make(map[[32]byte]promptCacheEntry)
		t.entriesByAccount[accountID] = entries
	}

	for _, breakpoint := range profile.Breakpoints {
		// Skip breakpoints below the minimum cacheable token threshold.
		if breakpoint.CumulativeTokens < minTokens {
			continue
		}
		entries[breakpoint.Fingerprint] = promptCacheEntry{
			ExpiresAt: now.Add(breakpoint.TTL),
			TTL:       breakpoint.TTL,
		}
	}
}

// pruneExpiredLocked sweeps expired entries, but at most once per
// promptCachePruneInterval (throttled): the full-map scan is expensive and
// running it on every Compute/Update was pure overhead since reads already skip
// expired entries lazily. Caller holds t.mu.
func (t *promptCacheTracker) pruneExpiredLocked(now time.Time) {
	if !t.lastPrune.IsZero() && now.Sub(t.lastPrune) < promptCachePruneInterval {
		return
	}
	t.lastPrune = now
	for accountID, entries := range t.entriesByAccount {
		for fingerprint, entry := range entries {
			if !entry.ExpiresAt.After(now) {
				delete(entries, fingerprint)
			}
		}
		if len(entries) == 0 {
			delete(t.entriesByAccount, accountID)
		}
	}
}

type cacheablePromptBlock struct {
	Value interface{}
	// Canonical is the canonical JSON of Value, computed ONCE at block
	// construction and reused for both the token estimate and the prefix hash
	// (buildPromptCacheProfile). Previously every block was canonicalized twice
	// — once here for tokens, once again in buildPromptCacheProfile for the hash.
	Canonical string
	Tokens    int
	TTL       time.Duration
	// IsBoundary marks a structural prefix boundary (end of the tool
	// definitions, end of the system prompt, end of each message) where the
	// simulation places an automatic cache breakpoint.
	IsBoundary bool
}

// makeCacheBlock canonicalizes value once and derives both the token estimate
// and the stored canonical string from that single serialization.
func makeCacheBlock(value interface{}, ttl time.Duration, isBoundary bool) cacheablePromptBlock {
	canonical := canonicalizeCacheValue(value)
	return cacheablePromptBlock{
		Value:      value,
		Canonical:  canonical,
		Tokens:     estimateApproxTokens(canonical),
		TTL:        ttl,
		IsBoundary: isBoundary,
	}
}

func flattenClaudeCacheBlocks(req *ClaudeRequest) []cacheablePromptBlock {
	blocks := make([]cacheablePromptBlock, 0)
	blocks = append(blocks, buildCachePreludeBlock(req))

	for toolIndex, tool := range req.Tools {
		toolValue := map[string]interface{}{
			"kind":         "tool",
			"tool_index":   toolIndex,
			"name":         tool.Name,
			"description":  tool.Description,
			"input_schema": tool.InputSchema,
		}
		fingerprintValue := stripCachePositionKeys(toolValue)
		blocks = append(blocks, makeCacheBlock(
			fingerprintValue,
			normalizePromptCacheTTL(extractPromptCacheTTL(tool)),
			toolIndex == len(req.Tools)-1,
		))
	}

	appendSystemCacheBlocks(&blocks, req.System)

	for messageIndex, msg := range req.Messages {
		appendMessageCacheBlocks(&blocks, messageIndex, msg)
	}

	return blocks
}

// flattenOpenAICacheBlocks maps an OpenAI-compatible request onto the same
// block/boundary structure: prelude, tool definitions (boundary after the
// last), then one block per message (each a boundary). OpenAI clients have no
// cache_control, so TTLs are always the default.
func flattenOpenAICacheBlocks(req *OpenAIRequest) []cacheablePromptBlock {
	blocks := make([]cacheablePromptBlock, 0, len(req.Messages)+len(req.Tools)+1)

	prelude := map[string]interface{}{
		"kind":  "request_prelude",
		"model": req.Model,
	}
	blocks = append(blocks, makeCacheBlock(prelude, 0, false))

	for toolIndex, tool := range req.Tools {
		toolValue := map[string]interface{}{
			"kind":         "tool",
			"name":         tool.Function.Name,
			"description":  tool.Function.Description,
			"input_schema": tool.Function.Parameters,
		}
		blocks = append(blocks, makeCacheBlock(toolValue, 0, toolIndex == len(req.Tools)-1))
	}

	for _, msg := range req.Messages {
		wrapper := map[string]interface{}{
			"kind":    "message",
			"role":    msg.Role,
			"content": msg.Content,
		}
		if msg.ToolCallID != "" {
			wrapper["tool_call_id"] = msg.ToolCallID
		}
		if len(msg.ToolCalls) > 0 {
			wrapper["tool_calls"] = msg.ToolCalls
		}
		blocks = append(blocks, makeCacheBlock(wrapper, 0, true))
	}

	return blocks
}

func buildCachePreludeBlock(req *ClaudeRequest) cacheablePromptBlock {
	prelude := map[string]interface{}{
		"kind":        "request_prelude",
		"model":       req.Model,
		"tool_choice": req.ToolChoice,
	}
	return makeCacheBlock(prelude, 0, false)
}

func appendSystemCacheBlocks(blocks *[]cacheablePromptBlock, system interface{}) {
	switch v := system.(type) {
	case string:
		appendPromptBlock(blocks, map[string]interface{}{
			"kind":         "system",
			"system_index": 0,
			"block": map[string]interface{}{
				"type": "text",
				"text": v,
			},
		}, true)
	case []interface{}:
		for i, block := range v {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":         "system",
				"system_index": i,
				"block":        block,
			}, i == len(v)-1)
		}
	case []string:
		for i, block := range v {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":         "system",
				"system_index": i,
				"block": map[string]interface{}{
					"type": "text",
					"text": block,
				},
			}, i == len(v)-1)
		}
	}
}

func appendMessageCacheBlocks(blocks *[]cacheablePromptBlock, messageIndex int, msg ClaudeMessage) {
	role := msg.Role
	switch content := msg.Content.(type) {
	case string:
		appendPromptBlock(blocks, map[string]interface{}{
			"kind":          "message",
			"message_index": messageIndex,
			"role":          role,
			"block_index":   0,
			"block": map[string]interface{}{
				"type": "text",
				"text": content,
			},
		}, true)
	case []interface{}:
		lastIdx := len(content) - 1
		for blockIndex, block := range content {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":          "message",
				"message_index": messageIndex,
				"role":          role,
				"block_index":   blockIndex,
				"block":         block,
			}, blockIndex == lastIdx)
		}
	default:
		if content != nil {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":          "message",
				"message_index": messageIndex,
				"role":          role,
				"block_index":   0,
				"block":         content,
			}, true)
		}
	}
}

func appendPromptBlock(blocks *[]cacheablePromptBlock, wrapper map[string]interface{}, isBoundary bool) {
	blockValue := wrapper["block"]
	ttl := normalizePromptCacheTTL(extractPromptCacheTTL(blockValue))

	// Drop volatile billing metadata from the cache fingerprint. Claude Code's
	// x-anthropic-billing-header can drift, appear, or disappear across
	// otherwise identical requests, and it does not change model semantics.
	if isAnthropicBillingHeaderBlock(blockValue) {
		return
	}

	fingerprintValue := stripCachePositionKeys(wrapper)
	*blocks = append(*blocks, makeCacheBlock(fingerprintValue, ttl, isBoundary))
}

func stripCachePositionKeys(value map[string]interface{}) map[string]interface{} {
	cloned := make(map[string]interface{}, len(value))
	for key, item := range value {
		if isCachePositionKey(key) {
			continue
		}
		cloned[key] = item
	}
	return cloned
}

func isAnthropicBillingHeaderBlock(value interface{}) bool {
	blockMap, ok := value.(map[string]interface{})
	if !ok {
		return false
	}

	// Only normalize text blocks (or blocks without an explicit type but containing text).
	if t, ok := blockMap["type"].(string); ok && t != "" && t != "text" {
		return false
	}

	text, ok := blockMap["text"].(string)
	if !ok {
		return false
	}

	trimmed := strings.TrimLeft(text, " \t\r\n")
	return strings.HasPrefix(strings.ToLower(trimmed), "x-anthropic-billing-header:")
}

func extractPromptCacheTTL(value interface{}) time.Duration {
	block, ok := value.(map[string]interface{})
	if !ok {
		if raw, err := json.Marshal(value); err == nil {
			var decoded map[string]interface{}
			if json.Unmarshal(raw, &decoded) == nil {
				block = decoded
				ok = true
			}
		}
	}
	if !ok {
		return 0
	}

	rawCache, ok := block["cache_control"]
	if !ok {
		return 0
	}
	cacheControl, ok := rawCache.(map[string]interface{})
	if !ok {
		return 0
	}
	cacheType, _ := cacheControl["type"].(string)
	if !strings.EqualFold(cacheType, "ephemeral") {
		return 0
	}

	if ttl, ok := parsePromptCacheTTLValue(cacheControl["ttl"]); ok {
		return ttl
	}
	return defaultPromptCacheTTL
}

func parsePromptCacheTTLValue(value interface{}) (time.Duration, bool) {
	switch v := value.(type) {
	case string:
		trimmed := strings.TrimSpace(strings.ToLower(v))
		if trimmed == "" {
			return 0, false
		}
		if d, err := time.ParseDuration(trimmed); err == nil {
			return d, true
		}
		if seconds, err := strconv.Atoi(trimmed); err == nil {
			return time.Duration(seconds) * time.Second, true
		}
	case float64:
		if v > 0 {
			return time.Duration(v) * time.Second, true
		}
	case int:
		if v > 0 {
			return time.Duration(v) * time.Second, true
		}
	case int64:
		if v > 0 {
			return time.Duration(v) * time.Second, true
		}
	}
	return 0, false
}

func normalizePromptCacheTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return 0
	}
	if ttl > time.Hour {
		return time.Hour
	}
	if ttl > defaultPromptCacheTTL {
		return time.Hour
	}
	return defaultPromptCacheTTL
}

// computePromptCacheTTLBreakdown splits the newly-created span
// (matchedTokens, lastTokens] into 5m and 1h tiers by breakpoint TTL. Bounding
// by lastTokens (the effective creation ceiling) keeps the invariant
// cache5m+cache1h == creation.
func computePromptCacheTTLBreakdown(profile *promptCacheProfile, matchedTokens, lastTokens int) (int, int) {
	if profile == nil || len(profile.Breakpoints) == 0 {
		return 0, 0
	}

	cache5m := 0
	cache1h := 0
	previous := matchedTokens
	for _, breakpoint := range profile.Breakpoints {
		current := minInt(breakpoint.CumulativeTokens, lastTokens)
		if current <= previous {
			continue
		}
		delta := current - previous
		if breakpoint.TTL >= time.Hour {
			cache1h += delta
		} else {
			cache5m += delta
		}
		previous = current
	}
	return cache5m, cache1h
}

// resolvePromptCacheUsage picks the request's final prompt-cache accounting:
// upstream metering truth when Kiro transmitted it (layer 1), otherwise the
// local prefix simulation (layer 2). Either way the result is clamped against
// the final input token count, which may come from a different source
// (contextUsage / upstream metadata) than the estimate the profile was built
// with. This single value feeds all three surfaces — client response usage,
// panels, and the usage ledgers — so they can never disagree.
func resolvePromptCacheUsage(simulated promptCacheUsage, hasMetering bool, meteringRead, meteringCreation, finalInputTokens int) promptCacheUsage {
	usage := simulated
	if hasMetering {
		usage = promptCacheUsage{
			CacheCreationInputTokens: meteringCreation,
			CacheReadInputTokens:     meteringRead,
			// Kiro metering has no 5m/1h split; attribute all creation to 5m.
			CacheCreation5mInputTokens: meteringCreation,
		}
	}
	return clampPromptCacheUsage(usage, finalInputTokens)
}

// clampPromptCacheUsage re-normalizes a computed usage against the final input
// token count (which may come from upstream metadata or contextUsage rather
// than the local estimate the profile was built with). Invariants enforced:
// read <= total, read+creation <= total (read wins over creation, matching the
// Rust clamp_to_total), and the 5m/1h split always sums to creation.
func clampPromptCacheUsage(usage promptCacheUsage, totalInputTokens int) promptCacheUsage {
	if totalInputTokens < 0 {
		totalInputTokens = 0
	}
	read := minInt(maxInt(usage.CacheReadInputTokens, 0), totalInputTokens)
	creation := minInt(maxInt(usage.CacheCreationInputTokens, 0), totalInputTokens-read)

	cache5m := maxInt(usage.CacheCreation5mInputTokens, 0)
	cache1h := maxInt(usage.CacheCreation1hInputTokens, 0)
	if cache5m+cache1h != creation {
		// Rescale the tiers preserving their ratio; remainder goes to 5m.
		if sum := cache5m + cache1h; sum > 0 && creation > 0 {
			cache1h = int(float64(cache1h) / float64(sum) * float64(creation))
			cache5m = creation - cache1h
		} else {
			cache5m = creation
			cache1h = 0
		}
	}

	return promptCacheUsage{
		CacheCreationInputTokens:   creation,
		CacheReadInputTokens:       read,
		CacheCreation5mInputTokens: cache5m,
		CacheCreation1hInputTokens: cache1h,
	}
}

func billedClaudeInputTokens(inputTokens int, usage promptCacheUsage) int {
	return maxInt(inputTokens-usage.CacheCreationInputTokens-usage.CacheReadInputTokens, 0)
}

func buildClaudeUsageMap(inputTokens, outputTokens int, usage promptCacheUsage, includeCache bool, credits float64) map[string]interface{} {
	result := map[string]interface{}{
		"input_tokens":  billedClaudeInputTokens(inputTokens, usage),
		"output_tokens": outputTokens,
	}
	// credits(#6): upstream Kiro meteringEvent truth for this turn (JSON number /
	// float64), surfaced at the top level of the Anthropic usage object as
	// "credits". Present (and > 0) only when the upstream reported credit
	// consumption; omitted when 0 (message_start, or a turn the upstream did not
	// meter). Never locally estimated — always passed through from OnCredits so
	// the value is metering truth. The downstream Kiro IDE plugin reads this field
	// to display per-turn credits.
	if credits > 0 {
		result["credits"] = credits
	}
	if !includeCache {
		return result
	}
	result["cache_creation_input_tokens"] = usage.CacheCreationInputTokens
	result["cache_read_input_tokens"] = usage.CacheReadInputTokens
	result["cache_creation"] = map[string]int{
		"ephemeral_5m_input_tokens": usage.CacheCreation5mInputTokens,
		"ephemeral_1h_input_tokens": usage.CacheCreation1hInputTokens,
	}
	return result
}

// buildOpenAIUsageMap renders usage in OpenAI Chat Completions format. Unlike
// Anthropic (whose input_tokens EXCLUDES cached tokens), OpenAI's prompt_tokens
// includes them and the hit is reported via prompt_tokens_details.cached_tokens.
func buildOpenAIUsageMap(inputTokens, outputTokens int, usage promptCacheUsage) map[string]interface{} {
	return map[string]interface{}{
		"prompt_tokens":     inputTokens,
		"completion_tokens": outputTokens,
		"total_tokens":      inputTokens + outputTokens,
		"prompt_tokens_details": map[string]int{
			"cached_tokens": usage.CacheReadInputTokens,
		},
	}
}

func canonicalizeCacheValue(value interface{}) string {
	var buf bytes.Buffer
	writeCanonicalJSON(&buf, value)
	return buf.String()
}

func writeCanonicalJSON(buf *bytes.Buffer, value interface{}) {
	switch v := value.(type) {
	case nil:
		buf.WriteString("null")
	case string:
		encoded, _ := json.Marshal(v)
		buf.Write(encoded)
	case bool:
		if v {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		encoded, _ := json.Marshal(v)
		buf.Write(encoded)
	case []interface{}:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalJSON(buf, item)
		}
		buf.WriteByte(']')
	case map[string]interface{}:
		buf.WriteByte('{')
		keys := make([]string, 0, len(v))
		for key := range v {
			if key == "cache_control" {
				continue
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for i, key := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			encoded, _ := json.Marshal(key)
			buf.Write(encoded)
			buf.WriteByte(':')
			writeCanonicalJSON(buf, v[key])
		}
		buf.WriteByte('}')
	default:
		encoded, _ := json.Marshal(v)
		buf.Write(encoded)
	}
}

func isCachePositionKey(key string) bool {
	switch key {
	case "tool_index", "system_index", "message_index", "block_index":
		return true
	default:
		return false
	}
}

func writeHashChunk(hasher hashWriter, chunk string) {
	length := strconv.Itoa(len(chunk))
	hasher.Write([]byte(length))
	hasher.Write([]byte{0})
	hasher.Write([]byte(chunk))
	hasher.Write([]byte{0})
}

type hashWriter interface {
	Write([]byte) (int, error)
	Sum([]byte) []byte
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
