// Package config provides configuration management for Kiro API Proxy.
//
// This package handles persistent storage and retrieval of:
//   - Account credentials and authentication tokens
//   - Server settings (port, host, API keys)
//   - Usage statistics and metrics
//   - Thinking mode configuration for AI responses
//
// All configuration is stored in a JSON file with thread-safe access
// via read-write mutex protection.
package config

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// GenerateMachineId generates a UUID v4 format machine identifier.
// This ID is used to uniquely identify the proxy instance in Kiro API requests,
// helping with request tracking and rate limiting on the server side.
func GenerateMachineId() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	bytes[6] = (bytes[6] & 0x0f) | 0x40 // 版本 4
	bytes[8] = (bytes[8] & 0x3f) | 0x80 // 变体
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

// Account represents a Kiro API account with authentication credentials and usage statistics.
type Account struct {
	// Basic identification
	ID       string `json:"id"`                 // Unique account identifier (UUID)
	Email    string `json:"email,omitempty"`    // User email address
	UserId   string `json:"userId,omitempty"`   // Kiro user ID
	Nickname string `json:"nickname,omitempty"` // Display name for admin panel

	// Authentication credentials
	AccessToken  string `json:"accessToken"`            // OAuth access token for API calls
	RefreshToken string `json:"refreshToken"`           // OAuth refresh token for token renewal
	ClientID     string `json:"clientId,omitempty"`     // OIDC client ID (for IdC auth)
	ClientSecret string `json:"clientSecret,omitempty"` // OIDC client secret (for IdC auth)
	AuthMethod   string `json:"authMethod"`             // Authentication method: "idc" (AWS IdC), "social" (GitHub/Google), "external_idp" (Microsoft Entra / Kiro Enterprise), or "api_key" (Kiro API Key)
	Provider     string `json:"provider,omitempty"`     // Identity provider name (e.g., "BuilderId", "GitHub")
	Region       string `json:"region"`                 // AWS region for OIDC endpoints
	StartUrl     string `json:"startUrl,omitempty"`     // AWS SSO start URL
	ExpiresAt    int64  `json:"expiresAt,omitempty"`    // Token expiration timestamp (Unix seconds)
	MachineId    string `json:"machineId,omitempty"`    // UUID machine identifier for request tracking
	ProfileArn   string `json:"profileArn,omitempty"`   // CodeWhisperer/Kiro profile ARN for generation requests

	// [login] 新增字段: 为扩展登录方式 (external_idp 企业 SSO / api_key Kiro API Key) 补充的凭证字段。
	// 这些字段为纯追加，向后兼容旧配置文件 (omitempty 保证未使用时不写盘)。
	KiroApiKey    string `json:"kiroApiKey,omitempty"`    // [login] 新增字段: Kiro API Key (ksk_ 前缀); authMethod=api_key 时直接作为 Bearer 使用, 无 refreshToken 也不刷新
	TokenEndpoint string `json:"tokenEndpoint,omitempty"` // [login] 新增字段: external_idp 的 OIDC token endpoint, 用于 refresh_token 刷新
	IssuerUrl     string `json:"issuerUrl,omitempty"`     // [login] 新增字段: external_idp 的 OIDC issuer, 供 discovery 兜底解析 tokenEndpoint
	Scopes        string `json:"scopes,omitempty"`        // [login] 新增字段: external_idp 的 OAuth scopes (空格分隔), 刷新时作为 scope 参数

	// Per-account outbound proxy (falls back to global ProxyURL if empty)
	ProxyURL string `json:"proxyURL,omitempty"`

	// Priority weight for load balancing (higher = more requests)
	Weight int `json:"weight,omitempty"` // 0 or 1 = normal, 2+ = higher priority

	// Upstream Overages state (mirrored from AWS Q `setUserPreference` / `getUsageLimits`).
	// OverageStatus is the only switch that decides whether to keep dispatching once UsageLimit is reached.
	// Allowed values: "ENABLED", "DISABLED", "UNKNOWN" (or empty when not yet fetched).
	OverageStatus     string  `json:"overageStatus,omitempty"`
	OverageCapability string  `json:"overageCapability,omitempty"` // "OVERAGE_CAPABLE" / "NOT_OVERAGE_CAPABLE"
	OverageCap        float64 `json:"overageCap,omitempty"`        // Hard upper bound (USD)
	OverageRate       float64 `json:"overageRate,omitempty"`       // Per-invocation rate (USD)
	CurrentOverages   float64 `json:"currentOverages,omitempty"`   // Cumulative overage charges (USD)
	OverageCheckedAt  int64   `json:"overageCheckedAt,omitempty"`  // Last successful upstream sync (Unix seconds)

	// LegacyAllowOverage is kept for backward-compatible JSON loading only.
	// Pre-Overages-switch deployments persisted `allowOverage: true` to mean
	// "keep dispatching when quota is exhausted". On first load we migrate it
	// into OverageStatus="ENABLED" and zero this field so it does not get
	// re-emitted on future saves. Do not read this field elsewhere.
	LegacyAllowOverage bool `json:"allowOverage,omitempty"`

	// Account status
	Enabled   bool   `json:"enabled"`             // Whether account is active in the pool
	BanStatus string `json:"banStatus,omitempty"` // Ban status: "ACTIVE", "BANNED", "SUSPENDED"
	BanReason string `json:"banReason,omitempty"` // Reason for ban/suspension
	BanTime   int64  `json:"banTime,omitempty"`   // Timestamp when ban was detected

	// Subscription information
	SubscriptionType  string `json:"subscriptionType,omitempty"`  // Tier: FREE, PRO, PRO_PLUS, or POWER
	SubscriptionTitle string `json:"subscriptionTitle,omitempty"` // Human-readable subscription name
	DaysRemaining     int    `json:"daysRemaining,omitempty"`     // Days until subscription expires

	// Usage tracking
	UsageCurrent  float64 `json:"usageCurrent,omitempty"`  // Current period usage (credits)
	UsageLimit    float64 `json:"usageLimit,omitempty"`    // Maximum allowed usage per period
	UsagePercent  float64 `json:"usagePercent,omitempty"`  // Usage percentage (0.0-1.0)
	NextResetDate string  `json:"nextResetDate,omitempty"` // Date when usage resets (YYYY-MM-DD)
	LastRefresh   int64   `json:"lastRefresh,omitempty"`   // Last info refresh timestamp

	// Trial usage tracking
	TrialUsageCurrent float64 `json:"trialUsageCurrent,omitempty"` // Trial quota current usage
	TrialUsageLimit   float64 `json:"trialUsageLimit,omitempty"`   // Trial quota total limit
	TrialUsagePercent float64 `json:"trialUsagePercent,omitempty"` // Trial quota usage percentage (0.0-1.0)
	TrialStatus       string  `json:"trialStatus,omitempty"`       // Trial status: ACTIVE, EXPIRED, NONE
	TrialExpiresAt    int64   `json:"trialExpiresAt,omitempty"`    // Trial expiration timestamp (Unix seconds)

	// Runtime statistics (updated during operation)
	RequestCount int     `json:"requestCount,omitempty"` // Total requests processed
	ErrorCount   int     `json:"errorCount,omitempty"`   // Total errors encountered
	LastUsed     int64   `json:"lastUsed,omitempty"`     // Last request timestamp
	TotalTokens  int     `json:"totalTokens,omitempty"`  // Cumulative tokens processed
	TotalCredits float64 `json:"totalCredits,omitempty"` // Cumulative credits consumed

	// CreatedAt is the account creation time (Unix seconds). Primarily used to
	// give the PostgreSQL backend a stable ordering key so the account pool order
	// is deterministic across restarts. Set on AddAccount when unset.
	CreatedAt int64 `json:"createdAt,omitempty"`
}

