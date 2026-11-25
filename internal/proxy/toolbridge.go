package proxy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// hasVisibleText reports whether s contains a non-space, non-format rune.
// It treats zero-width and other format characters as empty so an upstream
// response that only emits them is not reported as a successful stop.
func hasVisibleText(s string) bool {
	for _, r := range s {
		if r == 0 || unicode.IsSpace(r) {
			continue
		}
		if unicode.Is(unicode.Cf, r) {
			continue
		}
		return true
	}
	return false
}

// ToolCallReply is a client-side tool call parsed from the model's text.
type ToolCallReply struct {
	ID        string
	Name      string
	Arguments string
}

type toolDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type toolFunctionEnvelope struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolCallEnvelope struct {
	Name       string                `json:"name"`
	Tool       string                `json:"tool"`
	Arguments  json.RawMessage       `json:"arguments"`
	Parameters json.RawMessage       `json:"parameters"`
	Input      json.RawMessage       `json:"input"`
	Args       json.RawMessage       `json:"args"`
	Function   *toolFunctionEnvelope `json:"function"`
}

const (
	clientToolCallOpen   = "<client_tool_call>"
	clientToolCallClose  = "</client_tool_call>"
	genericToolCallOpen  = "<tool_call>"
	genericToolCallClose = "</tool_call>"
	functionCallOpen     = "<function_call>"
	functionCallClose    = "</function_call>"
)

// collectToolDefinitions normalizes the OpenAI tools/functions arrays into one
// list. Later duplicate names are ignored so a tool is defined only once.
func collectToolDefinitions(tools []Tool, functions []ToolFunction) []toolDefinition {
	seen := make(map[string]bool)
	defs := make([]toolDefinition, 0, len(tools)+len(functions))
	add := func(name, description string, parameters any) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		seen[name] = true
		defs = append(defs, toolDefinition{Name: name, Description: description, Parameters: parameters})
	}
	for _, t := range tools {
		add(t.Function.Name, t.Function.Description, t.Function.Parameters)
	}
	for _, f := range functions {
		add(f.Name, f.Description, f.Parameters)
	}
	return defs
}

// forwardToolDefinitions renders the caller's tool schema unchanged so the
// upstream model can use it. This belongs to the caller's instructions, not
// to the relay backend.
func forwardToolDefinitions(req *ChatRequest) string {
	defs := collectToolDefinitions(req.Tools, req.Functions)
	if len(defs) == 0 {
		return ""
	}
	payload := map[string]any{"tools": defs}
	if req.ToolChoice != nil {
		payload["tool_choice"] = req.ToolChoice
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return "Workspace actions available via the user's local client:\n" + string(body)
}

// buildToolBridgePrompt 定义客户端提供的工具定义及纯技术中立的客户端回调协议说明。
func buildToolBridgePrompt(req *ChatRequest, mode, name string, parallel bool) string {
	if mode == "none" {
		return ""
	}
	defs := forwardToolDefinitions(req)
	if defs == "" {
		return ""
	}

	var b strings.Builder
	b.WriteString("<client_tool_protocol>\n")
	b.WriteString(defs)
	b.WriteString("\n\nFormat:\n")
	b.WriteString("- Optionally write one brief sentence in the user's language describing the action, then emit one marker per action with no Markdown code fences and no trailing text:\n")
	b.WriteString(`<client_tool_call>{"name":"TOOL_NAME","arguments":{...}}</client_tool_call>`)
	b.WriteString("\n- Use only listed tool names with a JSON object for \"arguments\" matching the parameter schema.\n")
	b.WriteString("- By default, perform file creation, edits, exports (such as HTML/SVG/code files), and shell checks directly in the user's local workspace with these client tools rather than saving deliverables only in a remote workspace.\n")
	b.WriteString("- Keep each turn focused and incremental: each streaming turn has a strict upstream runtime limit (~8-9 minutes), so for large or complex deliverables (such as detailed 3D/Three.js/SVG/HTML scenes or files over ~250 lines), do not try to design and emit the entire codebase in a single oversized tool call.\n")
	b.WriteString("- Instead, keep the first turn's planning brief, immediately write a clean runnable scaffold or split into smaller files (~150-300 lines per tool call) in the local workspace, then continue across subsequent tool turns to add shaders, visual polish, interactions, and verification until the full deliverable is complete (do not stop at a skeleton unless blocked).\n")
	b.WriteString("- After the client returns the action result in the next turn, continue the task with that result.\n")
	b.WriteString("- If no workspace action is needed, answer normally in plain text without a marker.\n")
	if parallel {
		b.WriteString("- For several independent actions, emit several markers one after another.\n")
	} else {
		b.WriteString("- Request at most one action per turn.\n")
	}
	if mode == "function" && name != "" {
		fmt.Fprintf(&b, "- You must call %q.\n", name)
	}
	if mode == "required" {
		b.WriteString("- You must request at least one action before answering.\n")
	}
	b.WriteString("</client_tool_protocol>")
	return b.String()
}

func (e toolCallEnvelope) resolved() (string, json.RawMessage) {
	name := strings.TrimSpace(e.Name)
	if name == "" {
		name = strings.TrimSpace(e.Tool)
	}
	args := e.Arguments
	for _, candidate := range []json.RawMessage{e.Parameters, e.Input, e.Args} {
		if len(args) == 0 && len(candidate) > 0 {
			args = candidate
		}
	}
	if e.Function != nil {
		if n := strings.TrimSpace(e.Function.Name); n != "" {
			name = n
		}
		if len(e.Function.Arguments) > 0 {
			args = e.Function.Arguments
		}
	}
	return name, args
}

// resolveToolName tolerates the name shapes models produce: exact names,
// different case, namespace prefixes such as "functions.read_file", and a
// unique suffix after the last dot or slash. It returns "" when the name is
// not one of the caller's tools.
func resolveToolName(name string, allowed map[string]bool) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if allowed[name] {
		return name
	}
	lower := strings.ToLower(name)
	for candidate := range allowed {
		if strings.ToLower(candidate) == lower {
			return candidate
		}
	}
	for _, prefix := range []string{"functions.", "function.", "tools.", "tool."} {
		if strings.HasPrefix(lower, prefix) {
			if resolved := resolveToolName(strings.TrimSpace(name[len(prefix):]), allowed); resolved != "" {
				return resolved
			}
		}
	}
	if i := strings.LastIndexAny(name, "./:"); i >= 0 && i+1 < len(name) {
		if resolved := resolveToolName(strings.TrimSpace(name[i+1:]), allowed); resolved != "" {
			return resolved
		}
	}
	// A unique case-insensitive suffix match catches MCP-style names where the
	// caller declared the bare tool while the model used a server prefix.
	var match string
	for candidate := range allowed {
		suffix := strings.ToLower(strings.TrimSpace(candidate))
		if suffix == "" {
			continue
		}
		if strings.HasSuffix(lower, "."+suffix) || strings.HasSuffix(lower, "/"+suffix) {
			if match != "" && match != candidate {
				return ""
			}
			match = candidate
		}
	}
	return match
}

