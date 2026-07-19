package auth

import (
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// External IdP（客户自有 IdP，如 Microsoft Entra ID / Kiro 企业版）认证方式。
//
// 与 AWS 直连账号（idc / social）不同，external_idp 账号的 token 由客户企业 IdP 直接签发：
//   - 数据面（generateAssistantResponse 等聊天）走 runtime.{region}.kiro.dev
//   - 控制面（getUsageLimits / ListAvailableModels / ListAvailableProfiles）走 management.{region}.kiro.dev
//   - 调用 Kiro 后端时需附带请求头  TokenType: EXTERNAL_IDP  告知后端该 Bearer 是客户 IdP 直签 token
//
// 刷新流程是标准 OIDC public client：
//   POST {token_endpoint}
//   Content-Type: application/x-www-form-urlencoded
//   grant_type=refresh_token&client_id=..&refresh_token=..&scope=..
// 无需 client_secret（公共客户端）。token_endpoint 缺失时从 issuer_url 做 OIDC discovery 兜底。
//
// 注意：数据面/控制面的 host 路由与请求头（TokenType）属于 proxy/ 层职责（本包不改 proxy/）。
// 本文件提供 ExternalIdpManagementHost / ExternalIdpRuntimeHost / ExternalIdpTokenTypeHeader
// 供 proxy 主线接线时复用，避免魔法字符串散落。

// ExternalIdpAuthMethod 是 external_idp 账号在 config.Account.AuthMethod 中的取值。
const ExternalIdpAuthMethod = "external_idp"

// ExternalIdpTokenTypeHeader 是调用 Kiro 后端时用于声明 external_idp Bearer 的请求头名与值。
// proxy/ 在为 external_idp 账号构建 Kiro 请求时应设置 req.Header.Set(name, value)。
const (
	ExternalIdpTokenTypeHeader = "TokenType"
	ExternalIdpTokenTypeValue  = "EXTERNAL_IDP"
)

// externalIdpTokenEndpointDiscoveryURL 由 issuer 构造 OIDC discovery 文档地址。测试可替换以拦截网络调用。
var externalIdpTokenEndpointDiscoveryURL = func(issuerURL string) string {
	return strings.TrimRight(issuerURL, "/") + "/.well-known/openid-configuration"
}

// IsExternalIdpAccount 判断账号是否为 external_idp（企业 SSO）账号。
func IsExternalIdpAccount(account *config.Account) bool {
	return account != nil && strings.EqualFold(strings.TrimSpace(account.AuthMethod), ExternalIdpAuthMethod)
}

// ExternalIdpManagementHost 返回 external_idp 账号控制面（余额/模型/profile 查询）的 host。
// 例如 region="us-east-1" -> "management.us-east-1.kiro.dev"。
func ExternalIdpManagementHost(region string) string {
	if region == "" {
		region = "us-east-1"
	}
	return fmt.Sprintf("management.%s.kiro.dev", region)
}

// ExternalIdpRuntimeHost 返回 external_idp 账号数据面（generateAssistantResponse 聊天）的 host。
// 例如 region="us-east-1" -> "runtime.us-east-1.kiro.dev"。
func ExternalIdpRuntimeHost(region string) string {
	if region == "" {
		region = "us-east-1"
	}
	return fmt.Sprintf("runtime.%s.kiro.dev", region)
}

// refreshExternalIdpToken 刷新 external_idp（Microsoft Entra / Kiro 企业版）的 access token。
//
// 走标准 OAuth2 public client 的 refresh_token 授权：表单 POST 到 tokenEndpoint。
// tokenEndpoint 为空时用 issuerUrl 做 OIDC discovery 兜底。返回:
// accessToken, refreshToken(轮换, 可能为空), expiresAt(Unix 秒), profileArn(始终为空), error。
func refreshExternalIdpToken(account *config.Account, client *http.Client) (string, string, int64, string, error) {
	if account.RefreshToken == "" {
		return "", "", 0, "", fmt.Errorf("external_idp refresh requires refreshToken")
	}
	if account.ClientID == "" {
		return "", "", 0, "", fmt.Errorf("external_idp refresh requires clientId")
	}

	tokenEndpoint := strings.TrimSpace(account.TokenEndpoint)
	if tokenEndpoint == "" {
		issuer := strings.TrimSpace(account.IssuerUrl)
		if issuer == "" {
			return "", "", 0, "", fmt.Errorf("external_idp refresh requires tokenEndpoint or issuerUrl")
		}
		discovered, err := discoverExternalIdpTokenEndpoint(issuer, client)
		if err != nil {
			return "", "", 0, "", fmt.Errorf("discover token endpoint failed: %w", err)
		}
		tokenEndpoint = discovered
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", account.ClientID)
	form.Set("refresh_token", account.RefreshToken)
	if scopes := strings.TrimSpace(account.Scopes); scopes != "" {
		form.Set("scope", scopes)
	}

	req, _ := http.NewRequest("POST", tokenEndpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", "", 0, "", fmt.Errorf("refresh failed: %d %s", resp.StatusCode, string(respBody))
	}

	// 客户自有 IdP 返回标准 OAuth2 字段（snake_case），不同于 AWS 端点的 camelCase。
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", 0, "", err
	}
	if result.AccessToken == "" {
		return "", "", 0, "", fmt.Errorf("external_idp refresh returned empty access_token")
	}

	var expiresAt int64
	if result.ExpiresIn > 0 {
		expiresAt = time.Now().Unix() + int64(result.ExpiresIn)
	}
	// external_idp 的 token 响应不含 profileArn，返回空字符串（与 RefreshToken 签名保持一致）。
	return result.AccessToken, result.RefreshToken, expiresAt, "", nil
}

// discoverExternalIdpTokenEndpoint 从 OIDC issuer 的 discovery 文档解析 token_endpoint。
func discoverExternalIdpTokenEndpoint(issuerURL string, client *http.Client) (string, error) {
	discoveryURL := externalIdpTokenEndpointDiscoveryURL(issuerURL)

	req, _ := http.NewRequest("GET", discoveryURL, nil)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("OIDC discovery failed: %d %s", resp.StatusCode, string(respBody))
	}

	var doc struct {
		TokenEndpoint string `json:"token_endpoint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", err
	}
	if strings.TrimSpace(doc.TokenEndpoint) == "" {
		return "", fmt.Errorf("OIDC discovery response missing token_endpoint")
	}
	return doc.TokenEndpoint, nil
}