// PromptFilterRule defines a single custom prompt sanitization rule.
// Type can be: "regex" (regexp find/replace within prompt) or
// "lines-containing" (remove lines containing the match substring).
type PromptFilterRule struct {
	ID      string `json:"id"`                // Unique rule identifier
	Name    string `json:"name"`              // Human-readable rule name
	Type    string `json:"type"`              // "regex" or "lines-containing"
	Match   string `json:"match"`             // Pattern to match (regex pattern or substring)
	Replace string `json:"replace,omitempty"` // Replacement string (only for regex; empty = delete match)
	Enabled bool   `json:"enabled"`           // Whether this rule is active
}

// ApiKeyEntry represents a single API key with optional usage limits and counters.
// Limits with value 0 are treated as "no limit". Counters are cumulative and never reset
// automatically; operators can use the admin endpoint to manually reset them.
type ApiKeyEntry struct {
	ID         string `json:"id"`                 // Unique identifier (UUID)
	Name       string `json:"name,omitempty"`     // Human-readable label
	Key        string `json:"key"`                // The actual key value clients send
	Enabled    bool   `json:"enabled"`            // Whether this key may authenticate
	Migrated   bool   `json:"migrated,omitempty"` // True if migrated from legacy single ApiKey field
	CreatedAt  int64  `json:"createdAt"`          // Creation timestamp (Unix seconds)
	LastUsedAt int64  `json:"lastUsedAt,omitempty"`

	// Limits (0 = unlimited)
	TokenLimit  int64   `json:"tokenLimit,omitempty"`
	CreditLimit float64 `json:"creditLimit,omitempty"`

	// Cumulative usage (never auto-reset)
	TokensUsed    int64   `json:"tokensUsed,omitempty"`
	CreditsUsed   float64 `json:"creditsUsed,omitempty"`
	RequestsCount int64   `json:"requestsCount,omitempty"`

	// Unified ledger + card-key ("卡密") fields. All are pure additions with
	// omitempty, so older config files that lack them load unchanged.
	//
	// CreditsGranted is the "进账" side of the unified ledger: the running total of
	// credits ever recharged onto this key. The spendable balance is
	// CreditsGranted - CreditsUsed. Like CreditsUsed it is monotonic; when >0 it
	// takes precedence over the legacy CreditLimit in ApiKeyOverLimit.
	CreditsGranted float64 `json:"creditsGranted,omitempty"`
	// ExpiresAt is the key's hard expiry (Unix seconds); 0 == never expires.
	ExpiresAt int64 `json:"expiresAt,omitempty"`
	// BoundAccountIDs restricts which accounts this key may use; empty == any.
	BoundAccountIDs []string `json:"boundAccountIds,omitempty"`
	// ParentKeyID is the id of the key that minted this one; "" == root.
	ParentKeyID string `json:"parentKeyId,omitempty"`
	// MaxConcurrency is this key's per-key fairness baseline under contention
	// (a soft floor, not a hard ceiling — idle pool capacity is always lent out).
	// Unified tri-state semantics (nullable pointer):
	//   nil → inherit the system default (config.GetDefaultMaxConcurrency)
	//   0   → unlimited (bypass the fairness gate, like an admin/master key)
	//   N   → guarantee at least N concurrent slots for this key under contention
	// Persisted via the api_keys.max_concurrency column (NULL == inherit).
	MaxConcurrency *int `json:"maxConcurrency,omitempty"`
	// MaxRPM is this key's per-key requests-per-60s ceiling (a HARD cap, unlike
	// MaxConcurrency). Unified tri-state semantics (nullable pointer):
	//   nil → inherit the system default (config.GetDefaultMaxRPM)
	//   0   → unlimited (never RPM-gated)
	//   N   → reject with 429 once this key exceeds N requests in the trailing 60s
	// Persisted via the api_keys.max_rpm column (NULL == inherit).
	MaxRPM *int `json:"maxRPM,omitempty"`
}

