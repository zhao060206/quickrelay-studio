package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"quickrelay-studio/internal/accounts"
	"quickrelay-studio/internal/config"
	"quickrelay-studio/internal/version"
	"quickrelay-studio/internal/store"
)

// TestMapAnthropicModel 验证 Anthropic/Claude 模型别名能正确映射分流到 fast / smart / balanced / auto。
func TestMapAnthropicModel(t *testing.T) {
	cfg := config.Default()
	s := &Server{cfg: &cfg}

	cases := []struct {
		in   string
		want string
	}{
		{"", cfg.DefaultModel},
		{"claude-haiku-4-5-20251001", "fast"},
		{"claude-3-5-haiku-latest", "fast"},
		{"claude-sonnet-4-5", "smart"},
		{"claude-opus-4-5", "smart"},
		{"aws-quick-balanced", "balanced"},
		{"auto", "auto"},
	}
	for _, tc := range cases {
		if got := s.mapAnthropicModel(tc.in); got != tc.want {
			t.Errorf("mapAnthropicModel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestAnthropicToChatRequest 验证 system 块、图片附件、tool_use/tool_result、tool_choice 与 thinking 强度完整映射。
func TestAnthropicToChatRequest(t *testing.T) {
	ar := &anthropicMessagesRequest{
		Model:  "claude-sonnet-4-5",
		Stream: true,
		System: json.RawMessage(`[{"type":"text","text":"规则一"},{"type":"text","text":"规则二"}]`),
		Messages: []anthropicMessage{
			{
				Role: "user",
				Content: json.RawMessage(`[
					{"type":"text","text":"看这张图"},
					{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"AQID"}},
					{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}
				]`),
			},
			{
				Role: "assistant",
				Content: json.RawMessage(`[
					{"type":"text","text":"我将读取文件"},
					{"type":"tool_use","id":"tu_1","name":"Read","input":{"path":"a.go"}}
				]`),
			},
			{
				Role: "user",
				Content: json.RawMessage(`[
					{"type":"tool_result","tool_use_id":"tu_1","content":"file not found","is_error":true},
					{"type":"text","text":"请继续"}
				]`),
			},
		},
		Tools: []anthropicTool{
			{
				Name:        "Read",
				Description: "读取文件",
				InputSchema: map[string]any{"type": "object"},
			},
		},
		ToolChoice: map[string]any{
			"type":                      "tool",
			"name":                      "Read",
			"disable_parallel_tool_use": true,
		},
		Thinking: &anthropicThinking{
			Type:         "enabled",
			BudgetTokens: 2048,
		},
	}

	got := anthropicToChatRequest(ar)
	if len(got.Messages) != 5 {
		t.Fatalf("expected 5 internal messages (system, user, assistant, tool, user), got %d: %+v", len(got.Messages), got.Messages)
	}
	if got.Messages[0].Role != "system" || got.Messages[0].Content != "规则一\n\n规则二" {
		t.Fatalf("unexpected system message: %+v", got.Messages[0])
	}
	userParts, ok := got.Messages[1].Content.([]any)
	if !ok || len(userParts) != 3 {
		t.Fatalf("expected 3 multimodal parts in first user msg, got %#v", got.Messages[1].Content)
	}
	if got.Messages[2].Role != "assistant" || len(got.Messages[2].ToolCalls) != 1 || got.Messages[2].ToolCalls[0].Function.Name != "Read" {
		t.Fatalf("unexpected assistant tool call message: %+v", got.Messages[2])
	}
	if got.Messages[3].Role != "tool" || got.Messages[3].ToolCallID != "tu_1" || !strings.HasPrefix(got.Messages[3].Content.(string), "[tool error] ") {
		t.Fatalf("unexpected tool_result message: %+v", got.Messages[3])
	}
	if got.ParallelToolCalls == nil || *got.ParallelToolCalls != false {
		t.Fatalf("expected ParallelToolCalls=false, got %v", got.ParallelToolCalls)
	}
	if got.Reasoning == nil || got.Reasoning.Effort != "low" {
		t.Fatalf("expected thinking effort 'low' for budget 2048, got %+v", got.Reasoning)
	}
	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "Read" {
		t.Fatalf("unexpected converted tools: %+v", got.Tools)
	}
}

// TestAnthropicCountTokensAndHealth 验证 /healthz 服务标识与 /v1/messages/count_tokens 鉴权和本地计数逻辑。
func TestAnthropicCountTokensAndHealth(t *testing.T) {
	cfg := config.Default()
	cfg.APIKeys = []string{"sk-secret-key"}
	s := NewServer(&cfg, nil)
	h := s.Handler()

	// 1. /healthz 无需密钥且返回 quickrelay-studio 标识
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", rec.Code)
	}
	var health map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatalf("decode /healthz: %v", err)
	}
	if health["service"] != "quickrelay-studio" || health["version"] != version.Current {
		t.Fatalf("unexpected /healthz body: %+v", health)
	}

	// 2. /v1/messages/count_tokens 未带 Key 应返回 401
	body := `{"model":"claude-sonnet-4-5","system":"hello","messages":[{"role":"user","content":[{"type":"text","text":"你好世界"},{"type":"image","source":{"type":"base64","data":"AA=="}}]}]}`
	reqUnauth := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	recUnauth := httptest.NewRecorder()
	h.ServeHTTP(recUnauth, reqUnauth)
	if recUnauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated count_tokens status = %d, want 401", recUnauth.Code)
	}

	// 3. 带 X-Api-Key 正常估算（含图片固定估算开销 >= 1024）
	reqAuth := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	reqAuth.Header.Set("X-Api-Key", "sk-secret-key")
	recAuth := httptest.NewRecorder()
	h.ServeHTTP(recAuth, reqAuth)
	if recAuth.Code != http.StatusOK {
		t.Fatalf("authenticated count_tokens status = %d, want 200: %s", recAuth.Code, recAuth.Body.String())
	}
	var tokResp struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(recAuth.Body.Bytes(), &tokResp); err != nil {
		t.Fatalf("decode count_tokens: %v", err)
	}
	if tokResp.InputTokens < 1024 {
		t.Fatalf("expected input_tokens >= 1024 with image block, got %d", tokResp.InputTokens)
	}
}

// TestHandleAnthropicMessagesSyncAndStream 通过 Mock WebSocket 上游端到端测试 /v1/messages 同步、流式 SSE、<decision> 转 AskUserQuestion 及数据库日志记录。
func TestHandleAnthropicMessagesSyncAndStream(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		var writeMu sync.Mutex
		writeEvent := func(v any) {
			writeMu.Lock()
			defer writeMu.Unlock()
			_ = conn.WriteMessage(websocket.BinaryMessage, encodeMockAIDPServerEvent(v))
		}

		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var outer struct {
				Payload struct {
					Payload string `json:"payload"`
				} `json:"payload"`
			}
			if json.Unmarshal(raw, &outer) != nil || outer.Payload.Payload == "" {
				continue
			}
			innerBytes, err := base64.StdEncoding.DecodeString(outer.Payload.Payload)
			if err != nil {
				continue
			}
			var inner struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(innerBytes, &inner) != nil {
				continue
			}
			if inner.Type == "user_message" {
				go func() {
					writeEvent(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m_mock"}})
					writeEvent(map[string]any{"type": "thinking_delta", "payload": map[string]any{"delta": "先分析用户意图。"}})
					writeEvent(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "已完成检查。[@abc123]\n"}})
					writeEvent(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": `<decision question="接下来先做哪件事？"><option description="运行全量测试">跑测试</option><option description="构建发布包">打包</option></decision>`}})
					writeEvent(map[string]any{
						"type": "turn_usage",
						"payload": map[string]any{
							"input_tokens":      120,
							"output_tokens":     45,
							"total_tokens":      185,
							"cache_read_tokens": 20,
							"resolved_model_id": "claude-sonnet-4-5",
						},
					})
					writeEvent(map[string]any{"type": "message_complete", "payload": map[string]any{"continuation_expected": false}})
					writeEvent(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
				}()
			}
		}
	}))
	defer ts.Close()

	dbPath := filepath.Join(t.TempDir(), "proxy_anthropic.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	acct := &store.Account{
		Email:       "claude-tester@example.com",
		Provider:    "social",
		WSHost:      "ws://" + strings.TrimPrefix(ts.URL, "http://"),
		WSPath:      "/ws",
		AccessToken: "mock-jwt-token",
		ActorARN:    "arn:aws:quick:us-east-1:test",
		ExpiresAt:   time.Now().Add(2 * time.Hour).Unix(),
	}
	if err := st.Save(context.Background(), acct); err != nil {
		t.Fatalf("st.Save: %v", err)
	}

	cfg := config.Default()
	cfg.RequestTimeout = config.Duration(10 * time.Second)
	cfg.SessionReuse = false // 确保每次请求都独立完整触发上游 mock
	mgr := accounts.New(st, &cfg)
	srv := NewServer(&cfg, mgr)
	h := srv.Handler()

	reqPayload := map[string]any{
		"model":      "claude-sonnet-4-5",
		"max_tokens": 1024,
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": 2048},
		"system":     "请用中文回复",
		"messages": []map[string]any{
			{"role": "user", "content": "请给我下一步选择"},
		},
		"tools": []map[string]any{
			{
				"name":         "AskUserQuestion",
				"description":  "向用户提出选择",
				"input_schema": map[string]any{"type": "object"},
			},
		},
	}

	// ---- 1. 同步 JSON 模式测试 ----
	reqPayload["stream"] = false
	syncBytes, _ := json.Marshal(reqPayload)
	syncReq := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(syncBytes))
	syncReq.Header.Set("Content-Type", "application/json")
	syncRec := httptest.NewRecorder()
	h.ServeHTTP(syncRec, syncReq)

	if syncRec.Code != http.StatusOK {
		t.Fatalf("sync /v1/messages status = %d, body = %s", syncRec.Code, syncRec.Body.String())
	}

	var syncResp struct {
		Type       string `json:"type"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type     string `json:"type"`
			Thinking string `json:"thinking"`
			Text     string `json:"text"`
			Name     string `json:"name"`
		} `json:"content"`
		Usage struct {
			InputTokens          int `json:"input_tokens"`
			OutputTokens         int `json:"output_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(syncRec.Body.Bytes(), &syncResp); err != nil {
		t.Fatalf("unmarshal sync response: %v", err)
	}
	if syncResp.Type != "message" || syncResp.StopReason != "tool_use" {
		t.Fatalf("unexpected sync metadata: type=%q stop_reason=%q", syncResp.Type, syncResp.StopReason)
	}
	if syncResp.Usage.CacheReadInputTokens != 20 || syncResp.Usage.OutputTokens != 45 {
		t.Fatalf("unexpected usage stats: %+v", syncResp.Usage)
	}

	hasThinking, hasCleanText, hasAskTool := false, false, false
	for _, blk := range syncResp.Content {
		switch blk.Type {
		case "thinking":
			if strings.Contains(blk.Thinking, "先分析") {
				hasThinking = true
			}
		case "text":
			if strings.Contains(blk.Text, "已完成检查") && !strings.Contains(blk.Text, "<decision") && !strings.Contains(blk.Text, "[@abc123]") {
				hasCleanText = true
			}
		case "tool_use":
			if blk.Name == "AskUserQuestion" {
				hasAskTool = true
			}
		}
	}
	if !hasThinking || !hasCleanText || !hasAskTool {
		t.Fatalf("sync content blocks missing expected items (thinking=%v text=%v askTool=%v): %+v", hasThinking, hasCleanText, hasAskTool, syncResp.Content)
	}

	// 验证 request_logs 写入了成功记录
	logPage, err := st.QueryRequestLogs(context.Background(), store.RequestLogFilter{Protocol: "anthropic"})
	if err != nil {
		t.Fatalf("QueryRequestLogs: %v", err)
	}
	if logPage.Total != 1 || logPage.Items[0].Status != "ok" || logPage.Items[0].AccountEmail != "claude-tester@example.com" {
		t.Fatalf("expected 1 ok anthropic request_log, got %+v", logPage)
	}

	// ---- 2. 流式 SSE 模式测试 ----
	reqPayload["stream"] = true
	streamBytes, _ := json.Marshal(reqPayload)
	streamReq := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(streamBytes))
	streamReq.Header.Set("Content-Type", "application/json")
	streamRec := httptest.NewRecorder()
	h.ServeHTTP(streamRec, streamReq)

	if streamRec.Code != http.StatusOK {
		t.Fatalf("stream /v1/messages status = %d, body = %s", streamRec.Code, streamRec.Body.String())
	}
	if ct := streamRec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("unexpected Content-Type: %q", ct)
	}

	var events []string
	var rawSSE strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(streamRec.Body.Bytes()))
	for scanner.Scan() {
		line := scanner.Text()
		rawSSE.WriteString(line + "\n")
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimSpace(strings.TrimPrefix(line, "event: ")))
		}
	}
	sseStr := rawSSE.String()
	requiredSubstrings := []string{
		"event: message_start",
		`"type":"thinking_delta"`,
		`"type":"signature_delta"`,
		`"type":"text_delta"`,
		`"name":"AskUserQuestion"`,
		`"type":"input_json_delta"`,
		`"stop_reason":"tool_use"`,
		"event: message_stop",
	}
	for _, sub := range requiredSubstrings {
		if !strings.Contains(sseStr, sub) {
			t.Fatalf("SSE output missing %q;\nevents=%v\nraw=%s", sub, events, sseStr)
		}
	}
}

