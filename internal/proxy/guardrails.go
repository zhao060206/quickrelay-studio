package proxy

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"quickrelay-studio/internal/config"
)

var (
	envTagRe     = regexp.MustCompile(`(?is)<env>\s*(.*?)\s*</env>`)
	envKeyLineRe = regexp.MustCompile(`(?i)^\s*(?:[-*]\s*)?(working directory|primary working directory|current working directory|platform|os version|shell|is directory a git repo|today's date)\s*:`)
)

// isExplicitThinkingOff 判断调用方是否在请求中显式指定关闭思考（off / none）。
func isExplicitThinkingOff(req *ChatRequest) bool {
	if req == nil {
		return false
	}
	raw := strings.TrimSpace(req.ReasoningEffort)
	if raw == "" && req.Reasoning != nil {
		raw = strings.TrimSpace(req.Reasoning.Effort)
	}
	switch strings.ToLower(raw) {
	case "off", "none":
		return true
	default:
		return false
	}
}

// isCompactionRequest 识别 Claude Code 等客户端发起的 /compact 会话压缩摘要请求，
// 防止其在高思考档位下因纯输出超出上游单步 64K Token 上限而中途断流。
func isCompactionRequest(req *ChatRequest, prompt string) bool {
	if req == nil {
		return false
	}
	var candidates []string
	var userTexts []string
	for _, m := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		text := contentText(m.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if role == "system" || role == "developer" {
			candidates = append(candidates, text)
		} else if role == "user" {
			userTexts = append(userTexts, text)
		}
	}
	if len(userTexts) > 2 {
		userTexts = userTexts[len(userTexts)-2:]
	}
	candidates = append(candidates, userTexts...)
	if trimmed := strings.TrimSpace(prompt); trimmed != "" {
		if len(trimmed) > 4096 {
			trimmed = trimmed[len(trimmed)-4096:]
		}
		candidates = append(candidates, trimmed)
	}

	hasTools := len(req.Tools) > 0 || len(req.Functions) > 0
	var combinedLower strings.Builder
	for _, text := range candidates {
		lower := strings.ToLower(text)
		combinedLower.WriteString(lower)
		combinedLower.WriteByte('\n')
		if strings.Contains(lower, "critical: respond with text only. do not call any tools.") {
			return true
		}
		if strings.Contains(lower, "your task is to create a detailed summary of the conversation so far") {
			return true
		}
		if !hasTools && strings.Contains(lower, "compaction summary") {
			return true
		}
	}
	merged := combinedLower.String()
	if strings.Contains(merged, "<analysis>") && strings.Contains(merged, "<summary>") &&
		(strings.Contains(merged, "conversation so far") || strings.Contains(merged, "summarize the conversation")) {
		return true
	}
	return false
}

// applyThinkingGuards 根据防护开关对思考强度执行降档或限幅：
// 1) /compact 压缩请求强制降为 low（除非调用方显式关闭）；
// 2) 纯无工具长思考且强度为 max 时平滑限幅为 high，规避上游单步 64K Output 硬墙。
func applyThinkingGuards(effort string, explicitOff bool, isCompact bool, bridgeActive bool, choiceMode string, cfg *config.Config) (string, string) {
	compactEnabled := cfg == nil || cfg.Guardrails.CompactDowngradeEnabled()
	noToolCapEnabled := cfg == nil || cfg.Guardrails.NoToolMaxCapEnabled()

	if isCompact && compactEnabled {
		if !explicitOff && effort != "low" {
			return "low", "compact_downgrade"
		}
		return effort, ""
	}
	if !isCompact && effort == "max" && noToolCapEnabled && (!bridgeActive || choiceMode == "none") {
		return "high", "no_tool_max_cap"
	}
	return effort, ""
}

// isSuggestionModeRequest 判断当前请求是否为 Claude Code 发起的输入框下一步建议旁路请求。
// 即使请求为复用缓存而携带全量 tools 与长对话历史，只要最后一条有效用户消息以 "[suggestion mode:" 开头即返回 true。
func isSuggestionModeRequest(req *ChatRequest) bool {
	if req == nil {
		return false
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		role := strings.ToLower(strings.TrimSpace(req.Messages[i].Role))
		if role == "system" || role == "developer" {
			continue
		}
		if role == "" || role == "user" {
			trimmed := strings.ToLower(strings.TrimSpace(contentText(req.Messages[i].Content)))
			return strings.HasPrefix(trimmed, "[suggestion mode:")
		}
		return false
	}
	return false
}

