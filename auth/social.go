package auth

// Social 登录（app.kiro.dev/signin，支持 Google / GitHub / Microsoft / Amazon / 邮箱等）。
//
// 关键认知：app.kiro.dev/signin 页面只是"登录方式选择器"，真正的 OAuth 并不是一个独立的
// /oauth/token 接口。Kiro IDE 在底层复用的是 AWS Builder ID 的授权码流程：
//
//	issuer(startUrl) = https://view.awsapps.com/start
//	POST oidc.{region}.amazonaws.com/client/register   （注册 public 客户端, grantTypes=authorization_code+refresh_token）
//	GET  oidc.{region}.amazonaws.com/authorize?...      （PKCE: code_challenge=S256, 用户在浏览器登录/授权）
//	POST oidc.{region}.amazonaws.com/token              （authorization_code + code_verifier 换取 refreshToken）
//
// 因此 Social 登录最终产出的账号本质上是一个 IdC/Builder ID 凭证（clientId + clientSecret +
// refreshToken），后续刷新走标准 OIDC endpoint —— 即 auth.RefreshToken 里的 refreshOIDCToken 分支。
// 所以创建账号时应使用 AuthMethod = SocialAuthMethod ("idc")，Provider = "Social"。
//
// 实现上完全复用 iam_sso.go 的授权码流程（StartIamSsoLogin / CompleteIamSsoLogin），
// 仅把 startUrl 固定为 Builder ID 门户。

// SocialIssuerStartUrl 是 Social 登录复用的 AWS Builder ID 门户 issuer。
const SocialIssuerStartUrl = "https://view.awsapps.com/start"

// SocialAuthMethod 是 Social 登录建号时应写入 config.Account.AuthMethod 的值。
// 使用 "idc" 使得后续刷新走标准 OIDC endpoint（refreshOIDCToken），与真实 Kiro IDE 行为一致。
const SocialAuthMethod = "idc"

// SocialProvider 建议写入 config.Account.Provider，便于前端区分登录来源。
const SocialProvider = "Social"

// StartSocialLogin 发起 Social 登录。
//
// 返回 sessionID（用于后续 CompleteSocialLogin）、authorizeUrl（让用户在浏览器打开完成登录/授权）
// 以及会话有效期（秒）。region 留空默认 us-east-1（Kiro Social 账号通常在 us-east-1）。
//
// 用户在浏览器完成登录后会被重定向到 http://127.0.0.1/oauth/callback?code=...&state=...
// （浏览器显示"无法连接"是正常的），把整段回调 URL 粘贴回来交给 CompleteSocialLogin 即可。
func StartSocialLogin(region string) (sessionID, authorizeUrl string, expiresIn int, err error) {
	if region == "" {
		region = "us-east-1"
	}
	// Social 登录固定门户+区域，name 留空（账号标签后续由 GetUserInfo 的 email 决定）。
	return StartIamSsoLogin(SocialIssuerStartUrl, region, "")
}

// CompleteSocialLogin 用回调 URL（含授权码）换取 token，完成 Social 登录。
//
// 返回的凭证应存为 AuthMethod = SocialAuthMethod ("idc")、Provider = SocialProvider ("Social")。
// callbackUrl 支持完整回调 URL（含 code 与 state）。
func CompleteSocialLogin(sessionID, callbackUrl string) (accessToken, refreshToken, clientID, clientSecret, region string, expiresIn int, err error) {
	// 丢弃 IdC 的 label（Social 账号标签由上层 GetUserInfo 的 email 决定）。
	accessToken, refreshToken, clientID, clientSecret, region, _, expiresIn, err = CompleteIamSsoLogin(sessionID, callbackUrl)
	return
}
