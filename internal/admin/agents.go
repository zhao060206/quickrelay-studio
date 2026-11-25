package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	quickRelayClaudeDesktopPresetID   = "00000000-0000-4000-8000-000000008080"
	quickRelayClaudeDesktopPresetName = "QuickRelay Studio"
	maxAgentBackupsKeep               = 10
)

var validBackupIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ClaudeDesktopModelItem 表示单条从 Claude Code 桌面端别名到 QuickRelay 后端模型的映射规则。
type ClaudeDesktopModelItem struct {
	Model         string `json:"model"`
	Alias         string `json:"alias"`
	Tier          string `json:"tier,omitempty"`
	LabelOverride string `json:"label_override,omitempty"`
	Context1M     bool   `json:"context_1m"`
}

// ClaudeDesktopInstallInfo 描述本机 Claude 桌面客户端的安装探测结果。
type ClaudeDesktopInstallInfo struct {
	Installed     bool   `json:"installed"`
	InstallKind   string `json:"install_kind"` // "msix" | "exe" | "config_only" | "none"
	InstallDetail string `json:"install_detail"`
	LaunchTarget  string `json:"launch_target,omitempty"`
}

// AgentSummary 描述一个受支持的外部智能体接入概况，供列表接口返回及后续扩展多智能体。
type AgentSummary struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	Installed      bool   `json:"installed"`
	InstallKind    string `json:"install_kind"`
	Connected      bool   `json:"connected"`
	ConnectionText string `json:"connection_text"`
	AppliedPreset  string `json:"applied_preset"`
	BackupCount    int    `json:"backup_count"`
	HasOriginal    bool   `json:"has_original"`
}

type agentBackupFileSnapshot struct {
	Key     string `json:"key"`
	Path    string `json:"path"`
	Existed bool   `json:"existed"`
	Content string `json:"content,omitempty"`
}

type agentBackupPackage struct {
	ID        string                    `json:"id"`
	Kind      string                    `json:"kind"` // "original" | "snapshot"
	CreatedAt int64                     `json:"created_at"`
	Summary   string                    `json:"summary"`
	Files     []agentBackupFileSnapshot `json:"files"`
}

type claudeDesktopMappingStore struct {
	Version      int                      `json:"version"`
	UpdatedAt    int64                    `json:"updated_at"`
	BaseURL      string                   `json:"base_url"`
	ModelsHash   string                   `json:"models_hash"`
	Models       []ClaudeDesktopModelItem `json:"models"`
	ManagedAlias []string                 `json:"managed_aliases"`
}

type claudeDesktopTargetPaths struct {
	LocalAppData  string
	MainConfig    string
	ThreePDir     string
	ThreePConfig  string
	ConfigLibrary string
	MetaIndex     string
	GatewayConfig string
}

type claudeDesktopApplyInput struct {
	Models  []ClaudeDesktopModelItem `json:"models"`
	BaseURL string                   `json:"base_url"`
	APIKey  string                   `json:"api_key"`
}

type agentBackupListItem struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	CreatedAt int64  `json:"created_at"`
	Summary   string `json:"summary"`
}

// ClaudeDesktopDetailResponse 返回 Claude Code 桌面端配置的完整详情，供前端控制台渲染。
type ClaudeDesktopDetailResponse struct {
	Summary       AgentSummary               `json:"summary"`
	Install       ClaudeDesktopInstallInfo   `json:"install"`
	PresetID      string                     `json:"preset_id"`
	PresetName    string                     `json:"preset_name"`
	DefaultURL    string                     `json:"default_base_url"`
	BaseURL       string                     `json:"base_url"`
	APIKeyMasked  string                     `json:"api_key_masked"`
	HasLocalKey   bool                       `json:"has_local_key"`
	KeySealed     bool                       `json:"key_sealed"`
	Recommended   []ClaudeDesktopModelItem   `json:"recommended_models"`
	Models        []ClaudeDesktopModelItem   `json:"models"`
	TargetFiles   map[string]string          `json:"target_files"`
	TargetExists  map[string]bool            `json:"target_exists"`
	OtherPresets  []map[string]string        `json:"other_presets"`
	Backups       []agentBackupListItem      `json:"backups"`
	ModelAliases  map[string]string          `json:"model_aliases"`
}

func defaultLocalAppDataDir() string {
	if v := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, "AppData", "Local")
	}
	return ""
}

