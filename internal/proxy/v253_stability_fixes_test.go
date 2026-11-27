package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"quickrelay-studio/internal/config"
)

func buildMassiveMCPToolsAnthropic(count int) []map[string]any {
	tools := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		props := make(map[string]any, 18)
		reqFields := make([]string, 0, 8)
		for p := 0; p < 18; p++ {
			pName := fmt.Sprintf("parameter_field_%02d_selector_configuration", p)
			prop := map[string]any{
				"type":        "string",
				"description": strings.Repeat("Detailed MCP parameter explanation and constraints. ", 6),
			}
			if p%3 == 0 {
				prop["enum"] = []string{
					fmt.Sprintf("option_alpha_%02d_%02d", i, p),
					fmt.Sprintf("option_beta_%02d_%02d", i, p),
					fmt.Sprintf("option_gamma_%02d_%02d", i, p),
					fmt.Sprintf("option_delta_%02d_%02d", i, p),
				}
			}
			props[pName] = prop
			if p < 6 {
				reqFields = append(reqFields, pName)
			}
		}
		tools = append(tools, map[string]any{
			"name":        fmt.Sprintf("mcp__heavy_suite_%02d__execute_complex_browser_workflow_%02d", i/8, i),
			"description": strings.Repeat("Comprehensive MCP browser and workspace automation action. ", 10),
			"input_schema": map[string]any{
				"type":       "object",
				"properties": props,
				"required":   reqFields,
			},
		})
	}
	return tools
}

// TestV253_CompactRequestSkipsToolProtocolAndOldImages 验证携带 16 张历史图片 + 大量 MCP 工具的 /compact 请求
// 自动跳过客户端工具协议注入与旧图片二进制提取，不再触发 64 KB 单帧超限死锁。
func TestV253_CompactRequestSkipsToolProtocolAndOldImages(t *testing.T) {
	mock := newMockOutpostServer(t)
	var seenUserMessageText atomic.Value
	var uploadedImageCount atomic.Int32
	mock.onWSInbound = func(conn *websocket.Conn, writeFrame func(any) error, ev map[string]any, connIndex int32) {
		if ev["type"] != "user_message" {
			return
		}
		p, _ := ev["payload"].(map[string]any)
		txt, _ := p["text"].(string)
		seenUserMessageText.Store(txt)
		if atts, ok := p["attachments"].([]any); ok {
			for _, a := range atts {
				am, _ := a.(map[string]any)
				if name, _ := am["name"].(string); strings.HasPrefix(name, "img-") {
					uploadedImageCount.Add(1)
				}
			}
		}
		_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m_cmp"}})
		_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "<summary>Compacted session summary.</summary>"}})
		_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "<summary>Compacted session summary.</summary>"}})
		_ = writeFrame(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
	}

	_, httpSrv := setupOutpostTestServer(t, mock, config.ToolModeAuto)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}

	var msgs []map[string]any
	for i := 0; i < 16; i++ {
		b64 := fmt.Sprintf("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")
		// 让每张图片字节略有区分以覆盖满 16 个不同图片槽位
		rawBytes := append([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, byte(i)}, []byte("PNGDATA")...)
		_ = b64
		msgs = append(msgs,
			map[string]any{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": fmt.Sprintf("Historical screenshot #%d", i+1)},
					{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": encodeBase64TestBytes(rawBytes)}},
				},
			},
			map[string]any{
				"role":    "assistant",
				"content": fmt.Sprintf("Checked screenshot #%d.", i+1),
			},
		)
	}
	msgs = append(msgs, map[string]any{
		"role":    "user",
		"content": "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\nYour task is to create a detailed summary of the conversation so far.",
	})

	code, body := doProxyJSONCall(t, client, httpSrv.URL+"/v1/messages", nil, map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 1024,
		"stream":     false,
		"system":     strings.Repeat("Workspace guideline rule line.\n", 400),
		"tools":      buildMassiveMCPToolsAnthropic(70),
		"messages":   msgs,
	})
	if code != http.StatusOK {
		t.Fatalf("expected /compact with 16 images + 70 MCP tools to succeed (200), got %d: %s", code, string(body))
	}
	if !strings.Contains(string(body), "Compacted session summary.") {
		t.Fatalf("unexpected /compact response: %s", string(body))
	}
	if uploadedImageCount.Load() != 0 {
		t.Fatalf("expected /compact to skip uploading historical binary images, got %d", uploadedImageCount.Load())
	}
	sentTxt, _ := seenUserMessageText.Load().(string)
	if strings.Contains(sentTxt, "mcp__heavy_suite_") || strings.Contains(sentTxt, `{"tools":`) {
		t.Fatalf("expected /compact not to inject tool definitions, got len=%d", len(sentTxt))
	}
	if mock.putCount != 0 {
		t.Fatalf("expected /compact not to register virtual Outpost, got putCount=%d", mock.putCount)
	}
}

