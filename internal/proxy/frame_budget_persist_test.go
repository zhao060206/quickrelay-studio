package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"quickrelay-studio/internal/config"
	"quickrelay-studio/internal/store"
)

// TestForwardToolDefinitions_CompactJSON 验证工具定义序列化采用紧凑 JSON，无多行缩进膨胀。
func TestForwardToolDefinitions_CompactJSON(t *testing.T) {
	req := &ChatRequest{
		ToolChoice: "auto",
		Tools: []Tool{
			{
				Type: "function",
				Function: ToolFunction{
					Name:        "exec_command",
					Description: "Run shell command in local workspace.",
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"cmd":     map[string]any{"type": "string"},
							"workdir": map[string]any{"type": "string"},
						},
						"required": []string{"cmd"},
					},
				},
			},
		},
	}

	got := forwardToolDefinitions(req)
	if strings.Contains(got, "\n  \"") {
		t.Fatalf("expected compact JSON without multi-line indentation, got: %q", got)
	}
	if !strings.Contains(got, `"exec_command"`) || !strings.Contains(got, `"tool_choice":"auto"`) {
		t.Fatalf("compact tool JSON missing expected fields: %q", got)
	}
}

// TestCompactSkillsSection_PreservesNamesAndSavesSpace 验证技能列表压缩保留全部技能名与路径且大幅节省字节。
func TestCompactSkillsSection_PreservesNamesAndSavesSpace(t *testing.T) {
	var raw strings.Builder
	raw.WriteString("<skills_instructions>\n- `r0` = `/tmp/skills`\n### Available skills\n")
	for i := range 50 {
		raw.WriteString(fmt.Sprintf(
			"- skill-%02d: Use this comprehensive workflow when the user asks to inspect, refactor, validate, bundle, or troubleshoot component %02d across multiple repositories and runtime environments. Do not invoke unnecessarily for trivial greetings or unrelated prose tasks. (file: r0/skill-%02d/SKILL.md)\n",
			i, i, i,
		))
	}
	raw.WriteString("</skills_instructions>\n## Repository Rules\nAlways preserve existing tests.")

	orig := raw.String()
	compacted := compactSkillsSection(orig)
	if len(compacted) >= len(orig)/2 {
		t.Fatalf("expected >50%% byte reduction for long skills section, orig=%d compacted=%d", len(orig), len(compacted))
	}
	for i := range 50 {
		name := fmt.Sprintf("- skill-%02d:", i)
		fileRef := fmt.Sprintf("(file: r0/skill-%02d/SKILL.md)", i)
		if !strings.Contains(compacted, name) {
			t.Fatalf("compacted skills missing %q", name)
		}
		if !strings.Contains(compacted, fileRef) {
			t.Fatalf("compacted skills missing %q", fileRef)
		}
	}
	if !strings.Contains(compacted, "## Repository Rules\nAlways preserve existing tests.") {
		t.Fatalf("compacted skills altered trailing repository rules")
	}
}

// buildLargeTestToolReq 构造接近真实 Codex 规模的 20 个工具请求。
func buildLargeTestToolReq() *ChatRequest {
	tools := make([]Tool, 0, 20)
	for i := range 20 {
		name := fmt.Sprintf("tool_%02d", i)
		if i == 0 {
			name = "exec_command"
		}
		if i == 19 {
			name = "apply_patch"
		}
		tools = append(tools, Tool{
			Type: "function",
			Function: ToolFunction{
				Name:        name,
				Description: strings.Repeat(fmt.Sprintf("Perform workspace operation %d cleanly. ", i), 12),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"cmd":     map[string]any{"type": "string", "description": "Argument payload"},
						"workdir": map[string]any{"type": "string", "description": "Target directory"},
					},
					"required": []string{"cmd"},
				},
			},
		})
	}
	return &ChatRequest{Tools: tools, ToolChoice: "auto"}
}