func (s *Server) getLocalAppDataDir() (string, error) {
	var dir string
	if s.localAppDataDir != nil {
		dir = strings.TrimSpace(s.localAppDataDir())
	} else {
		dir = defaultLocalAppDataDir()
	}
	if dir == "" {
		return "", errors.New("cannot resolve LOCALAPPDATA directory")
	}
	return dir, nil
}

func (s *Server) resolveClaudeDesktopPaths() (claudeDesktopTargetPaths, error) {
	localDir, err := s.getLocalAppDataDir()
	if err != nil {
		return claudeDesktopTargetPaths{}, err
	}
	threePDir := filepath.Join(localDir, "Claude-3p")
	libDir := filepath.Join(threePDir, "configLibrary")
	return claudeDesktopTargetPaths{
		LocalAppData:  localDir,
		MainConfig:    filepath.Join(localDir, "Claude", "claude_desktop_config.json"),
		ThreePDir:     threePDir,
		ThreePConfig:  filepath.Join(threePDir, "claude_desktop_config.json"),
		ConfigLibrary: libDir,
		MetaIndex:     filepath.Join(libDir, "_meta.json"),
		GatewayConfig: filepath.Join(libDir, quickRelayClaudeDesktopPresetID+".json"),
	}, nil
}

func (s *Server) agentStateDir(agentID string) string {
	dataRoot := "data"
	if s.cfg != nil && strings.TrimSpace(s.cfg.Database) != "" {
		dataRoot = filepath.Dir(s.cfg.Database)
	}
	return filepath.Join(dataRoot, "agents", agentID)
}

func defaultClaudeDesktopModels() []ClaudeDesktopModelItem {
	return []ClaudeDesktopModelItem{
		{
			Model:         "smart",
			Alias:         "claude-opus-5",
			Tier:          "opus",
			LabelOverride: "QuickRelay Smart",
			Context1M:     true,
		},
		{
			Model:         "balanced",
			Alias:         "claude-sonnet-5",
			Tier:          "sonnet",
			LabelOverride: "QuickRelay Balanced",
			Context1M:     true,
		},
		{
			Model:         "fast",
			Alias:         "claude-haiku-4-5",
			Tier:          "haiku",
			LabelOverride: "QuickRelay Fast",
			Context1M:     false,
		},
	}
}

func inferClaudeFamilyTier(alias, explicit string) string {
	switch strings.ToLower(strings.TrimSpace(explicit)) {
	case "opus", "sonnet", "haiku":
		return strings.ToLower(strings.TrimSpace(explicit))
	}
	lower := strings.ToLower(strings.TrimSpace(alias))
	switch {
	case strings.Contains(lower, "haiku"), strings.Contains(lower, "fast"):
		return "haiku"
	case strings.Contains(lower, "sonnet"), strings.Contains(lower, "balanced"):
		return "sonnet"
	default:
		return "opus"
	}
}

func (s *Server) defaultGatewayBaseURL() string {
	addr := "127.0.0.1:8080"
	if s.cfg != nil && strings.TrimSpace(s.cfg.Addr) != "" {
		addr = strings.TrimSpace(s.cfg.Addr)
	}
	if strings.HasPrefix(addr, ":") {
		return "http://127.0.0.1" + addr
	}
	host, port, err := net.SplitHostPort(addr)
	if err == nil && port != "" {
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		return "http://" + net.JoinHostPort(host, port)
	}
	return "http://" + addr
}

func maskSecret(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + "***" + s[len(s)-4:]
}

func normalizeClaudeDesktopModels(raw []ClaudeDesktopModelItem) ([]ClaudeDesktopModelItem, error) {
	if len(raw) == 0 {
		return nil, errors.New("at least one model mapping must be configured")
	}
	seenAlias := make(map[string]bool, len(raw))
	out := make([]ClaudeDesktopModelItem, 0, len(raw))
	for i, item := range raw {
		alias := strings.TrimSpace(item.Alias)
		model := strings.TrimSpace(item.Model)
		if alias == "" {
			return nil, fmt.Errorf("row %d: desktop model alias cannot be empty", i+1)
		}
		if model == "" {
			return nil, fmt.Errorf("row %d: target QuickRelay model cannot be empty", i+1)
		}
		lowerAlias := strings.ToLower(alias)
		if seenAlias[lowerAlias] {
			return nil, fmt.Errorf("duplicate desktop model alias %q", alias)
		}
		seenAlias[lowerAlias] = true
		label := strings.TrimSpace(item.LabelOverride)
		if label == "" {
			label = model + " (QuickRelay)"
		}
		out = append(out, ClaudeDesktopModelItem{
			Model:         model,
			Alias:         alias,
			Tier:          inferClaudeFamilyTier(alias, item.Tier),
			LabelOverride: label,
			Context1M:     item.Context1M,
		})
	}
	return out, nil
}