// Config represents the global application configuration.
type Config struct {
	// Server settings
	Password      string        `json:"password"`          // Admin panel password
	Port          int           `json:"port"`              // HTTP server port (default: 8080)
	Host          string        `json:"host"`              // HTTP server bind address (default: 0.0.0.0)
	ApiKey        string        `json:"apiKey,omitempty"`  // [Deprecated] Legacy single API key, migrated into ApiKeys on first load
	RequireApiKey bool          `json:"requireApiKey"`     // [Deprecated] Whether to enforce API key validation; with multi-key support, len(ApiKeys)>0 implicitly enforces auth
	ApiKeys       []ApiKeyEntry `json:"apiKeys,omitempty"` // Multiple API keys, each with independent quota
	KiroVersion   string        `json:"kiroVersion,omitempty"`
	SystemVersion string        `json:"systemVersion,omitempty"`
	NodeVersion   string        `json:"nodeVersion,omitempty"`
	Accounts      []Account     `json:"accounts"` // Registered Kiro accounts

	// Per-key default limits (system-wide fairness baselines), nullable so the
	// "never configured" state is distinct from an explicit 0 (= unlimited):
	//   nil → factory default (concurrency 5, RPM 20), pre-filled in the settings UI
	//   0   → unlimited
	//   N   → that value
	// DefaultMaxConcurrency is a key's fair-share concurrency baseline under
	// contention; DefaultMaxRPM is a hard per-key requests-per-60s ceiling. Both
	// can be overridden per key via ApiKeyEntry.
	DefaultMaxConcurrency *int `json:"defaultMaxConcurrency,omitempty"`
	DefaultMaxRPM         *int `json:"defaultMaxRPM,omitempty"`

	// Thinking mode configuration for extended reasoning output
	ThinkingSuffix       string `json:"thinkingSuffix,omitempty"`       // Model suffix to trigger thinking mode (default: "-thinking")
	OpenAIThinkingFormat string `json:"openaiThinkingFormat,omitempty"` // OpenAI output format: "reasoning_content", "thinking", or "think"
	ClaudeThinkingFormat string `json:"claudeThinkingFormat,omitempty"` // Claude output format: "reasoning_content", "thinking", or "think"

	// Endpoint configuration: "auto", "kiro", "codewhisperer", or "amazonq"
	PreferredEndpoint string `json:"preferredEndpoint,omitempty"`

	// EndpointFallback controls whether to try other endpoints when the preferred one fails.
	// Defaults to true. Set to false to only use the preferred endpoint.
	EndpointFallback *bool `json:"endpointFallback,omitempty"`

	// AllowOverUsage allows accounts to continue serving requests even when their
	// usage quota has been exhausted. When enabled, the pool will not skip accounts
	// solely because usageCurrent >= usageLimit.
	AllowOverUsage bool `json:"allowOverUsage,omitempty"`

	// Proxy configuration: optional outbound proxy for Kiro API requests
	// Format: "socks5://host:port", "socks5://user:pass@host:port",
	//         "http://host:port",  "http://user:pass@host:port"
	// Leave empty to connect directly.
	ProxyURL string `json:"proxyURL,omitempty"`

	// SanitizeClaudeCodePrompt is kept for backward-compatible JSON loading only.
	// Migrated to FilterClaudeCode on first load. Do not use directly.
	SanitizeClaudeCodePrompt bool `json:"sanitizeClaudeCodePrompt,omitempty"`

	// FilterClaudeCode detects the Claude Code CLI built-in system prompt and replaces it
	// with a compact backend-only prompt, reducing token usage significantly.
	FilterClaudeCode bool `json:"filterClaudeCode,omitempty"`

	// FilterEnvNoise strips environment metadata lines from system prompts:
	// git status, recent commits, environment sections, fast_mode_info tags, etc.
	FilterEnvNoise bool `json:"filterEnvNoise,omitempty"`

	// FilterStripBoundaries removes --- SYSTEM PROMPT --- / --- END SYSTEM PROMPT --- markers.
	FilterStripBoundaries bool `json:"filterStripBoundaries,omitempty"`

	// PromptFilterRules is a list of user-defined prompt sanitization rules (regex or line-filter).
	PromptFilterRules []PromptFilterRule `json:"promptFilterRules,omitempty"`

	// LogLevel controls verbosity of application logs.
	// Accepted values: "debug", "info", "warn", "error". Defaults to "info".
	// Can be overridden by the LOG_LEVEL environment variable.
	LogLevel string `json:"logLevel,omitempty"`

	// Global statistics (persisted across restarts)
	TotalRequests   int     `json:"totalRequests,omitempty"`   // Total API requests received
	SuccessRequests int     `json:"successRequests,omitempty"` // Successful requests count
	FailedRequests  int     `json:"failedRequests,omitempty"`  // Failed requests count
	TotalTokens     int     `json:"totalTokens,omitempty"`     // Total tokens processed
	TotalCredits    float64 `json:"totalCredits,omitempty"`    // Total credits consumed
}

