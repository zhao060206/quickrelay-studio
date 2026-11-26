package proxy

import (
	"context"
	"encoding/json"
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

// TestOutpostReviewFix1_MultiSegmentContinuation 验证多段 continuation_expected=true 回答在
// OpenAI/Anthropic 的流式与非流式路径均完整保留全部段落、流结束标记只发一次，且单段普通结束与中断报错均准确无误。
func TestOutpostReviewFix1_MultiSegmentContinuation(t *testing.T) {
	var scenario atomic.Int32 // 0=两段 delta, 1=第二段仅 text_complete, 2=首段后中断, 3=普通单段

	mock := newMockOutpostServer(t)
	mock.onWSInbound = func(conn *websocket.Conn, writeFrame func(any) error, ev map[string]any, connIndex int32) {
		if ev["type"] != "user_message" {
			return
		}
		switch scenario.Load() {
		case 0:
			_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m1"}})
			_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "Segment one alpha."}})
			_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "Segment one alpha.", "continuation_expected": true}})
			time.Sleep(15 * time.Millisecond)
			_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m2"}})
			_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "Segment two beta."}})
			_ = writeFrame(map[string]any{"type": "turn_usage", "payload": map[string]any{"input_tokens": 5, "output_tokens": 9}})
			_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "Segment two beta."}})
			_ = writeFrame(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
		case 1:
			_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m1"}})
			_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "First half."}})
			_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"continuation_expected": true}})
			time.Sleep(10 * time.Millisecond)
			_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m2"}})
			_ = writeFrame(map[string]any{"type": "text_complete", "payload": map[string]any{"text": "Second half via text_complete."}})
			_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "Second half via text_complete.", "continuation_expected": false}})
		case 2:
			_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m1"}})
			_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "Incomplete prefix."}})
			_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "Incomplete prefix.", "continuation_expected": true}})
			time.Sleep(10 * time.Millisecond)
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "truncated"), time.Now().Add(time.Second))
			_ = conn.Close()
		case 3:
			_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m1"}})
			_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "Single segment ok."}})
			_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "Single segment ok."}})
		}
	}

	srv, httpSrv := setupOutpostTestServer(t, mock, config.ToolModeAuto)
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}}

	toolsOAI := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "Alpha", "description": "Alpha tool", "parameters": map[string]any{"type": "object"}}},
	}
	toolsAnt := []map[string]any{
		{"name": "Alpha", "description": "Alpha tool", "input_schema": map[string]any{"type": "object"}},
	}

	assertBothOnce := func(t *testing.T, body, first, second string) {
		t.Helper()
		if strings.Count(body, first) != 1 || strings.Count(body, second) != 1 {
			t.Fatalf("expected each segment exactly once (%q=%d, %q=%d), body:\n%s",
				first, strings.Count(body, first), second, strings.Count(body, second), body)
		}
	}

	// 1. OpenAI 非流式
	scenario.Store(0)
	code, body := doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", nil, map[string]any{
		"model":    "smart",
		"stream":   false,
		"tools":    toolsOAI,
		"messages": []map[string]any{{"role": "user", "content": "oai non-stream"}},
	})
	if code != http.StatusOK {
		t.Fatalf("OpenAI non-stream status=%d body=%s", code, body)
	}
	var oaiResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &oaiResp); err != nil {
		t.Fatalf("decode OpenAI resp: %v", err)
	}
	assertBothOnce(t, oaiResp.Choices[0].Message.Content, "Segment one alpha.", "Segment two beta.")

	// 2. OpenAI 流式（验证正文按 delta 仅发 1 次且 data: [DONE] 仅 1 次）
	resp, err := doRawJSONPost(t, client, httpSrv.URL+"/v1/chat/completions", nil, map[string]any{
		"model":    "smart",
		"stream":   true,
		"tools":    toolsOAI,
		"messages": []map[string]any{{"role": "user", "content": "oai stream"}},
	})
	if err != nil {
		t.Fatalf("OpenAI stream failed: %v", err)
	}
	sseBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	sseText := string(sseBytes)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("OpenAI stream status=%d body=%s", resp.StatusCode, sseText)
	}
	assertBothOnce(t, sseText, "Segment one alpha.", "Segment two beta.")
	if strings.Count(sseText, "data: [DONE]") != 1 {
		t.Fatalf("expected exactly one [DONE], got:\n%s", sseText)
	}

	// 3. Anthropic 非流式
	code, body = doProxyJSONCall(t, client, httpSrv.URL+"/v1/messages", nil, map[string]any{
		"model":      "claude-sonnet-4-5",
		"max_tokens": 256,
		"stream":     false,
		"tools":      toolsAnt,
		"messages":   []map[string]any{{"role": "user", "content": "ant non-stream"}},
	})
	if code != http.StatusOK {
		t.Fatalf("Anthropic non-stream status=%d body=%s", code, body)
	}
	assertBothOnce(t, string(body), "Segment one alpha.", "Segment two beta.")

	// 4. Anthropic 流式（验证仅 1 次 message_stop）
	resp, err = doRawJSONPost(t, client, httpSrv.URL+"/v1/messages", nil, map[string]any{
		"model":      "claude-sonnet-4-5",
		"max_tokens": 256,
		"stream":     true,
		"tools":      toolsAnt,
		"messages":   []map[string]any{{"role": "user", "content": "ant stream"}},
	})
	if err != nil {
		t.Fatalf("Anthropic stream failed: %v", err)
	}
	antSSEBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	antSSE := string(antSSEBytes)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Anthropic stream status=%d body=%s", resp.StatusCode, antSSE)
	}
	assertBothOnce(t, antSSE, "Segment one alpha.", "Segment two beta.")
	if strings.Count(antSSE, "event: message_stop") != 1 {
		t.Fatalf("expected exactly one message_stop, got:\n%s", antSSE)
	}

	// 5. 第二段仅通过 text_complete 交付时也不丢不重
	scenario.Store(1)
	code, body = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", nil, map[string]any{
		"model":    "smart",
		"stream":   false,
		"tools":    toolsOAI,
		"messages": []map[string]any{{"role": "user", "content": "text_complete second seg"}},
	})
	if code != http.StatusOK {
		t.Fatalf("second seg text_complete status=%d body=%s", code, body)
	}
	assertBothOnce(t, string(body), "First half.", "Second half via text_complete.")

	// 6. continuation_expected=true 后中途断流必须报错，绝不返回 200 也不写入已完成会话
	scenario.Store(2)
	code, body = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"X-Conversation-Id": "conv-truncated-cont"}, map[string]any{
		"model":    "smart",
		"stream":   false,
		"tools":    toolsOAI,
		"messages": []map[string]any{{"role": "user", "content": "truncated continuation"}},
	})
	if code == http.StatusOK {
		t.Fatalf("expected error on closed continuation stream, got 200: %s", body)
	}
	if rec, err := srv.store.GetConversation(context.Background(), "cid:conv-truncated-cont"); err == nil && rec != nil {
		t.Fatalf("incomplete continuation should not persist completed conversation, got %+v", rec)
	}

	// 7. 普通单段回答仍正常即时结束
	scenario.Store(3)
	code, body = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", nil, map[string]any{
		"model":    "smart",
		"stream":   false,
		"tools":    toolsOAI,
		"messages": []map[string]any{{"role": "user", "content": "single seg"}},
	})
	if code != http.StatusOK || !strings.Contains(string(body), "Single segment ok.") {
		t.Fatalf("single segment failed: code=%d body=%s", code, body)
	}
}