// TestPrepareFrameWithinBudget_ProtectsToolProtocol 验证超 63 KB 时规范转为附件而工具协议 100% 完整保留。
func TestPrepareFrameWithinBudget_ProtectsToolProtocol(t *testing.T) {
	req := buildLargeTestToolReq()
	toolProtocol := buildToolBridgePrompt(req, "auto", "", true)
	if !strings.Contains(toolProtocol, `"exec_command"`) || !strings.Contains(toolProtocol, `"apply_patch"`) {
		t.Fatalf("toolProtocol did not include expected tools")
	}

	// 场景 A：skills 压缩后即落入预算内，不上传规范附件
	var skillsOnly strings.Builder
	skillsOnly.WriteString("### Available skills\n")
	for i := range 120 {
		skillsOnly.WriteString(fmt.Sprintf("- s%03d: %s (file: r1/s%03d/SKILL.md)\n",
			i, strings.Repeat("Detailed trigger rule for automated coding task execution. ", 7), i))
	}
	guideSkills := wrapWorkspaceGuidelines([]string{skillsOnly.String()})
	payloadA := map[string]any{
		"text":      assembleRelayPrompt(guideSkills, toolProtocol, "请写一个测试脚本"),
		"thread_id": "t-a",
	}
	uploadsA := 0
	prepareFrameWithinBudget(payloadA, "msg-aaaaaaaa", guideSkills, toolProtocol, "", "请写一个测试脚本", "请写一个测试脚本", nil, nil,
		func(name string, data []byte) (attachmentRef, error) {
			uploadsA++
			return attachmentRef{Name: name, Size: len(data), Path: "qw-file://workspace/" + name, ID: "id-" + name}, nil
		})
	if uploadsA != 0 {
		t.Fatalf("expected skills compaction alone to fit without uploading guidelines, uploads=%d", uploadsA)
	}
	if n, err := userMessageBytes(payloadA); err != nil || n > maxUserMessageBytes {
		t.Fatalf("payloadA exceeded limit after compaction: n=%d err=%v", n, err)
	}
	if !strings.Contains(payloadA["text"].(string), `"exec_command"`) {
		t.Fatalf("payloadA lost exec_command")
	}

	// 场景 B：即使压缩后仍超 63 KB（大块项目规范 >65 KB），自动上传完整原始规范为 guidelines-<id8>.txt 并全量保留工具表
	hugeGuide := wrapWorkspaceGuidelines([]string{
		skillsOnly.String() + "\n## AGENTS.md\n" + strings.Repeat("Rule line for project compliance and verification.\n", 1400),
	})
	payloadB := map[string]any{
		"text":      assembleRelayPrompt(hugeGuide, toolProtocol, "请在本机 E:\\codex\\绘图 生成页面"),
		"thread_id": "t-b",
	}
	var uploadedName string
	var uploadedBody string
	prepareFrameWithinBudget(payloadB, "msg-12345678", hugeGuide, toolProtocol, "", "请在本机 E:\\codex\\绘图 生成页面", "请在本机 E:\\codex\\绘图 生成页面", nil, nil,
		func(name string, data []byte) (attachmentRef, error) {
			uploadedName = name
			uploadedBody = string(data)
			return attachmentRef{Name: name, Size: len(data), Path: "qw-file://workspace/" + name, ID: "att-1"}, nil
		})

	if uploadedName != "guidelines-msg12345.txt" {
		t.Fatalf("unexpected guidelines attachment name: %q", uploadedName)
	}
	if uploadedBody != hugeGuide {
		t.Fatalf("uploaded guidelines must retain exact uncompacted full text")
	}
	textB := payloadB["text"].(string)
	for _, mustKeep := range []string{
		"guidelines-msg12345.txt",
		"<client_tool_protocol>",
		"</client_tool_protocol>",
		`"exec_command"`,
		`"apply_patch"`,
		"请在本机 E:\\codex\\绘图 生成页面",
	} {
		if !strings.Contains(textB, mustKeep) {
			t.Fatalf("payloadB main frame lost required fragment %q", mustKeep)
		}
	}
	if err := fitUserMessage(payloadB); err != nil {
		t.Fatalf("fitUserMessage failed after guidelines externalization: %v", err)
	}
	if n, err := userMessageBytes(payloadB); err != nil || n > maxUserMessageBytes {
		t.Fatalf("payloadB still over limit: n=%d err=%v", n, err)
	}
}

// TestTrimWorkspaceGuidelinesOnly_FallbackSafety 验证即便没有附件通道，兜底裁剪仍只削减规范而保全工具协议。
func TestTrimWorkspaceGuidelinesOnly_FallbackSafety(t *testing.T) {
	req := buildLargeTestToolReq()
	toolProtocol := buildToolBridgePrompt(req, "auto", "", true)
	hugeGuide := wrapWorkspaceGuidelines([]string{strings.Repeat("Guideline line for codebase style.\n", 2600)})
	text := assembleRelayPrompt(hugeGuide, toolProtocol, "只看工具协议是否在")
	payload := map[string]any{"text": text}

	if err := fitUserMessage(payload); err != nil {
		t.Fatalf("fitUserMessage fallback failed: %v", err)
	}
	got := payload["text"].(string)
	for _, mustKeep := range []string{`<client_tool_protocol>`, `"exec_command"`, `"apply_patch"`, `</client_tool_protocol>`, `只看工具协议是否在`} {
		if !strings.Contains(got, mustKeep) {
			t.Fatalf("fallback trim lost %q", mustKeep)
		}
	}
}

