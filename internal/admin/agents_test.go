package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"quickrelay-studio/internal/accounts"
	"quickrelay-studio/internal/config"
	"quickrelay-studio/internal/store"
)

func doAdminJSONRequest(t *testing.T, client *http.Client, method, url, csrf string, body any) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequest(method, url, &buf)
	if err != nil {
		t.Fatalf("NewRequest(%s %s): %v", method, url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do(%s %s): %v", method, url, err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	if _, err := out.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, out.Bytes()
}

// TestClaudeDesktopAgentLifecycle 在完全隔离的临时目录中完整测试 Claude Code 桌面端检测、预览、事务应用（保留原有 ECPA 与偏好）、停用与字节级还原流程。
func TestClaudeDesktopAgentLifecycle(t *testing.T) {
	t.Setenv("QUICKPROXY_ADMIN_PASSWORD", "unit-test-admin-pass-123")

	tmpRoot := t.TempDir()
	fakeLocalAppData := filepath.Join(tmpRoot, "localappdata")
	cfgPath := filepath.Join(tmpRoot, "config.json")
	dbPath := filepath.Join(tmpRoot, "data", "quickrelay.db")

	cfg := config.Default()
	cfg.Addr = "127.0.0.1:18080"
	cfg.Database = dbPath
	cfg.Path = cfgPath
	if err := cfg.Save(); err != nil {
		t.Fatalf("initial cfg.Save: %v", err)
	}

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	mgr := accounts.New(st, &cfg)
	srv, err := NewServer(&cfg, st, mgr, false)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()
	srv.localAppDataDir = func() string { return fakeLocalAppData }

	// 预置用户原始 Claude / Claude-3p 文件，模拟与现有 ECPA 共存的场景
	origMainPath := filepath.Join(fakeLocalAppData, "Claude", "claude_desktop_config.json")
	orig3pPath := filepath.Join(fakeLocalAppData, "Claude-3p", "claude_desktop_config.json")
	origMetaPath := filepath.Join(fakeLocalAppData, "Claude-3p", "configLibrary", "_meta.json")
	gwPath := filepath.Join(fakeLocalAppData, "Claude-3p", "configLibrary", quickRelayClaudeDesktopPresetID+".json")

	origMainBytes := []byte("{\n  \"deploymentMode\": \"anthropic\",\n  \"customTopFlag\": true\n}\n")
	orig3pBytes := []byte("{\n  \"deploymentMode\": \"3p\",\n  \"coworkUserFilesPath\": \"C:\\\\Users\\\\demo\\\\Claude\",\n  \"preferences\": {\"theme\": \"dark\", \"keepMe\": 42}\n}\n")
	origMetaBytes := []byte("{\n  \"appliedId\": \"00000000-0000-4000-8000-000000831700\",\n  \"entries\": [\n    {\"id\": \"00000000-0000-4000-8000-000000831700\", \"name\": \"EasyCLIProxyAPI\"}\n  ]\n}\n")

	for _, f := range []struct {
		p string
		b []byte
	}{
		{origMainPath, origMainBytes},
		{orig3pPath, orig3pBytes},
		{origMetaPath, origMetaBytes},
	} {
		if err := os.MkdirAll(filepath.Dir(f.p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(f.p, f.b, 0o644); err != nil {
			t.Fatalf("write initial file %s: %v", f.p, err)
		}
	}
	// 同时模拟存在 MSIX 安装目录
	if err := os.MkdirAll(filepath.Join(fakeLocalAppData, "Packages", "Claude_demo123"), 0o755); err != nil {
		t.Fatalf("mkdir msix: %v", err)
	}

	ts := httptest.NewServer(srv.Handler(http.NotFoundHandler()))
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	// 登录管理端获取 Session Cookie 与 CSRF Token
	code, loginBody := doAdminJSONRequest(t, client, http.MethodPost, ts.URL+"/api/auth/login", "", map[string]string{
		"password": "unit-test-admin-pass-123",
	})
	if code != http.StatusOK {
		t.Fatalf("login failed: %d %s", code, string(loginBody))
	}
	var loginResp struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(loginBody, &loginResp); err != nil || loginResp.CSRFToken == "" {
		t.Fatalf("missing csrf_token in %s", string(loginBody))
	}
	csrf := loginResp.CSRFToken

	// 1. GET /api/agents 与 /api/agents/claude-desktop
	code, listBody := doAdminJSONRequest(t, client, http.MethodGet, ts.URL+"/api/agents", csrf, nil)
	if code != http.StatusOK || !strings.Contains(string(listBody), `"claude-desktop"`) {
		t.Fatalf("GET /api/agents unexpected: %d %s", code, string(listBody))
	}

	code, detailBody := doAdminJSONRequest(t, client, http.MethodGet, ts.URL+"/api/agents/claude-desktop", csrf, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/agents/claude-desktop failed: %d %s", code, string(detailBody))
	}
	var detail ClaudeDesktopDetailResponse
	if err := json.Unmarshal(detailBody, &detail); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	if !detail.Install.Installed || detail.Install.InstallKind != "msix" {
		t.Fatalf("expected msix install detected, got %+v", detail.Install)
	}
	if detail.Summary.Connected || detail.Summary.AppliedPreset != "EasyCLIProxyAPI" {
		t.Fatalf("unexpected initial connected/preset state: %+v", detail.Summary)
	}

	// 2. POST /api/agents/claude-desktop/preview 断言不写盘且脱敏密钥
	applyPayload := map[string]any{
		"base_url": "http://127.0.0.1:18080",
		"api_key":  "sk-secret-demo-token-9988",
		"models": []map[string]any{
			{"model": "smart", "alias": "claude-opus-5", "tier": "opus", "label_override": "QR Smart", "context_1m": true},
			{"model": "balanced", "alias": "claude-sonnet-5", "tier": "sonnet", "label_override": "QR Balanced", "context_1m": true},
			{"model": "fast", "alias": "claude-haiku-4-5", "tier": "haiku", "label_override": "QR Fast", "context_1m": false},
		},
	}
	code, prevBody := doAdminJSONRequest(t, client, http.MethodPost, ts.URL+"/api/agents/claude-desktop/preview", csrf, applyPayload)
	if code != http.StatusOK {
		t.Fatalf("preview failed: %d %s", code, string(prevBody))
	}
	if strings.Contains(string(prevBody), "sk-secret-demo-token-9988") {
		t.Fatalf("preview leaked unmasked API key: %s", string(prevBody))
	}
	if _, err := os.Stat(gwPath); !os.IsNotExist(err) {
		t.Fatalf("preview unexpectedly created gateway config file %s", gwPath)
	}

	// 3. POST /api/agents/claude-desktop/apply 验证合并写入与模型别名热生效
	code, applyResp := doAdminJSONRequest(t, client, http.MethodPost, ts.URL+"/api/agents/claude-desktop/apply", csrf, applyPayload)
	if code != http.StatusOK {
		t.Fatalf("apply failed: %d %s", code, string(applyResp))
	}

	// 检查原有 preferences 与 ECPA 条目保留
	afterMain, _ := os.ReadFile(origMainPath)
	if !strings.Contains(string(afterMain), `"deploymentMode": "3p"`) || !strings.Contains(string(afterMain), `"customTopFlag": true`) {
		t.Fatalf("main config merge corrupted: %s", string(afterMain))
	}
	after3p, _ := os.ReadFile(orig3pPath)
	if !strings.Contains(string(after3p), `"keepMe": 42`) || !strings.Contains(string(after3p), `coworkUserFilesPath`) {
		t.Fatalf("3p config lost original preferences: %s", string(after3p))
	}
	afterMeta, _ := os.ReadFile(origMetaPath)
	if !strings.Contains(string(afterMeta), quickRelayClaudeDesktopPresetID) || !strings.Contains(string(afterMeta), "00000000-0000-4000-8000-000000831700") {
		t.Fatalf("_meta.json did not preserve ECPA entry or switch appliedId: %s", string(afterMeta))
	}
	afterGw, err := os.ReadFile(gwPath)
	if err != nil {
		t.Fatalf("gateway file missing after apply: %v", err)
	}
	if !strings.Contains(string(afterGw), `"isFamilyDefault": true`) || !strings.Contains(string(afterGw), `"supports1m": true`) {
		t.Fatalf("gateway config missing expected ECPA-compatible fields: %s", string(afterGw))
	}

	// 验证内存与磁盘 config.json 模型别名已更新
	if target, ok := cfg.LookupModelAlias("claude-sonnet-5"); !ok || target != "balanced" {
		t.Fatalf("expected runtime model alias claude-sonnet-5 -> balanced, got (%q, %v)", target, ok)
	}
	reloadedCfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load after apply: %v", err)
	}
	if target, ok := reloadedCfg.LookupModelAlias("claude-opus-5"); !ok || target != "smart" {
		t.Fatalf("persisted config.json missing model alias claude-opus-5: (%q, %v)", target, ok)
	}

	// 4. 模拟 Claude Desktop 对凭证做了 __sealed__ 加密后，留空 api_key 重写模型不破坏现有凭证
	sealedGw := strings.ReplaceAll(string(afterGw), "sk-secret-demo-token-9988", "__sealed__:v1:mock-encrypted-blob")
	if err := os.WriteFile(gwPath, []byte(sealedGw), 0o644); err != nil {
		t.Fatalf("write sealed gateway: %v", err)
	}
	applyPayload["api_key"] = ""
	code, _ = doAdminJSONRequest(t, client, http.MethodPost, ts.URL+"/api/agents/claude-desktop/apply", csrf, applyPayload)
	if code != http.StatusOK {
		t.Fatalf("second apply failed: %d", code)
	}
	afterSecondGw, _ := os.ReadFile(gwPath)
	if !strings.Contains(string(afterSecondGw), "__sealed__:v1:mock-encrypted-blob") {
		t.Fatalf("second apply with blank api_key overwrote sealed key: %s", string(afterSecondGw))
	}

	// 5. POST /api/agents/claude-desktop/disable 验证切回原 ECPA 预设
	code, disableBody := doAdminJSONRequest(t, client, http.MethodPost, ts.URL+"/api/agents/claude-desktop/disable", csrf, map[string]any{})
	if code != http.StatusOK || !strings.Contains(string(disableBody), "00000000-0000-4000-8000-000000831700") {
		t.Fatalf("disable failed or did not switch back to ECPA preset: %d %s", code, string(disableBody))
	}

	// 6. POST /api/agents/claude-desktop/restore 验证彻底恢复初始字节状态并清除别名
	code, restoreBody := doAdminJSONRequest(t, client, http.MethodPost, ts.URL+"/api/agents/claude-desktop/restore", csrf, map[string]any{
		"backup_id": "original",
	})
	if code != http.StatusOK {
		t.Fatalf("restore failed: %d %s", code, string(restoreBody))
	}
	restoredMain, _ := os.ReadFile(origMainPath)
	restored3p, _ := os.ReadFile(orig3pPath)
	restoredMeta, _ := os.ReadFile(origMetaPath)
	if string(restoredMain) != string(origMainBytes) || string(restored3p) != string(orig3pBytes) || string(restoredMeta) != string(origMetaBytes) {
		t.Fatalf("restore did not match original bytes!\nmain=%q\n3p=%q\nmeta=%q", string(restoredMain), string(restored3p), string(restoredMeta))
	}
	if _, err := os.Stat(gwPath); !os.IsNotExist(err) {
		t.Fatalf("expected newly created gateway config file to be removed upon original restore")
	}
	if _, ok := cfg.LookupModelAlias("claude-sonnet-5"); ok {
		t.Fatalf("expected runtime model alias to be cleared after restore")
	}
}
