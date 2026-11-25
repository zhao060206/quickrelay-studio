package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestResolveThinkingEffort 验证各思考等级参数优先级、大小写及归一化规则。
func TestResolveThinkingEffort(t *testing.T) {
	cases := []struct {
		name     string
		req      *ChatRequest
		fallback string
		want     string
	}{
		{
			name:     "top_level_reasoning_effort_wins",
			req:      &ChatRequest{ReasoningEffort: " HIGH ", Reasoning: &ReasoningOptions{Effort: "low"}},
			fallback: "medium",
			want:     "high",
		},
		{
			name:     "nested_reasoning_effort",
			req:      &ChatRequest{Reasoning: &ReasoningOptions{Effort: "minimal"}},
			fallback: "high",
			want:     "low",
		},
		{
			name:     "xhigh_and_ultra_map_to_max",
			req:      &ChatRequest{Reasoning: &ReasoningOptions{Effort: "xhigh"}},
			fallback: "",
			want:     "max",
		},
		{
			name:     "ultra_maps_to_max",
			req:      &ChatRequest{ReasoningEffort: "ULTRA"},
			fallback: "",
			want:     "max",
		},
		{
			name:     "none_disables_fallback",
			req:      &ChatRequest{ReasoningEffort: "none"},
			fallback: "high",
			want:     "",
		},
		{
			name:     "off_disables_fallback",
			req:      &ChatRequest{Reasoning: &ReasoningOptions{Effort: "off"}},
			fallback: "medium",
			want:     "",
		},
		{
			name:     "unknown_falls_back_empty",
			req:      &ChatRequest{ReasoningEffort: "cosmic"},
			fallback: "high",
			want:     "",
		},
		{
			name:     "uses_fallback_when_empty",
			req:      &ChatRequest{},
			fallback: "medium",
			want:     "medium",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveThinkingEffort(tc.req, tc.fallback); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestToolBridgeStream_InstantPlainText 验证普通正文和代码块能够在 Feed 期间实时吐出而不等到 Flush。
func TestToolBridgeStream_InstantPlainText(t *testing.T) {
	allowed := map[string]bool{"read_file": true}
	s := newToolBridgeStream(allowed, "auto", "", true)

	chunks := []string{"你", "好，", "以下是最终结论：\n", "1. 第一点说明\n", "2. 第二点说明"}
	var got strings.Builder
	emittedInFeed := 0
	for _, ch := range chunks {
		txt, calls := s.Feed(ch)
		if len(calls) != 0 {
			t.Fatalf("unexpected tool calls: %#v", calls)
		}
		if txt != "" {
			emittedInFeed++
			got.WriteString(txt)
		}
	}
	if emittedInFeed < 3 {
		t.Fatalf("expected streaming text to emit incrementally during Feed, only emitted %d times", emittedInFeed)
	}
	rem, calls := s.Flush()
	if len(calls) != 0 {
		t.Fatalf("unexpected flush calls: %#v", calls)
	}
	got.WriteString(rem)
	want := "你好，以下是最终结论：\n1. 第一点说明\n2. 第二点说明"
	if got.String() != want {
		t.Fatalf("got %q, want %q", got.String(), want)
	}
}

// TestToolBridgeStream_SplitTagAndCommentary 验证前置说明实时流出、碎片化标记分离及并行控制。
func TestToolBridgeStream_SplitTagAndCommentary(t *testing.T) {
	allowed := map[string]bool{"read_file": true, "grep": true}
	s := newToolBridgeStream(allowed, "auto", "", true)

	deltas := []string{
		"我先查看配置文件。",
		"\n<clie",
		"nt_tool_call>{\"name\":\"read_file\",",
		"\"arguments\":{\"path\":\"config.json\"",
		"}}</client_tool_",
		"call>\n再搜索源码<client_tool_call>{\"name\":\"grep\",\"arguments\":{\"q\":\"abc\"}}</client_tool_call>",
	}

	var texts strings.Builder
	var allCalls []ToolCallReply
	var commentaryBeforeFirstCall bool

	for _, d := range deltas {
		txt, calls := s.Feed(d)
		texts.WriteString(txt)
		if len(calls) > 0 && len(allCalls) == 0 && strings.Contains(texts.String(), "我先查看配置文件。") {
			commentaryBeforeFirstCall = true
		}
		allCalls = append(allCalls, calls...)
	}
	rem, remCalls := s.Flush()
	texts.WriteString(rem)
	allCalls = append(allCalls, remCalls...)

	if !commentaryBeforeFirstCall {
		t.Fatalf("expected commentary to stream before or with first tool call, got texts=%q", texts.String())
	}
	if strings.Contains(texts.String(), "<client_tool_call>") || strings.Contains(texts.String(), "</client_tool_call>") {
		t.Fatalf("tool marker leaked into streamed text: %q", texts.String())
	}
	if len(allCalls) != 2 || allCalls[0].Name != "read_file" || allCalls[1].Name != "grep" {
		t.Fatalf("unexpected calls: %#v", allCalls)
	}

	// parallel=false 时只应放行第一个合法工具调用
	single := newToolBridgeStream(allowed, "auto", "", false)
	_, c1 := single.Feed(`<client_tool_call>{"name":"read_file","arguments":{}}</client_tool_call>`)
	_, c2 := single.Feed(`<client_tool_call>{"name":"grep","arguments":{}}</client_tool_call>`)
	_, c3 := single.Flush()
	total := append(append(c1, c2...), c3...)
	if len(total) != 1 || total[0].Name != "read_file" {
		t.Fatalf("expected parallel=false to keep only first call, got %#v", total)
	}
}

// TestToolBridgeStream_UnknownTagAndUnclosedAngle 验证普通 HTML、未授权工具标记和末尾未闭合尖括号均不丢字。
func TestToolBridgeStream_UnknownTagAndUnclosedAngle(t *testing.T) {
	allowed := map[string]bool{"read_file": true}
	s := newToolBridgeStream(allowed, "auto", "", true)

	parts := []string{
		"检查结果如下：<div class=\"ok\">PASS</div> 且 ",
		"<tool_call>{\"name\":\"not_allowed\",\"arguments\":{}}</tool_call>",
		" 结束 <client_tool_call>abc",
	}
	var out strings.Builder
	for _, p := range parts {
		txt, calls := s.Feed(p)
		if len(calls) != 0 {
			t.Fatalf("unexpected tool calls: %#v", calls)
		}
		out.WriteString(txt)
	}
	rem, calls := s.Flush()
	if len(calls) != 0 {
		t.Fatalf("unexpected flush calls: %#v", calls)
	}
	out.WriteString(rem)

	want := strings.Join(parts, "")
	if out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
}

// TestToolBridgeStream_LeadingFenceAndBareJSON 验证行首围栏工具、行首裸 JSON 工具不泄露且普通代码块实时流出。
func TestToolBridgeStream_LeadingFenceAndBareJSON(t *testing.T) {
	allowed := map[string]bool{"read_file": true}

	// 1. 行首围栏包裹的工具调用
	sFence := newToolBridgeStream(allowed, "auto", "", true)
	t1, c1 := sFence.Feed("```json\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.txt\"}}\n```")
	t2, c2 := sFence.Flush()
	calls := append(c1, c2...)
	if strings.TrimSpace(t1+t2) != "" {
		t.Fatalf("expected no leaked fence text, got %q", t1+t2)
	}
	if len(calls) != 1 || calls[0].Name != "read_file" {
		t.Fatalf("expected 1 read_file call, got %#v", calls)
	}

	// 2. 行首裸 JSON 工具调用
	sBare := newToolBridgeStream(allowed, "auto", "", true)
	b1, bc1 := sBare.Feed("{\"name\":\"read_file\",")
	b2, bc2 := sBare.Feed("\"arguments\":{\"path\":\"b.txt\"}}")
	b3, bc3 := sBare.Flush()
	bCalls := append(append(bc1, bc2...), bc3...)
	if strings.TrimSpace(b1+b2+b3) != "" {
		t.Fatalf("expected no leaked bare json, got %q", b1+b2+b3)
	}
	if len(bCalls) != 1 || bCalls[0].Name != "read_file" {
		t.Fatalf("expected 1 read_file call, got %#v", bCalls)
	}

	// 3. 普通 Python 代码块开头应在 Feed 中实时放行流式
	sCode := newToolBridgeStream(allowed, "auto", "", true)
	o1, _ := sCode.Feed("```python\n")
	o2, _ := sCode.Feed("print(1)\n")
	if o1+o2 == "" {
		t.Fatalf("expected ordinary python code fence to stream immediately after body starts")
	}
	o3, _ := sCode.Feed("```")
	o4, _ := sCode.Flush()
	if o1+o2+o3+o4 != "```python\nprint(1)\n```" {
		t.Fatalf("got %q", o1+o2+o3+o4)
	}
}

// TestThinkingDeltaText 验证 thinking_delta 各类载荷字段均能正确提取。
func TestThinkingDeltaText(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{raw: `"raw string"`, want: "raw string"},
		{raw: `{"delta":"step 1"}`, want: "step 1"},
		{raw: `{"delta":{"text":"nested step"}}`, want: "nested step"},
		{raw: `{"thinking":"full thinking"}`, want: "full thinking"},
		{raw: `{"text":"fallback text"}`, want: "fallback text"},
	}
	for _, tc := range cases {
		if got := thinkingDeltaText(json.RawMessage(tc.raw)); got != tc.want {
			t.Fatalf("raw=%s got %q, want %q", tc.raw, got, tc.want)
		}
	}
}
