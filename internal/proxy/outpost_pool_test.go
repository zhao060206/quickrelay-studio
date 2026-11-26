package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"quickrelay-studio/internal/accounts"
	"quickrelay-studio/internal/config"
	"quickrelay-studio/internal/store"
)

type mockOutpostServer struct {
	t   *testing.T
	srv *httptest.Server

	mu              sync.Mutex
	putCount        int
	unregisterCount int
	putFailStatus   int
	activeOutposts  map[string]map[string]any
	createdSessions []string
	importedEvents  []map[string]any
	wsFrames        []map[string]any
	wsConnCount     atomic.Int32

	onWSInbound func(conn *websocket.Conn, writeFrame func(any) error, ev map[string]any, connIndex int32)
}

func newMockOutpostServer(t *testing.T) *mockOutpostServer {
	t.Helper()
	m := &mockOutpostServer{
		t:              t,
		activeOutposts: make(map[string]map[string]any),
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/integration/quick-work/upload-attachment" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"attachmentId": "att-img-1",
				"path":         "/data/s3files/users/u1/sessions/s1/workspace/attached_files/" + r.URL.Query().Get("name"),
			})
			return
		}
		if r.URL.Path == "/integration/quick-work/attachment-dlp-status" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"dlpJobStatus": "COMPLETED",
				"policyAction": "ALLOW",
			})
			return
		}
		if r.URL.Path == "/integration/quick-work/qbs-event" {
			var req struct {
				EventType string                 `json:"eventType"`
				EventBody map[string]interface{} `json:"eventBody"`
			}
			bodyBytes, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(bodyBytes, &req)
			m.mu.Lock()
			failSt := m.putFailStatus
			m.mu.Unlock()
			if req.EventType == "putOutpost" && failSt != 0 {
				http.Error(w, `{"message":"not supported"}`, failSt)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			switch req.EventType {
			case "putOutpost":
				inner, _ := req.EventBody["putOutpost"].(map[string]any)
				oid, _ := inner["outpostId"].(string)
				m.mu.Lock()
				m.putCount++
				m.activeOutposts[oid] = inner
				m.mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{
					"eventResponse": map[string]any{"putOutpostResponse": map[string]any{"status": "ok"}},
				})
			case "unregisterOutpost":
				inner, _ := req.EventBody["unregisterOutpost"].(map[string]any)
				oid, _ := inner["outpostId"].(string)
				m.mu.Lock()
				m.unregisterCount++
				delete(m.activeOutposts, oid)
				m.mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{
					"eventResponse": map[string]any{"unregisterOutpostResponse": map[string]any{"status": "ok"}},
				})
			case "invokeAgent":
				inner, _ := req.EventBody["invokeAgent"].(map[string]any)
				path, _ := inner["path"].(string)
				bodyObj, _ := inner["body"].(map[string]any)
				m.mu.Lock()
				if path == "store/sessions" {
					sid, _ := bodyObj["session_id"].(string)
					m.createdSessions = append(m.createdSessions, sid)
				} else if strings.HasPrefix(path, "store/sessions/") && strings.HasSuffix(path, "/events/batch") {
					if evs, ok := bodyObj["events"].([]any); ok {
						for _, e := range evs {
							if em, ok := e.(map[string]any); ok {
								m.importedEvents = append(m.importedEvents, em)
							}
						}
					}
				}
				m.mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{
					"eventResponse": map[string]any{"invokeAgentResponse": map[string]any{"status": 201, "body": "{}"}},
				})
			default:
				http.NotFound(w, r)
			}
			return
		}
		if r.URL.Path == "/ws" {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			idx := m.wsConnCount.Add(1)
			var wMu sync.Mutex
			writeFrame := func(v any) error {
				inner, err := json.Marshal(v)
				if err != nil {
					return err
				}
				env, _ := json.Marshal(map[string]string{
					"payload": base64.StdEncoding.EncodeToString(inner),
				})
				frame := eventStreamEncode(map[string]string{
					":event-type":   "rawOutputEvent",
					":content-type": "application/json",
					":message-type": "event",
				}, env)
				wMu.Lock()
				defer wMu.Unlock()
				return conn.WriteMessage(websocket.BinaryMessage, frame)
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
				if json.Unmarshal(raw, &outer) != nil {
					continue
				}
				inner, err := base64.StdEncoding.DecodeString(outer.Payload.Payload)
				if err != nil {
					continue
				}
				var ev map[string]any
				if json.Unmarshal(inner, &ev) != nil {
					continue
				}
				m.mu.Lock()
				m.wsFrames = append(m.wsFrames, ev)
				m.mu.Unlock()

				typ, _ := ev["type"].(string)
				if typ == "session_connect" {
					p, _ := ev["payload"].(map[string]any)
					if _, hasSession := p["session_id"]; !hasSession {
						_ = writeFrame(map[string]any{"type": "connected", "payload": map[string]any{}})
						continue
					}
				}
				if typ == "ping" {
					_ = writeFrame(map[string]any{"type": "status", "payload": map[string]any{"status": "pong"}})
					continue
				}
				if m.onWSInbound != nil {
					m.onWSInbound(conn, writeFrame, ev, idx)
				}
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func setupOutpostTestServer(t *testing.T, mock *mockOutpostServer, toolMode string) (*Server, *httptest.Server) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "outpost_test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Save(context.Background(), &store.Account{
		Email:       "outpost@example.com",
		DisplayName: "Tester",
		Provider:    "social",
		TenantURL:   mock.srv.URL,
		WSHost:      "ws://" + strings.TrimPrefix(mock.srv.URL, "http://"),
		WSPath:      "/ws",
		AccessToken: "jwt-outpost-test",
		ActorARN:    "arn:aws:quick:us-east-1:test",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("st.Save: %v", err)
	}

	cfg := config.Default()
	cfg.SessionReuse = true
	cfg.ToolMode = toolMode
	srv := NewServer(&cfg, accounts.New(st, &cfg))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Close(ctx)
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	return srv, httpSrv
}

