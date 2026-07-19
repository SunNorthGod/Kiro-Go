package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Account is the db-layer DTO for a Kiro credential. Field set mirrors the
// persisted fields of config.Account (minus JSON-only legacy migration fields
// such as LegacyAllowOverage). The `db` struct tags map 1:1 onto the accounts
// table columns and are used by pgx's by-name row scanner.
//
// NOTE: config.Account uses Enabled (true == active); this table stores the
// inverse, Disabled (true == inactive), because the task's schema specifies a
// `disabled` column. Callers migrating from config should set Disabled = !Enabled.
type Account struct {
	ID           string `db:"id"`
	Email        string `db:"email"`
	UserID       string `db:"user_id"`
	Nickname     string `db:"nickname"`
	AccessToken  string `db:"access_token"`
	RefreshToken string `db:"refresh_token"`
	ClientID     string `db:"client_id"`
	ClientSecret string `db:"client_secret"`
	AuthMethod   string `db:"auth_method"`
	Provider     string `db:"provider"`
	Region       string `db:"region"`
	StartURL     string `db:"start_url"`
	ExpiresAt    int64  `db:"expires_at"`
	MachineID    string `db:"machine_id"`
	ProfileArn   string `db:"profile_arn"`
	ProxyURL     string `db:"proxy_url"`
	Weight       int    `db:"weight"`

	// Overage state mirrored from the AWS Q upstream switch.
	OverageStatus     string  `db:"overage_status"`
	OverageCapability string  `db:"overage_capability"`
	OverageCap        float64 `db:"overage_cap"`
	OverageRate       float64 `db:"overage_rate"`
	CurrentOverages   float64 `db:"current_overages"`
	OverageCheckedAt  int64   `db:"overage_checked_at"`

	// Status.
	Disabled  bool   `db:"disabled"`
	BanStatus string `db:"ban_status"`
	BanReason string `db:"ban_reason"`
	BanTime   int64  `db:"ban_time"`

	// Subscription.
	SubscriptionType  string `db:"subscription_type"`
	SubscriptionTitle string `db:"subscription_title"`
	DaysRemaining     int    `db:"days_remaining"`

	// Usage snapshot (cached from upstream; NOT the billing ledger).
	UsageCurrent  float64 `db:"usage_current"`
	UsageLimit    float64 `db:"usage_limit"`
	UsagePercent  float64 `db:"usage_percent"`
	NextResetDate string  `db:"next_reset_date"`
	LastRefresh   int64   `db:"last_refresh"`

	// Trial usage snapshot.
	TrialUsageCurrent float64 `db:"trial_usage_current"`
	TrialUsageLimit   float64 `db:"trial_usage_limit"`
	TrialUsagePercent float64 `db:"trial_usage_percent"`
	TrialStatus       string  `db:"trial_status"`
	TrialExpiresAt    int64   `db:"trial_expires_at"`

	// Runtime stats.
	RequestCount int     `db:"request_count"`
	ErrorCount   int     `db:"error_count"`
	LastUsed     int64   `db:"last_used"`
	TotalTokens  int64   `db:"total_tokens"`
	TotalCredits float64 `db:"total_credits"`
	CreatedAt    int64   `db:"created_at"`

	// Extended-login credentials (external_idp / api_key). Mirror the fields
	// config.Account gained for enterprise SSO and Kiro API Key accounts.
	KiroApiKey    string `db:"kiro_api_key"`
	TokenEndpoint string `db:"token_endpoint"`
	IssuerUrl     string `db:"issuer_url"`
	Scopes        string `db:"scopes"`
}

// accountColumns is the canonical column order shared by SELECT, INSERT and the
// accountValues slice. Keep this list, accountValues and the Account `db` tags in
// lockstep.
var accountColumns = []string{
	"id", "email", "user_id", "nickname",
	"access_token", "refresh_token", "client_id", "client_secret",
	"auth_method", "provider", "region", "start_url",
	"expires_at", "machine_id", "profile_arn", "proxy_url", "weight",
	"overage_status", "overage_capability", "overage_cap", "overage_rate", "current_overages", "overage_checked_at",
	"disabled", "ban_status", "ban_reason", "ban_time",
	"subscription_type", "subscription_title", "days_remaining",
	"usage_current", "usage_limit", "usage_percent", "next_reset_date", "last_refresh",
	"trial_usage_current", "trial_usage_limit", "trial_usage_percent", "trial_status", "trial_expires_at",
	"request_count", "error_count", "last_used", "total_tokens", "total_credits", "created_at",
	"kiro_api_key", "token_endpoint", "issuer_url", "scopes",
}

// accountValues returns a's field values in accountColumns order.
func accountValues(a *Account) []any {
	return []any{
		a.ID, a.Email, a.UserID, a.Nickname,
		a.AccessToken, a.RefreshToken, a.ClientID, a.ClientSecret,
		a.AuthMethod, a.Provider, a.Region, a.StartURL,
		a.ExpiresAt, a.MachineID, a.ProfileArn, a.ProxyURL, a.Weight,
		a.OverageStatus, a.OverageCapability, a.OverageCap, a.OverageRate, a.CurrentOverages, a.OverageCheckedAt,
		a.Disabled, a.BanStatus, a.BanReason, a.BanTime,
		a.SubscriptionType, a.SubscriptionTitle, a.DaysRemaining,
		a.UsageCurrent, a.UsageLimit, a.UsagePercent, a.NextResetDate, a.LastRefresh,
		a.TrialUsageCurrent, a.TrialUsageLimit, a.TrialUsagePercent, a.TrialStatus, a.TrialExpiresAt,
		a.RequestCount, a.ErrorCount, a.LastUsed, a.TotalTokens, a.TotalCredits, a.CreatedAt,
		a.KiroApiKey, a.TokenEndpoint, a.IssuerUrl, a.Scopes,
	}
}