func detectClaudeDesktopInstall(paths claudeDesktopTargetPaths) ClaudeDesktopInstallInfo {
	exeCandidates := []string{
		filepath.Join(paths.LocalAppData, "AnthropicClaude", "Claude.exe"),
		filepath.Join(paths.LocalAppData, "Programs", "Claude", "Claude.exe"),
		filepath.Join(paths.LocalAppData, "Claude", "Claude.exe"),
	}
	for _, p := range exeCandidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return ClaudeDesktopInstallInfo{
				Installed:     true,
				InstallKind:   "exe",
				InstallDetail: p,
				LaunchTarget:  p,
			}
		}
	}

	pkgRoot := filepath.Join(paths.LocalAppData, "Packages")
	if entries, err := os.ReadDir(pkgRoot); err == nil {
		for _, ent := range entries {
			if !ent.IsDir() || !strings.HasPrefix(ent.Name(), "Claude_") {
				continue
			}
			pfn := ent.Name()
			return ClaudeDesktopInstallInfo{
				Installed:     true,
				InstallKind:   "msix",
				InstallDetail: pfn,
				LaunchTarget:  `shell:AppsFolder\` + pfn + `!Claude`,
			}
		}
	}

	for _, dir := range []string{filepath.Dir(paths.MainConfig), paths.ThreePDir} {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return ClaudeDesktopInstallInfo{
				Installed:     false,
				InstallKind:   "config_only",
				InstallDetail: dir,
			}
		}
	}
	return ClaudeDesktopInstallInfo{
		Installed:     false,
		InstallKind:   "none",
		InstallDetail: "",
	}
}

func readJSONObjectOrEmpty(path string) (map[string]any, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return map[string]any{}, true, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, true, fmt.Errorf("parse %s: %w", path, err)
	}
	if obj == nil {
		obj = map[string]any{}
	}
	return obj, true, nil
}

func writeAtomicFile(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".qr-agent-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func marshalPrettyJSON(v any) ([]byte, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func (s *Server) buildClaudeDesktopState(paths claudeDesktopTargetPaths) ClaudeDesktopDetailResponse {
	install := detectClaudeDesktopInstall(paths)
	stateDir := s.agentStateDir("claude-desktop")

	// 检查本地持久化映射与网关配置
	metaObj, metaExists, _ := readJSONObjectOrEmpty(paths.MetaIndex)
	gwObj, gwExists, _ := readJSONObjectOrEmpty(paths.GatewayConfig)
	mainObj, mainExists, _ := readJSONObjectOrEmpty(paths.MainConfig)
	threePObj, threePExists, _ := readJSONObjectOrEmpty(paths.ThreePConfig)

	appliedID, _ := metaObj["appliedId"].(string)
	appliedPresetName := ""
	var otherPresets []map[string]string
	if rawEntries, ok := metaObj["entries"].([]any); ok {
		for _, rawItem := range rawEntries {
			itemMap, ok := rawItem.(map[string]any)
			if !ok {
				continue
			}
			id, _ := itemMap["id"].(string)
			name, _ := itemMap["name"].(string)
			if id == appliedID {
				appliedPresetName = name
			}
			if id != "" && id != quickRelayClaudeDesktopPresetID {
				otherPresets = append(otherPresets, map[string]string{
					"id":   id,
					"name": name,
				})
			}
		}
	}

	mainMode, _ := mainObj["deploymentMode"].(string)
	threePMode, _ := threePObj["deploymentMode"].(string)
	connected := appliedID == quickRelayClaudeDesktopPresetID && gwExists && mainMode == "3p" && threePMode == "3p"

	connText := "未接入（尚未配置 3P 网关）"
	if connected {
		connText = "已接入 QuickRelay Studio"
	} else if appliedID != "" {
		label := appliedPresetName
		if label == "" {
			label = appliedID
		}
		connText = "当前使用其它预设: " + label
	} else if gwExists {
		connText = "已写入预设（当前处于停用状态）"
	}

	// 解析当前模型的首选草案
	defaultURL := s.defaultGatewayBaseURL()
	baseURL := defaultURL
	if gwURL, ok := gwObj["inferenceGatewayBaseUrl"].(string); ok && strings.TrimSpace(gwURL) != "" {
		baseURL = strings.TrimSpace(gwURL)
	}
	gwKey, _ := gwObj["inferenceGatewayApiKey"].(string)
	keySealed := strings.HasPrefix(gwKey, "__sealed__:")
	maskedKey := ""
	if keySealed {
		maskedKey = "__sealed__:v1:*** (已由 Claude 桌面端本机加密)"
	} else if gwKey != "" {
		maskedKey = maskSecret(gwKey)
	}

	models := defaultClaudeDesktopModels()
	mappingPath := filepath.Join(stateDir, "current-mapping.json")
	if rawMap, err := os.ReadFile(mappingPath); err == nil {
		var saved claudeDesktopMappingStore
		if json.Unmarshal(rawMap, &saved) == nil && len(saved.Models) > 0 {
			models = saved.Models
			if strings.TrimSpace(saved.BaseURL) != "" {
				baseURL = strings.TrimSpace(saved.BaseURL)
			}
		}
	}

	// 读取备份目录摘要
	var backups []agentBackupListItem
	hasOriginal := false
	origPath := filepath.Join(stateDir, "original-backup.json")
	if rawOrig, err := os.ReadFile(origPath); err == nil {
		var pkg agentBackupPackage
		if json.Unmarshal(rawOrig, &pkg) == nil {
			hasOriginal = true
			backups = append(backups, agentBackupListItem{
				ID:        "original",
				Kind:      "original",
				CreatedAt: pkg.CreatedAt,
				Summary:   pkg.Summary,
			})
		}
	}
	backupDir := filepath.Join(stateDir, "backups")
	if ents, err := os.ReadDir(backupDir); err == nil {
		var snapshots []agentBackupListItem
		for _, ent := range ents {
			if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
				continue
			}
			rawPkg, err := os.ReadFile(filepath.Join(backupDir, ent.Name()))
			if err != nil {
				continue
			}
			var pkg agentBackupPackage
			if json.Unmarshal(rawPkg, &pkg) != nil {
				continue
			}
			snapshots = append(snapshots, agentBackupListItem{
				ID:        pkg.ID,
				Kind:      pkg.Kind,
				CreatedAt: pkg.CreatedAt,
				Summary:   pkg.Summary,
			})
		}
		sort.Slice(snapshots, func(i, j int) bool {
			return snapshots[i].CreatedAt > snapshots[j].CreatedAt
		})
		backups = append(backups, snapshots...)
	}

	hasLocalKey := s.cfg != nil && len(s.cfg.APIKeys) > 0
	var aliases map[string]string
	if s.cfg != nil {
		aliases = s.cfg.GetModelAliases()
	}

	return ClaudeDesktopDetailResponse{
		Summary: AgentSummary{
			ID:             "claude-desktop",
			Name:           "Claude Code 桌面端 (Claude Desktop)",
			Description:    "以 3P 网关模式接管 Windows 版 Claude Desktop / Cowork 桌面应用模型流量",
			Installed:      install.Installed,
			InstallKind:    install.InstallKind,
			Connected:      connected,
			ConnectionText: connText,
			AppliedPreset:  appliedPresetName,
			BackupCount:    len(backups),
			HasOriginal:    hasOriginal,
		},
		Install:      install,
		PresetID:     quickRelayClaudeDesktopPresetID,
		PresetName:   quickRelayClaudeDesktopPresetName,
		DefaultURL:   defaultURL,
		BaseURL:      baseURL,
		APIKeyMasked: maskedKey,
		HasLocalKey:  hasLocalKey,
		KeySealed:    keySealed,
		Recommended:  defaultClaudeDesktopModels(),
		Models:       models,
		TargetFiles: map[string]string{
			"main_config":    paths.MainConfig,
			"three_p_config": paths.ThreePConfig,
			"meta_index":     paths.MetaIndex,
			"gateway_config": paths.GatewayConfig,
		},
		TargetExists: map[string]bool{
			"main_config":    mainExists,
			"three_p_config": threePExists,
			"meta_index":     metaExists,
			"gateway_config": gwExists,
		},
		OtherPresets: otherPresets,
		Backups:      backups,
		ModelAliases: aliases,
	}
}

func (s *Server) buildGatewayPayload(baseURL, apiKey string, models []ClaudeDesktopModelItem) map[string]any {
	tierDefaultSeen := map[string]bool{}
	infModels := make([]map[string]any, 0, len(models))
	for _, m := range models {
		tier := inferClaudeFamilyTier(m.Alias, m.Tier)
		isDefault := !tierDefaultSeen[tier]
		tierDefaultSeen[tier] = true

		entry := map[string]any{
			"anthropicFamilyTier": tier,
			"isFamilyDefault":     isDefault,
			"labelOverride":       m.LabelOverride,
			"name":                m.Alias,
		}
		if m.Context1M {
			entry["prefer1m"] = true
			entry["supports1m"] = true
		}
		infModels = append(infModels, entry)
	}

	return map[string]any{
		"bootstrapEnabled":             false,
		"disableDeploymentModeChooser": true,
		"disableFeatureDiscovery":      true,
		"inferenceCredentialKind":      "static",
		"inferenceGatewayApiKey":       apiKey,
		"inferenceGatewayAuthScheme":   "bearer",
		"inferenceGatewayBaseUrl":      strings.TrimRight(baseURL, "/"),
		"inferenceModels":              infModels,
		"inferenceProvider":            "gateway",
		"modelDiscoveryEnabled":        false,
	}
}

func (s *Server) resolveApplyParams(input claudeDesktopApplyInput, existingKey string) (string, string, []ClaudeDesktopModelItem, error) {
	rawModels := input.Models
	if len(rawModels) == 0 {
		rawModels = defaultClaudeDesktopModels()
	}
	models, err := normalizeClaudeDesktopModels(rawModels)
	if err != nil {
		return "", "", nil, err
	}
	baseURL := strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")
	if baseURL == "" {
		baseURL = s.defaultGatewayBaseURL()
	}
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return "", "", nil, errors.New("base_url must start with http:// or https://")
	}

	apiKey := strings.TrimSpace(input.APIKey)
	if apiKey == "" {
		if s.cfg != nil && len(s.cfg.APIKeys) > 0 && strings.TrimSpace(s.cfg.APIKeys[0]) != "" {
			apiKey = strings.TrimSpace(s.cfg.APIKeys[0])
		} else if strings.TrimSpace(existingKey) != "" {
			apiKey = strings.TrimSpace(existingKey)
		} else {
			apiKey = "quickrelay-local"
		}
	}
	return baseURL, apiKey, models, nil
}

func captureTargetSnapshots(paths claudeDesktopTargetPaths) ([]agentBackupFileSnapshot, error) {
	items := []struct {
		key  string
		path string
	}{
		{"main_config", paths.MainConfig},
		{"three_p_config", paths.ThreePConfig},
		{"meta_index", paths.MetaIndex},
		{"gateway_config", paths.GatewayConfig},
	}
	snaps := make([]agentBackupFileSnapshot, 0, len(items))
	for _, it := range items {
		raw, err := os.ReadFile(it.path)
		if errors.Is(err, os.ErrNotExist) {
			snaps = append(snaps, agentBackupFileSnapshot{Key: it.key, Path: it.path, Existed: false})
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s for snapshot: %w", it.path, err)
		}
		snaps = append(snaps, agentBackupFileSnapshot{
			Key:     it.key,
			Path:    it.path,
			Existed: true,
			Content: string(raw),
		})
	}
	return snaps, nil
}

func restoreSnapshots(snaps []agentBackupFileSnapshot) error {
	for _, snap := range snaps {
		if !snap.Existed {
			if err := os.Remove(snap.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove %s: %w", snap.Path, err)
			}
			continue
		}
		if err := writeAtomicFile(snap.Path, []byte(snap.Content)); err != nil {
			return fmt.Errorf("restore %s: %w", snap.Path, err)
		}
	}
	return nil
}

func pruneAgentSnapshots(backupDir string, keep int) {
	ents, err := os.ReadDir(backupDir)
	if err != nil {
		return
	}
	var files []os.FileInfo
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		if info, err := ent.Info(); err == nil {
			files = append(files, info)
		}
	}
	if len(files) <= keep {
		return
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].ModTime().After(files[j].ModTime())
	})
	for _, f := range files[keep:] {
		_ = os.Remove(filepath.Join(backupDir, f.Name()))
	}
}

func (s *Server) syncAliasesWithMapping(nextManaged map[string]string, prevManaged []string) error {
	if s.cfg == nil {
		return nil
	}
	merged := s.cfg.GetModelAliases()
	if merged == nil {
		merged = make(map[string]string)
	}
	for _, oldKey := range prevManaged {
		delete(merged, strings.ToLower(strings.TrimSpace(oldKey)))
	}
	for k, v := range nextManaged {
		merged[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	s.cfg.SetModelAliases(merged)

	s.configMu.Lock()
	s.saved.SetModelAliases(merged)
	s.configMu.Unlock()

	if strings.TrimSpace(s.cfg.Path) != "" {
		if err := s.cfg.Save(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleAgentsList(w http.ResponseWriter, r *http.Request) {
	s.agentsMu.Lock()
	defer s.agentsMu.Unlock()
	paths, err := s.resolveClaudeDesktopPaths()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "agent_paths_error", err.Error())
		return
	}
	detail := s.buildClaudeDesktopState(paths)
	writeJSON(w, http.StatusOK, map[string]any{
		"agents": []AgentSummary{detail.Summary},
	})
}

func (s *Server) handleClaudeDesktopGet(w http.ResponseWriter, r *http.Request) {
	s.agentsMu.Lock()
	defer s.agentsMu.Unlock()
	paths, err := s.resolveClaudeDesktopPaths()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "agent_paths_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.buildClaudeDesktopState(paths))
}

func (s *Server) handleClaudeDesktopPreview(w http.ResponseWriter, r *http.Request) {
	var input claudeDesktopApplyInput
	if !decodeJSON(w, r, &input, 256<<10) {
		return
	}

	s.agentsMu.Lock()
	defer s.agentsMu.Unlock()
	paths, err := s.resolveClaudeDesktopPaths()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "agent_paths_error", err.Error())
		return
	}
	existingGw, _, _ := readJSONObjectOrEmpty(paths.GatewayConfig)
	existingKey, _ := existingGw["inferenceGatewayApiKey"].(string)

	baseURL, apiKey, models, err := s.resolveApplyParams(input, existingKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_agent_config", err.Error())
		return
	}

	gwPreview := s.buildGatewayPayload(baseURL, maskSecret(apiKey), models)
	aliasMap := make(map[string]string, len(models))
	for _, m := range models {
		aliasMap[strings.ToLower(m.Alias)] = m.Model
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"base_url": baseURL,
		"models":   models,
		"target_files": map[string]string{
			"main_config":    paths.MainConfig,
			"three_p_config": paths.ThreePConfig,
			"meta_index":     paths.MetaIndex,
			"gateway_config": paths.GatewayConfig,
		},
		"gateway_preview": gwPreview,
		"model_aliases":   aliasMap,
		"changes": []string{
			"启用 Claude 主配置与 3P 配置的 deploymentMode = \"3p\"",
			"在 _meta.json 保留其它预设并将 appliedId 切换为 QuickRelay Studio (" + quickRelayClaudeDesktopPresetID + ")",
			"写入或更新 Gateway 网关映射文件（关闭官方发现引导并注册本地路由模型）",
			"将别名路由写入 QuickRelay config.json.model_aliases 并立即热生效",
		},
	})
}

func (s *Server) handleClaudeDesktopApply(w http.ResponseWriter, r *http.Request) {
	var input claudeDesktopApplyInput
	if !decodeJSON(w, r, &input, 256<<10) {
		return
	}

	s.agentsMu.Lock()
	defer s.agentsMu.Unlock()

	paths, err := s.resolveClaudeDesktopPaths()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "agent_paths_error", err.Error())
		return
	}
	existingGw, _, _ := readJSONObjectOrEmpty(paths.GatewayConfig)
	existingKey, _ := existingGw["inferenceGatewayApiKey"].(string)

	baseURL, apiKey, models, err := s.resolveApplyParams(input, existingKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_agent_config", err.Error())
		return
	}

	// 1. 先解析并准备合并后的 4 份文件内容，校验旧 JSON 无语法破坏
	mainObj, _, err := readJSONObjectOrEmpty(paths.MainConfig)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_target_json", err.Error())
		return
	}
	mainObj["deploymentMode"] = "3p"

	threePObj, _, err := readJSONObjectOrEmpty(paths.ThreePConfig)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_target_json", err.Error())
		return
	}
	threePObj["deploymentMode"] = "3p"

	metaObj, _, err := readJSONObjectOrEmpty(paths.MetaIndex)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_target_json", err.Error())
		return
	}
	var preserved []map[string]any
	if rawEntries, ok := metaObj["entries"].([]any); ok {
		for _, item := range rawEntries {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			id, _ := m["id"].(string)
			if id == "" || id == quickRelayClaudeDesktopPresetID {
				continue
			}
			preserved = append(preserved, m)
		}
	}
	preserved = append(preserved, map[string]any{
		"id":   quickRelayClaudeDesktopPresetID,
		"name": quickRelayClaudeDesktopPresetName,
	})
	metaObj["appliedId"] = quickRelayClaudeDesktopPresetID
	metaObj["entries"] = preserved

	gwObj := s.buildGatewayPayload(baseURL, apiKey, models)

	bMain, err := marshalPrettyJSON(mainObj)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "marshal_error", err.Error())
		return
	}
	bThreeP, err := marshalPrettyJSON(threePObj)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "marshal_error", err.Error())
		return
	}
	bMeta, err := marshalPrettyJSON(metaObj)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "marshal_error", err.Error())
		return
	}
	bGateway, err := marshalPrettyJSON(gwObj)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "marshal_error", err.Error())
		return
	}

	// 2. 写入前抓取 4 个文件的当前快照，保存原始备份（若首次）及本次快照
	rollbackSnaps, err := captureTargetSnapshots(paths)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "snapshot_error", err.Error())
		return
	}

	stateDir := s.agentStateDir("claude-desktop")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "state_dir_error", err.Error())
		return
	}

	now := time.Now()
	origPath := filepath.Join(stateDir, "original-backup.json")
	if _, statErr := os.Stat(origPath); errors.Is(statErr, os.ErrNotExist) {
		origPkg := agentBackupPackage{
			ID:        "original",
			Kind:      "original",
			CreatedAt: now.Unix(),
			Summary:   "接入 QuickRelay Studio 前的初始状态备份",
			Files:     rollbackSnaps,
		}
		if rawOrig, mErr := marshalPrettyJSON(origPkg); mErr == nil {
			_ = writeAtomicFile(origPath, rawOrig)
		}
	} else {
		backupDir := filepath.Join(stateDir, "backups")
		_ = os.MkdirAll(backupDir, 0o755)
		snapID := now.Format("20060102-150405")
		pkg := agentBackupPackage{
			ID:        snapID,
			Kind:      "snapshot",
			CreatedAt: now.Unix(),
			Summary:   fmt.Sprintf("应用 %d 条映射前备份 (%s)", len(models), baseURL),
			Files:     rollbackSnaps,
		}
		if rawPkg, mErr := marshalPrettyJSON(pkg); mErr == nil {
			_ = writeAtomicFile(filepath.Join(backupDir, snapID+".json"), rawPkg)
			pruneAgentSnapshots(backupDir, maxAgentBackupsKeep)
		}
	}

	// 3. 事务写入目标文件，任意失败立即按快照回滚
	writes := []struct {
		path string
		data []byte
	}{
		{paths.MainConfig, bMain},
		{paths.ThreePConfig, bThreeP},
		{paths.MetaIndex, bMeta},
		{paths.GatewayConfig, bGateway},
	}
	for _, wItem := range writes {
		if wErr := writeAtomicFile(wItem.path, wItem.data); wErr != nil {
			_ = restoreSnapshots(rollbackSnaps)
			writeError(w, http.StatusInternalServerError, "agent_write_failed", fmt.Sprintf("写入失败并已回滚 %s: %v", wItem.path, wErr))
			return
		}
	}

	// 4. 更新代理本地 mapping 持久化与动态 ModelAliases
	mappingPath := filepath.Join(stateDir, "current-mapping.json")
	var prevManaged []string
	if prevRaw, err := os.ReadFile(mappingPath); err == nil {
		var prev claudeDesktopMappingStore
		if json.Unmarshal(prevRaw, &prev) == nil {
			prevManaged = prev.ManagedAlias
		}
	}

	nextAliases := make(map[string]string, len(models))
	managedList := make([]string, 0, len(models))
	for _, m := range models {
		lowerAlias := strings.ToLower(strings.TrimSpace(m.Alias))
		nextAliases[lowerAlias] = m.Model
		managedList = append(managedList, lowerAlias)
	}
	if err := s.syncAliasesWithMapping(nextAliases, prevManaged); err != nil {
		_ = restoreSnapshots(rollbackSnaps)
		writeError(w, http.StatusInternalServerError, "config_save_failed", "同步模型别名到 config.json 失败并已回滚: "+err.Error())
		return
	}

	hashRaw, _ := json.Marshal(models)
	sum := sha256.Sum256(hashRaw)
	mappingDoc := claudeDesktopMappingStore{
		Version:      1,
		UpdatedAt:    now.Unix(),
		BaseURL:      baseURL,
		ModelsHash:   hex.EncodeToString(sum[:]),
		Models:       models,
		ManagedAlias: managedList,
	}
	if bMap, err := marshalPrettyJSON(mappingDoc); err == nil {
		_ = writeAtomicFile(mappingPath, bMap)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"applied": true,
		"detail":  s.buildClaudeDesktopState(paths),
	})
}

func (s *Server) handleClaudeDesktopDisable(w http.ResponseWriter, r *http.Request) {
	s.agentsMu.Lock()
	defer s.agentsMu.Unlock()

	paths, err := s.resolveClaudeDesktopPaths()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "agent_paths_error", err.Error())
		return
	}
	metaObj, existed, err := readJSONObjectOrEmpty(paths.MetaIndex)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_target_json", err.Error())
		return
	}
	if !existed {
		writeJSON(w, http.StatusOK, map[string]any{
			"disabled": true,
			"detail":   s.buildClaudeDesktopState(paths),
		})
		return
	}

	fallbackID := ""
	if rawEntries, ok := metaObj["entries"].([]any); ok {
		for _, item := range rawEntries {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			id, _ := m["id"].(string)
			if id != "" && id != quickRelayClaudeDesktopPresetID {
				fallbackID = id
			}
		}
	}
	if fallbackID != "" {
		metaObj["appliedId"] = fallbackID
	} else {
		delete(metaObj, "appliedId")
	}
	rawMeta, err := marshalPrettyJSON(metaObj)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "marshal_error", err.Error())
		return
	}
	if err := writeAtomicFile(paths.MetaIndex, rawMeta); err != nil {
		writeError(w, http.StatusInternalServerError, "agent_write_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"disabled":        true,
		"restored_preset": fallbackID,
		"detail":          s.buildClaudeDesktopState(paths),
	})
}

func (s *Server) handleClaudeDesktopRestore(w http.ResponseWriter, r *http.Request) {
	var input struct {
		BackupID string `json:"backup_id"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if !decodeJSON(w, r, &input, 64<<10) {
			return
		}
	}

	s.agentsMu.Lock()
	defer s.agentsMu.Unlock()

	paths, err := s.resolveClaudeDesktopPaths()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "agent_paths_error", err.Error())
		return
	}

	stateDir := s.agentStateDir("claude-desktop")
	targetID := strings.TrimSpace(input.BackupID)
	if targetID == "" {
		targetID = "original"
	}
	if targetID != "original" && !validBackupIDPattern.MatchString(targetID) {
		writeError(w, http.StatusBadRequest, "invalid_backup_id", "非法的备份 ID 格式")
		return
	}

	backupPath := filepath.Join(stateDir, "original-backup.json")
	if targetID != "original" {
		backupPath = filepath.Join(stateDir, "backups", targetID+".json")
	}

	rawPkg, err := os.ReadFile(backupPath)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "backup_not_found", "未找到可还原的备份文件: "+targetID)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "backup_read_error", err.Error())
		return
	}
	var pkg agentBackupPackage
	if err := json.Unmarshal(rawPkg, &pkg); err != nil {
		writeError(w, http.StatusInternalServerError, "backup_corrupt", err.Error())
		return
	}

	if err := restoreSnapshots(pkg.Files); err != nil {
		writeError(w, http.StatusInternalServerError, "restore_failed", err.Error())
		return
	}

	// 若还原到初始态，清理本地 current-mapping.json 及受管的 model_aliases
	mappingPath := filepath.Join(stateDir, "current-mapping.json")
	if prevRaw, err := os.ReadFile(mappingPath); err == nil {
		var prev claudeDesktopMappingStore
		if json.Unmarshal(prevRaw, &prev) == nil && targetID == "original" {
			_ = s.syncAliasesWithMapping(nil, prev.ManagedAlias)
			_ = os.Remove(mappingPath)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"restored":  true,
		"backup_id": targetID,
		"detail":    s.buildClaudeDesktopState(paths),
	})
}
