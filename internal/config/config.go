// Package config loads and persists the server configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"quickrelay-studio/internal/version"
)

// modelAliasMu 保护运行时动态更新的 ModelAliases 映射表，避免放在 Config 结构体中引发按值拷贝锁告警。
var modelAliasMu sync.RWMutex

// Duration is a time.Duration encoded as a Go duration string ("5m", "1h").
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		v, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil {
			return err
		}
		*d = Duration(v)
		return nil
	}
	var seconds float64
	if err := json.Unmarshal(data, &seconds); err != nil {
		return fmt.Errorf("duration must be a string like \"5m\" or seconds")
	}
	*d = Duration(seconds * float64(time.Second))
	return nil
}

// OAuth holds defaults used by the `login` command.
type OAuth struct {
	Provider     string   `json:"provider"`
	BuildStage   string   `json:"build_stage"`
	Region       string   `json:"region"`
	CallbackPort int      `json:"callback_port"`
	Timeout      Duration `json:"timeout"`
}

type WebUI struct {
	Enabled    bool     `json:"enabled"`
	SessionTTL Duration `json:"session_ttl"`
}

type Usage struct {
	Enabled         bool     `json:"enabled"`
	RefreshInterval Duration `json:"refresh_interval"`
	MaxAttempts     int      `json:"max_attempts"`
}

// Guardrails 控制针对上游 64K 单步上限与超长提示词的自动保护策略。
type Guardrails struct {
	CompactDowngrade   *bool `json:"compact_downgrade,omitempty"`
	NoToolMaxCap       *bool `json:"no_tool_max_cap,omitempty"`
	PreserveEnvSummary *bool `json:"preserve_env_summary,omitempty"`
}

func boolPtr(v bool) *bool {
	return &v
}

// CompactDowngradeEnabled 返回是否启用 /compact 自动降档为 low。
func (g Guardrails) CompactDowngradeEnabled() bool {
	return g.CompactDowngrade == nil || *g.CompactDowngrade
}

// NoToolMaxCapEnabled 返回是否启用无工具请求 max 限幅为 high。
func (g Guardrails) NoToolMaxCapEnabled() bool {
	return g.NoToolMaxCap == nil || *g.NoToolMaxCap
}

// PreserveEnvSummaryEnabled 返回是否在外置工作区规范时保留环境摘要。
func (g Guardrails) PreserveEnvSummaryEnabled() bool {
	return g.PreserveEnvSummary == nil || *g.PreserveEnvSummary
}

// Updater 控制内置 GitHub 更新检查与国内加速源自动降级策略。
type Updater struct {
	Repo         string `json:"repo"`
	Mode         string `json:"mode"`
	CustomMirror string `json:"custom_mirror,omitempty"`
}

var validRepoRegexp = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// ValidateUpdater 校验更新器配置字段的合法性。
func ValidateUpdater(u Updater) error {
	mode := strings.ToLower(strings.TrimSpace(u.Mode))
	switch mode {
	case "", "auto", "cn", "github":
	default:
		return fmt.Errorf("updater.mode must be auto, cn or github")
	}
	repo := strings.TrimSpace(u.Repo)
	if repo != "" && !validRepoRegexp.MatchString(repo) {
		return fmt.Errorf("updater.repo must be in owner/repo format")
	}
	mirror := strings.TrimSpace(u.CustomMirror)
	if mirror != "" {
		parsed, err := url.Parse(mirror)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("updater.custom_mirror must be a valid http:// or https:// URL")
		}
	}
	return nil
}

// Config is the server configuration stored in config.json.
type Config struct {
	Addr             string   `json:"addr"`
	Database         string   `json:"database"`
	APIKeys          []string          `json:"api_keys"`
	Models           []string          `json:"models"`
	DefaultModel     string            `json:"default_model"`
	ModelAliases       map[string]string `json:"model_aliases,omitempty"`
	RequestTimeout     Duration          `json:"request_timeout"`
	MaxThinkingTimeout Duration          `json:"max_thinking_timeout,omitempty"`
	RefreshBefore      Duration          `json:"refresh_before"`
	MaxAttempts      int      `json:"max_attempts"`
	AppVersion       string   `json:"app_version"`
	Surface          string   `json:"surface"`
	OutpostID        string   `json:"outpost_id"`
	WorkingSessionID string   `json:"working_session_id"`
	Timezone         string   `json:"timezone"`
	Locale           string   `json:"locale"`
	ThinkingEffort   string   `json:"thinking_effort"`
	ChatAgentID      string   `json:"chat_agent_id"`
	KeepSessions     bool     `json:"keep_sessions"`
	SessionReuse     bool     `json:"session_reuse"`
	SessionTTL       Duration `json:"session_ttl"`
	OAuth            OAuth      `json:"oauth"`
	WebUI            WebUI      `json:"webui"`
	Usage            Usage      `json:"usage"`
	Guardrails       Guardrails `json:"guardrails"`
	Updater          Updater    `json:"updater"`

	// Path is the absolute config file path; not serialized.
	Path string `json:"-"`

	originalDatabase string
	resolvedDatabase string
}

