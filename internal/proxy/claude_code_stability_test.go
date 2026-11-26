package proxy

import (
	"bufio"
	"bytes"
	"context"
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

// TestToolDefinitionsCompaction_PreservesSchemaAndSavesSpace 验证大工具表自动触发无损语义瘦身，
// 在大幅压缩体积的同时保留全部工具名、参数键、enum 与 required 约束，并保证小工具表与原始请求不被篡改。
func TestToolDefinitionsCompaction_PreservesSchemaAndSavesSpace(t *testing.T) {
	// 1. 构造 35 个类似 Claude Code 的大型工具定义（含冗长 <example> 与换行说明，总长 >45KB）
	hugeTools := make([]Tool, 0, 35)
	for i := 0; i < 35; i++ {
		name := fmt.Sprintf("cc_tool_%02d", i)
		desc := fmt.Sprintf(
			"Execute local workspace operation %02d across authorized project directories. "+
				"Always prefer direct local file reads and edits over guesswork.\n\n"+
				"<example>\nUser: run step %02d\nAssistant: call cc_tool_%02d with mode=\"fast\"\n%s\n</example>\n"+
				"```json\n{\"mode\":\"fast\",\"path\":\"E:\\\\proj\\\\main.go\"}\n```\n"+
				"Additional extended usage details %s.",
			i, i, i, strings.Repeat("Detailed step-by-step walkthrough line. ", 14), strings.Repeat("note ", 40),
		)
		hugeTools = append(hugeTools, Tool{
			Type: "function",
			Function: ToolFunction{
				Name:        name,
				Description: desc,
				Parameters: map[string]any{
					"$schema":              "https://json-schema.org/draft/2020-12/schema",
					"title":                fmt.Sprintf("Params_%02d", i),
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]any{
						"target_path": map[string]any{
							"type":        "string",
							"title":       "TargetPath",
							"description": "Absolute local filesystem path to inspect or modify. " + strings.Repeat("Do not use relative guesses. ", 8),
						},
						"action_mode": map[string]any{
							"type":        "string",
							"enum":        []string{"read", "patch", "verify"},
							"description": "Operation mode selector for the target action. " + strings.Repeat("Must match enum. ", 6),
						},
					},
					"required": []string{"target_path", "action_mode"},
				},
			},
		})
	}
	reqHuge := &ChatRequest{Tools: hugeTools, ToolChoice: "auto"}
	origBytes, err := json.Marshal(map[string]any{"tools": collectToolDefinitions(reqHuge.Tools, nil), "tool_choice": reqHuge.ToolChoice})
	if err != nil {
		t.Fatalf("marshal original tools: %v", err)
	}
	if len(origBytes) <= 45*1024 {
		t.Fatalf("expected huge tools raw payload > 45KB, got %d bytes", len(origBytes))
	}

	gotHuge := forwardToolDefinitions(reqHuge)
	if len(gotHuge) >= len(origBytes)*4/10 {
		t.Fatalf("expected >60%% reduction for huge tools, orig=%d got=%d", len(origBytes), len(gotHuge))
	}
	for i := 0; i < 35; i++ {
		name := fmt.Sprintf("cc_tool_%02d", i)
		if !strings.Contains(gotHuge, `"`+name+`"`) {
			t.Fatalf("compacted tool definitions missing tool %q", name)
		}
	}
	for _, wantSchema := range []string{`"target_path"`, `"action_mode"`, `["read","patch","verify"]`, `["target_path","action_mode"]`} {
		if !strings.Contains(gotHuge, wantSchema) {
			t.Fatalf("compacted tool definitions missing schema token %q", wantSchema)
		}
	}
	if strings.Contains(gotHuge, `\u003cexample\u003e`) || strings.Contains(gotHuge, "<example>") || strings.Contains(gotHuge, `"$schema"`) || strings.Contains(gotHuge, `"additionalProperties":false`) {
		t.Fatalf("compacted tool definitions still contain stripped example or redundant metadata")
	}
	// 验证原始 reqHuge未被原地修改
	if !strings.Contains(hugeTools[0].Function.Description, "<example>") {
		t.Fatalf("original tool description mutated")
	}
	if _, ok := hugeTools[0].Function.Parameters.(map[string]any)["$schema"]; !ok {
		t.Fatalf("original tool schema mutated")
	}

	// 2. 小于 16KB 的小工具表应原样保留
	smallReq := &ChatRequest{
		Tools: []Tool{{
			Type: "function",
			Function: ToolFunction{
				Name:        "Read",
				Description: "Read file.\n<example>demo</example>",
				Parameters: map[string]any{
					"$schema":    "https://json-schema.org/draft/2020-12/schema",
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
				},
			},
		}},
	}
	gotSmall := forwardToolDefinitions(smallReq)
	if (!strings.Contains(gotSmall, `\u003cexample\u003edemo\u003c/example\u003e`) && !strings.Contains(gotSmall, `<example>demo</example>`)) || !strings.Contains(gotSmall, `"$schema"`) {
		t.Fatalf("small tool payload (<=16KB) should remain intact, got: %s", gotSmall)
	}
}

