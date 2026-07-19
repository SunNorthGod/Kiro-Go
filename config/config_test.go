package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

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
