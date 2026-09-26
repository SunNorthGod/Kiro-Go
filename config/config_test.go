package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeAPIKeyAccountPipeRegionAndMachineId(t *testing.T) {
	account := Account{
		KiroApiKey: " ksk_test_key|eu-central-1 ",
		AuthMethod: "API KEY",
	}
	if err := NormalizeAPIKeyAccount(&account); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if account.KiroApiKey != "ksk_test_key" {
		t.Fatalf("key = %q", account.KiroApiKey)
	}
	if account.AccessToken != "ksk_test_key" {
		t.Fatalf("accessToken should mirror api key, got %q", account.AccessToken)
	}
	if account.AuthMethod != "api_key" {
		t.Fatalf("authMethod = %q", account.AuthMethod)
	}
	if account.Region != "eu-central-1" {
		t.Fatalf("region = %q", account.Region)
	}
	if account.RefreshToken != "" || account.ProfileArn != "" || account.ExpiresAt != 0 {
		t.Fatalf("oauth fields should be cleared: %+v", account)
	}
	wantMachine := MachineIdFromAPIKey("ksk_test_key")
	if account.MachineId != wantMachine {
		t.Fatalf("machineId = %q, want %q", account.MachineId, wantMachine)
	}
	if !IsAPIKeyAccount(&account) {
		t.Fatal("expected IsAPIKeyAccount true")
	}
}

func TestAddAccountSameAPIKeyReplacesInPlace(t *testing.T) {
	// Local semantics: re-importing an account with the same ksk_ key updates
	// the existing entry in place (AddOrReplaceAccount) instead of erroring.
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	first := Account{ID: "api-1", KiroApiKey: "ksk_dup", AuthMethod: "api_key", Enabled: true}
	if err := AddOrReplaceAccount(&first); err != nil {
		t.Fatalf("add first: %v", err)
	}
	second := Account{ID: "api-2", KiroApiKey: "ksk_dup", AuthMethod: "api_key", Enabled: true}
	if err := AddOrReplaceAccount(&second); err != nil {
		t.Fatalf("same-key re-import should replace in place: %v", err)
	}
	got := GetAccounts()
	if len(got) != 1 {
		t.Fatalf("expected in-place replacement, got %d accounts", len(got))
	}
	if got[0].ID != "api-1" {
		t.Fatalf("replacement must keep the existing id, got %q", got[0].ID)
	}
}

func TestSplitKiroAPIKeyAndRegionValidation(t *testing.T) {
	key, region, err := SplitKiroAPIKeyAndRegion("ksk_abc|us-east-1")
	if err != nil || key != "ksk_abc" || region != "us-east-1" {
		t.Fatalf("got key=%q region=%q err=%v", key, region, err)
	}
	if _, _, err := SplitKiroAPIKeyAndRegion("ksk_abc|us-east-1|extra"); err == nil {
		t.Fatal("expected multi-pipe error")
	}
	if _, _, err := SplitKiroAPIKeyAndRegion("|us-east-1"); err == nil {
		t.Fatal("expected empty key error")
	}
}

func TestUpdateSettingsPatchPreservesOmittedAPIKeyFields(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := UpdateSettings("proxy-api-key", true, "admin-password"); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	if err := UpdateSettingsPatch(nil, nil, "new-admin-password"); err != nil {
		t.Fatalf("patch settings: %v", err)
	}

	if got := GetApiKey(); got != "proxy-api-key" {
		t.Fatalf("expected API key to be preserved, got %q", got)
	}
	if !IsApiKeyRequired() {
		t.Fatalf("expected requireApiKey to stay enabled")
	}
	if got := GetPassword(); got != "new-admin-password" {
		t.Fatalf("expected password to update, got %q", got)
	}
}

// Auth enforcement is mandatory now: even after explicitly requesting
// requireApiKey=false, IsApiKeyRequired must stay true. The legacy apiKey field
// can still be cleared independently.
func TestAuthAlwaysRequiredEvenWhenPatchedOff(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := UpdateSettings("proxy-api-key", true, "admin-password"); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	emptyKey := ""
	requireAPIKey := false
	if err := UpdateSettingsPatch(&emptyKey, &requireAPIKey, ""); err != nil {
		t.Fatalf("patch settings: %v", err)
	}

	if got := GetApiKey(); got != "" {
		t.Fatalf("expected legacy API key to be cleared, got %q", got)
	}
	if !IsApiKeyRequired() {
		t.Fatalf("expected auth to remain required regardless of the patch")
	}
	if got := GetPassword(); got != "admin-password" {
		t.Fatalf("expected password to be preserved, got %q", got)
	}
}

