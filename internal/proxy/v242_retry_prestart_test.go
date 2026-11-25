package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
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

type capturedUpstreamTurn struct {
	HasSessionCreate bool
	SessionCreateID  string
	ThreadID         string
	Text             string
}

// decodeClientFrame 解码测试收到的 AIDP/QuickWork 客户端 JSON 帧。
func decodeClientFrame(raw []byte) (msgType string, payload map[string]any, ok bool) {
	var outer struct {
		Payload struct {
			Payload string `json:"payload"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &outer) != nil || outer.Payload.Payload == "" {
		return "", nil, false
	}
	innerBytes, err := base64.StdEncoding.DecodeString(outer.Payload.Payload)
	if err != nil {
		return "", nil, false
	}
	var inner struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	if json.Unmarshal(innerBytes, &inner) != nil {
		return "", nil, false
	}
	return inner.Type, inner.Payload, true
}

// startControlledMockUpstream 创建一个可按连接序号自定义握手行为与返回内容的测试 WebSocket 上游，并记录每次请求负载。
func startControlledMockUpstream(
	t *testing.T,
	handler func(attempt int32, w http.ResponseWriter, r *http.Request) (upgrade bool, replyText string, errEvent map[string]any),
) (*httptest.Server, *atomic.Int32, *[]capturedUpstreamTurn, *sync.Mutex) {
	t.Helper()
	var dialCount atomic.Int32
	var mu sync.Mutex
	var captured []capturedUpstreamTurn
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := dialCount.Add(1)
		upgrade, replyText, errEvent := handler(n, w, r)
		if !upgrade {
			return
		}
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

		var turnRecord capturedUpstreamTurn
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			msgType, payload, ok := decodeClientFrame(raw)
			if !ok {
				continue
			}
			switch msgType {
			case "session_create":
				turnRecord.HasSessionCreate = true
				if sid, _ := payload["session_id"].(string); sid != "" {
					turnRecord.SessionCreateID = sid
				}
			case "user_message":
				turnRecord.ThreadID, _ = payload["thread_id"].(string)
				turnRecord.Text, _ = payload["text"].(string)
				mu.Lock()
				captured = append(captured, turnRecord)
				mu.Unlock()

				if errEvent != nil {
					writeEv(errEvent)
					return
				}
				writeEv(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m_mock"}})
				writeEv(map[string]any{"type": "thinking_delta", "payload": map[string]any{"delta": "思考完毕。"}})
				writeEv(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": replyText}})
				writeEv(map[string]any{"type": "turn_usage", "payload": map[string]any{"input_tokens": 100, "output_tokens": 20, "total_tokens": 120}})
				writeEv(map[string]any{"type": "message_complete", "payload": map[string]any{"continuation_expected": false}})
				writeEv(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &dialCount, &captured, &mu
}

func setupTwoAccountProxy(t *testing.T, wsA, wsB *httptest.Server, maxAttempts int) (*httptest.Server, *http.Client, *Server) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "v242_retry.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	saveAcct := func(email string, wsSrv *httptest.Server) {
		a := &store.Account{
			Email:       email,
			Provider:    "social",
			WSHost:      "ws://" + strings.TrimPrefix(wsSrv.URL, "http://"),
			WSPath:      "/ws",
			AccessToken: "jwt-" + email,
			ActorARN:    "arn:aws:quick:us-east-1:" + email,
			ExpiresAt:   time.Now().Add(time.Hour).Unix(),
		}
		if err := st.Save(context.Background(), a); err != nil {
			t.Fatalf("st.Save(%s): %v", email, err)
		}
	}
	saveAcct("acct-a@example.com", wsA)
	saveAcct("acct-b@example.com", wsB)

	cfg := config.Default()
	cfg.SessionReuse = true
	cfg.MaxAttempts = maxAttempts
	mgr := accounts.New(st, &cfg)
	srv := NewServer(&cfg, mgr)

	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	return httpSrv, client, srv
}

// TestStreamDialTimeout_DoesNotPrestartAndRetries 验证即使 SSE 首包预热阈值小于 WebSocket 握手超时，
// 拨号超时期间也不会过早发出 SSE 响应头，从而允许自动重试第 2 次并完整流式返回结果。
func TestStreamDialTimeout_DoesNotPrestartAndRetries(t *testing.T) {
	prevDialTimeout := wsDialHandshakeTimeout
	prevPrestart := maxThinkingPrestart
	prevInterval := sseKeepaliveInterval
	wsDialHandshakeTimeout = 90 * time.Millisecond
	maxThinkingPrestart = 45 * time.Millisecond
	sseKeepaliveInterval = 35 * time.Millisecond
	defer func() {
		wsDialHandshakeTimeout = prevDialTimeout
		maxThinkingPrestart = prevPrestart
		sseKeepaliveInterval = prevInterval
	}()

	runCase := func(name, path string, payload map[string]any, wantDoneMarker, wantText string) {
		t.Run(name, func(t *testing.T) {
			wsSrv, dialCount, _, _ := startControlledMockUpstream(t, func(attempt int32, w http.ResponseWriter, r *http.Request) (bool, string, map[string]any) {
				if attempt == 1 {
					// 模拟代理建连握手卡死，触发真实 wsDialHandshakeTimeout (90ms)，且长于 maxThinkingPrestart (45ms)
					select {
					case <-time.After(250 * time.Millisecond):
					case <-r.Context().Done():
					}
					return false, "", nil
				}
				return true, wantText, nil
			})

			httpSrv, client := setupMockProxyHTTPServer(t, wsSrv)
			body, _ := json.Marshal(payload)
			resp, err := client.Post(httpSrv.URL+path, "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("POST %s failed: %v", path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				rawErr, _ := io.ReadAll(resp.Body)
				t.Fatalf("unexpected HTTP %d: %s", resp.StatusCode, string(rawErr))
			}

			var lines []string
			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				lines = append(lines, scanner.Text())
			}
			if err := scanner.Err(); err != nil {
				t.Fatalf("stream read err: %v", err)
			}
			joined := strings.Join(lines, "\n")
			if strings.Contains(joined, `"type":"error"`) || strings.Contains(joined, `"upstream_error"`) {
				t.Fatalf("unexpected error event in retried stream:\n%s", joined)
			}
			if !strings.Contains(joined, wantText) || !strings.Contains(joined, wantDoneMarker) {
				t.Fatalf("stream missing expected reply or end marker:\n%s", joined)
			}
			if got := dialCount.Load(); got != 2 {
				t.Fatalf("dialCount = %d, want 2 (must retry after dial timeout)", got)
			}
		})
	}

	runCase("anthropic_high", "/v1/messages", map[string]any{
		"model":         "claude-opus-5",
		"max_tokens":    512,
		"stream":        true,
		"output_config": map[string]any{"effort": "high"},
		"messages":      []map[string]any{{"role": "user", "content": "测试握手超时自动重试"}},
	}, "event: message_stop", "Anthropic拨号重试成功")

	runCase("openai_high", "/v1/chat/completions", map[string]any{
		"model":            "smart",
		"stream":           true,
		"reasoning_effort": "high",
		"messages":         []map[string]any{{"role": "user", "content": "测试握手超时自动重试"}},
	}, "data: [DONE]", "OpenAI拨号重试成功")
}

// TestRetryTemporary_SameAccountRetryPreservesCache 验证续聊遇临时网络超时时，
// 会优先在原绑定账号原地重连 1 次，复用原 thread_id 且仅发增量，完美保留既有缓存。
func TestRetryTemporary_SameAccountRetryPreservesCache(t *testing.T) {
	prevDialTimeout := wsDialHandshakeTimeout
	wsDialHandshakeTimeout = 80 * time.Millisecond
	defer func() { wsDialHandshakeTimeout = prevDialTimeout }()

	var failNextA atomic.Bool
	wsA, dialsA, capA, muA := startControlledMockUpstream(t, func(attempt int32, w http.ResponseWriter, r *http.Request) (bool, string, map[string]any) {
		if failNextA.CompareAndSwap(true, false) {
			select {
			case <-time.After(220 * time.Millisecond):
			case <-r.Context().Done():
			}
			return false, "", nil
		}
		return true, "账号A成功回复", nil
	})
	var failNextB atomic.Bool
	wsB, dialsB, capB, muB := startControlledMockUpstream(t, func(attempt int32, w http.ResponseWriter, r *http.Request) (bool, string, map[string]any) {
		if failNextB.CompareAndSwap(true, false) {
			select {
			case <-time.After(220 * time.Millisecond):
			case <-r.Context().Done():
			}
			return false, "", nil
		}
		return true, "账号B成功回复", nil
	})

	httpSrv, client, _ := setupTwoAccountProxy(t, wsA, wsB, 3)

	postAnthropic := func(msgs []map[string]any) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{
			"model":      "claude-opus-5",
			"max_tokens": 512,
			"stream":     false,
			"messages":   msgs,
		})
		req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/messages", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Conversation-Id", "conv-same-acct-keep")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /v1/messages failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("status=%d body=%s", resp.StatusCode, string(raw))
		}
	}

	// 第 1 轮建立会话，定位被选中的绑定账号
	postAnthropic([]map[string]any{{"role": "user", "content": "第一轮独特记忆锚点-ALPHA"}})
	ownerIsA := dialsA.Load() == 1
	var ownerDials *atomic.Int32
	var otherDials *atomic.Int32
	var ownerCap *[]capturedUpstreamTurn
	var ownerMu *sync.Mutex
	var failNext *atomic.Bool
	if ownerIsA {
		ownerDials, otherDials, ownerCap, ownerMu, failNext = dialsA, dialsB, capA, muA, &failNextA
	} else {
		ownerDials, otherDials, ownerCap, ownerMu, failNext = dialsB, dialsA, capB, muB, &failNextB
	}
	ownerMu.Lock()
	establishedTID := (*ownerCap)[0].ThreadID
	ownerMu.Unlock()
	if establishedTID == "" {
		t.Fatalf("expected non-empty thread_id from round 1")
	}

	// 第 2 轮续聊：故意让原绑定账号第 1 次尝试超时，第 2 次恢复正常
	failNext.Store(true)
	postAnthropic([]map[string]any{
		{"role": "user", "content": "第一轮独特记忆锚点-ALPHA"},
		{"role": "assistant", "content": "第一轮回答"},
		{"role": "user", "content": "第二轮增量问题-BETA"},
	})

	if got := otherDials.Load(); got != 0 {
		t.Fatalf("expected backup account dials = 0 when owner recovers on 2nd try, got %d", got)
	}
	if got := ownerDials.Load(); got != 3 {
		t.Fatalf("expected owner dials = 3 (1 round1 + 1 failed + 1 retried), got %d", got)
	}
	ownerMu.Lock()
	defer ownerMu.Unlock()
	if len(*ownerCap) != 2 {
		t.Fatalf("expected 2 dispatched payloads on owner (1 fail was dial timeout before send), got %d", len(*ownerCap))
	}
	round2 := (*ownerCap)[1]
	if round2.HasSessionCreate {
		t.Fatalf("expected same-account temporary retry NOT to send session_create")
	}
	if round2.ThreadID != establishedTID {
		t.Fatalf("expected reused thread_id %q, got %q", establishedTID, round2.ThreadID)
	}
	if strings.Contains(round2.Text, "第一轮独特记忆锚点-ALPHA") || !strings.Contains(round2.Text, "第二轮增量问题-BETA") {
		t.Fatalf("expected incremental prompt on same-account retry, got: %s", round2.Text)
	}
}

// TestRetryTemporary_FailTwiceSwitchesAccountWithFullTranscript 验证续聊时若原绑定账号连续两次发生临时故障，
// 第 3 次切换至另一账号时必须新建 thread_id 并完整携带前文历史与完整工具表。
func TestRetryTemporary_FailTwiceSwitchesAccountWithFullTranscript(t *testing.T) {
	prevDialTimeout := wsDialHandshakeTimeout
	wsDialHandshakeTimeout = 80 * time.Millisecond
	defer func() { wsDialHandshakeTimeout = prevDialTimeout }()

	var alwaysTimeoutA, alwaysTimeoutB atomic.Bool
	wsA, dialsA, capA, muA := startControlledMockUpstream(t, func(attempt int32, w http.ResponseWriter, r *http.Request) (bool, string, map[string]any) {
		if alwaysTimeoutA.Load() {
			select {
			case <-time.After(220 * time.Millisecond):
			case <-r.Context().Done():
			}
			return false, "", nil
		}
		return true, "账号A正常回复", nil
	})
	wsB, dialsB, capB, muB := startControlledMockUpstream(t, func(attempt int32, w http.ResponseWriter, r *http.Request) (bool, string, map[string]any) {
		if alwaysTimeoutB.Load() {
			select {
			case <-time.After(220 * time.Millisecond):
			case <-r.Context().Done():
			}
			return false, "", nil
		}
		return true, "账号B兜底重建成功", nil
	})

	httpSrv, client, _ := setupTwoAccountProxy(t, wsA, wsB, 3)

	sendTurn := func(msgs []map[string]any) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{
			"model":      "claude-opus-5",
			"max_tokens": 512,
			"stream":     false,
			"tools": []map[string]any{{
				"name":         "UniqueSpecialToolXYZ",
				"description":  "测试工具表是否完整重发",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{"target": map[string]any{"type": "string"}}},
			}},
			"messages": msgs,
		})
		req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/messages", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Conversation-Id", "conv-switch-full")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /v1/messages failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("status=%d body=%s", resp.StatusCode, string(raw))
		}
	}

	// 第 1 轮成功建立会话
	sendTurn([]map[string]any{{"role": "user", "content": "历史关键上下文-ANCHOR-9988"}})
	ownerIsA := dialsA.Load() == 1
	var ownerCap, otherCap *[]capturedUpstreamTurn
	var ownerMu, otherMu *sync.Mutex
	var ownerDials, otherDials *atomic.Int32
	if ownerIsA {
		ownerCap, ownerMu, ownerDials = capA, muA, dialsA
		otherCap, otherMu, otherDials = capB, muB, dialsB
		alwaysTimeoutA.Store(true)
	} else {
		ownerCap, ownerMu, ownerDials = capB, muB, dialsB
		otherCap, otherMu, otherDials = capA, muA, dialsA
		alwaysTimeoutB.Store(true)
	}
	ownerMu.Lock()
	oldTID := (*ownerCap)[0].ThreadID
	ownerMu.Unlock()

	// 第 2 轮：原账号连续失败 2 次后，第 3 次切到备份账号
	sendTurn([]map[string]any{
		{"role": "user", "content": "历史关键上下文-ANCHOR-9988"},
		{"role": "assistant", "content": "第一轮回答"},
		{"role": "user", "content": "第二轮最新指令-NEW-7766"},
	})

	if got := ownerDials.Load(); got != 3 {
		// 第1轮1次 + 第2轮先尝试1次 + 原地重连1次 = 3次
		t.Fatalf("expected owner dials = 3 (1 round1 + 2 round2 attempts), got %d", got)
	}
	if got := otherDials.Load(); got != 1 {
		t.Fatalf("expected backup account dials = 1, got %d", got)
	}
	otherMu.Lock()
	defer otherMu.Unlock()
	if len(*otherCap) != 1 {
		t.Fatalf("expected 1 dispatched payload on backup account, got %d", len(*otherCap))
	}
	switched := (*otherCap)[0]
	if !switched.HasSessionCreate {
		t.Fatalf("expected session_create=true when switching account")
	}
	if switched.ThreadID == "" || switched.ThreadID == oldTID {
		t.Fatalf("expected brand-new thread_id != %q when switching account, got %q", oldTID, switched.ThreadID)
	}
	for _, want := range []string{"历史关键上下文-ANCHOR-9988", "第二轮最新指令-NEW-7766", "UniqueSpecialToolXYZ"} {
		if !strings.Contains(switched.Text, want) {
			t.Fatalf("switched account prompt missing %q:\n%s", want, switched.Text)
		}
	}
}

// TestRetryTemporary_RateLimitSwitchesImmediatelyAndNetworkOutagePreservesConv 验证：
// 1) 命中 429 限流时跳过原地重连、直接切往其他账号并用完整历史重建；
// 2) 全账号均因网络超时失败后不破坏既有持久化绑定，网络恢复后仍命中原 thread_id 复用。
func TestRetryTemporary_RateLimitSwitchesImmediatelyAndNetworkOutagePreservesConv(t *testing.T) {
	prevDialTimeout := wsDialHandshakeTimeout
	wsDialHandshakeTimeout = 80 * time.Millisecond
	defer func() { wsDialHandshakeTimeout = prevDialTimeout }()

	t.Run("rate_limit_429_switches_immediately", func(t *testing.T) {
		var rateLimitA, rateLimitB atomic.Bool
		wsA, dialsA, capA, muA := startControlledMockUpstream(t, func(attempt int32, w http.ResponseWriter, r *http.Request) (bool, string, map[string]any) {
			if rateLimitA.Load() {
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return false, "", nil
			}
			return true, "ok-A", nil
		})
		wsB, dialsB, capB, muB := startControlledMockUpstream(t, func(attempt int32, w http.ResponseWriter, r *http.Request) (bool, string, map[string]any) {
			if rateLimitB.Load() {
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return false, "", nil
			}
			return true, "ok-B", nil
		})
		httpSrv, client, _ := setupTwoAccountProxy(t, wsA, wsB, 3)

		postTurn := func(msgs []map[string]any) {
			b, _ := json.Marshal(map[string]any{
				"model":      "claude-opus-5",
				"max_tokens": 256,
				"stream":     false,
				"messages":   msgs,
			})
			req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/messages", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Conversation-Id", "conv-429-switch")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("POST failed: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status=%d body=%s", resp.StatusCode, string(raw))
			}
		}

		postTurn([]map[string]any{{"role": "user", "content": "首轮历史-RL-111"}})
		ownerIsA := dialsA.Load() == 1
		var ownerDials, otherDials *atomic.Int32
		var otherCap *[]capturedUpstreamTurn
		var otherMu *sync.Mutex
		if ownerIsA {
			ownerDials, otherDials, otherCap, otherMu = dialsA, dialsB, capB, muB
			rateLimitA.Store(true)
		} else {
			ownerDials, otherDials, otherCap, otherMu = dialsB, dialsA, capA, muA
			rateLimitB.Store(true)
		}

		postTurn([]map[string]any{
			{"role": "user", "content": "首轮历史-RL-111"},
			{"role": "assistant", "content": "第一轮答复"},
			{"role": "user", "content": "续聊-RL-222"},
		})

		// 命中 429 应不原地重连，原账号在第 2 轮仅试 1 次（总计 2 次），立刻切到备号并携带全量历史
		if got := ownerDials.Load(); got != 2 {
			t.Fatalf("ownerDials = %d, want 2 (no same-account retry on 429)", got)
		}
		if got := otherDials.Load(); got != 1 {
			t.Fatalf("otherDials = %d, want 1", got)
		}
		otherMu.Lock()
		defer otherMu.Unlock()
		if len(*otherCap) != 1 || !(*otherCap)[0].HasSessionCreate || !strings.Contains((*otherCap)[0].Text, "首轮历史-RL-111") {
			t.Fatalf("expected full transcript on 429 fallback, got %#v", *otherCap)
		}
	})

	t.Run("all_accounts_timeout_preserves_persistent_session", func(t *testing.T) {
		var timeoutAll atomic.Bool
		wsA, dialsA, capA, muA := startControlledMockUpstream(t, func(attempt int32, w http.ResponseWriter, r *http.Request) (bool, string, map[string]any) {
			if timeoutAll.Load() {
				select {
				case <-time.After(220 * time.Millisecond):
				case <-r.Context().Done():
				}
				return false, "", nil
			}
			return true, "ok-A", nil
		})
		wsB, dialsB, capB, muB := startControlledMockUpstream(t, func(attempt int32, w http.ResponseWriter, r *http.Request) (bool, string, map[string]any) {
			if timeoutAll.Load() {
				select {
				case <-time.After(220 * time.Millisecond):
				case <-r.Context().Done():
				}
				return false, "", nil
			}
			return true, "ok-B", nil
		})
		httpSrv, client, _ := setupTwoAccountProxy(t, wsA, wsB, 3)

		postRaw := func(msgs []map[string]any) int {
			b, _ := json.Marshal(map[string]any{
				"model":      "claude-opus-5",
				"max_tokens": 256,
				"stream":     false,
				"messages":   msgs,
			})
			req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/messages", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Conversation-Id", "conv-outage-preserve")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("POST failed: %v", err)
			}
			defer resp.Body.Close()
			return resp.StatusCode
		}

		// 第 1 轮正常建立
		if code := postRaw([]map[string]any{{"role": "user", "content": "持久锚点-OUTAGE-1"}}); code != http.StatusOK {
			t.Fatalf("round 1 status = %d, want 200", code)
		}
		ownerIsA := dialsA.Load() == 1
		var ownerCap *[]capturedUpstreamTurn
		var ownerMu *sync.Mutex
		var otherDials *atomic.Int32
		if ownerIsA {
			ownerCap, ownerMu, otherDials = capA, muA, dialsB
		} else {
			ownerCap, ownerMu, otherDials = capB, muB, dialsA
		}
		ownerMu.Lock()
		savedTID := (*ownerCap)[0].ThreadID
		ownerMu.Unlock()

		// 断网模拟：全账号拨号超时失败
		timeoutAll.Store(true)
		msgs2 := []map[string]any{
			{"role": "user", "content": "持久锚点-OUTAGE-1"},
			{"role": "assistant", "content": "第一轮答复"},
			{"role": "user", "content": "第二轮重发-OUTAGE-2"},
		}
		if code := postRaw(msgs2); code == http.StatusOK {
			t.Fatalf("expected failure during full network outage")
		}

		// 网络恢复：重发第 2 轮应依然直奔原绑定账号并复用 savedTID
		timeoutAll.Store(false)
		otherBefore := otherDials.Load()
		if code := postRaw(msgs2); code != http.StatusOK {
			t.Fatalf("recovery round 2 status = %d, want 200", code)
		}
		if otherDials.Load() != otherBefore {
			t.Fatalf("expected recovery round to stay pinned on original owner account")
		}
		ownerMu.Lock()
		defer ownerMu.Unlock()
		last := (*ownerCap)[len(*ownerCap)-1]
		if last.HasSessionCreate || last.ThreadID != savedTID {
			t.Fatalf("expected preserved session tid=%q without session_create after outage, got create=%t tid=%q", savedTID, last.HasSessionCreate, last.ThreadID)
		}
	})
}