// TestOutpostReviewFix3_ToolChoicePolicyEnforcement 验证指定 tool_choice 时：
// 1) 上游调用被策略过滤的工具时，非流式与流式均绝不泄露出不允许的工具并中止挂起、反注册 Outpost；
// 2) 允许的工具仅返回一次且可正常回传 tool_result 原位续接；
// 3) 混发不允许与允许的工具时仅发合规工具、清理未对齐 WS 并在下一轮冷导入平滑续接。
func TestOutpostReviewFix3_ToolChoicePolicyEnforcement(t *testing.T) {
	var behavior atomic.Int32 // 0=只调 ToolB(违约), 1=正常调 ToolA 并续写, 2=混发 ToolB+ToolA

	mock := newMockOutpostServer(t)
	mock.onWSInbound = func(conn *websocket.Conn, writeFrame func(any) error, ev map[string]any, connIndex int32) {
		typ, _ := ev["type"].(string)
		p, _ := ev["payload"].(map[string]any)
		switch typ {
		case "user_message":
			mock.mu.Lock()
			out := mock.activeOutposts[p["outpostId"].(string)]
			mock.mu.Unlock()
			tools := out["capabilities"].([]any)[0].(map[string]any)["tools"].([]any)
			aliasA := tools[0].(map[string]any)["name"].(string)
			aliasB := tools[1].(map[string]any)["name"].(string)

			switch behavior.Load() {
			case 0:
				_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{}})
				_ = writeFrame(map[string]any{
					"type": "tool_call",
					"payload": map[string]any{
						"tool_call_id": "call_b_only",
						"tool_name":    aliasB,
						"arguments":    map[string]any{"v": "1"},
					},
				})
				_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{}})
			case 1:
				_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{}})
				_ = writeFrame(map[string]any{
					"type": "tool_call",
					"payload": map[string]any{
						"tool_call_id": "call_a_ok",
						"tool_name":    aliasA,
						"arguments":    map[string]any{"v": "ok"},
					},
				})
				_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{}})
			case 2:
				txt, _ := p["text"].(string)
				if strings.Contains(txt, "Continue responding") {
					_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "cold-resumed-after-filter"}})
					_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "cold-resumed-after-filter"}})
					return
				}
				_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{}})
				_ = writeFrame(map[string]any{
					"type": "tool_call",
					"payload": map[string]any{
						"tool_call_id": "call_b_bad",
						"tool_name":    aliasB,
						"arguments":    map[string]any{"v": "bad"},
					},
				})
				_ = writeFrame(map[string]any{
					"type": "tool_call",
					"payload": map[string]any{
						"tool_call_id": "call_a_good",
						"tool_name":    aliasA,
						"arguments":    map[string]any{"v": "good"},
					},
				})
				_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{}})
			}
		case "tool_result":
			_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "resumed-tool-A"}})
			_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "resumed-tool-A"}})
		}
	}

	srv, httpSrv := setupOutpostTestServer(t, mock, config.ToolModeAuto)
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}}

	toolsOAI := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "ToolA", "description": "Tool A", "parameters": map[string]any{"type": "object"}}},
		{"type": "function", "function": map[string]any{"name": "ToolB", "description": "Tool B", "parameters": map[string]any{"type": "object"}}},
	}
	toolsAnt := []map[string]any{
		{"name": "ToolA", "description": "Tool A", "input_schema": map[string]any{"type": "object"}},
		{"name": "ToolB", "description": "Tool B", "input_schema": map[string]any{"type": "object"}},
	}

	// 1. 指定 ToolA，上游只返回 ToolB -> OpenAI & Anthropic 流式/非流式均不可出现 ToolB 调用且不挂起
	behavior.Store(0)
	code, body := doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", nil, map[string]any{
		"model":       "smart",
		"stream":      false,
		"tools":       toolsOAI,
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "ToolA"}},
		"messages":    []map[string]any{{"role": "user", "content": "must fail non-stream oai"}},
	})
	if code == http.StatusOK || strings.Contains(string(body), `"tool_calls"`) || strings.Contains(string(body), "call_b_only") {
		t.Fatalf("OpenAI non-stream leaked blocked ToolB or returned 200: code=%d body=%s", code, body)
	}

	resp, err := doRawJSONPost(t, client, httpSrv.URL+"/v1/chat/completions", nil, map[string]any{
		"model":       "smart",
		"stream":      true,
		"tools":       toolsOAI,
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "ToolA"}},
		"messages":    []map[string]any{{"role": "user", "content": "must fail stream oai"}},
	})
	if err != nil {
		t.Fatalf("OpenAI stream req failed: %v", err)
	}
	rawOAI, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK || strings.Contains(string(rawOAI), `"tool_calls"`) || strings.Contains(string(rawOAI), "call_b_only") {
		t.Fatalf("OpenAI stream leaked blocked ToolB or returned 200: status=%d body=%s", resp.StatusCode, rawOAI)
	}

	code, body = doProxyJSONCall(t, client, httpSrv.URL+"/v1/messages", nil, map[string]any{
		"model":       "claude-sonnet-4-5",
		"max_tokens":  128,
		"stream":      false,
		"tools":       toolsAnt,
		"tool_choice": map[string]any{"type": "tool", "name": "ToolA"},
		"messages":    []map[string]any{{"role": "user", "content": "must fail non-stream ant"}},
	})
	if code == http.StatusOK || strings.Contains(string(body), `"tool_use"`) || strings.Contains(string(body), "call_b_only") {
		t.Fatalf("Anthropic non-stream leaked blocked ToolB or returned 200: code=%d body=%s", code, body)
	}

	resp, err = doRawJSONPost(t, client, httpSrv.URL+"/v1/messages", nil, map[string]any{
		"model":       "claude-sonnet-4-5",
		"max_tokens":  128,
		"stream":      true,
		"tools":       toolsAnt,
		"tool_choice": map[string]any{"type": "tool", "name": "ToolA"},
		"messages":    []map[string]any{{"role": "user", "content": "must fail stream ant"}},
	})
	if err != nil {
		t.Fatalf("Anthropic stream req failed: %v", err)
	}
	rawAnt, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK || strings.Contains(string(rawAnt), `"tool_use"`) || strings.Contains(string(rawAnt), "call_b_only") {
		t.Fatalf("Anthropic stream leaked blocked ToolB or returned 200: status=%d body=%s", resp.StatusCode, rawAnt)
	}
	// 等待异步 unregister 结束，确认无挂起池残留
	time.Sleep(40 * time.Millisecond)
	srv.outposts.mu.Lock()
	idleCount := len(srv.outposts.idle)
	srv.outposts.mu.Unlock()
	if idleCount != 0 {
		t.Fatalf("expected 0 parked outposts after rejected tool_choice attempts, got %d", idleCount)
	}

	// 2. 允许 ToolA 时，只发 1 次且可原位续接
	behavior.Store(1)
	wsBefore := mock.wsConnCount.Load()
	resp, err = doRawJSONPost(t, client, httpSrv.URL+"/v1/messages", map[string]string{"X-Conversation-Id": "conv-choice-ok"}, map[string]any{
		"model":       "claude-sonnet-4-5",
		"max_tokens":  128,
		"stream":      true,
		"tools":       toolsAnt,
		"tool_choice": map[string]any{"type": "tool", "name": "ToolA"},
		"messages":    []map[string]any{{"role": "user", "content": "call ToolA"}},
	})
	if err != nil {
		t.Fatalf("Anthropic choice OK failed: %v", err)
	}
	okSSE, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Count(string(okSSE), `"id":"call_a_ok"`) != 1 {
		t.Fatalf("expected call_a_ok once in SSE, got:\n%s", okSSE)
	}
	code, body = doProxyJSONCall(t, client, httpSrv.URL+"/v1/messages", map[string]string{"X-Conversation-Id": "conv-choice-ok"}, map[string]any{
		"model":      "claude-sonnet-4-5",
		"max_tokens": 128,
		"stream":     false,
		"tools":      toolsAnt,
		"messages": []map[string]any{
			{"role": "user", "content": "call ToolA"},
			{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "call_a_ok", "name": "ToolA", "input": map[string]any{"v": "ok"}}}},
			{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "call_a_ok", "content": "done"}}},
		},
	})
	if code != http.StatusOK || !strings.Contains(string(body), "resumed-tool-A") {
		t.Fatalf("resume after valid tool_choice failed: code=%d body=%s", code, body)
	}
	if mock.wsConnCount.Load() != wsBefore+1 {
		t.Fatalf("expected live reuse of same WS for aligned tool_choice, connDelta=%d", mock.wsConnCount.Load()-wsBefore)
	}

	// 3. 混发 ToolB+ToolA 且允许并行时，只发射 ToolA、不挂起未对齐 WS，下一轮由冷导入续接
	behavior.Store(2)
	code, body = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"X-Conversation-Id": "conv-choice-partial"}, map[string]any{
		"model":       "smart",
		"stream":      false,
		"tools":       toolsOAI,
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "ToolA"}},
		"messages":    []map[string]any{{"role": "user", "content": "mixed B and A"}},
	})
	if code != http.StatusOK || strings.Contains(string(body), "ToolB") || !strings.Contains(string(body), "call_a_good") {
		t.Fatalf("partial filter failed: code=%d body=%s", code, body)
	}
	code, body = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"X-Conversation-Id": "conv-choice-partial"}, map[string]any{
		"model":  "smart",
		"stream": false,
		"tools":  toolsOAI,
		"messages": []map[string]any{
			{"role": "user", "content": "mixed B and A"},
			{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{"id": "call_a_good", "type": "function", "function": map[string]any{"name": "ToolA", "arguments": `{"v":"good"}`}}}},
			{"role": "tool", "tool_call_id": "call_a_good", "content": "A_RES"},
		},
	})
	if code != http.StatusOK || !strings.Contains(string(body), "cold-resumed-after-filter") {
		t.Fatalf("cold follow-up after unparked partial filter failed: code=%d body=%s", code, body)
	}
}

