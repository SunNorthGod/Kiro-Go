package auth

import (
	"kiro-go/config"
	"strings"
	"time"
)

// Kiro API Key 账号（headless 模式，authMethod=api_key）。
//
// 这类账号直接把 Kiro API Key（ksk_ 前缀）当作 Bearer Token 使用：
//   - 无 refreshToken、不刷新、不设过期（ExpiresAt=0）
//   - 调用 Kiro 后端时需附带请求头  tokentype: API_KEY  告知后端该 Bearer 是原生 Kiro API Key
//     （否则上游会按 OAuth access token 处理并要求 profileArn，导致 400 "Invalid profileArn"）
//
// 说明：数据面/控制面调用时附加 tokentype 头属于 proxy/ 层职责（本包不改 proxy/）。
// 本文件提供 KiroApiKeyTokenTypeHeader / KiroApiKeyTokenTypeValue 供 proxy 主线接线时复用。

// KiroApiKeyAuthMethod 是 API Key 账号在 config.Account.AuthMethod 中的取值。
const KiroApiKeyAuthMethod = "api_key"

// KiroApiKeyPrefix 是 Kiro API Key 的标准前缀。
const KiroApiKeyPrefix = "ksk_"

// KiroApiKeyTokenTypeHeader 是调用 Kiro 后端时用于声明原生 API Key 的请求头名与值。
// proxy/ 在为 api_key 账号构建 Kiro 请求时应设置 req.Header.Set(name, value)。
const (
	KiroApiKeyTokenTypeHeader = "tokentype"
	KiroApiKeyTokenTypeValue  = "API_KEY"
)

// NormalizeKiroApiKey 去除首尾空白，返回规范化后的 API Key。
func NormalizeKiroApiKey(apiKey string) string {
	return strings.TrimSpace(apiKey)
}

// IsApiKeyAccount 判断账号是否为 API Key 账号。
// 判定条件：authMethod == api_key/apikey，或显式配置了非空 KiroApiKey。
func IsApiKeyAccount(account *config.Account) bool {
	if account == nil {
		return false
	}
	m := strings.TrimSpace(account.AuthMethod)
	if strings.EqualFold(m, KiroApiKeyAuthMethod) || strings.EqualFold(m, "apikey") {
		return true
	}
	return strings.TrimSpace(account.KiroApiKey) != ""
}

// NewApiKeyAccount 用一个 Kiro API Key 构建一个可直接加入账号池的 config.Account。
//
// - AuthMethod 置为 api_key；KiroApiKey 与 AccessToken 均存放该 key
//   （AccessToken 存 key 使得 proxy 现有的 "Authorization: Bearer <AccessToken>" 逻辑无需改动即可工作）。
// - ExpiresAt=0、RefreshToken 为空：后台刷新逻辑（handler.go 仅在 ExpiresAt>0 时触发刷新）不会命中，
//   且 auth.RefreshToken 对 api_key 账号直接返回错误。
// - region 留空默认 us-east-1；nickname 可选（用于前端展示）。
//
// 返回的账号 Enabled=true。调用方（handler.go）负责去重后 config.AddAccount。
func NewApiKeyAccount(apiKey, region, nickname string) config.Account {
	key := NormalizeKiroApiKey(apiKey)
	if region == "" {
		region = "us-east-1"
	}
	return config.Account{
		ID:          GenerateAccountID(),
		Nickname:    nickname,
		AccessToken: key,
		KiroApiKey:  key,
		AuthMethod:  KiroApiKeyAuthMethod,
		Provider:    "KiroApiKey",
		Region:      region,
		ExpiresAt:   0,
		Enabled:     true,
		MachineId:   config.GenerateMachineId(),
		LastRefresh: time.Now().Unix(),
	}
}

// MaskKiroApiKey 脱敏展示 API Key：保留前缀 8 位 + 后 4 位，中间用省略号。
// 例如 "ksk_VLPm................ICQw" -> "ksk_VLPm…ICQw"。用于 admin 面板显示。
func MaskKiroApiKey(apiKey string) string {
	key := NormalizeKiroApiKey(apiKey)
	runes := []rune(key)
	if len(runes) <= 12 {
		return KiroApiKeyPrefix + "****"
	}
	head := string(runes[:8])
	tail := string(runes[len(runes)-4:])
	return head + "…" + tail
}
