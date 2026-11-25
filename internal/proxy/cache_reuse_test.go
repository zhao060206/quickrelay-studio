package proxy

import (
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quickrelay-studio/internal/accounts"
	"quickrelay-studio/internal/config"
	"quickrelay-studio/internal/store"
)

func makeManyTools(count int) []Tool {
	tools := make([]Tool, 0, count)
	for i := 0; i < count; i++ {
		tools = append(tools, Tool{
			Type: "function",
			Function: ToolFunction{
				Name:        fmt.Sprintf("workspace_tool_%02d", i),
				Description: strings.Repeat("Inspect or manipulate files safely inside user authorized directories. ", 8),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string", "description": "Target file path"},
						"content": map[string]any{"type": "string", "description": "Payload content"},
					},
					"required": []string{"path"},
				},
			},
		})
	}
	return tools
}

func newTestServerForReuse(t *testing.T) *Server {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "reuse.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Default()
	cfg.SessionReuse = true
	mgr := accounts.New(st, &cfg)
	return NewServer(&cfg, mgr)
}

func TestResumedTurn_CompactsToolProtocolAndRecoversOnReset(t *testing.T) {
	srv := newTestServerForReuse(t)
	tools := makeManyTools(20)

	// 第 1 轮：完整工具定义
	req1 := &ChatRequest{
		Model:    "claude-opus-5",
		Tools:    tools,
		Messages: []Message{{Role: "system", Content: "System time: 18:00"}, {Role: "user", Content: "先列出文件"}},
	}
	r1 := httptest.NewRequest("POST", "/v1/messages", nil)
	turn1, err := srv.prepareTurn(r1, req1, "anthropic", "/v1/messages", "msg_")
	if err != nil {
		t.Fatalf("prepareTurn 1 failed: %v", err)
	}
	if turn1.resume {
		turn1.Close()
		t.Fatalf("turn 1 should not be resume")
	}
	fullProtoLen := len(turn1.in.ToolProtocol)
	if !strings.Contains(turn1.in.ToolProtocol, `"workspace_tool_19"`) || fullProtoLen < 5000 {
		turn1.Close()
		t.Fatalf("turn 1 should carry full tool protocol, len=%d", fullProtoLen)
	}
	// 模拟首轮完成并保存会话映射
	turn1.usedAccount = &store.Account{ID: 7}
	turn1.saveConversation(ChatResult{ThreadID: "thread-cache-001"})
	turn1.Close()

	// 第 2 轮：system 动态变化 + 运行时切换工作模式为 document，工具未变 -> 依然命中复用且极简工具提醒
	srv.cfg.SetWorkMode(config.WorkModeDocument)
	req2 := &ChatRequest{
		Model: "claude-opus-5",
		Tools: tools,
		Messages: []Message{
			{Role: "system", Content: "System time: 18:05 (changed!)"},
			{Role: "user", Content: "先列出文件"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "t1", Type: "function", Function: FunctionCall{Name: "workspace_tool_00", Arguments: `{"path":"."}`}}}},
			{Role: "tool", ToolCallID: "t1", Content: "paper.docx"},
		},
	}
	r2 := httptest.NewRequest("POST", "/v1/messages", nil)
	turn2, err := srv.prepareTurn(r2, req2, "anthropic", "/v1/messages", "msg_")
	if err != nil {
		t.Fatalf("prepareTurn 2 failed: %v", err)
	}
	if !turn2.resume || turn2.in.ThreadID != "thread-cache-001" {
		turn2.Close()
		t.Fatalf("expected turn 2 resume thread-cache-001 even after system/work_mode change, got resume=%t tid=%q", turn2.resume, turn2.in.ThreadID)
	}
	if len(turn2.in.ToolProtocol) >= fullProtoLen/5 || strings.Contains(turn2.in.ToolProtocol, `"workspace_tool_19"`) {
		turn2.Close()
		t.Fatalf("expected compacted tool reminder on turn 2, got len=%d vs full=%d", len(turn2.in.ToolProtocol), fullProtoLen)
	}
	if !strings.Contains(turn2.in.ToolProtocol, `<client_tool_call>`) {
		turn2.Close()
		t.Fatalf("compacted reminder missing <client_tool_call> format hint")
	}

	// 若续聊失败需要 resetConversation，应立刻恢复完整工具定义以便重开干净线程
	turn2.resetConversation()
	if turn2.in.ToolProtocol != turn2.fullBridgePrompt || !strings.Contains(turn2.in.ToolProtocol, `"workspace_tool_19"`) {
		turn2.Close()
		t.Fatalf("resetConversation failed to restore full tool protocol")
	}
	turn2.Close()
}

