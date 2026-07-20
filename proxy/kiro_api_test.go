package proxy

import (
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResolveProfileArnReturnsCachedValueWithoutRequest(t *testing.T) {
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("unexpected HTTP request for cached profile ARN")
			return nil, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	account := &config.Account{ProfileArn: " arn:aws:codewhisperer:profile/test "}
	got, err := ResolveProfileArn(account)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "arn:aws:codewhisperer:profile/test" {
		t.Fatalf("expected trimmed cached ARN, got %q", got)
	}
}

func TestRegionalizeURLPrefersProfileArnRegion(t *testing.T) {
	account := &config.Account{
		Region:     "ap-southeast-1",
		ProfileArn: "arn:aws:codewhisperer:us-east-1:123456789012:profile/test",
	}

	rawURL := "https://q.us-east-1.amazonaws.com/getUsageLimits?origin=AI_EDITOR"
	if got := regionalizeURL(rawURL, account); got != rawURL {
		t.Fatalf("expected profile ARN region to keep us-east-1 URL, got %q", got)
	}
}

func TestRegionalizeURLForProfileUsesPayloadProfileArnRegion(t *testing.T) {
	account := &config.Account{Region: "ap-southeast-1"}

	got := regionalizeURLForProfile(
		"https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse",
		account,
		"arn:aws:codewhisperer:eu-central-1:123456789012:profile/test",
	)
	want := "https://q.eu-central-1.amazonaws.com/generateAssistantResponse"
	if got != want {
		t.Fatalf("expected payload profile ARN region URL %q, got %q", want, got)
	}
}

func TestResolveProfileArnFetchesAndCachesProfile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := config.Account{
		ID:           "acct-1",
		Email:        "user@example.com",
		AccessToken:  "access-token",
		Region:       "us-east-1",
		UsageCurrent: 7,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("expected POST, got %s", req.Method)
			}
			if req.URL.Path != "/ListAvailableProfiles" {
				t.Fatalf("expected ListAvailableProfiles path, got %s", req.URL.Path)
			}
			if got := req.Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("expected JSON content type, got %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"profiles":[{"arn":" arn:aws:codewhisperer:profile/fetched "}]} `)),
				Header:     make(http.Header),
			}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	requestAccount := account
	requestAccount.UsageCurrent = 0
	got, err := ResolveProfileArn(&requestAccount)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "arn:aws:codewhisperer:profile/fetched" {
		t.Fatalf("expected fetched ARN, got %q", got)
	}
	if requestAccount.ProfileArn != got {
		t.Fatalf("expected account to be updated with fetched ARN, got %q", requestAccount.ProfileArn)
	}

	accounts := config.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected one persisted account, got %d", len(accounts))
	}
	if accounts[0].ProfileArn != got {
		t.Fatalf("expected persisted account profile ARN %q, got %q", got, accounts[0].ProfileArn)
	}
	if accounts[0].UsageCurrent != 7 {
		t.Fatalf("expected profile cache update to preserve usage fields, got usageCurrent=%v", accounts[0].UsageCurrent)
	}
}

func TestResolveProfileArnSuppressesBuilderIDUnsupportedLookup(t *testing.T) {
	clearProfileArnResolutionCooldowns()
	t.Cleanup(clearProfileArnResolutionCooldowns)

	var calls int32
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			atomic.AddInt32(&calls, 1)
			if req.URL.Path != "/ListAvailableProfiles" {
				t.Fatalf("expected ListAvailableProfiles path, got %s", req.URL.Path)
			}
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader(`{"message":"AWS Builder ID is not supported for this operation.","reason":null}`)),
				Header:     make(http.Header),
			}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	account := &config.Account{
		ID:          "builder-1",
		Email:       "builder@example.com",
		AccessToken: "access-token",
		Provider:    "BuilderId",
		Region:      "us-east-1",
	}

	_, err := ResolveProfileArn(account)
	if err == nil || !isProfileArnResolutionUnsupportedError(err) {
		t.Fatalf("expected Builder ID unsupported error, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected one profile lookup, got %d", got)
	}

	_, err = ResolveProfileArn(account)
	if err == nil || !isProfileArnResolutionSkippedError(err) {
		t.Fatalf("expected skipped profile ARN resolution error, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected no repeated profile lookup after suppression, got %d", got)
	}
}