// parseToolEnvelopes accepts a single object, an array, an OpenAI tool_calls
// wrapper, or a legacy function_call wrapper.
func parseToolEnvelopes(payload string) []toolCallEnvelope {
	if out := parseToolEnvelopesStrict(payload); len(out) > 0 {
		return out
	}
	repaired := repairToolCallJSON(payload)
	if repaired != payload {
		if out := parseToolEnvelopesStrict(repaired); len(out) > 0 {
			return out
		}
	}
	return salvageToolEnvelope(payload)
}

func parseToolEnvelopesStrict(payload string) []toolCallEnvelope {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return nil
	}
	var arr []toolCallEnvelope
	if strings.HasPrefix(payload, "[") && json.Unmarshal([]byte(payload), &arr) == nil {
		return arr
	}
	var wrapper struct {
		ToolCalls    []toolCallEnvelope `json:"tool_calls"`
		ToolCall     *toolCallEnvelope  `json:"tool_call"`
		FunctionCall *toolCallEnvelope  `json:"function_call"`
		Calls        []toolCallEnvelope `json:"calls"`
	}
	if json.Unmarshal([]byte(payload), &wrapper) == nil {
		if len(wrapper.ToolCalls) > 0 {
			return wrapper.ToolCalls
		}
		if len(wrapper.Calls) > 0 {
			return wrapper.Calls
		}
		if wrapper.ToolCall != nil {
			return []toolCallEnvelope{*wrapper.ToolCall}
		}
		if wrapper.FunctionCall != nil {
			return []toolCallEnvelope{*wrapper.FunctionCall}
		}
	}
	var one toolCallEnvelope
	if json.Unmarshal([]byte(payload), &one) == nil {
		name, args := one.resolved()
		if name != "" || len(args) > 0 {
			return []toolCallEnvelope{one}
		}
	}
	return nil
}

type jsonRepairFrame struct {
	isObj       bool
	expectKey   bool
	expectValue bool
}

func skipJSONWhitespace(s string, pos int) int {
	for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t' || s[pos] == '\r' || s[pos] == '\n') {
		pos++
	}
	return pos
}

func isJSONKeyChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.'
}

func startsNextObjectKeyAt(raw string, quotePos int) bool {
	if quotePos >= len(raw) || raw[quotePos] != '"' {
		return false
	}
	p := quotePos + 1
	keyStart := p
	for p < len(raw) && p-keyStart <= 64 {
		c := raw[p]
		if c == '"' {
			if p == keyStart {
				return false
			}
			afterKey := skipJSONWhitespace(raw, p+1)
			return afterKey < len(raw) && raw[afterKey] == ':'
		}
		if !isJSONKeyChar(c) {
			return false
		}
		p++
	}
	return false
}

// looksLikeRawWindowsPath 判断自 start 起（紧邻开引号之后）是否为包含单反斜杠的 Windows 盘符路径。
func looksLikeRawWindowsPath(raw string, start int) bool {
	if start+3 >= len(raw) {
		return false
	}
	drive := raw[start]
	if !((drive >= 'A' && drive <= 'Z') || (drive >= 'a' && drive <= 'z')) {
		return false
	}
	if raw[start+1] != ':' || raw[start+2] != '\\' || raw[start+3] == '\\' {
		return false
	}
	limit := min(len(raw), start+512)
	for p := start + 3; p < limit; p++ {
		c := raw[p]
		if c == '\n' || c == '\r' {
			return false
		}
		if c == '"' && raw[p-1] != '\\' {
			after := skipJSONWhitespace(raw, p+1)
			if after >= len(raw) {
				return true
			}
			nc := raw[after]
			return nc == ',' || nc == '}' || nc == ']'
		}
	}
	return false
}

// isClosingValueQuote 通过 JSON 结构栈前瞻，判断字符串值内的未转义双引号是否为真实闭合引号。
func isClosingValueQuote(raw string, quotePos int, stack []jsonRepairFrame) bool {
	if len(stack) == 0 {
		return true
	}
	j := skipJSONWhitespace(raw, quotePos+1)
	if j >= len(raw) {
		return true
	}
	next := raw[j]
	if next != ',' && next != '}' && next != ']' {
		return false
	}

	top := stack[len(stack)-1]
	if top.isObj {
		switch next {
		case ']':
			return false
		case ',':
			k := skipJSONWhitespace(raw, j+1)
			if k >= len(raw) {
				return true
			}
			if raw[k] == '}' {
				return true
			}
			return startsNextObjectKeyAt(raw, k)
		case '}':
			// 验证 '}' 闭合之后是否紧跟合法外层 JSON 分隔符或到达尾部，防 Python 字典 {"id":"x"}.get() 误判
			k := skipJSONWhitespace(raw, j+1)
			if k >= len(raw) {
				return true
			}
			after := raw[k]
			outerDepth := len(stack) - 1
			if outerDepth == 0 {
				if after == ',' {
					return skipJSONWhitespace(raw, k+1) >= len(raw)
				}
				return false
			}
			outer := stack[outerDepth-1]
			if outer.isObj {
				if after == '}' {
					return true
				}
				if after == ',' {
					m := skipJSONWhitespace(raw, k+1)
					return m >= len(raw) || raw[m] == '}' || startsNextObjectKeyAt(raw, m)
				}
				return false
			}
			return after == ',' || after == ']'
		}
		return false
	}

	// 数组元素上下文
	switch next {
	case '}':
		return false
	case ',':
		k := skipJSONWhitespace(raw, j+1)
		if k >= len(raw) || raw[k] == ']' {
			return true
		}
		c := raw[k]
		return c == '"' || c == '{' || c == '[' || c == '-' || (c >= '0' && c <= '9') ||
			strings.HasPrefix(raw[k:], "true") || strings.HasPrefix(raw[k:], "false") || strings.HasPrefix(raw[k:], "null")
	case ']':
		k := skipJSONWhitespace(raw, j+1)
		if k >= len(raw) {
			return true
		}
		after := raw[k]
		return after == ',' || after == '}' || after == ']'
	}
	return false
}

