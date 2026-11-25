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

// isAuxiliarySideRequest 识别客户端发起的无工具旁路辅助请求（例如建议输入、生成会话标题），
// 使其使用独立一次性会话键与不写库隔离，防止覆盖或污染主编程/文档会话缓存。
func isAuxiliarySideRequest(req *ChatRequest, prompt string) bool {
	if req == nil || len(req.Tools) > 0 || len(req.Functions) > 0 {
		return false
	}
	var parts []string
	for _, m := range req.Messages {
		if text := strings.TrimSpace(contentText(m.Content)); text != "" {
			if len(text) > 1024 {
				text = text[:1024]
			}
			parts = append(parts, text)
		}
	}
	if trimmed := strings.TrimSpace(prompt); trimmed != "" {
		if len(trimmed) > 1024 {
			trimmed = trimmed[:1024]
		}
		parts = append(parts, trimmed)
	}
	lower := strings.ToLower(strings.Join(parts, "\n"))
	for _, sig := range []string{
		"[suggestion mode:",
		"you are coming up with a succinct title for an agent chat session",
		"generate a concise title",
	} {
		if strings.Contains(lower, sig) {
			return true
		}
	}
	return false
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
