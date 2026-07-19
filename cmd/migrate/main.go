// Command migrate imports the OLD Rust proxy's persisted JSON data into the Go
// (NorthGod Kiro-Go) system.
//
// It reads the Rust store files from -rust-dir:
//
//	credentials.json      -> accounts        (rustCredential, see rust.go)
//	api_keys.json         -> API keys / 卡密 (rustAPIKey)
//	api_key_usage.json    -> consumption      (rustUsageRecord) — used to rebuild CreditsUsed
//	api_key_recharge.json -> top-ups          (rustRechargeRecord) — used to rebuild CreditsGranted
//
// and writes a Go config.json (default ./config.migrated.json). Deployment then:
//
//	1) migrate -rust-dir <old> -out config.json -dry-run=false
//	2) start the server with DATABASE_URL set → its built-in one-time JSON→PG seed
//	   (config.EnableDatabaseFromEnv) loads everything into Postgres. We deliberately
//	   do NOT write to Postgres here — reusing the server's tested seeding path is
//	   safer than re-implementing inserts.
//
// Mapping notes / assumptions (verify against real data — this was written against
// the Rust source models, not a live dataset):
//   - IDs: Rust numeric ids (u32 keys / u64 credentials) are remapped to stable Go
//     UUID strings; boundCredentialIds and parentKeyId are rewritten through the
//     same map so references stay intact.
//   - PRIORITY IS INVERTED: Rust `priority` is "smaller == higher priority"; the Go
//     scheduler uses Weight where "larger == higher priority". We invert so the
//     relative ordering is preserved (smallest Rust priority → largest Go Weight).
//   - Unified ledger: CreditsGranted = Σ recharge.addCredits; CreditsUsed =
//     Σ usage.creditsUsed (falls back to estimatedCost). balance = granted - used,
//     matching the Rust balance. Legacy creditLimit is carried over too.
//   - Timestamps: RFC3339 strings and unix numbers are both accepted.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"kiro-go/config"

	"github.com/google/uuid"
)

// migrationReport collects human-readable notes + counts and prints a summary.
type migrationReport struct {
	infos []string
	warns []string

	accountsIn, accountsOut int
	keysIn, keysOut         int
	usageRows, rechargeRows int
	skipped                 int
}

func (r *migrationReport) infof(format string, a ...interface{}) {
	r.infos = append(r.infos, fmt.Sprintf(format, a...))
}
func (r *migrationReport) warnf(format string, a ...interface{}) {
	r.warns = append(r.warns, fmt.Sprintf(format, a...))
}
func (r *migrationReport) print() {
	fmt.Println("---- migration report ----")
	for _, s := range r.infos {
		fmt.Println("  info: " + s)
	}
	for _, s := range r.warns {
		fmt.Println("  WARN: " + s)
	}
	fmt.Printf("accounts: %d read -> %d mapped | keys: %d read -> %d mapped | usage rows: %d | recharge rows: %d | skipped: %d\n",
		r.accountsIn, r.accountsOut, r.keysIn, r.keysOut, r.usageRows, r.rechargeRows, r.skipped)
}

// idMapper hands out a stable Go UUID for each original Rust numeric id.
type idMapper struct{ m map[uint64]string }

func newIDMapper() *idMapper { return &idMapper{m: map[uint64]string{}} }
func (im *idMapper) get(id uint64) string {
	if s, ok := im.m[id]; ok {
		return s
	}
	s := uuid.NewString()
	im.m[id] = s
	return s
}
func (im *idMapper) lookup(id uint64) (string, bool) { s, ok := im.m[id]; return s, ok }

// parseRustTime accepts an RFC3339 string or a unix-seconds number (json.RawMessage)
// and returns unix seconds. Empty/null/unparseable → (0, false).
func parseRustTime(raw json.RawMessage) (int64, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, false
	}
	// Numeric (unix seconds, possibly fractional).
	if s[0] != '"' {
		var f float64
		if err := json.Unmarshal(raw, &f); err == nil {
			return int64(f), true
		}
		return 0, false
	}
	var str string
	if err := json.Unmarshal(raw, &str); err != nil {
		return 0, false
	}
	str = strings.TrimSpace(str)
	if str == "" {
		return 0, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, str); err == nil {
			return t.Unix(), true
		}
	}
	return 0, false
}