// repairToolCallJSON 对模型输出的工具 JSON 执行上下文感知的容错修复：
// 1) 修复字符串字面量内部非法的单反斜杠转义（如 \$、\' 及裸 Windows 路径里的 \tests 等）；
// 2) 转义字符串内部偶发未转义的双引号（如 Python docstring """...""" 或未转义 "id"）与裸控制字符；
// 3) 剔除对象或数组结尾处的多余尾逗号。
func repairToolCallJSON(raw string) string {
	var b strings.Builder
	b.Grow(len(raw) + 32)
	inStr := false
	inRawWinPath := false
	inKeyStr := false
	changed := false
	var stack []jsonRepairFrame

	for i := 0; i < len(raw); {
		ch := raw[i]
		if !inStr {
			if ch == '"' {
				inStr = true
				inKeyStr = len(stack) > 0 && stack[len(stack)-1].isObj && stack[len(stack)-1].expectKey
				inRawWinPath = !inKeyStr && looksLikeRawWindowsPath(raw, i+1)
				b.WriteByte(ch)
				i++
				continue
			}
			switch ch {
			case '{':
				stack = append(stack, jsonRepairFrame{isObj: true, expectKey: true})
			case '[':
				stack = append(stack, jsonRepairFrame{isObj: false, expectValue: true})
			case '}':
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
			case ']':
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
			case ':':
				if len(stack) > 0 && stack[len(stack)-1].isObj {
					stack[len(stack)-1].expectKey = false
					stack[len(stack)-1].expectValue = true
				}
			case ',':
				j := skipJSONWhitespace(raw, i+1)
				if j < len(raw) && (raw[j] == '}' || raw[j] == ']') {
					changed = true
					i++
					continue
				}
				if len(stack) > 0 && stack[len(stack)-1].isObj {
					stack[len(stack)-1].expectKey = true
					stack[len(stack)-1].expectValue = false
				} else if len(stack) > 0 {
					stack[len(stack)-1].expectValue = true
				}
			}
			b.WriteByte(ch)
			i++
			continue
		}

		if ch == '"' {
			if !inKeyStr && !inRawWinPath && !isClosingValueQuote(raw, i, stack) {
				b.WriteString(`\"`)
				changed = true
				i++
				continue
			}
			inStr = false
			inRawWinPath = false
			if len(stack) > 0 && stack[len(stack)-1].isObj {
				if inKeyStr {
					stack[len(stack)-1].expectKey = false
				} else {
					stack[len(stack)-1].expectValue = false
				}
			}
			inKeyStr = false
			b.WriteByte(ch)
			i++
			continue
		}
		if ch == '\\' {
			if inRawWinPath {
				if i+1 < len(raw) && (raw[i+1] == '\\' || raw[i+1] == '"') {
					b.WriteByte('\\')
					b.WriteByte(raw[i+1])
					i += 2
					continue
				}
				b.WriteString(`\\`)
				changed = true
				i++
				continue
			}
			if i+1 < len(raw) {
				next := raw[i+1]
				switch next {
				case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
					b.WriteByte('\\')
					b.WriteByte(next)
					i += 2
					continue
				case 'u':
					if i+6 <= len(raw) && isHexQuad(raw[i+2:i+6]) {
						b.WriteString(raw[i : i+6])
						i += 6
						continue
					}
				}
			}
			// 非法转义或尾部孤立反斜杠：补写一层反斜杠并保留后续字节交由下一轮处理
			b.WriteString(`\\`)
			changed = true
			i++
			continue
		}
		if ch < 0x20 {
			changed = true
			switch ch {
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				fmt.Fprintf(&b, `\u%04x`, ch)
			}
			i++
			continue
		}
		b.WriteByte(ch)
		i++
	}

	if !changed {
		return raw
	}
	return b.String()
}

var (
	salvageNamePattern    = regexp.MustCompile(`"(?:name|tool)"\s*:\s*"([^"\\]+)"`)
	salvageArgsPattern    = regexp.MustCompile(`"(?:arguments|parameters|input|args)"\s*:\s*\{`)
	salvageParamKeyRegex  = regexp.MustCompile(`(?:^|,)\s*"([A-Za-z_][A-Za-z0-9_-]{0,63})"\s*:\s*`)
)