// TestResumeToolReminder_IncludesActiveToolNames 验证续聊极简提醒携带无引号本地工具名清单与超长截断标记，
// 且严禁使用带双引号的 JSON 工具键名以确保触发与 cache_reuse_test 完全兼容。
func TestResumeToolReminder_IncludesActiveToolNames(t *testing.T) {
	names := []string{"Bash", "Read", "Edit", "Write", "Grep", "Glob"}
	shortReminder := buildResumeToolReminder("auto", "", true, names...)
	if !strings.Contains(shortReminder, "Active local tools: Bash, Read, Edit, Write, Grep, Glob") {
		t.Fatalf("reminder missing active tool names summary:\n%s", shortReminder)
	}
	if strings.Contains(shortReminder, `"Bash"`) || strings.Contains(shortReminder, `"Read"`) {
		t.Fatalf("reminder must not wrap tool names in quotes:\n%s", shortReminder)
	}

	var many []string
	for i := 0; i < 60; i++ {
		many = append(many, fmt.Sprintf("mcp__workspace__very_long_tool_name_%02d", i))
	}
	longReminder := buildResumeToolReminder("auto", "", true, many...)
	if !strings.Contains(longReminder, " more)") {
		t.Fatalf("expected truncated summary with (+N more), got:\n%s", longReminder)
	}
	if strings.Contains(longReminder, `"mcp__workspace__very_long_tool_name_00"`) {
		t.Fatalf("truncated reminder must not contain quoted tool name")
	}
}