// isTranscriptSideRequest 判断当前请求是否为无工具的 Claude Code / 桌面客户端后台 <transcript> 摘要请求，
// 使其走独立一次性旁路会话并降为低思考档位，避免污染主会话及浪费高耗时思考额度。
func isTranscriptSideRequest(req *ChatRequest) bool {
	if req == nil || len(req.Tools) > 0 || len(req.Functions) > 0 {
		return false
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		role := strings.ToLower(strings.TrimSpace(req.Messages[i].Role))
		if role == "system" || role == "developer" {
			continue
		}
		if role == "" || role == "user" {
			trimmed := strings.ToLower(strings.TrimSpace(contentText(req.Messages[i].Content)))
			return strings.HasPrefix(trimmed, "<transcript>")
		}
		return false
	}
	return false
}

// isAuxiliarySideRequest 识别客户端发起的旁路辅助请求（例如携带或未携带 tools 的建议输入、生成会话标题或后台 <transcript> 摘要），
// 使其使用独立一次性会话键与不写库隔离，防止覆盖或污染主编程/文档会话缓存。
func isAuxiliarySideRequest(req *ChatRequest, prompt string) bool {
	if req == nil {
		return false
	}
	if isSuggestionModeRequest(req) || isTranscriptSideRequest(req) {
		return true
	}
	if len(req.Tools) > 0 || len(req.Functions) > 0 {
		return false
	}
	var candidates []string
	var userTexts []string
	for _, m := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		text := strings.TrimSpace(contentText(m.Content))
		if text == "" {
			continue
		}
		if len(text) > 1024 {
			text = text[:1024]
		}
		if role == "system" || role == "developer" {
			candidates = append(candidates, text)
		} else if role == "" || role == "user" {
			userTexts = append(userTexts, text)
		}
	}
	if len(userTexts) > 2 {
		userTexts = userTexts[len(userTexts)-2:]
	}
	candidates = append(candidates, userTexts...)
	if trimmed := strings.TrimSpace(prompt); trimmed != "" {
		if len(trimmed) > 1024 {
			trimmed = trimmed[:1024]
		}
		candidates = append(candidates, trimmed)
	}
	lower := strings.ToLower(strings.Join(candidates, "\n"))
	for _, sig := range []string{
		"[suggestion mode:",
		"you are coming up with a succinct title",
		"generate a concise title",
		"write the title in the predominant language of the session",
	} {
		if strings.Contains(lower, sig) {
			return true
		}
	}
	return false
}

// trimSuggestionMessages 为建议模式旁路请求裁剪历史窗口：
// 仅保留末尾最多 6 条非系统消息，将历史消息压平为纯文本并截断超长正文、清空巨型工具调用与图片载荷，
// 同时完整保留最后一条 [SUGGESTION MODE: ...] 指令原文，绝不修改调用方原始消息切片。
func trimSuggestionMessages(msgs []Message) []Message {
	const (
		maxSuggestionWindow = 6
		maxHistoryRunes     = 2000
	)
	nonSys := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "system" || role == "developer" {
			continue
		}
		nonSys = append(nonSys, m)
	}
	if len(nonSys) > maxSuggestionWindow {
		nonSys = nonSys[len(nonSys)-maxSuggestionWindow:]
	}
	out := make([]Message, 0, len(nonSys))
	for i, m := range nonSys {
		if i == len(nonSys)-1 {
			out = append(out, Message{
				Role:    m.Role,
				Content: contentText(m.Content),
				Name:    m.Name,
			})
			continue
		}
		text := strings.TrimSpace(contentText(m.Content))
		if text == "" && (len(m.ToolCalls) > 0 || m.FunctionCall != nil) {
			var names []string
			for _, tc := range m.ToolCalls {
				if n := strings.TrimSpace(tc.Function.Name); n != "" {
					names = append(names, n)
				}
			}
			if m.FunctionCall != nil {
				if n := strings.TrimSpace(m.FunctionCall.Name); n != "" {
					names = append(names, n)
				}
			}
			if len(names) > 0 {
				text = "[called tools: " + strings.Join(names, ", ") + "]"
			} else {
				text = "[called tools]"
			}
		}
		if utf8.RuneCountInString(text) > maxHistoryRunes {
			runes := []rune(text)
			text = strings.TrimSpace(string(runes[:maxHistoryRunes])) + "...[truncated]"
		}
		out = append(out, Message{
			Role:       m.Role,
			Content:    text,
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
		})
	}
	return out
}

const (
	maxTranscriptInnerBytes = 12000
	transcriptKeepHeadBytes = 2500
	transcriptKeepTailBytes = 9000
	transcriptOmitMarker    = "\n...[earlier transcript entries omitted for fast classification]...\n"
)