// salvageToolEnvelope 作为兜底解析器：当复杂代码或残缺引号导致状态机仍未通过 json.Unmarshal 时，
// 按顶层参数键边界切片恢复工具名与各字段字符串内容。
func salvageToolEnvelope(payload string) []toolCallEnvelope {
	trimmed := strings.TrimSpace(payload)
	if trimmed == "" {
		return nil
	}
	nameMatch := salvageNamePattern.FindStringSubmatch(trimmed)
	if len(nameMatch) < 2 {
		return nil
	}
	toolName := strings.TrimSpace(nameMatch[1])
	argsLoc := salvageArgsPattern.FindStringIndex(trimmed)
	if argsLoc == nil {
		return nil
	}
	argsBody := trimmed[argsLoc[1]:]
	if endBrace := strings.LastIndexByte(argsBody, '}'); endBrace >= 0 {
		argsBody = strings.TrimSpace(argsBody[:endBrace])
		if strings.HasSuffix(argsBody, "}") {
			// 若末尾同时包裹了外层对象的闭合 '}'，剥去末尾这一层 '}'
			argsBody = strings.TrimSpace(strings.TrimSuffix(argsBody, "}"))
		}
	}
	matches := salvageParamKeyRegex.FindAllStringSubmatchIndex(argsBody, -1)
	if len(matches) == 0 {
		return nil
	}

	argMap := make(map[string]any, len(matches))
	for idx, m := range matches {
		key := argsBody[m[2]:m[3]]
		valStart := m[1]
		valEnd := len(argsBody)
		if idx+1 < len(matches) {
			valEnd = matches[idx+1][0]
		}
		valRaw := strings.TrimSpace(argsBody[valStart:valEnd])
		if strings.HasSuffix(valRaw, ",") && idx+1 >= len(matches) {
			valRaw = strings.TrimSpace(strings.TrimSuffix(valRaw, ","))
		}
		if len(valRaw) >= 2 && valRaw[0] == '"' {
			if valRaw[len(valRaw)-1] == '"' {
				valRaw = valRaw[1 : len(valRaw)-1]
			} else {
				valRaw = valRaw[1:]
			}
			isPathKey := strings.Contains(strings.ToLower(key), "path") || strings.Contains(strings.ToLower(key), "dir") || looksLikeRawWindowsPath(valRaw, 0)
			argMap[key] = decodeSalvagedString(valRaw, isPathKey)
			continue
		}
		var parsedVal any
		if json.Unmarshal([]byte(valRaw), &parsedVal) == nil {
			argMap[key] = parsedVal
		} else {
			argMap[key] = valRaw
		}
	}
	if len(argMap) == 0 {
		return nil
	}
	encoded, err := json.Marshal(argMap)
	if err != nil {
		return nil
	}
	return []toolCallEnvelope{{
		Name:      toolName,
		Arguments: json.RawMessage(encoded),
	}}
}