// TestOutpostReviewFix2_UserMessageFrameBudget 验证原生路径完整 user_message 帧预算检查：
// 覆盖预算内、刚超限、中文与 JSON 转义膨胀、短提问配长工具说明进入 custom_prompt，以及 auto 无损回退 text_bridge、native_outpost 明确报 400。
func TestOutpostReviewFix2_UserMessageFrameBudget(t *testing.T) {
	runCase := func(t *testing.T, toolMode, promptText, toolDesc string, expectHTTP int, expectBodySubstr string, expectNative bool) {
		t.Helper()
		mock := newMockOutpostServer(t)
		var nativeUserMsgs, oversizedNativeFrames, totalUserMsgs atomic.Int32

		mock.onWSInbound = func(conn *websocket.Conn, writeFrame func(any) error, ev map[string]any, connIndex int32) {
			if ev["type"] != "user_message" {
				return
			}
			totalUserMsgs.Add(1)
			p, _ := ev["payload"].(map[string]any)
			oid, _ := p["outpostId"].(string)
			mock.mu.Lock()
			_, isNative := mock.activeOutposts[oid]
			mock.mu.Unlock()

			rawFrame, _ := json.Marshal(ev)
			if isNative {
				nativeUserMsgs.Add(1)
				if len(rawFrame) > maxUserMessageBytes {
					oversizedNativeFrames.Add(1)
				}
			}
			_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "budget-ok"}})
			_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "budget-ok"}})
			_ = writeFrame(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
		}

		_, httpSrv := setupOutpostTestServer(t, mock, toolMode)
		client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
		code, body := doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", nil, map[string]any{
			"model": "smart",
			"tools": []map[string]any{
				{"type": "function", "function": map[string]any{"name": "Alpha", "description": toolDesc, "parameters": map[string]any{"type": "object"}}},
			},
			"messages": []map[string]any{{"role": "user", "content": promptText}},
		})
		if code != expectHTTP {
			t.Fatalf("status=%d want %d body=%s", code, expectHTTP, body)
		}
		if expectBodySubstr != "" && !strings.Contains(string(body), expectBodySubstr) {
			t.Fatalf("body %q missing %q", string(body), expectBodySubstr)
		}
		if oversizedNativeFrames.Load() != 0 {
			t.Fatalf("sent %d oversized native user_message frames", oversizedNativeFrames.Load())
		}
		if expectNative {
			if nativeUserMsgs.Load() != 1 || totalUserMsgs.Load() != 1 {
				t.Fatalf("expected 1 native frame (0 fallback), got native=%d total=%d", nativeUserMsgs.Load(), totalUserMsgs.Load())
			}
		} else if expectHTTP == http.StatusOK {
			if nativeUserMsgs.Load() != 0 || totalUserMsgs.Load() != 1 {
				t.Fatalf("expected seamless text_bridge fallback without duplicate dispatch, got native=%d total=%d", nativeUserMsgs.Load(), totalUserMsgs.Load())
			}
		} else if totalUserMsgs.Load() != 0 {
			t.Fatalf("expected 0 upstream user_message frames on 400 error, got %d", totalUserMsgs.Load())
		}
	}

	cases := []struct {
		name       string
		toolMode   string
		prompt     string
		toolDesc   string
		wantStatus int
		wantSubstr string
		wantNative bool
	}{
		{
			name:       "within_budget_native",
			toolMode:   config.ToolModeAuto,
			prompt:     strings.Repeat("a", 2000),
			toolDesc:   "Short alpha tool",
			wantStatus: http.StatusOK,
			wantSubstr: "budget-ok",
			wantNative: true,
		},
		{
			name:       "just_over_budget_auto_fallback",
			toolMode:   config.ToolModeAuto,
			prompt:     strings.Repeat("x", 64000),
			toolDesc:   "Short alpha tool",
			wantStatus: http.StatusOK,
			wantSubstr: "budget-ok",
			wantNative: false,
		},
		{
			name:       "json_escape_and_cjk_overflow_auto_fallback",
			toolMode:   config.ToolModeAuto,
			prompt:     strings.Repeat("中文\\\"引号\n", 9000),
			toolDesc:   "Short alpha tool",
			wantStatus: http.StatusOK,
			wantSubstr: "budget-ok",
			wantNative: false,
		},
		{
			name:       "short_prompt_with_long_tool_doc_in_custom_prompt_auto_fallback",
			toolMode:   config.ToolModeAuto,
			prompt:     "hi",
			toolDesc:   strings.Repeat("D", 64000), // >8192B 工具文档整体进入 custom_prompt，使整个帧超过 63KiB
			wantStatus: http.StatusOK,
			wantSubstr: "budget-ok",
			wantNative: false,
		},
		{
			name:       "short_prompt_with_long_tool_doc_native_outpost_400",
			toolMode:   config.ToolModeNativeOutpost,
			prompt:     "hi",
			toolDesc:   strings.Repeat("D", 64000),
			wantStatus: http.StatusBadRequest,
			wantSubstr: "request_too_large",
			wantNative: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runCase(t, tc.toolMode, tc.prompt, tc.toolDesc, tc.wantStatus, tc.wantSubstr, tc.wantNative)
		})
	}
}

