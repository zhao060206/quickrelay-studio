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
	"quickrelay-studio/internal/store"
)

// TestUsageAdd_ContextSnapshotUsesLastStepNotSum 验证多步累加保留计费总量的同时，返回给客户端的上下文占用取最后一个非零步快照。
func TestUsageAdd_ContextSnapshotUsesLastStepNotSum(t *testing.T) {
	var total Usage
	cost := 0.1
	for i := 0; i < 5; i++ {
		c := cost
		total.add(Usage{
			PromptTokens:     3,
			CacheReadTokens:  210000,
			CacheWriteTokens: 15000,
			CompletionTokens: 800,
			CostUSD:          &c,
		})
	}
	// 末尾再追加一个仅含输出、三项输入为 0 的事件，不应抹掉前面的有效单步上下文快照
	total.add(Usage{
		CompletionTokens: 200,
	})

	if total.PromptTokens != 15 || total.CacheReadTokens != 1050000 || total.CacheWriteTokens != 75000 || total.CompletionTokens != 4200 {
		t.Fatalf("unexpected accumulated totals: %+v", total)
	}
	if total.CostUSD == nil || *total.CostUSD < 0.499 || *total.CostUSD > 0.501 {
		t.Fatalf("unexpected accumulated CostUSD: %v", total.CostUSD)
	}

	inTok, crTok, cwTok := total.ContextSnapshot()
	if inTok != 3 || crTok != 210000 || cwTok != 15000 {
		t.Fatalf("ContextSnapshot() = (%d, %d, %d), want (3, 210000, 15000)", inTok, crTok, cwTok)
	}

	au := anthropicUsageMap(total)
	if au["input_tokens"] != 3 || au["cache_read_input_tokens"] != 210000 || au["cache_creation_input_tokens"] != 15000 || au["output_tokens"] != 4200 {
		t.Fatalf("unexpected anthropicUsageMap: %+v", au)
	}

	ou := usageMap(total)
	if ou["prompt_tokens"] != 225003 || ou["completion_tokens"] != 4200 || ou["total_tokens"] != 229203 {
		t.Fatalf("unexpected OpenAI usageMap: %+v", ou)
	}
	details, ok := ou["prompt_tokens_details"].(map[string]any)
	if !ok || details["cached_tokens"] != 210000 {
		t.Fatalf("unexpected prompt_tokens_details: %+v", ou["prompt_tokens_details"])
	}
}

