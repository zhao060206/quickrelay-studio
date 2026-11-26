package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quickrelay-studio/internal/accounts"
	"quickrelay-studio/internal/config"
	"quickrelay-studio/internal/store"
)

func makeTestJWT(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	bodyBytes, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(bodyBytes)
	return header + "." + payload + ".sig"
}

func setupImportTestServer(t *testing.T) (*httptest.Server, *http.Client, string, *store.Store) {
	t.Helper()
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
	t.Cleanup(func() { _ = st.Close() })

	mgr := accounts.New(st, &cfg)
	srv, err := NewServer(&cfg, st, mgr, false)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(srv.Close)

	ts := httptest.NewServer(srv.Handler(http.NotFoundHandler()))
	t.Cleanup(ts.Close)

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
	return ts, client, loginRes.CSRFToken, st
}

func TestAccountsImport_ShapesFieldsJWTAndDedupe(t *testing.T) {
	ts, client, csrf, st := setupImportTestServer(t)

	expFuture := time.Now().Add(2 * time.Hour).Unix()
	jwt1 := makeTestJWT(map[string]any{
		"email":          "alice@example.com",
		"sub":            "arn:aws:quick:us-east-1:alice",
		"aws_account_id": "111122223333",
		"region":         "us-west-2",
		"exp":            expFuture,
	})

	// 1. 单对象导入（驼峰字段 + JWT 自动推导 email/region/exp + 无明文回显）
	singlePayload := map[string]any{
		"idToken":      jwt1,
		"accessToken":  "acc-secret-1",
		"refreshToken": "ref-secret-1",
		"clientId":     "cid-1",
		"clientSecret": "csec-1",
		"disabled":     true,
	}
	code, body := doAdminJSONRequest(t, client, http.MethodPost, ts.URL+"/api/accounts/import", csrf, singlePayload)
	if code != http.StatusOK {
		t.Fatalf("single import failed: %d %s", code, string(body))
	}
	bodyStr := string(body)
	for _, secret := range []string{"acc-secret-1", "ref-secret-1", "csec-1", jwt1} {
		if strings.Contains(bodyStr, secret) {
			t.Fatalf("import response leaked sensitive credential %q", secret)
		}
	}
	var res1 struct {
		Total    int                       `json:"total"`
		Imported int                       `json:"imported"`
		Updated  int                       `json:"updated"`
		Failed   int                       `json:"failed"`
		Results  []importAccountItemResult `json:"results"`
	}
	if err := json.Unmarshal(body, &res1); err != nil {
		t.Fatalf("decode res1: %v", err)
	}
	if res1.Total != 1 || res1.Imported != 1 || res1.Updated != 0 || res1.Failed != 0 {
		t.Fatalf("unexpected res1 counts: %+v", res1)
	}
	if res1.Results[0].Email != "alice@example.com" || res1.Results[0].Status != "imported" {
		t.Fatalf("unexpected res1 item: %+v", res1.Results[0])
	}

	// 检查入库详情（disabled 保留、JWT 自动推导、token_endpoint 自动补齐）
	acct1, err := st.Get(context.Background(), res1.Results[0].ID)
	if err != nil {
		t.Fatalf("st.Get: %v", err)
	}
	if !acct1.Disabled || acct1.Region != "us-west-2" || acct1.ActorARN != "arn:aws:quick:us-east-1:alice" ||
		acct1.ExpiresAt != expFuture || acct1.WSHost == "" || acct1.TokenEndpoint != "https://oidc.us-west-2.amazonaws.com/token" {
		t.Fatalf("unexpected stored acct1 fields: %+v", acct1)
	}

	// 2. 纯数组导入（同账号更新计为 updated + 两个无 email 账号互不覆盖 + 一条缺凭据部分失败）
	arrPayload := []map[string]any{
		{
			"email":         "alice@example.com",
			"tenant_url":    acct1.TenantURL,
			"refresh_token": "ref-secret-1-new",
			"disabled":      false,
		},
		{
			"refresh_token": "no-email-refresh-A",
			"expires_at":    "2026-12-01T00:00:00Z",
		},
		{
			"refresh_token": "no-email-refresh-B",
			"expires_at":    1796083200000, // 毫秒时间戳自动转秒
		},
		{
			"email": "broken-no-token@example.com",
		},
	}
	code, body2 := doAdminJSONRequest(t, client, http.MethodPost, ts.URL+"/api/accounts/import", csrf, arrPayload)
	if code != http.StatusOK {
		t.Fatalf("array import failed: %d %s", code, string(body2))
	}
	var res2 struct {
		Total    int                       `json:"total"`
		Imported int                       `json:"imported"`
		Updated  int                       `json:"updated"`
		Failed   int                       `json:"failed"`
		Results  []importAccountItemResult `json:"results"`
	}
	_ = json.Unmarshal(body2, &res2)
	if res2.Total != 4 || res2.Imported != 2 || res2.Updated != 1 || res2.Failed != 1 {
		t.Fatalf("unexpected res2 counts: %+v", res2)
	}
	if res2.Results[0].Status != "updated" || res2.Results[3].Status != "failed" {
		t.Fatalf("unexpected res2 statuses: %+v", res2.Results)
	}
	if res2.Results[1].Email == res2.Results[2].Email || !strings.HasPrefix(res2.Results[1].Email, "imported-") {
		t.Fatalf("no-email accounts collided or missing fallback email: %+v", res2.Results)
	}

	// 验证数据库中总账号数为 3（alice + no-email-A + no-email-B）
	all, err := st.List(context.Background())
	if err != nil || len(all) != 3 {
		t.Fatalf("expected 3 accounts in db, got %d (err=%v)", len(all), err)
	}

	// 3. 包装对象 {"accounts":[...]} 导入新账号
	wrapPayload := map[string]any{
		"auto_refresh": false,
		"accounts": []map[string]any{{
			"email":        "bob@example.com",
			"provider":     "gateway",
			"access_token": "bob-acc",
			"ws_host":      "gw.example.com",
		}},
	}
	code, body3 := doAdminJSONRequest(t, client, http.MethodPost, ts.URL+"/api/accounts/import", csrf, wrapPayload)
	if code != http.StatusOK {
		t.Fatalf("wrapper import failed: %d %s", code, string(body3))
	}
	var res3 struct {
		Imported int                       `json:"imported"`
		Results  []importAccountItemResult `json:"results"`
	}
	_ = json.Unmarshal(body3, &res3)
	if res3.Imported != 1 {
		t.Fatalf("expected 1 imported in wrapper mode, got %+v", res3)
	}
	bob, _ := st.Get(context.Background(), res3.Results[0].ID)
	if bob.Provider != "gateway" || bob.WSHost != "gw.example.com" || bob.WSPath != defaultGatewayWSPath {
		t.Fatalf("unexpected gateway normalized fields: %+v", bob)
	}

	// 4. 空数组与非法形状返回 400 invalid_import
	code, _ = doAdminJSONRequest(t, client, http.MethodPost, ts.URL+"/api/accounts/import", csrf, []any{})
	if code != http.StatusBadRequest {
		t.Fatalf("empty array import status = %d, want 400", code)
	}
	code, _ = doAdminJSONRequest(t, client, http.MethodPost, ts.URL+"/api/accounts/import", csrf, map[string]any{"accounts": "not-array"})
	if code != http.StatusBadRequest {
		t.Fatalf("non-array wrapper status = %d, want 400", code)
	}
}
