package proxy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestParseToolEnvelopes_RepairsInvalidBackslashEscape 使用线上日志真实出错的嵌套 PowerShell 参数验证非法反斜杠转义修复。
func TestParseToolEnvelopes_RepairsInvalidBackslashEscape(t *testing.T) {
	// 取自真实失败日志 line 2399：\$(\$_.OwningProcess) 触发 Invalid \escape
	raw := `{"name":"Bash","arguments":{"command":"cd '/e/codex/绘图/Rylume'; echo '=== 8779 进程命令行 ==='; powershell -NoProfile -Command \"Get-NetTCPConnection -LocalPort 8779 -State Listen -ErrorAction SilentlyContinue | ForEach-Object { (Get-CimInstance Win32_Process -Filter \\\"ProcessId=\$(\$_.OwningProcess)\\\").CommandLine }\" 2>&1 | head -5","description":"只读：查看 8779 进程的启动参数和数据目录","timeout":60000}}`

	stream := newToolBridgeStream(map[string]bool{"Bash": true}, "auto", "", true)
	outText, calls := stream.Feed("<client_tool_call>" + raw + "</client_tool_call>")
	fText, fCalls := stream.Flush()
	outText += fText
	calls = append(calls, fCalls...)

	if strings.Contains(outText, "<client_tool_call>") {
		t.Fatalf("leaked raw tool tag after JSON repair: %q", outText)
	}
	if len(calls) != 1 || calls[0].Name != "Bash" {
		t.Fatalf("expected 1 Bash call, got %+v", calls)
	}

	var decoded struct {
		Command     string `json:"command"`
		Description string `json:"description"`
		Timeout     int    `json:"timeout"`
	}
	if err := json.Unmarshal([]byte(calls[0].Arguments), &decoded); err != nil {
		t.Fatalf("repaired arguments invalid JSON: %v (%s)", err, calls[0].Arguments)
	}
	if !strings.Contains(decoded.Command, `\$(\$_.OwningProcess)`) {
		t.Fatalf("repaired command altered powershell escape: %q", decoded.Command)
	}
	if !strings.Contains(decoded.Command, "/e/codex/绘图/Rylume") || decoded.Timeout != 60000 {
		t.Fatalf("unexpected decoded command fields: %+v", decoded)
	}
}

// TestParseToolEnvelopes_RepairsRawNewlinesInString 使用线上日志 line 1036 真实多行 heredoc 裸换行案例做回归验证。
func TestParseToolEnvelopes_RepairsRawNewlinesInString(t *testing.T) {
	raw := `{"name":"Bash","arguments":{"command":"cd \"E:/codex/绘图/Rylume\" && cat >> ui/styles/calm.css <<'EOF'

/* assets v9 (C): borderless tiles */
dialog.docked #asset-list{gap:4px!important}
EOF","timeout":300000}}`

	allowed := map[string]bool{"Bash": true}
	text, replies := parseClientToolCalls("<client_tool_call>"+raw+"</client_tool_call>", allowed)
	if text != "" {
		t.Fatalf("expected empty residual text, got %q", text)
	}
	if len(replies) != 1 || replies[0].Name != "Bash" {
		t.Fatalf("expected 1 Bash call, got %+v", replies)
	}
	var args struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	if err := json.Unmarshal([]byte(replies[0].Arguments), &args); err != nil {
		t.Fatalf("invalid arguments JSON: %v", err)
	}
	if !strings.Contains(args.Command, "\n\n/* assets v9 (C): borderless tiles */\n") || args.Timeout != 300000 {
		t.Fatalf("unexpected multiline command decoded: %+v", args)
	}
}