// TestOutpostReviewFix4_MaxThinkingPrestartHeartbeat 验证原生 paths：
// 1) 成功派发 user_message 后若长思考静默，按期发出 SSE 预热头与响应；
// 2) 握手/建连阶段阻滞超时时，不提前发送 HTTP 200 SSE 响应头，保留失败 JSON 错误返回。
func TestOutpostReviewFix4_MaxThinkingPrestartHeartbeat(t *testing.T) {
	origPrestart := maxThinkingPrestart
	origKeepalive := sseKeepaliveInterval
	origDial := wsDialHandshakeTimeout
	maxThinkingPrestart = 25 * time.Millisecond
	sseKeepaliveInterval = 25 * time.Millisecond
	t.Cleanup(func() {
		maxThinkingPrestart = origPrestart
		sseKeepaliveInterval = origKeepalive
		wsDialHandshakeTimeout = origDial
	})

	// 子场景 A：OpenAI & Anthropic 成功派发后静默期间提前送出 SSE 头与预热
	t.Run("dispatched_silent_prestarts_sse", func(t *testing.T) {
		var releaseChMu sync.Mutex
		var releaseCh chan struct{}

		mock := newMockOutpostServer(t)
		mock.onWSInbound = func(conn *websocket.Conn, writeFrame func(any) error, ev map[string]any, connIndex int32) {
			if ev["type"] != "user_message" {
				return
			}
			releaseChMu.Lock()
			ch := releaseCh
			releaseChMu.Unlock()
			go func() {
				if ch != nil {
					<-ch
				}
				_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "delayed-answer"}})
				_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "delayed-answer"}})
			}()
		}
		_, httpSrv := setupOutpostTestServer(t, mock, config.ToolModeAuto)
		client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}}

		checkEndpoint := func(url string, reqBody map[string]any) {
			t.Helper()
			ch := make(chan struct{})
			releaseChMu.Lock()
			releaseCh = ch
			releaseChMu.Unlock()

			bodyBytes, _ := json.Marshal(reqBody)
			req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(bodyBytes)))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")

			type headerRes struct {
				resp *http.Response
				err  error
			}
			hdrCh := make(chan headerRes, 1)
			go func() {
				resp, err := client.Do(req)
				hdrCh <- headerRes{resp: resp, err: err}
			}()

			// 上游尚未放开，必须在短窗口内收到预热的 200 SSE 响应头
			var got *http.Response
			select {
			case hr := <-hdrCh:
				if hr.err != nil {
					close(ch)
					t.Fatalf("client.Do failed: %v", hr.err)
				}
				got = hr.resp
			case <-time.After(400 * time.Millisecond):
				close(ch)
				t.Fatalf("SSE headers not prestarted while upstream was silent after dispatch")
			}

			close(ch)
			raw, _ := io.ReadAll(got.Body)
			got.Body.Close()
			if got.StatusCode != http.StatusOK || !strings.Contains(string(raw), "delayed-answer") {
				t.Fatalf("unexpected prestarted response: status=%d body=%s", got.StatusCode, raw)
			}
		}

		checkEndpoint(httpSrv.URL+"/v1/chat/completions", map[string]any{
			"model":            "smart",
			"stream":           true,
			"reasoning_effort": "max",
			"tools":            []map[string]any{{"type": "function", "function": map[string]any{"name": "Alpha", "parameters": map[string]any{"type": "object"}}}},
			"messages":         []map[string]any{{"role": "user", "content": "think long"}},
		})
		checkEndpoint(httpSrv.URL+"/v1/messages", map[string]any{
			"model":         "claude-opus-5",
			"max_tokens":    128,
			"stream":        true,
			"output_config": map[string]any{"effort": "max"},
			"tools":         []map[string]any{{"name": "Alpha", "input_schema": map[string]any{"type": "object"}}},
			"messages":      []map[string]any{{"role": "user", "content": "think long ant"}},
		})
	})

	// 子场景 B：握手阻塞导致超时的失败场景，绝不提前发 200 SSE 响应头
	t.Run("handshake_failure_does_not_prestart_sse", func(t *testing.T) {
		wsDialHandshakeTimeout = 70 * time.Millisecond
		blockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/integration/quick-work/qbs-event" {
				http.Error(w, `{"message":"unexpected"}`, http.StatusInternalServerError)
				return
			}
			// 刻意等待至超时，不返回 connected 帧也不完成 WS 升级
			time.Sleep(150 * time.Millisecond)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		}))
		t.Cleanup(blockSrv.Close)

		dbPath := filepath.Join(t.TempDir(), "prestart_fail.db")
		st, err := store.Open(dbPath)
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		t.Cleanup(func() { _ = st.Close() })
		_ = st.Save(context.Background(), &store.Account{
			Email: "hb@example.com", Provider: "social",
			TenantURL: blockSrv.URL, WSHost: "ws://" + strings.TrimPrefix(blockSrv.URL, "http://"), WSPath: "/ws",
			AccessToken: "jwt-hb", ActorARN: "arn:aws:quick:us-east-1:hb", ExpiresAt: time.Now().Add(time.Hour).Unix(),
		})
		cfg := config.Default()
		cfg.MaxAttempts = 1
		cfg.ToolMode = config.ToolModeNativeOutpost
		srv := NewServer(&cfg, accounts.New(st, &cfg))
		httpSrv := httptest.NewServer(srv.Handler())
		t.Cleanup(httpSrv.Close)

		client := &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{Proxy: nil}}
		resp, err := doRawJSONPost(t, client, httpSrv.URL+"/v1/messages", nil, map[string]any{
			"model":         "claude-opus-5",
			"max_tokens":    128,
			"stream":        true,
			"output_config": map[string]any{"effort": "max"},
			"tools":         []map[string]any{{"name": "Alpha", "input_schema": map[string]any{"type": "object"}}},
			"messages":      []map[string]any{{"role": "user", "content": "fail before dispatch"}},
		})
		if err != nil {
			t.Fatalf("request error: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("expected non-200 error when dial fails before dispatch, got 200 SSE")
		}
	})
}

