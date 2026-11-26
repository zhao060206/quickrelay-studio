package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"quickrelay-studio/internal/config"
)

func TestExplicitThreadHistoryAndQuotaRecovery(t *testing.T) {
	for _, mode := range []string{config.ToolModeAuto, config.ToolModeTextBridge} {
		t.Run(mode, func(t *testing.T) {
			mock := newMockOutpostServer(t)
			var quota atomic.Bool
			dispatched := make(chan string, 8)
			mock.onWSInbound = func(_ *websocket.Conn, send func(any) error, event map[string]any, _ int32) {
				if event["type"] != "user_message" {
					return
				}
				dispatched <- event["payload"].(map[string]any)["thread_id"].(string)
				if quota.Load() {
					_ = send(map[string]any{"type": "error", "payload": map[string]any{"code": "QuotaExceeded", "message": "test quota exhausted"}})
					return
				}
				_ = send(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "answer"}})
				_ = send(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "answer"}})
				_ = send(map[string]any{"type": "turn_completed"})
			}

			server, httpServer := setupOutpostTestServer(t, mock, mode)
			server.cfg.MaxAttempts = 2
			client := &http.Client{Timeout: 5 * time.Second}
			headers := map[string]string{"X-Conversation-Id": "review-thread"}
			call := func(messages []Message) (int, string) {
				status, body := doProxyJSONCall(t, client, httpServer.URL+"/v1/chat/completions", headers, map[string]any{
					"model": "smart", "tools": testDummyTools(), "messages": messages,
				})
				return status, string(body)
			}
			assertDispatch := func(expected string) string {
				t.Helper()
				select {
				case actual := <-dispatched:
					if actual == "" || (expected != "" && actual != expected) {
						t.Fatalf("dispatch thread = %q, expected %q", actual, expected)
					}
					return actual
				default:
					t.Fatal("request did not dispatch an upstream turn")
					return ""
				}
			}
			seed := Message{Role: "user", Content: "seed"}
			if status, body := call([]Message{seed}); status != http.StatusOK {
				t.Fatalf("seed status=%d body=%s", status, body)
			}
			threadID := assertDispatch("")
			headers["X-Thread-Id"] = threadID
			valid := []Message{seed, {Role: "assistant", Content: "answer"}, {Role: "user", Content: "follow up"}}
			wrong := append([]Message(nil), valid...)
			wrong[1].Content = "an unrelated assistant response"
			if status, body := call(wrong); status != http.StatusConflict || len(dispatched) != 0 {
				t.Fatalf("mismatched history status=%d dispatched=%d body=%s", status, len(dispatched), body)
			}

			quota.Store(true)
			if status, body := call(valid); status != http.StatusBadGateway || !strings.Contains(body, "test quota exhausted") {
				t.Fatalf("quota error changed status=%d body=%s", status, body)
			}
			assertDispatch(threadID)
			if record, err := server.store.GetConversationByThreadID(context.Background(), threadID); err != nil || record == nil {
				t.Fatalf("persisted thread binding lost: %v", err)
			}

			quota.Store(false)
			if status, body := call(valid); status != http.StatusOK {
				t.Fatalf("same-thread recovery status=%d body=%s", status, body)
			}
			assertDispatch(threadID)
			if status, body := call([]Message{{Role: "user", Content: "current message only"}}); status != http.StatusOK {
				t.Fatalf("single-message continuation status=%d body=%s", status, body)
			}
			assertDispatch(threadID)
		})
	}
}