// TestPrepareFrameWithinBudget_CompactsSkillsBeforePromptUpload 验证先压缩技能列表即可落入 63KB 预算时，
// 优先零附件直接塞入主帧而不再提前上传 prompt-*.txt。
func TestPrepareFrameWithinBudget_CompactsSkillsBeforePromptUpload(t *testing.T) {
	var skills strings.Builder
	skills.WriteString("### Available skills\n")
	for i := 0; i < 160; i++ {
		skills.WriteString(fmt.Sprintf("- skill-%03d: %s (file: r0/skill-%03d/SKILL.md)\n",
			i, strings.Repeat("Long guideline description for task dispatching and routing. ", 6), i))
	}
	guide := wrapWorkspaceGuidelines([]string{skills.String()})
	promptBody := strings.Repeat("History user and assistant turn context line. ", 250) // ~11KB (>8KB)
	if !shouldAttachPrompt(len(promptBody)) {
		t.Fatalf("expected promptBody len=%d >= attachmentMinPromptSize", len(promptBody))
	}
	toolProtocol := buildToolBridgePrompt(&ChatRequest{
		Tools: []Tool{{Type: "function", Function: ToolFunction{Name: "Bash"}}},
	}, "auto", "", true)

	payload := map[string]any{
		"text": renderTurnTextForMode(config.WorkModeGeneral, guide, toolProtocol, "", promptBody),
	}
	origBytes, _ := userMessageBytes(payload)
	if origBytes <= maxUserMessageBytes {
		t.Fatalf("expected initial frame > %d bytes, got %d", maxUserMessageBytes, origBytes)
	}

	uploads := 0
	prepareFrameWithinBudgetWithMode(payload, "msg-00011122", guide, toolProtocol, "", promptBody, "继续", nil, nil, true, config.WorkModeGeneral,
		func(name string, data []byte) (attachmentRef, error) {
			uploads++
			return attachmentRef{Name: name, Path: "/data/s3files/" + name, Size: len(data)}, nil
		})

	if uploads != 0 {
		t.Fatalf("expected 0 uploaded attachments when compacting skills fits within budget, got %d", uploads)
	}
	if _, hasAtt := payload["attachments"]; hasAtt {
		t.Fatalf("expected no attachments in payload")
	}
	if n, err := userMessageBytes(payload); err != nil || n > maxUserMessageBytes {
		t.Fatalf("expected frame to fit <= %d after skills compaction, got n=%d err=%v", maxUserMessageBytes, n, err)
	}
}