// TestOutpostReviewFix5_ExplicitThreadIDContract 验证原生 Outpost 下显式 X-Thread-Id 语义：
// 1) 同 Agent 已知归属冷续接必须沿用约定 thread_id（不覆盖生成新 ID、不重导 store/sessions）；
// 2) 跨 Agent、未知归属（包括老库未绑定）、挂起丢失遇未闭合 tool_result 均返回 HTTP 409 且 0 帧发往上游；
// 3) 不带 X-Thread-Id 的普通冷启动仍正常分配新线程返回 200。
func TestOutpostReviewFix5_ExplicitThreadIDContract(t *testing.T) {
	var seenThreadIDs []string
	var seenMu sync.Mutex

	mock := newMockOutpostServer(t)
	mock.onWSInbound = func(conn *websocket.Conn, writeFrame func(any) error, ev map[string]any, connIndex int32) {
		if ev["type"] == "user_message" {
			p, _ := ev["payload"].(map[string]any)
			tid, _ := p["thread_id"].(string)
			seenMu.Lock()
			seenThreadIDs = append(seenThreadIDs, tid)
			seenMu.Unlock()
			_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "ok:" + tid}})
			_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "ok:" + tid}})
		}
	}

	srv, httpSrv := setupOutpostTestServer(t, mock, config.ToolModeAuto)
	srv.cfg.ChatAgentID = "agent-native-A"
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}}

	// 1. 先以普通请求（不带 X-Thread-Id）建立一段属 agent-native-A 的历史会话，并记下其真实 thread_id
	code, _ := doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"X-Conversation-Id": "conv-exp-seed"}, map[string]any{
		"model":    "smart",
		"tools":    testDummyTools(),
		"messages": []map[string]any{{"role": "user", "content": "seed turn"}},
	})
	if code != http.StatusOK {
		t.Fatalf("seed turn failed: %d", code)
	}
	seenMu.Lock()
	knownTID := seenThreadIDs[0]
	seenMu.Unlock()

	// 清空内存会话池（模拟冷启动），但保留 SQLite 中的线程归属记录
	srv.outposts.InvalidateAll()
	srv.sessions.mu.Lock()
	clear(srv.sessions.entries)
	srv.sessions.mu.Unlock()
	mock.mu.Lock()
	createdBefore := len(mock.createdSessions)
	mock.mu.Unlock()

	// 2. 同 Agent 带显式已知 X-Thread-Id 冷续接 -> 必须原样使用 knownTID 且不调用 store/sessions 新建覆盖
	code, body := doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{
		"X-Thread-Id": knownTID,
	}, map[string]any{
		"model": "smart",
		"tools": testDummyTools(),
		"messages": []map[string]any{
			{"role": "user", "content": "seed turn"},
			{"role": "assistant", "content": "ok:" + knownTID},
			{"role": "user", "content": "resume explicit tid"},
		},
	})
	if code != http.StatusOK || !strings.Contains(string(body), "ok:"+knownTID) {
		t.Fatalf("explicit same-agent resume failed: code=%d body=%s", code, body)
	}
	mock.mu.Lock()
	createdAfter := len(mock.createdSessions)
	mock.mu.Unlock()
	if createdAfter != createdBefore {
		t.Fatalf("explicit same-agent resume should reuse existing thread without store/sessions overwrite, created=%d", createdAfter-createdBefore)
	}

	// 验证以下所有冲突/未知归属情形均严格 0 帧发往上游
	srv.outposts.InvalidateAll()
	framesBeforeReject := func() int {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return len(mock.wsFrames)
	}()

	// 3. 跨 Agent 续接该已知线程 -> 409
	srv.cfg.ChatAgentID = "agent-native-B"
	code, _ = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"X-Thread-Id": knownTID}, map[string]any{
		"model":    "smart",
		"tools":    testDummyTools(),
		"messages": []map[string]any{{"role": "user", "content": "cross agent"}},
	})
	if code != http.StatusConflict {
		t.Fatalf("expected 409 on cross-agent explicit X-Thread-Id, got %d", code)
	}

	// 4. 归属未知外部 X-Thread-Id -> 409
	srv.cfg.ChatAgentID = "agent-native-A"
	code, _ = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"X-Thread-Id": "unknown-external-thread-xyz"}, map[string]any{
		"model":    "smart",
		"tools":    testDummyTools(),
		"messages": []map[string]any{{"role": "user", "content": "unknown owner"}},
	})
	if code != http.StatusConflict {
		t.Fatalf("expected 409 on unknown explicit X-Thread-Id, got %d", code)
	}

	// 5. 已知同 Agent 线程但在挂起连接丢失后携带未完成 tool_result -> 409（拒绝静默替换新 thread）
	code, _ = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"X-Thread-Id": knownTID}, map[string]any{
		"model": "smart",
		"tools": testDummyTools(),
		"messages": []map[string]any{
			{"role": "user", "content": "seed turn"},
			{"role": "assistant", "content": "", "tool_calls": []map[string]any{{"id": "c1", "type": "function", "function": map[string]any{"name": "local_read_file", "arguments": `{}`}}}},
			{"role": "tool", "tool_call_id": "c1", "content": "res"},
		},
	})
	if code != http.StatusConflict {
		t.Fatalf("expected 409 when unparked tool_result resumes explicit X-Thread-Id, got %d", code)
	}

	// 6. 老库未绑定记录（ChatAgentBound=false）-> 409
	_ = srv.store.UpsertConversation(context.Background(), &store.ConversationRecord{
		Key: "h:legacy-unbound-outpost", ThreadID: "tid-legacy-outpost", AccountID: 1,
		BaseHash: "v2::", ChatAgentID: "", ChatAgentBound: false, LastUsed: time.Now().Unix(),
	})
	code, _ = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"X-Thread-Id": "tid-legacy-outpost"}, map[string]any{
		"model":    "smart",
		"tools":    testDummyTools(),
		"messages": []map[string]any{{"role": "user", "content": "legacy unbound"}},
	})
	if code != http.StatusConflict {
		t.Fatalf("expected 409 on legacy unbound explicit X-Thread-Id, got %d", code)
	}

	mock.mu.Lock()
	framesAfterReject := len(mock.wsFrames)
	mock.mu.Unlock()
	if framesAfterReject != framesBeforeReject {
		t.Fatalf("rejected explicit X-Thread-Id leaked %d WS frames", framesAfterReject-framesBeforeReject)
	}

	// 7. 无 X-Thread-Id 的普通冷启动依然正常生成新线程并返回 200
	code, body = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", nil, map[string]any{
		"model":    "smart",
		"tools":    testDummyTools(),
		"messages": []map[string]any{{"role": "user", "content": "normal fresh start"}},
	})
	if code != http.StatusOK || !strings.Contains(string(body), "ok:") {
		t.Fatalf("normal cold start failed: code=%d body=%s", code, body)
	}
}

// TestOutpostReviewFix6_WrongDevicePrefixBlocked 验证端到端收到错误设备前缀的
// permission_request 或 tool_call 时：前者发回 permission_denied，后者均不触发任何客户端工具事件且阻断请求。
func TestOutpostReviewFix6_WrongDevicePrefixBlocked(t *testing.T) {
	var mode atomic.Int32 // 0=错误设备前缀 permission_request, 1=错误设备前缀 tool_call

	mock := newMockOutpostServer(t)
	mock.onWSInbound = func(conn *websocket.Conn, writeFrame func(any) error, ev map[string]any, connIndex int32) {
		if ev["type"] != "user_message" {
			return
		}
		p, _ := ev["payload"].(map[string]any)
		mock.mu.Lock()
		out := mock.activeOutposts[p["outpostId"].(string)]
		mock.mu.Unlock()
		alias := out["capabilities"].([]any)[0].(map[string]any)["tools"].([]any)[0].(map[string]any)["name"].(string)
		wrongPrefixName := "QuickRelay_ffffffffffffffff_" + alias

		if mode.Load() == 0 {
			_ = writeFrame(map[string]any{
				"type": "permission_request",
				"payload": map[string]any{
					"connectorId": outpostConnectorName,
					"actionId":    wrongPrefixName,
				},
			})
			return
		}
		_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{}})
		_ = writeFrame(map[string]any{
			"type": "tool_call",
			"payload": map[string]any{
				"tool_call_id": "call_wrong_dev",
				"tool_name":    wrongPrefixName,
				"arguments":    map[string]any{"path": "a.txt"},
			},
		})
	}

	_, httpSrv := setupOutpostTestServer(t, mock, config.ToolModeAuto)
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}}

	// 1. permission_request 带错误设备前缀 -> 必须回传 permission_denied 并拦截
	mode.Store(0)
	code, body := doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", nil, map[string]any{
		"model":    "smart",
		"tools":    testDummyTools(),
		"messages": []map[string]any{{"role": "user", "content": "wrong dev perm"}},
	})
	if code == http.StatusOK {
		t.Fatalf("expected error for wrong-device permission_request, got 200: %s", body)
	}
	mock.mu.Lock()
	sawDenied := false
	for _, f := range mock.wsFrames {
		if f["type"] == "permission_denied" {
			sawDenied = true
			break
		}
	}
	mock.mu.Unlock()
	if !sawDenied {
		t.Fatalf("expected permission_denied for wrong-device prefix")
	}

	// 2. tool_call 带错误设备前缀（流式）-> 绝不输出工具事件且报错结束
	mode.Store(1)
	resp, err := doRawJSONPost(t, client, httpSrv.URL+"/v1/messages", nil, map[string]any{
		"model":      "claude-sonnet-4-5",
		"max_tokens": 128,
		"stream":     true,
		"tools":      []map[string]any{{"name": "Alpha", "input_schema": map[string]any{"type": "object"}}},
		"messages":   []map[string]any{{"role": "user", "content": "wrong dev tool_call"}},
	})
	if err != nil {
		t.Fatalf("stream request failed: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK || strings.Contains(string(raw), `"type":"tool_use"`) || strings.Contains(string(raw), "call_wrong_dev") {
		t.Fatalf("wrong-device tool_call produced client tool event or 200: status=%d body=%s", resp.StatusCode, raw)
	}
}

