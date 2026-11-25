package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
)

type CallUsage struct {
	Duration         time.Duration
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CacheReadTokens  int
	CacheWriteTokens int
	CostUSD          *float64
	Credits          []CreditUsage
	Success          bool
}

type CallStats struct {
	CallCount         int64         `json:"call_count"`
	SuccessCount      int64         `json:"success_count"`
	FailureCount      int64         `json:"failure_count"`
	TotalDurationMS   int64         `json:"total_duration_ms"`
	PromptTokens      int64         `json:"prompt_tokens"`
	CompletionTokens  int64         `json:"completion_tokens"`
	TotalTokens       int64         `json:"total_tokens"`
	CacheReadTokens   int64         `json:"cache_read_tokens"`
	CacheWriteTokens  int64         `json:"cache_write_tokens"`
	CostUSD           *float64      `json:"cost_usd"`
	CostReportedCalls int64         `json:"cost_reported_calls"`
	StatsSince        int64         `json:"stats_since"`
	Credits           []CreditStats `json:"credits"`
}

type CreditUsage struct {
	Unit   string
	Amount float64
}

type CreditStats struct {
	Unit          string  `json:"unit"`
	Amount        float64 `json:"amount"`
	ReportedCalls int64   `json:"reported_calls"`
}

func (stats *CallStats) decodeCredits(raw string) error {
	if err := json.Unmarshal([]byte(raw), &stats.Credits); err != nil {
		return errors.New("invalid stored credit statistics")
	}
	if stats.Credits == nil {
		stats.Credits = []CreditStats{}
	}
	return nil
}

func (stats *CallStats) decode(raw string) error {
	var values []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return errors.New("invalid stored call statistics")
	}
	if len(values) == 0 {
		return nil
	}
	destinations := []any{
		&stats.CallCount, &stats.SuccessCount, &stats.FailureCount, &stats.TotalDurationMS,
		&stats.PromptTokens, &stats.CompletionTokens, &stats.TotalTokens, &stats.CacheReadTokens,
		&stats.CacheWriteTokens, &stats.CostUSD, &stats.CostReportedCalls, &stats.StatsSince,
	}
	if len(values) != len(destinations) {
		return errors.New("invalid stored call statistics")
	}
	for index, destination := range destinations {
		if err := json.Unmarshal(values[index], destination); err != nil {
			return errors.New("invalid stored call statistics")
		}
	}
	return nil
}

