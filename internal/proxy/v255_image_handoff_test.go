package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"quickrelay-studio/internal/config"
)

// TestV255_OutpostReadImageToolResultDetachesToFreshThreadAndRecovers 端到端复现并验证：
// 1) 第 1 轮走虚拟 Outpost 下发原生 Read("home.png") 工具调用并挂起等待 tool_result；
// 2) 第 2 轮 Claude Code 回传含 Base64 图片的 tool_result 时，网关自动关闭反注册原挂起 Outpost，
//    切换全新 thread_id 携带完整历史 + 完整 <client_tool_protocol> + 上传图片附件走 text_bridge 秒回，绝不向原挂起 Outpost thread_id 发普通 user_message 死锁；
// 3) 后续纯文本对话跨过最近 6 条消息窗口后，再次平滑冷导入恢复到全新的虚拟 Outpost 会话。
func TestV255_OutpostReadImageToolResultDetachesToFreshThreadAndRecovers(t *testing.T) {
	mock := newMockOutpostServer(t)

	var mu sync.Mutex
	var turn1OutpostThreadID string
	var turn2TextBridgeThreadID string
	var turn2PromptText string
	var turn2UploadedImages int
	var turn2SawSessionCreate bool
	var deadlockedReuseDetected bool

	mock.onWSInbound = func(conn *websocket.Conn, writeFrame func(any) error, ev map[string]any, connIndex int32) {
		typ, _ := ev["type"].(string)
		p, _ := ev["payload"].(map[string]any)
		if typ == "session_create" {
			sid, _ := p["session_id"].(string)
			mu.Lock()
			if turn1OutpostThreadID != "" && turn2TextBridgeThreadID == "" {
				turn2SawSessionCreate = true
				turn2TextBridgeThreadID = sid
			}
			mu.Unlock()
			return
		}
		if typ != "user_message" {
			return
		}

		tid, _ := p["thread_id"].(string)
		oid, _ := p["outpostId"].(string)

		mock.mu.Lock()
		outpostObj, isRegisteredOutpost := mock.activeOutposts[oid]
		mock.mu.Unlock()

		if isRegisteredOutpost {
			mu.Lock()
			isFirstOutpost := turn1OutpostThreadID == ""
			if isFirstOutpost {
				turn1OutpostThreadID = tid
			}
			mu.Unlock()

			if isFirstOutpost {
				caps := outpostObj["capabilities"].([]any)
				tools := caps[0].(map[string]any)["tools"].([]any)
				readAlias := tools[0].(map[string]any)["name"].(string)
				_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{"role": "assistant"}})
				_ = writeFrame(map[string]any{
					"type": "tool_call",
					"payload": map[string]any{
						"tool_call_id": "toolu_read_img_1",
						"tool_name":    readAlias,
						"arguments":    map[string]any{"file_path": "E:/codex/screenshots/home.png"},
					},
				})
				_ = writeFrame(map[string]any{
					"type":    "turn_usage",
					"payload": map[string]any{"input_tokens": 120, "output_tokens": 30},
				})
				return
			}

			// 后续恢复到虚拟 Outpost 的纯文本轮次
			_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m_outpost_resumed"}})
			_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "Native outpost resumed cleanly."}})
			_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "Native outpost resumed cleanly."}})
			_ = writeFrame(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
			return
		}

		// 非虚拟 Outpost（即 text_bridge 路径）：若复用了第 1 轮挂起等待 tool_result 的 thread_id，模拟云端挂起死锁！
		mu.Lock()
		if tid != "" && tid == turn1OutpostThreadID {
			deadlockedReuseDetected = true
			mu.Unlock()
			// 不回任何实质事件，立即暴露死锁错误
			_ = writeFrame(map[string]any{"type": "error", "code": "DeadlockedOutpostThreadReuse", "message": "reused parked outpost thread in text_bridge"})
			return
		}
		if turn2TextBridgeThreadID == "" {
			turn2TextBridgeThreadID = tid
		}
		txt, _ := p["text"].(string)
		turn2PromptText = txt
		if atts, ok := p["attachments"].([]any); ok {
			for _, a := range atts {
				am, _ := a.(map[string]any)
				if name, _ := am["name"].(string); strings.HasPrefix(name, "img-") {
					turn2UploadedImages++
				}
			}
		}
		mu.Unlock()

		_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m_tb"}})
		_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "Image inspected via fresh text_bridge thread."}})
		_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "Image inspected via fresh text_bridge thread."}})
		_ = writeFrame(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
	}

	srv, httpSrv := setupOutpostTestServer(t, mock, config.ToolModeAuto)
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}}
	toolsAnt := []map[string]any{
		{
			"name":        "Read",
			"description": "Read a file or screenshot from local workspace",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"file_path": map[string]any{"type": "string"}},
				"required":   []string{"file_path"},
			},
		},
	}

	// 第 1 轮：虚拟 Outpost 触发 Read("E:/codex/screenshots/home.png")
	userQ1 := "请读取首页截图 home.png 检查顶部导航对齐情况"
	code1, body1 := doProxyJSONCall(t, client, httpSrv.URL+"/v1/messages", nil, map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 512,
		"stream":     false,
		"tools":      toolsAnt,
		"messages": []map[string]any{
			{"role": "user", "content": userQ1},
		},
	})
	if code1 != http.StatusOK {
		t.Fatalf("turn 1 failed (%d): %s", code1, string(body1))
	}
	if !strings.Contains(string(body1), "toolu_read_img_1") {
		t.Fatalf("expected native tool_use toolu_read_img_1 in turn 1 response, got: %s", string(body1))
	}
	if mock.putCount != 1 {
		t.Fatalf("expected 1 virtual outpost registration on turn 1, got %d", mock.putCount)
	}

	// 第 2 轮：客户端回传包含 PNG Base64 图片的 tool_result
	tinyPNG := encodeBase64TestBytes([]byte("0123456789"))
	msgs := []map[string]any{
		{"role": "user", "content": userQ1},
		{
			"role": "assistant",
			"content": []map[string]any{
				{
					"type":  "tool_use",
					"id":    "toolu_read_img_1",
					"name":  "Read",
					"input": map[string]any{"file_path": "E:/codex/screenshots/home.png"},
				},
			},
		},
		{
			"role": "user",
			"content": []map[string]any{
				{
					"type":        "tool_result",
					"tool_use_id": "toolu_read_img_1",
					"content": []map[string]any{
						{
							"type": "image",
							"source": map[string]any{
								"type":       "base64",
								"media_type": "image/png",
								"data":       tinyPNG,
							},
						},
					},
				},
			},
		},
	}

	code2, body2 := doProxyJSONCall(t, client, httpSrv.URL+"/v1/messages", nil, map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 512,
		"stream":     false,
		"tools":      toolsAnt,
		"messages":   msgs,
	})
	if code2 != http.StatusOK {
		t.Fatalf("turn 2 with image tool_result failed (%d): %s", code2, string(body2))
	}
	if !strings.Contains(string(body2), "Image inspected via fresh text_bridge thread.") {
		t.Fatalf("unexpected turn 2 body: %s", string(body2))
	}

	mu.Lock()
	deadlocked := deadlockedReuseDetected
	t1ID := turn1OutpostThreadID
	t2ID := turn2TextBridgeThreadID
	sawCreate := turn2SawSessionCreate
	pText := turn2PromptText
	upImgs := turn2UploadedImages
	mu.Unlock()

	if deadlocked {
		t.Fatalf("turn 2 fatally reused parked native_outpost thread_id %q on text_bridge", t1ID)
	}
	if t1ID == "" || t2ID == "" || t1ID == t2ID {
		t.Fatalf("expected distinct fresh thread_id on text_bridge handoff, got turn1=%q turn2=%q", t1ID, t2ID)
	}
	if !sawCreate {
		t.Fatalf("expected session_create for fresh text_bridge thread %q", t2ID)
	}
	if upImgs != 1 {
		t.Fatalf("expected 1 uploaded PNG attachment on turn 2, got %d", upImgs)
	}
	if !strings.Contains(pText, userQ1) || !strings.Contains(pText, `"name":"Read"`) {
		t.Fatalf("expected full history and full tool definitions in fresh text_bridge turn, got:\n%s", pText)
	}
	mock.mu.Lock()
	unregAfterTurn2 := mock.unregisterCount
	mock.mu.Unlock()
	if unregAfterTurn2 < 1 {
		t.Fatalf("expected parked turn 1 virtual outpost to be closed and unregistered on text_bridge handoff, unregisterCount=%d", unregAfterTurn2)
	}
	if srv.outposts.HasSessionOrThread("", t1ID) {
		t.Fatalf("expected turn 1 outpost thread %q to be evicted from outpostPool", t1ID)
	}

	// 追加足够多纯文本轮次使第 2 轮图片滑出最近 6 条窗口，验证再次平滑恢复到全新虚拟 Outpost
	msgs = append(msgs,
		map[string]any{"role": "assistant", "content": "Image inspected via fresh text_bridge thread."},
		map[string]any{"role": "user", "content": "纯文本追问 1"},
		map[string]any{"role": "assistant", "content": "回答 1"},
		map[string]any{"role": "user", "content": "纯文本追问 2"},
		map[string]any{"role": "assistant", "content": "回答 2"},
		map[string]any{"role": "user", "content": "纯文本追问 3"},
		map[string]any{"role": "assistant", "content": "回答 3"},
		map[string]any{"role": "user", "content": "现在所有图片都已在 6 条之前，请继续检查代码"},
	)
	prevPut := mock.putCount
	code3, body3 := doProxyJSONCall(t, client, httpSrv.URL+"/v1/messages", nil, map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 512,
		"stream":     false,
		"tools":      toolsAnt,
		"messages":   msgs,
	})
	if code3 != http.StatusOK {
		t.Fatalf("turn 3 recovery to native_outpost failed (%d): %s", code3, string(body3))
	}
	if !strings.Contains(string(body3), "Native outpost resumed cleanly.") {
		t.Fatalf("unexpected turn 3 response: %s", string(body3))
	}
	if mock.putCount <= prevPut {
		t.Fatalf("expected conversation to recover back to native Outpost once image aged past 6 messages, putCount=%d", mock.putCount)
	}
}