func TestUpdateAccountStaleSnapshotPreservesCredentialRotation(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := Account{
		ID:            "rotation-account",
		AccessToken:   "access-1",
		RefreshToken:  "refresh-1",
		ClientID:      "client",
		AuthMethod:    "external_idp",
		Region:        "us-east-1",
		ExpiresAt:     100,
		ProfileArn:    "arn:aws:codewhisperer:us-east-1:123456789012:profile/one",
		TokenEndpoint: "https://login.microsoftonline.com/tenant/oauth2/v2.0/token",
		IssuerUrl:     "https://login.microsoftonline.com/tenant/v2.0",
		Scopes:        "scope-one",
		Enabled:       true,
	}
	if err := AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}
	stale := GetAccounts()[0]

	const rotatedProfile = "arn:aws:codewhisperer:eu-central-1:123456789012:profile/two"
	if err := UpdateAccountCredentialState(
		account.ID,
		"access-2",
		"refresh-2",
		200,
		rotatedProfile,
	); err != nil {
		t.Fatalf("rotate credential: %v", err)
	}

	stale.Enabled = false
	stale.BanStatus = "BANNED"
	stale.BanReason = "stale status update"
	if err := UpdateAccount(account.ID, stale); err != nil {
		t.Fatalf("apply stale status snapshot: %v", err)
	}

	got := GetAccounts()[0]
	if got.AccessToken != "access-2" ||
		got.RefreshToken != "refresh-2" ||
		got.ExpiresAt != 200 ||
		got.ProfileArn != rotatedProfile {
		t.Fatalf("stale status update reverted credential state: %+v", got)
	}
	if got.RefreshTokenFingerprint != RefreshTokenFingerprint("refresh-1") {
		t.Fatalf("original refresh token fingerprint = %q", got.RefreshTokenFingerprint)
	}
	if got.Enabled || got.BanStatus != "BANNED" || got.BanReason != "stale status update" {
		t.Fatalf("status fields were not applied: %+v", got)
	}
}

// TestAccountAllowOverageMigration verifies that a config.json from before the
// upstream-Overages-switch refactor (which carried `allowOverage: true` per
// account) is migrated into OverageStatus="ENABLED" on first load, and that
// the legacy field is cleared so future saves don't re-emit it.
func TestAccountAllowOverageMigration(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")

	seed := map[string]interface{}{
		"password":      "p",
		"port":          8080,
		"host":          "0.0.0.0",
		"requireApiKey": false,
		"accounts": []map[string]interface{}{
			{"id": "acc-allow", "enabled": true, "allowOverage": true},
			{"id": "acc-deny", "enabled": true, "allowOverage": false},
			{"id": "acc-already-set", "enabled": true, "allowOverage": true, "overageStatus": "DISABLED"},
		},
	}
	raw, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	accounts := GetAccounts()
	byID := map[string]Account{}
	for _, a := range accounts {
		byID[a.ID] = a
	}

	if got := byID["acc-allow"].OverageStatus; got != "ENABLED" {
		t.Fatalf("expected acc-allow to migrate to OverageStatus=ENABLED, got %q", got)
	}
	if byID["acc-allow"].LegacyAllowOverage {
		t.Fatalf("expected legacy allowOverage to be cleared after migration")
	}
	if got := byID["acc-deny"].OverageStatus; got != "" {
		t.Fatalf("expected acc-deny to keep empty OverageStatus, got %q", got)
	}
	// Pre-set OverageStatus must win over the legacy field.
	if got := byID["acc-already-set"].OverageStatus; got != "DISABLED" {
		t.Fatalf("expected acc-already-set OverageStatus to be preserved, got %q", got)
	}
	if byID["acc-already-set"].LegacyAllowOverage {
		t.Fatalf("expected legacy field to still be cleared on acc-already-set")
	}

	// Re-read the file and confirm legacy field is gone (so it doesn't drift
	// back in on later saves).
	on_disk, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var reloaded struct {
		Accounts []map[string]interface{} `json:"accounts"`
	}
	if err := json.Unmarshal(on_disk, &reloaded); err != nil {
		t.Fatalf("decode reload: %v", err)
	}
	for _, a := range reloaded.Accounts {
		if _, ok := a["allowOverage"]; ok {
			t.Fatalf("expected allowOverage to be omitted from persisted file, got %+v", a)
		}
	}
}