func (s *Store) RecordCall(ctx context.Context, id int64, usage CallUsage) error {
	durationMS := max(usage.Duration.Milliseconds(), 0)
	success := 0
	if usage.Success {
		success = 1
	}
	var cost float64
	var costReported int
	if usage.CostUSD != nil && !math.IsNaN(*usage.CostUSD) && !math.IsInf(*usage.CostUSD, 0) && *usage.CostUSD >= 0 {
		cost = *usage.CostUSD
		costReported = 1
	}
	prompt := max(usage.PromptTokens, 0)
	completion := max(usage.CompletionTokens, 0)
	total := max(usage.TotalTokens, prompt+completion)
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	_, err = transaction.ExecContext(ctx, `
INSERT INTO account_metrics (account_id, call_count, success_count, failure_count, total_duration_ms,
	prompt_tokens, completion_tokens, total_tokens, cache_read_tokens, cache_write_tokens,
	cost_usd, cost_reported_calls, stats_since)
SELECT id, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ? FROM accounts WHERE id=?
ON CONFLICT(account_id) DO UPDATE SET
	call_count=call_count+1,
	success_count=success_count+excluded.success_count,
	failure_count=failure_count+excluded.failure_count,
	total_duration_ms=total_duration_ms+excluded.total_duration_ms,
	prompt_tokens=prompt_tokens+excluded.prompt_tokens,
	completion_tokens=completion_tokens+excluded.completion_tokens,
	total_tokens=total_tokens+excluded.total_tokens,
	cache_read_tokens=cache_read_tokens+excluded.cache_read_tokens,
	cache_write_tokens=cache_write_tokens+excluded.cache_write_tokens,
	cost_usd=cost_usd+excluded.cost_usd,
	cost_reported_calls=cost_reported_calls+excluded.cost_reported_calls`,
		success, 1-success, durationMS, prompt, completion, total,
		max(usage.CacheReadTokens, 0), max(usage.CacheWriteTokens, 0), cost, costReported, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	credits := make(map[string]float64)
	for _, credit := range usage.Credits {
		if strings.TrimSpace(credit.Unit) == "" || len(credit.Unit) > 128 || math.IsNaN(credit.Amount) || math.IsInf(credit.Amount, 0) || credit.Amount < 0 {
			continue
		}
		credits[credit.Unit] += credit.Amount
	}
	for unit, amount := range credits {
		if math.IsInf(amount, 0) {
			continue
		}
		if _, err := transaction.ExecContext(ctx, `
INSERT INTO account_credits (account_id, unit, amount, reported_calls)
SELECT id, ?, ?, 1 FROM accounts WHERE id=?
ON CONFLICT(account_id, unit) DO UPDATE SET amount=amount+excluded.amount, reported_calls=reported_calls+1`, unit, amount, id); err != nil {
			return err
		}
	}
	return transaction.Commit()
}

type QuotaBucket struct {
	Used      *float64 `json:"used"`
	Limit     *float64 `json:"limit"`
	Remaining *float64 `json:"remaining"`
	ResetsAt  int64    `json:"resets_at"`
	Warning   string   `json:"warning,omitempty"`
}

type QuotaSnapshot struct {
	Source           string       `json:"source"`
	Unit             string       `json:"unit"`
	Monthly          *QuotaBucket `json:"monthly"`
	OverageMonthly   *QuotaBucket `json:"overage_monthly"`
	SubscriptionType string       `json:"subscription_type,omitempty"`
	SyncedAt         int64        `json:"synced_at"`
	LastAttemptAt    int64        `json:"last_attempt_at"`
	Status           string       `json:"status"`
	ErrorCode        string       `json:"error_code,omitempty"`
}

func (quota *QuotaSnapshot) decode(raw string, syncedAt, attemptedAt int64, errorCode string) error {
	if err := json.Unmarshal([]byte(raw), quota); err != nil {
		return errors.New("invalid stored quota snapshot")
	}
	quota.Source = "quickwork_profile_usage"
	quota.Unit = "minutes"
	quota.SyncedAt = syncedAt
	quota.LastAttemptAt = attemptedAt
	quota.ErrorCode = errorCode
	switch {
	case errorCode != "" && syncedAt > 0:
		quota.Status = "stale"
	case errorCode == "usage_unavailable":
		quota.Status = "unsupported"
	case errorCode != "":
		quota.Status = "error"
	case syncedAt > 0:
		quota.Status = "available"
	default:
		quota.Status = "unknown"
	}
	return nil
}

func (s *Store) SaveQuota(ctx context.Context, id int64, quota QuotaSnapshot) error {
	body, err := json.Marshal(quota)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx, `
INSERT INTO account_quota (account_id, snapshot, synced_at, last_attempt_at, error_code)
SELECT id, ?, ?, ?, '' FROM accounts WHERE id=?
ON CONFLICT(account_id) DO UPDATE SET
	snapshot=excluded.snapshot, synced_at=excluded.synced_at,
	last_attempt_at=excluded.last_attempt_at, error_code=''`, string(body), now, now, id)
	return err
}

func (s *Store) SetQuotaError(ctx context.Context, id int64, code string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO account_quota (account_id, last_attempt_at, error_code)
SELECT id, ?, ? FROM accounts WHERE id=?
ON CONFLICT(account_id) DO UPDATE SET last_attempt_at=excluded.last_attempt_at, error_code=excluded.error_code`,
		time.Now().Unix(), code, id)
	return err
}
