package proxy

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	_ "modernc.org/sqlite"

	"quickrelay-studio/internal/accounts"
	"quickrelay-studio/internal/config"
	"quickrelay-studio/internal/store"
)

func makeFakePDF(tag string) []byte {
	return []byte("%PDF-1.4\n%" + tag + "\n1 0 obj\n<< /Type /Catalog >>\nendobj\n%%EOF\n")
}

// TestImprovement_SameOpenerMultipleWindowsNoMixup 验证第一批 2.1：
// 1) 同开场白双窗口不会错续到另一窗口线程；
// 2) 工具参数 JSON 仅键序/空白差异时仍精准命中复用，回复被修改则重建；
// 3) OpenAI 与 Anthropic 的流式与非流式均以最终对外清洗/解析后内容为准并在 Flush 后提交；
// 4) 流式报错或取消绝不提交半截记录。
func TestImprovement_SameOpenerMultipleWindowsNoMixup(t *testing.T) {
	var seq atomic.Int32
	wsSrv, _, captured, capMu := startControlledMockUpstream(t, func(attempt int32, w http.ResponseWriter, r *http.Request) (bool, string, map[string]any) {
		n := seq.Add(1)
		switch n {
		case 1:
			return true, "窗口A的专属回答", nil
		case 2:
			return true, "窗口B的专属回答", nil
		default:
			return true, fmt.Sprintf("第%d次回答", n), nil
		}
	})

	dbPath := filepath.Join(t.TempDir(), "mixup.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Save(context.Background(), &store.Account{
		Email: "mix@example.com", Provider: "social",
		WSHost: "ws://" + strings.TrimPrefix(wsSrv.URL, "http://"), WSPath: "/ws",
		AccessToken: "jwt-mix", ActorARN: "arn:aws:quick:us-east-1:mix", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("st.Save: %v", err)
	}

	cfg := config.Default()
	cfg.SessionReuse = true
	srv := NewServer(&cfg, accounts.New(st, &cfg))
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}

	callAnthropic := func(stream bool, msgs []map[string]any, tools []map[string]any) int {
		payload := map[string]any{
			"model": "claude-opus-5", "max_tokens": 512, "stream": stream, "messages": msgs,
		}
		if len(tools) > 0 {
			payload["tools"] = tools
		}
		b, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/messages", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer same-caller-key")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /v1/messages: %v", err)
		}
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
		return resp.StatusCode
	}

	// 1. 窗口 A 与窗口 B 无显式会话 ID，均以 "你好" 开场，窗口 B 覆盖相同 conv_key
	if code := callAnthropic(false, []map[string]any{{"role": "user", "content": "你好"}}, nil); code != http.StatusOK {
		t.Fatalf("window A round 1 status=%d", code)
	}
	if code := callAnthropic(false, []map[string]any{{"role": "user", "content": "你好"}}, nil); code != http.StatusOK {
		t.Fatalf("window B round 1 status=%d", code)
	}

	// 窗口 A 回传属于窗口 A 的回复继续对话 -> 必须拒绝续到窗口 B 的线程，用完整历史新建
	if code := callAnthropic(false, []map[string]any{
		{"role": "user", "content": "你好"},
		{"role": "assistant", "content": "窗口A的专属回答"},
		{"role": "user", "content": "这是窗口A的后续提问"},
	}, nil); code != http.StatusOK {
		t.Fatalf("window A round 2 status=%d", code)
	}

	capMu.Lock()
	if len(*captured) != 3 {
		capMu.Unlock()
		t.Fatalf("expected 3 captured turns, got %d", len(*captured))
	}
	tidB := (*captured)[1].ThreadID
	turnA2 := (*captured)[2]
	capMu.Unlock()

	if !turnA2.HasSessionCreate || turnA2.ThreadID == tidB {
		t.Fatalf("window A round 2 reused window B thread %q! create=%t tid=%q", tidB, turnA2.HasSessionCreate, turnA2.ThreadID)
	}
	if !strings.Contains(turnA2.Text, "窗口A的专属回答") || !strings.Contains(turnA2.Text, "这是窗口A的后续提问") {
		t.Fatalf("window A round 2 missing full transcript rebuild: %s", turnA2.Text)
	}

	// 2. 验证流式在 Flush() 清洗私有标签 + 解析工具调用后才提交最终对外输出指纹，且参数 JSON 键序变化仍命中复用
	t.Run("streaming_flush_commit_and_canonical_tool_args", func(t *testing.T) {
		var sSeq atomic.Int32
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		var sCap []capturedUpstreamTurn
		var sMu sync.Mutex

		sWS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
					sMu.Lock()
					sCap = append(sCap, rec)
					sMu.Unlock()

					idx := sSeq.Add(1)
					writeEv := func(v any) {
						_ = conn.WriteMessage(websocket.BinaryMessage, encodeMockAIDPServerEvent(v))
					}
					if idx == 1 {
						// 流末尾刻意带一个需要 QuickFilter 清洗的 <sources> 块以及 <client_tool_call>
						writeEv(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m1"}})
						writeEv(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "先看下文件。<sources>internal-src</sources>"}})
						writeEv(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": `<client_tool_call>{"id":"call_99","name":"ReadFile","arguments":{"b":2,"a":1}}</client_tool_call>`}})
						writeEv(map[string]any{"type": "message_complete", "payload": map[string]any{"continuation_expected": false}})
						writeEv(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
					} else if idx == 2 {
						// 第 2 次（OpenAI 失败测试）：输出半截文本后下发 error 事件，不得污染保存的指纹
						writeEv(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m2"}})
						writeEv(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "半截输出"}})
						writeEv(map[string]any{"type": "error", "payload": map[string]any{"code": "ValidationException", "message": "aborted"}})
						return
					} else {
						writeEv(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m3"}})
						writeEv(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "续聊成功"}})
						writeEv(map[string]any{"type": "message_complete", "payload": map[string]any{"continuation_expected": false}})
						writeEv(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
					}
				}
			}
		}))
		defer sWS.Close()

		sDBPath := filepath.Join(t.TempDir(), "stream_reuse.db")
		sST, err := store.Open(sDBPath)
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		defer sST.Close()
		_ = sST.Save(context.Background(), &store.Account{
			Email: "stream@example.com", Provider: "social",
			WSHost: "ws://" + strings.TrimPrefix(sWS.URL, "http://"), WSPath: "/ws",
			AccessToken: "jwt-stream", ActorARN: "arn:aws:quick:us-east-1:stream", ExpiresAt: time.Now().Add(time.Hour).Unix(),
		})
		sCfg := config.Default()
		sCfg.SessionReuse = true
		sSrv := NewServer(&sCfg, accounts.New(sST, &sCfg))
		hSrv := httptest.NewServer(sSrv.Handler())
		defer hSrv.Close()
		hClient := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}

		// Round 1 (Anthropic 流式)：产生已清洗文本 "先看下文件。" + tool_use(ReadFile, {"a":1,"b":2})
		r1Body, _ := json.Marshal(map[string]any{
			"model": "claude-opus-5", "max_tokens": 512, "stream": true,
			"tools": []map[string]any{{
				"name": "ReadFile", "input_schema": map[string]any{"type": "object"},
			}},
			"messages": []map[string]any{{"role": "user", "content": "查一下 config"}},
		})
		req1, _ := http.NewRequest(http.MethodPost, hSrv.URL+"/v1/messages", bytes.NewReader(r1Body))
		req1.Header.Set("Content-Type", "application/json")
		req1.Header.Set("X-Conversation-Id", "stream-clean-tool")
		resp1, err := hClient.Do(req1)
		if err != nil {
			t.Fatalf("r1 failed: %v", err)
		}
		var outToolID string
		sc := bufio.NewScanner(resp1.Body)
		for sc.Scan() {
			line := sc.Text()
			if strings.Contains(line, `"type":"tool_use"`) && strings.Contains(line, `"name":"ReadFile"`) {
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
			t.Fatalf("expected emitted tool_use id from round 1 stream")
		}

		// Round 2 (OpenAI 流式中途报错)：带回 "半截输出" 但失败，不应覆盖或污染上一轮记录
		r2ErrBody, _ := json.Marshal(map[string]any{
			"model": "smart", "stream": true,
			"messages": []map[string]any{{"role": "user", "content": "查一下 config"}},
		})
		req2Err, _ := http.NewRequest(http.MethodPost, hSrv.URL+"/v1/chat/completions", bytes.NewReader(r2ErrBody))
		req2Err.Header.Set("Content-Type", "application/json")
		req2Err.Header.Set("X-Conversation-Id", "stream-err-check")
		resp2Err, err := hClient.Do(req2Err)
		if err != nil {
			t.Fatalf("r2 failed: %v", err)
		}
		_, _ = io.ReadAll(resp2Err.Body)
		resp2Err.Body.Close()

		// Round 2b：回到 "stream-clean-tool"，用等价但键序和空格不同的 JSON 参数 {"a": 1,   "b": 2} 续聊 -> 必须命中复用！
		r2OKBody, _ := json.Marshal(map[string]any{
			"model": "claude-opus-5", "max_tokens": 512, "stream": true,
			"tools": []map[string]any{{
				"name": "ReadFile", "input_schema": map[string]any{"type": "object"},
			}},
			"messages": []map[string]any{
				{"role": "user", "content": "查一下 config"},
				{"role": "assistant", "content": []map[string]any{
					{"type": "text", "text": "先看下文件。"},
					{"type": "tool_use", "id": outToolID, "name": "ReadFile", "input": map[string]any{"a": 1, "b": 2}},
				}},
				{"role": "user", "content": []map[string]any{
					{"type": "tool_result", "tool_use_id": outToolID, "content": "ok-content"},
				}},
			},
		})
		req2OK, _ := http.NewRequest(http.MethodPost, hSrv.URL+"/v1/messages", bytes.NewReader(r2OKBody))
		req2OK.Header.Set("Content-Type", "application/json")
		req2OK.Header.Set("X-Conversation-Id", "stream-clean-tool")
		resp2OK, err := hClient.Do(req2OK)
		if err != nil {
			t.Fatalf("r2OK failed: %v", err)
		}
		_, _ = io.ReadAll(resp2OK.Body)
		resp2OK.Body.Close()

		sMu.Lock()
		defer sMu.Unlock()
		lastTurn := sCap[len(sCap)-1]
		if lastTurn.HasSessionCreate || lastTurn.ThreadID != sCap[0].ThreadID {
			t.Fatalf("expected stream canonical tool args to hit resume %q, got create=%t tid=%q", sCap[0].ThreadID, lastTurn.HasSessionCreate, lastTurn.ThreadID)
		}
		if strings.Contains(lastTurn.Text, "查一下 config") {
			t.Fatalf("expected incremental prompt on resumed stream, got: %s", lastTurn.Text)
		}
	})
}

