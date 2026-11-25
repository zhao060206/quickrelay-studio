package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// TestResumeDelta_SkipsPreviousAssistantDuplicate 验证续聊增量会跳过上一轮模型已生成的长 Assistant 回复与工具 JSON，
// 仅发送带精简 target 摘要的 Tool result，同时保留真正多段后续 Assistant 消息。
func TestResumeDelta_SkipsPreviousAssistantDuplicate(t *testing.T) {
	hugeContent := strings.Repeat("A", 12000)
	allMsgs := []Message{
		{Role: "user", Content: "先修改 main.go"},
		{
			Role:    "assistant",
			Content: "我将写入详细计算脚本。",
			ToolCalls: []ToolCall{{
				ID:   "call_write_01",
				Type: "function",
				Function: FunctionCall{
					Name:      "Write",
					Arguments: `{"file_path":"e:/proj/calc.py","content":"` + hugeContent + `"}`,
				},
			}},
		},
		{Role: "tool", ToolCallID: "call_write_01", Content: "written 12000 bytes"},
	}

	delta := allMsgs[1:]
	prompt, imgs, err := formatResumeDeltaWithImages(context.Background(), allMsgs, delta)
	if err != nil {
		t.Fatalf("formatResumeDeltaWithImages failed: %v", err)
	}
	if len(imgs) != 0 {
		t.Fatalf("expected 0 attachments, got %d", len(imgs))
	}
	if strings.Contains(prompt, hugeContent) || strings.Contains(prompt, "Assistant tool call") {
		t.Fatalf("resumed delta duplicated previous assistant payload (len=%d)", len(prompt))
	}
	wantSub := "Tool result (call_id=call_write_01, name=Write, target=e:/proj/calc.py): written 12000 bytes"
	if prompt != wantSub {
		t.Fatalf("got prompt %q, want %q", prompt, wantSub)
	}

	// 验证连续多条对话切片中，仅跳过首条重复 assistant，后续真正新增的 assistant 仍保留
	multiDelta := []Message{
		allMsgs[1],
		allMsgs[2],
		{Role: "assistant", Content: "这是后续新增说明"},
		{Role: "user", Content: "好，下一步"},
	}
	p2, _, err := formatResumeDeltaWithImages(context.Background(), append(allMsgs, multiDelta[2:]...), multiDelta)
	if err != nil {
		t.Fatalf("multiDelta failed: %v", err)
	}
	if strings.Contains(p2, hugeContent) {
		t.Fatalf("leading assistant duplicate was not skipped")
	}
	if !strings.Contains(p2, "Assistant: 这是后续新增说明") || !strings.Contains(p2, "User: 好，下一步") {
		t.Fatalf("subsequent messages missing in multiDelta: %q", p2)
	}

	// 验证日志摘要能精准呈现工具目标而非卡死在旧的 user 文本
	prev := requestLogPreview(append([]Message{{Role: "user", Content: "继续"}}, allMsgs[1:]...))
	if !strings.Contains(prev, "[Tool -> Write(e:/proj/calc.py)]") {
		t.Fatalf("unexpected requestLogPreview: %q", prev)
	}
}

// TestStreamToolBridge_EarlyToolStartClosesThinking 验证 /v1/messages 在模型写长工具 JSON 刚开始时
// 即刻闭合前序 thinking 块并提前发送 tool_use 的 content_block_start，消除长达数分钟零帧假死。
func TestStreamToolBridge_EarlyToolStartClosesThinking(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	gate := make(chan struct{})
	wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var mu sync.Mutex
		writeEv := func(v any) {
			mu.Lock()
			defer mu.Unlock()
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
					writeEv(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m_early"}})
					writeEv(map[string]any{"type": "thinking_delta", "payload": map[string]any{"delta": "先规划文件结构。"}})
					writeEv(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": `<client_tool_call>{"name":"Write","arguments":{"file_path":"a.txt","content":"`}})
					// 挂起等待下游确实已提前收到 tool_use start 后才继续发闭合标签
					<-gate
					writeEv(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": `hello"}}</client_tool_call>`}})
					writeEv(map[string]any{"type": "message_complete", "payload": map[string]any{"continuation_expected": false}})
					writeEv(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
				}()
			}
		}
	}))
	defer wsSrv.Close()

	httpSrv, client := setupMockProxyHTTPServer(t, wsSrv)
	reqBody, _ := json.Marshal(map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 1024,
		"stream":     true,
		"tools": []map[string]any{{
			"name":         "Write",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}},
		}},
		"messages": []map[string]any{{"role": "user", "content": "写文件"}},
	})
	resp, err := client.Post(httpSrv.URL+"/v1/messages", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		close(gate)
		t.Fatalf("POST /v1/messages: %v", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var events []string
	released := false
	for scanner.Scan() {
		line := scanner.Text()
		events = append(events, line)
		if !released && strings.Contains(line, `"type":"tool_use"`) && strings.Contains(line, `"name":"Write"`) {
			released = true
			close(gate)
		}
	}
	if !released {
		close(gate)
		t.Fatalf("did not receive early tool_use content_block_start before closing tag; events:\n%s", strings.Join(events, "\n"))
	}
	joined := strings.Join(events, "\n")
	for _, want := range []string{`"signature_delta"`, `"type":"tool_use"`, `"type":"input_json_delta"`, `\"file_path\":\"a.txt\"`, `"stop_reason":"tool_use"`, `event: message_stop`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("stream missing %q\nstream:\n%s", want, joined)
		}
	}
}