func TestOutpostPool_AnthropicStreamAndOpenAIParallelRoundTrip(t *testing.T) {
	mock := newMockOutpostServer(t)

	// 模拟真实 AIDP WS：第一轮下发原生 tool_call，挂起等待同 WS 的 tool_result 再续写文本完成
	mock.onWSInbound = func(conn *websocket.Conn, writeFrame func(any) error, ev map[string]any, connIndex int32) {
		typ, _ := ev["type"].(string)
		p, _ := ev["payload"].(map[string]any)
		switch typ {
		case "user_message":
			mock.mu.Lock()
			out := mock.activeOutposts[p["outpostId"].(string)]
			mock.mu.Unlock()
			caps := out["capabilities"].([]any)
			capObj := caps[0].(map[string]any)
			tools := capObj["tools"].([]any)
			firstAlias := tools[0].(map[string]any)["name"].(string)

			_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{"role": "assistant"}})
			_ = writeFrame(map[string]any{"type": "thinking_delta", "payload": map[string]any{"delta": "checking"}})
			_ = writeFrame(map[string]any{
				"type": "tool_call",
				"payload": map[string]any{
					"tool_call_id": "call_native_1",
					"tool_name":    firstAlias,
					"arguments": map[string]any{
						"params": map[string]any{
							"name":      firstAlias,
							"arguments": map[string]any{"file_path": "go.mod"},
							"_quickwork": map[string]any{"result_transport_version": 1},
						},
					},
				},
			})
			// 第二套并行工具请求（若有两个工具则一并发第 2 个）
			if len(tools) >= 2 && strings.Contains(p["text"].(string), "PARALLEL") {
				secondAlias := tools[1].(map[string]any)["name"].(string)
				_ = writeFrame(map[string]any{
					"type": "tool_call",
					"payload": map[string]any{
						"tool_call_id": "call_native_2",
						"tool_name":    secondAlias,
						"arguments":    map[string]any{"command": "go version"},
					},
				})
			}
			_ = writeFrame(map[string]any{
				"type":    "turn_usage",
				"payload": map[string]any{"input_tokens": 10, "output_tokens": 25, "cache_write_tokens": 70000, "resolved_model_id": "us.anthropic.claude-opus-5-5"},
			})
		case "tool_result":
			callID, _ := p["tool_call_id"].(string)
			resMap, _ := p["result"].(map[string]any)
			contentList, _ := resMap["content"].([]any)
			var txt string
			for _, item := range contentList {
				if m, ok := item.(map[string]any); ok {
					txt += fmt.Sprint(m["text"]) + "|"
				}
			}
			if callID == "call_native_1" && strings.Contains(txt, "MOD_OK") {
				_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "Result confirmed: MOD_OK"}})
				_ = writeFrame(map[string]any{"type": "turn_usage", "payload": map[string]any{"input_tokens": 2, "output_tokens": 12, "cache_read_tokens": 70000}})
				_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "Result confirmed: MOD_OK"}})
			} else if callID == "call_native_2" && strings.Contains(txt, "OUT_2") {
				_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "Parallel done: " + txt}})
				_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "Parallel done: " + txt}})
			}
		}
	}

	srv, httpSrv := setupOutpostTestServer(t, mock, config.ToolModeAuto)
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}}

	// 1. Anthropic 流式轮 1：收到原生 tool_use(Read)
	reqBody := map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 256,
		"stream":     true,
		"tools": []map[string]any{
			{"name": "Read", "description": "Read file", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}}},
			{"name": "Bash", "description": "Run shell", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}}},
		},
		"messages": []map[string]any{{"role": "user", "content": "Read go.mod"}},
	}
	resp, err := doRawJSONPost(t, client, httpSrv.URL+"/v1/messages", map[string]string{"X-Api-Key": "k-ant"}, reqBody)
	if err != nil {
		t.Fatalf("Anthropic stream round 1 failed: %v", err)
	}
	rawSSE, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	sseText := string(rawSSE)
	if !strings.Contains(sseText, `"type":"tool_use"`) || !strings.Contains(sseText, `"name":"Read"`) || !strings.Contains(sseText, `"stop_reason":"tool_use"`) {
		t.Fatalf("expected Anthropic tool_use Read SSE, got:\n%s", sseText)
	}
	if mock.wsConnCount.Load() != 1 {
		t.Fatalf("expected 1 WS connection after round 1, got %d", mock.wsConnCount.Load())
	}

	// 2. Anthropic 非流式轮 2：回传 tool_result，必须原位复用同一条 WS（wsConnCount 仍为 1！）
	reqBody["stream"] = false
	reqBody["messages"] = []map[string]any{
		{"role": "user", "content": "Read go.mod"},
		{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "call_native_1", "name": "Read", "input": map[string]any{"file_path": "go.mod"}}}},
		{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "call_native_1", "content": "MOD_OK"}}},
	}
	code, resBytes := doProxyJSONCall(t, client, httpSrv.URL+"/v1/messages", map[string]string{"X-Api-Key": "k-ant"}, reqBody)
	if code != http.StatusOK {
		t.Fatalf("Anthropic round 2 status=%d body=%s", code, resBytes)
	}
	if !strings.Contains(string(resBytes), "Result confirmed: MOD_OK") {
		t.Fatalf("Anthropic round 2 missing tool confirmation: %s", resBytes)
	}
	if mock.wsConnCount.Load() != 1 {
		t.Fatalf("round 2 should reuse parked WS (connCount=1), got %d", mock.wsConnCount.Load())
	}

	// 3. OpenAI 并行多工具回调 + 并发用户更新续写
	oaiReq := map[string]any{
		"model":  "smart",
		"stream": false,
		"tools": []map[string]any{
			{"type": "function", "function": map[string]any{"name": "read_a", "description": "Read file A", "parameters": map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}}}},
			{"type": "function", "function": map[string]any{"name": "read_b", "description": "Run cmd B", "parameters": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}}}},
		},
		"messages": []map[string]any{{"role": "user", "content": "PARALLEL test"}},
	}
	code, resBytes = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"Authorization": "Bearer k-oai"}, oaiReq)
	if code != http.StatusOK {
		t.Fatalf("OpenAI parallel round 1 status=%d body=%s", code, resBytes)
	}
	var oaiParsed struct {
		Choices []struct {
			FinishReason string     `json:"finish_reason"`
			Message      struct {
				ToolCalls []ToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(resBytes, &oaiParsed)
	if oaiParsed.Choices[0].FinishReason != "tool_calls" || len(oaiParsed.Choices[0].Message.ToolCalls) != 2 {
		t.Fatalf("expected 2 parallel OpenAI tool_calls, got: %s", resBytes)
	}
	oaiReq["messages"] = []map[string]any{
		{"role": "user", "content": "PARALLEL test"},
		{"role": "assistant", "content": nil, "tool_calls": oaiParsed.Choices[0].Message.ToolCalls},
		{"role": "tool", "tool_call_id": "call_native_1", "content": "OUT_1"},
		{"role": "tool", "tool_call_id": "call_native_2", "content": "OUT_2"},
		{"role": "user", "content": "also include extra note"},
	}
	code, resBytes = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"Authorization": "Bearer k-oai"}, oaiReq)
	if code != http.StatusOK || !strings.Contains(string(resBytes), "[QuickRelay client user update]") || !strings.Contains(string(resBytes), "OUT_2") {
		t.Fatalf("OpenAI parallel tool_result resume failed: code=%d body=%s", code, resBytes)
	}

	// 4. 优雅停机必须调用 unregisterOutpost 清理全部在线虚拟设备
	shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv.Close(shutCtx)

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.putCount == 0 || mock.unregisterCount == 0 || len(mock.activeOutposts) != 0 {
		t.Fatalf("outpost cleanup mismatch: put=%d unregister=%d active=%d", mock.putCount, mock.unregisterCount, len(mock.activeOutposts))
	}
}

