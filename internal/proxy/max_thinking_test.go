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

// TestTurnTimeoutFor_MaxThinkingExtendsTimeout 验证 thinking_effort=max 时会自动拉长单轮总超时，且不会缩短原本更高的 request_timeout。
func TestTurnTimeoutFor_MaxThinkingExtendsTimeout(t *testing.T) {
	cfg := config.Default()
	cfg.RequestTimeout = config.Duration(15 * time.Minute)
	cfg.MaxThinkingTimeout = config.Duration(30 * time.Minute)
	s := &Server{cfg: &cfg}

	if got := s.turnTimeoutFor("high"); got != 15*time.Minute {
		t.Fatalf("high effort timeout = %v, want 15m", got)
	}
	if got := s.turnTimeoutFor("max"); got != 30*time.Minute {
		t.Fatalf("max effort timeout = %v, want 30m", got)
	}
	if got := s.turnTimeoutFor(""); got != 15*time.Minute {
		t.Fatalf("default effort timeout = %v, want 15m", got)
	}

	// 若用户已把 request_timeout 调成更大的 45m，max 不应反向缩小为 30m
	cfg.RequestTimeout = config.Duration(45 * time.Minute)
	if got := s.turnTimeoutFor("max"); got != 45*time.Minute {
		t.Fatalf("max effort should preserve larger request_timeout=45m, got %v", got)
	}
}

func startDelayedMockUpstream(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
					time.Sleep(delay)
					writeEvent(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m_delayed"}})
					writeEvent(map[string]any{"type": "thinking_delta", "payload": map[string]any{"delta": "深度推理已完成。"}})
					writeEvent(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "长思考后返回结果。"}})
					writeEvent(map[string]any{"type": "turn_usage", "payload": map[string]any{"input_tokens": 50, "output_tokens": 20, "total_tokens": 70}})
					writeEvent(map[string]any{"type": "message_complete", "payload": map[string]any{"continuation_expected": false}})
					writeEvent(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
				}()
			}
		}
	}))
}

func setupMockProxyHTTPServer(t *testing.T, wsServer *httptest.Server) (*httptest.Server, *http.Client) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "max_think.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	acct := &store.Account{
		Email:       "maxthink@example.com",
		Provider:    "social",
		WSHost:      "ws://" + strings.TrimPrefix(wsServer.URL, "http://"),
		WSPath:      "/ws",
		AccessToken: "mock-jwt",
		ActorARN:    "arn:aws:quick:us-east-1:test",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	}
	if err := st.Save(context.Background(), acct); err != nil {
		t.Fatalf("st.Save: %v", err)
	}

	cfg := config.Default()
	cfg.SessionReuse = false
	mgr := accounts.New(st, &cfg)
	srv := NewServer(&cfg, mgr)

	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	return httpSrv, client
}

// TestAnthropicStream_MaxThinkingPrestartAndPing 验证 max 思考长静默等待期间，/v1/messages 会提前发送 message_start 并定期输出 Anthropic event: ping 保活帧。
func TestAnthropicStream_MaxThinkingPrestartAndPing(t *testing.T) {
	prevInterval := sseKeepaliveInterval
	prevPrestart := maxThinkingPrestart
	sseKeepaliveInterval = 35 * time.Millisecond
	maxThinkingPrestart = 45 * time.Millisecond
	defer func() {
		sseKeepaliveInterval = prevInterval
		maxThinkingPrestart = prevPrestart
	}()

	wsSrv := startDelayedMockUpstream(t, 190*time.Millisecond)
	defer wsSrv.Close()
	httpSrv, client := setupMockProxyHTTPServer(t, wsSrv)

	body, _ := json.Marshal(map[string]any{
		"model":         "claude-sonnet-4-5",
		"max_tokens":    512,
		"stream":        true,
		"output_config": map[string]any{"effort": "max"},
		"messages":      []map[string]any{{"role": "user", "content": "复杂架构分析"}},
	})
	resp, err := client.Post(httpSrv.URL+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/messages failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}

	var lines []string
	pingCount := 0
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		lines = append(lines, line)
		if line == "event: ping" {
			pingCount++
		}
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			var js map[string]any
			if err := json.Unmarshal([]byte(payload), &js); err != nil {
				t.Fatalf("corrupted JSON line in concurrent SSE stream: %v (%s)", err, line)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("stream read error: %v", err)
	}

	joined := strings.Join(lines, "\n")
	if pingCount < 1 {
		t.Fatalf("expected at least 1 event: ping keepalive during max thinking delay, got %d\nstream:\n%s", pingCount, joined)
	}
	for _, want := range []string{"event: message_start", `"type":"thinking_delta"`, "长思考后返回结果。", "event: message_stop"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Anthropic stream missing %q\nstream:\n%s", want, joined)
		}
	}
}

// TestOpenAIStream_MaxThinkingKeepaliveComment 验证 xhigh/max 模式下 /v1/chat/completions 长静默等待期间会输出 : keepalive 注释保活且不破坏 JSON 流。
func TestOpenAIStream_MaxThinkingKeepaliveComment(t *testing.T) {
	prevInterval := sseKeepaliveInterval
	prevPrestart := maxThinkingPrestart
	sseKeepaliveInterval = 35 * time.Millisecond
	maxThinkingPrestart = 45 * time.Millisecond
	defer func() {
		sseKeepaliveInterval = prevInterval
		maxThinkingPrestart = prevPrestart
	}()

	wsSrv := startDelayedMockUpstream(t, 190*time.Millisecond)
	defer wsSrv.Close()
	httpSrv, client := setupMockProxyHTTPServer(t, wsSrv)

	body, _ := json.Marshal(map[string]any{
		"model":            "smart",
		"stream":           true,
		"reasoning_effort": "xhigh",
		"messages":         []map[string]any{{"role": "user", "content": "复杂排查"}},
	})
	resp, err := client.Post(httpSrv.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}

	var lines []string
	keepaliveCount := 0
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		lines = append(lines, line)
		if line == ": keepalive" {
			keepaliveCount++
		}
		if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
			payload := strings.TrimPrefix(line, "data: ")
			var js map[string]any
			if err := json.Unmarshal([]byte(payload), &js); err != nil {
				t.Fatalf("corrupted OpenAI JSON chunk in concurrent SSE stream: %v (%s)", err, line)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("stream read error: %v", err)
	}

	joined := strings.Join(lines, "\n")
	if keepaliveCount < 1 {
		t.Fatalf("expected at least 1 ': keepalive' comment during xhigh/max delay, got %d\nstream:\n%s", keepaliveCount, joined)
	}
	for _, want := range []string{"深度推理已完成。", "长思考后返回结果。", "data: [DONE]"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("OpenAI stream missing %q\nstream:\n%s", want, joined)
		}
	}
}