// TestAnthropicStream_ThinkingPrestreamBufferAllowsAutoCorrection 验证：
// 1) 工具模式下上游先吐出简短 thinking_delta 再越权调 run_python 时，开流前缓冲窗未提前写出 SSE 头，
//    网关成功拦截并携带可用本地工具名清单自动纠偏重发，流式输出本地 tool_use；
// 2) 当思考等待超过 thinkingPrestreamHold 时，缓冲窗自动刷出并先发 SSE 头防止客户端超时。
func TestAnthropicStream_ThinkingPrestreamBufferAllowsAutoCorrection(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	t.Run("buffers_brief_thinking_and_autocorrects_remote_tool", func(t *testing.T) {
		prevHold := thinkingPrestreamHold
		prevMax := thinkingPrestreamMaxBytes
		thinkingPrestreamHold = 5 * time.Second
		thinkingPrestreamMaxBytes = 4096
		defer func() {
			thinkingPrestreamHold = prevHold
			thinkingPrestreamMaxBytes = prevMax
		}()

		var seq atomic.Int32
		var capTexts []string
		var mu sync.Mutex

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
				mType, payload, ok := decodeClientFrame(raw)
				if !ok || mType != "user_message" {
					continue
				}
				txt, _ := payload["text"].(string)
				mu.Lock()
				capTexts = append(capTexts, txt)
				mu.Unlock()
				n := seq.Add(1)
				writeEv := func(v any) {
					_ = conn.WriteMessage(websocket.BinaryMessage, encodeMockAIDPServerEvent(v))
				}
				if n == 1 {
					writeEv(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m_try1"}})
					writeEv(map[string]any{"type": "thinking_delta", "payload": map[string]any{"delta": "先分析用户需求，准备写本地 CSS 文件。"}})
					writeEv(map[string]any{"type": "tool_start", "payload": map[string]any{"tool_id": "t_bad_py", "tool_name": "run_python"}})
					writeEv(map[string]any{"type": "tool_arguments", "payload": map[string]any{"tool_id": "t_bad_py", "arguments": map[string]any{"code": "print(1)"}}})
					time.Sleep(400 * time.Millisecond)
					return
				}
				writeEv(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m_try2"}})
				writeEv(map[string]any{"type": "thinking_delta", "payload": map[string]any{"delta": "改用本地 Write 工具执行。"}})
				writeEv(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": `<client_tool_call>{"name":"Write","arguments":{"file_path":"E:/proj/style.css","content":"body{}"}}</client_tool_call>`}})
				writeEv(map[string]any{"type": "message_complete", "payload": map[string]any{"continuation_expected": false}})
				writeEv(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
			}
		}))
		defer wsSrv.Close()

		httpSrv, client := setupMockProxyHTTPServer(t, wsSrv)
		reqBody, _ := json.Marshal(map[string]any{
			"model":      "claude-opus-5",
			"max_tokens": 512,
			"stream":     true,
			"tools": []map[string]any{
				{"name": "Write", "input_schema": map[string]any{"type": "object"}},
				{"name": "Bash", "input_schema": map[string]any{"type": "object"}},
			},
			"messages": []map[string]any{{"role": "user", "content": "创建 style.css"}},
		})
		resp, err := client.Post(httpSrv.URL+"/v1/messages", "application/json", bytes.NewReader(reqBody))
		if err != nil {
			t.Fatalf("POST /v1/messages failed: %v", err)
		}
		defer resp.Body.Close()

		raw, _ := io.ReadAll(resp.Body)
		bodyStr := string(raw)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected HTTP 200 after auto-correction, got %d: %s", resp.StatusCode, bodyStr)
		}
		if !strings.Contains(bodyStr, `"name":"Write"`) || !strings.Contains(bodyStr, "改用本地 Write 工具执行。") {
			t.Fatalf("stream missing auto-corrected Write tool or round-2 thinking:\n%s", bodyStr)
		}
		if strings.Contains(bodyStr, "先分析用户需求，准备写本地 CSS 文件。") {
			t.Fatalf("buffered round-1 thinking leaked after resetOutput:\n%s", bodyStr)
		}
		if strings.Contains(bodyStr, `"type":"error"`) {
			t.Fatalf("unexpected stream error event:\n%s", bodyStr)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(capTexts) != 2 {
			t.Fatalf("expected 2 upstream attempts, got %d", len(capTexts))
		}
		if !strings.Contains(capTexts[1], "Available local tools: Write, Bash.") {
			t.Fatalf("correction prompt missing active tool names hint:\n%s", capTexts[1])
		}
	})

	t.Run("flushes_thinking_when_hold_window_expires", func(t *testing.T) {
		prevHold := thinkingPrestreamHold
		prevMax := thinkingPrestreamMaxBytes
		thinkingPrestreamHold = 30 * time.Millisecond
		thinkingPrestreamMaxBytes = 4096
		defer func() {
			thinkingPrestreamHold = prevHold
			thinkingPrestreamMaxBytes = prevMax
		}()

		gate := make(chan struct{})
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
				mType, _, ok := decodeClientFrame(raw)
				if !ok || mType != "user_message" {
					continue
				}
				go func() {
					writeEv := func(v any) {
						_ = conn.WriteMessage(websocket.BinaryMessage, encodeMockAIDPServerEvent(v))
					}
					writeEv(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m_hold"}})
					writeEv(map[string]any{"type": "thinking_delta", "payload": map[string]any{"delta": "第一段思考，"}})
					time.Sleep(50 * time.Millisecond)
					writeEv(map[string]any{"type": "thinking_delta", "payload": map[string]any{"delta": "超过缓冲窗刷出。"}})
					<-gate
					writeEv(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "思考完毕回答。"}})
					writeEv(map[string]any{"type": "message_complete", "payload": map[string]any{"continuation_expected": false}})
					writeEv(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
				}()
			}
		}))
		defer wsSrv.Close()

		httpSrv, client := setupMockProxyHTTPServer(t, wsSrv)
		reqBody, _ := json.Marshal(map[string]any{
			"model":      "claude-opus-5",
			"max_tokens": 512,
			"stream":     true,
			"tools":      []map[string]any{{"name": "Read", "input_schema": map[string]any{"type": "object"}}},
			"messages":   []map[string]any{{"role": "user", "content": "长思考后回复"}},
		})
		resp, err := client.Post(httpSrv.URL+"/v1/messages", "application/json", bytes.NewReader(reqBody))
		if err != nil {
			close(gate)
			t.Fatalf("POST failed: %v", err)
		}
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		var lines []string
		flushedBeforeGate := false
		for scanner.Scan() {
			line := scanner.Text()
			lines = append(lines, line)
			if !flushedBeforeGate && strings.Contains(line, "超过缓冲窗刷出。") {
				flushedBeforeGate = true
				close(gate)
			}
		}
		if !flushedBeforeGate {
			close(gate)
			t.Fatalf("expected thinking to flush when hold window expired; lines:\n%s", strings.Join(lines, "\n"))
		}
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "第一段思考，") || !strings.Contains(joined, "思考完毕回答。") {
			t.Fatalf("stream missing merged thinking or final text:\n%s", joined)
		}
	})
}