// TestOutpostReReview1_ExplicitThreadConflictAcrossTransports 验证：
// 同一账号下预先存在线程 A 与线程 B 时，若携带 A 的 X-Conversation-Id 与历史但显式指定 X-Thread-Id: B：
// 1) 在原生成功、原生不支持回退文本桥、直接文本桥三种模式下均返回 HTTP 409，绝不向 A 派发任何 user_message 帧；
// 2) 当请求历史与 B 一致时，三种传输模式下均正常发往线程 B。
func TestOutpostReReview1_ExplicitThreadConflictAcrossTransports(t *testing.T) {
	runMode := func(t *testing.T, toolMode string, putFailStatus int) {
		t.Helper()
		var dispatchedTIDs []string
		var dispMu sync.Mutex

		mock := newMockOutpostServer(t)
		mock.onWSInbound = func(conn *websocket.Conn, writeFrame func(any) error, ev map[string]any, connIndex int32) {
			if ev["type"] == "user_message" {
				p, _ := ev["payload"].(map[string]any)
				tid, _ := p["thread_id"].(string)
				dispMu.Lock()
				dispatchedTIDs = append(dispatchedTIDs, tid)
				dispMu.Unlock()
				_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "reply:" + tid}})
				_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "reply:" + tid}})
				_ = writeFrame(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
			}
		}

		srv, httpSrv := setupOutpostTestServer(t, mock, toolMode)
		srv.cfg.ChatAgentID = "agent-re1"
		client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}}

		// 先用默认可成功方式在同账号下建立线程 A 与线程 B
		code, _ := doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"X-Conversation-Id": "conv-A"}, map[string]any{
			"model":    "smart",
			"tools":    testDummyTools(),
			"messages": []map[string]any{{"role": "user", "content": "init A"}},
		})
		if code != http.StatusOK {
			t.Fatalf("seed A failed: %d", code)
		}
		code, _ = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"X-Conversation-Id": "conv-B"}, map[string]any{
			"model":    "smart",
			"tools":    testDummyTools(),
			"messages": []map[string]any{{"role": "user", "content": "init B"}},
		})
		if code != http.StatusOK {
			t.Fatalf("seed B failed: %d", code)
		}

		dispMu.Lock()
		threadA := dispatchedTIDs[0]
		threadB := dispatchedTIDs[1]
		dispatchedTIDs = nil
		dispMu.Unlock()
		if threadA == "" || threadB == "" || threadA == threadB {
			t.Fatalf("expected distinct threadA/threadB, got %q / %q", threadA, threadB)
		}

		// 打开目标传输故障注入（若验证回退路径）
		mock.mu.Lock()
		mock.putFailStatus = putFailStatus
		mock.mu.Unlock()

		// 1. 携带 A 的 X-Conversation-Id 和可命中 A 的历史，但显式指定 X-Thread-Id: B -> 必须 409，0 次发给 A 或 B
		code, _ = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{
			"X-Conversation-Id": "conv-A",
			"X-Thread-Id":       threadB,
		}, map[string]any{
			"model": "smart",
			"tools": testDummyTools(),
			"messages": []map[string]any{
				{"role": "user", "content": "init A"},
				{"role": "assistant", "content": "reply:" + threadA},
				{"role": "user", "content": "conflict turn"},
			},
		})
		if code != http.StatusConflict {
			t.Fatalf("expected 409 when X-Conversation-Id(A) conflicts with X-Thread-Id(B), got %d", code)
		}

		// 2. 不带 X-Conversation-Id 但消息历史命中 A（前缀是 A 而非 B）且显式指定 X-Thread-Id: B -> 同样 409，0 次派发
		code, _ = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{
			"X-Thread-Id": threadB,
		}, map[string]any{
			"model": "smart",
			"tools": testDummyTools(),
			"messages": []map[string]any{
				{"role": "user", "content": "init A"},
				{"role": "assistant", "content": "reply:" + threadA},
				{"role": "user", "content": "mismatched history for B"},
			},
		})
		if code != http.StatusConflict {
			t.Fatalf("expected 409 when request history belongs to A but X-Thread-Id is B, got %d", code)
		}

		dispMu.Lock()
		if len(dispatchedTIDs) != 0 {
			dispMu.Unlock()
			t.Fatalf("rejected conflict dispatched unexpected user_message threads: %v", dispatchedTIDs)
		}
		dispMu.Unlock()

		// 3. 携带与 B 一致的历史且显式指定 X-Thread-Id: B -> 200 且 thread_id 严格等于 B
		code, body := doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{
			"X-Thread-Id": threadB,
		}, map[string]any{
			"model": "smart",
			"tools": testDummyTools(),
			"messages": []map[string]any{
				{"role": "user", "content": "init B"},
				{"role": "assistant", "content": "reply:" + threadB},
				{"role": "user", "content": "valid follow-up for B"},
			},
		})
		if code != http.StatusOK || !strings.Contains(string(body), "reply:"+threadB) {
			t.Fatalf("expected matching history + X-Thread-Id(B) to reach B, code=%d body=%s", code, body)
		}
		dispMu.Lock()
		gotDispatched := append([]string(nil), dispatchedTIDs...)
		dispMu.Unlock()
		if len(gotDispatched) != 1 || gotDispatched[0] != threadB {
			t.Fatalf("expected single dispatch to threadB=%q, got %v", threadB, gotDispatched)
		}
	}

	t.Run("native_outpost_ok", func(t *testing.T) {
		runMode(t, config.ToolModeAuto, 0)
	})
	t.Run("native_unsupported_fallback_text_bridge", func(t *testing.T) {
		runMode(t, config.ToolModeAuto, http.StatusNotFound)
	})
	t.Run("direct_text_bridge", func(t *testing.T) {
		runMode(t, config.ToolModeTextBridge, 0)
	})
}

// TestOutpostReReview2_ContinuationDisconnectAfterSecondSegmentStart 验证：
// 当第一段以 continuation_expected=true 结束且第二段已发出 message_start（无正文或已有部分正文）后
// 上游以正常关闭码（1000）断开连接且未发完结事件时，OpenAI/Anthropic 的非流式与流式均不可当作成功返回且不得持久化完成会话。
func TestOutpostReReview2_ContinuationDisconnectAfterSecondSegmentStart(t *testing.T) {
	var seg2Partial atomic.Bool

	mock := newMockOutpostServer(t)
	mock.onWSInbound = func(conn *websocket.Conn, writeFrame func(any) error, ev map[string]any, connIndex int32) {
		if ev["type"] != "user_message" {
			return
		}
		_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m1"}})
		_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "Part1 complete."}})
		_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "Part1 complete.", "continuation_expected": true}})
		time.Sleep(10 * time.Millisecond)
		_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m2"}})
		if seg2Partial.Load() {
			_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "Part2 half-"}})
		}
		time.Sleep(10 * time.Millisecond)
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "halfway close"), time.Now().Add(time.Second))
		_ = conn.Close()
	}

	srv, httpSrv := setupOutpostTestServer(t, mock, config.ToolModeAuto)
	srv.cfg.MaxAttempts = 1
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}}

	toolsOAI := []map[string]any{{"type": "function", "function": map[string]any{"name": "Alpha", "parameters": map[string]any{"type": "object"}}}}
	toolsAnt := []map[string]any{{"name": "Alpha", "input_schema": map[string]any{"type": "object"}}}

	for _, partial := range []bool{false, true} {
		seg2Partial.Store(partial)
		label := "seg2_empty_then_close"
		if partial {
			label = "seg2_partial_delta_then_close"
		}
		t.Run(label, func(t *testing.T) {
			// OpenAI 非流式
			cidOAI := "cid-rerev2-oai-" + label
			code, body := doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"X-Conversation-Id": cidOAI}, map[string]any{
				"model":    "smart",
				"stream":   false,
				"tools":    toolsOAI,
				"messages": []map[string]any{{"role": "user", "content": label + " oai nonstream"}},
			})
			if code == http.StatusOK || strings.Contains(string(body), `"finish_reason":"stop"`) {
				t.Fatalf("OpenAI non-stream must not succeed on truncated second segment: code=%d body=%s", code, body)
			}
			if rec, _ := srv.store.GetConversation(context.Background(), "cid:"+cidOAI); rec != nil {
				t.Fatalf("OpenAI incomplete continuation persisted conversation: %+v", rec)
			}

			// OpenAI 流式（已发首段正文，结尾必须报错而非 finish_reason: stop）
			cidOAIStr := "cid-rerev2-oaistr-" + label
			resp, err := doRawJSONPost(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"X-Conversation-Id": cidOAIStr}, map[string]any{
				"model":    "smart",
				"stream":   true,
				"tools":    toolsOAI,
				"messages": []map[string]any{{"role": "user", "content": label + " oai stream"}},
			})
			if err != nil {
				t.Fatalf("OpenAI stream post err: %v", err)
			}
			sseRaw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			sseStr := string(sseRaw)
			if strings.Contains(sseStr, `"finish_reason":"stop"`) || !strings.Contains(sseStr, `"error"`) {
				t.Fatalf("OpenAI stream must terminate with error (no finish_reason stop), got:\n%s", sseStr)
			}
			if rec, _ := srv.store.GetConversation(context.Background(), "cid:"+cidOAIStr); rec != nil {
				t.Fatalf("OpenAI stream incomplete continuation persisted conversation: %+v", rec)
			}

			// Anthropic 非流式
			cidAnt := "cid-rerev2-ant-" + label
			code, body = doProxyJSONCall(t, client, httpSrv.URL+"/v1/messages", map[string]string{"X-Conversation-Id": cidAnt}, map[string]any{
				"model":      "claude-sonnet-4-5",
				"max_tokens": 128,
				"stream":     false,
				"tools":      toolsAnt,
				"messages":   []map[string]any{{"role": "user", "content": label + " ant nonstream"}},
			})
			if code == http.StatusOK || strings.Contains(string(body), `"stop_reason":"end_turn"`) {
				t.Fatalf("Anthropic non-stream must not succeed on truncated second segment: code=%d body=%s", code, body)
			}

			// Anthropic 流式（已发首段正文，结尾必须为 event: error 且不能有 message_stop / end_turn）
			cidAntStr := "cid-rerev2-antstr-" + label
			resp, err = doRawJSONPost(t, client, httpSrv.URL+"/v1/messages", map[string]string{"X-Conversation-Id": cidAntStr}, map[string]any{
				"model":      "claude-sonnet-4-5",
				"max_tokens": 128,
				"stream":     true,
				"tools":      toolsAnt,
				"messages":   []map[string]any{{"role": "user", "content": label + " ant stream"}},
			})
			if err != nil {
				t.Fatalf("Anthropic stream post err: %v", err)
			}
			antRaw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			antStr := string(antRaw)
			if strings.Contains(antStr, "event: message_stop") || strings.Contains(antStr, `"stop_reason":"end_turn"`) || !strings.Contains(antStr, "event: error") {
				t.Fatalf("Anthropic stream must terminate with event: error (no message_stop), got:\n%s", antStr)
			}
			if rec, _ := srv.store.GetConversation(context.Background(), "cid:"+cidAntStr); rec != nil {
				t.Fatalf("Anthropic stream incomplete continuation persisted conversation: %+v", rec)
			}
		})
	}
}

