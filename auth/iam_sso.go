package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"kiro-go/logger"

	"github.com/google/uuid"
)

type IamSsoSession struct {
	ClientID     string
	ClientSecret string
	CodeVerifier string
	State        string
	Region       string
	StartUrl     string
	Name         string // 用户填写的备注/用户名，登录成功后作为账号标签（email + nickname）
	RedirectUri  string
	ExpiresAt    time.Time
}

// idcAmzUserAgent 与真实 Kiro / AWS sso-oidc SDK 对齐，注册客户端与换 token 时携带，降低被拒概率。
const idcAmzUserAgent = "aws-sdk-js/3.738.0 ua/2.1 os/other lang/js md/browser#unknown_unknown api/sso-oidc#3.738.0 m/E KiroIDE"

// idcCandidateRegions 是自动探测门户所属区域时按常见程度排序的候选区域。
// 注册 OIDC 客户端时带 issuerUrl(=门户 startUrl)，只有门户真正所属的区域返回 200、
// 其余返回 400，据此可自动探测门户区域（含各种区域甚至跨区门户），无需用户手填。
var idcCandidateRegions = []string{
	"us-east-1", "us-west-2", "eu-central-1", "eu-west-1",
	"ap-southeast-1", "ap-southeast-2", "ap-northeast-1", "eu-west-2",
}

func oidcBaseFor(region string) string {
	return fmt.Sprintf("https://oidc.%s.amazonaws.com", region)
}

// nicknameFromStartUrl 从门户地址派生一个可读名称，如
// https://d-9066740be4.awsapps.com/start -> IdC-d-9066740be4，用于用户未填备注时的兜底标签。
func nicknameFromStartUrl(startUrl string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(startUrl), "https://"), "http://")
	host := s
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	sub := host
	if i := strings.IndexByte(sub, '.'); i >= 0 {
		sub = sub[:i]
	}
	if sub == "" {
		return ""
	}
	return "IdC-" + sub
}

// parseAuthCode 从用户粘贴的内容里解析 (code, state)。既支持完整回调 URL
// (http://127.0.0.1/oauth/callback?code=...&state=...)，也支持只粘贴 code。
func parseAuthCode(input string) (code, state string) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", ""
	}
	if !strings.Contains(s, "code=") {
		return s, "" // 只粘了 code
	}
	query := s
	if i := strings.LastIndexByte(s, '?'); i >= 0 {
		query = s[i+1:]
	}
	for _, pair := range strings.Split(query, "&") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			continue
		}
		v, err := url.QueryUnescape(kv[1])
		if err != nil {
			v = kv[1]
		}
		switch kv[0] {
		case "code":
			code = v
		case "state":
			state = v
		}
	}
	return code, state
}

var (
	sessions   = make(map[string]*IamSsoSession)
	sessionsMu sync.RWMutex
)

var scopes = []string{
	"codewhisperer:completions",
	"codewhisperer:analysis",
	"codewhisperer:conversations",
	"codewhisperer:transformations",
	"codewhisperer:taskassist",
}

// StartIamSsoLogin 发起企业 IdC(IAM SSO)授权码登录。
//
// startUrl 为企业门户地址（如 https://d-xxxx.awsapps.com/start）；region 留空或 "auto"
// 时自动探测门户所属区域（逐候选区域试注册，只有正确区域返回 200）；name 为用户填写的
// 备注/用户名，登录成功后作为账号标签。
func StartIamSsoLogin(startUrl, region, name string) (sessionID, authorizeUrl string, expiresIn int, err error) {
	startUrl = strings.TrimSpace(startUrl)
	if startUrl == "" {
		return "", "", 0, fmt.Errorf("startUrl 不能为空")
	}
	name = strings.TrimSpace(name)
	redirectUri := "http://127.0.0.1/oauth/callback"

	// 1. 注册 OIDC 客户端（显式区域直接用，否则自动探测门户区域）。
	var clientID, clientSecret string
	explicitRegion := strings.TrimSpace(region)
	if explicitRegion != "" && !strings.EqualFold(explicitRegion, "auto") {
		region = explicitRegion
		clientID, clientSecret, err = registerOIDCClient(oidcBaseFor(region), startUrl, redirectUri)
		if err != nil {
			return "", "", 0, fmt.Errorf("在区域 %s 注册客户端失败（该门户可能不属于此区域）: %w", region, err)
		}
	} else {
		region, clientID, clientSecret, err = detectRegionAndRegister(startUrl, redirectUri)
		if err != nil {
			return "", "", 0, err
		}
	}

	// 2. 生成 PKCE
	codeVerifier := generateCodeVerifier()
	codeChallenge := generateCodeChallenge(codeVerifier)
	state := uuid.New().String()

	// 3. 构建授权 URL（scopes 空格分隔，与真实 Kiro 客户端一致）
	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", clientID)
	params.Set("redirect_uri", redirectUri)
	params.Set("scopes", strings.Join(scopes, " "))
	params.Set("state", state)
	params.Set("code_challenge", codeChallenge)
	params.Set("code_challenge_method", "S256")

	authorizeUrl = fmt.Sprintf("%s/authorize?%s", oidcBaseFor(region), params.Encode())

	// 4. 保存会话
	sessionID = uuid.New().String()
	session := &IamSsoSession{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		CodeVerifier: codeVerifier,
		State:        state,
		Region:       region,
		StartUrl:     startUrl,
		Name:         name,
		RedirectUri:  redirectUri,
		ExpiresAt:    time.Now().Add(15 * time.Minute),
	}

	sessionsMu.Lock()
	sessions[sessionID] = session
	sessionsMu.Unlock()

	// 清理过期会话
	go cleanupExpiredSessions()

	return sessionID, authorizeUrl, 900, nil
}