// TestAnthropicStream_MultiTextBlocksPreserveConversationReuse 验证流式输出在 [text] -> [tool_use] -> [text]
// 多段 text block 交替后，下一轮回传多段 text block 时会话指纹精准对齐，仍 100% 命中 session_reused=true。
func TestAnthropicStream_MultiTextBlocksPreserveConversationReuse(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	var seq atomic.Int32
	var captured []capturedUpstreamTurn
	var capMu sync.Mutex

	wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var rec capturedUpstreamTurn
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			mType, payload, ok := decodeClientFrame(raw)
			if !ok {
				continue
			}
			switch mType {
			case "session_create":
				rec.HasSessionCreate = true
			case "user_message":
				rec.ThreadID, _ = payload["thread_id"].(string)
				rec.Text, _ = payload["text"].(string)
				capMu.Lock()
				captured = append(captured, rec)
				capMu.Unlock()

				n := seq.Add(1)
				writeEv := func(v any) {
					_ = conn.WriteMessage(websocket.BinaryMessage, encodeMockAIDPServerEvent(v))
				}
				writeEv(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": fmt.Sprintf("m_%d", n)}})
				if n == 1 {
					writeEv(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "第一段说明。"}})
					writeEv(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": `<client_tool_call>{"name":"Read","arguments":{"path":"a.go"}}</client_tool_call>`}})
					writeEv(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "第二段补充。"}})
				} else {
					writeEv(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "续聊成功确认"}})
				}
				writeEv(map[string]any{"type": "message_complete", "payload": map[string]any{"continuation_expected": false}})
				writeEv(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
			}
		}
	}))
	defer wsSrv.Close()

	dbPath := filepath.Join(t.TempDir(), "multi_text_reuse.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	if err := st.Save(context.Background(), &store.Account{
		Email: "multitext@example.com", Provider: "social",
		WSHost: "ws://" + strings.TrimPrefix(wsSrv.URL, "http://"), WSPath: "/ws",
		AccessToken: "jwt-multi", ActorARN: "arn:aws:quick:us-east-1:multi", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("st.Save: %v", err)
	}

	cfg := config.Default()
	cfg.SessionReuse = true
	srv := NewServer(&cfg, accounts.New(st, &cfg))
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}

	tools := []map[string]any{{"name": "Read", "input_schema": map[string]any{"type": "object"}}}
	r1Body, _ := json.Marshal(map[string]any{
		"model": "claude-opus-5", "max_tokens": 512, "stream": true,
		"tools":    tools,
		"messages": []map[string]any{{"role": "user", "content": "锚点首轮消息-MULTI-1"}},
	})
	req1, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/messages", bytes.NewReader(r1Body))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Conversation-Id", "conv-multi-text-blocks")
	resp1, err := client.Do(req1)
	if err != nil {
		t.Fatalf("POST round 1: %v", err)
	}
	var outToolID string
	sc := bufio.NewScanner(resp1.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, `"type":"tool_use"`) && strings.Contains(line, `"name":"Read"`) {
			var ev map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) == nil {
				if cb, _ := ev["content_block"].(map[string]any); cb != nil {
					outToolID, _ = cb["id"].(string)
				}
			}
		}
	}
	resp1.Body.Close()
	if outToolID == "" {
		t.Fatalf("missing emitted tool_use id from round 1")
	}

	// 第 2 轮客户端以 [text, tool_use, text] 数组形式回传上一轮助手消息
	r2Body, _ := json.Marshal(map[string]any{
		"model": "claude-opus-5", "max_tokens": 512, "stream": true,
		"tools": tools,
		"messages": []map[string]any{
			{"role": "user", "content": "锚点首轮消息-MULTI-1"},
			{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "第一段说明。"},
					{"type": "tool_use", "id": outToolID, "name": "Read", "input": map[string]any{"path": "a.go"}},
					{"type": "text", "text": "第二段补充。"},
				},
			},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": outToolID, "content": "package main"},
				},
			},
		},
	})
	req2, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/messages", bytes.NewReader(r2Body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Conversation-Id", "conv-multi-text-blocks")
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("POST round 2: %v", err)
	}
	_, _ = io.ReadAll(resp2.Body)
	resp2.Body.Close()

	capMu.Lock()
	defer capMu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("expected 2 captured turns, got %d", len(captured))
	}
	if captured[1].HasSessionCreate || captured[1].ThreadID != captured[0].ThreadID {
		t.Fatalf("multi text block round 2 lost session reuse: create=%t tid2=%q vs tid1=%q",
			captured[1].HasSessionCreate, captured[1].ThreadID, captured[0].ThreadID)
	}
	if strings.Contains(captured[1].Text, "锚点首轮消息-MULTI-1") {
		t.Fatalf("expected incremental prompt on round 2, got full transcript:\n%s", captured[1].Text)
	}
}