// TestSessionPreserved_OnCancelAndTransientErrors 验证按 Esc 取消、超时、单轮内容过滤拦截后不会误删既有会话缓存，
// 同长度重发或下一轮仍能无缝沿用原 thread_id；仅在会话不存在或账号失效时重置。
func TestSessionPreserved_OnCancelAndTransientErrors(t *testing.T) {
	conv := &conversation{
		threadID:  "tid-keep-999",
		accountID: 1,
		baseHash:  "v2:abc:def",
		msgHashes: nonLeadingMessageFingerprints([]Message{{Role: "user", Content: "hi"}}),
	}

	if shouldResetConversation(conv, context.Canceled) {
		t.Fatalf("context.Canceled must NOT reset conversation")
	}
	if shouldResetConversation(conv, context.DeadlineExceeded) {
		t.Fatalf("context.DeadlineExceeded must NOT reset conversation")
	}
	if shouldResetConversation(conv, &UpstreamError{Err: errors.New("upstream error: The model's content filter stopped this reply.")}) {
		t.Fatalf("content filter on established conversation must NOT reset conversation")
	}
	if !shouldResetConversation(conv, &UpstreamError{Code: "SessionNotFound", Err: errors.New("session expired")}) {
		t.Fatalf("SessionNotFound must reset conversation")
	}
	if !shouldResetConversation(conv, &UpstreamError{StatusCode: http.StatusForbidden, Err: errors.New("forbidden")}) {
		t.Fatalf("403 forbidden must reset conversation")
	}
	if shouldResetConversation(&conversation{}, context.Canceled) {
		t.Fatalf("empty conv with Canceled should still return false")
	}

	// 验证真实服务端在客户端取消后保留第 1 轮已保存的会话指纹，下一轮仍命中 session_reused=true
	dbPath := filepath.Join(t.TempDir(), "preserve.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	srv := NewServer(&config.Config{SessionReuse: true, SessionTTL: config.Duration(time.Hour)}, accounts.New(st, &config.Config{}))
	req1 := &ChatRequest{
		Model:    "claude-opus-5",
		Messages: []Message{{Role: "user", Content: "第一轮问题"}},
	}
	t1, err := srv.prepareTurn(httptest.NewRequest("POST", "/v1/messages", nil), req1, "anthropic", "/v1/messages", "msg_")
	if err != nil {
		t.Fatalf("prepareTurn 1: %v", err)
	}
	t1.usedAccount = &store.Account{ID: 3}
	t1.saveConversation(ChatResult{ThreadID: "tid-preserved-01"})
	t1.Close()

	// 模拟第 2 轮发生 context.Canceled：按新策略不应重置
	req2 := &ChatRequest{
		Model: "claude-opus-5",
		Messages: []Message{
			{Role: "user", Content: "第一轮问题"},
			{Role: "assistant", Content: "第一轮回答"},
			{Role: "user", Content: "第二轮问题"},
		},
	}
	t2, err := srv.prepareTurn(httptest.NewRequest("POST", "/v1/messages", nil), req2, "anthropic", "/v1/messages", "msg_")
	if err != nil {
		t.Fatalf("prepareTurn 2: %v", err)
	}
	if !t2.resume || t2.in.ThreadID != "tid-preserved-01" {
		t2.Close()
		t.Fatalf("expected t2 resume tid-preserved-01")
	}
	if shouldResetConversation(t2.conv, context.Canceled) {
		srv.sessions.reset(t2.conv)
	}
	t2.Close()

	// 重新发起同一第 2 轮请求：仍能 100% 命中原 thread_id 复用
	t3, err := srv.prepareTurn(httptest.NewRequest("POST", "/v1/messages", nil), req2, "anthropic", "/v1/messages", "msg_")
	if err != nil {
		t.Fatalf("prepareTurn 3: %v", err)
	}
	defer t3.Close()
	if !t3.resume || t3.in.ThreadID != "tid-preserved-01" {
		t.Fatalf("expected preserved thread tid-preserved-01 after cancel, got resume=%t tid=%q", t3.resume, t3.in.ThreadID)
	}
}

// TestUpstreamReadEvents_NormalClose1000 验证上游未发 turn_completed 直接以 WebSocket close 1000 结束流时：
// 1) 若已有内容，直接判定本轮正常完成；
// 2) 若零内容，触发 ErrEmptyResponse 并按 retryTemporary 自动重试而不误判失败。
func TestUpstreamReadEvents_NormalClose1000(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	var callNum atomic.Int32

	wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
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
				n := callNum.Add(1)
				if n == 1 {
					// 场景 1：输出正文后直接发 close 1000（省略 turn_completed）
					_ = conn.WriteMessage(websocket.BinaryMessage, encodeMockAIDPServerEvent(map[string]any{
						"type":    "text_delta",
						"payload": map[string]any{"delta": "normal close output"},
					}))
				}
				// 场景 2（n==2）：零输出直接 close 1000 -> 上层应转 ErrEmptyResponse 自动重试
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Streaming completed successfully."),
					time.Now().Add(time.Second))
				return
			}
		}
	}))
	defer wsSrv.Close()

	cfg := config.Default()
	u := NewUpstream(&cfg)
	sess := &Session{
		JWT:      "test-jwt",
		WSHost:   "ws://" + strings.TrimPrefix(wsSrv.URL, "http://"),
		WSPath:   "/ws",
		Provider: "social",
		ActorARN: "arn:aws:quick:us-east-1:test",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := u.Chat(ctx, sess, ChatInput{Prompt: "test 1", Model: "smart", CreateSession: true}, nil)
	if err != nil {
		t.Fatalf("expected normal close 1000 with text to succeed, got err: %v", err)
	}
	if res.Text != "normal close output" {
		t.Fatalf("got text %q, want %q", res.Text, "normal close output")
	}

	// 验证 close 1000 若无文本导致 ErrEmptyResponse 时允许自动重试
	close1000Err := &websocket.CloseError{Code: websocket.CloseNormalClosure, Text: "Streaming completed successfully."}
	if action := retryActionFor(close1000Err); action != retryTemporary {
		t.Fatalf("expected retryTemporary for close 1000 fallback, got %v", action)
	}
	if action := retryActionFor(ErrEmptyResponse); action != retryTemporary {
		t.Fatalf("expected retryTemporary for ErrEmptyResponse, got %v", action)
	}
}