// TestImprovement_LegacyDBMigrationAndBackToBackRestart 验证旧版无 last_assistant_hash 列数据库平滑迁移、
// 首次接续旧记录时安全重建并补写新指纹、以及背靠背重启后的无损复用恢复。
func TestImprovement_LegacyDBMigrationAndBackToBackRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy_v242.db")

	// 手动创建不含 last_assistant_hash 列的旧版数据库并插入一条旧记录
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = rawDB.Exec(`
		CREATE TABLE conversations (
			conv_key TEXT PRIMARY KEY,
			thread_id TEXT NOT NULL,
			account_id INTEGER NOT NULL DEFAULT 0,
			base_hash TEXT NOT NULL DEFAULT '',
			message_hashes TEXT NOT NULL DEFAULT '[]',
			last_used INTEGER NOT NULL DEFAULT 0
		);
	`)
	if err != nil {
		rawDB.Close()
		t.Fatalf("create legacy table: %v", err)
	}
	legacyHashes, _ := json.Marshal(nonLeadingMessageFingerprints([]Message{{Role: "user", Content: "旧首句"}}))
	_, err = rawDB.Exec(`INSERT INTO conversations (conv_key, thread_id, account_id, base_hash, message_hashes, last_used) VALUES (?, ?, ?, ?, ?, ?)`,
		"h:legacy-conv-1", "legacy-tid-0001", 1, "v2::", string(legacyHashes), time.Now().Unix())
	rawDB.Close()
	if err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	wsSrv, _, captured, capMu := startControlledMockUpstream(t, func(attempt int32, w http.ResponseWriter, r *http.Request) (bool, string, map[string]any) {
		return true, fmt.Sprintf("迁移后第%d次回答", attempt), nil
	})

	startProxy := func() (*store.Store, *httptest.Server, *http.Client) {
		st, err := store.Open(dbPath)
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		list, _ := st.List(context.Background())
		if len(list) == 0 {
			if err := st.Save(context.Background(), &store.Account{
				Email: "mig@example.com", Provider: "social",
				WSHost: "ws://" + strings.TrimPrefix(wsSrv.URL, "http://"), WSPath: "/ws",
				AccessToken: "jwt-mig", ActorARN: "arn:aws:quick:us-east-1:mig", ExpiresAt: time.Now().Add(time.Hour).Unix(),
			}); err != nil {
				t.Fatalf("st.Save: %v", err)
			}
		}
		cfg := config.Default()
		cfg.SessionReuse = true
		srv := NewServer(&cfg, accounts.New(st, &cfg))
		hs := httptest.NewServer(srv.Handler())
		return st, hs, &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
	}

	// 第一次启动：接续旧记录 -> 因缺失 last_assistant_hash 必须安全重建并补写新指纹
	st1, hs1, cl1 := startProxy()
	send := func(hs *httptest.Server, cl *http.Client, msgs []map[string]any) {
		b, _ := json.Marshal(map[string]any{
			"model": "claude-opus-5", "max_tokens": 256, "stream": false, "messages": msgs,
		})
		req, _ := http.NewRequest(http.MethodPost, hs.URL+"/v1/messages", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Conversation-Id", "legacy-conv-1")
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatalf("POST failed: %v", err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d", resp.StatusCode)
		}
	}

	send(hs1, cl1, []map[string]any{
		{"role": "user", "content": "旧首句"},
		{"role": "assistant", "content": "旧回答"},
		{"role": "user", "content": "迁移后第一句"},
	})
	hs1.Close()
	_ = st1.Close()

	capMu.Lock()
	rebuiltTID := (*captured)[0].ThreadID
	if !(*captured)[0].HasSessionCreate || rebuiltTID == "legacy-tid-0001" {
		capMu.Unlock()
		t.Fatalf("expected legacy record without assistant hash to safely rebuild, create=%t tid=%q", (*captured)[0].HasSessionCreate, rebuiltTID)
	}
	capMu.Unlock()

	// 背靠背重启：新进程打开同一个 SQLite 文件并发送下一轮 -> 应无缝命中新 thread_id 复用！
	st2, hs2, cl2 := startProxy()
	defer st2.Close()
	defer hs2.Close()

	send(hs2, cl2, []map[string]any{
		{"role": "user", "content": "旧首句"},
		{"role": "assistant", "content": "旧回答"},
		{"role": "user", "content": "迁移后第一句"},
		{"role": "assistant", "content": "迁移后第1次回答"},
		{"role": "user", "content": "重启后继续提问"},
	})

	capMu.Lock()
	defer capMu.Unlock()
	afterRestart := (*captured)[1]
	if afterRestart.HasSessionCreate || afterRestart.ThreadID != rebuiltTID {
		t.Fatalf("expected back-to-back restart to reuse %q, got create=%t tid=%q", rebuiltTID, afterRestart.HasSessionCreate, afterRestart.ThreadID)
	}
	if strings.Contains(afterRestart.Text, "旧首句") || !strings.Contains(afterRestart.Text, "重启后继续提问") {
		t.Fatalf("expected incremental prompt after restart, got: %s", afterRestart.Text)
	}
}