// TestConversationPersistenceAcrossRestart 验证重建全新 conversationStore（模拟进程重启）后仍能从 SQLite 恢复会话与前缀差分。
func TestConversationPersistenceAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "persist.db")
	db1, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open db1: %v", err)
	}

	msgs1 := []Message{
		{Role: "developer", Content: "Project rules:\nAlways respond accurately."},
		{Role: "user", Content: "请记住这个暗号：ORION-9472。只需回复确认。"},
	}
	req1 := &ChatRequest{Model: "aws-quick-smart", Messages: msgs1}
	lead1 := leadingGuidelines(msgs1)
	baseHash1 := conversationBaseHash(lead1, "")

	r1 := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r1.Header.Set("Authorization", "Bearer test-key")
	key1 := conversationKey(r1, req1, baseHash1)

	s1 := newConversationStore(24*time.Hour, 64, db1)
	c1 := s1.acquire(key1)
	s1.save(c1, "thread-persist-001", 4, baseHash1, msgs1)
	s1.release(c1)
	_ = db1.Close()

	// 模拟进程重启：重新打开 SQLite 并使用空内存缓存创建 s2
	db2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open db2: %v", err)
	}
	defer db2.Close()

	msgs2 := []Message{
		{Role: "developer", Content: "Project rules:\nAlways respond accurately."},
		{Role: "user", Content: "请记住这个暗号：ORION-9472。只需回复确认。"},
		{Role: "assistant", Content: "已记住，暗号是 ORION-9472。"},
		{Role: "developer", Content: "Current time context: 2026-11-23T21:45:00+08:00"},
		{Role: "user", Content: "我上一轮让你记住的暗号是什么？"},
	}
	req2 := &ChatRequest{Model: "aws-quick-smart", Messages: msgs2}
	lead2 := leadingGuidelines(msgs2)
	baseHash2 := conversationBaseHash(lead2, "")
	if baseHash2 != baseHash1 {
		t.Fatalf("trailing developer time context unexpectedly changed baseHash")
	}

	r2 := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r2.Header.Set("Authorization", "Bearer test-key")
	key2 := conversationKey(r2, req2, baseHash2)
	if key2 != key1 {
		t.Fatalf("conversationKey drifted after trailing developer update: %q vs %q", key2, key1)
	}

	s2 := newConversationStore(24*time.Hour, 64, db2)
	c2 := s2.acquire(key2)

	if c2.threadID != "thread-persist-001" || c2.accountID != 4 {
		s2.release(c2)
		t.Fatalf("failed to restore session from SQLite after restart: thread=%q account=%d", c2.threadID, c2.accountID)
	}
	delta, ok := c2.delta(msgs2)
	if !ok {
		s2.release(c2)
		t.Fatalf("expected prefix delta match after restart")
	}
	if len(delta) != 3 {
		s2.release(c2)
		t.Fatalf("expected 3 delta messages, got %d", len(delta))
	}
	deltaGuide := deltaDeveloperGuidelines(delta)
	if !strings.Contains(deltaGuide, "Current time context") {
		s2.release(c2)
		t.Fatalf("expected deltaDeveloperGuidelines to capture new developer message: %q", deltaGuide)
	}
	if strings.Contains(deltaGuide, "Always respond accurately.") {
		s2.release(c2)
		t.Fatalf("deltaDeveloperGuidelines must not re-include static leading guidelines: %q", deltaGuide)
	}

	// 验证重置后会清除持久化记录
	s2.reset(c2)
	s2.release(c2)

	c2AfterReset := s2.acquire(key2)
	if c2AfterReset.threadID != "" {
		s2.release(c2AfterReset)
		t.Fatalf("expected reset to clear in-memory threadID")
	}
	s2.release(c2AfterReset)

	s3 := newConversationStore(24*time.Hour, 64, db2)
	c3 := s3.acquire(key2)
	defer s3.release(c3)
	if c3.threadID != "" {
		t.Fatalf("expected reset to remove persisted record from SQLite, got %q", c3.threadID)
	}
}