// TestV253_OversizedMCPToolsWith16ImagesOffloadsToolsAttachment 验证普通 text_bridge 请求在携带 16 张图片附件
// 与超大型 MCP 工具集导致元数据 + 工具协议超过 63 KiB 时，自动执行第 3 档 Schema 瘦身并按需外置 tools-*.txt 附件而不报 64 KB 错误。
func TestV253_OversizedMCPToolsWith16ImagesOffloadsToolsAttachment(t *testing.T) {
	payload := map[string]any{
		"surfaceType": "QUICK_WORK_DESKTOP_WINDOWS",
		"thread_id":   "11111111-2222-3333-4444-555555555555",
		"message_id":  "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"agent_mode":  "smart",
	}
	// 构造超大工具定义集合，使其即便经第 3 档 Schema 压缩并在 JSON 字符串二次转义后加上 18 个附件元数据仍超过 63 KiB
	tools := make([]Tool, 0, 140)
	for i := 0; i < 140; i++ {
		props := make(map[string]any, 16)
		reqList := make([]string, 0, 8)
		for p := 0; p < 16; p++ {
			pn := fmt.Sprintf("arg_field_%02d_long_identifier", p)
			props[pn] = map[string]any{
				"type": "string",
				"enum": []string{"mode_standard_alpha", "mode_extended_beta", "mode_fallback_gamma"},
			}
			if p < 8 {
				reqList = append(reqList, pn)
			}
		}
		rawParams, _ := jsonMarshalNoErr(map[string]any{
			"type":       "object",
			"properties": props,
			"required":   reqList,
		})
		tools = append(tools, Tool{
			Type: "function",
			Function: ToolFunction{
				Name:        fmt.Sprintf("mcp__enterprise_connector_%03d__perform_action_%03d", i, i),
				Description: "Enterprise connector workspace operation with structured arguments.",
				Parameters:  rawParams,
			},
		})
	}
	toolProto := buildToolBridgePromptForMode(&ChatRequest{Tools: tools}, "auto", "", true, config.WorkModeGeneral)

	atts := make([]map[string]any, 0, 16)
	for i := 0; i < 16; i++ {
		atts = append(atts, placeholderPromptAttachment(fmt.Sprintf("img-%02d-abcdef12.png", i+1)))
	}

	var uploadedNames []string
	err := prepareFrameWithinBudgetDetailed(
		payload,
		"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		strings.Repeat("System rule line for workspace.\n", 300),
		toolProto,
		"",
		"History block\n\nUser: 请继续修复样式并更新组件",
		"History block",
		"User: 请继续修复样式并更新组件",
		"请继续修复样式并更新组件",
		false,
		atts,
		nil,
		true,
		config.WorkModeGeneral,
		func(name string, data []byte) (attachmentRef, error) {
			uploadedNames = append(uploadedNames, name)
			return attachmentRef{
				Name: name,
				Size: len(data),
				Path: "/data/s3files/users/u1/sessions/s1/workspace/attached_files/" + name,
				ID:   "att-" + name,
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("prepareFrameWithinBudgetDetailed failed unexpectedly: %v", err)
	}
	n, bErr := userMessageBytes(payload)
	if bErr != nil || n > maxUserMessageBytes {
		t.Fatalf("expected final frame <= %d bytes, got n=%d err=%v", maxUserMessageBytes, n, bErr)
	}
	finalText, _ := payload["text"].(string)
	if !strings.Contains(finalText, "<client_tool_protocol>") || !strings.Contains(finalText, "请继续修复样式并更新组件") {
		t.Fatalf("expected inline tool protocol anchor and verbatim current user turn in final frame, got:\n%s", finalText)
	}
	sawToolsUpload := false
	for _, u := range uploadedNames {
		if strings.HasPrefix(u, "tools-") {
			sawToolsUpload = true
		}
	}
	if !sawToolsUpload {
		t.Fatalf("expected tools-*.txt attachment offload when 140 tools + 16 images exceed 63 KiB, uploaded=%v", uploadedNames)
	}
}

// TestV253_OutpostEarlyThinkingDisconnectAutoRetriesAndSendsWSPing 验证：
// 1) 虚拟 Outpost 流式请求在早期 thinking_delta 阶段遭遇非预期 WebSocket 断开时，自动清空暂存思考并重连成功；
// 2) 重连后的长思考跨越多次 pingTicker 保活周期时不会误判连接关闭，且持续发送应用层 ping 与底层 RFC6455 Ping 控制帧。
func TestV253_OutpostEarlyThinkingDisconnectAutoRetriesAndSendsWSPing(t *testing.T) {
	oldKeep := outpostKeepaliveInterval
	outpostKeepaliveInterval = 30 * time.Millisecond
	defer func() { outpostKeepaliveInterval = oldKeep }()

	var wsAttempt atomic.Int32
	var controlPingCount atomic.Int32

	mock := newMockOutpostServer(t)
	mock.onWSInbound = func(conn *websocket.Conn, writeFrame func(any) error, ev map[string]any, connIndex int32) {
		conn.SetPingHandler(func(appData string) error {
			controlPingCount.Add(1)
			return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
		})
		if ev["type"] != "user_message" {
			return
		}
		attempt := wsAttempt.Add(1)
		if attempt == 1 {
			go func() {
				_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m_drop"}})
				_ = writeFrame(map[string]any{"type": "thinking_delta", "payload": map[string]any{"delta": "stale-thinking-before-drop "}})
				time.Sleep(15 * time.Millisecond)
				_ = conn.Close()
			}()
			return
		}
		// 第 2 次尝试故意持续思考跨越 3 个以上 keepalive 周期（110ms > 3*30ms），验证 pingTicker 分支正常 continue 不会误断连接
		go func() {
			_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m_ok"}})
			_ = writeFrame(map[string]any{"type": "thinking_delta", "payload": map[string]any{"delta": "clean-thinking "}})
			time.Sleep(110 * time.Millisecond)
			_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "Recovered seamlessly."}})
			_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "Recovered seamlessly."}})
			_ = writeFrame(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
		}()
	}

	_, httpSrv := setupOutpostTestServer(t, mock, config.ToolModeAuto)
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}}

	resp, err := doRawJSONPost(t, client, httpSrv.URL+"/v1/messages", nil, map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 512,
		"stream":     true,
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": 20000},
		"tools": []map[string]any{
			{"name": "Read", "description": "Read local file", "input_schema": map[string]any{"type": "object"}},
		},
		"messages": []map[string]any{{"role": "user", "content": "请检查模块并回答"}},
	})
	if err != nil {
		t.Fatalf("POST stream failed: %v", err)
	}
	rawBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	sse := string(rawBytes)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK after seamless retry, got %d: %s", resp.StatusCode, sse)
	}
	if wsAttempt.Load() != 2 {
		t.Fatalf("expected 2 upstream attempts (1 drop + 1 recovery), got %d", wsAttempt.Load())
	}
	if strings.Contains(sse, "stale-thinking-before-drop") {
		t.Fatalf("expected stale thinking from dropped attempt 1 to be discarded, got:\n%s", sse)
	}
	if !strings.Contains(sse, "Recovered seamlessly.") || !strings.Contains(sse, "clean-thinking") {
		t.Fatalf("expected clean thinking and final text in SSE output, got:\n%s", sse)
	}
	if controlPingCount.Load() < 2 {
		t.Fatalf("expected at least 2 RFC6455 WebSocket Ping control frames across multi-keepalive wait, got %d", controlPingCount.Load())
	}
}

