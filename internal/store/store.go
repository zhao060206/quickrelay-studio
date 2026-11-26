// Package store persists upstream accounts in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Account is one upstream QuickWork identity plus everything needed to
// refresh it and open its WebSocket session.
type Account struct {
	ID          int64  `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Provider    string `json:"provider"`
	BuildStage  string `json:"build_stage"`
	Region      string `json:"region"`

	TenantURL string `json:"tenant_url"`
	WSHost    string `json:"ws_host"`
	WSPath    string `json:"ws_path"`

	AccessToken  string `json:"-"`
	RefreshToken string `json:"-"`
	IDToken      string `json:"-"`

	ActorARN     string `json:"actor_arn"`
	AWSAccountID string `json:"aws_account_id"`
	AccountName  string `json:"account_name"`

	ClientID      string `json:"-"`
	ClientSecret  string `json:"-"`
	IssuerURL     string `json:"-"`
	IDCRegion     string `json:"-"`
	AuthEndpoint  string `json:"-"`
	TokenEndpoint string `json:"-"`
	RedirectURI   string `json:"-"`

	UserInfo string `json:"-"`

	ExpiresAt  int64         `json:"expires_at"`
	ObtainedAt int64         `json:"obtained_at"`
	CreatedAt  int64         `json:"created_at"`
	UpdatedAt  int64         `json:"updated_at"`
	LastUsedAt int64         `json:"last_used_at"`
	UseCount   int64         `json:"use_count"`
	Disabled   bool          `json:"disabled"`
	LastError  string        `json:"last_error"`
	Usage      CallStats     `json:"usage"`
	Quota      QuotaSnapshot `json:"quota"`
}

// JWT returns the token used for the upstream WebSocket handshake.
func (a *Account) JWT() string {
	if a.IDToken != "" {
		return a.IDToken
	}
	return a.AccessToken
}

// Expiring reports whether the access token should be refreshed now.
func (a *Account) Expiring(now time.Time, before time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return false
	}
	return now.Add(before).Unix() >= a.ExpiresAt
}

// Store is a small SQLite-backed account repository. A single connection is
// used to avoid SQLite writer contention.
type Store struct {
	db *sql.DB
}

// Open creates the database and its schema when needed.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("database path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=10000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	// The database holds refresh tokens and client secrets.
	_ = os.Chmod(path, 0o600)
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS accounts (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	email          TEXT NOT NULL DEFAULT '' COLLATE NOCASE,
	display_name   TEXT NOT NULL DEFAULT '',
	provider       TEXT NOT NULL DEFAULT '',
	build_stage    TEXT NOT NULL DEFAULT 'prod',
	region         TEXT NOT NULL DEFAULT '',
	tenant_url     TEXT NOT NULL DEFAULT '',
	ws_host        TEXT NOT NULL DEFAULT '',
	ws_path        TEXT NOT NULL DEFAULT '',
	access_token   TEXT NOT NULL DEFAULT '',
	refresh_token  TEXT NOT NULL DEFAULT '',
	id_token       TEXT NOT NULL DEFAULT '',
	actor_arn      TEXT NOT NULL DEFAULT '',
	aws_account_id TEXT NOT NULL DEFAULT '',
	account_name   TEXT NOT NULL DEFAULT '',
	client_id      TEXT NOT NULL DEFAULT '',
	client_secret  TEXT NOT NULL DEFAULT '',
	issuer_url     TEXT NOT NULL DEFAULT '',
	idc_region     TEXT NOT NULL DEFAULT '',
	auth_endpoint  TEXT NOT NULL DEFAULT '',
	token_endpoint TEXT NOT NULL DEFAULT '',
	redirect_uri   TEXT NOT NULL DEFAULT '',
	user_info      TEXT NOT NULL DEFAULT '',
	expires_at     INTEGER NOT NULL DEFAULT 0,
	obtained_at    INTEGER NOT NULL DEFAULT 0,
	created_at     INTEGER NOT NULL DEFAULT 0,
	updated_at     INTEGER NOT NULL DEFAULT 0,
	last_used_at   INTEGER NOT NULL DEFAULT 0,
	use_count      INTEGER NOT NULL DEFAULT 0,
	disabled       INTEGER NOT NULL DEFAULT 0,
	last_error     TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_email_tenant ON accounts(email, tenant_url);
CREATE INDEX IF NOT EXISTS idx_accounts_enabled ON accounts(disabled, id);
CREATE TABLE IF NOT EXISTS account_metrics (
	account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
	call_count INTEGER NOT NULL DEFAULT 0,
	success_count INTEGER NOT NULL DEFAULT 0,
	failure_count INTEGER NOT NULL DEFAULT 0,
	total_duration_ms INTEGER NOT NULL DEFAULT 0,
	prompt_tokens INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	total_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens INTEGER NOT NULL DEFAULT 0,
	cache_write_tokens INTEGER NOT NULL DEFAULT 0,
	cost_usd REAL NOT NULL DEFAULT 0,
	cost_reported_calls INTEGER NOT NULL DEFAULT 0,
	stats_since INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS account_quota (
	account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
	snapshot TEXT NOT NULL DEFAULT '{}',
	synced_at INTEGER NOT NULL DEFAULT 0,
	last_attempt_at INTEGER NOT NULL DEFAULT 0,
	error_code TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS account_credits (
	account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	unit TEXT NOT NULL,
	amount REAL NOT NULL DEFAULT 0,
	reported_calls INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (account_id, unit)
);
CREATE TABLE IF NOT EXISTS conversations (
	conv_key TEXT PRIMARY KEY,
	thread_id TEXT NOT NULL,
	account_id INTEGER NOT NULL DEFAULT 0,
	base_hash TEXT NOT NULL DEFAULT '',
	chat_agent_id TEXT NOT NULL DEFAULT '',
	chat_agent_bound INTEGER NOT NULL DEFAULT 0,
	message_hashes TEXT NOT NULL DEFAULT '[]',
	last_used INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_conversations_last_used ON conversations(last_used);
CREATE INDEX IF NOT EXISTS idx_conversations_thread_id ON conversations(thread_id);
CREATE TABLE IF NOT EXISTS request_logs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	request_id TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	protocol TEXT NOT NULL DEFAULT 'openai',
	endpoint TEXT NOT NULL DEFAULT '/v1/chat/completions',
	requested_model TEXT NOT NULL DEFAULT '',
	upstream_model TEXT NOT NULL DEFAULT '',
	resolved_model TEXT NOT NULL DEFAULT '',
	thinking_effort TEXT NOT NULL DEFAULT '',
	stream INTEGER NOT NULL DEFAULT 0,
	account_id INTEGER NOT NULL DEFAULT 0,
	account_email TEXT NOT NULL DEFAULT '',
	thread_id TEXT NOT NULL DEFAULT '',
	session_reused INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL DEFAULT 'ok',
	finish_reason TEXT NOT NULL DEFAULT '',
	error_message TEXT NOT NULL DEFAULT '',
	attempts INTEGER NOT NULL DEFAULT 1,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	prompt_tokens INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	total_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens INTEGER NOT NULL DEFAULT 0,
	cache_write_tokens INTEGER NOT NULL DEFAULT 0,
	cost_usd REAL NOT NULL DEFAULT 0,
	tool_calls_count INTEGER NOT NULL DEFAULT 0,
	images_count INTEGER NOT NULL DEFAULT 0,
	prompt_preview TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_request_logs_created_at ON request_logs(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_request_logs_status ON request_logs(status);
CREATE INDEX IF NOT EXISTS idx_request_logs_account ON request_logs(account_id);
`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	return s.ensureConversationColumns()
}

