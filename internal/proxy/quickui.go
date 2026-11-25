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
	Multi    bool
	Options  []decisionOption
}

var (
	reQuickCitation = regexp.MustCompile(`@[0-9a-fA-F]{6,8}(?:[\s,;]+@[0-9a-fA-F]{6,8})*`)
	reHexCitationID = regexp.MustCompile(`^@[0-9a-fA-F]{6,8}$`)

	reDecisionOpenTag = regexp.MustCompile(`(?is)^<decision(?:\s+([^>]*))?>`)
	reDecisionClose   = regexp.MustCompile(`(?is)(?:</invoke>\s*</decision>|</decision>|</invoke>)`)
	reDecisionsClose  = regexp.MustCompile(`(?is)</decisions>`)
	reDecisionAny     = regexp.MustCompile(`(?is)<decision(?:\s+[^>]*)?>[\s\S]*?(?:</invoke>\s*</decision>|</decision>|</invoke>)`)
	reOptionElem      = regexp.MustCompile(`(?is)<option(?:\s+([^>]*))?>(.*?)</option>`)
	reHTMLAttr        = regexp.MustCompile(`([a-zA-Z_:][a-zA-Z0-9_.:-]*)\s*=\s*(?:"([^"]*)"|'([^']*)')`)

	reSourcesClose  = regexp.MustCompile(`(?is)</sources>`)
	reArtifactClose = regexp.MustCompile(`(?is)</artifact>`)

	reACPRoute         = regexp.MustCompile(`[ \t]*\[ACP_ROUTE:[^\]]*\]`)
	reNotchStyle       = regexp.MustCompile(`[ \t]*\[NOTCH_REPLY_STYLE:[^\]]*\]`)
	reDecisionResponse = regexp.MustCompile(`(?is)</?decision_response>`)
	reOrphanCloseTags  = regexp.MustCompile(`(?is)[ \t]*</(?:sources|decisions|decision|option|artifact)>`)
)

// parseDecisionBlock 解析 AWS Quick 私有 <decision question="...">...</decision> 块（兼容 </invoke> 闭合及 multi 属性）。
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
	multi := strings.EqualFold(strings.TrimSpace(extractHTMLAttr(attrs, "multi")), "true")
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
	if len(options) < 2 {
		return decisionBlock{}, false
	}
	return decisionBlock{
		Question: question,
		Multi:    multi,
		Options:  options,
	}, true
}

