// PostgreSQL storage backend for accounts, API keys ("卡密") and usage/billing.
//
// Design: hybrid persistence controlled by the DATABASE_URL environment variable.
//   - When DATABASE_URL is unset, everything stays in the JSON config file exactly
//     as before (zero behavior change).
//   - When DATABASE_URL is set, PostgreSQL becomes the source of truth for
//     accounts, API keys and the usage/billing ledger. The in-memory cfg slice is
//     a read cache, loaded from the DB at startup and kept in sync via targeted
//     write-through on each mutation. Server settings, thinking/filter config and
//     global stats continue to live in the JSON file (they have no DB table).
//
// Billing correctness: per-request usage goes through db.RecordUsage (the
// authoritative, monotonic usage_counters ledger) plus db.TouchAPIKeyUsage (the
// per-key mirror). Neither ever scans or trims detail rows, so the credit total
// can never roll back — fixing the root cause of the reported usage regressions.
package config

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"kiro-go/db"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	dbEnabled bool
	dbPool    *pgxpool.Pool
	dbCtx     = context.Background()
)

// dbOpTimeout bounds a single request-path DB operation so a stuck connection
// cannot wedge a request goroutine indefinitely. Startup (connect/migrate/seed)
// deliberately keeps using the unbounded dbCtx, because one-time migration and
// JSON->PG seeding can legitimately run longer than this budget.
const dbOpTimeout = 10 * time.Second

// dbOpCtx returns a context bounded by dbOpTimeout for one DB operation. Callers
// must defer the returned cancel func.
func dbOpCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), dbOpTimeout)
}

// UsingDB reports whether the PostgreSQL backend is active.
func UsingDB() bool { return dbEnabled }

// DatabasePool returns the active pgx pool, or nil when the DB backend is off.
// Exposed so admin/usage views can read the authoritative usage_counters ledger
// (db.GetTotalCredits / db.GetUsageSummary) directly.
func DatabasePool() *pgxpool.Pool { return dbPool }

// EnableDatabaseFromEnv connects to PostgreSQL when DATABASE_URL is set, runs the
// idempotent schema migration, then reconciles the JSON-loaded config with the DB:
//
//   - Empty DB + existing JSON accounts/keys => one-time JSON -> PG seed.
//   - The in-memory cfg.Accounts / cfg.ApiKeys are then replaced with the DB
//     contents, making Postgres the source of truth from that point on.
//
// No-op (returns false, nil) when DATABASE_URL is empty. Call once at startup,
// after config.Init.
func EnableDatabaseFromEnv() (bool, error) {
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		return false, nil
	}

	pool, err := db.InitPool(dbCtx, dsn)
	if err != nil {
		return false, fmt.Errorf("connect postgres: %w", err)
	}
	if err := db.Migrate(dbCtx, pool); err != nil {
		pool.Close()
		return false, fmt.Errorf("migrate postgres: %w", err)
	}

	cfgLock.Lock()
	defer cfgLock.Unlock()

	dbAccounts, err := db.ListAccounts(dbCtx, pool)
	if err != nil {
		pool.Close()
		return false, fmt.Errorf("load accounts: %w", err)
	}
	dbKeys, err := db.ListAPIKeys(dbCtx, pool)
	if err != nil {
		pool.Close()
		return false, fmt.Errorf("load api keys: %w", err)
	}

	// One-time migration: empty DB + existing JSON data -> seed the DB.
	if len(dbAccounts) == 0 && len(dbKeys) == 0 && (len(cfg.Accounts) > 0 || len(cfg.ApiKeys) > 0) {
		if err := seedDatabaseFromConfigLocked(pool); err != nil {
			pool.Close()
			return false, fmt.Errorf("seed postgres from json: %w", err)
		}
		dbAccounts, _ = db.ListAccounts(dbCtx, pool)
		dbKeys, _ = db.ListAPIKeys(dbCtx, pool)
	}

	// Postgres is now authoritative: replace the in-memory cache.
	accounts := make([]Account, 0, len(dbAccounts))
	for _, a := range dbAccounts {
		accounts = append(accounts, accountFromDB(a))
	}
	keys := make([]ApiKeyEntry, 0, len(dbKeys))
	for _, k := range dbKeys {
		keys = append(keys, apiKeyFromDB(k))
	}
	cfg.Accounts = accounts
	cfg.ApiKeys = keys

	dbPool = pool
	dbEnabled = true
	return true, nil
}