// TestV255_OutpostOversizedFallbackDetachesToFreshThread 验证虚拟 Outpost 会话在后续轮次因单帧超 63 KiB
// 触发 ErrOutpostUnsupported 自动回退 text_bridge 时，同样销毁挂起 Outpost 并切换全新 thread_id。
func TestV255_OutpostOversizedFallbackDetachesToFreshThread(t *testing.T) {
	mock := newMockOutpostServer(t)

	var mu sync.Mutex
	var outpostThreadID string
	var fallbackThreadID string

	mock.onWSInbound = func(conn *websocket.Conn, writeFrame func(any) error, ev map[string]any, connIndex int32) {
		if ev["type"] != "user_message" {
			return
		}
		p, _ := ev["payload"].(map[string]any)
		tid, _ := p["thread_id"].(string)
		oid, _ := p["outpostId"].(string)
		mock.mu.Lock()
		_, isOutpost := mock.activeOutposts[oid]
		mock.mu.Unlock()

		mu.Lock()
		if isOutpost {
			outpostThreadID = tid
		} else {
			fallbackThreadID = tid
		}
		mu.Unlock()

		_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m_fb"}})
		_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "Handled cleanly."}})
		_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "Handled cleanly."}})
		_ = writeFrame(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
	}

	_, httpSrv := setupOutpostTestServer(t, mock, config.ToolModeAuto)
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}}
	toolsAnt := []map[string]any{
		{"name": "Read", "description": "Read local file", "input_schema": map[string]any{"type": "object"}},
	}

	code1, _ := doProxyJSONCall(t, client, httpSrv.URL+"/v1/messages", nil, map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 256,
		"stream":     false,
		"tools":      toolsAnt,
		"messages":   []map[string]any{{"role": "user", "content": "第一轮简短问题"}},
	})
	if code1 != http.StatusOK {
		t.Fatalf("turn 1 failed: %d", code1)
	}

	// 第 2 轮发送 > 65 KB 文本使虚拟 Outpost 预检返回 ErrOutpostUnsupported 并回退 text_bridge
	hugeUserTurn := "第二轮超长日志粘贴：\n" + strings.Repeat("ERROR line stacktrace detail payload segment 0123456789\n", 1300)
	code2, body2 := doProxyJSONCall(t, client, httpSrv.URL+"/v1/messages", nil, map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 256,
		"stream":     false,
		"tools":      toolsAnt,
		"messages": []map[string]any{
			{"role": "user", "content": "第一轮简短问题"},
			{"role": "assistant", "content": "Handled cleanly."},
			{"role": "user", "content": hugeUserTurn},
		},
	})
	if code2 != http.StatusOK {
		t.Fatalf("turn 2 oversized fallback failed (%d): %s", code2, string(body2))
	}

	mu.Lock()
	t1, t2 := outpostThreadID, fallbackThreadID
	mu.Unlock()
	if t1 == "" || t2 == "" || t1 == t2 {
		t.Fatalf("expected oversized fallback to switch to fresh thread_id, got outpost=%q fallback=%q", t1, t2)
	}
	mock.mu.Lock()
	unreg := mock.unregisterCount
	mock.mu.Unlock()
	if unreg < 1 {
		t.Fatalf("expected idle outpost from turn 1 to be unregistered upon fallback, got unregisterCount=%d", unreg)
	}
}