// trimTranscriptMessages 对无工具的 <transcript> 旁路分类/摘要请求执行结构感知裁剪：
// 100% 保留所有 system / developer 消息（分类器输出格式规范位于此处），
// 并保留末尾有效 <transcript>...</transcript> 之外的闭合标签与待审指令，仅裁剪超长中间历史。
func trimTranscriptMessages(msgs []Message) []Message {
	if len(msgs) == 0 {
		return nil
	}
	targetIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		role := strings.ToLower(strings.TrimSpace(msgs[i].Role))
		if role == "system" || role == "developer" {
			continue
		}
		if role == "" || role == "user" {
			trimmed := strings.ToLower(strings.TrimSpace(contentText(msgs[i].Content)))
			if strings.HasPrefix(trimmed, "<transcript>") {
				targetIdx = i
			}
		}
		break
	}
	if targetIdx < 0 {
		return nil
	}
	out := make([]Message, len(msgs))
	copy(out, msgs)
	orig := out[targetIdx]
	out[targetIdx] = Message{
		Role:       orig.Role,
		Content:    trimTranscriptText(contentText(orig.Content)),
		Name:       orig.Name,
		ToolCallID: orig.ToolCallID,
	}
	return out
}

// trimTranscriptText 在保留首部任务目标与 </transcript> 闭合后待审指令的前提下，裁剪超长中间转录历史。
func trimTranscriptText(raw string) string {
	lower := strings.ToLower(raw)
	openIdx := strings.Index(lower, "<transcript>")
	closeIdx := strings.LastIndex(lower, "</transcript>")
	if openIdx >= 0 && closeIdx > openIdx+len("<transcript>") {
		innerStart := openIdx + len("<transcript>")
		inner := raw[innerStart:closeIdx]
		if len(inner) <= maxTranscriptInnerBytes {
			return raw
		}
		headEnd := transcriptKeepHeadBytes
		if headEnd > len(inner) {
			headEnd = len(inner)
		}
		for headEnd > 0 && headEnd < len(inner) && !utf8.RuneStart(inner[headEnd]) {
			headEnd--
		}
		tailStart := len(inner) - transcriptKeepTailBytes
		if tailStart < headEnd {
			tailStart = headEnd
		}
		for tailStart < len(inner) && !utf8.RuneStart(inner[tailStart]) {
			tailStart++
		}
		if tailStart <= headEnd {
			return raw
		}
		return raw[:innerStart] + inner[:headEnd] + transcriptOmitMarker + inner[tailStart:] + raw[closeIdx:]
	}
	if len(raw) <= maxTranscriptInnerBytes {
		return raw
	}
	headEnd := transcriptKeepHeadBytes
	if headEnd > len(raw) {
		headEnd = len(raw)
	}
	for headEnd > 0 && headEnd < len(raw) && !utf8.RuneStart(raw[headEnd]) {
		headEnd--
	}
	tailStart := len(raw) - 9500
	if tailStart < headEnd {
		tailStart = headEnd
	}
	for tailStart < len(raw) && !utf8.RuneStart(raw[tailStart]) {
		tailStart++
	}
	if tailStart <= headEnd {
		return raw
	}
	return raw[:headEnd] + transcriptOmitMarker + raw[tailStart:]
}

