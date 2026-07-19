package auth

import (
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// initTestConfig initializes the config package with a throwaway file so that
// RefreshToken's proxy resolution (config.GetProxyURL) does not dereference a nil cfg.
func initTestConfig(t *testing.T) {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init failed: %v", err)
	}
}

func TestIsApiKeyAccount(t *testing.T) {
	cases := []struct {
		name string
		acc  *config.Account
		want bool
	}{
		{"nil", nil, false},
		{"authMethod api_key", &config.Account{AuthMethod: "api_key"}, true},
		{"authMethod apikey", &config.Account{AuthMethod: "apikey"}, true},
		{"authMethod APIKEY case", &config.Account{AuthMethod: "API_KEY"}, true},
		{"kiroApiKey set", &config.Account{KiroApiKey: "ksk_abc"}, true},
		{"idc", &config.Account{AuthMethod: "idc"}, false},
		{"social", &config.Account{AuthMethod: "social"}, false},
	}
	for _, c := range cases {
		if got := IsApiKeyAccount(c.acc); got != c.want {
			t.Errorf("%s: IsApiKeyAccount = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestNewApiKeyAccount(t *testing.T) {
	acc := NewApiKeyAccount("  ksk_secret123  ", "", "my-key")
	if acc.KiroApiKey != "ksk_secret123" {
		t.Errorf("KiroApiKey = %q, want trimmed ksk_secret123", acc.KiroApiKey)
	}
	if acc.AccessToken != "ksk_secret123" {
		t.Errorf("AccessToken should mirror the key so the proxy Bearer logic works, got %q", acc.AccessToken)
	}
	if acc.AuthMethod != KiroApiKeyAuthMethod {
		t.Errorf("AuthMethod = %q, want %q", acc.AuthMethod, KiroApiKeyAuthMethod)
	}
	if acc.Region != "us-east-1" {
		t.Errorf("Region default = %q, want us-east-1", acc.Region)
	}
	if acc.ExpiresAt != 0 {
		t.Errorf("ExpiresAt = %d, want 0 (never refreshed)", acc.ExpiresAt)
	}
	if !acc.Enabled {
		t.Error("account should be enabled")
	}
	if acc.MachineId == "" {
		t.Error("MachineId should be generated")
	}
	if !IsApiKeyAccount(&acc) {
		t.Error("NewApiKeyAccount result should be detected as an API key account")
	}
}

func TestMaskKiroApiKey(t *testing.T) {
	if got := MaskKiroApiKey("ksk_VLPmABCDEFGHIJKLMNOPICQw"); got != "ksk_VLPm…ICQw" {
		t.Errorf("MaskKiroApiKey = %q, want ksk_VLPm…ICQw", got)
	}
	if got := MaskKiroApiKey("short"); got != "ksk_****" {
		t.Errorf("MaskKiroApiKey(short) = %q, want ksk_****", got)
	}
}

func TestRefreshTokenRejectsApiKeyAccount(t *testing.T) {
	initTestConfig(t)
	acc := &config.Account{AuthMethod: "api_key", KiroApiKey: "ksk_abc", AccessToken: "ksk_abc"}
	_, _, _, _, err := RefreshToken(acc)
	if err == nil {
		t.Fatal("expected RefreshToken to reject api_key accounts")
	}
	if !strings.Contains(err.Error(), "api_key") {
		t.Errorf("error should mention api_key, got %v", err)
	}
}

func TestIsExternalIdpAccountAndHosts(t *testing.T) {
	if !IsExternalIdpAccount(&config.Account{AuthMethod: "external_idp"}) {
		t.Error("external_idp should be detected")
	}
	if IsExternalIdpAccount(&config.Account{AuthMethod: "idc"}) {
		t.Error("idc must not be detected as external_idp")
	}
	if got := ExternalIdpManagementHost("eu-central-1"); got != "management.eu-central-1.kiro.dev" {
		t.Errorf("ExternalIdpManagementHost = %q", got)
	}
	if got := ExternalIdpRuntimeHost(""); got != "runtime.us-east-1.kiro.dev" {
		t.Errorf("ExternalIdpRuntimeHost default = %q", got)
	}
}

func TestRefreshExternalIdpTokenDirectEndpoint(t *testing.T) {
	initTestConfig(t)

	var gotForm string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = r.ParseForm()
			gotForm = r.Form.Encode()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"at-new","refresh_token":"rt-rotated","expires_in":3600}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	// Install a proxy-free auth client so the httptest server is reachable regardless of env proxy.
	old := SetGlobalAuthClientForTest(&http.Client{Transport: &http.Transport{Proxy: nil}})
	defer SetGlobalAuthClientForTest(old)

	acc := &config.Account{
		AuthMethod:    "external_idp",
		ClientID:      "client-123",
		RefreshToken:  "rt-old",
		TokenEndpoint: srv.URL + "/token",
		Scopes:        "openid offline_access",
	}
	access, refresh, expiresAt, profileArn, err := RefreshToken(acc)
	if err != nil {
		t.Fatalf("RefreshToken(external_idp) failed: %v", err)
	}
	if access != "at-new" {
		t.Errorf("access = %q, want at-new", access)
	}
	if refresh != "rt-rotated" {
		t.Errorf("refresh = %q, want rt-rotated", refresh)
	}
	if expiresAt == 0 {
		t.Error("expiresAt should be set from expires_in")
	}
	if profileArn != "" {
		t.Errorf("external_idp refresh should not return a profileArn, got %q", profileArn)
	}
	// Verify the outgoing form used the standard OAuth2 public-client refresh grant.
	for _, want := range []string{"grant_type=refresh_token", "client_id=client-123", "refresh_token=rt-old", "scope="} {
		if !strings.Contains(gotForm, want) {
			t.Errorf("refresh form %q missing %q", gotForm, want)
		}
	}
}

func TestRefreshExternalIdpTokenViaDiscovery(t *testing.T) {
	initTestConfig(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token_endpoint":"` + tokenEndpointFor(r) + `"}`))
		case "/oauth2/v2.0/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"at-disc","expires_in":1800}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	old := SetGlobalAuthClientForTest(&http.Client{Transport: &http.Transport{Proxy: nil}})
	defer SetGlobalAuthClientForTest(old)

	// Point discovery at our test server's issuer.
	prev := externalIdpTokenEndpointDiscoveryURL
	externalIdpTokenEndpointDiscoveryURL = func(issuerURL string) string {
		return srv.URL + "/.well-known/openid-configuration"
	}
	defer func() { externalIdpTokenEndpointDiscoveryURL = prev }()
	// The discovery doc returns a token_endpoint on the same server.
	discoveredTokenEndpoint = srv.URL + "/oauth2/v2.0/token"

	acc := &config.Account{
		AuthMethod:   "external_idp",
		ClientID:     "client-xyz",
		RefreshToken: "rt-old",
		IssuerUrl:    "https://login.microsoftonline.com/tenant/v2.0",
	}
	access, _, expiresAt, _, err := RefreshToken(acc)
	if err != nil {
		t.Fatalf("RefreshToken(external_idp discovery) failed: %v", err)
	}
	if access != "at-disc" {
		t.Errorf("access = %q, want at-disc", access)
	}
	if expiresAt == 0 {
		t.Error("expiresAt should be set")
	}
}

// discoveredTokenEndpoint lets the discovery test inject the token_endpoint the
// mock discovery document should advertise.
var discoveredTokenEndpoint string

func tokenEndpointFor(_ *http.Request) string { return discoveredTokenEndpoint }

func TestSocialLoginConstants(t *testing.T) {
	if SocialIssuerStartUrl != "https://view.awsapps.com/start" {
		t.Errorf("SocialIssuerStartUrl = %q", SocialIssuerStartUrl)
	}
	if SocialAuthMethod != "idc" {
		t.Errorf("SocialAuthMethod = %q, want idc (refresh goes through the standard OIDC endpoint)", SocialAuthMethod)
	}
}
