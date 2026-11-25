package admin

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"quickrelay-studio/internal/config"
	"quickrelay-studio/internal/version"
)

type settings struct {
	Addr             string          `json:"addr"`
	Database         string          `json:"database"`
	Models           []string        `json:"models"`
	DefaultModel       string          `json:"default_model"`
	RequestTimeout     config.Duration `json:"request_timeout"`
	MaxThinkingTimeout config.Duration `json:"max_thinking_timeout"`
	RefreshBefore      config.Duration `json:"refresh_before"`
	MaxAttempts      int             `json:"max_attempts"`
	AppVersion       string          `json:"app_version"`
	Surface          string          `json:"surface"`
	OutpostID        string          `json:"outpost_id"`
	WorkingSessionID string          `json:"working_session_id"`
	Timezone         string          `json:"timezone"`
	Locale           string          `json:"locale"`
	ThinkingEffort   string          `json:"thinking_effort"`
	ChatAgentID      string          `json:"chat_agent_id"`
	KeepSessions     bool            `json:"keep_sessions"`
	SessionReuse     bool            `json:"session_reuse"`
	SessionTTL       config.Duration `json:"session_ttl"`
	OAuth            config.OAuth      `json:"oauth"`
	WebUI            config.WebUI      `json:"webui"`
	Usage            config.Usage      `json:"usage"`
	Guardrails       config.Guardrails `json:"guardrails"`
	Updater          config.Updater    `json:"updater"`
}

func settingsFromConfig(cfg config.Config) settings {
	return settings{
		Addr: cfg.Addr, Database: cfg.Database, Models: cfg.Models, DefaultModel: cfg.DefaultModel,
		RequestTimeout: cfg.RequestTimeout, MaxThinkingTimeout: cfg.MaxThinkingTimeout, RefreshBefore: cfg.RefreshBefore, MaxAttempts: cfg.MaxAttempts,
		AppVersion: cfg.AppVersion, Surface: cfg.Surface, OutpostID: cfg.OutpostID,
		WorkingSessionID: cfg.WorkingSessionID, Timezone: cfg.Timezone, Locale: cfg.Locale,
		ThinkingEffort: cfg.ThinkingEffort, ChatAgentID: cfg.ChatAgentID, KeepSessions: cfg.KeepSessions,
		SessionReuse: cfg.SessionReuse, SessionTTL: cfg.SessionTTL, OAuth: cfg.OAuth, WebUI: cfg.WebUI, Usage: cfg.Usage,
		Guardrails: cfg.Guardrails, Updater: cfg.Updater,
	}
}

func (value settings) config(keys []string, path string) config.Config {
	maxThink := value.MaxThinkingTimeout
	if maxThink <= 0 {
		maxThink = config.Duration(30 * time.Minute)
		if value.RequestTimeout > maxThink {
			maxThink = value.RequestTimeout
		}
	}
	updaterCfg := value.Updater
	if strings.TrimSpace(updaterCfg.Repo) == "" {
		updaterCfg.Repo = version.DefaultRepo
	} else {
		updaterCfg.Repo = strings.TrimSpace(updaterCfg.Repo)
	}
	if strings.TrimSpace(updaterCfg.Mode) == "" {
		updaterCfg.Mode = "auto"
	} else {
		updaterCfg.Mode = strings.ToLower(strings.TrimSpace(updaterCfg.Mode))
	}
	updaterCfg.CustomMirror = strings.TrimRight(strings.TrimSpace(updaterCfg.CustomMirror), "/")
	guardrailsCfg := value.Guardrails
	if guardrailsCfg.CompactDowngrade == nil {
		v := true
		guardrailsCfg.CompactDowngrade = &v
	}
	if guardrailsCfg.NoToolMaxCap == nil {
		v := true
		guardrailsCfg.NoToolMaxCap = &v
	}
	if guardrailsCfg.PreserveEnvSummary == nil {
		v := true
		guardrailsCfg.PreserveEnvSummary = &v
	}
	return config.Config{
		Addr: value.Addr, Database: value.Database, APIKeys: keys, Models: value.Models, DefaultModel: value.DefaultModel,
		RequestTimeout: value.RequestTimeout, MaxThinkingTimeout: maxThink, RefreshBefore: value.RefreshBefore, MaxAttempts: value.MaxAttempts,
		AppVersion: value.AppVersion, Surface: value.Surface, OutpostID: value.OutpostID,
		WorkingSessionID: value.WorkingSessionID, Timezone: value.Timezone, Locale: value.Locale,
		ThinkingEffort: value.ThinkingEffort, ChatAgentID: value.ChatAgentID, KeepSessions: value.KeepSessions,
		SessionReuse: value.SessionReuse, SessionTTL: value.SessionTTL, OAuth: value.OAuth, WebUI: value.WebUI, Usage: value.Usage,
		Guardrails: guardrailsCfg, Updater: updaterCfg, Path: path,
	}
}