// TestImprovement_ConversationInvalidationAndStaleSaveBlocked 验证第一批 2.2：
// 单删、全清同步使内存与 SQLite 失效，且清理前发起、清理后结束的旧请求不能把旧映射写回复活。
func TestImprovement_ConversationInvalidationAndStaleSaveBlocked(t *testing.T) {
	gateReady := make(chan struct{})
	gateRelease := make(chan struct{})
	var holdTurn atomic.Bool
	var seq atomic.Int32
	var capTurns []capturedUpstreamTurn
	var capMu sync.Mutex
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

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
				capTurns = append(capTurns, rec)
				capMu.Unlock()

				n := seq.Add(1)
				if holdTurn.CompareAndSwap(true, false) {
					close(gateReady)
					<-gateRelease
				}
				writeEv := func(v any) {
					_ = conn.WriteMessage(websocket.BinaryMessage, encodeMockAIDPServerEvent(v))
				}
				writeEv(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m"}})
				writeEv(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": fmt.Sprintf("reply-%d", n)}})
				writeEv(map[string]any{"type": "message_complete", "payload": map[string]any{"continuation_expected": false}})
				writeEv(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
			}
		}
	}))
	defer wsSrv.Close()

	dbPath := filepath.Join(t.TempDir(), "inval.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	_ = st.Save(context.Background(), &store.Account{
		Email: "inv@example.com", Provider: "social",
		WSHost: "ws://" + strings.TrimPrefix(wsSrv.URL, "http://"), WSPath: "/ws",
		AccessToken: "jwt-inv", ActorARN: "arn:aws:quick:us-east-1:inv", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})

	cfg := config.Default()
	cfg.SessionReuse = true
	srv := NewServer(&cfg, accounts.New(st, &cfg))
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}

	postConv := func(cid string, msgs []map[string]any) {
		b, _ := json.Marshal(map[string]any{"model": "claude-opus-5", "max_tokens": 256, "stream": false, "messages": msgs})
		req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/messages", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Conversation-Id", cid)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST(%s): %v", cid, err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST(%s) status=%d", cid, resp.StatusCode)
		}
	}

	// 1. 先建立两条会话，并验证单删与全清对内存和 DB 都立即生效
	postConv("k1", []map[string]any{{"role": "user", "content": "问题1"}})
	postConv("k2", []map[string]any{{"role": "user", "content": "问题2"}})
	if err := srv.DeleteConversation(context.Background(), "h:k1"); err != nil {
		t.Fatalf("DeleteConversation: %v", err)
	}
	postConv("k1", []map[string]any{
		{"role": "user", "content": "问题1"},
		{"role": "assistant", "content": "reply-1"},
		{"role": "user", "content": "问题1续聊"},
	})
	capMu.Lock()
	if !capTurns[2].HasSessionCreate || capTurns[2].ThreadID == capTurns[0].ThreadID {
		capMu.Unlock()
		t.Fatalf("expected k1 after DeleteConversation to create brand-new session")
	}
	capMu.Unlock()

	if err := srv.ClearConversations(context.Background()); err != nil {
		t.Fatalf("ClearConversations: %v", err)
	}
	postConv("k2", []map[string]any{
		{"role": "user", "content": "问题2"},
		{"role": "assistant", "content": "reply-2"},
		{"role": "user", "content": "问题2续聊"},
	})
	capMu.Lock()
	if !capTurns[3].HasSessionCreate || capTurns[3].ThreadID == capTurns[1].ThreadID {
		capMu.Unlock()
		t.Fatalf("expected k2 after ClearConversations to create brand-new session")
	}
	capMu.Unlock()

	// 2. 在途旧请求竞态：请求 k3 开始于清理前，在清理完成后才结束 -> 不得写回 SQLite 与内存
	holdTurn.Store(true)
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		postConv("k3", []map[string]any{{"role": "user", "content": "在途竞态问题"}})
	}()
	<-gateReady
	if err := srv.ClearConversations(context.Background()); err != nil {
		close(gateRelease)
		t.Fatalf("ClearConversations during inflight: %v", err)
	}
	close(gateRelease)
	<-doneCh

	if rec, _ := st.GetConversation(context.Background(), "h:k3"); rec != nil {
		t.Fatalf("stale inflight request resurrected deleted conversation in DB: %+v", rec)
	}
	postConv("k3", []map[string]any{
		{"role": "user", "content": "在途竞态问题"},
		{"role": "assistant", "content": "reply-5"},
		{"role": "user", "content": "竞态后续聊"},
	})
	capMu.Lock()
	defer capMu.Unlock()
	last := capTurns[len(capTurns)-1]
	prev := capTurns[len(capTurns)-2]
	if !last.HasSessionCreate || last.ThreadID == prev.ThreadID {
		t.Fatalf("expected post-clear request not to reuse stale inflight thread %q", prev.ThreadID)
	}
}

