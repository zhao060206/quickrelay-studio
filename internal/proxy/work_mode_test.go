package proxy

import (
	"strings"
	"testing"

	"quickrelay-studio/internal/config"
)

func TestWorkModes_CollaborationAndToolBridgePrompts(t *testing.T) {
	req := &ChatRequest{
		Model: "smart",
		Tools: []Tool{{
			Type: "function",
			Function: ToolFunction{
				Name:        "read_file",
				Description: "Read a file from disk.",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
				},
			},
		}},
		Messages: []Message{
			{Role: "system", Content: "You are Claude Code. Help review the paper."},
			{Role: "user", Content: "请审阅我的 Word 综述论文初稿"},
		},
	}

	// 1. document 模式去除了编程词汇与命令偏见
	docBridge := buildToolBridgePromptForMode(req, "auto", "", true, config.WorkModeDocument)
	docGuide := callerGuidelinesForMode(req.Messages, config.WorkModeDocument)
	docPrompt := assembleRelayPromptForMode(config.WorkModeDocument, docGuide, docBridge, "请审阅我的 Word 综述论文初稿")

	for _, forbidden := range []string{"Three.js", "shaders", "exec_command", "apply_patch", "local coding workspace"} {
		if strings.Contains(docPrompt, forbidden) {
			t.Fatalf("document mode prompt must not contain %q", forbidden)
		}
	}
	for _, required := range []string{"document app", "remote Quick spaces", "<client_tool_call>", "Document and workspace actions"} {
		if !strings.Contains(docPrompt, required) {
			t.Fatalf("document mode prompt missing %q", required)
		}
	}

	// 2. general 模式保持中立，也不含 Three.js / shaders / local coding workspace
	genBridge := buildToolBridgePromptForMode(req, "auto", "", true, config.WorkModeGeneral)
	genGuide := callerGuidelinesForMode(req.Messages, config.WorkModeGeneral)
	genPrompt := assembleRelayPromptForMode(config.WorkModeGeneral, genGuide, genBridge, "帮我整理资料")
	for _, forbidden := range []string{"Three.js", "shaders", "exec_command", "local coding workspace"} {
		if strings.Contains(genPrompt, forbidden) {
			t.Fatalf("general mode prompt must not contain %q", forbidden)
		}
	}

	// 3. coding 模式完整保留代码工程脚手架指导
	codeBridge := buildToolBridgePromptForMode(req, "auto", "", true, config.WorkModeCoding)
	codeGuide := callerGuidelinesForMode(req.Messages, config.WorkModeCoding)
	codePrompt := assembleRelayPromptForMode(config.WorkModeCoding, codeGuide, codeBridge, "创建页面")
	for _, required := range []string{"Three.js", "shaders", "exec_command", "local coding workspace"} {
		if !strings.Contains(codePrompt, required) {
			t.Fatalf("coding mode prompt missing %q", required)
		}
	}
}

func TestApplyWorkModeGuards_DocumentIcebreak(t *testing.T) {
	// 首轮无工具回传历史：document 模式把 high/max 破冰降为 medium
	if eff, reason := applyWorkModeGuards("high", false, false, true, "auto", config.WorkModeDocument, false); eff != "medium" || reason != "document_first_turn_icebreak" {
		t.Fatalf("document first turn high got (%q, %q), want (medium, document_first_turn_icebreak)", eff, reason)
	}
	if eff, reason := applyWorkModeGuards("max", false, false, true, "auto", config.WorkModeDocument, false); eff != "medium" || reason != "document_first_turn_icebreak" {
		t.Fatalf("document first turn max got (%q, %q), want (medium, document_first_turn_icebreak)", eff, reason)
	}

	// 已有工具回传后：保留原本 high/max
	if eff, reason := applyWorkModeGuards("max", false, false, true, "auto", config.WorkModeDocument, true); eff != "max" || reason != "" {
		t.Fatalf("document with tool history got (%q, %q), want (max, \"\")", eff, reason)
	}

	// general / coding 模式不做首轮破冰降档
	if eff, reason := applyWorkModeGuards("max", false, false, true, "auto", config.WorkModeCoding, false); eff != "max" || reason != "" {
		t.Fatalf("coding first turn got (%q, %q), want (max, \"\")", eff, reason)
	}
	if eff, reason := applyWorkModeGuards("high", false, false, true, "auto", config.WorkModeGeneral, false); eff != "high" || reason != "" {
		t.Fatalf("general first turn got (%q, %q), want (high, \"\")", eff, reason)
	}

	// hasToolResultHistory 验证
	if hasToolResultHistory([]Message{{Role: "user", Content: "hi"}}) {
		t.Fatalf("unexpected tool history on pure user message")
	}
	if !hasToolResultHistory([]Message{{Role: "user", Content: "hi"}, {Role: "tool", Content: "ok"}}) {
		t.Fatalf("expected tool history when role=tool exists")
	}
}

func TestDocumentMode_AvoidsGuidelinesAttachment(t *testing.T) {
	envHeader := "<env>\nPrimary working directory: C:\\Users\\demo\\Desktop\\Papers\nPlatform: win32\n</env>\n"
	hugeGuide := envHeader + strings.Repeat("Extra plugin instruction line that bloats guidelines payload.\n", 2300)
	toolProtocol := buildToolBridgePromptForMode(&ChatRequest{
		Tools: []Tool{{Type: "function", Function: ToolFunction{Name: "Read"}}},
	}, "auto", "", true, config.WorkModeDocument)

	payload := map[string]any{
		"text": renderTurnTextForMode(config.WorkModeDocument, hugeGuide, toolProtocol, "", "请审阅三篇论文"),
	}

	guidelinesUploaded := false
	prepareFrameWithinBudgetWithMode(payload, "msg-doc-001", hugeGuide, toolProtocol, "", "请审阅三篇论文", "请审阅三篇论文", nil, nil, true, config.WorkModeDocument,
		func(name string, data []byte) (attachmentRef, error) {
			if strings.HasPrefix(name, attachmentGuidelinesPrefix) {
				guidelinesUploaded = true
			}
			return attachmentRef{Name: name, Path: "/tmp/" + name, Size: len(data)}, nil
		})

	if guidelinesUploaded {
		t.Fatalf("document mode must NOT upload guidelines-*.txt attachment")
	}
	text, _ := payload["text"].(string)
	if !strings.Contains(text, `Primary working directory: C:\Users\demo\Desktop\Papers`) {
		t.Fatalf("document mode should retain working directory summary inline, got: %s", text)
	}
	if !strings.Contains(text, toolProtocol) {
		t.Fatalf("document mode lost toolProtocol")
	}
	if n, err := userMessageBytes(payload); err != nil || n > maxUserMessageBytes {
		t.Fatalf("document mode payload exceeded maxUserMessageBytes: n=%d err=%v", n, err)
	}
}

func TestConfigWorkModeLifecycle(t *testing.T) {
	if err := config.ValidateWorkMode("document"); err != nil {
		t.Fatalf("ValidateWorkMode(document) err: %v", err)
	}
	if err := config.ValidateWorkMode("invalid"); err == nil {
		t.Fatalf("expected error on invalid work_mode")
	}
	cfg := config.Default()
	if got := cfg.GetWorkMode(); got != config.WorkModeGeneral {
		t.Fatalf("default GetWorkMode() = %q, want general", got)
	}
	cfg.SetWorkMode("document")
	if got := cfg.GetWorkMode(); got != config.WorkModeDocument {
		t.Fatalf("after SetWorkMode(document) = %q", got)
	}
}