func strval(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}
func f64val(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func main() {
	rustDir := flag.String("rust-dir", "", "directory holding the Rust proxy JSON files (required)")
	out := flag.String("out", "config.migrated.json", "output Go config.json path")
	dryRun := flag.Bool("dry-run", true, "report only; do not write the output file")
	password := flag.String("password", "changeme", "admin password to seed into the output config")
	port := flag.Int("port", 8080, "server port for the output config")
	host := flag.String("host", "0.0.0.0", "server bind host for the output config")
	flag.Parse()

	if strings.TrimSpace(*rustDir) == "" {
		fmt.Fprintln(os.Stderr, "error: -rust-dir is required")
		flag.Usage()
		os.Exit(2)
	}

	rep := &migrationReport{}
	join := func(name string) string { return *rustDir + string(os.PathSeparator) + name }

	creds := loadRustCredentials(join("credentials.json"), rep)
	keys := loadRustAPIKeys(join("api_keys.json"), rep)
	usage := loadRustUsage(join("api_key_usage.json"), rep)
	recharge := loadRustRecharge(join("api_key_recharge.json"), rep)
	rep.accountsIn, rep.keysIn = len(creds), len(keys)
	rep.usageRows, rep.rechargeRows = len(usage), len(recharge)

	credIDs := newIDMapper()
	keyIDs := newIDMapper()

	accounts := mapAccounts(creds, credIDs, rep)
	apiKeys := mapAPIKeys(keys, usage, recharge, keyIDs, credIDs, rep)
	rep.accountsOut, rep.keysOut = len(accounts), len(apiKeys)

	cfg := config.Config{
		Password:      *password,
		Port:          *port,
		Host:          *host,
		RequireApiKey: true,
		Accounts:      accounts,
		ApiKeys:       apiKeys,
	}

	rep.print()

	if *dryRun {
		fmt.Println("\n[dry-run] no file written. Re-run with -dry-run=false to write:", *out)
		return
	}
	data, err := json.MarshalIndent(&cfg, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: marshal config:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, data, 0600); err != nil {
		fmt.Fprintln(os.Stderr, "error: write config:", err)
		os.Exit(1)
	}
	fmt.Println("\nwrote", *out, "-", len(accounts), "accounts,", len(apiKeys), "keys.")
	fmt.Println("Next: start the server with this config.json; set DATABASE_URL to auto-seed Postgres on first boot.")
}

// mapAccounts converts Rust credentials to Go accounts, inverting priority→Weight.
func mapAccounts(creds []rustCredential, credIDs *idMapper, rep *migrationReport) []config.Account {
	// Determine priority range for inversion (Rust: smaller == higher priority).
	maxPrio := uint32(0)
	allZero := true
	for _, c := range creds {
		if c.Priority > maxPrio {
			maxPrio = c.Priority
		}
		if c.Priority != 0 {
			allZero = false
		}
	}
	if !allZero {
		rep.infof("priority inverted: Rust 'smaller==higher' → Go Weight 'larger==higher' (max rust priority %d)", maxPrio)
	}

	out := make([]config.Account, 0, len(creds))
	for i, c := range creds {
		if c.ID == nil {
			rep.warnf("credential #%d has no id; skipped", i)
			rep.skipped++
			continue
		}
		weight := 1
		if !allZero {
			weight = int(maxPrio-c.Priority) + 1 // smallest rust priority → largest weight
		}
		region := strval(c.Region)
		if region == "" {
			region = strval(c.APIRegion)
		}
		if region == "" {
			region = "us-east-1"
		}
		acc := config.Account{
			ID:                credIDs.get(*c.ID),
			Email:             strval(c.Email),
			Nickname:          strval(c.Nickname),
			AccessToken:       strval(c.AccessToken),
			RefreshToken:      strval(c.RefreshToken),
			ClientID:          strval(c.ClientID),
			ClientSecret:      strval(c.ClientSecret),
			AuthMethod:        strval(c.AuthMethod),
			Region:            region,
			ProfileArn:        strval(c.ProfileArn),
			MachineId:         strval(c.MachineID),
			ProxyURL:          strval(c.ProxyURL),
			KiroApiKey:        strval(c.KiroAPIKey),
			TokenEndpoint:     strval(c.TokenEndpoint),
			IssuerUrl:         strval(c.IssuerURL),
			Scopes:            strval(c.Scopes),
			SubscriptionTitle: strval(c.SubscriptionTitle),
			Weight:            weight,
			Enabled:           !c.Disabled,
			CreatedAt:         time.Now().Unix() - int64(len(creds)-i), // preserve slice order; no createdAt in Rust model
		}
		if c.ExpiresAt != nil {
			if ts, ok := parseRustTime(json.RawMessage(`"` + *c.ExpiresAt + `"`)); ok {
				acc.ExpiresAt = ts
			}
		}
		out = append(out, acc)
	}
	return out
}

// mapAPIKeys converts Rust API keys to Go entries and rebuilds the unified ledger
// (granted from recharge, used from usage). apiKeyId 0 is the master key (no card).
func mapAPIKeys(keys []rustAPIKey, usage []rustUsageRecord, recharge []rustRechargeRecord,
	keyIDs, credIDs *idMapper, rep *migrationReport) []config.ApiKeyEntry {

	// Pre-register key ids so parentKeyId references resolve regardless of order.
	for _, k := range keys {
		keyIDs.get(uint64(k.ID))
	}

	grantedBy := map[uint32]float64{}
	for _, r := range recharge {
		grantedBy[r.APIKeyID] += f64val(r.AddCredits)
	}
	type used struct {
		credits float64
		tokens  int64
		reqs    int64
	}
	usedBy := map[uint32]*used{}
	for _, u := range usage {
		if u.APIKeyID == 0 {
			continue // master traffic, not a card key
		}
		e := usedBy[u.APIKeyID]
		if e == nil {
			e = &used{}
			usedBy[u.APIKeyID] = e
		}
		if u.CreditsUsed != nil {
			e.credits += *u.CreditsUsed
		} else {
			e.credits += u.EstimatedCost
		}
		e.tokens += int64(u.InputTokens) + int64(u.OutputTokens)
		e.reqs++
	}

	out := make([]config.ApiKeyEntry, 0, len(keys))
	for _, k := range keys {
		if strings.TrimSpace(k.Key) == "" {
			rep.warnf("api key id %d has empty key value; skipped", k.ID)
			rep.skipped++
			continue
		}
		id, _ := keyIDs.lookup(uint64(k.ID))
		entry := config.ApiKeyEntry{
			ID:       id,
			Name:     k.Name,
			Key:      k.Key,
			Enabled:  k.enabledOrDefault(),
			Migrated: true,
		}
		if ts, ok := parseRustTime(k.CreatedAt); ok {
			entry.CreatedAt = ts
		} else {
			entry.CreatedAt = time.Now().Unix()
		}
		if ts, ok := parseRustTime(k.ExpiresAt); ok {
			entry.ExpiresAt = ts
		}
		// Legacy fixed limit (prefer creditLimit, fall back to deprecated spendingLimit).
		if k.CreditLimit != nil {
			entry.CreditLimit = *k.CreditLimit
		} else if k.SpendingLimit != nil {
			entry.CreditLimit = *k.SpendingLimit
		}
		// Granted = the key's total allotment. In Rust that is the running creditLimit
		// (which equals the sum of recharge addCredits for recharge-created keys, but is
		// also set directly on keys whose allotment predates recharge tracking). Taking
		// max(recharge sum, creditLimit) avoids a spurious zero grant — which would
		// otherwise render as a negative balance (0 - used) for such keys.
		entry.CreditsGranted = grantedBy[k.ID]
		if entry.CreditLimit > entry.CreditsGranted {
			entry.CreditsGranted = entry.CreditLimit
		}
		if u := usedBy[k.ID]; u != nil {
			entry.CreditsUsed = u.credits
			entry.TokensUsed = u.tokens
			entry.RequestsCount = u.reqs
		}
		// Rewrite bound-credential references through the credential id map.
		for _, bc := range k.BoundCredentialIDs {
			if mapped, ok := credIDs.lookup(bc); ok {
				entry.BoundAccountIDs = append(entry.BoundAccountIDs, mapped)
			} else {
				rep.warnf("api key %q binds unknown credential id %d (dropped)", k.Name, bc)
			}
		}
		// Parent link (sub-card lineage).
		if k.ParentKeyID != nil {
			if mapped, ok := keyIDs.lookup(uint64(*k.ParentKeyID)); ok {
				entry.ParentKeyID = mapped
			} else {
				rep.warnf("api key %q references unknown parent id %d (dropped)", k.Name, *k.ParentKeyID)
			}
		}
		out = append(out, entry)
	}

	// Stable output order: parents before children, then by name.
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].ParentKeyID == "") != (out[j].ParentKeyID == "") {
			return out[i].ParentKeyID == ""
		}
		return out[i].Name < out[j].Name
	})
	return out
}
