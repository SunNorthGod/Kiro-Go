package main

// rust.go — data shapes of the OLD Rust proxy's persisted JSON files.
//
// These structs mirror (field-for-field, via the same camelCase JSON names) the
// serde models in the Rust source tree:
//
//   proxy/src/kiro/model/credentials.rs  -> credentials.json   (rustCredential)
//   proxy/src/model/api_key.rs           -> api_keys.json       (rustAPIKey)
//   proxy/src/model/usage.rs             -> api_key_usage.json  (rustUsageRecord)
//   proxy/src/model/recharge.rs          -> api_key_recharge.json (rustRechargeRecord)
//
// Everything the Rust side wrote with `#[serde(skip_serializing_if = "Option::is_none")]`
// is modelled here as a pointer so we can tell "absent" from "zero value". IDs are
// numeric on the Rust side (u32 for API keys, u64 for credentials); the mapping
// layer (mapping.go) remaps them to Go UUID strings.

import (
	"encoding/json"
	"os"
)

// rustCredential is one account entry in credentials.json.
//
// The file itself is either a single object (legacy) or an array of objects (see
// CredentialsConfig in credentials.rs); loadRustCredentials handles both.
type rustCredential struct {
	ID                *uint64 `json:"id"`
	AccessToken       *string `json:"accessToken"`
	RefreshToken      *string `json:"refreshToken"`
	ProfileArn        *string `json:"profileArn"`
	ExpiresAt         *string `json:"expiresAt"` // RFC3339 string on the Rust side
	AuthMethod        *string `json:"authMethod"`
	ClientID          *string `json:"clientId"`
	ClientSecret      *string `json:"clientSecret"`
	TokenEndpoint     *string `json:"tokenEndpoint"`
	IssuerURL         *string `json:"issuerUrl"`
	Scopes            *string `json:"scopes"`
	Priority          uint32  `json:"priority"` // smaller == higher priority (default 0)
	Region            *string `json:"region"`
	AuthRegion        *string `json:"authRegion"`
	APIRegion         *string `json:"apiRegion"`
	MachineID         *string `json:"machineId"`
	KiroAPIKey        *string `json:"kiroApiKey"`
	Email             *string `json:"email"`
	Nickname          *string `json:"nickname"`
	SubscriptionTitle *string `json:"subscriptionTitle"`
	ProxyURL          *string `json:"proxyUrl"`
	ProxyUsername     *string `json:"proxyUsername"`
	ProxyPassword     *string `json:"proxyPassword"`
	Disabled          bool    `json:"disabled"` // default false
}

// rustAPIKey is one entry in api_keys.json (the "卡密" store).
//
// createdAt / expiresAt / activatedAt are chrono DateTime<Utc>, serialized by
// serde as RFC3339 strings; they are kept as json.RawMessage so the timestamp
// parser can also accept unix seconds if a hand-edited file uses numbers.
type rustAPIKey struct {
	ID                 uint32          `json:"id"`
	Key                string          `json:"key"`
	Name               string          `json:"name"`
	Enabled            *bool           `json:"enabled"` // Rust default is true when absent
	CreatedAt          json.RawMessage `json:"createdAt"`
	ExpiresAt          json.RawMessage `json:"expiresAt"`
	SpendingLimit      *float64        `json:"spendingLimit"` // deprecated in Rust (migrated to creditLimit)
	CreditLimit        *float64        `json:"creditLimit"`
	DurationDays       *float64        `json:"durationDays"`
	ActivatedAt        json.RawMessage `json:"activatedAt"`
	BoundCredentialIDs []uint64        `json:"boundCredentialIds"`
	ParentKeyID        *uint32         `json:"parentKeyId"`
	CommittedCredits   float64         `json:"committedCredits"` // shared-pool settlement (no direct Go field)
}

// enabledOrDefault applies Rust's `#[serde(default = "default_enabled")]` (true).
func (k rustAPIKey) enabledOrDefault() bool {
	if k.Enabled == nil {
		return true
	}
	return *k.Enabled
}

// rustUsageRecord is one row of api_key_usage.json. Only the fields relevant to
// balance reconstruction and per-(key,credential,model) attribution are modelled;
// cache/relay breakdown fields are read where useful and otherwise ignored.
type rustUsageRecord struct {
	APIKeyID                 uint32          `json:"apiKeyId"` // 0 == master key (no card-key row)
	CredentialID             *uint64         `json:"credentialId"`
	Model                    string          `json:"model"`
	InputTokens              int32           `json:"inputTokens"`
	OutputTokens             int32           `json:"outputTokens"`
	EstimatedCost            float64         `json:"estimatedCost"`
	CreditsUsed              *float64        `json:"creditsUsed"`
	CacheReadInputTokens     *int32          `json:"cacheReadInputTokens"`
	CacheCreationInputTokens *int32          `json:"cacheCreationInputTokens"`
	CreatedAt                json.RawMessage `json:"createdAt"`
	ClientIP                 *string         `json:"clientIp"`
	Relay                    *string         `json:"relay"`
}