// Default returns the built-in configuration.
func Default() Config {
	return Config{
		Addr:           "127.0.0.1:8080",
		Database:       "data/quickrelay.db",
		APIKeys:        []string{},
		Models:         []string{"smart", "fast", "balanced", "auto"},
		DefaultModel:       "smart",
		RequestTimeout:     Duration(15 * time.Minute),
		MaxThinkingTimeout: Duration(30 * time.Minute),
		RefreshBefore:      Duration(10 * time.Minute),
		MaxAttempts:    3,
		AppVersion:     "1.2.4660",
		Surface:        "QUICK_WORK_DESKTOP_WINDOWS",
		Timezone:       "Asia/Singapore",
		Locale:         "zh-CN",
		ThinkingEffort: "",
		KeepSessions:   false,
		SessionReuse:   true,
		SessionTTL:     Duration(24 * time.Hour),
		OAuth: OAuth{
			Provider:     "social",
			BuildStage:   "prod",
			CallbackPort: 18080,
			Timeout:      Duration(time.Hour),
		},
		WebUI: WebUI{
			Enabled:    true,
			SessionTTL: Duration(12 * time.Hour),
		},
		Usage: Usage{
			Enabled:         true,
			RefreshInterval: Duration(5 * time.Minute),
			MaxAttempts:     3,
		},
		Guardrails: Guardrails{
			CompactDowngrade:   boolPtr(true),
			NoToolMaxCap:       boolPtr(true),
			PreserveEnvSummary: boolPtr(true),
		},
		Updater: Updater{
			Repo:         version.DefaultRepo,
			Mode:         "auto",
			CustomMirror: "",
		},
	}
}

// Load reads configPath, creating it with defaults when it does not exist.
// Relative paths inside the config (database) are resolved against the
// config file's directory so the server behaves the same from any cwd.
func Load(configPath string) (*Config, error) {
	if strings.TrimSpace(configPath) == "" {
		configPath = "config.json"
	}
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return nil, err
	}
	cfg := Default()
	body, err := os.ReadFile(abs)
	switch {
	case err == nil:
		if err := json.Unmarshal(body, &cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", abs, err)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := write(abs, &cfg); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	cfg.Path = abs
	applyDefaults(&cfg)
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", abs, err)
	}
	cfg.originalDatabase = cfg.Database
	if !filepath.IsAbs(cfg.Database) {
		cfg.Database = filepath.Join(filepath.Dir(abs), cfg.Database)
	}
	cfg.resolvedDatabase = cfg.Database
	return &cfg, nil
}

func (c *Config) FileValues() Config {
	value := *c
	value.ModelAliases = c.GetModelAliases()
	if c.originalDatabase != "" && c.Database == c.resolvedDatabase {
		value.Database = c.originalDatabase
	}
	return value
}

// LookupModelAlias 线程安全地按小写名称查询运行时自定义的模型路由别名。
func (c *Config) LookupModelAlias(name string) (string, bool) {
	if c == nil {
		return "", false
	}
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return "", false
	}
	modelAliasMu.RLock()
	defer modelAliasMu.RUnlock()
	if len(c.ModelAliases) == 0 {
		return "", false
	}
	target, ok := c.ModelAliases[key]
	target = strings.TrimSpace(target)
	if !ok || target == "" {
		return "", false
	}
	return target, true
}