// hasToolResultHistory 检测当前对话历史中是否已有客户端工具执行结果回传，
// 用于区分“首次建立工具协作的破冰轮次”与“已读入文件后的深度分析轮次”。
func hasToolResultHistory(msgs []Message) bool {
	for _, m := range msgs {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "tool" || role == "function" || len(m.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// applyWorkModeGuards 针对文档与学术办公场景的首轮工具调用执行平滑破冰降档：
// 首轮尚未收到工具回传时，将 high / max 暂降为 medium，抑制过长自我怀疑引发的拒答；
// 第二轮开始已存在工具回传记录后，恢复原本的 high / max 深度推理能力。
func applyWorkModeGuards(effort string, explicitOff bool, isCompact bool, bridgeActive bool, choiceMode string, workMode string, hasToolHistory bool) (string, string) {
	if config.NormalizeWorkMode(workMode) != config.WorkModeDocument {
		return effort, ""
	}
	if isCompact || explicitOff || !bridgeActive || choiceMode == "none" || hasToolHistory {
		return effort, ""
	}
	if effort == "high" || effort == "max" {
		return "medium", "document_first_turn_icebreak"
	}
	return effort, ""
}

// compactGuidelinesInline 在文档办公模式不希望上传 guidelines-*.txt 附件触发防注入警觉时，
// 构造精简内联版 <workspace_guidelines>，保留环境目录摘要并优先保护完整 <client_tool_protocol>。
func compactGuidelinesInline(envSummary string) string {
	var b strings.Builder
	b.WriteString("<workspace_guidelines>\n")
	b.WriteString("Client workspace preferences summarized inline to keep focus on the user's documents and local actions.")
	if trimmed := strings.TrimSpace(envSummary); trimmed != "" {
		b.WriteString("\n\nKey workspace environment summary:\n")
		b.WriteString(trimmed)
	}
	b.WriteString("\n</workspace_guidelines>")
	return b.String()
}

// extractEnvSummary 从完整工作区规范中提取 <env> 环境块与关键工作目录元信息，
// 在规范外置为附件时仍将其保留在主消息帧内，消除模型翻查附件路径引发的多余思考。
func extractEnvSummary(rawGuidelines string) string {
	if strings.TrimSpace(rawGuidelines) == "" {
		return ""
	}

	var lines []string
	seen := make(map[string]bool)
	addLine := func(line string) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			return
		}
		key := strings.ToLower(trimmed)
		if seen[key] {
			return
		}
		seen[key] = true
		lines = append(lines, trimmed)
	}

	if matches := envTagRe.FindAllStringSubmatch(rawGuidelines, -1); len(matches) > 0 {
		for _, m := range matches {
			if len(m) < 2 {
				continue
			}
			for _, ln := range strings.Split(m[1], "\n") {
				addLine(ln)
			}
		}
	}

	for _, ln := range strings.Split(rawGuidelines, "\n") {
		if envKeyLineRe.MatchString(ln) {
			addLine(ln)
		}
	}

	if len(lines) == 0 {
		return ""
	}

	summary := strings.Join(lines, "\n")
	const maxEnvSummaryBytes = 800
	if len(summary) > maxEnvSummaryBytes {
		n := maxEnvSummaryBytes
		for n > 0 && !utf8.RuneStart(summary[n]) {
			n--
		}
		summary = strings.TrimSpace(summary[:n])
	}
	return summary
}

var (
	// alwaysForbiddenRemoteToolKeywords 无论当前是否上传附件或传入何种参数，一律 0 容忍直接阻断。
	alwaysForbiddenRemoteToolKeywords = []string{
		"web_search", "websearch", "web_fetch", "webfetch", "search_web", "online_search",
		"internet", "browser", "browse", "download", "curl", "wget", "http_request", "fetch_url", "fetch",
		"write", "create_file", "edit_file", "str_replace", "save_file", "delete", "remove_file", "patch", "mkdir",
		"quick_space", "space_search", "search_space", "knowledge", "rag_",
	}
	// scriptRemoteToolKeywords 在无本轮上传附件时 0 容忍拦截；有上传附件时仅允许最多 2 次纯只读附件切片查看。
	scriptRemoteToolKeywords = []string{
		"bash", "shell", "exec", "terminal", "command", "python", "code_interpreter", "run_code", "sandbox", "script",
	}
	readonlyRemoteToolKeywords = []string{
		"read", "view", "cat", "open", "inspect", "attachment", "file", "list",
		"ripgrep", "rg", "grep", "fdfind", "fd", "find", "glob", "ls", "head", "tail", "stat", "wc",
	}
	dangerousRemoteCodeMarkers = []string{
		"http://", "https://", "urllib", "requests", "httpx", "aiohttp", "curl ", "wget ", "socket.", "fetch(",
		".write(", "write_text(", "write_bytes(", "mode='w", `mode="w`, ", 'w'", `, "w"`, ", 'wb'", `, "wb"`, ", 'a'", `, "a"`,
		"shutil.", "os.makedirs", "mkdir", "ffmpeg", "moviepy", "cv2", "manim", "remotion", "playwright", "puppeteer",
		"subprocess", "os.system", "pip install", "npm ", "npx ", "git ", ">> ", " > ", "tee ",
	}
	readonlyRemoteCodeMarkers = []string{
		".txt", ".pdf", ".png", ".jpg", ".md", ".json", ".csv", ".webp", ".gif", ".jpeg",
		"s3files", "open(", "read", "listdir", "glob", "pathlib", "os.walk",
		"os.getcwd", "os.path", "os.stat", "linecache", "readlines", "splitlines", "json.load",
		"cat ", "head ", "tail ", "ls ", "sed ", "awk ", "wc ", "grep ", "rg ",
	}
	windowsDrivePathRe = regexp.MustCompile(`(?:^|[^a-z0-9])[a-z]:[\\/]`)
)

func argsLookLikeUploadedAttachment(args string) bool {
	lower := strings.ToLower(args)
	for _, marker := range []string{"/data/s3files/", "prompt-", "guidelines-", "img-", "doc-"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// argsHasDangerousRemoteAction 检查远端脚本/命令行参数是否包含联网、下载、写文件、子进程或媒体渲染等危险操作。
func argsHasDangerousRemoteAction(args string) bool {
	norm := strings.NewReplacer(`\"`, `"`, `\'`, `'`, `\\`, `\`, `\u003e`, `>`, `\u003c`, `<`).Replace(strings.ToLower(args))
	for _, marker := range dangerousRemoteCodeMarkers {
		if strings.Contains(norm, marker) {
			return true
		}
	}
	return false
}

// argsLookLikeReadonlyAttachmentScript 检查完整后的脚本参数是否确系对已上传附件的只读检索或切片读取。
func argsLookLikeReadonlyAttachmentScript(args string) bool {
	if argsLookLikeUploadedAttachment(args) {
		return true
	}
	norm := strings.NewReplacer(`\"`, `"`, `\'`, `'`, `\\`, `\`).Replace(strings.ToLower(args))
	for _, marker := range readonlyRemoteCodeMarkers {
		if strings.Contains(norm, marker) {
			return true
		}
	}
	return false
}

// hasSubstantiveToolArgs 判断工具参数是否已包含实质内容，排除上游在 tool_start/占位帧中发送的空对象 "{}" 或 null。
func hasSubstantiveToolArgs(args string) bool {
	trimmed := strings.TrimSpace(args)
	switch trimmed {
	case "", "{}", "null", "[]", `""`:
		return false
	default:
		return true
	}
}

// argsTargetUserLocalPath 检测远端只读工具参数是否试图探测用户的本地盘符或客户端会话目录。
func argsTargetUserLocalPath(args string) bool {
	if argsLookLikeUploadedAttachment(args) {
		return false
	}
	normalized := strings.ReplaceAll(strings.ToLower(args), `\\`, `\`)
	if windowsDrivePathRe.MatchString(normalized) {
		return true
	}
	for _, prefix := range []string{"/sessions/", "/users/", "/home/"} {
		if strings.Contains(normalized, prefix) {
			return true
		}
	}
	return false
}

// classifyRemoteSandboxTool 按四级策略审计启用客户端工具桥接时的远端原生工具调用：
// 1) 纯越权联网/下载/写文件工具一律 0 容忍立即拦截；
// 2) 脚本/命令行工具仅在携带上传附件且无危险操作时允许有限次数只读查看附件；
// 3) 只读文件/检索工具（含 ripgrep/fdfind）放行有限次数查看云端附件，但指向本机盘符仍立刻拦截；
// 4) 未知陌生工具一律拦截。
func classifyRemoteSandboxTool(name, args string, argsFinal, hasUploadedAttachments bool, allowedReadCount, scriptReadCount int) (blocked bool, reason string, isScriptRead bool) {
	lowerName := strings.ToLower(strings.TrimSpace(name))
	if lowerName == "" {
		return false, "", false
	}
	for _, kw := range alwaysForbiddenRemoteToolKeywords {
		if strings.Contains(lowerName, kw) {
			return true, "forbidden_remote_tool", false
		}
	}
	for _, kw := range scriptRemoteToolKeywords {
		if strings.Contains(lowerName, kw) {
			if !hasUploadedAttachments {
				return true, "forbidden_remote_tool", false
			}
			if argsHasDangerousRemoteAction(args) {
				return true, "forbidden_remote_code_action", false
			}
			if argsTargetUserLocalPath(args) {
				return true, "remote_tool_targeting_local_path", false
			}
			if scriptReadCount >= 2 {
				return true, "remote_read_limit_exceeded", false
			}
			if argsFinal && hasSubstantiveToolArgs(args) && !argsLookLikeReadonlyAttachmentScript(args) {
				return true, "forbidden_remote_tool", false
			}
			return false, "", true
		}
	}
	isReadCandidate := argsLookLikeUploadedAttachment(args)
	if !isReadCandidate {
		for _, kw := range readonlyRemoteToolKeywords {
			if strings.Contains(lowerName, kw) {
				isReadCandidate = true
				break
			}
		}
	}
	if isReadCandidate {
		if argsTargetUserLocalPath(args) {
			return true, "remote_tool_targeting_local_path", false
		}
		if hasUploadedAttachments {
			if allowedReadCount >= 6 {
				return true, "remote_read_limit_exceeded", false
			}
			return false, "", false
		}
		if allowedReadCount >= 2 {
			return true, "remote_read_limit_exceeded", false
		}
		return false, "", false
	}
	return true, "unrecognized_remote_tool", false
}