// rustRechargeRecord is one row of api_key_recharge.json (the "进账" ledger).
type rustRechargeRecord struct {
	APIKeyID         uint32          `json:"apiKeyId"`
	Kind             string          `json:"kind"` // "create" | "topup"
	AddCredits       *float64        `json:"addCredits"`
	AddDays          *float64        `json:"addDays"`
	CreditLimitAfter *float64        `json:"creditLimitAfter"`
	ExpiresAtAfter   json.RawMessage `json:"expiresAtAfter"`
	Source           string          `json:"source"` // "admin" | "reseller"
	Note             *string         `json:"note"`
	CreatedAt        json.RawMessage `json:"createdAt"`
}

// ---- loaders ------------------------------------------------------------------

// firstNonSpaceByte returns the first non-whitespace byte, or 0 for blank input.
// Used to detect whether credentials.json is an array ('[') or a single object ('{').
func firstNonSpaceByte(b []byte) byte {
	for _, c := range b {
		switch c {
		case ' ', '\n', '\t', '\r':
			continue
		default:
			return c
		}
	}
	return 0
}

// readMaybe reads a file, treating "does not exist" and "empty" as a benign empty
// input (returns nil, false) with a note — matching the Rust loaders, which all
// return an empty collection for a missing/blank file. Hard read errors are
// reported and also yield nil.
func readMaybe(path, kind string, rep *migrationReport) ([]byte, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			rep.infof("%s: file not found at %s (treated as empty)", kind, path)
			return nil, false
		}
		rep.warnf("%s: failed to read %s: %v (treated as empty)", kind, path, err)
		return nil, false
	}
	if firstNonSpaceByte(data) == 0 {
		rep.infof("%s: file %s is empty (treated as empty)", kind, path)
		return nil, false
	}
	return data, true
}

// decodeJSONArrayElements decodes a JSON array element-by-element so a single
// malformed entry is skipped-and-reported instead of failing the whole file.
func decodeJSONArrayElements[T any](data []byte, kind string, rep *migrationReport) []T {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		rep.warnf("%s: not a JSON array (%v); nothing imported from this file", kind, err)
		return nil
	}
	out := make([]T, 0, len(raws))
	for i, raw := range raws {
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			rep.warnf("%s: skipping malformed entry #%d: %v", kind, i, err)
			continue
		}
		out = append(out, v)
	}
	return out
}

// loadRustCredentials loads credentials.json, transparently accepting either the
// single-object legacy format or the multi-account array format.
func loadRustCredentials(path string, rep *migrationReport) []rustCredential {
	data, ok := readMaybe(path, "credentials", rep)
	if !ok {
		return nil
	}
	switch firstNonSpaceByte(data) {
	case '[':
		return decodeJSONArrayElements[rustCredential](data, "credentials", rep)
	case '{':
		var single rustCredential
		if err := json.Unmarshal(data, &single); err != nil {
			rep.warnf("credentials: failed to parse single-object form: %v", err)
			return nil
		}
		return []rustCredential{single}
	default:
		rep.warnf("credentials: unexpected JSON shape; expected object or array")
		return nil
	}
}

// loadRustAPIKeys loads api_keys.json (always an array in the Rust source).
func loadRustAPIKeys(path string, rep *migrationReport) []rustAPIKey {
	data, ok := readMaybe(path, "api_keys", rep)
	if !ok {
		return nil
	}
	return decodeJSONArrayElements[rustAPIKey](data, "api_keys", rep)
}

// loadRustUsage loads api_key_usage.json (array of usage records).
func loadRustUsage(path string, rep *migrationReport) []rustUsageRecord {
	data, ok := readMaybe(path, "api_key_usage", rep)
	if !ok {
		return nil
	}
	return decodeJSONArrayElements[rustUsageRecord](data, "api_key_usage", rep)
}

// loadRustRecharge loads api_key_recharge.json (array of recharge records).
func loadRustRecharge(path string, rep *migrationReport) []rustRechargeRecord {
	data, ok := readMaybe(path, "api_key_recharge", rep)
	if !ok {
		return nil
	}
	return decodeJSONArrayElements[rustRechargeRecord](data, "api_key_recharge", rep)
}
