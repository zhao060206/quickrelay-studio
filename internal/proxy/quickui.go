package proxy

import (
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strings"
	"unicode"
)

type decisionOption struct {
	Label       string
	Description string
}

type decisionBlock struct {
	Question string
	Options  []decisionOption
}

var (
	reQuickCitation   = regexp.MustCompile(`[ \t]*\[@[0-9a-fA-F]{6,8}\]`)
	reDecisionOpenTag = regexp.MustCompile(`(?is)^<decision(?:\s+([^>]*))?>`)
	reDecisionClose   = regexp.MustCompile(`(?is)</decision>`)
	reOptionElem      = regexp.MustCompile(`(?is)<option(?:\s+([^>]*))?>(.*?)</option>`)
	reHTMLAttr        = regexp.MustCompile(`([a-zA-Z_:][a-zA-Z0-9_.:-]*)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
)

// parseDecisionBlock 解析 AWS Quick 私有 <decision question="...">...</decision> 块。
func parseDecisionBlock(raw string) (decisionBlock, bool) {
	openMatch := reDecisionOpenTag.FindStringSubmatchIndex(raw)
	if len(openMatch) < 2 {
		return decisionBlock{}, false
	}
	closeLoc := reDecisionClose.FindStringIndex(raw[openMatch[1]:])
	if len(closeLoc) < 2 {
		return decisionBlock{}, false
	}
	attrs := ""
	if openMatch[2] >= 0 && openMatch[3] >= 0 {
		attrs = raw[openMatch[2]:openMatch[3]]
	}
	question := strings.TrimSpace(extractHTMLAttr(attrs, "question"))
	inner := raw[openMatch[1] : openMatch[1]+closeLoc[0]]

	optMatches := reOptionElem.FindAllStringSubmatch(inner, -1)
	if len(optMatches) == 0 {
		return decisionBlock{}, false
	}
	options := make([]decisionOption, 0, len(optMatches))
	for _, m := range optMatches {
		optAttrs := ""
		if len(m) > 1 {
			optAttrs = m[1]
		}
		label := strings.TrimSpace(html.UnescapeString(stripInlineWhitespace(m[2])))
		desc := strings.TrimSpace(html.UnescapeString(stripInlineWhitespace(extractHTMLAttr(optAttrs, "description"))))
		if label == "" && desc == "" {
			continue
		}
		if label == "" {
			label = desc
		}
		options = append(options, decisionOption{
			Label:       label,
			Description: desc,
		})
	}
	if len(options) == 0 {
		return decisionBlock{}, false
	}
	return decisionBlock{
		Question: question,
		Options:  options,
	}, true
}

func extractHTMLAttr(attrText, name string) string {
	for _, m := range reHTMLAttr.FindAllStringSubmatch(attrText, -1) {
		if strings.EqualFold(m[1], name) {
			if m[2] != "" {
				return html.UnescapeString(m[2])
			}
			return html.UnescapeString(m[3])
		}
	}
	return ""
}

func stripInlineWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func containsNonASCII(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII {
			return true
		}
	}
	return false
}

// convertDecision 优先把 AWS Quick <decision> 块转换为客户端原生提问工具调用（Claude Code 的 AskUserQuestion
// 或 Codex 的 request_user_input）；若客户端未提供对应工具或当前不允许发起调用，则降级为干净 Markdown 列表。
func convertDecision(block decisionBlock, allowed map[string]bool, canEmitCall func(ToolCallReply) bool) (markdown string, call *ToolCallReply) {
	if len(block.Options) >= 2 && len(allowed) > 0 && canEmitCall != nil {
		question := block.Question
		if question == "" {
			question = "请选择下一步操作"
		}
		header := "Next step"
		if containsNonASCII(question) || containsNonASCII(block.Options[0].Label) {
			header = "下一步"
		}

		// 1. 优先匹配 Claude Code 原生 AskUserQuestion 工具
		if askName := resolveToolName("AskUserQuestion", allowed); askName != "" {
			limit := min(4, len(block.Options))
			opts := make([]map[string]string, 0, limit)
			for _, o := range block.Options[:limit] {
				desc := o.Description
				if desc == "" {
					desc = o.Label
				}
				opts = append(opts, map[string]string{
					"label":       o.Label,
					"description": desc,
				})
			}
			argsBytes, err := json.Marshal(map[string]any{
				"questions": []map[string]any{{
					"question":    question,
					"header":      header,
					"multiSelect": false,
					"options":     opts,
				}},
			})
			if err == nil {
				tc := ToolCallReply{
					ID:        "call_" + strings.ReplaceAll(uuid(), "-", ""),
					Name:      askName,
					Arguments: string(argsBytes),
				}
				if canEmitCall(tc) {
					return "", &tc
				}
			}
		}

		// 2. 次选匹配 Codex Plan 模式原生 request_user_input 工具（严格不附加 multiSelect）
		if reqInputName := resolveToolName("request_user_input", allowed); reqInputName != "" {
			limit := min(3, len(block.Options))
			opts := make([]map[string]string, 0, limit)
			for _, o := range block.Options[:limit] {
				desc := o.Description
				if desc == "" {
					desc = o.Label
				}
				opts = append(opts, map[string]string{
					"label":       o.Label,
					"description": desc,
				})
			}
			argsBytes, err := json.Marshal(map[string]any{
				"questions": []map[string]any{{
					"id":       "next_step",
					"header":   header,
					"question": question,
					"options":  opts,
				}},
			})
			if err == nil {
				tc := ToolCallReply{
					ID:        "call_" + strings.ReplaceAll(uuid(), "-", ""),
					Name:      reqInputName,
					Arguments: string(argsBytes),
				}
				if canEmitCall(tc) {
					return "", &tc
				}
			}
		}
	}

	return formatDecisionMarkdown(block), nil
}

// formatDecisionMarkdown 将 <decision> 块排版为清爽的标准 Markdown 编号列表。
func formatDecisionMarkdown(block decisionBlock) string {
	var b strings.Builder
	b.WriteString("\n\n")
	title := block.Question
	if title == "" {
		title = "请选择下一步操作"
	}
	fmt.Fprintf(&b, "**%s**\n", title)
	for i, o := range block.Options {
		if o.Description != "" {
			fmt.Fprintf(&b, "%d. **%s**：%s\n", i+1, o.Label, o.Description)
		} else {
			fmt.Fprintf(&b, "%d. **%s**\n", i+1, o.Label)
		}
	}
	return b.String()
}

// quickOutputFilter 在流式与非流式输出中统一清洗 AWS Quick 私有引用角标 [@xxxxxx]
// 与 <decision> 选项卡片，并确保 Markdown 围栏代码块内部内容完全不受破坏。
type quickOutputFilter struct {
	allowed     map[string]bool
	canEmitCall func(ToolCallReply) bool

	buf         string
	inFence     bool
	inDecision  bool
	atLineStart bool
}

func newQuickOutputFilter(allowed map[string]bool, canEmitCall func(ToolCallReply) bool) *quickOutputFilter {
	return &quickOutputFilter{
		allowed:     allowed,
		canEmitCall: canEmitCall,
		atLineStart: true,
	}
}

func (f *quickOutputFilter) Reset() {
	f.buf = ""
	f.inFence = false
	f.inDecision = false
	f.atLineStart = true
}

func (f *quickOutputFilter) updateLineState(emitted string) {
	if emitted == "" {
		return
	}
	lastNL := strings.LastIndexByte(emitted, '\n')
	tail := emitted
	if lastNL >= 0 {
		tail = emitted[lastNL+1:]
	} else if !f.atLineStart {
		return
	}
	// 行首允许有最多 3 个空格再起围栏或新内容
	if lastNL >= 0 || f.atLineStart {
		for _, r := range tail {
			if r != ' ' && r != '\t' {
				f.atLineStart = false
				return
			}
		}
		f.atLineStart = len(tail) <= 3
	}
}

// Feed 输入文本增量，流式返回清洗后的安全正文及转换出的原生选项工具调用。
func (f *quickOutputFilter) Feed(delta string) (outText string, outCalls []ToolCallReply) {
	if delta == "" {
		return "", nil
	}
	f.buf += delta
	var out strings.Builder

	emit := func(s string) {
		if s == "" {
			return
		}
		out.WriteString(s)
		f.updateLineState(s)
	}

	for f.buf != "" {
		if f.inDecision {
			loc := reDecisionClose.FindStringIndex(f.buf)
			if len(loc) < 2 {
				// 防御异常超长不闭合块
				if len(f.buf) > 32*1024 {
					raw := f.buf
					f.buf = ""
					f.inDecision = false
					emit(raw)
				}
				break
			}
			rawBlock := f.buf[:loc[1]]
			f.buf = f.buf[loc[1]:]
			f.inDecision = false

			if block, ok := parseDecisionBlock(rawBlock); ok {
				md, tc := convertDecision(block, f.allowed, f.canEmitCall)
				if tc != nil {
					outCalls = append(outCalls, *tc)
				} else {
					emit(md)
				}
			} else {
				emit(reQuickCitation.ReplaceAllString(rawBlock, ""))
			}
			continue
		}

		if f.inFence {
			if f.atLineStart {
				if hold, toggled, consumed := f.checkFenceToggle(true); hold {
					break
				} else if toggled {
					emit(consumed)
					f.buf = f.buf[len(consumed):]
					f.inFence = false
					continue
				}
			}
			// 代码块内仅按行寻址下一可能闭合围栏的位置，其余原封不动放出
			nl := strings.IndexByte(f.buf, '\n')
			if nl >= 0 {
				emit(f.buf[:nl+1])
				f.buf = f.buf[nl+1:]
				continue
			}
			if f.atLineStart && len(f.buf) <= 8 {
				// 仍处于行首且很短，等待换行或第 3 个反引号确认是否结束围栏
				break
			}
			emit(f.buf)
			f.buf = ""
			break
		}

		// 普通正文状态：按行边界处理，确保同一片中跨入围栏代码块时不误洗代码块内部内容
		if f.atLineStart {
			if hold, toggled, consumed := f.checkFenceToggle(false); hold {
				break
			} else if toggled {
				emit(consumed)
				f.buf = f.buf[len(consumed):]
				f.inFence = true
				continue
			}
		}

		// 若缓冲区包含换行，先只处理第一行（或换行之前的 <decision> 起始块）
		if nl := strings.IndexByte(f.buf, '\n'); nl >= 0 {
			line := f.buf[:nl+1]
			if decIdx := findDecisionOpenIndex(line); decIdx >= 0 {
				if decIdx > 0 {
					emit(reQuickCitation.ReplaceAllString(f.buf[:decIdx], ""))
					f.buf = f.buf[decIdx:]
				}
				f.inDecision = true
				continue
			}
			emit(reQuickCitation.ReplaceAllString(line, ""))
			f.buf = f.buf[nl+1:]
			continue
		}

		// 单行无换行增量：安全消除当前行中完整出现的私有引用 [@xxxxxx]
		cleaned := reQuickCitation.ReplaceAllString(f.buf, "")
		if cleaned != f.buf {
			f.buf = cleaned
			continue
		}

		// 检查单行中是否出现完整闭合起始标签 <decision...>
		if decIdx := findDecisionOpenIndex(f.buf); decIdx >= 0 {
			if decIdx > 0 {
				emit(f.buf[:decIdx])
				f.buf = f.buf[decIdx:]
			}
			f.inDecision = true
			continue
		}

		// 仅保留尾部可能尚未集齐前缀字节的短尾巴（<decision / [ \t]*[@... / 行首反引号），其余安全放行
		hold := trailingQuickHoldLen(f.buf, f.atLineStart)
		safeLen := len(f.buf) - hold
		if safeLen > 0 {
			safe := f.buf[:safeLen]
			emit(safe)
			f.buf = f.buf[safeLen:]
		}
		if hold > 0 {
			break
		}
	}

	return out.String(), outCalls
}

// Flush 在响应流结束后刷出所有缓冲残留；若出现极端未闭合标签则安全回退透传。
func (f *quickOutputFilter) Flush() (outText string, outCalls []ToolCallReply) {
	if f.buf == "" {
		return "", nil
	}
	rem := f.buf
	f.buf = ""
	if f.inFence {
		f.inFence = false
		return rem, nil
	}
	if f.inDecision {
		f.inDecision = false
		if block, ok := parseDecisionBlock(rem); ok {
			md, tc := convertDecision(block, f.allowed, f.canEmitCall)
			if tc != nil {
				return "", []ToolCallReply{*tc}
			}
			return md, nil
		}
	}
	cleaned := reQuickCitation.ReplaceAllString(rem, "")
	return cleaned, nil
}

// checkFenceToggle 检测在行首状态下是否遇到了开启或闭合的 Markdown 围栏 ```` ``` ````。
func (f *quickOutputFilter) checkFenceToggle(closing bool) (waitMore, toggled bool, consumed string) {
	i := 0
	for i < len(f.buf) && i < 3 && (f.buf[i] == ' ' || f.buf[i] == '\t') {
		i++
	}
	if i == len(f.buf) {
		return !closing && len(f.buf) <= 3, false, ""
	}
	if f.buf[i] != '`' {
		return false, false, ""
	}
	j := i
	for j < len(f.buf) && f.buf[j] == '`' {
		j++
	}
	if j-i < 3 {
		if j == len(f.buf) {
			return true, false, ""
		}
		return false, false, ""
	}
	if !closing {
		return false, true, f.buf[:j]
	}
	// 闭合围栏后面同一行只能有空白（直到换行）
	nl := strings.IndexByte(f.buf[j:], '\n')
	if nl < 0 {
		for k := j; k < len(f.buf); k++ {
			if f.buf[k] != ' ' && f.buf[k] != '\t' && f.buf[k] != '\r' {
				return false, false, ""
			}
		}
		return true, false, ""
	}
	lineRest := f.buf[j : j+nl]
	if strings.TrimSpace(lineRest) != "" {
		return false, false, ""
	}
	return false, true, f.buf[:j+nl+1]
}

// findDecisionOpenIndex 查找文本中首次出现的完整 <decision 起始位置（后面需紧跟空白或 '>'）。
func findDecisionOpenIndex(s string) int {
	lower := strings.ToLower(s)
	offset := 0
	for {
		idx := strings.Index(lower[offset:], "<decision")
		if idx < 0 {
			return -1
		}
		abs := offset + idx
		after := abs + len("<decision")
		if after < len(s) {
			ch := s[after]
			if ch == '>' || ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' {
				return abs
			}
		}
		offset = abs + 1
	}
}

// trailingQuickHoldLen 返回字符串末尾可能尚未接收完毕、需要等待下一流片再判决的最小尾部字节数。
func trailingQuickHoldLen(s string, atLineStart bool) int {
	maxHold := 0

	// 1. 保留末尾可能构成 "<decision"（及紧随的判断字符）的未完成前缀
	lower := strings.ToLower(s)
	if lastLT := strings.LastIndexByte(lower, '<'); lastLT >= 0 {
		tail := lower[lastLT:]
		const decPrefix = "<decision"
		if len(tail) <= len(decPrefix) && strings.HasPrefix(decPrefix, tail) {
			if h := len(s) - lastLT; h > maxHold {
				maxHold = h
			}
		}
	}

	// 2. 保留末尾可能构成 "[ \t]*\[@[0-9a-fA-F]{6,8}\]" 的未完成前缀
	tailStart := max(0, len(s)-16)
	sub := s[tailStart:]
	if lastLB := strings.LastIndexByte(sub, '['); lastLB >= 0 {
		cand := sub[lastLB:]
		if isPartialQuickCitation(cand) {
			// 把紧贴在 '[' 前面的少量水平空白一起兜住，保证抹除引用时不留冗余空格
			wsStart := tailStart + lastLB
			for wsStart > 0 && (s[wsStart-1] == ' ' || s[wsStart-1] == '\t') && (tailStart+lastLB-wsStart) < 4 {
				wsStart--
			}
			if h := len(s) - wsStart; h > maxHold {
				maxHold = h
			}
		}
	} else {
		// 末尾紧随的 1-2 个水平空白，防止下一个 chunk 直接以 "[@" 开头导致前导空格已吐出
		wsCount := 0
		for i := len(s) - 1; i >= 0 && (s[i] == ' ' || s[i] == '\t') && wsCount < 4; i-- {
			wsCount++
		}
		if wsCount > 0 && wsCount > maxHold {
			maxHold = wsCount
		}
	}

	// 3. 若刚好在行首或刚吐完换行，等待末尾的少量空白和 1-2 个反引号以防漏判代码围栏
	if lastNL := strings.LastIndexByte(s, '\n'); lastNL >= 0 || atLineStart {
		lineTail := s
		if lastNL >= 0 {
			lineTail = s[lastNL+1:]
		}
		if len(lineTail) <= 6 {
			i := 0
			for i < len(lineTail) && i < 3 && (lineTail[i] == ' ' || lineTail[i] == '\t') {
				i++
			}
			onlyBackticks := i < len(lineTail)
			for j := i; j < len(lineTail); j++ {
				if lineTail[j] != '`' {
					onlyBackticks = false
					break
				}
			}
			if onlyBackticks && len(lineTail) > maxHold {
				maxHold = len(lineTail)
			}
		}
	}

	return maxHold
}

func isPartialQuickCitation(cand string) bool {
	if cand == "[" {
		return true
	}
	if !strings.HasPrefix(cand, "[@") {
		return false
	}
	hexPart := cand[2:]
	if len(hexPart) > 8 {
		return false
	}
	for i := 0; i < len(hexPart); i++ {
		c := hexPart[i]
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

// cleanQuickOutput 使用与流式模式完全一致的状态机对整段响应执行一次性私有标签清洗与原生选项转换。
func cleanQuickOutput(text string, allowed map[string]bool, canEmitCall func(ToolCallReply) bool) (string, []ToolCallReply) {
	f := newQuickOutputFilter(allowed, canEmitCall)
	out1, calls1 := f.Feed(text)
	out2, calls2 := f.Flush()
	var calls []ToolCallReply
	calls = append(calls, calls1...)
	calls = append(calls, calls2...)
	return out1 + out2, calls
}
