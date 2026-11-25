package proxy

import (
	"fmt"
	"strings"
	"testing"

	"quickrelay-studio/internal/config"
)

func boolRef(v bool) *bool {
	return &v
}

func TestIsCompactionRequest(t *testing.T) {
	cases := []struct {
		name string
		req  *ChatRequest
		want bool
	}{
		{
			name: "Claude Code CRITICAL TEXT ONLY 指令",
			req: &ChatRequest{
				Messages: []Message{
					{Role: "user", Content: "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\nYour task is to create a detailed summary of the conversation so far."},
				},
			},
			want: true,
		},
		{
			name: "Claude Code analysis 与 summary 标签摘要",
			req: &ChatRequest{
				Messages: []Message{
					{Role: "system", Content: "Summarize the conversation cleanly."},
					{Role: "user", Content: "Please produce <analysis>...</analysis> and <summary>...</summary>."},
				},
			},
			want: true,
		},
		{
			name: "无工具 compaction summary 请求",
			req: &ChatRequest{
				Messages: []Message{
					{Role: "user", Content: "Please generate a compaction summary for earlier turns."},
				},
			},
			want: true,
		},
		{
			name: "普通编程请求不误判",
			req: &ChatRequest{
				Tools: []Tool{{Type: "function", Function: ToolFunction{Name: "Read"}}},
				Messages: []Message{
					{Role: "system", Content: "You are a helpful coding assistant."},
					{Role: "user", Content: "请帮我检查 e:\\project\\main.go 中的 summary 字段定义。"},
				},
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCompactionRequest(tc.req, ""); got != tc.want {
				t.Fatalf("isCompactionRequest() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestApplyThinkingGuards(t *testing.T) {
	cfg := config.Default()

	// 1. /compact + max -> low
	if eff, reason := applyThinkingGuards("max", false, true, true, "auto", &cfg); eff != "low" || reason != "compact_downgrade" {
		t.Fatalf("compact+max got (%q, %q), want (low, compact_downgrade)", eff, reason)
	}
	// 2. /compact + "" -> low
	if eff, reason := applyThinkingGuards("", false, true, false, "", &cfg); eff != "low" || reason != "compact_downgrade" {
		t.Fatalf("compact+empty got (%q, %q), want (low, compact_downgrade)", eff, reason)
	}
	// 3. /compact + explicit off -> 保持 ""
	if eff, reason := applyThinkingGuards("", true, true, false, "", &cfg); eff != "" || reason != "" {
		t.Fatalf("compact+explicitOff got (%q, %q), want (\"\", \"\")", eff, reason)
	}
	// 4. 关闭 compact_downgrade 后保持原档位
	cfgNoCompact := config.Default()
	cfgNoCompact.Guardrails.CompactDowngrade = boolRef(false)
	if eff, reason := applyThinkingGuards("max", false, true, true, "auto", &cfgNoCompact); eff != "max" || reason != "" {
		t.Fatalf("compact disabled got (%q, %q), want (max, \"\")", eff, reason)
	}

	// 5. 无工具请求 max -> high
	if eff, reason := applyThinkingGuards("max", false, false, false, "", &cfg); eff != "high" || reason != "no_tool_max_cap" {
		t.Fatalf("no-tool+max got (%q, %q), want (high, no_tool_max_cap)", eff, reason)
	}
	// 6. 工具存在但 choiceMode=none -> high
	if eff, reason := applyThinkingGuards("max", false, false, true, "none", &cfg); eff != "high" || reason != "no_tool_max_cap" {
		t.Fatalf("choice=none+max got (%q, %q), want (high, no_tool_max_cap)", eff, reason)
	}
	// 7. 正常带工具 max -> 保留 max
	if eff, reason := applyThinkingGuards("max", false, false, true, "auto", &cfg); eff != "max" || reason != "" {
		t.Fatalf("with-tool+max got (%q, %q), want (max, \"\")", eff, reason)
	}
	// 8. 关闭 no_tool_max_cap 后保留 max
	cfgNoCap := config.Default()
	cfgNoCap.Guardrails.NoToolMaxCap = boolRef(false)
	if eff, reason := applyThinkingGuards("max", false, false, false, "", &cfgNoCap); eff != "max" || reason != "" {
		t.Fatalf("no-tool cap disabled got (%q, %q), want (max, \"\")", eff, reason)
	}
}

func TestExtractEnvSummary(t *testing.T) {
	raw := `Some long system instructions.
<env>
Working directory: E:\demo\app
Platform: win32
Shell: powershell
</env>
Other paragraphs...
Primary working directory: E:\demo\app
Working directory: E:\demo\app
OS Version: Windows 11`

	summary := extractEnvSummary(raw)
	if !strings.Contains(summary, `Working directory: E:\demo\app`) {
		t.Fatalf("summary missing Working directory: %q", summary)
	}
	if !strings.Contains(summary, "Platform: win32") {
		t.Fatalf("summary missing Platform: %q", summary)
	}
	if !strings.Contains(summary, "OS Version: Windows 11") {
		t.Fatalf("summary missing OS Version: %q", summary)
	}
	if strings.Count(summary, "Primary working directory: E:\\demo\\app") != 1 {
		t.Fatalf("expected Primary working directory once in summary: %q", summary)
	}
	if strings.Count(summary, "\nWorking directory:") != 0 && !strings.HasPrefix(summary, "Working directory: E:\\demo\\app") {
		t.Fatalf("expected Working directory deduped in summary: %q", summary)
	}
	// 验证不出现两行一模一样的 Working directory:
	wdLines := 0
	for _, ln := range strings.Split(summary, "\n") {
		if ln == `Working directory: E:\demo\app` {
			wdLines++
		}
	}
	if wdLines != 1 {
		t.Fatalf("expected exactly 1 'Working directory' line, got %d in %q", wdLines, summary)
	}

	// 验证超过 800 字节时的 UTF-8 安全截断
	longVal := strings.Repeat("中文目录名/", 120)
	longSummary := extractEnvSummary("<env>\nWorking directory: " + longVal + "\n</env>")
	if len(longSummary) > 800 {
		t.Fatalf("summary exceeded 800 bytes: %d", len(longSummary))
	}
}

func TestPrepareFrameWithinBudgetOpts_PreserveEnvSummary(t *testing.T) {
	envHeader := "<env>\nPrimary working directory: E:\\workspace\\demo\nPlatform: win32\n</env>\n"
	hugeGuide := envHeader + strings.Repeat("Repository rule line for client context.\n", 2200)
	toolProtocol := "<client_tool_protocol>\nTool Read(path)\n</client_tool_protocol>"

	makePayload := func(guide string) map[string]any {
		return map[string]any{
			"text": renderTurnText(guide, toolProtocol, "", "请创建文件"),
		}
	}

	uploadStub := func(name string, data []byte) (attachmentRef, error) {
		return attachmentRef{Name: name, Path: "/tmp/" + name, Size: len(data)}, nil
	}

	// 开启保留环境摘要
	p1 := makePayload(hugeGuide)
	prepareFrameWithinBudgetOpts(p1, "msg-11223344", hugeGuide, toolProtocol, "", "请创建文件", "请创建文件", nil, nil, true, uploadStub)
	t1, _ := p1["text"].(string)
	if !strings.Contains(t1, `Primary working directory: E:\workspace\demo`) {
		t.Fatalf("expected env summary in payload text, got: %s", t1)
	}
	if !strings.Contains(t1, toolProtocol) {
		t.Fatalf("expected tool protocol preserved")
	}

	// 关闭保留环境摘要
	p2 := makePayload(hugeGuide)
	prepareFrameWithinBudgetOpts(p2, "msg-55667788", hugeGuide, toolProtocol, "", "请创建文件", "请创建文件", nil, nil, false, uploadStub)
	t2, _ := p2["text"].(string)
	if strings.Contains(t2, `Primary working directory: E:\workspace\demo`) {
		t.Fatalf("did not expect env summary when disabled, got: %s", fmt.Sprint(t2))
	}
}