// seedDatabaseFromConfigLocked pushes the current in-memory accounts + API keys
// into an empty database. Caller holds cfgLock. Accounts lacking a CreatedAt get
// strictly-increasing timestamps in slice order so the pool order is preserved
// (ListAccounts orders by created_at).
func seedDatabaseFromConfigLocked(pool *pgxpool.Pool) error {
	base := time.Now().Unix() - int64(len(cfg.Accounts))
	for i := range cfg.Accounts {
		if cfg.Accounts[i].CreatedAt == 0 {
			cfg.Accounts[i].CreatedAt = base + int64(i)
		}
		if err := db.UpsertAccount(dbCtx, pool, accountToDB(cfg.Accounts[i])); err != nil {
			return err
		}
	}
	now := time.Now().Unix()
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].CreatedAt == 0 {
			cfg.ApiKeys[i].CreatedAt = now
		}
		if err := db.UpsertAPIKey(dbCtx, pool, apiKeyToDB(cfg.ApiKeys[i])); err != nil {
			return err
		}
	}
	return nil
}

// ---- write-through helpers (caller holds cfgLock) ----
//
// These do the targeted DB write for a single entity. They are only invoked when
// dbEnabled is true (guarded by persist* wrappers), so they assume dbPool != nil.

func dbUpsertAccount(a Account) error {
	ctx, cancel := dbOpCtx()
	defer cancel()
	return db.UpsertAccount(ctx, dbPool, accountToDB(a))
}

func dbDeleteAccount(id string) error {
	ctx, cancel := dbOpCtx()
	defer cancel()
	return db.DeleteAccount(ctx, dbPool, id)
}

func dbUpsertApiKey(e ApiKeyEntry) error {
	ctx, cancel := dbOpCtx()
	defer cancel()
	return db.UpsertAPIKey(ctx, dbPool, apiKeyToDB(e))
}

func dbDeleteApiKey(id string) error {
	ctx, cancel := dbOpCtx()
	defer cancel()
	return db.DeleteAPIKey(ctx, dbPool, id)
}

// dbDeleteApiKeyWithSettlement deletes a sub-card and folds its consumed credits
// into the parent's credits_used mirror in one transaction. Caller holds cfgLock.
func dbDeleteApiKeyWithSettlement(childID, parentID string, settleCredits float64) error {
	ctx, cancel := dbOpCtx()
	defer cancel()
	return db.DeleteAPIKeyWithSettlement(ctx, dbPool, childID, parentID, settleCredits)
}

// dbInsertApiKeyWithOpeningGrant creates a sub-card row (grant included) plus its
// opening recharge audit row in one transaction. Caller holds cfgLock.
func dbInsertApiKeyWithOpeningGrant(e ApiKeyEntry, operator, note string) error {
	ctx, cancel := dbOpCtx()
	defer cancel()
	return db.InsertAPIKeyWithOpeningGrant(ctx, dbPool, apiKeyToDB(e), operator, note)
}

// dbRecordApiKeyUsage folds one request into the authoritative ledger + optional
// detail log (db.RecordUsageWithDetail writes usage_counters AND usage_records in
// one transaction) and then bumps the per-key quota mirror (db.TouchAPIKeyUsage).
// Model attribution is carried through now; credential attribution stays coarse at
// this layer (the config API doesn't know which account served the request).
// Negative deltas are clamped so both ledgers stay monotonic (matching the
// in-memory mirror, which only adds positive deltas).
func dbRecordApiKeyUsage(id, model string, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int64, credits float64, lastUsedAt int64) error {
	if inputTokens < 0 {
		inputTokens = 0
	}
	if outputTokens < 0 {
		outputTokens = 0
	}
	if cacheReadTokens < 0 {
		cacheReadTokens = 0
	}
	if cacheCreationTokens < 0 {
		cacheCreationTokens = 0
	}
	if credits < 0 {
		credits = 0
	}
	ctx, cancel := dbOpCtx()
	defer cancel()
	// Counter + detail log + per-key mirror advance together in ONE transaction
	// (RecordUsageAndTouch), so the authoritative ledger and the api_keys mirror
	// the quota check reads can never diverge from a mid-way failure. Cache tokens
	// ride along into the detail log only; billing stays on input/output/credits.
	return db.RecordUsageAndTouch(ctx, dbPool, db.Usage{
		APIKeyID:                 id,
		Model:                    model,
		Requests:                 1,
		InputTokens:              inputTokens,
		OutputTokens:             outputTokens,
		Credits:                  credits,
		CacheReadInputTokens:     cacheReadTokens,
		CacheCreationInputTokens: cacheCreationTokens,
	}, lastUsedAt)
}