// detectRegionAndRegister 逐候选区域试注册 OIDC 客户端，第一个成功的即为门户所属区域。
func detectRegionAndRegister(startUrl, redirectUri string) (region, clientID, clientSecret string, err error) {
	var lastErr error
	for _, r := range idcCandidateRegions {
		cid, csec, e := registerOIDCClient(oidcBaseFor(r), startUrl, redirectUri)
		if e == nil {
			logger.Infof("[IamSso] 自动探测到门户 %s 所属区域: %s", startUrl, r)
			return r, cid, csec, nil
		}
		lastErr = e
	}
	return "", "", "", fmt.Errorf("无法自动探测该门户所属区域（已尝试 %d 个常见区域），请确认门户地址是否正确。最后错误: %v", len(idcCandidateRegions), lastErr)
}

// CompleteIamSsoLogin 完成企业 IdC 登录：用用户粘贴回来的回调内容（含 code）换取 token。
// label 为账号标签：优先用用户填写的备注/用户名，否则由门户地址派生（IdC-xxxx）。
func CompleteIamSsoLogin(sessionID, callbackUrl string) (accessToken, refreshToken, clientID, clientSecret, region, label string, expiresIn int, err error) {
	sessionsMu.RLock()
	session, ok := sessions[sessionID]
	sessionsMu.RUnlock()

	if !ok {
		return "", "", "", "", "", "", 0, fmt.Errorf("会话不存在或已过期")
	}

	if time.Now().After(session.ExpiresAt) {
		sessionsMu.Lock()
		delete(sessions, sessionID)
		sessionsMu.Unlock()
		return "", "", "", "", "", "", 0, fmt.Errorf("会话已过期，请重新发起登录")
	}

	// 从粘贴内容解析 code/state（支持完整回调 URL 或只粘贴 code）
	code, state := parseAuthCode(callbackUrl)
	if code == "" {
		return "", "", "", "", "", "", 0, fmt.Errorf("未能从粘贴内容解析出授权码 code，请粘贴完整的回调地址（http://127.0.0.1/oauth/callback?code=...）或 code")
	}
	if state != "" && state != session.State {
		return "", "", "", "", "", "", 0, fmt.Errorf("state 不匹配（可能粘贴了旧链接），请重新发起登录")
	}

	// 用 code 换取 token
	accessToken, refreshToken, expiresIn, err = exchangeToken(
		oidcBaseFor(session.Region),
		session.ClientID,
		session.ClientSecret,
		code,
		session.CodeVerifier,
		session.RedirectUri,
	)
	if err != nil {
		return "", "", "", "", "", "", 0, err
	}

	label = session.Name
	if label == "" {
		label = nicknameFromStartUrl(session.StartUrl)
	}

	// 清理会话
	sessionsMu.Lock()
	delete(sessions, sessionID)
	sessionsMu.Unlock()

	return accessToken, refreshToken, session.ClientID, session.ClientSecret, session.Region, label, expiresIn, nil
}

func registerOIDCClient(oidcBase, startUrl, redirectUri string) (clientID, clientSecret string, err error) {
	payload := map[string]interface{}{
		"clientName":   "Kiro IDE",
		"clientType":   "public",
		"scopes":       scopes,
		"grantTypes":   []string{"authorization_code", "refresh_token"},
		"redirectUris": []string{redirectUri},
		"issuerUrl":    startUrl,
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", oidcBase+"/client/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-amz-user-agent", idcAmzUserAgent)
	req.Header.Set("User-Agent", "node")

	resp, err := httpClient().Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", err
	}

	return result.ClientID, result.ClientSecret, nil
}

func exchangeToken(oidcBase, clientID, clientSecret, code, codeVerifier, redirectUri string) (accessToken, refreshToken string, expiresIn int, err error) {
	payload := map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"grantType":    "authorization_code",
		"redirectUri":  redirectUri,
		"code":         code,
		"codeVerifier": codeVerifier,
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", oidcBase+"/token", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-amz-user-agent", idcAmzUserAgent)
	req.Header.Set("User-Agent", "node")

	resp, err := httpClient().Do(req)
	if err != nil {
		return "", "", 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", "", 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", 0, err
	}

	return result.AccessToken, result.RefreshToken, result.ExpiresIn, nil
}

func generateCodeVerifier() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func generateCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func joinScopes() string {
	result := ""
	for i, s := range scopes {
		if i > 0 {
			result += ","
		}
		result += s
	}
	return result
}

func cleanupExpiredSessions() {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	now := time.Now()
	for id, s := range sessions {
		if now.After(s.ExpiresAt) {
			delete(sessions, id)
		}
	}
}