// TestImprovement_ActiveEvictionProtectionAndCancelableWait 验证第一批 2.3：
// 容量为 1 时活跃条目绝不被淘汰；同会话严格串行且排队请求可随时响应 context 取消立即退出。
func TestImprovement_ActiveEvictionProtectionAndCancelableWait(t *testing.T) {
	cs := newConversationStore(time.Hour, 1)

	ctxA1 := context.Background()
	convA1, ep1, ver1, err := cs.acquire(ctxA1, "keyA")
	if err != nil {
		t.Fatalf("acquire keyA #1: %v", err)
	}
	cs.save(convA1, ep1, ver1, "tid-A", 1, "base", []Message{{Role: "user", Content: "hi"}}, "hashA")

	// 插入/获取 keyB 触发容量检查 (max=1)：活跃的 keyA refs>0，绝不能被淘汰
	convB, _, _, err := cs.acquire(context.Background(), "keyB")
	if err != nil {
		cs.release(convA1)
		t.Fatalf("acquire keyB: %v", err)
	}
	cs.release(convB)

	// keyA 第 2 个请求排队等待同一个 convA1，在 convA1 未释放前不得提前拿到执行权
	waitGotLock := make(chan *conversation, 1)
	go func() {
		c2, _, _, err := cs.acquire(context.Background(), "keyA")
		if err == nil {
			waitGotLock <- c2
		}
	}()
	select {
	case <-waitGotLock:
		cs.release(convA1)
		t.Fatalf("keyA #2 entered concurrently while keyA #1 still active (eviction broke serialization)")
	case <-time.After(40 * time.Millisecond):
	}

	// keyA 第 3 个排队请求带可取消 context：未等 keyA #1 结束即应立刻返回 context.Canceled
	ctxCancel, cancel := context.WithCancel(context.Background())
	cancelDone := make(chan error, 1)
	go func() {
		_, _, _, err := cs.acquire(ctxCancel, "keyA")
		cancelDone <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case cerr := <-cancelDone:
		if !errors.Is(cerr, context.Canceled) {
			cs.release(convA1)
			t.Fatalf("expected context.Canceled for queued waiter, got %v", cerr)
		}
	case <-time.After(300 * time.Millisecond):
		cs.release(convA1)
		t.Fatalf("queued waiter blocked instead of aborting promptly on cancel")
	}

	// 释放 convA1 后，keyA #2 顺利拿到同一个会话对象且数据完整
	cs.release(convA1)
	select {
	case c2 := <-waitGotLock:
		if c2 != convA1 || c2.threadID != "tid-A" {
			cs.release(c2)
			t.Fatalf("expected same active conversation object tid-A, got %p tid=%q", c2, c2.threadID)
		}
		cs.release(c2)
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("keyA #2 did not resume after keyA #1 released")
	}
}

// TestImprovement_AttachmentIntegrityStrict 验证第二批 3.1：
// 16 个有效 PDF 正常发送，17 个/损坏/无 TenantURL/上传失败均明确报错且绝不向模型发出残缺生成请求；
// 文本 document 不占配额，续聊仅检查本轮实际附件。
func TestImprovement_AttachmentIntegrityStrict(t *testing.T) {
	var failUpload atomic.Bool
	var wsDials atomic.Int32
	var uploadedCount atomic.Int32

	tenantSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/integration/quick-work/upload-attachment":
			if failUpload.Load() {
				http.Error(w, "storage down", http.StatusInternalServerError)
				return
			}
			n := uploadedCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"attachmentId": fmt.Sprintf("att-%d", n),
				"path":         fmt.Sprintf("/data/files/att-%d.pdf", n),
			})
		case "/integration/quick-work/attachment-dlp-status":
			_ = json.NewEncoder(w).Encode(map[string]string{"dlpJobStatus": "COMPLETED", "policyAction": "ALLOW"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer tenantSrv.Close()

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	var lastMsgPayload string
	var userMsgCount atomic.Int32
	var wsMu sync.Mutex
	wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsDials.Add(1)
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
			if !ok {
				continue
			}
			if mType == "user_message" {
				userMsgCount.Add(1)
				wsMu.Lock()
				lastMsgPayload, _ = payload["text"].(string)
				wsMu.Unlock()
				writeEv := func(v any) {
					_ = conn.WriteMessage(websocket.BinaryMessage, encodeMockAIDPServerEvent(v))
				}
				writeEv(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": "m_att"}})
				writeEv(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": "附件已分析"}})
				writeEv(map[string]any{"type": "message_complete", "payload": map[string]any{"continuation_expected": false}})
				writeEv(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
			}
		}
	}))
	defer wsSrv.Close()

	dbPath := filepath.Join(t.TempDir(), "att_strict.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	_ = st.Save(context.Background(), &store.Account{
		Email: "att@example.com", Provider: "social",
		TenantURL: tenantSrv.URL,
		WSHost:    "ws://" + strings.TrimPrefix(wsSrv.URL, "http://"), WSPath: "/ws",
		AccessToken: "jwt-att", ActorARN: "arn:aws:quick:us-east-1:att", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})

	cfg := config.Default()
	cfg.SessionReuse = true
	srv := NewServer(&cfg, accounts.New(st, &cfg))
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}

	makePDFBlocks := func(n int) []map[string]any {
		blocks := []map[string]any{{"type": "text", "text": "请检查这些文件"}}
		for i := 1; i <= n; i++ {
			blocks = append(blocks, map[string]any{
				"type":  "document",
				"title": fmt.Sprintf("paper-%02d.pdf", i),
				"source": map[string]any{
					"type": "base64", "media_type": "application/pdf",
					"data": base64.StdEncoding.EncodeToString(makeFakePDF(fmt.Sprintf("P%02d", i))),
				},
			})
		}
		return blocks
	}
	postAnthropic := func(cid string, msgs []map[string]any) (int, string) {
		b, _ := json.Marshal(map[string]any{"model": "claude-opus-5", "max_tokens": 256, "stream": false, "messages": msgs})
		req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/messages", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Conversation-Id", cid)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	// 1. 17 个 PDF 必须拒绝，不拨号不发 user_message
	beforeDials := wsDials.Load()
	code17, body17 := postAnthropic("c17", []map[string]any{{"role": "user", "content": makePDFBlocks(17)}})
	if code17 != http.StatusBadRequest || !strings.Contains(body17, "paper-17.pdf") || wsDials.Load() != beforeDials {
		t.Fatalf("expected 400 naming paper-17.pdf and 0 upstream dials, got code=%d dials=%d body=%s", code17, wsDials.Load()-beforeDials, body17)
	}

	// 2. 损坏 Base64 PDF 必须拒绝且 0 次拨号
	codeBad, _ := postAnthropic("cBad", []map[string]any{{"role": "user", "content": []map[string]any{
		{"type": "document", "title": "broken.pdf", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": "!!!not-base64!!!"}},
	}}})
	if codeBad != http.StatusBadRequest || wsDials.Load() != beforeDials {
		t.Fatalf("expected 400 and 0 dials for corrupted PDF, got code=%d dials=%d", codeBad, wsDials.Load()-beforeDials)
	}

	// 3. 16 个 PDF + 1 个纯文本文档（内联）全部成功，且第 2 轮无新附件的增量续聊也正常通过
	blocks16PlusTxt := append(makePDFBlocks(16), map[string]any{
		"type": "document", "title": "notes.txt",
		"source": map[string]any{"type": "base64", "media_type": "text/plain", "data": base64.StdEncoding.EncodeToString([]byte("补充纯文本说明"))},
	})
	code16, _ := postAnthropic("c16", []map[string]any{{"role": "user", "content": blocks16PlusTxt}})
	if code16 != http.StatusOK {
		t.Fatalf("expected 16 PDFs + inline text doc to succeed, got %d", code16)
	}
	wsMu.Lock()
	if !strings.Contains(lastMsgPayload, "补充纯文本说明") {
		wsMu.Unlock()
		t.Fatalf("inline text document content missing in prompt")
	}
	wsMu.Unlock()

	code16Next, _ := postAnthropic("c16", []map[string]any{
		{"role": "user", "content": blocks16PlusTxt},
		{"role": "assistant", "content": "附件已分析"},
		{"role": "user", "content": "总结第 3 份重点"},
	})
	if code16Next != http.StatusOK {
		t.Fatalf("expected resumed turn without new attachments to succeed, got %d", code16Next)
	}

	// 4. 上游上传返回 500 最终失败时，绝不向模型派发 user_message 生成请求
	failUpload.Store(true)
	userMsgsBeforeFail := userMsgCount.Load()
	codeUpFail, _ := postAnthropic("cUpFail", []map[string]any{{"role": "user", "content": makePDFBlocks(1)}})
	if codeUpFail == http.StatusOK || userMsgCount.Load() != userMsgsBeforeFail {
		t.Fatalf("expected failure and 0 user_message dispatches when attachment upload fails, got code=%d dispatched=%d", codeUpFail, userMsgCount.Load()-userMsgsBeforeFail)
	}

	// 5. 账号无 TenantURL 不支持附件上传时，同样明确报错且 0 次 user_message 派发
	failUpload.Store(false)
	acct, getErr := st.Get(context.Background(), 1)
	if getErr != nil {
		t.Fatalf("st.Get: %v", getErr)
	}
	acct.TenantURL = ""
	if updErr := st.Update(context.Background(), acct); updErr != nil {
		t.Fatalf("st.Update: %v", updErr)
	}
	userMsgsBeforeUnsupported := userMsgCount.Load()
	codeUnsupported, bodyUnsupported := postAnthropic("cUnsupported", []map[string]any{{"role": "user", "content": makePDFBlocks(1)}})
	if codeUnsupported == http.StatusOK || !strings.Contains(bodyUnsupported, "attachment uploads") || userMsgCount.Load() != userMsgsBeforeUnsupported {
		t.Fatalf("expected unsupported attachment upload error and 0 user_message dispatches, got code=%d dispatched=%d body=%s", codeUnsupported, userMsgCount.Load()-userMsgsBeforeUnsupported, bodyUnsupported)
	}
}