// dbUpdateAccountStats persists only the runtime-stats columns (targeted UPDATE),
// invoked OUTSIDE cfgLock by the stats worker. Disjoint from token columns.
func dbUpdateAccountStats(id string, requestCount, errorCount, totalTokens int, totalCredits float64, lastUsed int64) error {
	ctx, cancel := dbOpCtx()
	defer cancel()
	return db.UpdateAccountStats(ctx, dbPool, id, requestCount, errorCount, int64(totalTokens), totalCredits, lastUsed)
}

// dbUpdateAccountToken persists only the token/expiry columns (targeted UPDATE),
// invoked OUTSIDE cfgLock. Disjoint from the stats columns.
func dbUpdateAccountToken(id, accessToken, refreshToken string, expiresAt int64) error {
	ctx, cancel := dbOpCtx()
	defer cancel()
	return db.UpdateAccountToken(ctx, dbPool, id, accessToken, refreshToken, expiresAt)
}

// dbRechargeApiKey folds a top-up into the unified ledger via db.RechargeAPIKey,
// which atomically bumps api_keys.credits_granted and appends a recharge_records
// row. It returns the new running grant total (balance_after) so the caller can
// refresh the in-memory mirror. Caller holds cfgLock.
func dbRechargeApiKey(id string, amount float64, operator, note string) (float64, error) {
	ctx, cancel := dbOpCtx()
	defer cancel()
	rec, err := db.RechargeAPIKey(ctx, dbPool, id, amount, operator, note)
	if err != nil {
		return 0, err
	}
	return rec.BalanceAfter, nil
}

// dbGetApiKeyBalance reads the key's unified-ledger balance straight from the
// api_keys row mirrors (granted/used) via db.GetKeyBalance. An unknown key surfaces
// as an error. Caller serializes with writers via cfgLock.
func dbGetApiKeyBalance(id string) (granted, used, balance float64, err error) {
	ctx, cancel := dbOpCtx()
	defer cancel()
	return db.GetKeyBalance(ctx, dbPool, id)
}

func dbResetApiKeyUsage(id string) error {
	ctx, cancel := dbOpCtx()
	defer cancel()
	if err := db.ResetUsageCounters(ctx, dbPool, id); err != nil {
		return err
	}
	return db.ResetAPIKeyUsage(ctx, dbPool, id)
}

// persistAccountLocked persists an account mutation to Postgres (when enabled) or
// the JSON file (otherwise). Caller holds cfgLock.
func persistAccountLocked(a Account) error {
	if dbEnabled {
		return dbUpsertAccount(a)
	}
	return Save()
}

func persistAccountDeleteLocked(id string) error {
	if dbEnabled {
		return dbDeleteAccount(id)
	}
	return Save()
}

func persistApiKeyLocked(e ApiKeyEntry) error {
	if dbEnabled {
		return dbUpsertApiKey(e)
	}
	return saveLocked()
}

func persistApiKeyDeleteLocked(id string) error {
	if dbEnabled {
		return dbDeleteApiKey(id)
	}
	return saveLocked()
}

// ---- DTO converters (config.Account <-> db.Account, config.ApiKeyEntry <-> db.APIKey) ----
//
// Note the Enabled/Disabled inversion: config uses Enabled (true == active) while
// the DB stores Disabled (true == inactive).

func accountToDB(a Account) db.Account {
	return db.Account{
		ID: a.ID, Email: a.Email, UserID: a.UserId, Nickname: a.Nickname,
		AccessToken: a.AccessToken, RefreshToken: a.RefreshToken, ClientID: a.ClientID, ClientSecret: a.ClientSecret,
		AuthMethod: a.AuthMethod, Provider: a.Provider, Region: a.Region, StartURL: a.StartUrl,
		ExpiresAt: a.ExpiresAt, MachineID: a.MachineId, ProfileArn: a.ProfileArn, ProxyURL: a.ProxyURL, Weight: a.Weight,
		OverageStatus: a.OverageStatus, OverageCapability: a.OverageCapability, OverageCap: a.OverageCap,
		OverageRate: a.OverageRate, CurrentOverages: a.CurrentOverages, OverageCheckedAt: a.OverageCheckedAt,
		Disabled: !a.Enabled, BanStatus: a.BanStatus, BanReason: a.BanReason, BanTime: a.BanTime,
		SubscriptionType: a.SubscriptionType, SubscriptionTitle: a.SubscriptionTitle, DaysRemaining: a.DaysRemaining,
		UsageCurrent: a.UsageCurrent, UsageLimit: a.UsageLimit, UsagePercent: a.UsagePercent,
		NextResetDate: a.NextResetDate, LastRefresh: a.LastRefresh,
		TrialUsageCurrent: a.TrialUsageCurrent, TrialUsageLimit: a.TrialUsageLimit, TrialUsagePercent: a.TrialUsagePercent,
		TrialStatus: a.TrialStatus, TrialExpiresAt: a.TrialExpiresAt,
		RequestCount: a.RequestCount, ErrorCount: a.ErrorCount, LastUsed: a.LastUsed,
		TotalTokens: int64(a.TotalTokens), TotalCredits: a.TotalCredits, CreatedAt: a.CreatedAt,
		KiroApiKey: a.KiroApiKey, TokenEndpoint: a.TokenEndpoint, IssuerUrl: a.IssuerUrl, Scopes: a.Scopes,
	}
}

