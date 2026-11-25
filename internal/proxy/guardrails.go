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