// AccountInfo contains account metadata retrieved from Kiro API.
// Used for updating subscription and usage information.
type AccountInfo struct {
	Email             string
	UserId            string
	SubscriptionType  string
	SubscriptionTitle string
	DaysRemaining     int
	UsageCurrent      float64
	UsageLimit        float64
	UsagePercent      float64
	NextResetDate     string
	LastRefresh       int64
	TrialUsageCurrent float64
	TrialUsageLimit   float64
	TrialUsagePercent float64
	TrialStatus       string
	TrialExpiresAt    int64
}

// Version current version. MUST stay in sync with version.json at the repo root:
// this constant is what /admin/api/version and /health report, while version.json
// is what the admin panel compares against for the update banner. A mismatch made
// the panel report an update that was already installed.
const Version = "1.1.19"

var (
	cfg     *Config
	cfgLock sync.RWMutex
	// cfgPath is written once by Init and read from Save/ConfigDir/Load, which
	// run on request/background goroutines. A plain string field raced (the
	// -race detector flagged the Init write vs. those unsynchronized reads, even
	// though they are temporally separated). Storing it in an atomic.Pointer
	// gives every read a proper happens-before edge to the single startup write.
	cfgPathAtomic atomic.Pointer[string]
)

// setCfgPath records the config file path (called once from Init).
func setCfgPath(p string) { cfgPathAtomic.Store(&p) }

// getCfgPath returns the config file path, or "" before Init has run.
func getCfgPath() string {
	if p := cfgPathAtomic.Load(); p != nil {
		return *p
	}
	return ""
}

// Init initializes the configuration system with the specified file path.
// If the file doesn't exist, a default configuration is created.
func Init(path string) error {
	setCfgPath(path)
	return Load()
}

func Load() error {
	cfgLock.Lock()
	defer cfgLock.Unlock()

	data, err := os.ReadFile(getCfgPath())
	if err != nil {
		if os.IsNotExist(err) {
			// Create default configuration.
			// Binds to 0.0.0.0 by default for Docker/container compatibility.
			cfg = &Config{
				Password:      "changeme",
				Port:          8080,
				Host:          "0.0.0.0",
				RequireApiKey: false,
				Accounts:      []Account{},
			}
			return saveLocked()
		}
		return err
	}

	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	cfg = &c

	// Migration: if a legacy single ApiKey is present and the new ApiKeys list is empty,
	// promote it into the new structure. The migrated entry inherits the legacy
	// RequireApiKey state — if the legacy deployment was public (RequireApiKey=false),
	// we mark the entry disabled so it doesn't accidentally start enforcing auth.
	// Operators can flip it on later from the admin UI. The legacy field is kept
	// for backward compatibility when reading older config files.
	if cfg.ApiKey != "" && len(cfg.ApiKeys) == 0 {
		cfg.ApiKeys = append(cfg.ApiKeys, ApiKeyEntry{
			ID:        newUUID(),
			Name:      "legacy",
			Key:       cfg.ApiKey,
			Enabled:   cfg.RequireApiKey,
			Migrated:  true,
			CreatedAt: time.Now().Unix(),
		})
		if err := saveLocked(); err != nil {
			return err
		}
	}

	// Migration: per-account AllowOverage → OverageStatus.
	// Pre-Overages-switch deployments stored `allowOverage: true` to mean "keep
	// dispatching when quota is exhausted". The new model reads OverageStatus
	// from the upstream AWS Q switch instead. To avoid silently disabling
	// previously-allowed accounts on first launch, treat allowOverage=true as
	// OverageStatus="ENABLED" (operators can refresh from AWS later). The
	// legacy field is then cleared so future saves don't re-emit it.
	overageMigrated := false
	for i := range cfg.Accounts {
		if cfg.Accounts[i].LegacyAllowOverage {
			if cfg.Accounts[i].OverageStatus == "" {
				cfg.Accounts[i].OverageStatus = "ENABLED"
			}
			cfg.Accounts[i].LegacyAllowOverage = false
			overageMigrated = true
		}
	}
	if overageMigrated {
		if err := saveLocked(); err != nil {
			return err
		}
	}

	// Migration: per-key limit semantics were unified so that 0 == unlimited and
	// the "-1" sentinel no longer exists. Older JSON configs may still carry
	// MaxConcurrency/MaxRPM == -1 (the previous "unlimited" marker); remap those
	// to 0 so they keep meaning unlimited under the new rule. Values of 0 in an
	// old JSON meant "inherit default" — but a JSON key that was never set is
	// absent (nil pointer) rather than 0, and the admin UI only ever emitted 0
	// for the "default" mode as an explicit field, so a literal 0 here is rare;
	// we leave any explicit 0 as unlimited per the new unified rule.
	limitsMigrated := false
	for i := range cfg.ApiKeys {
		if v := cfg.ApiKeys[i].MaxConcurrency; v != nil && *v < 0 {
			zero := 0
			cfg.ApiKeys[i].MaxConcurrency = &zero
			limitsMigrated = true
		}
		if v := cfg.ApiKeys[i].MaxRPM; v != nil && *v < 0 {
			zero := 0
			cfg.ApiKeys[i].MaxRPM = &zero
			limitsMigrated = true
		}
	}
	if v := cfg.DefaultMaxConcurrency; v != nil && *v < 0 {
		zero := 0
		cfg.DefaultMaxConcurrency = &zero
		limitsMigrated = true
	}
	if v := cfg.DefaultMaxRPM; v != nil && *v < 0 {
		zero := 0
		cfg.DefaultMaxRPM = &zero
		limitsMigrated = true
	}
	if limitsMigrated {
		if err := saveLocked(); err != nil {
			return err
		}
	}
	return nil
}

// saveLocked persists cfg to disk. Caller MUST already hold cfgLock.
// This is identical to Save() (which does not take the lock either) but is named
// distinctly so call sites that already hold cfgLock are explicit about it.
func saveLocked() error {
	return Save()
}