// ensureConversationColumns 幂等迁移 conversations 表新增列，确保老版本数据库无损升级。
func (s *Store) ensureConversationColumns() error {
	rows, err := s.db.Query(`PRAGMA table_info(conversations)`)
	if err != nil {
		return err
	}
	hasLastAssistantHash := false
	hasChatAgentID := false
	hasChatAgentBound := false
	for rows.Next() {
		var (
			cid       int
			name, typ string
			notnull   int
			dflt      sql.NullString
			pk        int
		)
		if scanErr := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); scanErr != nil {
			rows.Close()
			return scanErr
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "last_assistant_hash":
			hasLastAssistantHash = true
		case "chat_agent_id":
			hasChatAgentID = true
		case "chat_agent_bound":
			hasChatAgentBound = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if !hasLastAssistantHash {
		if _, err := s.db.Exec(`ALTER TABLE conversations ADD COLUMN last_assistant_hash TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !hasChatAgentID {
		if _, err := s.db.Exec(`ALTER TABLE conversations ADD COLUMN chat_agent_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !hasChatAgentBound {
		if _, err := s.db.Exec(`ALTER TABLE conversations ADD COLUMN chat_agent_bound INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	return nil
}

const columns = `id, email, display_name, provider, build_stage, region,
	tenant_url, ws_host, ws_path, access_token, refresh_token, id_token,
	actor_arn, aws_account_id, account_name, client_id, client_secret,
	issuer_url, idc_region, auth_endpoint, token_endpoint, redirect_uri,
	user_info, expires_at, obtained_at, created_at, updated_at,
	last_used_at, use_count, disabled, last_error,
	COALESCE((SELECT json_array(call_count, success_count, failure_count, total_duration_ms,
		prompt_tokens, completion_tokens, total_tokens, cache_read_tokens, cache_write_tokens,
		CASE WHEN cost_reported_calls > 0 THEN cost_usd ELSE NULL END, cost_reported_calls, stats_since)
		FROM account_metrics WHERE account_id=accounts.id), '[]'),
	COALESCE((SELECT snapshot FROM account_quota WHERE account_id=accounts.id), '{}'),
	COALESCE((SELECT synced_at FROM account_quota WHERE account_id=accounts.id), 0),
	COALESCE((SELECT last_attempt_at FROM account_quota WHERE account_id=accounts.id), 0),
	COALESCE((SELECT error_code FROM account_quota WHERE account_id=accounts.id), ''),
	COALESCE((SELECT json_group_array(json_object('unit', unit, 'amount', amount, 'reported_calls', reported_calls))
		FROM account_credits WHERE account_id=accounts.id), '[]')`

type scanner interface {
	Scan(dest ...any) error
}

func scanAccount(row scanner) (*Account, error) {
	var a Account
	var disabled int
	var metricsJSON, quotaJSON, quotaError, creditsJSON string
	var syncedAt, attemptedAt int64
	err := row.Scan(
		&a.ID, &a.Email, &a.DisplayName, &a.Provider, &a.BuildStage, &a.Region,
		&a.TenantURL, &a.WSHost, &a.WSPath, &a.AccessToken, &a.RefreshToken, &a.IDToken,
		&a.ActorARN, &a.AWSAccountID, &a.AccountName, &a.ClientID, &a.ClientSecret,
		&a.IssuerURL, &a.IDCRegion, &a.AuthEndpoint, &a.TokenEndpoint, &a.RedirectURI,
		&a.UserInfo, &a.ExpiresAt, &a.ObtainedAt, &a.CreatedAt, &a.UpdatedAt,
		&a.LastUsedAt, &a.UseCount, &disabled, &a.LastError,
		&metricsJSON, &quotaJSON, &syncedAt, &attemptedAt, &quotaError, &creditsJSON,
	)
	if err != nil {
		return nil, err
	}
	a.Disabled = disabled != 0
	if err := a.Usage.decode(metricsJSON); err != nil {
		return nil, err
	}
	if err := a.Quota.decode(quotaJSON, syncedAt, attemptedAt, quotaError); err != nil {
		return nil, err
	}
	if err := a.Usage.decodeCredits(creditsJSON); err != nil {
		return nil, err
	}
	return &a, nil
}

// Save inserts a new account or updates the matching (email, tenant_url)
// account. The account ID is updated in place.
func (s *Store) Save(ctx context.Context, a *Account) error {
	now := time.Now().Unix()
	if a.CreatedAt == 0 {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	var disabled int
	if a.Disabled {
		disabled = 1
	}
	const q = `
INSERT INTO accounts (email, display_name, provider, build_stage, region,
	tenant_url, ws_host, ws_path, access_token, refresh_token, id_token,
	actor_arn, aws_account_id, account_name, client_id, client_secret,
	issuer_url, idc_region, auth_endpoint, token_endpoint, redirect_uri,
	user_info, expires_at, obtained_at, created_at, updated_at,
	last_used_at, use_count, disabled, last_error)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(email, tenant_url) DO UPDATE SET
	display_name=excluded.display_name,
	provider=excluded.provider,
	build_stage=excluded.build_stage,
	region=excluded.region,
	ws_host=excluded.ws_host,
	ws_path=excluded.ws_path,
	access_token=excluded.access_token,
	refresh_token=excluded.refresh_token,
	id_token=excluded.id_token,
	actor_arn=excluded.actor_arn,
	aws_account_id=excluded.aws_account_id,
	account_name=excluded.account_name,
	client_id=excluded.client_id,
	client_secret=excluded.client_secret,
	issuer_url=excluded.issuer_url,
	idc_region=excluded.idc_region,
	auth_endpoint=excluded.auth_endpoint,
	token_endpoint=excluded.token_endpoint,
	redirect_uri=excluded.redirect_uri,
	user_info=excluded.user_info,
	expires_at=excluded.expires_at,
	obtained_at=excluded.obtained_at,
	updated_at=excluded.updated_at,
	disabled=0,
	last_error=''
RETURNING id`
	err := s.db.QueryRowContext(ctx, q,
		a.Email, a.DisplayName, a.Provider, a.BuildStage, a.Region,
		a.TenantURL, a.WSHost, a.WSPath, a.AccessToken, a.RefreshToken, a.IDToken,
		a.ActorARN, a.AWSAccountID, a.AccountName, a.ClientID, a.ClientSecret,
		a.IssuerURL, a.IDCRegion, a.AuthEndpoint, a.TokenEndpoint, a.RedirectURI,
		a.UserInfo, a.ExpiresAt, a.ObtainedAt, a.CreatedAt, a.UpdatedAt,
		a.LastUsedAt, a.UseCount, disabled, a.LastError,
	).Scan(&a.ID)
	return err
}

// Update rewrites a stored account after a token refresh.
func (s *Store) Update(ctx context.Context, a *Account) error {
	a.UpdatedAt = time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
UPDATE accounts SET
	display_name=?, provider=?, build_stage=?, region=?, tenant_url=?, ws_host=?, ws_path=?,
	access_token=?, refresh_token=?, id_token=?, actor_arn=?, aws_account_id=?, account_name=?,
	client_id=?, client_secret=?, issuer_url=?, idc_region=?, auth_endpoint=?, token_endpoint=?,
	redirect_uri=?, user_info=?, expires_at=?, obtained_at=?, updated_at=?, last_error=?
WHERE id=?`,
		a.DisplayName, a.Provider, a.BuildStage, a.Region, a.TenantURL, a.WSHost, a.WSPath,
		a.AccessToken, a.RefreshToken, a.IDToken, a.ActorARN, a.AWSAccountID, a.AccountName,
		a.ClientID, a.ClientSecret, a.IssuerURL, a.IDCRegion, a.AuthEndpoint, a.TokenEndpoint,
		a.RedirectURI, a.UserInfo, a.ExpiresAt, a.ObtainedAt, a.UpdatedAt, a.LastError, a.ID,
	)
	return err
}

// List returns all accounts ordered by ID.
func (s *Store) List(ctx context.Context) ([]Account, error) {
	return s.query(ctx, `SELECT `+columns+` FROM accounts ORDER BY id`)
}

// Enabled returns accounts eligible for rotation ordered by ID.
func (s *Store) Enabled(ctx context.Context) ([]Account, error) {
	return s.query(ctx, `SELECT `+columns+` FROM accounts WHERE disabled=0 ORDER BY id`)
}

func (s *Store) query(ctx context.Context, q string, args ...any) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Account, 0, 8)
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// Get returns one account, or sql.ErrNoRows when absent.
func (s *Store) Get(ctx context.Context, id int64) (*Account, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM accounts WHERE id=?`, id)
	return scanAccount(row)
}

// Delete removes one account.
func (s *Store) Delete(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id=?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteAll removes every account and returns how many were deleted.
func (s *Store) DeleteAll(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM accounts`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// MarkUsed records a successful account selection.
func (s *Store) MarkUsed(ctx context.Context, id int64) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET last_used_at=?, use_count=use_count+1 WHERE id=?`, now, id)
	return err
}

// SetError records the last failure without disabling the account.
func (s *Store) SetError(ctx context.Context, id int64, message string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET last_error=?, updated_at=? WHERE id=?`,
		truncate(message, 300), time.Now().Unix(), id)
	return err
}

// SetDisabled toggles an account in or out of rotation.
func (s *Store) SetDisabled(ctx context.Context, id int64, disabled bool) error {
	v := 0
	if disabled {
		v = 1
	}
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET disabled=? WHERE id=?`, v, id)
	return err
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n]
}
