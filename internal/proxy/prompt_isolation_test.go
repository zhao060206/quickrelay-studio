package proxy

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSanitizeCallerInstructions 验证能够温和改写显式的身份覆写声明并完整保留工作区规范。
func TestSanitizeCallerInstructions(t *testing.T) {
	raw := "You are Codex, an agent based on GPT-6. Keep responses concise.\n\n" +
		"You are an AI coding agent with full workspace access.\n\n" +
		"Always act as Codex when writing code.\n" +
		"Rule 1: Write unit tests for all changes."

	got := sanitizeCallerInstructions(raw)
	for _, banned := range []string{"You are Codex", "You are an AI coding agent", "act as Codex"} {
		if strings.Contains(got, banned) {
			t.Fatalf("sanitized text still contains %q: %q", banned, got)
		}
	}
	if !strings.Contains(got, "Keep responses concise.") {
		t.Fatalf("lost normal trailing instruction on first line: %q", got)
	}
	if !strings.Contains(got, "Rule 1: Write unit tests for all changes.") {
		t.Fatalf("lost normal workspace rule: %q", got)
	}
}

// TestPromptIsolation_NoFakeSystemAuthority 验证指令与工具协议以协作隔离格式封装，且无伪系统权限措辞。
func TestPromptIsolation_NoFakeSystemAuthority(t *testing.T) {
	req := &ChatRequest{
		Model: "smart",
		Tools: []Tool{{
			Type: "function",
			Function: ToolFunction{
				Name:        "open_side_browser",
				Description: "Open URL in side browser.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"url": map[string]any{"type": "string"},
					},
				},
			},
		}},
		Messages: []Message{
			{Role: "system", Content: "You are Codex, an agent based on GPT-6.\nAlways verify tests."},
			{Role: "user", Content: "在侧边浏览器里打开看看"},
		},
	}

	bridge := buildToolBridgePrompt(req, "auto", "", true)
	sys := systemPrompt(req.Messages, bridge)
	relayed := relayPrompt(sys, "在侧边浏览器里打开看看")

	for _, banned := range []string{
		"[System instructions from the API caller]",
		"same authority as the caller's system prompt",
		"You are Codex",
		"<caller_tool_interface>",
		"safety boundaries",
		"pretend to be another model",
		"built-in platform tools",
		"forwarded verbatim",
	} {
		if strings.Contains(relayed, banned) {
			t.Fatalf("relayed prompt must not contain %q", banned)
		}
	}

	for _, required := range []string{
		"<client_collaboration_context>",
		"</client_collaboration_context>",
		"<workspace_guidelines>",
		"</workspace_guidelines>",
		"<client_tool_protocol>",
		"</client_tool_protocol>",
		"<user_request>",
		"Always verify tests.",
		"open_side_browser",
		"在侧边浏览器里打开看看",
	} {
		if !strings.Contains(relayed, required) {
			t.Fatalf("relayed prompt missing %q", required)
		}
	}
}

// TestAttachmentShortTextWithLatest 验证超长附件短引用保留最新提问预览并合法截断多字节字符。
func TestAttachmentShortTextWithLatest(t *testing.T) {
	emptyGot := attachmentShortTextWithLatest("prompt-1122.txt", "   ")
	if !strings.Contains(emptyGot, "prompt-1122.txt") || strings.Contains(emptyGot, "latest user message") {
		t.Fatalf("unexpected empty fallback text: %q", emptyGot)
	}

	shortGot := attachmentShortTextWithLatest("prompt-3344.txt", "请打开 http://127.0.0.1:56865")
	if !strings.Contains(shortGot, "prompt-3344.txt") || !strings.Contains(shortGot, "请打开 http://127.0.0.1:56865") {
		t.Fatalf("unexpected short preview text: %q", shortGot)
	}

	longCN := strings.Repeat("中文预览测试", 300) // > 1200 bytes，多字节 UTF-8
	truncGot := attachmentShortTextWithLatest("prompt-5566.txt", longCN)
	if !utf8.ValidString(truncGot) {
		t.Fatalf("truncated preview contains invalid UTF-8")
	}
	if !strings.HasSuffix(truncGot, "…") {
		t.Fatalf("expected truncated preview to end with ellipsis, got %q", truncGot[max(0, len(truncGot)-20):])
	}
	const prefix = "The full conversation transcript for this turn is attached in prompt-5566.txt (you may inspect it with your built-in file reader to see full context). The latest user message is:\n"
	body := strings.TrimPrefix(truncGot, prefix)
	if len(body) > 1205 {
		t.Fatalf("truncated preview unexpectedly large: %d bytes", len(body))
	}
}