// newUUID returns a UUID v4 string. Defined here to avoid pulling extra deps in this file.
func newUUID() string {
	return GenerateMachineId()
}

// Save persists the current configuration to the JSON file.
// Uses indented formatting for human readability.
func Save() error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(getCfgPath(), data, 0600)
}

// ConfigDir returns the directory holding the config file, so sibling runtime
// data files (e.g. daily_stats.json) can live alongside it. Returns "." until
// Init has run.
func ConfigDir() string {
	p := getCfgPath()
	if p == "" {
		return "."
	}
	return filepath.Dir(p)
}

// SetPassword updates the admin password.
// Primarily used for environment variable override in containerized deployments.
func SetPassword(password string) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.Password = password
}

// GetConfigDir returns the directory containing the config JSON file.
// Useful for sibling state (e.g. stored Responses, caches) that should live
// alongside the configuration file.
func GetConfigDir() string {
	dir := getCfgPath()
	if dir == "" {
		return "."
	}
	for i := len(dir) - 1; i >= 0; i-- {
		if dir[i] == '/' || dir[i] == '\\' {
			return dir[:i]
		}
	}
	return "."
}

func Get() *Config {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg
}

func GetPassword() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.Password
}

func GetPort() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Port == 0 {
		return 8080
	}
	return cfg.Port
}

func GetHost() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Host == "" {
		return "127.0.0.1"
	}
	return cfg.Host
}

// Factory defaults for the per-key limits, shown pre-filled in the settings UI
// and used whenever the admin has never configured a value (DefaultMax* == nil).
// These are real seeded values, not hidden magic fallbacks: concurrency 5, RPM 20.
const (
	FactoryDefaultConcurrency = 5
	FactoryDefaultRPM         = 20
)

// GetDefaultMaxConcurrency returns the system-wide per-key concurrency baseline
// (fair share under contention). Unified semantics:
//
//	nil (never set) → factory default (FactoryDefaultConcurrency)
//	0               → unlimited
//	N               → that value
func GetDefaultMaxConcurrency() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.DefaultMaxConcurrency == nil {
		return FactoryDefaultConcurrency
	}
	return *cfg.DefaultMaxConcurrency
}

// GetDefaultMaxRPM returns the system-wide per-key requests-per-60s ceiling.
// Unified semantics:
//
//	nil (never set) → factory default (FactoryDefaultRPM)
//	0               → unlimited (no RPM gating)
//	N               → that value
func GetDefaultMaxRPM() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.DefaultMaxRPM == nil {
		return FactoryDefaultRPM
	}
	return *cfg.DefaultMaxRPM
}

// UpdateDefaultLimits sets the system-wide per-key concurrency baseline and RPM
// ceiling. nil args are left unchanged. A stored value of 0 means unlimited; any
// negative value is clamped to 0 (there is no "-1" sentinel any more).
func UpdateDefaultLimits(maxConcurrency, maxRPM *int) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	if maxConcurrency != nil {
		v := *maxConcurrency
		if v < 0 {
			v = 0
		}
		cfg.DefaultMaxConcurrency = &v
	}
	if maxRPM != nil {
		v := *maxRPM
		if v < 0 {
			v = 0
		}
		cfg.DefaultMaxRPM = &v
	}
	return saveLocked()
}

func GetAccounts() []Account {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	accounts := make([]Account, len(cfg.Accounts))
	copy(accounts, cfg.Accounts)
	return accounts
}

func GetEnabledAccounts() []Account {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	var accounts []Account
	for _, a := range cfg.Accounts {
		if a.Enabled {
			accounts = append(accounts, a)
		}
	}
	return accounts
}

func AddAccount(account Account) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if account.CreatedAt == 0 {
		account.CreatedAt = time.Now().Unix()
	}
	cfg.Accounts = append(cfg.Accounts, account)
	if err := persistAccountLocked(account); err != nil {
		// Roll back the in-memory append so we don't drift from the store.
		cfg.Accounts = cfg.Accounts[:len(cfg.Accounts)-1]
		return err
	}
	return nil
}

// accountIdentityMatch reports whether two accounts refer to the same underlying
// Kiro identity: same non-empty UserId, or (when UserId is unknown on either
// side) same non-empty Email. This is the dedupe key used on (re-)import so the
// same account refreshed with new tokens updates in place instead of piling up
// duplicate rows.
func accountIdentityMatch(a, b *Account) bool {
	// Kiro API-key (ksk_) accounts carry no email/userId; the key itself is the
	// stable identity, so dedupe on it first.
	if strings.TrimSpace(a.KiroApiKey) != "" && strings.TrimSpace(b.KiroApiKey) != "" {
		return strings.TrimSpace(a.KiroApiKey) == strings.TrimSpace(b.KiroApiKey)
	}
	if a.UserId != "" && b.UserId != "" {
		return strings.EqualFold(strings.TrimSpace(a.UserId), strings.TrimSpace(b.UserId))
	}
	if a.Email != "" && b.Email != "" {
		return strings.EqualFold(strings.TrimSpace(a.Email), strings.TrimSpace(b.Email))
	}
	return false
}