func TestResolveProfileArnKeepsRefreshFallbackForBuilderIDUnsupportedLookup(t *testing.T) {
	clearProfileArnResolutionCooldowns()
	t.Cleanup(clearProfileArnResolutionCooldowns)

	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader(`{"message":"AWS Builder ID is not supported for this operation.","reason":null}`)),
				Header:     make(http.Header),
			}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600,"profileArn":"arn:aws:codewhisperer:profile/from-refresh"}`))
	}))
	t.Cleanup(authServer.Close)

	oldTokenURL := auth.GetOIDCTokenURLForTest()
	auth.SetOIDCTokenURLForTest(func(string) string { return authServer.URL })
	t.Cleanup(func() { auth.SetOIDCTokenURLForTest(oldTokenURL) })
	oldAuthClient := auth.SetGlobalAuthClientForTest(authServer.Client())
	t.Cleanup(func() { auth.SetGlobalAuthClientForTest(oldAuthClient) })

	account := &config.Account{
		ID:           "builder-refresh-1",
		Email:        "builder@example.com",
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		AuthMethod:   "idc",
		Provider:     "BuilderId",
		Region:       "us-east-1",
	}

	got, err := ResolveProfileArn(account)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "arn:aws:codewhisperer:profile/from-refresh" {
		t.Fatalf("expected refresh fallback ARN, got %q", got)
	}
	if isProfileArnResolutionSuppressed(account) {
		t.Fatalf("refresh fallback success should not suppress future profile resolution")
	}
}

func TestRefreshAccountInfoDoesNotDisableBuilderIDWhenProfileLookupUnsupported(t *testing.T) {
	clearProfileArnResolutionCooldowns()
	t.Cleanup(clearProfileArnResolutionCooldowns)

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := config.Account{
		ID:          "builder-refresh-info-1",
		Email:       "builder@example.com",
		AccessToken: "access-token",
		Provider:    "BuilderId",
		Region:      "us-east-1",
		Enabled:     true,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	var profileCalls, usageCalls int32
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/ListAvailableProfiles":
				atomic.AddInt32(&profileCalls, 1)
				return &http.Response{
					StatusCode: http.StatusForbidden,
					Body:       io.NopCloser(strings.NewReader(`{"message":"AWS Builder ID is not supported for this operation.","reason":null}`)),
					Header:     make(http.Header),
				}, nil
			case "/getUsageLimits":
				atomic.AddInt32(&usageCalls, 1)
				if strings.Contains(req.URL.RawQuery, "profileArn=") {
					t.Fatalf("expected Builder ID usage refresh to continue without profileArn, got query %q", req.URL.RawQuery)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{}`)),
					Header:     make(http.Header),
				}, nil
			default:
				t.Fatalf("unexpected request path %s", req.URL.Path)
				return nil, nil
			}
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	requestAccount := account
	if _, err := RefreshAccountInfo(&requestAccount); err != nil {
		t.Fatalf("expected refresh to continue without profile ARN, got %v", err)
	}
	if got := atomic.LoadInt32(&profileCalls); got != 1 {
		t.Fatalf("expected one profile lookup, got %d", got)
	}
	if got := atomic.LoadInt32(&usageCalls); got != 1 {
		t.Fatalf("expected one usage request, got %d", got)
	}
	accounts := config.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected one account, got %d", len(accounts))
	}
	if !accounts[0].Enabled || accounts[0].BanStatus != "" {
		t.Fatalf("expected account to remain enabled, got enabled=%v banStatus=%q", accounts[0].Enabled, accounts[0].BanStatus)
	}
}

