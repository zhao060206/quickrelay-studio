package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

const sampleScreenshotDecision = `3. 这些本地改动没有覆盖本机的正式安装，也没动用户的图版。 [@0cc622]
<decision question="接下来先做哪件事？ "> <option description="只查看不改动：看 git status 和 git log，确认 HEAD 是不是 82e9890，以及有哪些改动没提交">核对本地源码状态</option> <option description="从服务返回的模型里选一个网关允许的文本模型，实际跑一次 AI 起稿和批注重绘（要改模型配置，需要你先同意） ">让 AI 起稿真正跑通</option> <option description="提交本地改动，跑完整回归测试，然后打包并验证更新">准备下一版发布</option> </decision>`

// TestQuickOutput_DecisionToAskUserQuestion 验证将截图中的真实 <decision> 转为 Claude Code 原生 AskUserQuestion 工具调用且剔除 [@xxxxxx]。
func TestQuickOutput_DecisionToAskUserQuestion(t *testing.T) {
	allowed := map[string]bool{
		"Read":            true,
		"AskUserQuestion": true,
	}
	clean, calls := cleanQuickOutput(sampleScreenshotDecision, allowed, func(tc ToolCallReply) bool {
		return true
	})

	if strings.Contains(clean, "<decision") || strings.Contains(clean, "<option") || strings.Contains(clean, "[@0cc622]") {
		t.Fatalf("output leaked raw quickwork tags or citations: %q", clean)
	}
	if !strings.Contains(clean, "也没动用户的图版。") {
		t.Fatalf("clean text lost main sentence: %q", clean)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 AskUserQuestion call, got %d", len(calls))
	}
	if calls[0].Name != "AskUserQuestion" {
		t.Fatalf("expected AskUserQuestion, got %q", calls[0].Name)
	}

	var parsed struct {
		Questions []struct {
			Question    string `json:"question"`
			Header      string `json:"header"`
			MultiSelect bool   `json:"multiSelect"`
			Options     []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal([]byte(calls[0].Arguments), &parsed); err != nil {
		t.Fatalf("invalid AskUserQuestion JSON arguments: %v (%s)", err, calls[0].Arguments)
	}
	if len(parsed.Questions) != 1 {
		t.Fatalf("expected 1 question, got %d", len(parsed.Questions))
	}
	q := parsed.Questions[0]
	if q.Question != "接下来先做哪件事？" || q.Header != "下一步" || q.MultiSelect {
		t.Fatalf("unexpected question metadata: %+v", q)
	}
	if len(q.Options) != 3 || q.Options[0].Label != "核对本地源码状态" || q.Options[2].Label != "准备下一版发布" {
		t.Fatalf("unexpected converted options: %+v", q.Options)
	}
}

// TestQuickOutput_DecisionToCodexRequestUserInput 验证 Codex Plan 模式下精准转为 request_user_input 且不含 multiSelect。
func TestQuickOutput_DecisionToCodexRequestUserInput(t *testing.T) {
	allowed := map[string]bool{
		"exec_command":       true,
		"request_user_input": true,
	}
	_, calls := cleanQuickOutput(sampleScreenshotDecision, allowed, func(tc ToolCallReply) bool {
		return true
	})
	if len(calls) != 1 || calls[0].Name != "request_user_input" {
		t.Fatalf("expected request_user_input call, got %+v", calls)
	}
	if strings.Contains(calls[0].Arguments, `"multiSelect"`) {
		t.Fatalf("request_user_input schema forbids multiSelect: %s", calls[0].Arguments)
	}

	var raw map[string]any
	if err := json.Unmarshal([]byte(calls[0].Arguments), &raw); err != nil {
		t.Fatalf("invalid request_user_input JSON: %v", err)
	}
	qs := raw["questions"].([]any)
	q0 := qs[0].(map[string]any)
	if q0["id"] != "next_step" || q0["header"] != "下一步" {
		t.Fatalf("unexpected question keys: %+v", q0)
	}
	if len(q0["options"].([]any)) != 3 {
		t.Fatalf("expected 3 options, got %d", len(q0["options"].([]any)))
	}
}

// TestQuickOutput_DecisionFallbackMarkdown 验证无提问工具或不允许再发工具时输出清爽 Markdown 编号列表。
func TestQuickOutput_DecisionFallbackMarkdown(t *testing.T) {
	clean, calls := cleanQuickOutput(sampleScreenshotDecision, map[string]bool{"exec_command": true}, func(tc ToolCallReply) bool {
		return false
	})
	if len(calls) != 0 {
		t.Fatalf("expected 0 tool calls on fallback, got %d", len(calls))
	}
	for _, banned := range []string{"<decision", "<option", "</decision>", "[@0cc622]"} {
		if strings.Contains(clean, banned) {
			t.Fatalf("markdown fallback leaked %q: %q", banned, clean)
		}
	}
	for _, want := range []string{
		"**接下来先做哪件事？**",
		"1. **核对本地源码状态**：只查看不改动",
		"2. **让 AI 起稿真正跑通**：从服务返回的模型里选一个",
		"3. **准备下一版发布**：提交本地改动",
	} {
		if !strings.Contains(clean, want) {
			t.Fatalf("markdown fallback missing %q in:\n%s", want, clean)
		}
	}
}

// TestQuickOutput_StripsCitationsPreservesCodeBlock 验证仅删除正文中的 [@xxxxxx]，而围栏代码块内完全保持原样。
func TestQuickOutput_StripsCitationsPreservesCodeBlock(t *testing.T) {
	input := "已完成修改 [@0cc622]。另外参考 [@8dbdfb] 后继续：\n" +
		"```html\n" +
		"<p>示例保留 [@0cc622] 和 <decision question=\"ok\"><option>A</option><option>B</option></decision></p>\n" +
		"```\n" +
		"收尾说明 [@a1b2c3d4]。"

	clean, calls := cleanQuickOutput(input, map[string]bool{"AskUserQuestion": true}, func(tc ToolCallReply) bool {
		return true
	})
	if len(calls) != 0 {
		t.Fatalf("must not parse <decision> inside code fence, got %d calls", len(calls))
	}
	if !strings.Contains(clean, `<p>示例保留 [@0cc622] 和 <decision question="ok"><option>A</option><option>B</option></decision></p>`) {
		t.Fatalf("code fence content was altered:\n%s", clean)
	}
	if strings.Contains(clean, "已完成修改 [@0cc622]") || strings.Contains(clean, "参考 [@8dbdfb]") || strings.Contains(clean, "收尾说明 [@a1b2c3d4]") {
		t.Fatalf("outside-fence citations were not stripped:\n%s", clean)
	}
	if !strings.Contains(clean, "已完成修改。另外参考 后继续：") {
		t.Fatalf("unexpected spacing after stripping citations:\n%s", clean)
	}
}

// TestQuickOutputFilter_StreamingChunkBoundaries 验证 1~3 字节随机碎片流式喂入与一次性解析完全一致。
func TestQuickOutputFilter_StreamingChunkBoundaries(t *testing.T) {
	input := "结论如上 [@0cc622]。\n" +
		"```go\n" +
		"// inside [@deadbeef]\n" +
		"```\n" +
		"请确认 [@8dbdfb]：\n" +
		`<decision question='Choose next step?'><option description="Inspect repo first">Check repo</option><option>Skip</option></decision>`

	allowed := map[string]bool{"AskUserQuestion": true}
	wantText, wantCalls := cleanQuickOutput(input, allowed, func(tc ToolCallReply) bool { return true })

	for chunkSize := 1; chunkSize <= 3; chunkSize++ {
		filter := newQuickOutputFilter(allowed, func(tc ToolCallReply) bool { return true })
		var gotText strings.Builder
		var gotCalls []ToolCallReply

		for i := 0; i < len(input); i += chunkSize {
			end := min(len(input), i+chunkSize)
			tOut, cOut := filter.Feed(input[i:end])
			gotText.WriteString(tOut)
			gotCalls = append(gotCalls, cOut...)
		}
		fOut, fcOut := filter.Flush()
		gotText.WriteString(fOut)
		gotCalls = append(gotCalls, fcOut...)

		if gotText.String() != wantText {
			t.Fatalf("chunkSize=%d streaming text mismatch:\ngot:  %q\nwant: %q", chunkSize, gotText.String(), wantText)
		}
		if len(gotCalls) != len(wantCalls) || len(gotCalls) != 1 {
			t.Fatalf("chunkSize=%d expected 1 call, got %d", chunkSize, len(gotCalls))
		}
		if gotCalls[0].Name != "AskUserQuestion" {
			t.Fatalf("chunkSize=%d unexpected tool name %q", chunkSize, gotCalls[0].Name)
		}
	}
}

func assertStreamingMatchesClean(t *testing.T, input string, allowed map[string]bool) (string, []ToolCallReply) {
	t.Helper()
	wantText, wantCalls := cleanQuickOutput(input, allowed, func(tc ToolCallReply) bool { return true })
	for chunkSize := 1; chunkSize <= 3; chunkSize++ {
		filter := newQuickOutputFilter(allowed, func(tc ToolCallReply) bool { return true })
		var gotText strings.Builder
		var gotCalls []ToolCallReply
		for i := 0; i < len(input); i += chunkSize {
			end := min(len(input), i+chunkSize)
			tOut, cOut := filter.Feed(input[i:end])
			gotText.WriteString(tOut)
			gotCalls = append(gotCalls, cOut...)
		}
		fOut, fcOut := filter.Flush()
		gotText.WriteString(fOut)
		gotCalls = append(gotCalls, fcOut...)

		if gotText.String() != wantText {
			t.Fatalf("chunkSize=%d streaming mismatch:\ngot:  %q\nwant: %q", chunkSize, gotText.String(), wantText)
		}
		if len(gotCalls) != len(wantCalls) {
			t.Fatalf("chunkSize=%d tool calls count mismatch: got %d, want %d", chunkSize, len(gotCalls), len(wantCalls))
		}
		for idx := range wantCalls {
			if gotCalls[idx].Name != wantCalls[idx].Name || gotCalls[idx].Arguments != wantCalls[idx].Arguments {
				t.Fatalf("chunkSize=%d call[%d] mismatch:\ngot:  %+v\nwant: %+v", chunkSize, idx, gotCalls[idx], wantCalls[idx])
			}
		}
	}
	return wantText, wantCalls
}

// TestQuickOutput_StripsSourcesBlockAndLinks 还原真实截图中的 <sources> 块、末尾未闭合 <sources> 与孤立 </sources> 并验证流式一致性。
func TestQuickOutput_StripsSourcesBlockAndLinks(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "screenshot_sources_block",
			input: "最终结论如下。\n\n<sources>\n[quote-heavy content via file](qw-file://doc/123)\n</sources>",
			want:  "最终结论如下。\n\n",
		},
		{
			name:  "sources_in_middle_keeps_following_text",
			input: "第一段。\n<sources>\n- [Ref 1](https://example.com)\n</sources>\n第二段内容。",
			want:  "第一段。\n第二段内容。",
		},
		{
			name:  "unclosed_trailing_sources",
			input: "分析已完成。\n<sources>\n[quote-heavy content via file](qw-file://doc/1)",
			want:  "分析已完成。\n",
		},
		{
			name:  "orphan_closing_sources",
			input: "已整理完文档。\n</sources>",
			want:  "已整理完文档。\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, calls := assertStreamingMatchesClean(t, tc.input, nil)
			if len(calls) != 0 {
				t.Fatalf("unexpected tool calls: %+v", calls)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestQuickOutput_MultiCitationsAndPrivateLinks 验证多引用角标 [@xxx, @yyy] 与 kg://、qw-file://、qw-tool://、mention:// 私有链接清洗且不误伤正常链接。
func TestQuickOutput_MultiCitationsAndPrivateLinks(t *testing.T) {
	input := "总结 [@0cc622, @8dbdfb] 与 [@abc123; @def456] 后，请 [@Alice](kg://person/1) 和 [@Bob](mention://U123) 检查" +
		" [设计稿.docx](qw-file://file/1) 与工具 [[1]](qw-tool://tool/2) 或 [@0cc622](kg://ref/3)，" +
		"参考 [官方仓库](https://github.com/zhao060206/quickrelay-studio) 即可。"

	got, _ := assertStreamingMatchesClean(t, input, nil)
	want := "总结 与 后，请 Alice 和 @Bob 检查 设计稿.docx 与工具 或，" +
		"参考 [官方仓库](https://github.com/zhao060206/quickrelay-studio) 即可。"
	if got != want {
		t.Fatalf("got:\n  %q\nwant:\n  %q", got, want)
	}
}

// TestQuickOutput_DecisionsPluralAndInvokeClose 验证 <decisions> 复数块、multi="true" 及 </invoke> 闭合变体转原生 AskUserQuestion 或 Markdown 列表。
func TestQuickOutput_DecisionsPluralAndInvokeClose(t *testing.T) {
	input := "请确认以下两项：\n" +
		`<decisions>` +
		`<decision question="选择目标目录？" multi="true"><option description="仅导出 PDF">PDF</option><option description="导出 Word">DOCX</option></decision>` +
		`<decision question="是否覆盖旧文件？"><option>覆盖</option><option>新建副本</option></invoke>` +
		`</decisions>`

	gotText, calls := assertStreamingMatchesClean(t, input, map[string]bool{"AskUserQuestion": true})
	if strings.Contains(gotText, "<decision") || strings.Contains(gotText, "</invoke>") {
		t.Fatalf("decisions tags leaked into text: %q", gotText)
	}
	if len(calls) != 1 || calls[0].Name != "AskUserQuestion" {
		t.Fatalf("expected 1 AskUserQuestion call, got %+v", calls)
	}
	var parsed struct {
		Questions []struct {
			Question    string `json:"question"`
			MultiSelect bool   `json:"multiSelect"`
			Options     []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal([]byte(calls[0].Arguments), &parsed); err != nil {
		t.Fatalf("invalid arguments JSON: %v", err)
	}
	if len(parsed.Questions) != 2 || !parsed.Questions[0].MultiSelect || parsed.Questions[1].MultiSelect {
		t.Fatalf("unexpected multiSelect or question count: %+v", parsed.Questions)
	}

	// 验证降级 Markdown 时渲染两道题且剔除标签
	mdText, mdCalls := assertStreamingMatchesClean(t, input, nil)
	if len(mdCalls) != 0 {
		t.Fatalf("expected 0 calls when tool unavailable")
	}
	if !strings.Contains(mdText, "**选择目标目录？**") || !strings.Contains(mdText, "**是否覆盖旧文件？**") {
		t.Fatalf("markdown fallback missing questions: %q", mdText)
	}
}

// TestQuickOutput_ArtifactAndControlMarkers 验证 <artifact>、<decision_response> 与路由控制标记均被准确清洗。
func TestQuickOutput_ArtifactAndControlMarkers(t *testing.T) {
	input := "报告已准备就绪。\n" +
		`<artifact id="art-1" title="Chart"/>` + "\n" +
		`<artifact type="text/html"><div>hidden</div></artifact>` + "\n" +
		"[ACP_ROUTE:planner] [NOTCH_REPLY_STYLE:concise]\n" +
		"[CHAT_AGENT_DELEGATE:\nagent=writer\ntask=summary\n]\n" +
		"<decision_response>已确认选项A</decision_response>\n" +
		"</option></artifact>收尾完成。"

	got, _ := assertStreamingMatchesClean(t, input, nil)
	want := "报告已准备就绪。\n\n已确认选项A\n收尾完成。"
	if got != want {
		t.Fatalf("got:\n  %q\nwant:\n  %q", got, want)
	}
}

// TestQuickOutput_PreservesInlineCodeAndCodeFences 验证行内反引号与 ``` / ~~~ 围栏代码块内的所有标签原封不动保留。
func TestQuickOutput_PreservesInlineCodeAndCodeFences(t *testing.T) {
	input := "检查示例 `<sources>a</sources>` 与 `[@0cc622, @8dbdfb]` 以及 `[ACP_ROUTE:x]` 和 `[link](qw-file://demo)`：\n" +
		"~~~xml\n" +
		"<sources>\n<artifact id=\"1\"/>\n[@deadbeef]\n</sources>\n" +
		"~~~\n" +
		"普通正文结束 [@0cc622]。"

	got, calls := assertStreamingMatchesClean(t, input, nil)
	if len(calls) != 0 {
		t.Fatalf("unexpected tool calls: %+v", calls)
	}
	for _, preserved := range []string{
		"`<sources>a</sources>`",
		"`[@0cc622, @8dbdfb]`",
		"`[ACP_ROUTE:x]`",
		"`[link](qw-file://demo)`",
		"<artifact id=\"1\"/>",
		"[@deadbeef]",
	} {
		if !strings.Contains(got, preserved) {
			t.Fatalf("expected %q preserved, got:\n%s", preserved, got)
		}
	}
	if strings.Contains(got, "普通正文结束 [@0cc622]") {
		t.Fatalf("trailing bare citation should be stripped: %q", got)
	}
	if !strings.Contains(got, "普通正文结束。") {
		t.Fatalf("missing clean tail: %q", got)
	}
}