// AddOrReplaceAccount adds a new account, or — when one with the same identity
// (UserId, else Email) already exists — refreshes that account's credentials in
// place instead of creating a duplicate. On a match the existing ID, CreatedAt,
// accumulated runtime stats and operator preferences (weight / proxy / nickname /
// subscription cache) are preserved, while the auth credentials, region and
// enabled state are updated from `account` (and any prior ban is cleared, since
// the fresh credentials were just validated). The effective stored account —
// including its stable ID — is written back into *account so callers report the
// ID actually used. When `account` carries no identity (empty UserId AND Email)
// it cannot be deduped and is appended like AddAccount.
func AddOrReplaceAccount(account *Account) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	pick := func(next, cur string) string {
		if strings.TrimSpace(next) != "" {
			return next
		}
		return cur
	}
	for i := range cfg.Accounts {
		existing := &cfg.Accounts[i]
		if !accountIdentityMatch(account, existing) {
			continue
		}
		existing.Email = pick(account.Email, existing.Email)
		if account.Nickname != "" {
			existing.Nickname = account.Nickname
		}
		if account.UserId != "" {
			existing.UserId = account.UserId
		}
		existing.AccessToken = account.AccessToken
		existing.RefreshToken = account.RefreshToken
		existing.ClientID = account.ClientID
		existing.ClientSecret = account.ClientSecret
		existing.AuthMethod = pick(account.AuthMethod, existing.AuthMethod)
		existing.Provider = pick(account.Provider, existing.Provider)
		existing.Region = pick(account.Region, existing.Region)
		existing.StartUrl = pick(account.StartUrl, existing.StartUrl)
		existing.ExpiresAt = account.ExpiresAt
		if account.ProfileArn != "" {
			existing.ProfileArn = account.ProfileArn
		}
		existing.KiroApiKey = pick(account.KiroApiKey, existing.KiroApiKey)
		existing.TokenEndpoint = pick(account.TokenEndpoint, existing.TokenEndpoint)
		existing.IssuerUrl = pick(account.IssuerUrl, existing.IssuerUrl)
		existing.Scopes = pick(account.Scopes, existing.Scopes)
		if existing.MachineId == "" {
			existing.MachineId = account.MachineId
		}
		// Fresh, just-validated credentials: re-enable and clear any prior ban.
		existing.Enabled = true
		existing.BanStatus = ""
		existing.BanReason = ""
		existing.BanTime = 0
		snapshot := *existing
		*account = snapshot
		return persistAccountLocked(snapshot)
	}
	if account.CreatedAt == 0 {
		account.CreatedAt = time.Now().Unix()
	}
	cfg.Accounts = append(cfg.Accounts, *account)
	if err := persistAccountLocked(*account); err != nil {
		cfg.Accounts = cfg.Accounts[:len(cfg.Accounts)-1]
		return err
	}
	return nil
}

func UpdateAccount(id string, account Account) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			// Preserve the original CreatedAt when the incoming copy lacks one,
			// keeping the DB ordering key stable across updates.
			if account.CreatedAt == 0 {
				account.CreatedAt = cfg.Accounts[i].CreatedAt
			}
			cfg.Accounts[i] = account
			return persistAccountLocked(cfg.Accounts[i])
		}
	}
	return nil
}

// UpdateAccountOverageStatus persists the cached upstream overage status fields.
// Called after a successful setUserPreference or getUsageLimits round-trip.
func UpdateAccountOverageStatus(id, status, capability string, cap, rate, current float64, checkedAt int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			if status != "" {
				cfg.Accounts[i].OverageStatus = status
			}
			if capability != "" {
				cfg.Accounts[i].OverageCapability = capability
			}
			cfg.Accounts[i].OverageCap = cap
			cfg.Accounts[i].OverageRate = rate
			cfg.Accounts[i].CurrentOverages = current
			if checkedAt > 0 {
				cfg.Accounts[i].OverageCheckedAt = checkedAt
			}
			return persistAccountLocked(cfg.Accounts[i])
		}
	}
	return nil
}

// SetAccountEnabled toggles the enabled state of an account and persists the change.
// Used to disable accounts whose refresh token has been revoked (401 Bad credentials)
// so subsequent requests skip them automatically.
func SetAccountEnabled(id string, enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].Enabled = enabled
			if !enabled {
				cfg.Accounts[i].BanStatus = "DISABLED"
				cfg.Accounts[i].BanTime = time.Now().Unix()
			}
			return persistAccountLocked(cfg.Accounts[i])
		}
	}
	return nil
}

// SetAccountBanStatus marks an account as banned/disabled with a reason.
// Reason is recorded so operators can see why the account was auto-disabled.
func SetAccountBanStatus(id, status, reason string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].BanStatus = status
			cfg.Accounts[i].BanReason = reason
			cfg.Accounts[i].BanTime = time.Now().Unix()
			if status == "BANNED" || status == "DISABLED" {
				cfg.Accounts[i].Enabled = false
			}
			return persistAccountLocked(cfg.Accounts[i])
		}
	}
	return nil
}

func UpdateAccountProfileArn(id, profileArn string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].ProfileArn = profileArn
			return persistAccountLocked(cfg.Accounts[i])
		}
	}
	return nil
}

func DeleteAccount(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts = append(cfg.Accounts[:i], cfg.Accounts[i+1:]...)
			return persistAccountDeleteLocked(id)
		}
	}
	return nil
}

func UpdateAccountToken(id, accessToken, refreshToken string, expiresAt int64) error {
	cfgLock.Lock()
	if cfg == nil {
		cfgLock.Unlock()
		return nil
	}
	idx := -1
	for i := range cfg.Accounts {
		if cfg.Accounts[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		cfgLock.Unlock()
		return nil
	}
	cfg.Accounts[idx].AccessToken = accessToken
	if refreshToken != "" {
		cfg.Accounts[idx].RefreshToken = refreshToken
	}
	cfg.Accounts[idx].ExpiresAt = expiresAt
	dbOn := dbEnabled
	if !dbOn {
		err := saveLocked()
		cfgLock.Unlock()
		return err
	}
	cfgLock.Unlock()
	// DB mode: targeted token UPDATE OUTSIDE cfgLock. Disjoint from the stats
	// columns, so it can't revert a concurrent stats write and vice versa. The
	// blank-refreshToken "keep existing" semantics are handled in db.UpdateAccountToken.
	return dbUpdateAccountToken(id, accessToken, refreshToken, expiresAt)
}

func GetApiKey() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.ApiKey
}