// TestRefreshAccountInfoToleratesTransientAuthFailures: 单次/两次 401/403 不封号
// (仅报错),连续第 authFailureBanThreshold 次才 BANNED+禁用——防止 5min 定时刷新
// 把瞬时上游抖动放大成误封。
func TestRefreshAccountInfoToleratesTransientAuthFailures(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := config.Account{
		ID:          "auth-streak-1",
		Email:       "streak@example.com",
		AccessToken: "access-token",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:123456789012:profile/test",
		Enabled:     true,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}
	t.Cleanup(func() { clearAuthFailureStreak(account.ID) })

	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader(`{"message":"forbidden"}`)),
				Header:     make(http.Header),
			}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	// 前 N-1 次失败:报错但不封。
	for attempt := 1; attempt < authFailureBanThreshold; attempt++ {
		requestAccount := account
		if _, err := RefreshAccountInfo(&requestAccount); err == nil {
			t.Fatalf("attempt %d: expected error", attempt)
		}
		got := config.GetAccounts()[0]
		if !got.Enabled || got.BanStatus == "BANNED" {
			t.Fatalf("attempt %d: transient auth failure must not ban (enabled=%v banStatus=%q)",
				attempt, got.Enabled, got.BanStatus)
		}
	}

	// 第 N 次连续失败:封禁并禁用。
	requestAccount := account
	if _, err := RefreshAccountInfo(&requestAccount); err == nil {
		t.Fatal("final attempt: expected error")
	}
	got := config.GetAccounts()[0]
	if got.Enabled || got.BanStatus != "BANNED" {
		t.Fatalf("expected ban after %d consecutive auth failures, got enabled=%v banStatus=%q",
			authFailureBanThreshold, got.Enabled, got.BanStatus)
	}
}

// TestRefreshAccountInfoRecoversViaTokenRefresh: 401 后先刷 token 重试同一次调用,
// 成功则既不报错也不累计失败(被动过期自愈,不进入封禁判定)。
func TestRefreshAccountInfoRecoversViaTokenRefresh(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := config.Account{
		ID:           "auth-recover-1",
		Email:        "recover@example.com",
		AccessToken:  "stale-access",
		RefreshToken: "refresh-token",
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		AuthMethod:   "idc",
		Region:       "us-east-1",
		ProfileArn:   "arn:aws:codewhisperer:us-east-1:123456789012:profile/test",
		Enabled:      true,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}
	t.Cleanup(func() { clearAuthFailureStreak(account.ID) })

	var usageCalls int32
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/getUsageLimits" {
				t.Fatalf("unexpected request path %s", req.URL.Path)
			}
			// 第一次 401(旧 token),刷新后第二次 200。
			if atomic.AddInt32(&usageCalls, 1) == 1 {
				return &http.Response{
					StatusCode: http.StatusUnauthorized,
					Body:       io.NopCloser(strings.NewReader(`{"message":"expired token"}`)),
					Header:     make(http.Header),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"usageBreakdownList":[{"currentUsage":1,"usageLimit":100}]}`)),
				Header:     make(http.Header),
			}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"fresh-access","refreshToken":"fresh-refresh","expiresIn":3600}`))
	}))
	t.Cleanup(authServer.Close)
	oldTokenURL := auth.GetOIDCTokenURLForTest()
	auth.SetOIDCTokenURLForTest(func(string) string { return authServer.URL })
	t.Cleanup(func() { auth.SetOIDCTokenURLForTest(oldTokenURL) })
	oldAuthClient := auth.SetGlobalAuthClientForTest(authServer.Client())
	t.Cleanup(func() { auth.SetGlobalAuthClientForTest(oldAuthClient) })

	requestAccount := account
	info, err := RefreshAccountInfo(&requestAccount)
	if err != nil {
		t.Fatalf("expected recovery via token refresh, got %v", err)
	}
	if info.UsageCurrent != 1 || info.UsageLimit != 100 {
		t.Fatalf("expected parsed usage 1/100, got %v/%v", info.UsageCurrent, info.UsageLimit)
	}
	if got := atomic.LoadInt32(&usageCalls); got != 2 {
		t.Fatalf("expected exactly 2 usage calls (fail + retry), got %d", got)
	}
	got := config.GetAccounts()[0]
	if !got.Enabled || got.BanStatus == "BANNED" {
		t.Fatalf("recovered account must stay enabled, got enabled=%v banStatus=%q", got.Enabled, got.BanStatus)
	}
	if got.AccessToken != "fresh-access" {
		t.Fatalf("expected refreshed token persisted, got %q", got.AccessToken)
	}
}