func TestOutpostPool_ColdHistoryImportAndPermissionGuard(t *testing.T) {
	mock := newMockOutpostServer(t)
	mock.onWSInbound = func(conn *websocket.Conn, writeFrame func(any) error, ev map[string]any, connIndex int32) {
		typ, _ := ev["type"].(string)
		p, _ := ev["payload"].(map[string]any)
		switch typ {
		case "user_message":
			txt, _ := p["text"].(string)
			if strings.Contains(txt, "UNAUTHORIZED_PERM") {
				// 触发一个不属于 mcp__quickrelay 的权限请求，代理应发 permission_denied 并阻断
				_ = writeFrame(map[string]any{
					"type": "permission_request",
					"payload": map[string]any{
						"connectorId": "cloud_device",
						"actionId":    "run_python_on_device",
					},
				})
				return
			}
			_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "cold-restored-ok"}})
			_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "cold-restored-ok"}})
		}
	}

	_, httpSrv := setupOutpostTestServer(t, mock, config.ToolModeAuto)
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}}

	// 1. 携带已完成的多轮历史 + 尾轮 tool_result 冷恢复启动（无先前挂起 WS，必须自动调 store/sessions + events/batch）
	code, body := doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"Authorization": "Bearer k-cold"}, map[string]any{
		"model": "smart",
		"tools": []map[string]any{
			{"type": "function", "function": map[string]any{"name": "read_a", "description": "Read A", "parameters": map[string]any{"type": "object"}}},
		},
		"messages": []map[string]any{
			{"role": "user", "content": "Read canary.txt"},
			{"role": "assistant", "content": "", "tool_calls": []map[string]any{
				{"id": "call_cold_1", "type": "function", "function": map[string]any{"name": "read_a", "arguments": `{"p":"canary.txt"}`}},
			}},
			{"role": "tool", "tool_call_id": "call_cold_1", "content": "CANARY=7731"},
		},
	})
	if code != http.StatusOK || !strings.Contains(string(body), "cold-restored-ok") {
		t.Fatalf("cold history import failed: code=%d body=%s", code, body)
	}
	mock.mu.Lock()
	sessCount := len(mock.createdSessions)
	evCount := len(mock.importedEvents)
	mock.mu.Unlock()
	if sessCount != 1 || evCount < 6 {
		t.Fatalf("expected store/sessions and >=6 imported events, got sessions=%d events=%d", sessCount, evCount)
	}

	// 2. 越权 permission_request 被拒绝且回发 permission_denied
	code, body = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"Authorization": "Bearer k-perm"}, map[string]any{
		"model":    "smart",
		"tools":    testDummyTools(),
		"messages": []map[string]any{{"role": "user", "content": "UNAUTHORIZED_PERM"}},
	})
	if code == http.StatusOK {
		t.Fatalf("expected unauthorized permission request to be blocked, got 200: %s", body)
	}
	mock.mu.Lock()
	var sawDenied bool
	for _, f := range mock.wsFrames {
		if f["type"] == "permission_denied" {
			sawDenied = true
			break
		}
	}
	mock.mu.Unlock()
	if !sawDenied {
		t.Fatalf("expected permission_denied frame sent to upstream")
	}
}