// TestTranscriptSideRequest_IsolatedAndLowThinking 验证无工具的 <transcript> 后台摘要请求
// 被隔离为一次性 aux: 会话（DeleteSession=true、不入库、思考降至 low），而带工具的正常提问不误判。
func TestTranscriptSideRequest_IsolatedAndLowThinking(t *testing.T) {
	srv := newTestServerForReuse(t)

	// 1. 无工具 <transcript> 请求 -> aux 隔离 + low 思考 + 不持久化
	transcriptReq := &ChatRequest{
		Model:     "claude-sonnet-5",
		Reasoning: &ReasoningOptions{Effort: "max"},
		Messages: []Message{
			{Role: "system", Content: "Summarize briefly."},
			{Role: "user", Content: `<transcript> {"user":"做宣传片"} {"assistant":"好的"} </transcript>`},
		},
	}
	if !isTranscriptSideRequest(transcriptReq) || !isAuxiliarySideRequest(transcriptReq, "") {
		t.Fatalf("expected <transcript> without tools to be classified as auxiliary request")
	}
	reqHTTP := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	reqHTTP.Header.Set("X-Conversation-Id", "main-session-id-must-not-collide")
	turn, err := srv.prepareTurn(reqHTTP, transcriptReq, "anthropic", "/v1/messages", "msg_")
	if err != nil {
		t.Fatalf("prepareTurn transcript failed: %v", err)
	}
	if !turn.auxRequest || !turn.in.DeleteSession {
		turn.Close()
		t.Fatalf("expected auxRequest=true and DeleteSession=true, got aux=%t del=%t", turn.auxRequest, turn.in.DeleteSession)
	}
	if turn.in.ThinkingEffort != "low" {
		turn.Close()
		t.Fatalf("expected thinking effort downgraded to low, got %q", turn.in.ThinkingEffort)
	}
	if !strings.HasPrefix(turn.conv.key, "aux:") {
		turn.Close()
		t.Fatalf("expected conv key prefixed with aux:, got %q", turn.conv.key)
	}
	turn.usedAccount = &store.Account{ID: 1}
	turn.saveConversation(ChatResult{ThreadID: "aux-transcript-tid"}, "summary", nil)
	turn.Close()
	if n, _ := srv.store.CountConversations(context.Background()); n != 0 {
		t.Fatalf("transcript side request must not persist into conversations table, got %d", n)
	}

	// 2. 带工具的主对话正文中即使提及 <transcript> 也不误判
	normalWithTools := &ChatRequest{
		Model: "claude-opus-5",
		Tools: []Tool{{Type: "function", Function: ToolFunction{Name: "Read"}}},
		Messages: []Message{
			{Role: "user", Content: "<transcript> 请解析这段日志并修改代码"},
		},
	}
	if isTranscriptSideRequest(normalWithTools) || isAuxiliarySideRequest(normalWithTools, "") {
		t.Fatalf("normal request with tools must NOT be treated as transcript side request")
	}
}