func decodeSalvagedString(s string, rawWinPath bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+1 < len(s) {
			n := s[i+1]
			if rawWinPath {
				if n == '\\' || n == '"' {
					b.WriteByte(n)
					i += 2
					continue
				}
				b.WriteByte('\\')
				i++
				continue
			}
			switch n {
			case 'n':
				b.WriteByte('\n')
				i += 2
				continue
			case 'r':
				b.WriteByte('\r')
				i += 2
				continue
			case 't':
				b.WriteByte('\t')
				i += 2
				continue
			case '"', '\\', '/':
				b.WriteByte(n)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func isHexQuad(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// rawArgumentText returns the JSON text a tool call carries. OpenAI arguments
// are normally a JSON string, but models also emit a bare object or an object
// nested as a string; all three shapes are unwrapped here.
func rawArgumentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(string(raw))
}

func normalizeArguments(raw json.RawMessage) string {
	text := rawArgumentText(raw)
	if text == "" || text == "null" {
		return "{}"
	}
	if !json.Valid([]byte(text)) {
		repaired := repairToolCallJSON(text)
		if !json.Valid([]byte(repaired)) {
			return "{}"
		}
		text = repaired
	}
	var compacted any
	if json.Unmarshal([]byte(text), &compacted) != nil {
		return "{}"
	}
	body, err := json.Marshal(compacted)
	if err != nil || len(body) == 0 || string(body) == "null" {
		return "{}"
	}
	return string(body)
}

func repliesFromEnvelopes(envelopes []toolCallEnvelope, allowed map[string]bool) []ToolCallReply {
	out := make([]ToolCallReply, 0, len(envelopes))
	for _, env := range envelopes {
		name, args := env.resolved()
		name = resolveToolName(name, allowed)
		if name == "" {
			continue
		}
		out = append(out, ToolCallReply{
			ID:        "call_" + strings.ReplaceAll(uuid(), "-", ""),
			Name:      name,
			Arguments: normalizeArguments(args),
		})
	}
	return out
}

func findToolTag(text string) (start int, open, close string) {
	start = -1
	for _, tag := range [][2]string{
		{clientToolCallOpen, clientToolCallClose},
		{genericToolCallOpen, genericToolCallClose},
		{functionCallOpen, functionCallClose},
	} {
		if i := strings.Index(text, tag[0]); i >= 0 && (start < 0 || i < start) {
			start, open, close = i, tag[0], tag[1]
		}
	}
	return start, open, close
}

// extractTaggedToolCalls removes valid tool markers and returns the remaining
// text. Invalid or unknown markers are left untouched so ordinary text is
// never lost.
func extractTaggedToolCalls(text string, allowed map[string]bool) (string, []ToolCallReply) {
	var clean strings.Builder
	var replies []ToolCallReply
	rest := text
	changed := false
	for {
		start, open, close := findToolTag(rest)
		if start < 0 || open == "" {
			clean.WriteString(rest)
			break
		}
		bodyStart := start + len(open)
		relEnd := strings.Index(rest[bodyStart:], close)
		if relEnd < 0 {
			clean.WriteString(rest)
			break
		}
		bodyEnd := bodyStart + relEnd
		parsed := repliesFromEnvelopes(parseToolEnvelopes(rest[bodyStart:bodyEnd]), allowed)
		if len(parsed) == 0 {
			clearEnd := bodyEnd + len(close)
			clean.WriteString(rest[:clearEnd])
			rest = rest[clearEnd:]
			continue
		}
		clean.WriteString(rest[:start])
		replies = append(replies, parsed...)
		rest = rest[bodyEnd+len(close):]
		changed = true
	}
	if !changed {
		return text, nil
	}
	return strings.TrimSpace(clean.String()), replies
}

// dropEmptyFences removes a Markdown fence that only wrapped a tool marker.
// Fences around real content are preserved.
func dropEmptyFences(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") || !strings.HasSuffix(t, "```") || len(t) < 6 {
		return t
	}
	inner := t[3 : len(t)-3]
	if i := strings.IndexByte(inner, '\n'); i >= 0 {
		inner = inner[i+1:]
	}
	if hasVisibleText(inner) {
		return t
	}
	return ""
}

// extractFencedToolCalls accepts a tool call inside a Markdown fence anywhere
// in the message. Fences that do not contain one of the caller's tools are
// left untouched.
func extractFencedToolCalls(text string, allowed map[string]bool) (string, []ToolCallReply) {
	var clean strings.Builder
	var replies []ToolCallReply
	rest := text
	for {
		start := strings.Index(rest, "```")
		if start < 0 {
			clean.WriteString(rest)
			break
		}
		afterOpen := start + 3
		nl := strings.IndexByte(rest[afterOpen:], '\n')
		if nl < 0 {
			clean.WriteString(rest)
			break
		}
		bodyStart := afterOpen + nl + 1
		relEnd := strings.Index(rest[bodyStart:], "```")
		if relEnd < 0 {
			clean.WriteString(rest)
			break
		}
		bodyEnd := bodyStart + relEnd
		parsed := repliesFromEnvelopes(parseToolEnvelopes(strings.TrimSpace(rest[bodyStart:bodyEnd])), allowed)
		if len(parsed) == 0 {
			clean.WriteString(rest[:bodyEnd+3])
			rest = rest[bodyEnd+3:]
			continue
		}
		clean.WriteString(rest[:start])
		replies = append(replies, parsed...)
		rest = rest[bodyEnd+3:]
	}
	if len(replies) == 0 {
		return text, nil
	}
	return dropEmptyFences(strings.TrimSpace(clean.String())), replies
}

// extractBareToolCall accepts a whole-message JSON tool call, optionally
// wrapped in a Markdown fence.
func extractBareToolCall(text string, allowed map[string]bool) (string, []ToolCallReply) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "```") {
		if i := strings.Index(trimmed, "\n"); i >= 0 {
			trimmed = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(trimmed[i+1:]), "```"))
		}
	}
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return text, nil
	}
	replies := repliesFromEnvelopes(parseToolEnvelopes(trimmed), allowed)
	if len(replies) == 0 {
		return text, nil
	}
	return "", replies
}

func parseClientToolCalls(text string, allowed map[string]bool) (string, []ToolCallReply) {
	clean, replies := extractTaggedToolCalls(text, allowed)
	if len(replies) > 0 {
		return dropEmptyFences(clean), replies
	}
	clean, replies = extractFencedToolCalls(text, allowed)
	if len(replies) > 0 {
		return clean, replies
	}
	return extractBareToolCall(text, allowed)
}

// limitToolReplies enforces tool_choice/parallel_tool_calls limits.
func limitToolReplies(replies []ToolCallReply, mode, name string, parallel bool) []ToolCallReply {
	out := make([]ToolCallReply, 0, len(replies))
	for _, reply := range replies {
		if mode == "function" && name != "" && reply.Name != name {
			continue
		}
		out = append(out, reply)
		if !parallel {
			break
		}
	}
	return out
}

func replyToolCalls(replies []ToolCallReply) []ToolCall {
	out := make([]ToolCall, 0, len(replies))
	for _, r := range replies {
		out = append(out, ToolCall{
			ID:   r.ID,
			Type: "function",
			Function: FunctionCall{
				Name:      r.Name,
				Arguments: r.Arguments,
			},
		})
	}
	return out
}