func TestOutpostPool_FallbackAndAgentReset(t *testing.T) {
	mock := newMockOutpostServer(t)
	mock.onWSInbound = func(conn *websocket.Conn, writeFrame func(any) error, ev map[string]any, connIndex int32) {
		typ, _ := ev["type"].(string)
		if typ == "user_message" {
			_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "<client_tool_call>{\"name\":\"local_read_file\",\"arguments\":{\"path\":\"fallback.txt\"}}</client_tool_call>"}})
			_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{}})
			_ = writeFrame(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
		}
	}

	// 测试 1：putOutpost 返回 404 时，ToolModeAuto 自动回退文本桥
	mock.mu.Lock()
	mock.putFailStatus = http.StatusNotFound
	mock.mu.Unlock()
	srv, httpSrv := setupOutpostTestServer(t, mock, config.ToolModeAuto)
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}}

	code, body := doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"Authorization": "Bearer k-fb"}, map[string]any{
		"model":    "smart",
		"tools":    testDummyTools(),
		"messages": []map[string]any{{"role": "user", "content": "test fallback"}},
	})
	if code != http.StatusOK || !strings.Contains(string(body), `"name":"local_read_file"`) {
		t.Fatalf("auto fallback to text_bridge failed: code=%d body=%s", code, body)
	}

	// 测试 2：含图片的多模态工具请求直接绕开 Outpost 走经典文本桥
	mock.mu.Lock()
	mock.putFailStatus = 0
	prevPut := mock.putCount
	mock.mu.Unlock()
	tinyPNG := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+ip1sAAAAASUVORK5CYII="
	code, _ = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"Authorization": "Bearer k-img"}, map[string]any{
		"model": "smart",
		"tools": testDummyTools(),
		"messages": []map[string]any{{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "inspect image"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": tinyPNG}},
			},
		}},
	})
	if code != http.StatusOK {
		t.Fatalf("image turn status=%d", code)
	}
	mock.mu.Lock()
	if mock.putCount != prevPut {
		mock.mu.Unlock()
		t.Fatalf("image turn should bypass putOutpost, got putCount %d->%d", prevPut, mock.putCount)
	}
	mock.mu.Unlock()

	// 测试 3：Agent 切换触发旧 Outpost 立即重建（wsConnCount 递增）
	mock.onWSInbound = func(conn *websocket.Conn, writeFrame func(any) error, ev map[string]any, connIndex int32) {
		if ev["type"] == "user_message" {
			_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": fmt.Sprintf("agent-turn-%d", connIndex)}})
			_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{}})
		}
	}
	beforeConn := mock.wsConnCount.Load()
	req := map[string]any{
		"model":    "smart",
		"tools":    testDummyTools(),
		"messages": []map[string]any{{"role": "user", "content": "same conv"}},
	}
	code, _ = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"X-Conversation-Id": "conv-agent-switch"}, req)
	if code != http.StatusOK {
		t.Fatalf("first agent turn failed: %d", code)
	}
	srv.cfg.ChatAgentID = "agent-custom-1"
	req["messages"] = []map[string]any{
		{"role": "user", "content": "same conv"},
		{"role": "assistant", "content": "agent-turn"},
		{"role": "user", "content": "follow up after agent switch"},
	}
	code, _ = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"X-Conversation-Id": "conv-agent-switch"}, req)
	if code != http.StatusOK {
		t.Fatalf("second agent turn failed: %d", code)
	}
	if mock.wsConnCount.Load() < beforeConn+2 {
		t.Fatalf("expected agent switch to open fresh outpost WS connection")
	}
}

func doRawJSONPost(t *testing.T, client *http.Client, url string, headers map[string]string, payload any) (*http.Response, error) {
	t.Helper()
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return client.Do(req)
}
