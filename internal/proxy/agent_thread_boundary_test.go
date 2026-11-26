package proxy

import (
	"bytes"
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

// detailedCapturedTurn 记录测试 Mock WebSocket 收到的完整 user_message 帧细节，用于校验线程、Agent、附件与真实字节大小。
type detailedCapturedTurn struct {
	Attempt          int32
	HasSessionCreate bool
	SessionCreateID  string
	ThreadID         string
	ChatAgentID      string
	Text             string
	Attachments      []map[string]any
	RawFrameBytes    int
	ToolNamesMention []string
}

func startDetailedMockUpstream(
	t *testing.T,
	handler func(attempt int32, req *http.Request, msgType string, payload map[string]any) (replyText string, errEvent map[string]any),
) (*httptest.Server, *atomic.Int32, *[]detailedCapturedTurn, *sync.Mutex) {
	t.Helper()
	var dialCount atomic.Int32
	var mu sync.Mutex
	var captured []detailedCapturedTurn
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := dialCount.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		var writeMu sync.Mutex
		writeEv := func(v any) {
			writeMu.Lock()
			defer writeMu.Unlock()
			_ = conn.WriteMessage(websocket.BinaryMessage, encodeMockAIDPServerEvent(v))
		}

		var rec detailedCapturedTurn
		rec.Attempt = attempt
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
			rawInnerLen := 0
			if json.Unmarshal(raw, &outer) == nil && outer.Payload.Payload != "" {
				if innerBytes, decErr := base64.StdEncoding.DecodeString(outer.Payload.Payload); decErr == nil {
					rawInnerLen = len(innerBytes)
				}
			}
			msgType, payload, ok := decodeClientFrame(raw)
			if !ok {
				continue
			}
			switch msgType {
			case "session_create":
				rec.HasSessionCreate = true
				if sid, _ := payload["session_id"].(string); sid != "" {
					rec.SessionCreateID = sid
				}
			case "user_message":
				rec.ThreadID, _ = payload["thread_id"].(string)
				if id, ok := payload["chat_agent_id"].(string); ok {
					rec.ChatAgentID = id
				} else {
					rec.ChatAgentID = ""
				}
				rec.Text, _ = payload["text"].(string)
				rec.RawFrameBytes = rawInnerLen
				if rec.RawFrameBytes == 0 {
					if b, err := json.Marshal(map[string]any{"type": "user_message", "payload": payload}); err == nil {
						rec.RawFrameBytes = len(b)
					}
				}
				if rawAtts, ok := payload["attachments"].([]any); ok {
					for _, item := range rawAtts {
						if m, ok := item.(map[string]any); ok {
							rec.Attachments = append(rec.Attachments, m)
						}
					}
				}
				mu.Lock()
				captured = append(captured, rec)
				mu.Unlock()

				replyText := "ok"
				var errEvent map[string]any
				if handler != nil {
					replyText, errEvent = handler(attempt, r, msgType, payload)
				}
				if errEvent != nil {
					writeEv(errEvent)
					return
				}
				writeEv(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m_detail"}})
				writeEv(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": replyText}})
				writeEv(map[string]any{"type": "turn_usage", "payload": map[string]any{"input_tokens": 120, "output_tokens": 24, "total_tokens": 144}})
				writeEv(map[string]any{"type": "message_complete", "payload": map[string]any{"continuation_expected": false}})
				writeEv(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &dialCount, &captured, &mu
}

func testDummyTools() []Tool {
	return []Tool{{
		Type: "function",
		Function: ToolFunction{
			Name:        "local_read_file",
			Description: "Read local workspace file.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
				},
				"required": []string{"path"},
			},
		},
	}}
}

func doProxyJSONCall(
	t *testing.T,
	client *http.Client,
	url string,
	headers map[string]string,
	body any,
) (int, []byte) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// TestAgentSwitch_ThreadBoundaryMatrix 验证 P0 全部场景：
// 默认->A、A->默认、A->B、B->B（复用）、无关会话保留，涵盖 OpenAI 与 Anthropic 入口及进程重启重建。
func TestAgentSwitch_ThreadBoundaryMatrix(t *testing.T) {
	var seq atomic.Int32
	wsSrv, _, captured, capMu := startDetailedMockUpstream(t, func(attempt int32, r *http.Request, msgType string, payload map[string]any) (string, map[string]any) {
		n := seq.Add(1)
		return fmt.Sprintf("reply-%d", n), nil
	})

	dbPath := filepath.Join(t.TempDir(), "p0_matrix.db")
	st1, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open #1: %v", err)
	}
	if err := st1.Save(context.Background(), &store.Account{
		Email: "p0@example.com", Provider: "social",
		WSHost: "ws://" + strings.TrimPrefix(wsSrv.URL, "http://"), WSPath: "/ws",
		AccessToken: "jwt-p0", ActorARN: "arn:aws:quick:us-east-1:p0", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("st1.Save: %v", err)
	}
	// 写入一条不相关的其他持久化会话，确认切换 Agent 不会误清其他会话。
	if err := st1.UpsertConversation(context.Background(), &store.ConversationRecord{
		Key: "h:unrelated-other-session", ThreadID: "tid-unrelated", AccountID: 1,
		BaseHash: "v2::", ChatAgentID: "agent-A", ChatAgentBound: true,
		MessageHashes: []string{"nl:abc"}, LastAssistantHash: "asst", LastUsed: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("UpsertConversation unrelated: %v", err)
	}

	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
	newServerHTTP := func(db *store.Store, chatAgentID string) *httptest.Server {
		cfg := config.Default()
		cfg.SessionReuse = true
		cfg.ChatAgentID = chatAgentID
		srv := NewServer(&cfg, accounts.New(db, &cfg))
		h := httptest.NewServer(srv.Handler())
		return h
	}

	// 1. 默认 Agent（空）发送首轮
	h1 := newServerHTTP(st1, "")
	msgs := []map[string]any{{"role": "user", "content": "第一轮问题"}}
	code, _ := doProxyJSONCall(t, client, h1.URL+"/v1/chat/completions", map[string]string{"Authorization": "Bearer k-p0"}, map[string]any{
		"model": "aws-quick-smart", "messages": msgs, "tools": testDummyTools(),
	})
	if code != http.StatusOK {
		t.Fatalf("round1 default agent status=%d", code)
	}
	h1.Close()
	_ = st1.Close()

	// 2. 重启切换为 agent-A -> 禁止把默认 Agent 旧线程发往 agent-A，必须新建并带完整历史与完整工具表
	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open #2: %v", err)
	}
	h2 := newServerHTTP(st2, "agent-A")
	msgs = append(msgs,
		map[string]any{"role": "assistant", "content": "reply-1"},
		map[string]any{"role": "user", "content": "第二轮（切换至 A）"},
	)
	code, _ = doProxyJSONCall(t, client, h2.URL+"/v1/chat/completions", map[string]string{"Authorization": "Bearer k-p0"}, map[string]any{
		"model": "aws-quick-smart", "messages": msgs, "tools": testDummyTools(),
	})
	if code != http.StatusOK {
		t.Fatalf("round2 agent-A status=%d", code)
	}
	h2.Close()
	_ = st2.Close()

	// 3. 重启切换为 agent-B -> 必须再建新线程（Anthropic 入口测试）
	st3, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open #3: %v", err)
	}
	h3 := newServerHTTP(st3, "agent-B")
	msgs = append(msgs,
		map[string]any{"role": "assistant", "content": "reply-2"},
		map[string]any{"role": "user", "content": "第三轮（A->B）"},
	)
	code, _ = doProxyJSONCall(t, client, h3.URL+"/v1/messages", map[string]string{"Authorization": "Bearer k-p0"}, map[string]any{
		"model": "claude-opus-5", "max_tokens": 256, "messages": msgs,
		"tools": []map[string]any{{"name": "local_read_file", "description": "Read local workspace file.", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}},
	})
	if code != http.StatusOK {
		t.Fatalf("round3 agent-B status=%d", code)
	}
	h3.Close()
	_ = st3.Close()

	// 4. 重启仍为 agent-B（同 Agent）-> 应当复用第 3 轮线程且只发送增量
	st4, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open #4: %v", err)
	}
	h4 := newServerHTTP(st4, "agent-B")
	msgs = append(msgs,
		map[string]any{"role": "assistant", "content": "reply-3"},
		map[string]any{"role": "user", "content": "第四轮（B->B 同 Agent）"},
	)
	code, _ = doProxyJSONCall(t, client, h4.URL+"/v1/chat/completions", map[string]string{"Authorization": "Bearer k-p0"}, map[string]any{
		"model": "aws-quick-smart", "messages": msgs, "tools": testDummyTools(),
	})
	if code != http.StatusOK {
		t.Fatalf("round4 agent-B->B status=%d", code)
	}
	h4.Close()
	_ = st4.Close()

	// 5. 重启从 agent-B 切回默认 Agent（""）-> 必须建新线程、完整历史、chat_agent_id == ""
	st5, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open #5: %v", err)
	}
	defer st5.Close()
	h5 := newServerHTTP(st5, "")
	defer h5.Close()
	msgs = append(msgs,
		map[string]any{"role": "assistant", "content": "reply-4"},
		map[string]any{"role": "user", "content": "第五轮（B->默认）"},
	)
	code, _ = doProxyJSONCall(t, client, h5.URL+"/v1/chat/completions", map[string]string{"Authorization": "Bearer k-p0"}, map[string]any{
		"model": "aws-quick-smart", "messages": msgs, "tools": testDummyTools(),
	})
	if code != http.StatusOK {
		t.Fatalf("round5 B->default status=%d", code)
	}

	// 验证无关会话仍然完整保留在 SQLite 中
	unrelated, err := st5.GetConversation(context.Background(), "h:unrelated-other-session")
	if err != nil || unrelated == nil || unrelated.ThreadID != "tid-unrelated" {
		t.Fatalf("expected unrelated conversation to stay intact, got %+v err=%v", unrelated, err)
	}

	capMu.Lock()
	turns := append([]detailedCapturedTurn(nil), (*captured)...)
	capMu.Unlock()
	if len(turns) != 5 {
		t.Fatalf("expected 5 upstream turns, got %d", len(turns))
	}

	// 第 1 轮：默认
	if turns[0].ChatAgentID != "" || !turns[0].HasSessionCreate || turns[0].ThreadID == "" {
		t.Fatalf("turn 0 unexpected: %+v", turns[0])
	}
	// 第 2 轮（默认->A）：绝不可复用第 1 轮线程，必须携带完整历史及工具表
	if turns[1].ChatAgentID != "agent-A" || !turns[1].HasSessionCreate || turns[1].ThreadID == turns[0].ThreadID {
		t.Fatalf("turn 1 (default->A) mixed thread or failed rebuild: turn0=%+v turn1=%+v", turns[0], turns[1])
	}
	if !strings.Contains(turns[1].Text, "第一轮问题") || !strings.Contains(turns[1].Text, `"local_read_file"`) {
		t.Fatalf("turn 1 missing full history or tool schema: %q", turns[1].Text)
	}
	// 第 3 轮（A->B）：绝不可复用第 1 或第 2 轮线程
	if turns[2].ChatAgentID != "agent-B" || !turns[2].HasSessionCreate || turns[2].ThreadID == turns[1].ThreadID || turns[2].ThreadID == turns[0].ThreadID {
		t.Fatalf("turn 2 (A->B) mixed thread: %+v", turns[2])
	}
	if !strings.Contains(turns[2].Text, "第二轮（切换至 A）") || !strings.Contains(turns[2].Text, `"local_read_file"`) {
		t.Fatalf("turn 2 missing full history or tool schema: %q", turns[2].Text)
	}
	// 第 4 轮（B->B 同 Agent）：必须精准复用 turns[2].ThreadID，且 HasSessionCreate == false、只发送新增内容
	if turns[3].ChatAgentID != "agent-B" || turns[3].HasSessionCreate || turns[3].ThreadID != turns[2].ThreadID {
		t.Fatalf("turn 3 (B->B) expected reuse tid=%q, got %+v", turns[2].ThreadID, turns[3])
	}
	if strings.Contains(turns[3].Text, "第一轮问题") || !strings.Contains(turns[3].Text, "第四轮（B->B 同 Agent）") {
		t.Fatalf("turn 3 delta incorrect: %q", turns[3].Text)
	}
	// 第 5 轮（B->默认）：绝不可复用 turns[2].ThreadID
	if turns[4].ChatAgentID != "" || !turns[4].HasSessionCreate || turns[4].ThreadID == turns[2].ThreadID || turns[4].ThreadID == turns[1].ThreadID {
		t.Fatalf("turn 4 (B->default) mixed thread: %+v", turns[4])
	}
	if !strings.Contains(turns[4].Text, "第四轮（B->B 同 Agent）") || !strings.Contains(turns[4].Text, `"local_read_file"`) {
		t.Fatalf("turn 4 missing full history or tool schema: %q", turns[4].Text)
	}

	// 全局零容忍校验：任何旧线程 + 不同 Agent 组合出现次数必须严格为 0
	ownerByThread := make(map[string]string)
	for i, tr := range turns {
		if prevAgent, seen := ownerByThread[tr.ThreadID]; seen && prevAgent != tr.ChatAgentID {
			t.Fatalf("turn %d reused thread %q across different agents (%q -> %q)", i, tr.ThreadID, prevAgent, tr.ChatAgentID)
		}
		ownerByThread[tr.ThreadID] = tr.ChatAgentID
	}
}