// TestV253_OldHistoricalImagesAllowNativeOutpostWhileRecentImagesFallback 验证：
// 早于最近 6 条消息的陈年旧截图自动转为占位文本并走虚拟 Outpost 原生工具；而近期新发图片仍回退 text_bridge 真实上传。
func TestV253_OldHistoricalImagesAllowNativeOutpostWhileRecentImagesFallback(t *testing.T) {
	mock := newMockOutpostServer(t)
	mock.onWSInbound = func(conn *websocket.Conn, writeFrame func(any) error, ev map[string]any, connIndex int32) {
		if ev["type"] != "user_message" {
			return
		}
		_ = writeFrame(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m_hist_img"}})
		_ = writeFrame(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "ok-with-old-image"}})
		_ = writeFrame(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "ok-with-old-image"}})
		_ = writeFrame(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
	}

	_, httpSrv := setupOutpostTestServer(t, mock, config.ToolModeAuto)
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}}
	tinyPNG := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	toolsAnt := []map[string]any{
		{"name": "Read", "description": "Read file", "input_schema": map[string]any{"type": "object"}},
	}

	// 1. 第 1 条消息含陈年旧图，后续已有 8 条纯文本对话 -> 允许走虚拟 Outpost (putCount 增加)
	oldImgMsgs := []map[string]any{
		{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": "早期截图"},
				{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": tinyPNG}},
			},
		},
		{"role": "assistant", "content": "已查看早期截图。"},
		{"role": "user", "content": "步骤 1"},
		{"role": "assistant", "content": "完成 1"},
		{"role": "user", "content": "步骤 2"},
		{"role": "assistant", "content": "完成 2"},
		{"role": "user", "content": "步骤 3"},
		{"role": "assistant", "content": "完成 3"},
		{"role": "user", "content": "现在请继续重构音频逻辑"},
	}
	code, body := doProxyJSONCall(t, client, httpSrv.URL+"/v1/messages", nil, map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 256,
		"stream":     false,
		"tools":      toolsAnt,
		"messages":   oldImgMsgs,
	})
	if code != http.StatusOK {
		t.Fatalf("old historical image request failed: %d %s", code, string(body))
	}
	if mock.putCount != 1 {
		t.Fatalf("expected old historical image (>6 messages ago) to use native Outpost, got putCount=%d", mock.putCount)
	}
	foundOmittedMarker := false
	mock.mu.Lock()
	for _, ev := range mock.importedEvents {
		data, _ := ev["data"].(map[string]any)
		if c, _ := data["content"].(string); strings.Contains(c, outpostOmittedMediaMarker) {
			foundOmittedMarker = true
		}
	}
	mock.mu.Unlock()
	if !foundOmittedMarker {
		t.Fatalf("expected imported history events to include %q", outpostOmittedMediaMarker)
	}

	// 2. 上一轮（最近 6 条消息内）刚发的图片 -> 保持回退 text_bridge 真实上传图片 (putCount 不增加)
	prevPut := mock.putCount
	recentImgMsgs := []map[string]any{
		{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": "刚发的截图"},
				{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": tinyPNG}},
			},
		},
		{"role": "assistant", "content": "看到了。"},
		{"role": "user", "content": "这张图右上角写的是什么？"},
	}
	code2, body2 := doProxyJSONCall(t, client, httpSrv.URL+"/v1/messages", nil, map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 256,
		"stream":     false,
		"tools":      toolsAnt,
		"messages":   recentImgMsgs,
	})
	if code2 != http.StatusOK {
		t.Fatalf("recent image request failed: %d %s", code2, string(body2))
	}
	if mock.putCount != prevPut {
		t.Fatalf("expected recent image within 6 messages to fallback to text_bridge, putCount %d->%d", prevPut, mock.putCount)
	}
}

func encodeBase64TestBytes(seed []byte) string {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	c := byte(0)
	if len(seed) > 8 {
		c = seed[8]
	}
	img.Set(0, 0, color.RGBA{R: c, G: 128, B: 64, A: 255})
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func jsonMarshalNoErr(v any) ([]byte, error) {
	return json.Marshal(v)
}