// nativeToolReplies maps upstream-native tool events to client tool calls.
// Only names the caller declared are exposed.
func nativeToolReplies(calls []ToolCallState, allowed map[string]bool) []ToolCallReply {
	out := make([]ToolCallReply, 0, len(calls))
	for _, call := range calls {
		name := resolveToolName(call.Name, allowed)
		if name == "" {
			continue
		}
		id := strings.TrimSpace(call.ID)
		if id == "" {
			id = "call_" + strings.ReplaceAll(uuid(), "-", "")
		}
		args := strings.TrimSpace(call.Arguments)
		if args == "" || !json.Valid([]byte(args)) {
			args = "{}"
		}
		out = append(out, ToolCallReply{ID: id, Name: name, Arguments: args})
	}
	return out
}

func requestedToolNames(req *ChatRequest) map[string]bool {
	names := make(map[string]bool)
	for _, t := range req.Tools {
		if name := strings.TrimSpace(t.Function.Name); name != "" {
			names[name] = true
		}
	}
	for _, f := range req.Functions {
		if name := strings.TrimSpace(f.Name); name != "" {
			names[name] = true
		}
	}
	return names
}

func parseToolChoice(v any) (mode, name string) {
	switch x := v.(type) {
	case string:
		return strings.ToLower(strings.TrimSpace(x)), ""
	case map[string]any:
		if fn, ok := x["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok {
				return "function", strings.TrimSpace(n)
			}
		}
		if n, ok := x["name"].(string); ok {
			return "function", strings.TrimSpace(n)
		}
		if t, ok := x["type"].(string); ok {
			return strings.ToLower(strings.TrimSpace(t)), ""
		}
	}
	return "auto", ""
}

// toolBridgeStream 在流式输出期间实时分离普通文本与 <client_tool_call> 工具标记，
// 使普通文本与最终回复逐字即时上屏，且每个工具标记一闭合即立刻下发。
type toolBridgeStream struct {
	allowed      map[string]bool
	choiceMode   string
	choiceName   string
	parallel     bool
	buf          string
	inTag        bool
	tagOpen      string
	tagClose     string
	emittedText  bool
	emittedCalls int
	holdReleased bool
}

func newToolBridgeStream(allowed map[string]bool, choiceMode, choiceName string, parallel bool) *toolBridgeStream {
	return &toolBridgeStream{
		allowed:    allowed,
		choiceMode: choiceMode,
		choiceName: choiceName,
		parallel:   parallel,
	}
}

func (s *toolBridgeStream) Reset() {
	s.buf = ""
	s.inTag = false
	s.tagOpen = ""
	s.tagClose = ""
	s.emittedText = false
	s.emittedCalls = 0
	s.holdReleased = false
}

func (s *toolBridgeStream) acceptCall(r ToolCallReply) bool {
	if s.choiceMode == "function" && s.choiceName != "" && r.Name != s.choiceName {
		return false
	}
	if !s.parallel && s.emittedCalls >= 1 {
		return false
	}
	s.emittedCalls++
	return true
}