// TestAnthropicStreamAndSync_PreservesRequestedModelAndSingleStepContext 验证同步与流式 /v1/messages 均回传原始请求模型名且只上报最后一步真实上下文大小。
func TestAnthropicStreamAndSync_PreservesRequestedModelAndSingleStepContext(t *testing.T) {
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
					writeEvent(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m_ctx"}})
					writeEvent(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "上下文上报验证响应。"}})
					steps := []struct {
						in, out, cr, cw int
					}{
						{3, 100, 180000, 10000},
						{3, 200, 200000, 12000},
						{3, 300, 212000, 15000},
					}
					for _, stp := range steps {
						writeEvent(map[string]any{
							"type": "turn_usage",
							"payload": map[string]any{
								"input_tokens":       stp.in,
								"output_tokens":      stp.out,
								"total_tokens":       stp.in + stp.out,
								"cache_read_tokens":  stp.cr,
								"cache_write_tokens": stp.cw,
								"resolved_model_id":  "us.anthropic.claude-opus-5-5",
							},
						})
					}
					writeEvent(map[string]any{"type": "message_complete", "payload": map[string]any{"continuation_expected": false}})
					writeEvent(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
				}()
			}
		}
	}))
	defer ts.Close()

	dbPath := filepath.Join(t.TempDir(), "proxy_context_usage.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	acct := &store.Account{
		Email:       "ctx-tester@example.com",
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
	cfg.SessionReuse = false
	cfg.SetModelAliases(map[string]string{"claude-opus-5": "smart"})
	defer cfg.SetModelAliases(nil)

	mgr := accounts.New(st, &cfg)
	srv := NewServer(&cfg, mgr)
	h := srv.Handler()

	reqBody := func(stream bool) []byte {
		b, _ := json.Marshal(map[string]any{
			"model":      "claude-opus-5",
			"max_tokens": 1024,
			"stream":     stream,
			"messages":   []map[string]any{{"role": "user", "content": "check context"}},
		})
		return b
	}

	// 1. 同步 JSON 模式
	syncReq := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(reqBody(false)))
	syncReq.Header.Set("Content-Type", "application/json")
	syncRec := httptest.NewRecorder()
	h.ServeHTTP(syncRec, syncReq)
	if syncRec.Code != http.StatusOK {
		t.Fatalf("sync status = %d, body = %s", syncRec.Code, syncRec.Body.String())
	}

	var syncResp struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(syncRec.Body.Bytes(), &syncResp); err != nil {
		t.Fatalf("unmarshal sync response: %v", err)
	}
	if syncResp.Model != "claude-opus-5" {
		t.Fatalf("sync response model = %q, want %q", syncResp.Model, "claude-opus-5")
	}
	if syncResp.Usage.InputTokens != 3 || syncResp.Usage.CacheReadInputTokens != 212000 || syncResp.Usage.CacheCreationInputTokens != 15000 || syncResp.Usage.OutputTokens != 600 {
		t.Fatalf("unexpected sync usage: %+v", syncResp.Usage)
	}

	// 数据库日志仍应准确保留全步累加耗费与真实底层模型代号
	logPage, err := st.QueryRequestLogs(context.Background(), store.RequestLogFilter{Protocol: "anthropic"})
	if err != nil {
		t.Fatalf("QueryRequestLogs: %v", err)
	}
	if logPage.Total != 1 || logPage.Items[0].CacheReadTokens != 592000 || logPage.Items[0].ResolvedModel != "us.anthropic.claude-opus-5-5" || logPage.Items[0].UpstreamModel != "smart" {
		t.Fatalf("expected DB log to retain full accumulated cache_read=592000 and resolved model, got %+v", logPage.Items[0])
	}

	// 2. 流式 SSE 模式
	streamReq := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(reqBody(true)))
	streamReq.Header.Set("Content-Type", "application/json")
	streamRec := httptest.NewRecorder()
	h.ServeHTTP(streamRec, streamReq)
	if streamRec.Code != http.StatusOK {
		t.Fatalf("stream status = %d, body = %s", streamRec.Code, streamRec.Body.String())
	}

	var sawMessageStartModel string
	var deltaUsage struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	}
	var currentEvent string
	scanner := bufio.NewScanner(bytes.NewReader(streamRec.Body.Bytes()))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			rawJSON := strings.TrimPrefix(line, "data: ")
			switch currentEvent {
			case "message_start":
				var ev struct {
					Message struct {
						Model string `json:"model"`
					} `json:"message"`
				}
				if err := json.Unmarshal([]byte(rawJSON), &ev); err == nil {
					sawMessageStartModel = ev.Message.Model
				}
			case "message_delta":
				var ev struct {
					Usage struct {
						InputTokens              int `json:"input_tokens"`
						OutputTokens             int `json:"output_tokens"`
						CacheReadInputTokens     int `json:"cache_read_input_tokens"`
						CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					} `json:"usage"`
				}
				if err := json.Unmarshal([]byte(rawJSON), &ev); err == nil {
					deltaUsage = ev.Usage
				}
			}
		}
	}

	if sawMessageStartModel != "claude-opus-5" {
		t.Fatalf("SSE message_start model = %q, want %q", sawMessageStartModel, "claude-opus-5")
	}
	if deltaUsage.InputTokens != 3 || deltaUsage.CacheReadInputTokens != 212000 || deltaUsage.CacheCreationInputTokens != 15000 || deltaUsage.OutputTokens != 600 {
		t.Fatalf("unexpected SSE message_delta usage: %+v", deltaUsage)
	}
}

// TestAnthropicCountTokens_IgnoresImageBase64Payload 验证 count_tokens 遇到常规图片及 tool_result 内嵌 2MB base64 图片时按固定视觉权重估算，绝不把 base64 文本按字节虚算。
func TestAnthropicCountTokens_IgnoresImageBase64Payload(t *testing.T) {
	cfg := config.Default()
	srv := NewServer(&cfg, nil)
	h := srv.Handler()

	bigBase64 := strings.Repeat("A", 2*1024*1024)
	payload, err := json.Marshal(map[string]any{
		"model":  "claude-opus-5",
		"system": "你是代码助手。",
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": "请检查这两张截图中的布局问题。"},
					{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": "image/png",
							"data":       bigBase64,
						},
					},
				},
			},
			{
				"role": "user",
				"content": []map[string]any{
					{
						"type":        "tool_result",
						"tool_use_id": "call_1",
						"content": []map[string]any{
							{"type": "text", "text": "工具返回截图如下："},
							{
								"type": "image",
								"source": map[string]any{
									"type":       "base64",
									"media_type": "image/png",
									"data":       bigBase64,
								},
							},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("count_tokens status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.InputTokens < 2000 || resp.InputTokens > 5000 {
		t.Fatalf("count_tokens returned unreasonable input_tokens=%d (expected 2000..5000, not base64 payload inflation)", resp.InputTokens)
	}
}