// IsApiKeyRequired always returns true: API-key authentication is mandatory and
// cannot be turned off. NorthGod Kiro-Go never runs open — when no keys are
// configured, authenticate() fails closed. The legacy cfg.RequireApiKey flag is
// retained only for backward-compatible JSON loading and is no longer consulted
// for enforcement.
func IsApiKeyRequired() bool {
	return true
}

func UpdateSettings(apiKey string, requireApiKey bool, password string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ApiKey = apiKey
	cfg.RequireApiKey = requireApiKey
	if password != "" {
		cfg.Password = password
	}
	return Save()
}

func UpdateSettingsPatch(apiKey *string, requireApiKey *bool, password string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if apiKey != nil {
		cfg.ApiKey = *apiKey
	}
	if requireApiKey != nil {
		cfg.RequireApiKey = *requireApiKey
	}
	if password != "" {
		cfg.Password = password
	}
	return Save()
}

func UpdateStats(totalReq, successReq, failedReq, totalTokens int, totalCredits float64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.TotalRequests = totalReq
	cfg.SuccessRequests = successReq
	cfg.FailedRequests = failedReq
	cfg.TotalTokens = totalTokens
	cfg.TotalCredits = totalCredits
	return Save()
}

func GetStats() (int, int, int, int, float64) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.TotalRequests, cfg.SuccessRequests, cfg.FailedRequests, cfg.TotalTokens, cfg.TotalCredits
}

func UpdateAccountStats(id string, requestCount, errorCount, totalTokens int, totalCredits float64, lastUsed int64) error {
	cfgLock.Lock()
	if cfg == nil {
		cfgLock.Unlock()
		return nil
	}
	idx := -1
	for i := range cfg.Accounts {
		if cfg.Accounts[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		cfgLock.Unlock()
		return nil
	}
	cfg.Accounts[idx].RequestCount = requestCount
	cfg.Accounts[idx].ErrorCount = errorCount
	cfg.Accounts[idx].TotalTokens = totalTokens
	cfg.Accounts[idx].TotalCredits = totalCredits
	cfg.Accounts[idx].LastUsed = lastUsed
	dbOn := dbEnabled
	if !dbOn {
		err := saveLocked()
		cfgLock.Unlock()
		return err
	}
	cfgLock.Unlock()
	// DB mode: targeted stats UPDATE OUTSIDE cfgLock (disjoint from token columns).
	return dbUpdateAccountStats(id, requestCount, errorCount, totalTokens, totalCredits, lastUsed)
}

// UpdateAccountInfo updates an account's subscription and usage information.
// Called after refreshing account data from Kiro API.
func UpdateAccountInfo(id string, info AccountInfo) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			if info.Email != "" {
				cfg.Accounts[i].Email = info.Email
			}
			if info.UserId != "" {
				cfg.Accounts[i].UserId = info.UserId
			}
			cfg.Accounts[i].SubscriptionType = info.SubscriptionType
			cfg.Accounts[i].SubscriptionTitle = info.SubscriptionTitle
			cfg.Accounts[i].DaysRemaining = info.DaysRemaining
			cfg.Accounts[i].UsageCurrent = info.UsageCurrent
			cfg.Accounts[i].UsageLimit = info.UsageLimit
			cfg.Accounts[i].UsagePercent = info.UsagePercent
			cfg.Accounts[i].NextResetDate = info.NextResetDate
			cfg.Accounts[i].LastRefresh = info.LastRefresh
			cfg.Accounts[i].TrialUsageCurrent = info.TrialUsageCurrent
			cfg.Accounts[i].TrialUsageLimit = info.TrialUsageLimit
			cfg.Accounts[i].TrialUsagePercent = info.TrialUsagePercent
			cfg.Accounts[i].TrialStatus = info.TrialStatus
			cfg.Accounts[i].TrialExpiresAt = info.TrialExpiresAt
			return persistAccountLocked(cfg.Accounts[i])
		}
	}
	return nil
}

// GetFilterClaudeCode returns whether Claude Code system prompt detection is enabled.
// Also checks the legacy SanitizeClaudeCodePrompt flag for backward compatibility.
func GetFilterClaudeCode() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterClaudeCode || cfg.SanitizeClaudeCodePrompt
}

// GetFilterEnvNoise returns whether environment noise line stripping is enabled.
func GetFilterEnvNoise() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterEnvNoise
}

// GetFilterStripBoundaries returns whether boundary marker stripping is enabled.
func GetFilterStripBoundaries() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterStripBoundaries
}

// PromptFilterConfig holds all prompt filter settings for API responses.
type PromptFilterConfig struct {
	FilterClaudeCode      bool               `json:"filterClaudeCode"`
	FilterEnvNoise        bool               `json:"filterEnvNoise"`
	FilterStripBoundaries bool               `json:"filterStripBoundaries"`
	Rules                 []PromptFilterRule `json:"rules"`
}

// GetPromptFilterConfig returns all prompt filter settings.
func GetPromptFilterConfig() PromptFilterConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return PromptFilterConfig{Rules: []PromptFilterRule{}}
	}
	rules := make([]PromptFilterRule, len(cfg.PromptFilterRules))
	copy(rules, cfg.PromptFilterRules)
	return PromptFilterConfig{
		FilterClaudeCode:      cfg.FilterClaudeCode || cfg.SanitizeClaudeCodePrompt,
		FilterEnvNoise:        cfg.FilterEnvNoise,
		FilterStripBoundaries: cfg.FilterStripBoundaries,
		Rules:                 rules,
	}
}