// TestAgentSwitch_ExplicitHeadersAndLegacyDB 验证显式会话头与老数据库迁移记录：
// 1) X-Conversation-Id 跨 Agent 时不混淆，自动安全废弃旧映射并用新线程发送；
// 2) X-Thread-Id 遇 Agent 冲突（A->B / A->默认 / 跨 key 复用 / 旧库未知归属 chat_agent_bound=0）时明确返回 409 且 0 次发出冲突帧；
// 3) 旧库无绑定常规会话首轮一次性安全重建，次轮恢复正常复用。
func TestAgentSwitch_ExplicitHeadersAndLegacyDB(t *testing.T) {
	var seq atomic.Int32
	wsSrv, _, captured, capMu := startDetailedMockUpstream(t, func(attempt int32, r *http.Request, msgType string, payload map[string]any) (string, map[string]any) {
		return fmt.Sprintf("ok-%d", seq.Add(1)), nil
	})

	dbPath := filepath.Join(t.TempDir(), "p0_headers.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	if err := st.Save(context.Background(), &store.Account{
		Email: "hdr@example.com", Provider: "social",
		WSHost: "ws://" + strings.TrimPrefix(wsSrv.URL, "http://"), WSPath: "/ws",
		AccessToken: "jwt-hdr", ActorARN: "arn:aws:quick:us-east-1:hdr", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("st.Save: %v", err)
	}

	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
	cfg := config.Default()
	cfg.SessionReuse = true
	cfg.ChatAgentID = "agent-A"
	srv := NewServer(&cfg, accounts.New(st, &cfg))
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	// 1. 显式 X-Conversation-Id 在 agent-A 建线程，随后切到 agent-B 必须自动重建新线程
	msgsCID := []map[string]any{{"role": "user", "content": "CID首轮"}}
	code, _ := doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{
		"Authorization":     "Bearer k1",
		"X-Conversation-Id": "conv-shared-1",
	}, map[string]any{"model": "aws-quick-smart", "messages": msgsCID})
	if code != http.StatusOK {
		t.Fatalf("CID agent-A status=%d", code)
	}

	cfg.ChatAgentID = "agent-B"
	msgsCID = append(msgsCID,
		map[string]any{"role": "assistant", "content": "ok-1"},
		map[string]any{"role": "user", "content": "CID第二轮(B)"},
	)
	code, _ = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{
		"Authorization":     "Bearer k1",
		"X-Conversation-Id": "conv-shared-1",
	}, map[string]any{"model": "aws-quick-smart", "messages": msgsCID})
	if code != http.StatusOK {
		t.Fatalf("CID agent-B status=%d", code)
	}

	// 2. 显式 X-Thread-Id 在 agent-A 建线程
	cfg.ChatAgentID = "agent-A"
	explicitTID := "thread-explicit-A"
	msgsTID := []map[string]any{{"role": "user", "content": "TID首轮(A)"}}
	code, _ = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{
		"Authorization": "Bearer k1",
		"X-Thread-Id":   explicitTID,
	}, map[string]any{"model": "aws-quick-smart", "messages": msgsTID})
	if code != http.StatusOK {
		t.Fatalf("explicit TID agent-A status=%d", code)
	}

	// 同 Agent 续接该显式 X-Thread-Id 必须成功
	msgsTID = append(msgsTID,
		map[string]any{"role": "assistant", "content": "ok-3"},
		map[string]any{"role": "user", "content": "TID同A续接"},
	)
	code, _ = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{
		"Authorization": "Bearer k1",
		"X-Thread-Id":   explicitTID,
	}, map[string]any{"model": "aws-quick-smart", "messages": msgsTID})
	if code != http.StatusOK {
		t.Fatalf("explicit TID same agent-A status=%d", code)
	}

	// 切换为 agent-B 后仍带同一个 X-Thread-Id -> 必须 409 拒绝（Anthropic & OpenAI），0 次发送给上游
	beforeRejectCount := func() int {
		capMu.Lock()
		defer capMu.Unlock()
		return len(*captured)
	}()
	cfg.ChatAgentID = "agent-B"
	code, errBody := doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{
		"Authorization": "Bearer k1",
		"X-Thread-Id":   explicitTID,
	}, map[string]any{"model": "aws-quick-smart", "messages": msgsTID})
	if code != http.StatusConflict || !strings.Contains(string(errBody), "thread_agent_mismatch") {
		t.Fatalf("expected 409 thread_agent_mismatch on OpenAI, got status=%d body=%s", code, errBody)
	}

	// 跨 key + 同时带不同 X-Conversation-Id 但携带属于 agent-A 的 X-Thread-Id，在 Anthropic 也必须 409 拦截
	code, errBody = doProxyJSONCall(t, client, httpSrv.URL+"/v1/messages", map[string]string{
		"Authorization":     "Bearer k-other",
		"X-Conversation-Id": "conv-brand-new",
		"X-Thread-Id":       explicitTID,
	}, map[string]any{"model": "claude-opus-5", "max_tokens": 128, "messages": []map[string]any{{"role": "user", "content": "冲突测试"}}})
	if code != http.StatusConflict || !strings.Contains(string(errBody), "X-Thread-Id") {
		t.Fatalf("expected 409 on Anthropic X-Thread-Id conflict, got status=%d body=%s", code, errBody)
	}

	// 切回默认 Agent（""）仍带属于 agent-A 的 X-Thread-Id 也必须 409
	cfg.ChatAgentID = ""
	code, _ = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{
		"Authorization": "Bearer k1",
		"X-Thread-Id":   explicitTID,
	}, map[string]any{"model": "aws-quick-smart", "messages": msgsTID})
	if code != http.StatusConflict {
		t.Fatalf("expected 409 when switching A->default with explicit X-Thread-Id, got status=%d", code)
	}

	// 3. 构造老版本数据库未记录绑定的持久化会话（ChatAgentBound = false）
	legacyKey := "h:conv-legacy-unbound"
	legacyMsg := []map[string]any{{"role": "user", "content": "老库第一轮"}}
	msgFP := messageFingerprint(Message{Role: "user", Content: "老库第一轮"})
	if err := st.UpsertConversation(context.Background(), &store.ConversationRecord{
		Key: legacyKey, ThreadID: "tid-legacy-unbound", AccountID: 1,
		BaseHash: "v2::", ChatAgentID: "", ChatAgentBound: false,
		MessageHashes: []string{"nl:" + msgFP}, LastAssistantHash: assistantOutputFingerprint("老回复", nil), LastUsed: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("UpsertConversation legacy: %v", err)
	}

	// 带显式 X-Thread-Id 引用未绑定旧线程 -> 必须 409 拒绝
	code, _ = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{
		"Authorization": "Bearer k1",
		"X-Thread-Id":   "tid-legacy-unbound",
	}, map[string]any{"model": "aws-quick-smart", "messages": legacyMsg})
	if code != http.StatusConflict {
		t.Fatalf("expected 409 for unbound legacy X-Thread-Id, got %d", code)
	}

	capMu.Lock()
	afterRejectCount := len(*captured)
	capMu.Unlock()
	if afterRejectCount != beforeRejectCount {
		t.Fatalf("rejected X-Thread-Id conflicts leaked %d upstream frames", afterRejectCount-beforeRejectCount)
	}

	// 常规会话访问老库未绑定记录（即使当前也是默认 Agent ""）：一次性安全重建新线程，且次轮恢复正常复用
	legacyNext := append(legacyMsg,
		map[string]any{"role": "assistant", "content": "老回复"},
		map[string]any{"role": "user", "content": "升级后第二轮"},
	)
	code, _ = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{
		"Authorization":     "Bearer k1",
		"X-Conversation-Id": "conv-legacy-unbound",
	}, map[string]any{"model": "aws-quick-smart", "messages": legacyNext})
	if code != http.StatusOK {
		t.Fatalf("legacy first rebuild status=%d", code)
	}
	legacyRound3 := append(legacyNext,
		map[string]any{"role": "assistant", "content": "ok-5"},
		map[string]any{"role": "user", "content": "升级后第三轮（应恢复复用）"},
	)
	code, _ = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{
		"Authorization":     "Bearer k1",
		"X-Conversation-Id": "conv-legacy-unbound",
	}, map[string]any{"model": "aws-quick-smart", "messages": legacyRound3})
	if code != http.StatusOK {
		t.Fatalf("legacy second turn reuse status=%d", code)
	}

	capMu.Lock()
	turns := append([]detailedCapturedTurn(nil), (*captured)...)
	capMu.Unlock()
	// 期望：CID(A), CID(B), TID(A), TID(A), legacyRebuild, legacyReuse => 共 6 帧
	if len(turns) != 6 {
		t.Fatalf("expected 6 upstream frames, got %d", len(turns))
	}
	if turns[0].ThreadID == turns[1].ThreadID || turns[1].ChatAgentID != "agent-B" || !turns[1].HasSessionCreate {
		t.Fatalf("CID cross-agent rebuild check failed: t0=%+v t1=%+v", turns[0], turns[1])
	}
	if turns[4].ThreadID == "tid-legacy-unbound" || !turns[4].HasSessionCreate || !strings.Contains(turns[4].Text, "老库第一轮") {
		t.Fatalf("legacy record did not trigger safe rebuild: %+v", turns[4])
	}
	if turns[5].ThreadID != turns[4].ThreadID || turns[5].HasSessionCreate || strings.Contains(turns[5].Text, "老库第一轮") {
		t.Fatalf("legacy record failed to resume after rebuild: t4=%+v t5=%+v", turns[4], turns[5])
	}
}