// TestAddOrReplaceAccountDedup verifies that (re-)adding an account with the same
// identity refreshes it in place (stable ID, preserved CreatedAt) instead of
// piling up duplicates, while genuinely different identities still append.
func TestAddOrReplaceAccountDedup(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}

	// First insert: a fresh account keyed by UserId.
	first := Account{
		ID:           "id-first",
		Email:        "alice@example.com",
		UserId:       "user-alice",
		RefreshToken: "refresh-v1",
		AccessToken:  "access-v1",
		AuthMethod:   "social",
		Region:       "us-east-1",
		Enabled:      true,
		CreatedAt:    1000,
	}
	if err := AddOrReplaceAccount(&first); err != nil {
		t.Fatalf("add first: %v", err)
	}
	if got := len(GetAccounts()); got != 1 {
		t.Fatalf("expected 1 account after first add, got %d", got)
	}

	// Re-import the SAME identity (same UserId) with rotated credentials and a
	// different incoming ID: must update in place, not append.
	reimport := Account{
		ID:           "id-second-ignored",
		Email:        "alice@example.com",
		UserId:       "user-alice",
		RefreshToken: "refresh-v2",
		AccessToken:  "access-v2",
		AuthMethod:   "social",
		Region:       "us-east-1",
		Enabled:      true,
		CreatedAt:    2000,
	}
	if err := AddOrReplaceAccount(&reimport); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	accounts := GetAccounts()
	if got := len(accounts); got != 1 {
		t.Fatalf("expected re-import to dedupe to 1 account, got %d", got)
	}
	a := accounts[0]
	if a.ID != "id-first" {
		t.Fatalf("expected stable ID id-first after re-import, got %q", a.ID)
	}
	if a.RefreshToken != "refresh-v2" || a.AccessToken != "access-v2" {
		t.Fatalf("expected credentials to be refreshed, got refresh=%q access=%q", a.RefreshToken, a.AccessToken)
	}
	if a.CreatedAt != 1000 {
		t.Fatalf("expected original CreatedAt=1000 to be preserved, got %d", a.CreatedAt)
	}
	// The caller's struct should reflect the effective stored ID.
	if reimport.ID != "id-first" {
		t.Fatalf("expected reimport.ID rewritten to id-first, got %q", reimport.ID)
	}

	// A different identity must still append.
	second := Account{
		ID:           "id-bob",
		Email:        "bob@example.com",
		UserId:       "user-bob",
		RefreshToken: "refresh-bob",
		AuthMethod:   "social",
		Enabled:      true,
	}
	if err := AddOrReplaceAccount(&second); err != nil {
		t.Fatalf("add second: %v", err)
	}
	if got := len(GetAccounts()); got != 2 {
		t.Fatalf("expected 2 accounts after distinct add, got %d", got)
	}

	// Dedup by Email when UserId is absent on the incoming record.
	byEmail := Account{
		ID:           "id-bob-nouser",
		Email:        "bob@example.com",
		RefreshToken: "refresh-bob-v2",
		AuthMethod:   "social",
		Enabled:      true,
	}
	if err := AddOrReplaceAccount(&byEmail); err != nil {
		t.Fatalf("add byEmail: %v", err)
	}
	if got := len(GetAccounts()); got != 2 {
		t.Fatalf("expected email dedupe to keep 2 accounts, got %d", got)
	}
}