// TestParseToolEnvelopes_RepairsTrailingCommaAndWindowsPaths 验证尾逗号、Windows 反斜杠路径及合法 \uXXXX 转义不受损。
func TestParseToolEnvelopes_RepairsTrailingCommaAndWindowsPaths(t *testing.T) {
	raw := `{"name":"Read","arguments":{"file_path":"E:\codex\绘图\docs\sample.txt","note":"合法转义:\"ok\" 和 \u4e2d\u6587",},}`
	envs := parseToolEnvelopes(raw)
	if len(envs) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(envs))
	}
	replies := repliesFromEnvelopes(envs, map[string]bool{"Read": true})
	if len(replies) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(replies))
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(replies[0].Arguments), &args); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, replies[0].Arguments)
	}
	if args["file_path"] != `E:\codex\绘图\docs\sample.txt` {
		t.Fatalf("windows path mismatch: %q", args["file_path"])
	}
	if args["note"] != `合法转义:"ok" 和 中文` {
		t.Fatalf("valid escape altered: %q", args["note"])
	}
}

// TestToolBridgeStream_RecoversUnclosedTagAtEOF 验证流末尾漏写闭合 </client_tool_call> 时 Flush 可兜底解析出合法工具调用。
func TestToolBridgeStream_RecoversUnclosedTagAtEOF(t *testing.T) {
	stream := newToolBridgeStream(map[string]bool{"Read": true}, "auto", "", true)
	t1, c1 := stream.Feed(`先读一下文件。<client_tool_call>{"name":"Read","arguments":{"file_path":"E:\codex\a.txt"}}`)
	if len(c1) != 0 || t1 != "先读一下文件。" {
		t.Fatalf("unexpected Feed output: text=%q calls=%+v", t1, c1)
	}
	t2, c2 := stream.Flush()
	if t2 != "" || len(c2) != 1 || c2[0].Name != "Read" {
		t.Fatalf("expected Flush recovery of unclosed tool call, got text=%q calls=%+v", t2, c2)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(c2[0].Arguments), &args); err != nil || args["file_path"] != `E:\codex\a.txt` {
		t.Fatalf("unexpected recovered args: %s err=%v", c2[0].Arguments, err)
	}
}