// TestV255_LegacyStalledResumedThreadAutoHealsBeforePrestart 模拟 SQLite 中遗留 v2.5.4 旧格式 ("v2:...")
// 的死锁线程（云端只发保活帧、不吐任何模型推理事件）：验证 wsResumeFirstProgressTimeout 在 SSE 首包预热前主动中断连接，
// 触发 retryNewSession 以全新 thread_id + 完整历史无缝自愈返回 200 OK。
func TestV255_LegacyStalledResumedThreadAutoHealsBeforePrestart(t *testing.T) {
	oldFirstProg := wsResumeFirstProgressTimeout
	wsResumeFirstProgressTimeout = 90 * time.Millisecond
	defer func() { wsResumeFirstProgressTimeout = oldFirstProg }()

	mock := newMockOutpostServer(t)
	const legacyStalledThread = "71d1c9de-d338-4e66-8209-524f2d637ca3"

	var wsUserMsgCalls atomic.Int32
	var healedThreadID atomic.Value
	var healedPrompt atomic.Value

	mock.onWSInbound = func(conn *websocket.Conn, writeFrame func(any) error, ev map[string]any, connIndex int32) {
		if ev["type"] != "user_message" {
			return
		}
		callNum := wsUserMsgCalls.Add(1)
		p, _ := ev["payload"].(map[string]any)
		tid, _ := p["thread_id"].(string)
		txt, _ := p["text"].(string)

		if tid == legacyStalledThread {
			// 模拟云端挂起死锁：每 25ms 发一次非模型心跳帧刷新底层读取，但绝不发任何模型推理事件
			go func() {
				for i := 0; i < 10; i++ {
					if err := writeFrame(map[string]any{"type": "status", "payload": map[string]any{"status": "pong"}}); err != nil {
						return
					}
					time.Sleep(25 * time.Millisecond)
				}
			}()
			return
		}

		healedThreadID.Store(tid)
		healedPrompt.Store(txt)
		_ = callNum
		_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m_healed"}})
		_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "Auto-healed from legacy stalled thread!"}})
		_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "Auto-healed from legacy stalled thread!"}})
		_ = writeFrame(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
	}

	srv, httpSrv := setupOutpostTestServer(t, mock, config.ToolModeTextBridge)
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}}

	// 预置一条模拟 v2.5.4 遗留的 "v2:..." 会话映射指向死锁 thread_id
	prevMsgs := []Message{
		{Role: "user", Content: "早期问题：检查首页布局"},
	}
	asstMsg := Message{Role: "assistant", Content: "好的，正在查看。"}
	asstHash := assistantMessageOutputFingerprint(asstMsg)

	reqPreview := &ChatRequest{
		Model:    "claude-opus-5",
		Messages: append(append([]Message{}, prevMsgs...), asstMsg, Message{Role: "user", Content: "请继续分析截图细节"}),
	}
	dummyHTTPReq, _ := http.NewRequest("POST", httpSrv.URL+"/v1/messages", nil)
	baseHash := encodeBaseHash("", "")
	convKey := conversationKey(dummyHTTPReq, reqPreview, baseHash)
	conv, epoch, ver, err := srv.sessions.acquire(context.Background(), convKey)
	if err != nil {
		t.Fatalf("acquire conv: %v", err)
	}
	srv.sessions.saveWithAgent(conv, epoch, ver, legacyStalledThread, 1, baseHash, "", prevMsgs, asstHash)
	srv.sessions.release(conv)

	// 发起流式 SSE 请求（同时携带高思考预算），验证在预热前自愈返回 200 OK
	resp, err := doRawJSONPost(t, client, httpSrv.URL+"/v1/messages", nil, map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 512,
		"stream":     true,
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": 16000},
		"messages": []map[string]any{
			{"role": "user", "content": "早期问题：检查首页布局"},
			{"role": "assistant", "content": "好的，正在查看。"},
			{"role": "user", "content": fmt.Sprintf("请继续分析截图细节（timestamp=%d）", time.Now().UnixNano())},
		},
	})
	if err != nil {
		t.Fatalf("stream request failed: %v", err)
	}
	rawSSE, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	sseText := string(rawSSE)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK after stalled session auto-heal, got %d: %s", resp.StatusCode, sseText)
	}
	if !strings.Contains(sseText, "Auto-healed from legacy stalled thread!") {
		t.Fatalf("expected auto-healed message in SSE stream, got:\n%s", sseText)
	}
	if wsUserMsgCalls.Load() != 2 {
		t.Fatalf("expected 2 upstream attempts (1 stalled resume + 1 fresh session), got %d", wsUserMsgCalls.Load())
	}
	newTID, _ := healedThreadID.Load().(string)
	if newTID == "" || newTID == legacyStalledThread {
		t.Fatalf("expected fresh thread_id after auto-heal, got %q", newTID)
	}
	fullTxt, _ := healedPrompt.Load().(string)
	if !strings.Contains(fullTxt, "早期问题：检查首页布局") {
		t.Fatalf("expected full historical transcript replayed on fresh thread, got:\n%s", fullTxt)
	}
}