// parseDecisionsBlock 解析 <decisions>...</decisions> 组块中的全部有效 <decision> 问题。
func parseDecisionsBlock(raw string) ([]decisionBlock, bool) {
	var blocks []decisionBlock
	for _, match := range reDecisionAny.FindAllString(raw, -1) {
		if blk, ok := parseDecisionBlock(match); ok {
			blocks = append(blocks, blk)
		}
	}
	return blocks, len(blocks) > 0
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

// convertDecision 将单个 <decision> 块转换为原生工具调用或清爽 Markdown 编号列表。
func convertDecision(block decisionBlock, allowed map[string]bool, canEmitCall func(ToolCallReply) bool) (markdown string, call *ToolCallReply) {
	return convertDecisions([]decisionBlock{block}, allowed, canEmitCall)
}

// convertDecisions 优先把一个或多个 <decision> 块合并转换为客户端原生提问工具调用
// （Claude Code 的 AskUserQuestion 或 Codex 的 request_user_input）；无法使用原生工具时降级为 Markdown 编号列表。
func convertDecisions(blocks []decisionBlock, allowed map[string]bool, canEmitCall func(ToolCallReply) bool) (markdown string, call *ToolCallReply) {
	valid := make([]decisionBlock, 0, len(blocks))
	for _, b := range blocks {
		if len(b.Options) >= 2 {
			valid = append(valid, b)
		}
	}
	if len(valid) == 0 {
		return "", nil
	}
	if len(valid) > 4 {
		valid = valid[:4]
	}

	if len(allowed) > 0 && canEmitCall != nil {
		// 1. 优先匹配 Claude Code 原生 AskUserQuestion 工具（支持单题或多题及 multiSelect）
		if askName := resolveToolName("AskUserQuestion", allowed); askName != "" {
			qs := make([]map[string]any, 0, len(valid))
			for i, b := range valid {
				question := b.Question
				if question == "" {
					question = "请选择下一步操作"
				}
				header := fmt.Sprintf("Step %d", i+1)
				if len(valid) == 1 {
					header = "Next step"
				}
				if containsNonASCII(question) || containsNonASCII(b.Options[0].Label) {
					if len(valid) == 1 {
						header = "下一步"
					} else {
						header = fmt.Sprintf("问题 %d", i+1)
					}
				}
				limit := min(4, len(b.Options))
				opts := make([]map[string]string, 0, limit)
				for _, o := range b.Options[:limit] {
					desc := o.Description
					if desc == "" {
						desc = o.Label
					}
					opts = append(opts, map[string]string{
						"label":       o.Label,
						"description": desc,
					})
				}
				qs = append(qs, map[string]any{
					"question":    question,
					"header":      header,
					"multiSelect": b.Multi,
					"options":     opts,
				})
			}
			if argsBytes, err := json.Marshal(map[string]any{"questions": qs}); err == nil {
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
			qs := make([]map[string]any, 0, len(valid))
			for i, b := range valid {
				question := b.Question
				if question == "" {
					question = "请选择下一步操作"
				}
				header := "Next step"
				if len(valid) > 1 {
					header = fmt.Sprintf("Step %d", i+1)
				}
				if containsNonASCII(question) || containsNonASCII(b.Options[0].Label) {
					if len(valid) == 1 {
						header = "下一步"
					} else {
						header = fmt.Sprintf("问题 %d", i+1)
					}
				}
				limit := min(3, len(b.Options))
				opts := make([]map[string]string, 0, limit)
				for _, o := range b.Options[:limit] {
					desc := o.Description
					if desc == "" {
						desc = o.Label
					}
					opts = append(opts, map[string]string{
						"label":       o.Label,
						"description": desc,
					})
				}
				qid := "next_step"
				if i > 0 {
					qid = fmt.Sprintf("next_step_%d", i+1)
				}
				qs = append(qs, map[string]any{
					"id":       qid,
					"header":   header,
					"question": question,
					"options":  opts,
				})
			}
			if argsBytes, err := json.Marshal(map[string]any{"questions": qs}); err == nil {
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

	var md strings.Builder
	for _, b := range valid {
		md.WriteString(formatDecisionMarkdown(b))
	}
	return md.String(), nil
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

var privateQuickURLSchemes = []string{"kg://", "qw-file://", "qw-tool://", "mention://"}

func hasPrivateQuickScheme(rawURL string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(rawURL))
	for _, scheme := range privateQuickURLSchemes {
		if strings.HasPrefix(lower, scheme) {
			return scheme, true
		}
	}
	return "", false
}

func couldBePrivateQuickSchemePrefix(raw string) bool {
	lower := strings.ToLower(raw)
	if lower == "" {
		return true
	}
	for _, scheme := range privateQuickURLSchemes {
		if strings.HasPrefix(scheme, lower) || strings.HasPrefix(lower, scheme) {
			return true
		}
	}
	return false
}

// stripQuickInlineSegment 对非代码正文段执行一次性内联私有标记清洗：
// 1) 剔除私有协议链接（kg://, qw-file://, qw-tool://, mention://）：纯角标彻底删除，可读文本保留名称；
// 2) 剔除单引用 [@0cc622] 及多引用组 [@0cc622, @8dbdfb]（不误伤正常 Markdown 链接）；
// 3) 剔除平台内部路由样式标签、decision_response 外壳及孤立闭合标签。
func stripQuickInlineSegment(s string) string {
	if s == "" {
		return ""
	}
	s = stripPrivateQuickLinks(s)
	s = stripBareQuickCitations(s)
	s = reACPRoute.ReplaceAllString(s, "")
	s = reNotchStyle.ReplaceAllString(s, "")
	s = reDecisionResponse.ReplaceAllString(s, "")
	s = reOrphanCloseTags.ReplaceAllString(s, "")
	return s
}

// stripQuickInlineOutsideCode 按行内反引号跨度切分文本，仅清洗反引号之外的普通段落，确保行内代码原样保留。
func stripQuickInlineOutsideCode(s string) string {
	if s == "" {
		return ""
	}
	if !strings.ContainsRune(s, '`') {
		return stripQuickInlineSegment(s)
	}
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	plainStart := 0
	for i < len(s) {
		if s[i] != '`' {
			i++
			continue
		}
		tickLen := 1
		for i+tickLen < len(s) && s[i+tickLen] == '`' {
			tickLen++
		}
		closeIdx := findMatchingBackticks(s, i+tickLen, tickLen)
		if closeIdx < 0 {
			i += tickLen
			continue
		}
		if i > plainStart {
			out.WriteString(stripQuickInlineSegment(s[plainStart:i]))
		}
		out.WriteString(s[i : closeIdx+tickLen])
		i = closeIdx + tickLen
		plainStart = i
	}
	if plainStart < len(s) {
		out.WriteString(stripQuickInlineSegment(s[plainStart:]))
	}
	return out.String()
}

func findMatchingBackticks(s string, from, tickLen int) int {
	i := from
	for i < len(s) {
		if s[i] == '\n' {
			return -1
		}
		if s[i] != '`' {
			i++
			continue
		}
		run := 1
		for i+run < len(s) && s[i+run] == '`' {
			run++
		}
		if run == tickLen {
			return i
		}
		i += run
	}
	return -1
}

// stripBareQuickCitations 移除所有非 Markdown 链接形式的裸引用角标（如 [@0cc622] 或 [@0cc622, @8dbdfb]）及紧邻前导水平空白。
func stripBareQuickCitations(s string) string {
	if !strings.Contains(s, "[@") {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	last := 0
	i := 0
	for i < len(s) {
		idx := strings.Index(s[i:], "[@")
		if idx < 0 {
			break
		}
		lb := i + idx
		rbRel := strings.IndexByte(s[lb+1:], ']')
		if rbRel < 0 {
			i = lb + 2
			continue
		}
		rb := lb + 1 + rbRel
		inner := s[lb+1 : rb]
		if !reQuickCitation.MatchString(inner) || reQuickCitation.FindString(inner) != inner {
			i = lb + 2
			continue
		}
		// 若紧跟 '(' 则属于 Markdown 链接语法，交由或保留给链接解析
		if rb+1 < len(s) && s[rb+1] == '(' {
			i = rb + 1
			continue
		}
		wsStart := lb
		for wsStart > last && (s[wsStart-1] == ' ' || s[wsStart-1] == '\t') {
			wsStart--
		}
		out.WriteString(s[last:wsStart])
		last = rb + 1
		i = last
	}
	if last == 0 {
		return s
	}
	out.WriteString(s[last:])
	return out.String()
}

// stripPrivateQuickLinks 解析并清洗 kg://、qw-file://、qw-tool:// 与 mention:// 私有死链（兼容 [[1]](qw-tool://...) 两层方括号）。
func stripPrivateQuickLinks(s string) string {
	if !strings.Contains(s, "](") {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	last := 0
	i := 0
	for i < len(s) {
		if s[i] != '[' {
			i++
			continue
		}
		labelStart := i + 1
		depth := 1
		j := labelStart
		validLabel := true
		for j < len(s) && j-i <= 512 {
			ch := s[j]
			if ch == '\n' || ch == '\r' {
				validLabel = false
				break
			}
			if ch == '[' {
				depth++
				if depth > 2 {
					validLabel = false
					break
				}
			} else if ch == ']' {
				depth--
				if depth == 0 {
					break
				}
			}
			j++
		}
		if !validLabel || j >= len(s) || depth != 0 || j+1 >= len(s) || s[j+1] != '(' {
			i++
			continue
		}
		urlStart := j + 2
		urlEnd := urlStart
		validURL := true
		for urlEnd < len(s) && urlEnd-urlStart <= 1024 {
			ch := s[urlEnd]
			if ch == '\n' || ch == '\r' || ch == ' ' || ch == '\t' {
				validURL = false
				break
			}
			if ch == ')' {
				break
			}
			urlEnd++
		}
		if !validURL || urlEnd >= len(s) || s[urlEnd] != ')' {
			i++
			continue
		}
		rawURL := s[urlStart:urlEnd]
		scheme, isPrivate := hasPrivateQuickScheme(rawURL)
		if !isPrivate {
			i = urlEnd + 1
			continue
		}

		label := strings.TrimSpace(s[labelStart:j])
		if (strings.HasPrefix(label, "[") && strings.HasSuffix(label, "]")) || reHexCitationID.MatchString(label) {
			wsStart := i
			for wsStart > last && (s[wsStart-1] == ' ' || s[wsStart-1] == '\t') {
				wsStart--
			}
			out.WriteString(s[last:wsStart])
		} else if strings.HasPrefix(label, "@") && scheme != "mention://" {
			out.WriteString(s[last:i])
			out.WriteString(strings.TrimPrefix(label, "@"))
		} else {
			out.WriteString(s[last:i])
			out.WriteString(label)
		}
		last = urlEnd + 1
		i = last
	}
	if last == 0 {
		return s
	}
	out.WriteString(s[last:])
	return out.String()
}

// quickOutputFilter 在流式与非流式输出中统一清洗 AWS Quick 私有引用角标、来源块、工件占位及 <decision> 选项卡片，
// 同时确保 Markdown 围栏代码块与行内反引号内部内容 100% 原样保留。
type quickOutputFilter struct {
	allowed     map[string]bool
	canEmitCall func(ToolCallReply) bool

	buf                string
	inFence            bool
	fenceChar          byte
	fenceLen           int
	blockMode          string // "" | "decision" | "decisions" | "sources" | "artifact" | "delegate"
	atLineStart        bool
	dropLeadingNewline bool
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
	f.fenceChar = 0
	f.fenceLen = 0
	f.blockMode = ""
	f.atLineStart = true
	f.dropLeadingNewline = false
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
		if f.dropLeadingNewline {
			if f.buf[0] == '\r' {
				if len(f.buf) == 1 {
					break
				}
				if f.buf[1] == '\n' {
					f.buf = f.buf[2:]
				} else {
					f.buf = f.buf[1:]
				}
				f.dropLeadingNewline = false
				continue
			}
			if f.buf[0] == '\n' {
				f.buf = f.buf[1:]
			}
			f.dropLeadingNewline = false
		}
		if f.buf == "" {
			break
		}

		if f.blockMode != "" {
			consumed, keepWaiting, emittedMD, emittedCall := f.consumeActiveBlock()
			if emittedCall != nil {
				outCalls = append(outCalls, *emittedCall)
			}
			if emittedMD != "" {
				emit(emittedMD)
			}
			if consumed {
				continue
			}
			if keepWaiting {
				break
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
					f.fenceChar = 0
					f.fenceLen = 0
					continue
				}
			}
			nl := strings.IndexByte(f.buf, '\n')
			if nl >= 0 {
				emit(f.buf[:nl+1])
				f.buf = f.buf[nl+1:]
				continue
			}
			if f.atLineStart && len(f.buf) <= 8 {
				break
			}
			emit(f.buf)
			f.buf = ""
			break
		}

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

		// 若缓冲区包含换行，先按第一行处理（跳过行内反引号内的假起始标签）
		if nl := strings.IndexByte(f.buf, '\n'); nl >= 0 {
			line := f.buf[:nl+1]
			if openIdx, mode := findNextQuickBlockOpen(line); openIdx >= 0 {
				if openIdx > 0 {
					emit(stripQuickInlineOutsideCode(f.buf[:openIdx]))
					f.buf = f.buf[openIdx:]
				}
				f.blockMode = mode
				continue
			}
			emit(stripQuickInlineOutsideCode(line))
			f.buf = f.buf[nl+1:]
			continue
		}

		// 单行无换行增量：首先检查完整行内块标记起点（跳开行内反引号）
		if openIdx, mode := findNextQuickBlockOpen(f.buf); openIdx >= 0 {
			if openIdx > 0 {
				emit(stripQuickInlineOutsideCode(f.buf[:openIdx]))
				f.buf = f.buf[openIdx:]
			}
			f.blockMode = mode
			continue
		}

		// 切出确定安全的非代码前缀与末尾待定尾巴（避免切断行内反引号、多引用组、私有链接与块前缀）
		hold := trailingQuickHoldLen(f.buf, f.atLineStart)
		safeLen := len(f.buf) - hold
		if safeLen > 0 {
			safe := f.buf[:safeLen]
			emit(stripQuickInlineOutsideCode(safe))
			f.buf = f.buf[safeLen:]
		}
		if hold > 0 {
			break
		}
	}

	return out.String(), outCalls
}

func (f *quickOutputFilter) consumeActiveBlock() (consumed, keepWaiting bool, md string, call *ToolCallReply) {
	switch f.blockMode {
	case "decision":
		loc := reDecisionClose.FindStringIndex(f.buf)
		if len(loc) < 2 {
			if len(f.buf) > 32*1024 {
				raw := f.buf
				f.buf = ""
				f.blockMode = ""
				return true, false, raw, nil
			}
			return false, true, "", nil
		}
		// 若遇到 </invoke> 但紧跟着还可能有可选空白 + </decision>，多等几个字节
		tailAfterClose := f.buf[loc[1]:]
		if strings.EqualFold(f.buf[loc[0]:loc[1]], "</invoke>") && len(strings.TrimLeft(tailAfterClose, " \t\r\n")) < len("</decision>") {
			trimmedTail := strings.ToLower(strings.TrimLeft(tailAfterClose, " \t\r\n"))
			if strings.HasPrefix("</decision>", trimmedTail) {
				return false, true, "", nil
			}
		}
		rawBlock := f.buf[:loc[1]]
		f.buf = f.buf[loc[1]:]
		f.blockMode = ""
		if block, ok := parseDecisionBlock(rawBlock); ok {
			mdOut, tc := convertDecision(block, f.allowed, f.canEmitCall)
			return true, false, mdOut, tc
		}
		return true, false, stripQuickInlineOutsideCode(rawBlock), nil

	case "decisions":
		loc := reDecisionsClose.FindStringIndex(f.buf)
		if len(loc) < 2 {
			if len(f.buf) > 48*1024 {
				raw := f.buf
				f.buf = ""
				f.blockMode = ""
				return true, false, raw, nil
			}
			return false, true, "", nil
		}
		rawBlock := f.buf[:loc[1]]
		f.buf = f.buf[loc[1]:]
		f.blockMode = ""
		f.dropLeadingNewline = true
		if blocks, ok := parseDecisionsBlock(rawBlock); ok {
			mdOut, tc := convertDecisions(blocks, f.allowed, f.canEmitCall)
			return true, false, mdOut, tc
		}
		return true, false, stripQuickInlineOutsideCode(rawBlock), nil

	case "sources":
		loc := reSourcesClose.FindStringIndex(f.buf)
		if len(loc) < 2 {
			if len(f.buf) > 32*1024 {
				raw := f.buf
				f.buf = ""
				f.blockMode = ""
				return true, false, raw, nil
			}
			return false, true, "", nil
		}
		f.buf = f.buf[loc[1]:]
		f.blockMode = ""
		f.dropLeadingNewline = true
		return true, false, "", nil

	case "artifact":
		endIdx := findArtifactEnd(f.buf)
		if endIdx < 0 {
			if len(f.buf) > 32*1024 {
				raw := f.buf
				f.buf = ""
				f.blockMode = ""
				return true, false, raw, nil
			}
			return false, true, "", nil
		}
		f.buf = f.buf[endIdx:]
		f.blockMode = ""
		f.dropLeadingNewline = true
		return true, false, "", nil

	case "delegate":
		endIdx := findDelegateEnd(f.buf)
		if endIdx < 0 {
			if len(f.buf) > 16*1024 {
				raw := f.buf
				f.buf = ""
				f.blockMode = ""
				return true, false, raw, nil
			}
			return false, true, "", nil
		}
		f.buf = f.buf[endIdx:]
		f.blockMode = ""
		f.dropLeadingNewline = true
		return true, false, "", nil
	}

	f.blockMode = ""
	return true, false, "", nil
}

// findArtifactEnd 匹配 <artifact .../> 自闭合或 <artifact ...>...</artifact> 闭合结束偏移。
func findArtifactEnd(s string) int {
	inQuote := byte(0)
	gtPos := -1
	for i := len("<artifact"); i < len(s); i++ {
		ch := s[i]
		if inQuote != 0 {
			if ch == inQuote {
				inQuote = 0
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			inQuote = ch
			continue
		}
		if ch == '>' {
			gtPos = i
			break
		}
	}
	if gtPos < 0 {
		return -1
	}
	j := gtPos - 1
	for j >= 0 && (s[j] == ' ' || s[j] == '\t') {
		j--
	}
	if j >= 0 && s[j] == '/' {
		return gtPos + 1
	}
	loc := reArtifactClose.FindStringIndex(s[gtPos+1:])
	if len(loc) < 2 {
		return -1
	}
	return gtPos + 1 + loc[1]
}

// findDelegateEnd 匹配 [CHAT_AGENT_DELEGATE:\n...\n] 的闭合右括号位置。
func findDelegateEnd(s string) int {
	for i := len("[CHAT_AGENT_DELEGATE:"); i < len(s); i++ {
		if s[i] == ']' {
			return i + 1
		}
	}
	return -1
}

// Flush 在响应流结束后刷出所有缓冲残留；未闭合的 <sources> 等尾部私有块静默剥离，其余安全透传。
func (f *quickOutputFilter) Flush() (outText string, outCalls []ToolCallReply) {
	if f.buf == "" {
		f.blockMode = ""
		f.dropLeadingNewline = false
		return "", nil
	}
	rem := f.buf
	f.buf = ""
	if f.dropLeadingNewline {
		f.dropLeadingNewline = false
		if strings.HasPrefix(rem, "\r\n") {
			rem = rem[2:]
		} else if strings.HasPrefix(rem, "\n") || strings.HasPrefix(rem, "\r") {
			rem = rem[1:]
		}
	}
	if rem == "" {
		f.blockMode = ""
		return "", nil
	}
	if f.inFence {
		f.inFence = false
		f.fenceChar = 0
		f.fenceLen = 0
		return rem, nil
	}
	mode := f.blockMode
	f.blockMode = ""
	switch mode {
	case "decision":
		if block, ok := parseDecisionBlock(rem); ok {
			md, tc := convertDecision(block, f.allowed, f.canEmitCall)
			if tc != nil {
				return "", []ToolCallReply{*tc}
			}
			return md, nil
		}
	case "decisions":
		if blocks, ok := parseDecisionsBlock(rem); ok {
			md, tc := convertDecisions(blocks, f.allowed, f.canEmitCall)
			if tc != nil {
				return "", []ToolCallReply{*tc}
			}
			return md, nil
		}
	case "sources":
		// 模型在流末尾吐出未闭合 <sources> 块时，在 32 KB 防御限制内静默移除
		if len(rem) <= 32*1024 {
			return "", nil
		}
		return rem, nil
	case "artifact", "delegate":
		if len(rem) <= 4*1024 {
			return "", nil
		}
		return rem, nil
	}
	return stripQuickInlineOutsideCode(rem), nil
}

// checkFenceToggle 检测在行首状态下是否遇到了开启或闭合的 Markdown 围栏 ```` ``` ```` 或 `~~~`。
func (f *quickOutputFilter) checkFenceToggle(closing bool) (waitMore, toggled bool, consumed string) {
	i := 0
	for i < len(f.buf) && i < 3 && (f.buf[i] == ' ' || f.buf[i] == '\t') {
		i++
	}
	if i == len(f.buf) {
		return !closing && len(f.buf) <= 3, false, ""
	}
	ch := f.buf[i]
	if ch != '`' && ch != '~' {
		return false, false, ""
	}
	if closing && ch != f.fenceChar {
		return false, false, ""
	}
	j := i
	for j < len(f.buf) && f.buf[j] == ch {
		j++
	}
	runLen := j - i
	if runLen < 3 {
		if j == len(f.buf) {
			return true, false, ""
		}
		return false, false, ""
	}
	if closing && runLen < f.fenceLen {
		return false, false, ""
	}
	if !closing {
		f.fenceChar = ch
		f.fenceLen = runLen
		return false, true, f.buf[:j]
	}
	nl := strings.IndexByte(f.buf[j:], '\n')
	if nl < 0 {
		for k := j; k < len(f.buf); k++ {
			if f.buf[k] != ' ' && f.buf[k] != '\t' && f.buf[k] != '\r' {
				return false, false, ""
			}
		}
		return true, false, ""
	}
	if strings.TrimSpace(f.buf[j:j+nl]) != "" {
		return false, false, ""
	}
	return false, true, f.buf[:j+nl+1]
}

func hasASCIIPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		a := s[i]
		if a >= 'A' && a <= 'Z' {
			a += 'a' - 'A'
		}
		b := prefix[i]
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}

// findNextQuickBlockOpen 在 s 中查找最先出现的块级私有标记起点（自动跳过行内反引号 ``...`` 内部，且纯按原字节下标比对，免疫 UTF-8 分片）。
func findNextQuickBlockOpen(s string) (idx int, mode string) {
	n := len(s)
	i := 0
	for i < n {
		if s[i] == '`' {
			tickLen := 1
			for i+tickLen < n && s[i+tickLen] == '`' {
				tickLen++
			}
			if closeIdx := findMatchingBackticks(s, i+tickLen, tickLen); closeIdx >= 0 {
				i = closeIdx + tickLen
				continue
			}
			// 遇到未闭合的行内反引号，剩余内容暂视为行内代码，不触发块级捕获
			return -1, ""
		}
		if s[i] == '<' {
			rest := s[i:]
			if hasASCIIPrefixFold(rest, "<decisions") && len(rest) > len("<decisions") {
				next := rest[len("<decisions")]
				if next == '>' || isHorizSpace(next) || next == '\r' || next == '\n' {
					return i, "decisions"
				}
			}
			if hasASCIIPrefixFold(rest, "<decision") && len(rest) > len("<decision") {
				next := rest[len("<decision")]
				if next == '>' || isHorizSpace(next) || next == '\r' || next == '\n' {
					return i, "decision"
				}
			}
			if hasASCIIPrefixFold(rest, "<sources") && len(rest) > len("<sources") {
				next := rest[len("<sources")]
				if next == '>' || isHorizSpace(next) || next == '\r' || next == '\n' {
					return i, "sources"
				}
			}
			if hasASCIIPrefixFold(rest, "<artifact") && len(rest) > len("<artifact") {
				next := rest[len("<artifact")]
				if isHorizSpace(next) || next == '\r' || next == '\n' {
					return i, "artifact"
				}
			}
		} else if s[i] == '[' && strings.HasPrefix(s[i:], "[CHAT_AGENT_DELEGATE:") {
			return i, "delegate"
		}
		i++
	}
	return -1, ""
}

func isHorizSpace(b byte) bool {
	return b == ' ' || b == '\t'
}

// unclosedInlineCodeStart 返回当前单行文本末尾未闭合行内反引号段的起始索引；无未闭合段返回 -1。
func unclosedInlineCodeStart(s string) int {
	n := len(s)
	i := 0
	for i < n {
		if s[i] != '`' {
			i++
			continue
		}
		tickLen := 1
		for i+tickLen < n && s[i+tickLen] == '`' {
			tickLen++
		}
		closeIdx := findMatchingBackticks(s, i+tickLen, tickLen)
		if closeIdx < 0 {
			if n-i <= 512 {
				return i
			}
			i += tickLen
			continue
		}
		i = closeIdx + tickLen
	}
	return -1
}

var quickOpenPrefixes = []string{
	"<decisions",
	"<decision",
	"<sources",
	"<artifact",
	"</sources>",
	"</decisions>",
	"</decision>",
	"</option>",
	"</artifact>",
	"<decision_response>",
	"</decision_response>",
}

var quickBracketPrefixes = []string{
	"[ACP_ROUTE:",
	"[NOTCH_REPLY_STYLE:",
	"[CHAT_AGENT_DELEGATE:",
}

// trailingQuickHoldLen 返回字符串末尾可能尚未接收完毕、需要等待下一流片再判决的最小尾部字节数。
func trailingQuickHoldLen(s string, atLineStart bool) int {
	maxHold := 0
	updateHold := func(h int) {
		if h > maxHold {
			maxHold = h
		}
	}

	// 0. 保护未闭合行内反引号 ``...``，避免内部出现私有标签字符串时提前切碎误清洗
	if codeStart := unclosedInlineCodeStart(s); codeStart >= 0 {
		updateHold(len(s) - codeStart)
	}

	// 1. 保留末尾可能构成任何 XML 私有块标签（含起始与孤立闭合标签）的未完成前缀
	// 直接基于原字符串 s 计算 lastLT 并按 ASCII 忽略大小写比较，避免 UTF-8 截断字节被 strings.ToLower 扩成 U+FFFD 引发下标偏移错位。
	if lastLT := strings.LastIndexByte(s, '<'); lastLT >= 0 {
		tail := s[lastLT:]
		for _, prefix := range quickOpenPrefixes {
			if len(tail) <= len(prefix) && hasASCIIPrefixFold(prefix, tail) {
				updateHold(len(s) - lastLT)
				break
			}
		}
	}

	// 2. 保留末尾可能构成私有方括号控制标记、多引用角标 [@xxx, @yyy] 或私有协议 Markdown 链接的未完成前缀
	if tailH := pendingBracketSuffixLen(s); tailH > 0 {
		updateHold(tailH)
	}

	// 3. 若刚好在行首，等待末尾的少量空白和 1-2 个围栏反引号/波浪号
	if lastNL := strings.LastIndexByte(s, '\n'); lastNL >= 0 || atLineStart {
		lineTail := s
		if lastNL >= 0 {
			lineTail = s[lastNL+1:]
		}
		if len(lineTail) <= 6 {
			i := 0
			for i < len(lineTail) && i < 3 && isHorizSpace(lineTail[i]) {
				i++
			}
			if i < len(lineTail) && (lineTail[i] == '`' || lineTail[i] == '~') {
				fc := lineTail[i]
				onlyFence := true
				for j := i; j < len(lineTail); j++ {
					if lineTail[j] != fc {
						onlyFence = false
						break
					}
				}
				if onlyFence {
					updateHold(len(lineTail))
				}
			}
		}
	}

	return maxHold
}

func pendingBracketSuffixLen(s string) int {
	// 先检查短尾空白是否可能紧随私有角标/路由标签，避免前导空白提前吐出
	tailStart := max(0, len(s)-256)
	sub := s[tailStart:]

	best := 0
	consider := func(idxInSub int) {
		wsStart := tailStart + idxInSub
		for wsStart > 0 && isHorizSpace(s[wsStart-1]) && (tailStart+idxInSub-wsStart) < 4 {
			wsStart--
		}
		if h := len(s) - wsStart; h > best {
			best = h
		}
	}

	// 检查各候选 '[' 位置
	for p := 0; p < len(sub); p++ {
		if sub[p] != '[' {
			continue
		}
		cand := sub[p:]
		if isPendingBracketCandidate(cand) {
			consider(p)
			break
		}
	}

	if best == 0 {
		wsCount := 0
		for i := len(s) - 1; i >= 0 && isHorizSpace(s[i]) && wsCount < 4; i-- {
			wsCount++
		}
		if wsCount > 0 {
			best = wsCount
		}
	}
	return best
}

func isPendingBracketCandidate(cand string) bool {
	if cand == "[" {
		return true
	}
	// A. 平台内部路由控制前缀：[ACP_ROUTE:...] / [NOTCH_REPLY_STYLE:...] / [CHAT_AGENT_DELEGATE:...]
	for _, pref := range quickBracketPrefixes {
		if len(cand) <= len(pref) && strings.HasPrefix(pref, cand) {
			return true
		}
		if strings.HasPrefix(cand, pref) && !strings.ContainsRune(cand[len(pref):], ']') {
			return true
		}
	}
	// B. 单/多引用角标 [@0cc622, @8dbdfb]（若已到 ']' 则多等 1 字符确认不紧随 '('；不跨 96 字节）
	if strings.HasPrefix(cand, "[@") && len(cand) <= 96 && isPendingMultiCitation(cand) {
		return true
	}
	// C. 可能指向私有协议的 Markdown 链接 [...](kg://... / qw-file://... / qw-tool://... / mention://...)
	if strings.ContainsRune(cand, '\n') || len(cand) > 240 {
		return false
	}
	depth := 1
	rb := -1
	for i := 1; i < len(cand); i++ {
		if cand[i] == '\r' || cand[i] == '\n' {
			return false
		}
		if cand[i] == '[' {
			depth++
			if depth > 2 {
				return false
			}
		} else if cand[i] == ']' {
			depth--
			if depth == 0 {
				rb = i
				break
			}
		}
	}
	if rb < 0 {
		return len(cand) <= 80 && !strings.ContainsRune(cand, '\t')
	}
	if rb == len(cand)-1 {
		return true
	}
	if cand[rb+1] != '(' {
		return false
	}
	urlPart := cand[rb+2:]
	if strings.ContainsRune(urlPart, ')') {
		return false
	}
	return couldBePrivateQuickSchemePrefix(urlPart)
}

func isPendingMultiCitation(cand string) bool {
	// 允许 "[@" + 若干 hex/空白/逗号/分号/@ 字符，最多末尾带 1 个闭合 ']' 等待看下一个字符是否 '('
	body := cand[2:]
	if strings.HasSuffix(body, "]") {
		body = body[:len(body)-1]
	}
	if strings.ContainsRune(body, ']') {
		return false
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex && c != '@' && c != ',' && c != ';' && c != ' ' && c != '\t' {
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