func accountFromDB(a db.Account) Account {
	return Account{
		ID: a.ID, Email: a.Email, UserId: a.UserID, Nickname: a.Nickname,
		AccessToken: a.AccessToken, RefreshToken: a.RefreshToken, ClientID: a.ClientID, ClientSecret: a.ClientSecret,
		AuthMethod: a.AuthMethod, Provider: a.Provider, Region: a.Region, StartUrl: a.StartURL,
		ExpiresAt: a.ExpiresAt, MachineId: a.MachineID, ProfileArn: a.ProfileArn, ProxyURL: a.ProxyURL, Weight: a.Weight,
		OverageStatus: a.OverageStatus, OverageCapability: a.OverageCapability, OverageCap: a.OverageCap,
		OverageRate: a.OverageRate, CurrentOverages: a.CurrentOverages, OverageCheckedAt: a.OverageCheckedAt,
		Enabled: !a.Disabled, BanStatus: a.BanStatus, BanReason: a.BanReason, BanTime: a.BanTime,
		SubscriptionType: a.SubscriptionType, SubscriptionTitle: a.SubscriptionTitle, DaysRemaining: a.DaysRemaining,
		UsageCurrent: a.UsageCurrent, UsageLimit: a.UsageLimit, UsagePercent: a.UsagePercent,
		NextResetDate: a.NextResetDate, LastRefresh: a.LastRefresh,
		TrialUsageCurrent: a.TrialUsageCurrent, TrialUsageLimit: a.TrialUsageLimit, TrialUsagePercent: a.TrialUsagePercent,
		TrialStatus: a.TrialStatus, TrialExpiresAt: a.TrialExpiresAt,
		RequestCount: a.RequestCount, ErrorCount: a.ErrorCount, LastUsed: a.LastUsed,
		TotalTokens: int(a.TotalTokens), TotalCredits: a.TotalCredits, CreatedAt: a.CreatedAt,
		KiroApiKey: a.KiroApiKey, TokenEndpoint: a.TokenEndpoint, IssuerUrl: a.IssuerUrl, Scopes: a.Scopes,
	}
}

// apiKeyToDB / apiKeyFromDB round-trip every persisted field. The unified-ledger
// and card-key columns (CreditsGranted, ExpiresAt, BoundAccountIDs, ParentKeyID)
// MUST be mapped in both directions: omitting them from apiKeyToDB previously made
// every upsert overwrite the DB columns with zero values, silently wiping expiry,
// account bindings and lineage on any key mutation (a data-loss bug).
//
// MaxConcurrency has no db.APIKey column, so it is intentionally not mapped here;
// it survives only through the JSON backend.

func apiKeyToDB(e ApiKeyEntry) db.APIKey {
	return db.APIKey{
		ID: e.ID, Name: e.Name, Key: e.Key, Enabled: e.Enabled, Migrated: e.Migrated,
		TokenLimit: e.TokenLimit, CreditLimit: e.CreditLimit,
		TokensUsed: e.TokensUsed, CreditsUsed: e.CreditsUsed, RequestsCount: e.RequestsCount,
		CreatedAt: e.CreatedAt, LastUsedAt: e.LastUsedAt,
		CreditsGranted: e.CreditsGranted,
		ExpiresAt:      e.ExpiresAt, BoundAccountIDs: e.BoundAccountIDs, ParentKeyID: e.ParentKeyID,
	}
}

func apiKeyFromDB(k db.APIKey) ApiKeyEntry {
	return ApiKeyEntry{
		ID: k.ID, Name: k.Name, Key: k.Key, Enabled: k.Enabled, Migrated: k.Migrated,
		CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt,
		TokenLimit: k.TokenLimit, CreditLimit: k.CreditLimit,
		TokensUsed: k.TokensUsed, CreditsUsed: k.CreditsUsed, RequestsCount: k.RequestsCount,
		CreditsGranted: k.CreditsGranted,
		ExpiresAt:      k.ExpiresAt, BoundAccountIDs: k.BoundAccountIDs, ParentKeyID: k.ParentKeyID,
	}
}