// TestImprovement_AccountRateLimitCooldownAndRecovery 验证第二批 3.2：
// 明确 429 限流设置跨请求冷却、有备号时用完整历史接手、单号/全号冷却时停止重拨返回 429 与 Retry-After 且保留原会话、
// 模拟时钟推进到期后自动恢复，且普通网络超时不触发冷却。
func TestImprovement_AccountRateLimitCooldownAndRecovery(t *testing.T) {
	t.Run("two_accounts_429_cooldown_skip_and_expiry", func(t *testing.T) {
		var rl1, rl2 atomic.Bool
		ws1, dials1, _, _ := startControlledMockUpstream(t, func(attempt int32, w http.ResponseWriter, r *http.Request) (bool, string, map[string]any) {
			if rl1.Load() {
				w.Header().Set("Retry-After", "30")
				http.Error(w, "too many requests", http.StatusTooManyRequests)
				return false, "", nil
			}
			return true, "from-acct1", nil
		})
		ws2, dials2, cap2, mu2 := startControlledMockUpstream(t, func(attempt int32, w http.ResponseWriter, r *http.Request) (bool, string, map[string]any) {
			if rl2.Load() {
				w.Header().Set("Retry-After", "30")
				http.Error(w, "too many requests", http.StatusTooManyRequests)
				return false, "", nil
			}
			return true, "from-acct2", nil
		})

		httpSrv, client, srv := setupTwoAccountProxy(t, ws1, ws2, 3)
		now := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
		srv.accounts.SetNowFunc(func() time.Time { return now })

		postMsg := func(cid, text string) int {
			b, _ := json.Marshal(map[string]any{
				"model": "claude-opus-5", "max_tokens": 256, "stream": false,
				"messages": []map[string]any{{"role": "user", "content": text}},
			})
			req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/messages", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Conversation-Id", cid)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("POST failed: %v", err)
			}
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			return resp.StatusCode
		}

		// 先令第 1 个被轮到的账号限流
		rl1.Store(true)
		rl2.Store(false)
		// 跑一轮发现是哪个账号触发了 429 并成功切给另一个
		if code := postMsg("cd-1", "首轮限流探测"); code != http.StatusOK {
			t.Fatalf("expected fallback account to succeed, got %d", code)
		}
		if dials1.Load() == 0 {
			// round-robin 先落到了 ws2；让 ws2 限流、恢复 ws1 再触发一次冷却
			rl1.Store(false)
			rl2.Store(true)
			if code := postMsg("cd-2", "触发另一账号限流"); code != http.StatusOK {
				t.Fatalf("expected fallback to acct1, got %d", code)
			}
		}

		// 检查此刻确实有且仅有一个账号进入冷却期，随后连续发 4 次新请求均不能拨打处于冷却的账号
		coolingDials := dials1
		activeDials := dials2
		if rl2.Load() {
			coolingDials = dials2
			activeDials = dials1
		}
		dBefore := coolingDials.Load()
		aBefore := activeDials.Load()
		for i := 0; i < 4; i++ {
			if code := postMsg(fmt.Sprintf("cd-new-%d", i), "新请求必须避开冷却账号"); code != http.StatusOK {
				t.Fatalf("new request %d failed: %d", i, code)
			}
		}
		if coolingDials.Load() != dBefore {
			t.Fatalf("cooled account was dialed %d extra times across subsequent requests!", coolingDials.Load()-dBefore)
		}
		if activeDials.Load() != aBefore+4 {
			t.Fatalf("expected all 4 requests routed to healthy account, got %d", activeDials.Load()-aBefore)
		}
		mu2.Lock()
		_ = cap2
		mu2.Unlock()

		// 推进模拟时钟 31 秒：冷却到期，账号立即恢复可被选回
		rl1.Store(false)
		rl2.Store(false)
		now = now.Add(31 * time.Second)
		for i := 0; i < 3; i++ {
			if code := postMsg(fmt.Sprintf("cd-rec-%d", i), "冷却到期恢复测试"); code != http.StatusOK {
				t.Fatalf("post-expiry request failed: %d", code)
			}
		}
		if coolingDials.Load() <= dBefore {
			t.Fatalf("expected previously cooled account to recover after cooldown expiry")
		}
	})

	t.Run("single_account_429_stops_redial_preserves_session_and_recovers", func(t *testing.T) {
		var rl atomic.Bool
		wsSrv, dials, capTurns, capMu := startControlledMockUpstream(t, func(attempt int32, w http.ResponseWriter, r *http.Request) (bool, string, map[string]any) {
			if rl.Load() {
				w.Header().Set("Retry-After", "20")
				http.Error(w, "rate limit hit", http.StatusTooManyRequests)
				return false, "", nil
			}
			return true, fmt.Sprintf("single-ok-%d", attempt), nil
		})

		dbPath := filepath.Join(t.TempDir(), "single_cd.db")
		st, err := store.Open(dbPath)
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		defer st.Close()
		_ = st.Save(context.Background(), &store.Account{
			Email: "single@example.com", Provider: "social",
			WSHost: "ws://" + strings.TrimPrefix(wsSrv.URL, "http://"), WSPath: "/ws",
			AccessToken: "jwt-single", ActorARN: "arn:aws:quick:us-east-1:single", ExpiresAt: time.Now().Add(time.Hour).Unix(),
		})
		cfg := config.Default()
		cfg.SessionReuse = true
		cfg.MaxAttempts = 3
		mgr := accounts.New(st, &cfg)
		now := time.Date(2026, 11, 26, 9, 30, 0, 0, time.UTC)
		mgr.SetNowFunc(func() time.Time { return now })
		srv := NewServer(&cfg, mgr)
		httpSrv := httptest.NewServer(srv.Handler())
		defer httpSrv.Close()
		client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}

		post := func(msgs []map[string]any) (int, http.Header) {
			b, _ := json.Marshal(map[string]any{"model": "claude-opus-5", "max_tokens": 256, "stream": false, "messages": msgs})
			req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/messages", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Conversation-Id", "single-cd-conv")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("POST failed: %v", err)
			}
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			return resp.StatusCode, resp.Header
		}

		// 1. 首轮正常建立会话
		if code, _ := post([]map[string]any{{"role": "user", "content": "单号首轮-ANCHOR"}}); code != http.StatusOK {
			t.Fatalf("round 1 status=%d", code)
		}
		capMu.Lock()
		savedTID := (*capTurns)[0].ThreadID
		capMu.Unlock()

		// 2. 第 2 轮触发 429：单号应停止反复重拨并返回 429 + Retry-After
		rl.Store(true)
		msgs2 := []map[string]any{
			{"role": "user", "content": "单号首轮-ANCHOR"},
			{"role": "assistant", "content": "single-ok-1"},
			{"role": "user", "content": "单号第2轮"},
		}
		code2, hdr2 := post(msgs2)
		if code2 != http.StatusTooManyRequests || hdr2.Get("Retry-After") == "" {
			t.Fatalf("expected HTTP 429 with Retry-After, got status=%d Retry-After=%q", code2, hdr2.Get("Retry-After"))
		}
		dAfter429 := dials.Load()

		// 3. 冷却期内再次请求：绝不再拨打上游（0 次新增拨号），直接返回 429
		code3, _ := post(msgs2)
		if code3 != http.StatusTooManyRequests || dials.Load() != dAfter429 {
			t.Fatalf("expected fast 429 without redialing during cooldown, got status=%d extra_dials=%d", code3, dials.Load()-dAfter429)
		}

		// 4. 冷却到期后：原持久化会话仍完好保留，直接以增量方式恢复续聊！
		rl.Store(false)
		now = now.Add(21 * time.Second)
		code4, _ := post(msgs2)
		if code4 != http.StatusOK {
			t.Fatalf("expected recovery after cooldown, got status=%d", code4)
		}
		capMu.Lock()
		defer capMu.Unlock()
		last := (*capTurns)[len(*capTurns)-1]
		if last.HasSessionCreate || last.ThreadID != savedTID {
			t.Fatalf("expected preserved session tid=%q after cooldown expiry, got create=%t tid=%q", savedTID, last.HasSessionCreate, last.ThreadID)
		}
		if mgr.CooldownRemaining(1) != 0 {
			t.Fatalf("expected cooldown cleared after expiry")
		}
	})

	t.Run("network_timeout_does_not_cooldown", func(t *testing.T) {
		prevDialTimeout := wsDialHandshakeTimeout
		wsDialHandshakeTimeout = 70 * time.Millisecond
		defer func() { wsDialHandshakeTimeout = prevDialTimeout }()

		wsSrv, dials, _, _ := startControlledMockUpstream(t, func(attempt int32, w http.ResponseWriter, r *http.Request) (bool, string, map[string]any) {
			if attempt == 1 {
				select {
				case <-time.After(180 * time.Millisecond):
				case <-r.Context().Done():
				}
				return false, "", nil
			}
			return true, "recovered-from-timeout", nil
		})
		dbPath := filepath.Join(t.TempDir(), "timeout_nocd.db")
		st, err := store.Open(dbPath)
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		defer st.Close()
		_ = st.Save(context.Background(), &store.Account{
			Email: "nocd@example.com", Provider: "social",
			WSHost: "ws://" + strings.TrimPrefix(wsSrv.URL, "http://"), WSPath: "/ws",
			AccessToken: "jwt-nocd", ActorARN: "arn:aws:quick:us-east-1:nocd", ExpiresAt: time.Now().Add(time.Hour).Unix(),
		})
		cfg := config.Default()
		cfg.MaxAttempts = 2
		mgr := accounts.New(st, &cfg)
		srv := NewServer(&cfg, mgr)
		httpSrv := httptest.NewServer(srv.Handler())
		defer httpSrv.Close()
		client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}

		b, _ := json.Marshal(map[string]any{
			"model": "claude-opus-5", "max_tokens": 128, "stream": false,
			"messages": []map[string]any{{"role": "user", "content": "普通网络超时测试"}},
		})
		resp, err := client.Post(httpSrv.URL+"/v1/messages", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("POST failed: %v", err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || dials.Load() != 2 {
			t.Fatalf("expected timeout retry success in 2 dials, got status=%d dials=%d", resp.StatusCode, dials.Load())
		}
		if rem := mgr.CooldownRemaining(1); rem != 0 {
			t.Fatalf("network timeout must NOT trigger account cooldown, got remaining=%v", rem)
		}
	})
}