// GetModelAliases 线程安全地返回模型别名映射的副本。
func (c *Config) GetModelAliases() map[string]string {
	if c == nil {
		return nil
	}
	modelAliasMu.RLock()
	defer modelAliasMu.RUnlock()
	if len(c.ModelAliases) == 0 {
		return nil
	}
	out := make(map[string]string, len(c.ModelAliases))
	for k, v := range c.ModelAliases {
		kl := strings.ToLower(strings.TrimSpace(k))
		vt := strings.TrimSpace(v)
		if kl != "" && vt != "" {
			out[kl] = vt
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// SetModelAliases 线程安全地规范化并更新运行时模型别名映射，使其无需重启立即生效。
func (c *Config) SetModelAliases(aliases map[string]string) {
	if c == nil {
		return
	}
	normalized := make(map[string]string, len(aliases))
	for k, v := range aliases {
		kl := strings.ToLower(strings.TrimSpace(k))
		vt := strings.TrimSpace(v)
		if kl != "" && vt != "" {
			normalized[kl] = vt
		}
	}
	modelAliasMu.Lock()
	defer modelAliasMu.Unlock()
	if len(normalized) == 0 {
		c.ModelAliases = nil
	} else {
		c.ModelAliases = normalized
	}
}

func write(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	file, err := os.CreateTemp(filepath.Dir(path), ".quickproxy-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(body); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func (c *Config) Save() error {
	if c.Path == "" {
		return errors.New("config path is empty")
	}
	if err := c.Validate(); err != nil {
		return err
	}
	value := c.FileValues()
	return write(c.Path, &value)
}

func applyDefaults(c *Config) {
	d := Default()
	if strings.TrimSpace(c.Addr) == "" {
		c.Addr = d.Addr
	}
	if strings.TrimSpace(c.Database) == "" {
		c.Database = d.Database
	}
	if len(c.Models) == 0 {
		c.Models = d.Models
	}
	if strings.TrimSpace(c.DefaultModel) == "" {
		c.DefaultModel = d.DefaultModel
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = d.RequestTimeout
	}
	if c.MaxThinkingTimeout <= 0 {
		c.MaxThinkingTimeout = d.MaxThinkingTimeout
	}
	if c.RefreshBefore < 0 {
		c.RefreshBefore = d.RefreshBefore
	}
	if c.SessionTTL <= 0 {
		c.SessionTTL = d.SessionTTL
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = d.MaxAttempts
	}
	if strings.TrimSpace(c.AppVersion) == "" {
		c.AppVersion = d.AppVersion
	}
	if strings.TrimSpace(c.Surface) == "" {
		c.Surface = d.Surface
	}
	if strings.TrimSpace(c.Timezone) == "" {
		c.Timezone = d.Timezone
	}
	if strings.TrimSpace(c.Locale) == "" {
		c.Locale = d.Locale
	}
	if strings.TrimSpace(c.OAuth.Provider) == "" {
		c.OAuth.Provider = d.OAuth.Provider
	}
	if strings.TrimSpace(c.OAuth.BuildStage) == "" {
		c.OAuth.BuildStage = d.OAuth.BuildStage
	}
	if c.OAuth.CallbackPort <= 0 {
		c.OAuth.CallbackPort = d.OAuth.CallbackPort
	}
	if c.OAuth.Timeout <= 0 {
		c.OAuth.Timeout = d.OAuth.Timeout
	}
	if c.WebUI.SessionTTL <= 0 {
		c.WebUI.SessionTTL = d.WebUI.SessionTTL
	}
	if c.Usage.RefreshInterval <= 0 {
		c.Usage.RefreshInterval = d.Usage.RefreshInterval
	}
	if c.Usage.MaxAttempts <= 0 {
		c.Usage.MaxAttempts = d.Usage.MaxAttempts
	}
	if c.Guardrails.CompactDowngrade == nil {
		c.Guardrails.CompactDowngrade = boolPtr(true)
	}
	if c.Guardrails.NoToolMaxCap == nil {
		c.Guardrails.NoToolMaxCap = boolPtr(true)
	}
	if c.Guardrails.PreserveEnvSummary == nil {
		c.Guardrails.PreserveEnvSummary = boolPtr(true)
	}
	if strings.TrimSpace(c.Updater.Repo) == "" {
		c.Updater.Repo = d.Updater.Repo
	} else {
		c.Updater.Repo = strings.TrimSpace(c.Updater.Repo)
	}
	if strings.TrimSpace(c.Updater.Mode) == "" {
		c.Updater.Mode = d.Updater.Mode
	} else {
		c.Updater.Mode = strings.ToLower(strings.TrimSpace(c.Updater.Mode))
	}
	c.Updater.CustomMirror = strings.TrimRight(strings.TrimSpace(c.Updater.CustomMirror), "/")
	if len(c.ModelAliases) > 0 {
		c.SetModelAliases(c.ModelAliases)
	}
}

func (c *Config) Validate() error {
	if c.RequestTimeout <= 0 {
		return fmt.Errorf("request_timeout must be positive")
	}
	if c.MaxThinkingTimeout <= 0 {
		return fmt.Errorf("max_thinking_timeout must be positive")
	}
	if c.OAuth.CallbackPort < 0 || c.OAuth.CallbackPort > 65535 {
		return fmt.Errorf("oauth.callback_port must be 0-65535")
	}
	if c.WebUI.SessionTTL < Duration(time.Minute) || c.WebUI.SessionTTL > Duration(7*24*time.Hour) {
		return fmt.Errorf("webui.session_ttl must be between 1m and 168h")
	}
	if c.Usage.RefreshInterval < Duration(time.Minute) || c.Usage.RefreshInterval > Duration(24*time.Hour) {
		return fmt.Errorf("usage.refresh_interval must be between 1m and 24h")
	}
	if c.Usage.MaxAttempts < 1 || c.Usage.MaxAttempts > 5 {
		return fmt.Errorf("usage.max_attempts must be between 1 and 5")
	}
	switch strings.ToLower(c.OAuth.Provider) {
	case "social", "enterprise", "gateway", "social_gateway":
	default:
		return fmt.Errorf("oauth.provider must be social, enterprise, gateway or social_gateway")
	}
	switch strings.ToLower(c.OAuth.BuildStage) {
	case "prod", "beta", "integ", "dev":
	default:
		return fmt.Errorf("oauth.build_stage must be prod, beta, integ or dev")
	}
	if err := ValidateUpdater(c.Updater); err != nil {
		return err
	}
	return nil
}