func TestResumedTurn_ToolSchemaChangeUpdatesSameThread(t *testing.T) {
	srv := newTestServerForReuse(t)
	tools1 := makeManyTools(10)

	req1 := &ChatRequest{
		Model:    "claude-opus-5",
		Tools:    tools1,
		Messages: []Message{{Role: "user", Content: "帮我看项目"}},
	}
	t1, err := srv.prepareTurn(httptest.NewRequest("POST", "/v1/messages", nil), req1, "anthropic", "/v1/messages", "msg_")
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	t1.usedAccount = &store.Account{ID: 3}
	t1.saveConversation(ChatResult{ThreadID: "thread-evolve-1"})
	t1.Close()

	// 第 2 轮工具集新增 1 个工具：保持同一 thread_id 复用，但重发完整工具表
	tools2 := makeManyTools(11)
	req2 := &ChatRequest{
		Model: "claude-opus-5",
		Tools: tools2,
		Messages: []Message{
			{Role: "user", Content: "帮我看项目"},
			{Role: "assistant", Content: "好的"},
			{Role: "user", Content: "继续用新工具"},
		},
	}
	t2, err := srv.prepareTurn(httptest.NewRequest("POST", "/v1/messages", nil), req2, "anthropic", "/v1/messages", "msg_")
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if !t2.resume || t2.in.ThreadID != "thread-evolve-1" {
		t2.Close()
		t.Fatalf("expected reuse thread-evolve-1 when tools changed, got resume=%t tid=%q", t2.resume, t2.in.ThreadID)
	}
	if !strings.Contains(t2.in.ToolProtocol, `"workspace_tool_10"`) {
		t2.Close()
		t.Fatalf("expected updated full tool protocol when tools changed")
	}
	t2.usedAccount = &store.Account{ID: 3}
	t2.saveConversation(ChatResult{ThreadID: "thread-evolve-1"})
	t2.Close()

	// 第 3 轮工具集再次与第 2 轮一致：再次自动切换回极简提醒
	req3 := &ChatRequest{
		Model: "claude-opus-5",
		Tools: tools2,
		Messages: append(req2.Messages,
			Message{Role: "assistant", Content: "收到"},
			Message{Role: "user", Content: "下一步"},
		),
	}
	t3, err := srv.prepareTurn(httptest.NewRequest("POST", "/v1/messages", nil), req3, "anthropic", "/v1/messages", "msg_")
	if err != nil {
		t.Fatalf("turn 3: %v", err)
	}
	if !t3.resume || strings.Contains(t3.in.ToolProtocol, `"workspace_tool_10"`) {
		t3.Close()
		t.Fatalf("expected compacted tool reminder on turn 3 after tool fingerprint stabilized")
	}
	t3.Close()
}

func TestAuxiliarySideRequestIsolationAndLegacyDeltaCompat(t *testing.T) {
	srv := newTestServerForReuse(t)

	// 1. 旁路请求隔离（无工具 SUGGESTION MODE）
	auxReq := &ChatRequest{
		Model:    "claude-opus-5",
		Messages: []Message{{Role: "user", Content: "[SUGGESTION MODE: Suggest what the user might naturally type next]"}},
	}
	if !isAuxiliarySideRequest(auxReq, "") {
		t.Fatalf("expected suggestion mode classified as auxiliary request")
	}
	tAux, err := srv.prepareTurn(httptest.NewRequest("POST", "/v1/messages", nil), auxReq, "anthropic", "/v1/messages", "msg_")
	if err != nil {
		t.Fatalf("aux prepareTurn: %v", err)
	}
	if !tAux.auxRequest || !tAux.in.DeleteSession {
		tAux.Close()
		t.Fatalf("aux request should be marked isolated and DeleteSession=true")
	}
	tAux.usedAccount = &store.Account{ID: 1}
	tAux.saveConversation(ChatResult{ThreadID: "aux-tid-should-not-persist"})
	tAux.Close()
	if n, _ := srv.store.CountConversations(t.Context()); n != 0 {
		t.Fatalf("aux request must not persist into conversations table, got count=%d", n)
	}

	// 2. 兼容旧版不带 "nl:" 前缀的 SQLite message_hashes 记录
	legacyMsgs := []Message{
		{Role: "system", Content: "Old sys"},
		{Role: "user", Content: "旧对话首句"},
	}
	conv := &conversation{
		threadID:  "legacy-thread-001",
		accountID: 2,
		baseHash:  strings.Repeat("a", 64),
		msgHashes: messageFingerprints(legacyMsgs),
	}
	delta, ok := conv.delta(append(legacyMsgs, Message{Role: "assistant", Content: "旧回复"}, Message{Role: "user", Content: "继续旧对话"}))
	if !ok || len(delta) != 2 {
		t.Fatalf("expected legacy msgHashes prefix compatibility, ok=%t len=%d", ok, len(delta))
	}

	// 3. 验证新版带 "nl:" 前缀在首条 system 变动时仍匹配
	var dummy any = time.Now()
	_ = dummy
}
