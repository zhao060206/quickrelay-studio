package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"quickrelay-studio/internal/accounts"
	"quickrelay-studio/internal/config"
	"quickrelay-studio/internal/store"
)

type stubInvalidator struct {
	deletedKeys []string
	cleared     int
	delErr      error
	clearErr    error
}

func (s *stubInvalidator) DeleteConversation(_ context.Context, key string) error {
	s.deletedKeys = append(s.deletedKeys, key)
	return s.delErr
}

func (s *stubInvalidator) ClearConversations(_ context.Context) error {
	s.cleared++
	return s.clearErr
}

func TestAdminConversationInvalidationRoutingAndErrors(t *testing.T) {
	t.Setenv("QUICKPROXY_ADMIN_PASSWORD", "unit-test-conv-pass")

	dbPath := filepath.Join(t.TempDir(), "admin_conv.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Default()
	mgr := accounts.New(st, &cfg)
	srv, err := NewServer(&cfg, st, mgr, false)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(srv.Close)

	inv := &stubInvalidator{}
	srv.SetConversationInvalidator(inv)
	ts := httptest.NewServer(srv.Handler(http.NotFoundHandler()))
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	code, loginBody := doAdminJSONRequest(t, client, http.MethodPost, ts.URL+"/api/auth/login", "", map[string]string{
		"password": "unit-test-conv-pass",
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

	// 1. 单删命中网关统一失效入口
	c1, b1 := doAdminJSONRequest(t, client, http.MethodDelete, ts.URL+"/api/conversations/c:abc123", csrf, nil)
	if c1 != http.StatusOK || len(inv.deletedKeys) != 1 || inv.deletedKeys[0] != "c:abc123" {
		t.Fatalf("single delete unexpected: code=%d keys=%v body=%s", c1, inv.deletedKeys, string(b1))
	}

	// 2. 全清命中网关统一失效入口
	c2, b2 := doAdminJSONRequest(t, client, http.MethodDelete, ts.URL+"/api/conversations", csrf, nil)
	if c2 != http.StatusOK || inv.cleared != 1 {
		t.Fatalf("clear conversations unexpected: code=%d cleared=%d body=%s", c2, inv.cleared, string(b2))
	}

	// 3. 数据库删除/清空失败时必须明确返回非 200 错误，不能静默吞错
	inv.delErr = errors.New("disk full")
	c3, _ := doAdminJSONRequest(t, client, http.MethodDelete, ts.URL+"/api/conversations/c:abc123", csrf, nil)
	if c3 != http.StatusInternalServerError {
		t.Fatalf("expected 500 when single delete fails, got %d", c3)
	}

	inv.clearErr = errors.New("db locked")
	c4, _ := doAdminJSONRequest(t, client, http.MethodDelete, ts.URL+"/api/conversations", csrf, nil)
	if c4 != http.StatusInternalServerError {
		t.Fatalf("expected 500 when clear fails, got %d", c4)
	}
}