func (value settings) validate() error {
	_, port, err := net.SplitHostPort(value.Addr)
	if err != nil {
		return fmt.Errorf("addr must be a host:port address")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("addr port must be between 1 and 65535")
	}
	if strings.TrimSpace(value.Database) == "" || strings.ContainsRune(value.Database, 0) {
		return fmt.Errorf("database must be a non-empty file path")
	}
	if len(value.Models) == 0 || len(value.Models) > 256 {
		return fmt.Errorf("models must contain between 1 and 256 aliases")
	}
	seen := make(map[string]bool, len(value.Models))
	for _, model := range value.Models {
		if strings.TrimSpace(model) == "" || model != strings.TrimSpace(model) || seen[model] {
			return fmt.Errorf("models must contain unique, non-empty aliases without surrounding whitespace")
		}
		seen[model] = true
	}
	if !seen[value.DefaultModel] {
		return fmt.Errorf("default_model must be one of models")
	}
	if value.RequestTimeout <= 0 || value.MaxThinkingTimeout < 0 || value.RefreshBefore < 0 || value.SessionTTL <= 0 || value.OAuth.Timeout <= 0 {
		return fmt.Errorf("timeouts must be positive and refresh_before must not be negative")
	}
	if value.MaxAttempts < 1 || value.MaxAttempts > 100 {
		return fmt.Errorf("max_attempts must be between 1 and 100")
	}
	if value.OAuth.CallbackPort < 1 || value.OAuth.CallbackPort > 65535 {
		return fmt.Errorf("oauth.callback_port must be between 1 and 65535")
	}
	if strings.TrimSpace(value.AppVersion) == "" || strings.TrimSpace(value.Surface) == "" || strings.TrimSpace(value.Locale) == "" {
		return fmt.Errorf("app_version, surface and locale must not be empty")
	}
	if _, err := time.LoadLocation(value.Timezone); err != nil {
		return fmt.Errorf("timezone must be a valid IANA time zone")
	}
	if len(value.OAuth.Region) > 64 || strings.Trim(value.OAuth.Region, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
		return fmt.Errorf("oauth.region may only contain lowercase letters, digits and hyphens")
	}
	switch value.ThinkingEffort {
	case "", "off", "low", "medium", "high", "max":
	default:
		return fmt.Errorf("thinking_effort must be empty, off, low, medium, high or max")
	}
	if err := config.ValidateUpdater(value.Updater); err != nil {
		return err
	}
	return nil
}

func validAPIKey(key string) bool {
	if len(key) == 0 || len(key) > 1024 {
		return false
	}
	for _, character := range key {
		if character < 33 || character > 126 {
			return false
		}
	}
	return true
}

func (s *Server) writeSettingsLocked(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"settings": settingsFromConfig(s.saved), "api_keys_count": len(s.saved.APIKeys),
		"restart_required": s.restartNeeded, "overrides": map[string]bool{"addr": s.addrOverridden},
	})
}

func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.writeSettingsLocked(w)
}

func (s *Server) handleConfigSave(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Settings *settings `json:"settings"`
		APIKeys  *[]string `json:"api_keys"`
	}
	if !decodeJSON(w, r, &input, 256<<10) {
		return
	}
	if input.Settings == nil {
		writeError(w, http.StatusBadRequest, "invalid_config", "settings must be specified")
		return
	}
	if err := input.Settings.validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_config", err.Error())
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	keys := s.saved.APIKeys
	if input.APIKeys != nil {
		if len(*input.APIKeys) > 256 {
			writeError(w, http.StatusBadRequest, "invalid_api_keys", "At most 256 API keys are allowed")
			return
		}
		keys = make([]string, 0, len(*input.APIKeys))
		seen := make(map[string]bool, len(*input.APIKeys))
		for _, key := range *input.APIKeys {
			if !validAPIKey(key) || seen[key] {
				writeError(w, http.StatusBadRequest, "invalid_api_keys", "API keys must be unique and contain 1-1024 printable ASCII characters without whitespace")
				return
			}
			seen[key] = true
			keys = append(keys, key)
		}
	}
	candidate := input.Settings.config(keys, s.cfg.Path)
	candidate.SetModelAliases(s.saved.GetModelAliases())
	if err := candidate.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_config", err.Error())
		return
	}
	if err := candidate.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, "config_save_failed", "Could not save configuration; check field values and config directory permissions")
		return
	}
	s.saved = candidate
	s.restartNeeded = true
	s.writeSettingsLocked(w)
}