// TestImprovement_FullTranscriptHistoricalAttachmentBudget 验证多轮长对话全量重放时的附件配额分配与严格校验：
// 1) 历史累计 20 张旧图 + 当前轮纯文本：保留最新 16 张，更早 4 张诚实标为 [earlier image omitted]，绝不虚标 [attached ...]；
// 2) 历史累计 18 张旧图 + 1 份历史纯文本文档 + 当前轮 3 张新图：优先保当前轮全部 3 张 + 历史最近 13 张，纯文本文档内联不占配额；
// 3) 历史累计 10 张旧图 + 当前轮新发 17 张新图：严格返回 ErrInvalidAttachment 并指明第 17 张；
// 4) 历史累计 5 张旧图 + 当前轮 1 张损坏 Base64 图：严格返回 ErrInvalidAttachment；
// 5) 历史含 1 张损坏旧图 + 当前轮正常提问：降级标为 [earlier image unavailable] 不锁死长会话；
// 6) 端到端 HTTP 集成：老库无 last_assistant_hash 的 20 图长对话升级后首轮安全重建（上传 16 张），并补写指纹使第 2 轮命中增量复用。
func TestImprovement_FullTranscriptHistoricalAttachmentBudget(t *testing.T) {
	makePNGDataURI := func(tag byte) string {
		return "data:image/png;base64," + base64.StdEncoding.EncodeToString(makeFakePNG(tag))
	}
	makeImageMsg := func(tag byte, note string) Message {
		return Message{
			Role: "user",
			Content: []any{
				map[string]any{"type": "text", "text": note},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": makePNGDataURI(tag)}},
			},
		}
	}

	t.Run("20_historical_images_with_plain_followup_keeps_latest_16", func(t *testing.T) {
		var msgs []Message
		for i := 1; i <= 20; i++ {
			msgs = append(msgs, makeImageMsg(byte(i), fmt.Sprintf("第%d张历史图：", i)))
			msgs = append(msgs, Message{Role: "assistant", Content: fmt.Sprintf("收到第%d张图", i)})
		}
		msgs = append(msgs, Message{Role: "user", Content: "请总结刚才的全部结果"})

		prompt, imgs, err := formatPromptWithImages(context.Background(), msgs)
		if err != nil {
			t.Fatalf("formatPromptWithImages failed: %v", err)
		}
		if len(imgs) != maxImagesPerTurn {
			t.Fatalf("got %d uploaded images, want %d", len(imgs), maxImagesPerTurn)
		}
		// 应保留第 5~20 张，前 4 张诚实省略
		if omitted := strings.Count(prompt, "[earlier image omitted]"); omitted != 4 {
			t.Fatalf("expected 4 [earlier image omitted] markers, got %d in %q", omitted, prompt)
		}
		if attached := strings.Count(prompt, "[attached image: "); attached != maxImagesPerTurn {
			t.Fatalf("expected %d [attached image: ...] markers, got %d", maxImagesPerTurn, attached)
		}
		for idx, att := range imgs {
			wantTag := byte(idx + 5)
			if !bytes.Equal(att.Data, makeFakePNG(wantTag)) {
				t.Fatalf("img[%d] expected historical tag %d, got %v", idx, wantTag, att.Data)
			}
			marker := "[attached image: " + att.Name + "]"
			if strings.Count(prompt, marker) != 1 {
				t.Fatalf("expected prompt to contain %q once", marker)
			}
		}
	})

	t.Run("18_historical_plus_inline_text_doc_and_3_current_images", func(t *testing.T) {
		var msgs []Message
		for i := 1; i <= 18; i++ {
			content := []any{
				map[string]any{"type": "text", "text": fmt.Sprintf("旧图%d：", i)},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": makePNGDataURI(byte(i))}},
			}
			if i == 3 {
				content = append(content, map[string]any{
					"type":  "document",
					"title": "history-notes.txt",
					"source": map[string]any{
						"type":       "base64",
						"media_type": "text/plain",
						"data":       base64.StdEncoding.EncodeToString([]byte("重要历史文本笔记")),
					},
				})
			}
			msgs = append(msgs, Message{Role: "user", Content: content})
			msgs = append(msgs, Message{Role: "assistant", Content: fmt.Sprintf("已阅旧图%d", i)})
		}
		msgs = append(msgs, Message{
			Role: "user",
			Content: []any{
				map[string]any{"type": "text", "text": "对比这3张新图："},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": makePNGDataURI(101)}},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": makePNGDataURI(102)}},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": makePNGDataURI(103)}},
			},
		})

		prompt, imgs, err := formatPromptWithImages(context.Background(), msgs)
		if err != nil {
			t.Fatalf("formatPromptWithImages failed: %v", err)
		}
		if len(imgs) != maxImagesPerTurn {
			t.Fatalf("got %d images, want %d", len(imgs), maxImagesPerTurn)
		}
		// 历史 18 张只能进 13 张（第 6~18 张），最早 5 张省略；纯文本文档应保留内联且不占配额
		if omitted := strings.Count(prompt, "[earlier image omitted]"); omitted != 5 {
			t.Fatalf("expected 5 [earlier image omitted] markers, got %d", omitted)
		}
		if !strings.Contains(prompt, "重要历史文本笔记") {
			t.Fatalf("expected historical inline text document to be preserved without counting toward quota")
		}
		if attached := strings.Count(prompt, "[attached image: "); attached != maxImagesPerTurn {
			t.Fatalf("expected %d [attached image: ...] markers, got %d", maxImagesPerTurn, attached)
		}
		// 最后 3 张必须是当前轮全部 3 张新图 (101, 102, 103)
		for k := 0; k < 3; k++ {
			got := imgs[13+k]
			if !bytes.Equal(got.Data, makeFakePNG(byte(101+k))) {
				t.Fatalf("current turn image #%d mismatch at slot %d", k+1, 13+k)
			}
			if !strings.Contains(prompt, "[attached image: "+got.Name+"]") {
				t.Fatalf("missing current turn image ref %q", got.Name)
			}
		}
	})

	t.Run("10_historical_and_17_current_images_strictly_rejected", func(t *testing.T) {
		var msgs []Message
		for i := 1; i <= 10; i++ {
			msgs = append(msgs, makeImageMsg(byte(i), fmt.Sprintf("历史%d", i)))
			msgs = append(msgs, Message{Role: "assistant", Content: "ok"})
		}
		var curParts []any
		curParts = append(curParts, map[string]any{"type": "text", "text": "本轮17张图"})
		for i := 1; i <= 17; i++ {
			curParts = append(curParts, map[string]any{
				"type":      "input_image",
				"image_url": makePNGDataURI(byte(150 + i)),
			})
		}
		msgs = append(msgs, Message{Role: "user", Content: curParts})

		_, _, err := formatPromptWithImages(context.Background(), msgs)
		if !errors.Is(err, ErrInvalidAttachment) || !strings.Contains(err.Error(), "image #17") {
			t.Fatalf("expected ErrInvalidAttachment naming image #17, got %v", err)
		}
	})

	t.Run("5_historical_and_corrupted_current_image_rejected", func(t *testing.T) {
		var msgs []Message
		for i := 1; i <= 5; i++ {
			msgs = append(msgs, makeImageMsg(byte(i), fmt.Sprintf("历史%d", i)))
			msgs = append(msgs, Message{Role: "assistant", Content: "ok"})
		}
		msgs = append(msgs, Message{
			Role: "user",
			Content: []any{
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64,!!!bad-base64!!!"},
			},
		})
		_, _, err := formatPromptWithImages(context.Background(), msgs)
		if !errors.Is(err, ErrInvalidAttachment) {
			t.Fatalf("expected ErrInvalidAttachment for corrupted current image, got %v", err)
		}
	})

	t.Run("corrupted_historical_image_degrades_honestly_without_blocking", func(t *testing.T) {
		msgs := []Message{
			{
				Role: "user",
				Content: []any{
					map[string]any{"type": "text", "text": "旧坏图："},
					map[string]any{"type": "input_image", "image_url": "data:image/png;base64,!!!broken!!!"},
				},
			},
			{Role: "assistant", Content: "当时分析完了"},
			{Role: "user", Content: "继续当前问题"},
		}
		prompt, imgs, err := formatPromptWithImages(context.Background(), msgs)
		if err != nil {
			t.Fatalf("expected corrupted historical image not to block turn, got %v", err)
		}
		if len(imgs) != 0 {
			t.Fatalf("expected 0 valid attachments, got %d", len(imgs))
		}
		if !strings.Contains(prompt, "[earlier image unavailable]") || strings.Contains(prompt, "[attached image:") {
			t.Fatalf("expected honest [earlier image unavailable] without fake attached ref, got %q", prompt)
		}
	})

	t.Run("e2e_legacy_db_20_images_upgrade_rebuild_then_resume", func(t *testing.T) {
		var uploadedNames []string
		var uploadMu sync.Mutex
		tenantSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/integration/quick-work/upload-attachment":
				name := r.URL.Query().Get("name")
				uploadMu.Lock()
				uploadedNames = append(uploadedNames, name)
				n := len(uploadedNames)
				uploadMu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{
					"attachmentId": fmt.Sprintf("att-%d", n),
					"path":         fmt.Sprintf("/data/files/%s", name),
				})
			case "/integration/quick-work/attachment-dlp-status":
				_ = json.NewEncoder(w).Encode(map[string]string{"dlpJobStatus": "COMPLETED", "policyAction": "ALLOW"})
			default:
				http.NotFound(w, r)
			}
		}))
		defer tenantSrv.Close()

		type capFrame struct {
			hasCreate bool
			threadID  string
			text      string
			fileCount int
		}
		var frames []capFrame
		var fMu sync.Mutex
		var seq atomic.Int32
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			hasCreate := false
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
					hasCreate = true
				case "user_message":
					tid, _ := payload["thread_id"].(string)
					text, _ := payload["text"].(string)
					atts, _ := payload["attachments"].([]any)
					fMu.Lock()
					frames = append(frames, capFrame{
						hasCreate: hasCreate,
						threadID:  tid,
						text:      text,
						fileCount: len(atts),
					})
					fMu.Unlock()
					n := seq.Add(1)
					writeEv := func(v any) {
						_ = conn.WriteMessage(websocket.BinaryMessage, encodeMockAIDPServerEvent(v))
					}
					writeEv(map[string]any{"type": "message_start", "payload": map[string]any{"message_id": fmt.Sprintf("m%d", n)}})
					writeEv(map[string]any{"type": "text_delta", "payload": map[string]any{"delta": fmt.Sprintf("重建答复-%d", n)}})
					writeEv(map[string]any{"type": "message_complete", "payload": map[string]any{"continuation_expected": false}})
					writeEv(map[string]any{"type": "turn_completed", "payload": map[string]any{}})
				}
			}
		}))
		defer wsSrv.Close()

		// 构造含 20 张历史图的 Anthropic 对话
		var rawMsgs []map[string]any
		var proxyMsgs []Message
		for i := 1; i <= 20; i++ {
			b64 := base64.StdEncoding.EncodeToString(makeFakePNG(byte(i)))
			rawMsgs = append(rawMsgs, map[string]any{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": fmt.Sprintf("旧图#%d", i)},
					{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": b64}},
				},
			})
			rawMsgs = append(rawMsgs, map[string]any{
				"role":    "assistant",
				"content": fmt.Sprintf("旧答复#%d", i),
			})
			proxyMsgs = append(proxyMsgs, makeImageMsg(byte(i), fmt.Sprintf("旧图#%d", i)))
			proxyMsgs = append(proxyMsgs, Message{Role: "assistant", Content: fmt.Sprintf("旧答复#%d", i)})
		}

		// 在 SQLite 中写入不含 last_assistant_hash 的 v2.4.2 旧记录（模拟刚升级 v2.4.3/v2.4.4）
		dbPath := filepath.Join(t.TempDir(), "legacy_20img.db")
		st, err := store.Open(dbPath)
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		defer st.Close()
		if err := st.Save(context.Background(), &store.Account{
			Email: "img@example.com", Provider: "social",
			TenantURL: tenantSrv.URL,
			WSHost:    "ws://" + strings.TrimPrefix(wsSrv.URL, "http://"), WSPath: "/ws",
			AccessToken: "jwt-img", ActorARN: "arn:aws:quick:us-east-1:img", ExpiresAt: time.Now().Add(time.Hour).Unix(),
		}); err != nil {
			t.Fatalf("st.Save: %v", err)
		}
		// 注意：传入仅含前 39 条（前 20 轮 user + 前 19 条 asst + 第 20 轮 user，不含最后一条 asst）且 last_assistant_hash 为空
		legacyHashes := nonLeadingMessageFingerprints(proxyMsgs[:len(proxyMsgs)-1])
		if err := st.UpsertConversation(context.Background(), &store.ConversationRecord{
			Key:               "h:legacy-20img",
			ThreadID:          "old-tid-v242",
			AccountID:         1,
			BaseHash:          "v2::",
			MessageHashes:     legacyHashes,
			LastAssistantHash: "",
			LastUsed:          time.Now().Unix(),
		}); err != nil {
			t.Fatalf("UpsertConversation: %v", err)
		}

		cfg := config.Default()
		cfg.SessionReuse = true
		srv := NewServer(&cfg, accounts.New(st, &cfg))
		httpSrv := httptest.NewServer(srv.Handler())
		defer httpSrv.Close()
		client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}

		postAnthropic := func(msgs []map[string]any) (int, string) {
			b, _ := json.Marshal(map[string]any{
				"model": "claude-opus-5", "max_tokens": 256, "stream": false, "messages": msgs,
			})
			req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/messages", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Conversation-Id", "legacy-20img")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("POST /v1/messages: %v", err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(raw)
		}

		// 首轮：升级后接续老记录，强制全量重建 -> 不再报 400，而是成功新建线程并上传 16 张最新历史图
		msgsRound1 := append(append([]map[string]any(nil), rawMsgs...), map[string]any{
			"role": "user", "content": "升级后的第 1 次提问",
		})
		code1, body1 := postAnthropic(msgsRound1)
		if code1 != http.StatusOK {
			t.Fatalf("expected 200 OK on legacy 20-image rebuild, got %d: %s", code1, body1)
		}

		// 验证 DB 已补写 last_assistant_hash
		rec, err := st.GetConversation(context.Background(), "h:legacy-20img")
		if err != nil || rec == nil || rec.LastAssistantHash == "" || rec.ThreadID == "old-tid-v242" {
			t.Fatalf("expected updated conversation with new threadID & last_assistant_hash, got %+v err=%v", rec, err)
		}

		// 第 2 轮：携带刚刚返回的 "重建答复-1" 续聊 -> 直接命中同一个新 thread_id 增量复用
		msgsRound2 := append(append([]map[string]any(nil), msgsRound1...),
			map[string]any{"role": "assistant", "content": "重建答复-1"},
			map[string]any{"role": "user", "content": "续聊测试"},
		)
		code2, body2 := postAnthropic(msgsRound2)
		if code2 != http.StatusOK {
			t.Fatalf("expected 200 OK on round 2 resume, got %d: %s", code2, body2)
		}

		fMu.Lock()
		defer fMu.Unlock()
		if len(frames) != 2 {
			t.Fatalf("expected 2 upstream frames, got %d", len(frames))
		}
		if !frames[0].hasCreate || frames[0].fileCount != maxImagesPerTurn {
			t.Fatalf("round 1 expected session_create + 16 uploaded files, got create=%t files=%d", frames[0].hasCreate, frames[0].fileCount)
		}
		if strings.Count(frames[0].text, "[earlier image omitted]") != 4 {
			t.Fatalf("round 1 expected 4 omitted markers, got %d", strings.Count(frames[0].text, "[earlier image omitted]"))
		}
		if frames[1].hasCreate || frames[1].threadID != frames[0].threadID || frames[1].fileCount != 0 {
			t.Fatalf("round 2 expected incremental reuse on %q with 0 files, got create=%t tid=%q files=%d", frames[0].threadID, frames[1].hasCreate, frames[1].threadID, frames[1].fileCount)
		}
	})
}