// UpdatePromptFilterConfig saves all prompt filter settings atomically.
func UpdatePromptFilterConfig(filterClaudeCode, filterEnvNoise, filterStripBoundaries bool, rules []PromptFilterRule) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.FilterClaudeCode = filterClaudeCode
	cfg.FilterEnvNoise = filterEnvNoise
	cfg.FilterStripBoundaries = filterStripBoundaries
	// Clear legacy flag to avoid double-applying after first save
	cfg.SanitizeClaudeCodePrompt = false
	if rules != nil {
		cfg.PromptFilterRules = rules
	}
	return Save()
}

// GetPromptFilterRules returns the current prompt filter rules.
func GetPromptFilterRules() []PromptFilterRule {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	rules := make([]PromptFilterRule, len(cfg.PromptFilterRules))
	copy(rules, cfg.PromptFilterRules)
	return rules
}

// ThinkingConfig holds settings for AI thinking/reasoning mode.
// When enabled, models output their reasoning process alongside the response.
type ThinkingConfig struct {
	Suffix       string `json:"suffix"`       // Model name suffix that triggers thinking mode
	OpenAIFormat string `json:"openaiFormat"` // Output format for OpenAI-compatible responses
	ClaudeFormat string `json:"claudeFormat"` // Output format for Claude-compatible responses
}

// GetThinkingConfig 获取 thinking 配置。
// 与本包其余 getter 一致:配置尚未加载(cfg == nil)时返回全默认值而不是 panic。
func GetThinkingConfig() ThinkingConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	if cfg == nil {
		return ThinkingConfig{
			Suffix:       "-thinking",
			OpenAIFormat: "reasoning_content",
			ClaudeFormat: "thinking",
		}
	}

	suffix := cfg.ThinkingSuffix
	if suffix == "" {
		suffix = "-thinking"
	}
	openaiFormat := cfg.OpenAIThinkingFormat
	if openaiFormat == "" {
		openaiFormat = "reasoning_content"
	}
	claudeFormat := cfg.ClaudeThinkingFormat
	if claudeFormat == "" {
		claudeFormat = "thinking"
	}

	return ThinkingConfig{
		Suffix:       suffix,
		OpenAIFormat: openaiFormat,
		ClaudeFormat: claudeFormat,
	}
}

// UpdateThinkingConfig 更新 thinking 配置
func UpdateThinkingConfig(suffix, openaiFormat, claudeFormat string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ThinkingSuffix = suffix
	cfg.OpenAIThinkingFormat = openaiFormat
	cfg.ClaudeThinkingFormat = claudeFormat
	return Save()
}

// GetPreferredEndpoint 获取首选端点配置
func GetPreferredEndpoint() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.PreferredEndpoint == "" {
		return "auto"
	}
	return cfg.PreferredEndpoint
}

// UpdatePreferredEndpoint 更新首选端点配置
func UpdatePreferredEndpoint(endpoint string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.PreferredEndpoint = endpoint
	return Save()
}

// GetEndpointFallback returns whether endpoint fallback is enabled. Defaults to true.
func GetEndpointFallback() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.EndpointFallback == nil {
		return true
	}
	return *cfg.EndpointFallback
}

// UpdateEndpointFallback sets the endpoint fallback switch and persists the change.
func UpdateEndpointFallback(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.EndpointFallback = &enabled
	return Save()
}

// GetProxyURL 获取出站代理地址
func GetProxyURL() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.ProxyURL
}

// UpdateProxySettings 更新出站代理配置
func UpdateProxySettings(proxyURL string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ProxyURL = proxyURL
	return Save()
}

// GetAllowOverUsage is deprecated and always returns false. Over-quota routing is
// now strictly per-account — only the upstream Overages switch (OverageStatus=
// ENABLED) keeps an over-quota account routable. The former global override was
// removed; this stub is kept so pool callers compile unchanged.
func GetAllowOverUsage() bool {
	return false
}



// UpdateAllowOverUsage sets the over-usage setting and persists the change.
func UpdateAllowOverUsage(allow bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.AllowOverUsage = allow
	return Save()
}

// GetLogLevel returns the configured log level (debug/info/warn/error). Defaults to "info".
func GetLogLevel() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.LogLevel == "" {
		return "info"
	}
	return cfg.LogLevel
}

// UpdateLogLevel updates the log level setting and persists the change.
func UpdateLogLevel(level string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.LogLevel = level
	return Save()
}

type KiroClientConfig struct {
	KiroVersion   string
	SystemVersion string
	NodeVersion   string
}

func GetKiroClientConfig() KiroClientConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	kiroVersion := "0.11.107"
	if cfg != nil && cfg.KiroVersion != "" {
		kiroVersion = cfg.KiroVersion
	}

	systemVersion := ""
	if cfg != nil {
		systemVersion = cfg.SystemVersion
	}
	if systemVersion == "" {
		systemVersion = defaultSystemVersion()
	}

	nodeVersion := "22.22.0"
	if cfg != nil && cfg.NodeVersion != "" {
		nodeVersion = cfg.NodeVersion
	}

	return KiroClientConfig{
		KiroVersion:   kiroVersion,
		SystemVersion: systemVersion,
		NodeVersion:   nodeVersion,
	}
}

func defaultSystemVersion() string {
	switch runtime.GOOS {
	case "windows":
		return "win32#10.0.22631"
	case "darwin":
		return "darwin#24.6.0"
	default:
		return "linux#6.6.87"
	}
}
