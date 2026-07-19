package proxy

import (
	"fmt"
	"kiro-go/auth"
	"kiro-go/config"
	"net/http"
)

const (
	kiroStreamingSDKVersion = "1.0.34"
	kiroRuntimeSDKVersion   = "1.0.0"
)

type kiroHeaderValues struct {
	UserAgent    string
	AmzUserAgent string
	Host         string
}

func buildStreamingHeaderValues(account *config.Account, host string) kiroHeaderValues {
	return buildKiroHeaderValues(account, host, "codewhispererstreaming", kiroStreamingSDKVersion, "m/E")
}

func buildRuntimeHeaderValues(account *config.Account, host string) kiroHeaderValues {
	return buildKiroHeaderValues(account, host, "codewhispererruntime", kiroRuntimeSDKVersion, "m/N,E")
}

func buildKiroHeaderValues(account *config.Account, host, apiName, sdkVersion, mode string) kiroHeaderValues {
	clientCfg := config.GetKiroClientConfig()
	machineID := ""
	if account != nil {
		machineID = account.MachineId
	}

	userAgent := fmt.Sprintf(
		"aws-sdk-js/%s ua/2.1 os/%s lang/js md/nodejs#%s api/%s#%s %s KiroIDE-%s",
		sdkVersion,
		clientCfg.SystemVersion,
		clientCfg.NodeVersion,
		apiName,
		sdkVersion,
		mode,
		clientCfg.KiroVersion,
	)
	amzUserAgent := fmt.Sprintf("aws-sdk-js/%s KiroIDE-%s", sdkVersion, clientCfg.KiroVersion)
	if machineID != "" {
		userAgent += "-" + machineID
		amzUserAgent += "-" + machineID
	}

	return kiroHeaderValues{
		UserAgent:    userAgent,
		AmzUserAgent: amzUserAgent,
		Host:         host,
	}
}

func applyKiroBaseHeaders(req *http.Request, account *config.Account, values kiroHeaderValues) {
	if account != nil && account.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+account.AccessToken)
	}
	req.Header.Set("User-Agent", values.UserAgent)
	req.Header.Set("x-amz-user-agent", values.AmzUserAgent)
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	if values.Host != "" {
		req.Host = values.Host
	}
	applyAuthTypeHeaders(req, account)
}

// applyAuthTypeHeaders 为 external_idp / api_key 账号追加 tokentype 头,告知 Kiro 后端
// 该 Bearer 的类型(否则后端按 OAuth access token 处理,api_key/external_idp 会 400)。
// 该函数被 applyKiroBaseHeaders 统一调用,故数据面(generateAssistantResponse)与
// 控制面(getUsageLimits/ListAvailable* 经 setKiroHeaders)都会带上正确的 tokentype。
//
// 注意:header 名用小写 "tokentype"(对齐真实 Kiro 客户端与 Rust 实现)。这里直接写
// req.Header map 而非 Set(),避免 Go 规范化成 "Tokentype";HTTP/2 本就发小写,HTTP/1.1
// 亦保持小写,最大化后端兼容(部分后端对该头大小写敏感)。
func applyAuthTypeHeaders(req *http.Request, account *config.Account) {
	if account == nil {
		return
	}
	// external_idp 与 api_key 共用同一个小写头名 "tokentype",仅值不同(对齐 Rust 实现)。
	const tokenTypeHeaderLower = "tokentype"
	switch {
	case auth.IsExternalIdpAccount(account):
		req.Header[tokenTypeHeaderLower] = []string{auth.ExternalIdpTokenTypeValue}
		// 企业版 profileArn 通过 header 传递(同时也保留在 body 内)。
		if arn := account.ProfileArn; arn != "" {
			req.Header.Set("x-amzn-kiro-profile-arn", arn)
		}
	case auth.IsApiKeyAccount(account):
		req.Header[tokenTypeHeaderLower] = []string{auth.KiroApiKeyTokenTypeValue}
	}
}