// TestDefaultLimitSemantics verifies the unified per-key default-limit rules:
// nil (never configured) resolves to the factory default; an explicit 0 means
// unlimited; a positive N is returned verbatim; and negatives are clamped to 0.
func TestDefaultLimitSemantics(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}

	// Fresh config → never configured → factory defaults (5 / 20), NOT 0.
	if got := GetDefaultMaxConcurrency(); got != FactoryDefaultConcurrency {
		t.Fatalf("unset concurrency = %d, want factory %d", got, FactoryDefaultConcurrency)
	}
	if got := GetDefaultMaxRPM(); got != FactoryDefaultRPM {
		t.Fatalf("unset RPM = %d, want factory %d", got, FactoryDefaultRPM)
	}

	// Explicit 0 means unlimited and must be preserved (not coerced to factory).
	zero := 0
	if err := UpdateDefaultLimits(&zero, &zero); err != nil {
		t.Fatalf("update to 0: %v", err)
	}
	if got := GetDefaultMaxConcurrency(); got != 0 {
		t.Fatalf("concurrency after set 0 = %d, want 0 (unlimited)", got)
	}
	if got := GetDefaultMaxRPM(); got != 0 {
		t.Fatalf("RPM after set 0 = %d, want 0 (unlimited)", got)
	}

	// Positive values pass through unchanged.
	c, r := 12, 99
	if err := UpdateDefaultLimits(&c, &r); err != nil {
		t.Fatalf("update to N: %v", err)
	}
	if got := GetDefaultMaxConcurrency(); got != 12 {
		t.Fatalf("concurrency = %d, want 12", got)
	}
	if got := GetDefaultMaxRPM(); got != 99 {
		t.Fatalf("RPM = %d, want 99", got)
	}

	// Negative values are clamped to 0 (there is no -1 sentinel any more).
	neg := -1
	if err := UpdateDefaultLimits(&neg, &neg); err != nil {
		t.Fatalf("update to negative: %v", err)
	}
	if got := GetDefaultMaxConcurrency(); got != 0 {
		t.Fatalf("negative concurrency clamped = %d, want 0", got)
	}
	if got := GetDefaultMaxRPM(); got != 0 {
		t.Fatalf("negative RPM clamped = %d, want 0", got)
	}
}

// TestLegacyLimitMigration verifies that a config.json written under the old
// semantics (where -1 meant "unlimited") is remapped to the unified rule where
// 0 means unlimited, on first load — for both the per-key overrides and the
// system-wide defaults.
func TestLegacyLimitMigration(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")

	seed := map[string]interface{}{
		"password":              "p",
		"port":                  8080,
		"host":                  "0.0.0.0",
		"requireApiKey":         false,
		"defaultMaxConcurrency": -1, // legacy unlimited
		"defaultMaxRPM":         7,  // explicit value, must survive
		"accounts":              []map[string]interface{}{},
		"apiKeys": []map[string]interface{}{
			{"id": "k-unl", "key": "sk-unl", "enabled": true, "maxConcurrency": -1, "maxRPM": -1},
			{"id": "k-val", "key": "sk-val", "enabled": true, "maxConcurrency": 3, "maxRPM": 50},
		},
	}
	raw, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	// System-wide: -1 concurrency → 0 (unlimited); RPM 7 preserved.
	if got := GetDefaultMaxConcurrency(); got != 0 {
		t.Fatalf("migrated default concurrency = %d, want 0", got)
	}
	if got := GetDefaultMaxRPM(); got != 7 {
		t.Fatalf("migrated default RPM = %d, want 7", got)
	}

	// Per-key: legacy -1 → 0 (unlimited); explicit values untouched.
	unl := GetApiKeyEntry("k-unl")
	if unl == nil || unl.MaxConcurrency == nil || *unl.MaxConcurrency != 0 {
		t.Fatalf("k-unl concurrency = %v, want ptr(0)", unl.MaxConcurrency)
	}
	if unl.MaxRPM == nil || *unl.MaxRPM != 0 {
		t.Fatalf("k-unl RPM = %v, want ptr(0)", unl.MaxRPM)
	}
	val := GetApiKeyEntry("k-val")
	if val == nil || val.MaxConcurrency == nil || *val.MaxConcurrency != 3 {
		t.Fatalf("k-val concurrency = %v, want ptr(3)", val.MaxConcurrency)
	}
	if val.MaxRPM == nil || *val.MaxRPM != 50 {
		t.Fatalf("k-val RPM = %v, want ptr(50)", val.MaxRPM)
	}
}