// TestAgentSwitch_RetryDoesNotReintroduceOldThread 验证切换 Agent 后即使首轮遭遇临时故障切号重试，
// 重试帧依然严格使用新线程、完整历史和完整工具表，绝不会把旧线程 ID 带回来。
func TestAgentSwitch_RetryDoesNotReintroduceOldThread(t *testing.T) {
	var seq atomic.Int32
	wsSrv, _, captured, capMu := startDetailedMockUpstream(t, func(attempt int32, r *http.Request, msgType string, payload map[string]any) (string, map[string]any) {
		n := seq.Add(1)
		if n == 2 {
			return "", map[string]any{"type": "error", "payload": map[string]any{"code": "ServiceUnavailableException", "message": "temporary issue"}}
		}
		return fmt.Sprintf("ans-%d", n), nil
	})

	dbPath := filepath.Join(t.TempDir(), "p0_retry.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	for _, email := range []string{"a1@example.com", "a2@example.com"} {
		if err := st.Save(context.Background(), &store.Account{
			Email: email, Provider: "social",
			WSHost: "ws://" + strings.TrimPrefix(wsSrv.URL, "http://"), WSPath: "/ws",
			AccessToken: "jwt-" + email, ActorARN: "arn:aws:quick:us-east-1:" + email, ExpiresAt: time.Now().Add(time.Hour).Unix(),
		}); err != nil {
			t.Fatalf("st.Save(%s): %v", email, err)
		}
	}

	cfg := config.Default()
	cfg.SessionReuse = true
	cfg.MaxAttempts = 3
	cfg.ChatAgentID = "agent-A"
	srv := NewServer(&cfg, accounts.New(st, &cfg))
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}

	msgs := []map[string]any{{"role": "user", "content": "轮1"}}
	code, _ := doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{
		"Authorization":     "Bearer k-retry",
		"X-Conversation-Id": "conv-retry-agent",
	}, map[string]any{"model": "aws-quick-smart", "messages": msgs, "tools": testDummyTools()})
	if code != http.StatusOK {
		t.Fatalf("round1 status=%d", code)
	}

	// 切换至 agent-B，触发一次临时失败与自动重试
	cfg.ChatAgentID = "agent-B"
	msgs = append(msgs,
		map[string]any{"role": "assistant", "content": "ans-1"},
		map[string]any{"role": "user", "content": "轮2(切至B遇临时故障)"},
	)
	code, _ = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{
		"Authorization":     "Bearer k-retry",
		"X-Conversation-Id": "conv-retry-agent",
	}, map[string]any{"model": "aws-quick-smart", "messages": msgs, "tools": testDummyTools()})
	if code != http.StatusOK {
		t.Fatalf("round2 after retry status=%d", code)
	}

	capMu.Lock()
	turns := append([]detailedCapturedTurn(nil), (*captured)...)
	capMu.Unlock()
	if len(turns) != 3 {
		t.Fatalf("expected 3 captured turns (1 initial + 1 failed + 1 retry), got %d", len(turns))
	}
	oldTID := turns[0].ThreadID
	for i := 1; i <= 2; i++ {
		if turns[i].ThreadID == oldTID || turns[i].ChatAgentID != "agent-B" || !turns[i].HasSessionCreate {
			t.Fatalf("turn %d carried old thread or wrong agent on retry: old=%q turn=%+v", i, oldTID, turns[i])
		}
		if !strings.Contains(turns[i].Text, "轮1") || !strings.Contains(turns[i].Text, `"local_read_file"`) {
			t.Fatalf("turn %d lost full transcript or tools on retry: %q", i, turns[i].Text)
		}
	}
}