// TestCompactSkillsIndexOnly_FurtherReducesSize 验证二档纯索引压缩保留技能名与路径且字节数严格低于常规压缩。
func TestCompactSkillsIndexOnly_FurtherReducesSize(t *testing.T) {
	var raw strings.Builder
	raw.WriteString("### Available skills\n")
	for i := range 60 {
		raw.WriteString(fmt.Sprintf(
			"- skill-%02d: Comprehensive description about workflow automation, diagnostics, verification and reporting for case %02d. (file: r1/skill-%02d/SKILL.md)\n",
			i, i, i,
		))
	}
	raw.WriteString("</skills_instructions>")

	standard := compactSkillsSection(raw.String())
	indexOnly := compactSkillsIndexOnly(raw.String())
	if len(indexOnly) >= len(standard) {
		t.Fatalf("expected index-only compaction to be smaller than standard compaction: index=%d standard=%d", len(indexOnly), len(standard))
	}
	for i := range 60 {
		prefix := fmt.Sprintf("- skill-%02d: (file: r1/skill-%02d/SKILL.md)", i, i)
		if !strings.Contains(indexOnly, prefix) {
			t.Fatalf("index-only output missing %q", prefix)
		}
	}
	if !strings.Contains(indexOnly, "</skills_instructions>") {
		t.Fatalf("index-only output lost closing tag")
	}
}

// TestLatestTurnPreview_PrefersTrailingToolResult 验证末尾为工具执行结果时内联预览优先展示工具输出而非上一轮提问。
func TestLatestTurnPreview_PrefersTrailingToolResult(t *testing.T) {
	msgs := []Message{
		{Role: "developer", Content: "Workspace rules"},
		{Role: "user", Content: "请检查仓库状态"},
		{
			Role: "assistant",
			ToolCalls: []ToolCall{{
				ID:       "call_101",
				Type:     "function",
				Function: FunctionCall{Name: "exec_command", Arguments: `{"cmd":"git status"}`},
			}},
		},
		{
			Role:       "tool",
			ToolCallID: "call_101",
			Content:    "On branch main\nnothing to commit",
		},
	}

	preview := latestTurnPreview(msgs)
	if !strings.Contains(preview, "Tool result (exec_command):") || !strings.Contains(preview, "On branch main") {
		t.Fatalf("expected trailing tool result in preview, got: %q", preview)
	}
	if strings.Contains(preview, "请检查仓库状态") {
		t.Fatalf("preview should prefer trailing tool output instead of earlier user text: %q", preview)
	}

	short := attachmentShortTextWithLatest("prompt-tool1234.txt", preview)
	if !strings.Contains(short, "The latest client tool result(s) are:") || !strings.Contains(short, "On branch main") {
		t.Fatalf("unexpected attachment short text for tool result: %q", short)
	}
}

// TestReadEvents_ReturnsContextTimeoutNotClosedConn 验证因超时触发连接关闭时返回 context.DeadlineExceeded 而非连接已关闭错误。
func TestReadEvents_ReturnsContextTimeoutNotClosedConn(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// 持续消费客户端帧但不返回 turn_completed，模拟上游长耗时直到客户端超时
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
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

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	_, err := u.Chat(ctx, sess, ChatInput{
		Prompt:        "test timeout error mapping",
		Model:         "smart",
		CreateSession: true,
	}, nil)
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got: %v", err)
	}
	if strings.Contains(err.Error(), "use of closed network connection") {
		t.Fatalf("unexpected raw socket close error surfaced to caller: %v", err)
	}
}

// TestToolBridgePrompt_IncludesIncrementalStepGuidance 验证协议中注入了防 8-9 分钟熔断的分步增量落盘规则且无禁忌词。
func TestToolBridgePrompt_IncludesIncrementalStepGuidance(t *testing.T) {
	req := &ChatRequest{
		Model: "smart",
		Tools: []Tool{{
			Type: "function",
			Function: ToolFunction{
				Name:        "exec_command",
				Description: "Run shell command locally.",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{"cmd": map[string]any{"type": "string"}},
					"required":   []string{"cmd"},
				},
			},
		}},
		Messages: []Message{
			{Role: "system", Content: "Always verify output."},
			{Role: "user", Content: "用three.js写一个夕阳沙滩场景"},
		},
	}

	bridge := buildToolBridgePrompt(req, "auto", "", true)
	for _, want := range []string{"incremental", "~150-300 lines", "subsequent tool turns"} {
		if !strings.Contains(bridge, want) {
			t.Fatalf("buildToolBridgePrompt missing incremental guidance %q: %s", want, bridge)
		}
	}

	relayed := assembleRelayPrompt(callerGuidelines(req.Messages), bridge, "用three.js写一个夕阳沙滩场景")
	if !strings.Contains(relayed, "work incrementally across multiple concise tool calls") {
		t.Fatalf("clientCollaborationIntro missing incremental cue: %s", relayed)
	}
	for _, banned := range []string{
		"safety boundaries",
		"pretend to be another model",
		"built-in platform tools",
		"forwarded verbatim",
		"same authority as the caller's system prompt",
		"You are Codex",
		"[System instructions from the API caller]",
		"<caller_tool_interface>",
	} {
		if strings.Contains(relayed, banned) {
			t.Fatalf("relayed prompt must not contain %q", banned)
		}
	}
}
