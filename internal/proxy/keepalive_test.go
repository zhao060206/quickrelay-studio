package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"quickrelay-studio/internal/config"
)

func encodeMockAIDPServerEvent(v any) []byte {
	inner, _ := json.Marshal(v)
	outer, _ := json.Marshal(map[string]string{
		"payload": base64.StdEncoding.EncodeToString(inner),
	})
	return eventStreamEncode(map[string]string{":message-type": "event"}, outer)
}

// TestUpstream_SendsAppLevelPingFrames 验证长思考或长耗时等待期间，上游客户端会定时发送应用层 ping 帧防止 95 秒断连。
func TestUpstream_SendsAppLevelPingFrames(t *testing.T) {
	prev := wsKeepaliveInterval
	wsKeepaliveInterval = 40 * time.Millisecond
	defer func() { wsKeepaliveInterval = prev }()

	var pingCount atomic.Int32
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
			switch inner.Type {
			case "ping":
				pingCount.Add(1)
				writeEvent(map[string]any{
					"type":    "status",
					"payload": map[string]any{"status": "pong"},
				})
			case "user_message":
				go func() {
					time.Sleep(180 * time.Millisecond)
					writeEvent(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m1"}})
					writeEvent(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "ok"}})
					writeEvent(map[string]any{"type": "message_complete", "payload": map[string]any{"continuation_expected": false}})
					writeEvent(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
				}()
			}
		}
	}))
	defer ts.Close()

	cfg := config.Default()
	u := NewUpstream(&cfg)
	sess := &Session{
		JWT:      "test-jwt",
		WSHost:   "ws://" + strings.TrimPrefix(ts.URL, "http://"),
		WSPath:   "/ws",
		Provider: "social",
		ActorARN: "arn:aws:quick:us-east-1:test",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := u.Chat(ctx, sess, ChatInput{
		Prompt:        "hello",
		Model:         "smart",
		CreateSession: true,
	}, nil)
	if err != nil {
		t.Fatalf("Upstream.Chat failed: %v", err)
	}
	if res.Text != "ok" {
		t.Fatalf("got text %q, want %q", res.Text, "ok")
	}
	if got := pingCount.Load(); got < 2 {
		t.Fatalf("expected at least 2 application-level ping frames, got %d", got)
	}
}