// TestAnthropicDocumentBlock_PDFAndText 验证 Anthropic /v1/messages 传入 type:"document"（含 Base64 PDF 与文本文档）
// 能正确提取、保留文本或解码 PDF 作为 type:"file", mimeType:"application/pdf" 附件，不再静默丢失。
func TestAnthropicDocumentBlock_PDFAndText(t *testing.T) {
	pdfBytes := []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\n%%EOF\n")
	pdfB64 := base64.StdEncoding.EncodeToString(pdfBytes)
	txtB64 := base64.StdEncoding.EncodeToString([]byte("这是一段纯文本文档内容"))

	ar := anthropicMessagesRequest{
		Model:  "claude-opus-5",
		Stream: false,
		Messages: []anthropicMessage{{
			Role: "user",
			Content: json.RawMessage(`[
				{"type":"text","text":"请审阅附件："},
				{"type":"document","title":"初稿.pdf","source":{"type":"base64","media_type":"application/pdf","data":"` + pdfB64 + `"}},
				{"type":"document","title":"notes.txt","source":{"type":"base64","media_type":"text/plain","data":"` + txtB64 + `"}}
			]`),
		}},
	}

	cr := anthropicToChatRequest(&ar)
	prompt, atts, err := formatPromptWithImages(context.Background(), cr.Messages)
	if err != nil {
		t.Fatalf("formatPromptWithImages failed: %v", err)
	}
	if !strings.Contains(prompt, "这是一段纯文本文档内容") {
		t.Fatalf("plain text document missing in prompt: %q", prompt)
	}
	if len(atts) != 1 {
		t.Fatalf("expected 1 PDF document attachment, got %d", len(atts))
	}
	if !atts[0].IsDocument || atts[0].MIMEType != "application/pdf" || !strings.HasSuffix(atts[0].Name, ".pdf") {
		t.Fatalf("unexpected document attachment metadata: %#v", atts[0])
	}
	if !strings.Contains(prompt, "[attached document: "+atts[0].Name+"]") {
		t.Fatalf("prompt missing attached document marker: %q", prompt)
	}
	// 验证 contentText 对无 collector 调用不泄露 base64
	plainSummary := contentText(cr.Messages[0].Content)
	if strings.Contains(plainSummary, pdfB64) {
		t.Fatalf("contentText leaked raw PDF base64")
	}
	if !strings.Contains(plainSummary, "[document: 初稿.pdf]") {
		t.Fatalf("contentText missing document summary placeholder: %q", plainSummary)
	}
}