func TestToolChoiceAcrossTextBridgeTransports(t *testing.T) {
	for _, mode := range []string{config.ToolModeAuto, config.ToolModeTextBridge} {
		t.Run(mode, func(t *testing.T) {
			mock := newMockOutpostServer(t)
			mock.putFailStatus = http.StatusNotFound
			var sendTool, omitEndTag atomic.Bool
			mock.onWSInbound = func(_ *websocket.Conn, send func(any) error, event map[string]any, _ int32) {
				if event["type"] != "user_message" {
					return
				}
				text := "Only plain text was returned."
				if sendTool.Load() {
					text = `<client_tool_call>{"name":"Lookup","arguments":{}}`
					if !omitEndTag.Load() {
						text += `</client_tool_call>`
					}
				}
				_ = send(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": text}})
				_ = send(map[string]any{"type": "message_complete", "payload": map[string]any{"text": text}})
				_ = send(map[string]any{"type": "turn_completed"})
			}
			_, httpServer := setupOutpostTestServer(t, mock, mode)
			client := &http.Client{Timeout: 5 * time.Second}
			for _, protocol := range []struct {
				path            string
				tools           any
				required, named any
				plainStop       string
				toolStop        string
				stopEvent       string
			}{
				{
					path: "/v1/chat/completions", plainStop: `"finish_reason":"stop"`, toolStop: `"finish_reason":"tool_calls"`, stopEvent: "data: [DONE]",
					tools:    []Tool{{Type: "function", Function: ToolFunction{Name: "Lookup", Parameters: map[string]any{"type": "object"}}}},
					required: "required", named: map[string]any{"type": "function", "function": map[string]any{"name": "Lookup"}},
				},
				{
					path: "/v1/messages", plainStop: `"stop_reason":"end_turn"`, toolStop: `"stop_reason":"tool_use"`, stopEvent: "event: message_stop",
					tools:    []map[string]any{{"name": "Lookup", "input_schema": map[string]any{"type": "object"}}},
					required: map[string]any{"type": "any"}, named: map[string]any{"type": "tool", "name": "Lookup"},
				},
			} {
				t.Run(protocol.path, func(t *testing.T) {
					for _, stream := range []bool{false, true} {
						request := map[string]any{
							"model": "smart", "max_tokens": 128, "stream": stream, "tools": protocol.tools,
							"messages": []Message{{Role: "user", Content: "look up the sample"}},
						}
						call := func(choice any) (int, string) {
							t.Helper()
							if choice == nil {
								delete(request, "tool_choice")
							} else {
								request["tool_choice"] = choice
							}
							status, body := doProxyJSONCall(t, client, httpServer.URL+protocol.path, nil, request)
							return status, string(body)
						}
						sendTool.Store(false)
						for _, choice := range []any{protocol.required, protocol.named} {
							status, body := call(choice)
							if (!stream && status != http.StatusBadGateway) || !strings.Contains(body, `"error"`) ||
								strings.Contains(body, protocol.plainStop) || strings.Contains(body, protocol.stopEvent) {
								t.Fatalf("stream=%v choice=%v: status=%d, want tool-choice error without success marker: %s", stream, choice, status, body)
							}
						}
						if status, body := call(nil); status != http.StatusOK || !strings.Contains(body, protocol.plainStop) {
							t.Fatalf("optional tool choice status=%d body=%s", status, body)
						}
						sendTool.Store(true)
						omitEndTag.Store(stream)
						if status, body := call(protocol.named); status != http.StatusOK || !strings.Contains(body, protocol.toolStop) || !strings.Contains(body, `"name":"Lookup"`) {
							t.Fatalf("valid tool choice stream=%v status=%d body=%s", stream, status, body)
						}
					}
				})
			}
		})
	}
}

func TestExplicitThreadSuppressesToolsAfterObservedDisconnect(t *testing.T) {
	for _, closeKind := range []string{"read_error", "channel_closed", "reader_done"} {
		t.Run(closeKind, func(t *testing.T) {
			requests := &ChatRequest{Tools: testDummyTools()}
			cfg := config.Default()
			turn := &preparedTurn{server: &Server{cfg: &cfg}, convKeyStr: "test", explicitThreadID: "thread", parallelTools: true}
			session := &outpostSession{tools: newOutpostToolRegistry(requests), eventsCh: make(chan outpostInboundPacket, 2), readDone: make(chan struct{})}
			call, err := json.Marshal(map[string]any{"type": "tool_call", "payload": map[string]any{
				"tool_call_id": "example-id", "tool_name": outpostToolAlias("local_read_file"), "arguments": map[string]any{},
			}})
			if err != nil {
				t.Fatal(err)
			}
			session.eventsCh <- outpostInboundPacket{Events: []string{string(call)}}
			switch closeKind {
			case "read_error":
				session.eventsCh <- outpostInboundPacket{Err: &websocket.CloseError{Code: websocket.CloseNormalClosure}}
			case "channel_closed":
				close(session.eventsCh)
			case "reader_done":
				close(session.readDone)
				session.eventsCh <- outpostInboundPacket{Events: []string{`{"type":"message_complete"}`}}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			emitted := 0
			result, sawTools, canPark, err := session.readTurnEvents(ctx, turn, func(event StreamEvent) error {
				if event.Type == "tool_call" {
					emitted++
				}
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "closed before it could accept tool results") || emitted != 0 || sawTools || canPark || len(result.ToolCalls) != 0 {
				t.Fatalf("expected disconnect error before tools: err=%v emitted=%d saw=%t park=%t resultCalls=%d", err, emitted, sawTools, canPark, len(result.ToolCalls))
			}
		})
	}
}
