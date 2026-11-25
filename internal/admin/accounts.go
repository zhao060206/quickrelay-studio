package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"quickrelay-studio/internal/accounts"
	"quickrelay-studio/internal/store"
	"quickrelay-studio/internal/version"
)

type accountView struct {
	ID          int64               `json:"id"`
	Email       string              `json:"email"`
	DisplayName string              `json:"display_name"`
	Provider    string              `json:"provider"`
	BuildStage  string              `json:"build_stage"`
	Region      string              `json:"region"`
	WSHost      string              `json:"ws_host"`
	ExpiresAt   int64               `json:"expires_at"`
	UseCount    int64               `json:"use_count"`
	LastUsedAt  int64               `json:"last_used_at"`
	Disabled    bool                `json:"disabled"`
	HasError    bool                `json:"has_error"`
	Usage       store.CallStats     `json:"usage"`
	Quota       store.QuotaSnapshot `json:"quota"`
}

func publicAccount(account store.Account) accountView {
	return accountView{
		ID: account.ID, Email: account.Email, DisplayName: account.DisplayName,
		Provider: account.Provider, BuildStage: account.BuildStage, Region: account.Region,
		WSHost: account.WSHost, ExpiresAt: account.ExpiresAt, UseCount: account.UseCount,
		LastUsedAt: account.LastUsedAt, Disabled: account.Disabled, HasError: account.LastError != "",
		Usage: account.Usage, Quota: account.Quota,
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "Could not read account status")
		return
	}
	counts := struct {
		Total             int                 `json:"total"`
		Enabled           int                 `json:"enabled"`
		Expired           int                 `json:"expired"`
		Errors            int                 `json:"errors"`
		Uses              int64               `json:"uses"`
		Calls             int64               `json:"calls"`
		Successes         int64               `json:"successes"`
		Failures          int64               `json:"failures"`
		TotalDurationMS   int64               `json:"total_duration_ms"`
		PromptTokens      int64               `json:"prompt_tokens"`
		CompletionTokens  int64               `json:"completion_tokens"`
		TotalTokens       int64               `json:"total_tokens"`
		CostUSD           *float64            `json:"cost_usd"`
		CostReportedCalls int64               `json:"cost_reported_calls"`
		Credits           []store.CreditStats `json:"credits"`
	}{Total: len(list)}
	counts.Credits = []store.CreditStats{}
	credits := make(map[string]store.CreditStats)
	now := time.Now().Unix()
	for _, account := range list {
		if !account.Disabled {
			counts.Enabled++
		}
		if account.ExpiresAt > 0 && account.ExpiresAt <= now {
			counts.Expired++
		}
		if account.LastError != "" {
			counts.Errors++
		}
		counts.Uses += account.UseCount
		counts.Calls += account.Usage.CallCount
		counts.Successes += account.Usage.SuccessCount
		counts.Failures += account.Usage.FailureCount
		counts.TotalDurationMS += account.Usage.TotalDurationMS
		counts.PromptTokens += account.Usage.PromptTokens
		counts.CompletionTokens += account.Usage.CompletionTokens
		counts.TotalTokens += account.Usage.TotalTokens
		counts.CostReportedCalls += account.Usage.CostReportedCalls
		if account.Usage.CostUSD != nil {
			if counts.CostUSD == nil {
				amount := 0.0
				counts.CostUSD = &amount
			}
			*counts.CostUSD += *account.Usage.CostUSD
		}
		for _, credit := range account.Usage.Credits {
			total := credits[credit.Unit]
			total.Unit = credit.Unit
			total.Amount += credit.Amount
			total.ReportedCalls += credit.ReportedCalls
			credits[credit.Unit] = total
		}
	}
	for _, credit := range credits {
		counts.Credits = append(counts.Credits, credit)
	}
	keysConfigured := false
	for _, key := range s.cfg.APIKeys {
		if strings.TrimSpace(key) != "" {
			keysConfigured = true
			break
		}
	}
	activeConversations, _ := s.store.CountConversations(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "quickrelay-studio", "version": version.Current, "started_at": s.startedAt.Unix(), "accounts": counts,
		"models": s.cfg.Models, "default_model": s.cfg.DefaultModel, "work_mode": s.cfg.GetWorkMode(),
		"api_keys_configured": keysConfigured, "webui_enabled": s.cfg.WebUI.Enabled,
		"active_conversations": activeConversations, "usage_sync": s.accounts.UsageStatus(),
	})
}

func (s *Server) handleAccountUsageRefresh(w http.ResponseWriter, r *http.Request) {
	id, ok := s.accountID(w, r)
	if !ok {
		return
	}
	quota, err := s.accounts.RefreshUsage(r.Context(), id)
	if err != nil {
		writeUsageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"quota": quota})
}

func (s *Server) handleAccountsUsageRefresh(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout.Duration())
	defer cancel()
	results, err := s.accounts.RefreshAllUsage(ctx)
	if err != nil {
		writeUsageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func writeUsageError(w http.ResponseWriter, err error) {
	code := accounts.UsageErrorCode(err)
	status := http.StatusBadGateway
	message := "Could not synchronize official account usage; the last successful snapshot is preserved"
	switch code {
	case "usage_busy":
		status = http.StatusConflict
		message = "Account usage synchronization is already running"
	case "usage_unavailable":
		status = http.StatusServiceUnavailable
		message = "Official account usage is not available for this account or provider"
	case "usage_unauthorized":
		message = "Official account usage authorization failed; sign in to this account again"
	}
	writeError(w, status, code, message)
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "Could not list accounts")
		return
	}
	accounts := make([]accountView, 0, len(list))
	for _, account := range list {
		accounts = append(accounts, publicAccount(account))
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

func (s *Server) accountID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	accountID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || accountID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_account_id", "Account ID must be a positive integer")
		return 0, false
	}
	if _, err := s.store.Get(r.Context(), accountID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "account_not_found", "Account not found")
		} else {
			writeError(w, http.StatusInternalServerError, "database_error", "Could not read account")
		}
		return 0, false
	}
	return accountID, true
}

func (s *Server) handleAccountUpdate(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Disabled *bool `json:"disabled"`
	}
	if !decodeJSON(w, r, &input, 1024) {
		return
	}
	if input.Disabled == nil {
		writeError(w, http.StatusBadRequest, "invalid_account_update", "disabled must be specified")
		return
	}
	accountID, ok := s.accountID(w, r)
	if !ok {
		return
	}
	if err := s.store.SetDisabled(r.Context(), accountID, *input.Disabled); err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "Could not update account")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.accountID(w, r)
	if !ok {
		return
	}
	removed, err := s.store.Delete(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "Could not delete account")
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "account_not_found", "Account not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAccountRefresh(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.accountID(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout.Duration())
	defer cancel()
	account, err := s.accounts.RefreshOne(ctx, accountID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "refresh_failed", "Could not refresh account; sign in to it again if its refresh token has expired")
		return
	}
	writeJSON(w, http.StatusOK, publicAccount(*account))
}

func (s *Server) handleAccountsRefresh(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout.Duration())
	defer cancel()
	results, err := s.accounts.RefreshAll(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "refresh_failed", "Could not complete account refresh")
		return
	}
	type refreshResult struct {
		ID int64 `json:"id"`
		OK bool  `json:"ok"`
	}
	output := make([]refreshResult, 0, len(results))
	for _, result := range results {
		if result.Account != nil {
			output = append(output, refreshResult{ID: result.Account.ID, OK: result.Err == nil})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": output})
}
