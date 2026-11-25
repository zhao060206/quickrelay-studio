package admin

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"quickrelay-studio/internal/accounts"
	"quickrelay-studio/internal/config"
	"quickrelay-studio/internal/store"
)

func TestAdminWorkModeHotSwitch(t *testing.T) {
	t.Setenv("QUICKPROXY_ADMIN_PASSWORD", "unit-test-admin-pass-123")
	tmpRoot := t.TempDir()
	cfgPath := filepath.Join(tmpRoot, "config.json")
	dbPath := filepath.Join(tmpRoot, "data", "quickrelay.db")

	cfg := config.Default()
	cfg.Addr = "127.0.0.1:18080"
	cfg.Database = dbPath
	cfg.Path = cfgPath
	if err := cfg.Save(); err != nil {
		t.Fatalf("cfg.Save: %v", err)
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

	ts := httptest.NewServer(srv.Handler(http.NotFoundHandler()))
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	code, loginBody := doAdminJSONRequest(t, client, http.MethodPost, ts.URL+"/api/auth/login", "", map[string]string{
		"password": "unit-test-admin-pass-123",
	})
	if code != http.StatusOK {
		t.Fatalf("login failed: %d %s", code, string(loginBody))
	}
	var loginRes struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.Unmarshal(loginBody, &loginRes)

	// 1. 热切换为 document 模式
	code, resBody := doAdminJSONRequest(t, client, http.MethodPut, ts.URL+"/api/work-mode", loginRes.CSRFToken, map[string]string{
		"work_mode": "document",
	})
	if code != http.StatusOK {
		t.Fatalf("PUT /api/work-mode failed: %d %s", code, string(resBody))
	}
	if got := cfg.GetWorkMode(); got != config.WorkModeDocument {
		t.Fatalf("runtime work_mode = %q, want document", got)
	}

	// 2. 验证 /api/status 立刻返回 work_mode=document
	code, stBody := doAdminJSONRequest(t, client, http.MethodGet, ts.URL+"/api/status", "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/status failed: %d", code)
	}
	var statusMap map[string]any
	_ = json.Unmarshal(stBody, &statusMap)
	if statusMap["work_mode"] != "document" {
		t.Fatalf("status work_mode = %v, want document", statusMap["work_mode"])
	}

	// 3. 验证持久化到 config.json
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if reloaded.WorkMode != "document" {
		t.Fatalf("persisted work_mode = %q, want document", reloaded.WorkMode)
	}

	// 4. 非法模式返回 400
	code, _ = doAdminJSONRequest(t, client, http.MethodPut, ts.URL+"/api/work-mode", loginRes.CSRFToken, map[string]string{
		"work_mode": "unknown_mode",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("invalid work_mode status = %d, want 400", code)
	}
}