// TestRetryActionFor_TransientUpstreamMessagesAndFilterStop 验证临时性无错误码上游提示触发换号重试，内容安全拦截严禁重试。
func TestRetryActionFor_TransientUpstreamMessagesAndFilterStop(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want retryAction
	}{
		{
			name: "something_went_wrong_retries",
			err:  &UpstreamError{StatusCode: 0, Err: errors.New("upstream error: Something went wrong — please try again.")},
			want: retryTemporary,
		},
		{
			name: "temporary_issue_retries",
			err:  &UpstreamError{StatusCode: 0, Err: errors.New("upstream error: The AI service encountered a temporary issue. Please try again.")},
			want: retryTemporary,
		},
		{
			name: "content_filter_stops",
			err:  &UpstreamError{StatusCode: 0, Err: errors.New("upstream error: The model's content filter stopped this reply. Rephrase, or start a new chat.")},
			want: retryStop,
		},
		{
			name: "automated_check_stops",
			err:  &UpstreamError{StatusCode: 500, Err: errors.New("That content did not pass our automated check.")},
			want: retryStop,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryActionFor(tc.err); got != tc.want {
				t.Fatalf("retryActionFor(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestDLPPollDelay_ProgressiveBackoff 验证附件 DLP 状态轮询采用 300ms -> 600ms -> 1000ms -> 1500ms 渐进退避。
func TestDLPPollDelay_ProgressiveBackoff(t *testing.T) {
	if got := dlpPollDelay(1); got != 300*time.Millisecond {
		t.Fatalf("attempt 1 = %v", got)
	}
	if got := dlpPollDelay(2); got != 600*time.Millisecond {
		t.Fatalf("attempt 2 = %v", got)
	}
	if got := dlpPollDelay(3); got != 1000*time.Millisecond {
		t.Fatalf("attempt 3 = %v", got)
	}
	if got := dlpPollDelay(4); got != attachmentPollInterval {
		t.Fatalf("attempt 4 = %v", got)
	}
}

// TestParseToolEnvelopes_RepairsUnescapedTripleQuotesAndWindowsTabs 使用用户截图中的真实 Write 泄漏案例验证：
// 1) 未转义的四引号 Python docstring `""""..."""` 及内部代码引号；
// 2) 含 \tests 与 \test_ 的裸 Windows 盘符路径不被误解析为 TAB 制表符。
func TestParseToolEnvelopes_RepairsUnescapedTripleQuotesAndWindowsTabs(t *testing.T) {
	raw := `<client_tool_call>{"name":"Write","arguments":{"file_path":"E:\codex\绘图\Rylume\tests\test_label_placement.py","content":""""Object labels stay attached to their shape and inside the right compartment.\n\nStep 2 of docs/修复计划-2026-11-24-排版.md: ligand labels hug the shape,\nmembrane-protein labels sit beside the object at membrane height instead of\nfalling into the cytoplasm, detached labels are reported, and labels the user\nplaced by hand are never moved or flagged.\n""""\nimport math\nimport pytest\nfrom scene_document import apply_operations, audit_scene, normalize_scene\n\nMEMBRANE = ('membrane', 'plasma_membrane')\n\ndef _kind(scene, o):\n return {c['id']: c['kind'] for c in scene['compartments']}.get(o.get('compartment'), '')\n\n@pytest.mark.parametrize('name', ['egfr', 'jak_stat'])\ndef test_ligand_label_hugs_its_shape(name):\n assert _gap(o, box) <= 3, (o['id'], _gap(o, box))\n assert 'label-wrong-compartment' not in codes\n}}</client_tool_call>`

	stream := newToolBridgeStream(map[string]bool{"Write": true}, "auto", "", true)
	var outText strings.Builder
	var calls []ToolCallReply

	// 模拟真实 SSE 按小块分片喂入流中
	const chunkSize = 23
	for off := 0; off < len(raw); off += chunkSize {
		end := min(len(raw), off+chunkSize)
		txt, cs := stream.Feed(raw[off:end])
		outText.WriteString(txt)
		calls = append(calls, cs...)
	}
	ft, fc := stream.Flush()
	outText.WriteString(ft)
	calls = append(calls, fc...)

	if strings.Contains(outText.String(), "<client_tool_call>") || strings.TrimSpace(outText.String()) != "" {
		t.Fatalf("leaked raw tool call into visible text: %q", outText.String())
	}
	if len(calls) != 1 || calls[0].Name != "Write" {
		t.Fatalf("expected 1 Write call, got %+v", calls)
	}

	var decoded struct {
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal([]byte(calls[0].Arguments), &decoded); err != nil {
		t.Fatalf("repaired arguments invalid JSON: %v (%s)", err, calls[0].Arguments)
	}
	wantPath := `E:\codex\绘图\Rylume\tests\test_label_placement.py`
	if decoded.FilePath != wantPath {
		t.Fatalf("file_path corrupted (tab escape bug?): got %q, want %q", decoded.FilePath, wantPath)
	}
	if !strings.HasPrefix(decoded.Content, `"""Object labels stay attached`) {
		t.Fatalf("content prefix mismatch: %q", decoded.Content[:min(len(decoded.Content), 60)])
	}
	if !strings.Contains(decoded.Content, `assert 'label-wrong-compartment' not in codes`) {
		t.Fatalf("content truncated: %q", decoded.Content)
	}

	// 同时验证若代码中出现未转义双引号字典与逗号表达式 {"id": "x"}.get(...) 也能成功修复
	rawWithDoubleQuotes := `<client_tool_call>{"name":"Write","arguments":{"file_path":"E:\codex\a\tests\run.py","content":""""Doc""""\ndef fn(scene, o):\n    val = {"id": "a"}.get("id", "b")\n    return ("egfr", "jak_stat", val)\n}}</client_tool_call>`
	clean2, calls2 := parseClientToolCalls(rawWithDoubleQuotes, map[string]bool{"Write": true})
	if clean2 != "" || len(calls2) != 1 || calls2[0].Name != "Write" {
		t.Fatalf("failed on unescaped inner double quotes dict/tuple: clean=%q calls=%+v", clean2, calls2)
	}
	var decoded2 struct {
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal([]byte(calls2[0].Arguments), &decoded2); err != nil {
		t.Fatalf("invalid arguments JSON for inner quotes case: %v (%s)", err, calls2[0].Arguments)
	}
	if !strings.Contains(decoded2.Content, `{"id": "a"}.get("id", "b")`) {
		t.Fatalf("inner double-quoted python dict altered: %q", decoded2.Content)
	}
}