// TestMapAnthropicModel_CustomAliases 验证桌面智能体设置的 model_aliases 能在不重启服务的情况下立即生效且优先级高于内置名称推断。
func TestMapAnthropicModel_CustomAliases(t *testing.T) {
	cfg := config.Default()
	cfg.SetModelAliases(map[string]string{
		"claude-sonnet-5": "balanced",
		"claude-opus-5":   "fast",
	})
	s := &Server{cfg: &cfg}

	if got := s.mapAnthropicModel("claude-sonnet-5"); got != "balanced" {
		t.Fatalf("mapAnthropicModel(claude-sonnet-5) = %q, want %q", got, "balanced")
	}
	if got := s.mapAnthropicModel("claude-opus-5"); got != "fast" {
		t.Fatalf("mapAnthropicModel(claude-opus-5) = %q, want %q", got, "fast")
	}
	if got := s.mapModel("claude-sonnet-5"); got != "balanced" {
		t.Fatalf("mapModel(claude-sonnet-5) = %q, want %q", got, "balanced")
	}
	// 未被自定义映射的模型仍走默认规则
	if got := s.mapAnthropicModel("claude-haiku-4-5"); got != "fast" {
		t.Fatalf("mapAnthropicModel(claude-haiku-4-5) = %q, want %q", got, "fast")
	}
}