// Feed 输入一段上游文本增量，实时输出可上屏的纯文本与已闭合的工具调用列表。
func (s *toolBridgeStream) Feed(delta string) (outText string, outCalls []ToolCallReply) {
	if delta == "" {
		return "", nil
	}
	s.buf += delta
	var textOut strings.Builder

	for s.buf != "" {
		if s.inTag {
			end := strings.Index(s.buf, s.tagClose)
			if end < 0 {
				break
			}
			inner := s.buf[:end]
			s.buf = s.buf[end+len(s.tagClose):]
			s.inTag = false

			parsed := repliesFromEnvelopes(parseToolEnvelopes(inner), s.allowed)
			if len(parsed) == 0 {
				rawTag := s.tagOpen + inner + s.tagClose
				textOut.WriteString(rawTag)
				if hasVisibleText(rawTag) {
					s.emittedText = true
				}
				continue
			}
			for _, tc := range parsed {
				if s.acceptCall(tc) {
					outCalls = append(outCalls, tc)
				}
			}
			continue
		}

		// 若回复从一开头就可能是代码围栏或裸 JSON 工具调用，暂缓一小段以防伪装格式泄露，
		// 普通文本则零延迟直接放行进入实时流式输出。
		if !s.emittedText && s.emittedCalls == 0 && !s.holdReleased {
			trimmed := strings.TrimLeftFunc(s.buf, unicode.IsSpace)
			if trimmed == "" || trimmed == "`" || trimmed == "``" {
				break
			}
			if strings.HasPrefix(trimmed, "```") {
				nl := strings.IndexByte(trimmed, '\n')
				if nl < 0 {
					if len(trimmed) < 32 {
						break
					}
					s.holdReleased = true
				} else {
					bodyAfterFence := trimmed[nl+1:]
					if closeIdx := strings.Index(bodyAfterFence, "```"); closeIdx >= 0 {
						prefixWS := len(s.buf) - len(trimmed)
						chunkEnd := prefixWS + nl + 1 + closeIdx + 3
						chunk := s.buf[:chunkEnd]
						clean, parsed := parseClientToolCalls(chunk, s.allowed)
						if len(parsed) > 0 {
							s.buf = s.buf[chunkEnd:]
							if clean != "" {
								textOut.WriteString(clean)
								if hasVisibleText(clean) {
									s.emittedText = true
								}
							}
							for _, tc := range parsed {
								if s.acceptCall(tc) {
									outCalls = append(outCalls, tc)
								}
							}
							continue
						}
						s.holdReleased = true
					} else {
						afterBodyTrim := strings.TrimLeftFunc(bodyAfterFence, unicode.IsSpace)
						if afterBodyTrim != "" {
							first := afterBodyTrim[0]
							if first != '<' && first != '{' && first != '[' {
								s.holdReleased = true
							}
						}
						if !s.holdReleased && len(s.buf) > 32*1024 {
							s.holdReleased = true
						}
						if !s.holdReleased {
							break
						}
					}
				}
			} else if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
				if len(s.buf) > 64*1024 {
					s.holdReleased = true
				} else if len(trimmed) < 128 {
					break
				} else {
					head := trimmed
					if len(head) > 256 {
						head = head[:256]
					}
					if !strings.Contains(head, `"name"`) &&
						!strings.Contains(head, `"tool"`) &&
						!strings.Contains(head, `"function"`) &&
						!strings.Contains(head, `"calls"`) {
						s.holdReleased = true
					} else {
						break
					}
				}
			} else {
				s.holdReleased = true
			}
		}

		start, open, close := findToolTag(s.buf)
		if start >= 0 {
			if start > 0 {
				pre := s.buf[:start]
				textOut.WriteString(pre)
				if hasVisibleText(pre) {
					s.emittedText = true
				}
			}
			s.inTag = true
			s.tagOpen = open
			s.tagClose = close
			s.buf = s.buf[start+len(open):]
			continue
		}

		keep := trailingToolTagPrefixLen(s.buf)
		emitLen := len(s.buf) - keep
		if emitLen > 0 {
			safe := s.buf[:emitLen]
			textOut.WriteString(safe)
			if hasVisibleText(safe) {
				s.emittedText = true
			}
			s.buf = s.buf[emitLen:]
		}
		break
	}

	return textOut.String(), outCalls
}

// Flush 在上游流结束后刷出缓冲区中剩余文本或整段兜底解析（如整段围栏/裸 JSON 或流末未闭合标签的工具调用）。
func (s *toolBridgeStream) Flush() (outText string, outCalls []ToolCallReply) {
	if s.inTag {
		if parsed := repliesFromEnvelopes(parseToolEnvelopes(s.buf), s.allowed); len(parsed) > 0 {
			s.buf = ""
			s.inTag = false
			s.tagOpen = ""
			s.tagClose = ""
			for _, tc := range parsed {
				if s.acceptCall(tc) {
					outCalls = append(outCalls, tc)
				}
			}
			return "", outCalls
		}
		out := s.tagOpen + s.buf
		s.buf = ""
		s.inTag = false
		s.tagOpen = ""
		s.tagClose = ""
		if hasVisibleText(out) {
			s.emittedText = true
		}
		return out, nil
	}
	if s.buf == "" {
		return "", nil
	}
	rem := s.buf
	s.buf = ""
	if !s.emittedText && s.emittedCalls == 0 {
		clean, parsed := parseClientToolCalls(rem, s.allowed)
		if len(parsed) > 0 {
			for _, tc := range parsed {
				if s.acceptCall(tc) {
					outCalls = append(outCalls, tc)
				}
			}
			if hasVisibleText(clean) {
				s.emittedText = true
			}
			return clean, outCalls
		}
	}
	if hasVisibleText(rem) {
		s.emittedText = true
	}
	return rem, nil
}

func trailingToolTagPrefixLen(s string) int {
	maxKeep := 0
	for _, open := range []string{clientToolCallOpen, genericToolCallOpen, functionCallOpen} {
		limit := min(len(open)-1, len(s))
		for k := limit; k > maxKeep; k-- {
			if s[len(s)-k:] == open[:k] {
				maxKeep = k
				break
			}
		}
	}
	return maxKeep
}