// TestRefreshAccountInfoUpdatesOverageFromSameResponse: 定时刷新用同一份
// getUsageLimits 响应顺带更新 overage 状态(零额外上游调用);仅在 OverageStatus
// 已定型(非空)时写回,未定型账号留给 maybeAutoEnableOverage 的 ""-guard。
func TestRefreshAccountInfoUpdatesOverageFromSameResponse(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	determined := config.Account{
		ID:            "overage-piggyback-1",
		Email:         "piggyback@example.com",
		AccessToken:   "access-token",
		ProfileArn:    "arn:aws:codewhisperer:us-east-1:123456789012:profile/test",
		Enabled:       true,
		OverageStatus: "ENABLED", // 已定型:应被响应里的 DISABLED 覆盖
	}
	undetermined := config.Account{
		ID:          "overage-piggyback-2",
		Email:       "undetermined@example.com",
		AccessToken: "access-token",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:123456789012:profile/test2",
		Enabled:     true, // OverageStatus == "":不得被顺带写入
	}
	for _, acc := range []config.Account{determined, undetermined} {
		if err := config.AddAccount(acc); err != nil {
			t.Fatalf("add account: %v", err)
		}
	}
	t.Cleanup(func() {
		clearAuthFailureStreak(determined.ID)
		clearAuthFailureStreak(undetermined.ID)
	})

	usageBody := `{
		"usageBreakdownList":[{"currentUsage":5,"usageLimit":100,"overageRate":0.04,"overageCap":20,"currentOverages":1.5}],
		"subscriptionInfo":{"subscriptionTitle":"KIRO PRO","overageCapability":"OVERAGE_CAPABLE"},
		"overageConfiguration":{"overageStatus":"DISABLED"}
	}`
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/getUsageLimits" {
				t.Fatalf("unexpected request path %s (overage refresh must not add upstream calls)", req.URL.Path)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(usageBody)),
				Header:     make(http.Header),
			}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	reqDetermined := determined
	if _, err := RefreshAccountInfo(&reqDetermined); err != nil {
		t.Fatalf("refresh determined: %v", err)
	}
	reqUndetermined := undetermined
	if _, err := RefreshAccountInfo(&reqUndetermined); err != nil {
		t.Fatalf("refresh undetermined: %v", err)
	}

	var gotDetermined, gotUndetermined *config.Account
	accounts := config.GetAccounts()
	for i := range accounts {
		switch accounts[i].ID {
		case determined.ID:
			gotDetermined = &accounts[i]
		case undetermined.ID:
			gotUndetermined = &accounts[i]
		}
	}
	if gotDetermined == nil || gotUndetermined == nil {
		t.Fatal("accounts missing from config")
	}
	if gotDetermined.OverageStatus != "DISABLED" || gotDetermined.OverageCapability != "OVERAGE_CAPABLE" {
		t.Fatalf("expected piggybacked overage DISABLED/OVERAGE_CAPABLE, got %q/%q",
			gotDetermined.OverageStatus, gotDetermined.OverageCapability)
	}
	if gotDetermined.OverageCap != 20 || gotDetermined.CurrentOverages != 1.5 {
		t.Fatalf("expected overage cap 20 / current 1.5, got %v/%v",
			gotDetermined.OverageCap, gotDetermined.CurrentOverages)
	}
	if gotDetermined.OverageCheckedAt == 0 {
		t.Fatal("expected overageCheckedAt to be stamped")
	}
	if gotUndetermined.OverageStatus != "" {
		t.Fatalf("undetermined account must be left for auto-enable, got OverageStatus=%q", gotUndetermined.OverageStatus)
	}
}

func TestExtractOverageSnapshotReturnsNilWithoutOverageInfo(t *testing.T) {
	if snap := extractOverageSnapshot(nil); snap != nil {
		t.Fatalf("nil usage must yield nil snapshot, got %+v", snap)
	}
	if snap := extractOverageSnapshot(&UsageLimitsResponse{}); snap != nil {
		t.Fatalf("empty usage must yield nil snapshot (never downgrade known state to UNKNOWN), got %+v", snap)
	}
}

func clearProfileArnResolutionCooldowns() {
	profileArnResolutionCooldowns.Range(func(key, _ interface{}) bool {
		profileArnResolutionCooldowns.Delete(key)
		return true
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}
