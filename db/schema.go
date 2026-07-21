package db

// schemaStatements holds every DDL statement Migrate runs, in dependency order.
// All use IF NOT EXISTS so migration is idempotent.
//
// Design notes:
//
//   - All identifiers (accounts.id, api_keys.id, usage_counters.api_key_id /
//     credential_id) are TEXT because the Go config layer identifies accounts and
//     API keys by UUID strings, not integers. Storing them as TEXT keeps a 1:1
//     mapping with config.Account.ID / config.ApiKeyEntry.ID and avoids a lossy
//     id remap during migration.
//
//   - Timestamps are stored as BIGINT Unix seconds to match every *At/*Time field
//     in config.Account / config.ApiKeyEntry (which are int64 Unix seconds). This
//     keeps the DTO<->row mapping trivial and avoids timezone ambiguity.
//
//   - usage_counters is the authoritative billing ledger (monotonic). Its primary
//     key (api_key_id, credential_id, model) is exactly the aggregation grain that
//     RecordUsage upserts into. credential_id defaults to '' (empty) rather than
//     NULL so it can safely participate in the primary key when the originating
//     account is unknown.
//
//   - usage_records is an optional detail log for display only; billing never reads
//     it. Its id is BIGSERIAL because rows are opaque and never addressed by the
//     app via a business key.
var schemaStatements = []string{
	// ---- accounts: Kiro credentials + cached subscription/usage state ----
	`CREATE TABLE IF NOT EXISTS accounts (
    id                  TEXT PRIMARY KEY,
    email               TEXT             NOT NULL DEFAULT '',
    user_id             TEXT             NOT NULL DEFAULT '',
    nickname            TEXT             NOT NULL DEFAULT '',
    access_token        TEXT             NOT NULL DEFAULT '',
    refresh_token       TEXT             NOT NULL DEFAULT '',
    client_id           TEXT             NOT NULL DEFAULT '',
    client_secret       TEXT             NOT NULL DEFAULT '',
    auth_method         TEXT             NOT NULL DEFAULT '',
    provider            TEXT             NOT NULL DEFAULT '',
    region              TEXT             NOT NULL DEFAULT '',
    start_url           TEXT             NOT NULL DEFAULT '',
    expires_at          BIGINT           NOT NULL DEFAULT 0,
    machine_id          TEXT             NOT NULL DEFAULT '',
    profile_arn         TEXT             NOT NULL DEFAULT '',
    proxy_url           TEXT             NOT NULL DEFAULT '',
    weight              INTEGER          NOT NULL DEFAULT 0,
    overage_status      TEXT             NOT NULL DEFAULT '',
    overage_capability  TEXT             NOT NULL DEFAULT '',
    overage_cap         DOUBLE PRECISION NOT NULL DEFAULT 0,
    overage_rate        DOUBLE PRECISION NOT NULL DEFAULT 0,
    current_overages    DOUBLE PRECISION NOT NULL DEFAULT 0,
    overage_checked_at  BIGINT           NOT NULL DEFAULT 0,
    disabled            BOOLEAN          NOT NULL DEFAULT FALSE,
    ban_status          TEXT             NOT NULL DEFAULT '',
    ban_reason          TEXT             NOT NULL DEFAULT '',
    ban_time            BIGINT           NOT NULL DEFAULT 0,
    subscription_type   TEXT             NOT NULL DEFAULT '',
    subscription_title  TEXT             NOT NULL DEFAULT '',
    days_remaining      INTEGER          NOT NULL DEFAULT 0,
    usage_current       DOUBLE PRECISION NOT NULL DEFAULT 0,
    usage_limit         DOUBLE PRECISION NOT NULL DEFAULT 0,
    usage_percent       DOUBLE PRECISION NOT NULL DEFAULT 0,
    next_reset_date     TEXT             NOT NULL DEFAULT '',
    last_refresh        BIGINT           NOT NULL DEFAULT 0,
    trial_usage_current DOUBLE PRECISION NOT NULL DEFAULT 0,
    trial_usage_limit   DOUBLE PRECISION NOT NULL DEFAULT 0,
    trial_usage_percent DOUBLE PRECISION NOT NULL DEFAULT 0,
    trial_status        TEXT             NOT NULL DEFAULT '',
    trial_expires_at    BIGINT           NOT NULL DEFAULT 0,
    request_count       INTEGER          NOT NULL DEFAULT 0,
    error_count         INTEGER          NOT NULL DEFAULT 0,
    last_used           BIGINT           NOT NULL DEFAULT 0,
    total_tokens        BIGINT           NOT NULL DEFAULT 0,
    total_credits       DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at          BIGINT           NOT NULL DEFAULT 0,
    kiro_api_key        TEXT             NOT NULL DEFAULT '',
    token_endpoint      TEXT             NOT NULL DEFAULT '',
    issuer_url          TEXT             NOT NULL DEFAULT '',
    scopes              TEXT             NOT NULL DEFAULT ''
)`,
	`CREATE INDEX IF NOT EXISTS idx_accounts_disabled ON accounts (disabled)`,
	// Idempotent column adds for deployments whose accounts table predates the
	// extended-login fields (external_idp / api_key). CREATE TABLE IF NOT EXISTS
	// never alters an existing table, so these ALTERs backfill the columns.
	`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS kiro_api_key   TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS token_endpoint TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS issuer_url     TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS scopes         TEXT NOT NULL DEFAULT ''`,

	// ---- api_keys: client-facing keys ("卡密") with independent quotas ----
	// "key" is quoted because KEY, while non-reserved in PostgreSQL, reads more
	// safely quoted and stays unambiguous across tools.
	`CREATE TABLE IF NOT EXISTS api_keys (
    id                TEXT PRIMARY KEY,
    name              TEXT             NOT NULL DEFAULT '',
    "key"             TEXT             NOT NULL UNIQUE,
    enabled           BOOLEAN          NOT NULL DEFAULT FALSE,
    migrated          BOOLEAN          NOT NULL DEFAULT FALSE,
    token_limit       BIGINT           NOT NULL DEFAULT 0,
    credit_limit      DOUBLE PRECISION NOT NULL DEFAULT 0,
    tokens_used       BIGINT           NOT NULL DEFAULT 0,
    credits_used      DOUBLE PRECISION NOT NULL DEFAULT 0,
    requests_count    BIGINT           NOT NULL DEFAULT 0,
    expires_at        BIGINT           NOT NULL DEFAULT 0,
    bound_account_ids JSONB            NOT NULL DEFAULT '[]'::jsonb,
    parent_key_id     TEXT             NOT NULL DEFAULT '',
    created_at        BIGINT           NOT NULL DEFAULT 0,
    last_used_at      BIGINT           NOT NULL DEFAULT 0,
    credits_granted   DOUBLE PRECISION NOT NULL DEFAULT 0,
    max_concurrency   INTEGER,
    max_rpm           INTEGER
)`,
	`CREATE INDEX IF NOT EXISTS idx_api_keys_parent ON api_keys (parent_key_id)`,
	// Idempotent backfill for deployments whose api_keys table predates the
	// recharge ledger. credits_granted is the "进账" side of the unified ledger:
	// a key's balance is credits_granted - credits_used.
	`ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS credits_granted DOUBLE PRECISION NOT NULL DEFAULT 0`,
	// Per-key limit overrides. New deployments create these as NULLable (NULL ==
	// inherit system default). Fresh backfill for tables that never had them.
	`ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS max_concurrency INTEGER`,
	`ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS max_rpm INTEGER`,
	// One-shot semantics migration (idempotent, guarded by is_nullable='NO').
	// The v1.1.12 columns were `INTEGER NOT NULL DEFAULT 0`, under the OLD rule
	// where 0 == "inherit default" and -1 == "unlimited". The unified rule flips
	// this: NULL == inherit, 0 == unlimited, N == value. Reinterpreting the raw
	// bytes blindly would silently turn every existing key unlimited, so remap
	// the stored values as we drop the NOT NULL/default:
	//   -1 → 0    (old unlimited      → new unlimited)
	//    0 → NULL (old inherit-default→ new inherit)
	//    N → N    (explicit value unchanged)
	// The whole block only runs while max_concurrency is still NOT NULL, so it
	// executes exactly once and is a no-op on every subsequent boot.
	`DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'api_keys' AND column_name = 'max_concurrency'
      AND is_nullable = 'NO'
  ) THEN
    ALTER TABLE api_keys ALTER COLUMN max_concurrency DROP DEFAULT;
    ALTER TABLE api_keys ALTER COLUMN max_concurrency DROP NOT NULL;
    UPDATE api_keys SET max_concurrency = CASE
      WHEN max_concurrency < 0 THEN 0
      WHEN max_concurrency = 0 THEN NULL
      ELSE max_concurrency END;
  END IF;
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'api_keys' AND column_name = 'max_rpm'
      AND is_nullable = 'NO'
  ) THEN
    ALTER TABLE api_keys ALTER COLUMN max_rpm DROP DEFAULT;
    ALTER TABLE api_keys ALTER COLUMN max_rpm DROP NOT NULL;
    UPDATE api_keys SET max_rpm = CASE
      WHEN max_rpm < 0 THEN 0
      WHEN max_rpm = 0 THEN NULL
      ELSE max_rpm END;
  END IF;
END $$;`,

	// ---- usage_counters: AUTHORITATIVE monotonic billing ledger ----
	// total_cache_read_tokens / total_cache_creation_tokens are DISPLAY-ONLY
	// monotonic counters (same nature as total_input/output_tokens): billing reads
	// total_credits only. They feed the "cache hit rate" surfaced in the panels /
	// web (hit rate = cache_read / total_input, since total_input already includes
	// cached prompt tokens).
	`CREATE TABLE IF NOT EXISTS usage_counters (
    api_key_id                  TEXT             NOT NULL,
    credential_id               TEXT             NOT NULL DEFAULT '',
    model                       TEXT             NOT NULL DEFAULT '',
    total_requests              BIGINT           NOT NULL DEFAULT 0,
    total_input_tokens          BIGINT           NOT NULL DEFAULT 0,
    total_output_tokens         BIGINT           NOT NULL DEFAULT 0,
    total_cache_read_tokens     BIGINT           NOT NULL DEFAULT 0,
    total_cache_creation_tokens BIGINT           NOT NULL DEFAULT 0,
    total_credits               DOUBLE PRECISION NOT NULL DEFAULT 0,
    updated_at                  BIGINT           NOT NULL DEFAULT 0,
    PRIMARY KEY (api_key_id, credential_id, model)
)`,
	`CREATE INDEX IF NOT EXISTS idx_usage_counters_api_key ON usage_counters (api_key_id)`,
	`CREATE INDEX IF NOT EXISTS idx_usage_counters_credential ON usage_counters (credential_id)`,
	// Idempotent backfill for deployments whose usage_counters predates the
	// display-only cache-token counters.
	`ALTER TABLE usage_counters ADD COLUMN IF NOT EXISTS total_cache_read_tokens     BIGINT NOT NULL DEFAULT 0`,
	`ALTER TABLE usage_counters ADD COLUMN IF NOT EXISTS total_cache_creation_tokens BIGINT NOT NULL DEFAULT 0`,

	// ---- usage_records: OPTIONAL detail log (display only, never billed) ----
	`CREATE TABLE IF NOT EXISTS usage_records (
    id                          BIGSERIAL PRIMARY KEY,
    api_key_id                  TEXT             NOT NULL DEFAULT '',
    credential_id               TEXT             NOT NULL DEFAULT '',
    model                       TEXT             NOT NULL DEFAULT '',
    input_tokens                BIGINT           NOT NULL DEFAULT 0,
    output_tokens               BIGINT           NOT NULL DEFAULT 0,
    cache_read_input_tokens     BIGINT           NOT NULL DEFAULT 0,
    cache_creation_input_tokens BIGINT           NOT NULL DEFAULT 0,
    credits                     DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at                  BIGINT           NOT NULL DEFAULT 0,
    client_ip                   TEXT             NOT NULL DEFAULT ''
)`,
	`CREATE INDEX IF NOT EXISTS idx_usage_records_api_key ON usage_records (api_key_id, created_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_usage_records_credential ON usage_records (credential_id, created_at DESC)`,
	// Pure created_at index: the overview's recent-window aggregates
	// (GetRecentCacheStats / GetDailyUsage) filter by created_at alone, which
	// the composite indexes above (leading on api_key_id / credential_id)
	// cannot serve — without this, each poll is a full-table seq scan.
	`CREATE INDEX IF NOT EXISTS idx_usage_records_created ON usage_records (created_at)`,
	// Idempotent backfill for deployments whose usage_records table predates the
	// prompt-cache accounting columns. These are display-only (the cache hit-rate
	// panel); billing never reads them.
	`ALTER TABLE usage_records ADD COLUMN IF NOT EXISTS cache_read_input_tokens     BIGINT NOT NULL DEFAULT 0`,
	`ALTER TABLE usage_records ADD COLUMN IF NOT EXISTS cache_creation_input_tokens BIGINT NOT NULL DEFAULT 0`,

	// ---- recharge_records: append-only "进账" (充值/开卡) ledger ----
	// Symmetric to usage_records but for credits GRANTED rather than consumed.
	// Each row logs one top-up: amount is the credits added this time, and
	// balance_after snapshots api_keys.credits_granted immediately after the
	// increment. A key's balance is api_keys.credits_granted - credits_used, so
	// this log and usage_counters are the two monotonic sides of one account book.
	// id is BIGSERIAL because rows are opaque and never addressed by a business key.
	`CREATE TABLE IF NOT EXISTS recharge_records (
    id            BIGSERIAL PRIMARY KEY,
    api_key_id    TEXT             NOT NULL,
    amount        DOUBLE PRECISION NOT NULL,
    operator      TEXT             NOT NULL DEFAULT '',
    note          TEXT             NOT NULL DEFAULT '',
    balance_after DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at    BIGINT           NOT NULL DEFAULT 0
)`,
	`CREATE INDEX IF NOT EXISTS idx_recharge_records_api_key ON recharge_records (api_key_id, created_at DESC)`,

	// ---- request_logs: append-only audit trail of every proxied request ----
	// One row per request (success or error) with endpoint/model/account, token +
	// credit cost, latency and error classification. This is the operational audit
	// log; billing truth still lives in usage_counters. Rows are pruned only beyond
	// a large retention window so the trail stays auditable without growing forever.
	`CREATE TABLE IF NOT EXISTS request_logs (
    id          BIGSERIAL PRIMARY KEY,
    ts          BIGINT           NOT NULL DEFAULT 0,
    endpoint    TEXT             NOT NULL DEFAULT '',
    model       TEXT             NOT NULL DEFAULT '',
    account_id  TEXT             NOT NULL DEFAULT '',
    api_key_id  TEXT             NOT NULL DEFAULT '',
    status      TEXT             NOT NULL DEFAULT '',
    error       TEXT             NOT NULL DEFAULT '',
    error_type  TEXT             NOT NULL DEFAULT '',
    tokens      BIGINT           NOT NULL DEFAULT 0,
    credits     DOUBLE PRECISION NOT NULL DEFAULT 0,
    duration_ms BIGINT           NOT NULL DEFAULT 0
)`,
	`CREATE INDEX IF NOT EXISTS idx_request_logs_id_desc ON request_logs (id DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_request_logs_account ON request_logs (account_id, id DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_request_logs_apikey ON request_logs (api_key_id, id DESC)`,
}