// TestOutpostReReview3_ForcedToolChoiceRejectsTextOnly 验证：
// 当上游仅返回纯文字并正常结束时，tool_choice 为指定工具或 required/any 必须报工具策略不匹配，而 auto 仍允许纯文字回答。
func TestOutpostReReview3_ForcedToolChoiceRejectsTextOnly(t *testing.T) {
	mock := newMockOutpostServer(t)
	mock.onWSInbound = func(conn *websocket.Conn, writeFrame func(any) error, ev map[string]any, connIndex int32) {
		if ev["type"] != "user_message" {
			return
		}
		_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{}})
		_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "Pure text reply without tool calls."}})
		_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "Pure text reply without tool calls."}})
	}

	srv, httpSrv := setupOutpostTestServer(t, mock, config.ToolModeAuto)
	srv.cfg.MaxAttempts = 1
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}}

	toolsOAI := []map[string]any{{"type": "function", "function": map[string]any{"name": "Lookup", "description": "Lookup info", "parameters": map[string]any{"type": "object"}}}}
	toolsAnt := []map[string]any{{"name": "Lookup", "description": "Lookup info", "input_schema": map[string]any{"type": "object"}}}

	assertOAI := func(t *testing.T, choice any, expectOK bool) {
		t.Helper()
		code, body := doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", nil, map[string]any{
			"model":       "smart",
			"stream":      false,
			"tools":       toolsOAI,
			"tool_choice": choice,
			"messages":    []map[string]any{{"role": "user", "content": "forced check oai nonstream"}},
		})
		if expectOK {
			if code != http.StatusOK || !strings.Contains(string(body), `"finish_reason":"stop"`) {
				t.Fatalf("expected OpenAI auto text-only 200 stop, got code=%d body=%s", code, body)
			}
		} else {
			if code != http.StatusBadGateway || !strings.Contains(string(body), "upstream_tool_mismatch") {
				t.Fatalf("expected OpenAI non-stream 502 upstream_tool_mismatch, got code=%d body=%s", code, body)
			}
		}

		resp, err := doRawJSONPost(t, client, httpSrv.URL+"/v1/chat/completions", nil, map[string]any{
			"model":       "smart",
			"stream":      true,
			"tools":       toolsOAI,
			"tool_choice": choice,
			"messages":    []map[string]any{{"role": "user", "content": "forced check oai stream"}},
		})
		if err != nil {
			t.Fatalf("OpenAI stream post err: %v", err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		s := string(raw)
		if expectOK {
			if resp.StatusCode != http.StatusOK || !strings.Contains(s, `"finish_reason":"stop"`) || strings.Contains(s, `"error"`) {
				t.Fatalf("expected OpenAI stream auto text-only success, got status=%d body=%s", resp.StatusCode, s)
			}
		} else {
			if strings.Contains(s, `"finish_reason":"stop"`) || !strings.Contains(s, `"error"`) {
				t.Fatalf("expected OpenAI stream forced choice rejection without stop finish, got status=%d body=%s", resp.StatusCode, s)
			}
		}
	}

	assertAnt := func(t *testing.T, choice any, expectOK bool) {
		t.Helper()
		code, body := doProxyJSONCall(t, client, httpSrv.URL+"/v1/messages", nil, map[string]any{
			"model":       "claude-sonnet-4-5",
			"max_tokens":  128,
			"stream":      false,
			"tools":       toolsAnt,
			"tool_choice": choice,
			"messages":    []map[string]any{{"role": "user", "content": "forced check ant nonstream"}},
		})
		if expectOK {
			if code != http.StatusOK || !strings.Contains(string(body), `"stop_reason":"end_turn"`) {
				t.Fatalf("expected Anthropic auto text-only 200 end_turn, got code=%d body=%s", code, body)
			}
		} else {
			if code != http.StatusBadGateway || !strings.Contains(string(body), "required tool") && !strings.Contains(string(body), "expected at least one tool") {
				t.Fatalf("expected Anthropic non-stream 502 tool mismatch, got code=%d body=%s", code, body)
			}
		}

		resp, err := doRawJSONPost(t, client, httpSrv.URL+"/v1/messages", nil, map[string]any{
			"model":       "claude-sonnet-4-5",
			"max_tokens":  128,
			"stream":      true,
			"tools":       toolsAnt,
			"tool_choice": choice,
			"messages":    []map[string]any{{"role": "user", "content": "forced check ant stream"}},
		})
		if err != nil {
			t.Fatalf("Anthropic stream post err: %v", err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		s := string(raw)
		if expectOK {
			if resp.StatusCode != http.StatusOK || !strings.Contains(s, "event: message_stop") || strings.Contains(s, "event: error") {
				t.Fatalf("expected Anthropic stream auto text-only success, got status=%d body=%s", resp.StatusCode, s)
			}
		} else {
			if strings.Contains(s, "event: message_stop") || strings.Contains(s, `"stop_reason":"end_turn"`) || !strings.Contains(s, "event: error") {
				t.Fatalf("expected Anthropic stream forced choice rejection without message_stop, got status=%d body=%s", resp.StatusCode, s)
			}
		}
	}

	assertOAI(t, map[string]any{"type": "function", "function": map[string]any{"name": "Lookup"}}, false)
	assertOAI(t, "required", false)
	assertOAI(t, "auto", true)

	assertAnt(t, map[string]any{"type": "tool", "name": "Lookup"}, false)
	assertAnt(t, map[string]any{"type": "any"}, false)
	assertAnt(t, map[string]any{"type": "auto"}, true)
}

// TestOutpostReReview4_ExplicitThreadPartialFilterConsistency 验证：
// 在已知显式 X-Thread-Id 与指定 tool_choice=Lookup 下：
// 1) 上游混发 Other 与 Lookup 导致无法安全挂起对齐结果时，代理在向客户端发射任何工具调用前直接报错，0 次漏出任何 tool_call；
// 2) 上游只发合规 Lookup 时正常返回调用 ID，客户端随后持同一 X-Thread-Id（同/跨 convKey）回传该 ID 结果可 200 原位续接。
func TestOutpostReReview4_ExplicitThreadPartialFilterConsistency(t *testing.T) {
	var behavior atomic.Int32 // 0=seed, 1=mixed Other+Lookup, 2=only Lookup + tool_result resume
	var firstTID string
	var firstMu sync.Mutex

	mock := newMockOutpostServer(t)
	mock.onWSInbound = func(conn *websocket.Conn, writeFrame func(any) error, ev map[string]any, connIndex int32) {
		typ, _ := ev["type"].(string)
		p, _ := ev["payload"].(map[string]any)
		switch typ {
		case "user_message":
			tid, _ := p["thread_id"].(string)
			firstMu.Lock()
			if firstTID == "" {
				firstTID = tid
			}
			firstMu.Unlock()

			mock.mu.Lock()
			out := mock.activeOutposts[p["outpostId"].(string)]
			mock.mu.Unlock()
			tools := out["capabilities"].([]any)[0].(map[string]any)["tools"].([]any)
			lookupAlias := tools[0].(map[string]any)["name"].(string)
			var otherAlias string
			if len(tools) >= 2 {
				otherAlias = tools[1].(map[string]any)["name"].(string)
			}

			switch behavior.Load() {
			case 0:
				_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "seed-done"}})
				_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "seed-done"}})
			case 1:
				_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{}})
				_ = writeFrame(map[string]any{
					"type": "tool_call",
					"payload": map[string]any{
						"tool_call_id": "call_other_mixed",
						"tool_name":    otherAlias,
						"arguments":    map[string]any{"q": "other"},
					},
				})
				_ = writeFrame(map[string]any{
					"type": "tool_call",
					"payload": map[string]any{
						"tool_call_id": "call_lookup_mixed",
						"tool_name":    lookupAlias,
						"arguments":    map[string]any{"q": "lookup"},
					},
				})
				_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{}})
			case 2:
				_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{}})
				_ = writeFrame(map[string]any{
					"type": "tool_call",
					"payload": map[string]any{
						"tool_call_id": "call_lookup_clean",
						"tool_name":    lookupAlias,
						"arguments":    map[string]any{"q": "lookup"},
					},
				})
				_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{}})
			}
		case "tool_result":
			_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "explicit-tool-result-ok"}})
			_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "explicit-tool-result-ok"}})
		}
	}

	srv, httpSrv := setupOutpostTestServer(t, mock, config.ToolModeAuto)
	srv.cfg.ChatAgentID = "agent-re4"
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}}

	toolsOAI := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "Lookup", "description": "Lookup", "parameters": map[string]any{"type": "object"}}},
		{"type": "function", "function": map[string]any{"name": "Other", "description": "Other", "parameters": map[string]any{"type": "object"}}},
	}
	toolsAnt := []map[string]any{
		{"name": "Lookup", "description": "Lookup", "input_schema": map[string]any{"type": "object"}},
		{"name": "Other", "description": "Other", "input_schema": map[string]any{"type": "object"}},
	}

	// 1. 预建 knownTID 并清空空闲池
	behavior.Store(0)
	code, _ := doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"X-Conversation-Id": "conv-re4-seed"}, map[string]any{
		"model":    "smart",
		"tools":    toolsOAI,
		"messages": []map[string]any{{"role": "user", "content": "seed"}},
	})
	if code != http.StatusOK {
		t.Fatalf("seed failed: %d", code)
	}
	firstMu.Lock()
	knownTID := firstTID
	firstMu.Unlock()
	srv.outposts.InvalidateAll()

	// 2. 混发 Other+Lookup 且显式指定 X-Thread-Id: knownTID -> OpenAI/Anthropic 流式与非流式均必须在发出调用前报错
	behavior.Store(1)
	code, body := doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"X-Thread-Id": knownTID}, map[string]any{
		"model":       "smart",
		"stream":      false,
		"tools":       toolsOAI,
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "Lookup"}},
		"messages": []map[string]any{
			{"role": "user", "content": "seed"},
			{"role": "assistant", "content": "seed-done"},
			{"role": "user", "content": "mixed reject nonstream"},
		},
	})
	if code != http.StatusBadGateway || strings.Contains(string(body), "call_lookup_mixed") || strings.Contains(string(body), "call_other_mixed") {
		t.Fatalf("OpenAI non-stream leaked unresumable tool call or didn't fail 502: code=%d body=%s", code, body)
	}

	resp, err := doRawJSONPost(t, client, httpSrv.URL+"/v1/messages", map[string]string{"X-Thread-Id": knownTID}, map[string]any{
		"model":       "claude-sonnet-4-5",
		"max_tokens":  128,
		"stream":      true,
		"tools":       toolsAnt,
		"tool_choice": map[string]any{"type": "tool", "name": "Lookup"},
		"messages": []map[string]any{
			{"role": "user", "content": "seed"},
			{"role": "assistant", "content": "seed-done"},
			{"role": "user", "content": "mixed reject stream"},
		},
	})
	if err != nil {
		t.Fatalf("Anthropic stream post err: %v", err)
	}
	antRaw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK || strings.Contains(string(antRaw), "call_lookup_mixed") || strings.Contains(string(antRaw), `"type":"tool_use"`) {
		t.Fatalf("Anthropic stream leaked unresumable tool_use: status=%d body=%s", resp.StatusCode, antRaw)
	}

	// 3. 合规只发 Lookup -> 返回 call_lookup_clean；随后带同一 X-Thread-Id（即使更换 X-Conversation-Id 也应通过 checkoutByThreadID 命中）回传结果必须 200 成功
	behavior.Store(2)
	code, body = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"X-Thread-Id": knownTID}, map[string]any{
		"model":       "smart",
		"stream":      false,
		"tools":       toolsOAI,
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "Lookup"}},
		"messages": []map[string]any{
			{"role": "user", "content": "seed"},
			{"role": "assistant", "content": "seed-done"},
			{"role": "user", "content": "clean lookup"},
		},
	})
	if code != http.StatusOK || !strings.Contains(string(body), "call_lookup_clean") {
		t.Fatalf("expected clean Lookup tool call, got code=%d body=%s", code, body)
	}

	wsCountBeforeResume := mock.wsConnCount.Load()
	code, body = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"X-Thread-Id": knownTID}, map[string]any{
		"model":  "smart",
		"stream": false,
		"tools":  toolsOAI,
		"messages": []map[string]any{
			{"role": "user", "content": "seed"},
			{"role": "assistant", "content": "seed-done"},
			{"role": "user", "content": "clean lookup"},
			{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{"id": "call_lookup_clean", "type": "function", "function": map[string]any{"name": "Lookup", "arguments": `{"q":"lookup"}`}}}},
			{"role": "tool", "tool_call_id": "call_lookup_clean", "content": "FOUND_VAL"},
		},
	})
	if code != http.StatusOK || !strings.Contains(string(body), "explicit-tool-result-ok") {
		t.Fatalf("expected explicit X-Thread-Id tool_result resume 200 OK, got code=%d body=%s", code, body)
	}
	if mock.wsConnCount.Load() != wsCountBeforeResume {
		t.Fatalf("expected live in-place WS reuse for explicit thread tool_result, wsDelta=%d", mock.wsConnCount.Load()-wsCountBeforeResume)
	}
}