// accountSelect / accountUpsert are built once from accountColumns so the column
// list can never drift between statements.
var (
	accountSelect = "SELECT " + strings.Join(accountColumns, ", ") + " FROM accounts"
	accountUpsert = buildUpsert("accounts", accountColumns, []string{"id"})
)

// buildUpsert assembles "INSERT INTO <table> (cols) VALUES ($1..) ON CONFLICT
// (conflictCols) DO UPDATE SET col = EXCLUDED.col" for every non-conflict column.
func buildUpsert(table string, cols, conflict []string) string {
	placeholders := make([]string, len(cols))
	for i := range cols {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	conflictSet := make(map[string]bool, len(conflict))
	for _, c := range conflict {
		conflictSet[c] = true
	}
	updates := make([]string, 0, len(cols))
	for _, c := range cols {
		if conflictSet[c] {
			continue
		}
		updates = append(updates, fmt.Sprintf("%s = EXCLUDED.%s", c, c))
	}
	return fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (%s) DO UPDATE SET %s",
		table,
		strings.Join(cols, ", "),
		strings.Join(placeholders, ", "),
		strings.Join(conflict, ", "),
		strings.Join(updates, ", "),
	)
}

// UpsertAccount inserts a new account or updates the existing one with the same
// id. It is the combined create/update entry point (mirrors config.AddAccount +
// config.UpdateAccount).
func UpsertAccount(ctx context.Context, q Querier, a Account) error {
	if a.ID == "" {
		return errors.New("db: account id must not be empty")
	}
	if _, err := q.Exec(ctx, accountUpsert, accountValues(&a)...); err != nil {
		return fmt.Errorf("db: upsert account: %w", err)
	}
	return nil
}

// GetAccount returns the account with the given id, or (nil, nil) when not found.
func GetAccount(ctx context.Context, q Querier, id string) (*Account, error) {
	rows, err := q.Query(ctx, accountSelect+" WHERE id = $1", id)
	if err != nil {
		return nil, fmt.Errorf("db: get account: %w", err)
	}
	a, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[Account])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get account: %w", err)
	}
	return &a, nil
}

// ListAccounts returns every account ordered by created_at (oldest first).
func ListAccounts(ctx context.Context, q Querier) ([]Account, error) {
	rows, err := q.Query(ctx, accountSelect+" ORDER BY created_at ASC, id ASC")
	if err != nil {
		return nil, fmt.Errorf("db: list accounts: %w", err)
	}
	accounts, err := pgx.CollectRows(rows, pgx.RowToStructByName[Account])
	if err != nil {
		return nil, fmt.Errorf("db: list accounts: %w", err)
	}
	return accounts, nil
}

// ListEnabledAccounts returns only accounts that are not disabled.
func ListEnabledAccounts(ctx context.Context, q Querier) ([]Account, error) {
	rows, err := q.Query(ctx, accountSelect+" WHERE disabled = FALSE ORDER BY created_at ASC, id ASC")
	if err != nil {
		return nil, fmt.Errorf("db: list enabled accounts: %w", err)
	}
	accounts, err := pgx.CollectRows(rows, pgx.RowToStructByName[Account])
	if err != nil {
		return nil, fmt.Errorf("db: list enabled accounts: %w", err)
	}
	return accounts, nil
}

// SetAccountDisabled toggles the disabled flag for an account.
func SetAccountDisabled(ctx context.Context, q Querier, id string, disabled bool) error {
	if _, err := q.Exec(ctx, `UPDATE accounts SET disabled = $2 WHERE id = $1`, id, disabled); err != nil {
		return fmt.Errorf("db: set account disabled: %w", err)
	}
	return nil
}

// UpdateAccountToken updates the OAuth tokens and expiry for an account. A blank
// refreshToken is ignored (keeps the existing value), matching config semantics.
func UpdateAccountToken(ctx context.Context, q Querier, id, accessToken, refreshToken string, expiresAt int64) error {
	_, err := q.Exec(ctx, `
UPDATE accounts
   SET access_token  = $2,
       refresh_token = CASE WHEN $3 = '' THEN refresh_token ELSE $3 END,
       expires_at    = $4
 WHERE id = $1`, id, accessToken, refreshToken, expiresAt)
	if err != nil {
		return fmt.Errorf("db: update account token: %w", err)
	}
	return nil
}

// DeleteAccount removes the account with the given id. It is idempotent: deleting
// an unknown id is not an error.
func DeleteAccount(ctx context.Context, q Querier, id string) error {
	if _, err := q.Exec(ctx, `DELETE FROM accounts WHERE id = $1`, id); err != nil {
		return fmt.Errorf("db: delete account: %w", err)
	}
	return nil
}