// TestRunPython_EmptyArgumentsInToolStartWaitsForRealCode 验证上游 tool_start 携带 "{}" 空占位时，
// 携带上传附件的只读 run_python 等待后续真实参数到达而不被提前误杀，写文件恶意参数仍精准拦截并自动纠偏。
func TestRunPython_EmptyArgumentsInToolStartWaitsForRealCode(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	clientTools := []map[string]any{
		{"name": "Bash", "input_schema": map[string]any{"type": "object"}},
		{"name": "Write", "input_schema": map[string]any{"type": "object"}},
	}
	makeFakePNG := func(seed byte) []byte {
		return []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 'I', 'H', 'D', 'R', seed}
	}

	t.Run("allows_readonly_code_after_empty_tool_start", func(t *testing.T) {
		tenantSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/integration/quick-work/upload-attachment":
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"attachmentId": "att-ro", "path": "/data/s3files/prompt-ro.txt"})
			case "/integration/quick-work/attachment-dlp-status":
				_ = json.NewEncoder(w).Encode(map[string]string{"dlpJobStatus": "COMPLETED", "policyAction": "ALLOW"})
			}
		}))
		defer tenantSrv.Close()

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
				mType, _, ok := decodeClientFrame(raw)
				if !ok || mType != "user_message" {
					continue
				}
				writeEv := func(v any) {
					_ = conn.WriteMessage(websocket.BinaryMessage, encodeMockAIDPServerEvent(v))
				}
				writeEv(map[string]any{"type": "tool_start", "payload": map[string]any{"tool_id": "t_py_empty", "tool_name": "run_python", "arguments": map[string]any{}}})
				writeEv(map[string]any{"type": "tool_arguments", "payload": map[string]any{"tool_id": "t_py_empty", "arguments": map[string]any{"code": "print(open('/data/s3files/prompt-abc.txt').read()[-2000:])"}}})
				writeEv(map[string]any{"type": "tool_result", "payload": map[string]any{"tool_id": "t_py_empty"}})
				writeEv(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m_py_ok"}})
				writeEv(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "已读完附件内容"}})
				writeEv(map[string]any{"type": "message_complete", "payload": map[string]any{"continuation_expected": false}})
				writeEv(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
			}
		}))
		defer wsSrv.Close()

		dbPath := filepath.Join(t.TempDir(), "empty_start_ro.db")
		st, _ := store.Open(dbPath)
		defer st.Close()
		_ = st.Save(context.Background(), &store.Account{
			Email: "emptyro@example.com", Provider: "social", TenantURL: tenantSrv.URL,
			WSHost: "ws://" + strings.TrimPrefix(wsSrv.URL, "http://"), WSPath: "/ws",
			AccessToken: "jwt-ero", ActorARN: "arn:aws:quick:us-east-1:ero", ExpiresAt: time.Now().Add(time.Hour).Unix(),
		})
		cfg := config.Default()
		srv := NewServer(&cfg, accounts.New(st, &cfg))
		httpSrv := httptest.NewServer(srv.Handler())
		defer httpSrv.Close()

		b, _ := json.Marshal(map[string]any{
			"model": "claude-opus-5", "max_tokens": 128, "stream": false,
			"tools": clientTools,
			"messages": []map[string]any{{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": "继续分析上传附件"},
					{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgoAAAANSUhEUg=="}},
				},
			}},
		})
		// 用标准合法 PNG bytes 保证图片校验通过
		_ = makeFakePNG(1)
		var arPayload map[string]any
		_ = json.Unmarshal(b, &arPayload)
		arPayload["messages"].([]any)[0].(map[string]any)["content"].([]any)[1].(map[string]any)["source"].(map[string]any)["data"] = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
		b, _ = json.Marshal(arPayload)

		resp, err := (&http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}).Post(httpSrv.URL+"/v1/messages", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "已读完附件内容") {
			t.Fatalf("expected 200 OK waiting for real readonly code after empty tool_start, got %d: %s", resp.StatusCode, string(raw))
		}
	})

	t.Run("blocks_write_code_after_empty_tool_start", func(t *testing.T) {
		tenantSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/integration/quick-work/upload-attachment":
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"attachmentId": "att-wr", "path": "/data/s3files/img-wr.png"})
			case "/integration/quick-work/attachment-dlp-status":
				_ = json.NewEncoder(w).Encode(map[string]string{"dlpJobStatus": "COMPLETED", "policyAction": "ALLOW"})
			}
		}))
		defer tenantSrv.Close()

		var seq atomic.Int32
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
				mType, _, ok := decodeClientFrame(raw)
				if !ok || mType != "user_message" {
					continue
				}
				n := seq.Add(1)
				writeEv := func(v any) {
					_ = conn.WriteMessage(websocket.BinaryMessage, encodeMockAIDPServerEvent(v))
				}
				if n == 1 {
					writeEv(map[string]any{"type": "tool_start", "payload": map[string]any{"tool_id": "t_py_wr", "tool_name": "run_python", "arguments": map[string]any{}}})
					writeEv(map[string]any{"type": "tool_arguments", "payload": map[string]any{"tool_id": "t_py_wr", "arguments": map[string]any{"code": "open('a.txt','w').write('x')"}}})
					time.Sleep(400 * time.Millisecond)
					return
				}
				writeEv(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m_py_wr_ok"}})
				writeEv(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": `<client_tool_call>{"name":"Write","arguments":{"file_path":"a.txt","content":"x"}}</client_tool_call>`}})
				writeEv(map[string]any{"type": "message_complete", "payload": map[string]any{"continuation_expected": false}})
				writeEv(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
			}
		}))
		defer wsSrv.Close()

		dbPath := filepath.Join(t.TempDir(), "empty_start_wr.db")
		st, _ := store.Open(dbPath)
		defer st.Close()
		_ = st.Save(context.Background(), &store.Account{
			Email: "emptywr@example.com", Provider: "social", TenantURL: tenantSrv.URL,
			WSHost: "ws://" + strings.TrimPrefix(wsSrv.URL, "http://"), WSPath: "/ws",
			AccessToken: "jwt-ewr", ActorARN: "arn:aws:quick:us-east-1:ewr", ExpiresAt: time.Now().Add(time.Hour).Unix(),
		})
		cfg := config.Default()
		srv := NewServer(&cfg, accounts.New(st, &cfg))
		httpSrv := httptest.NewServer(srv.Handler())
		defer httpSrv.Close()

		b, _ := json.Marshal(map[string]any{
			"model": "claude-opus-5", "max_tokens": 128, "stream": false,
			"tools": clientTools,
			"messages": []map[string]any{{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": "写入本地文件 a.txt"},
					{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}},
				},
			}},
		})
		resp, err := (&http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}).Post(httpSrv.URL+"/v1/messages", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "Write") {
			t.Fatalf("expected auto-correction back to Write tool when write code arrived after empty tool_start, got %d: %s", resp.StatusCode, string(raw))
		}
	})
}
