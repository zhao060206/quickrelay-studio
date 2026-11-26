package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	outpostMaxToolDescBytes    = 8192
	outpostHistoryBatchSize    = 100
	outpostConnectorName       = "mcp__quickrelay"
	outpostMaxHTTPResponseSize = 4 * 1024 * 1024
)

var (
	// ErrOutpostUnsupported 表示当前账号、代理桩或上游版本不支持虚拟 Outpost / events/batch 协议，
	// 在 auto 模式下可安全无损回退到经典文本工具桥。
	ErrOutpostUnsupported = errors.New("virtual outpost native tool protocol unsupported")

	// errOutpostHistoryNotImportable 表示历史消息结构无法无损映射为服务端结构化事件（如未闭合工具调用或包含二进制多模态分片）。
	errOutpostHistoryNotImportable = errors.New("history cannot be imported into outpost session")

	// outpostHTTPClient 供 QBS 事件接口复用的独立客户端，单元测试可替换以调整超时或传输层。
	outpostHTTPClient = &http.Client{Timeout: 25 * time.Second}
)

func sha256LowerHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// outpostDeviceName 基于线程 ID 派生隔离的虚拟设备显示名，避免并发会话互相覆盖工具注册。
func outpostDeviceName(threadID string) string {
	return "QuickRelay " + sha256LowerHex(threadID)[:16]
}

// outpostToolAlias 为客户端工具生成稳定、纯 ASCII 且长度受控的注册别名。
func outpostToolAlias(origName string) string {
	var slug strings.Builder
	for i := 0; i < len(origName) && slug.Len() < 24; i++ {
		c := origName[i]
		isASCIIAlphaNum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
		if isASCIIAlphaNum {
			slug.WriteByte(c)
		} else {
			slug.WriteByte('_')
		}
	}
	return "qr__" + sha256LowerHex(origName)[:16] + "_" + slug.String()
}

// outpostModelToolNameForAlias 基于设备名与工具实际注册别名生成上游模型可见的完整工具名，并遵守 Bedrock 64 字符上限。
func outpostModelToolNameForAlias(deviceName, alias string) string {
	prefix := strings.ReplaceAll(strings.TrimSpace(deviceName), " ", "_")
	if prefix == "" {
		return alias
	}
	raw := prefix + "_" + alias
	if len(raw) <= 64 {
		return raw
	}
	return "t_" + sha256LowerHex(raw)[:8] + "_" + raw[len(raw)-53:]
}

// outpostModelToolName 生成上游模型可见的完整工具名，并遵守 Bedrock 64 字符上限。
func outpostModelToolName(deviceName, origName string) string {
	return outpostModelToolNameForAlias(deviceName, outpostToolAlias(origName))
}

type outpostRegisteredTool struct {
	OriginalName string
	Alias        string
	Description  string
	InputSchema  any
}

// outpostToolRegistry 维护单轮请求的虚拟 Outpost 工具映射表、描述文档与名称反解逻辑。
type outpostToolRegistry struct {
	Tools       []outpostRegisteredTool
	AliasToOrig map[string]string
	OrigToAlias map[string]string
	PromptDocs  string
	ToolsHash   string
}

func newOutpostToolRegistry(req *ChatRequest) *outpostToolRegistry {
	if req == nil {
		return &outpostToolRegistry{
			AliasToOrig: make(map[string]string),
			OrigToAlias: make(map[string]string),
		}
	}
	defs := collectToolDefinitions(req.Tools, req.Functions)
	reg := &outpostToolRegistry{
		Tools:       make([]outpostRegisteredTool, 0, len(defs)),
		AliasToOrig: make(map[string]string, len(defs)),
		OrigToAlias: make(map[string]string, len(defs)),
	}
	var docs []string
	h := sha256.New()
	for _, def := range defs {
		alias := outpostToolAlias(def.Name)
		// 极罕见哈希+截断碰撞保护
		for existing, exists := reg.AliasToOrig[alias]; exists && existing != def.Name; {
			alias = alias + "_"
		}
		reg.AliasToOrig[alias] = def.Name
		reg.OrigToAlias[def.Name] = alias

		fullDesc := strings.TrimSpace("Client tool: " + def.Name + "\n" + def.Description)
		regDesc := fullDesc
		if len(regDesc) > outpostMaxToolDescBytes {
			docs = append(docs, fmt.Sprintf("## Client tool: %s (wire name %s)\n%s", def.Name, alias, def.Description))
			const suffix = "\n[Full usage documentation is included in custom_prompt.]"
			cut := outpostMaxToolDescBytes - len(suffix)
			if cut < 0 {
				cut = 0
			}
			for cut > 0 && !utf8.RuneStart(regDesc[cut]) {
				cut--
			}
			regDesc = regDesc[:cut] + suffix
		}
		schema := def.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		reg.Tools = append(reg.Tools, outpostRegisteredTool{
			OriginalName: def.Name,
			Alias:        alias,
			Description:  regDesc,
			InputSchema:  schema,
		})
		h.Write([]byte(def.Name))
		h.Write([]byte{0})
		h.Write([]byte(def.Description))
		h.Write([]byte{0})
		if b, err := json.Marshal(schema); err == nil {
			h.Write(b)
		}
		h.Write([]byte{0})
	}
	if len(docs) > 0 {
		reg.PromptDocs = "\n\n# Client tool documentation\n\n" + strings.Join(docs, "\n\n")
	}
	reg.ToolsHash = hex.EncodeToString(h.Sum(nil))
	return reg
}

func (r *outpostToolRegistry) empty() bool {
	return r == nil || len(r.Tools) == 0
}

func (r *outpostToolRegistry) capability() map[string]any {
	tools := make([]map[string]any, 0, len(r.Tools))
	for _, t := range r.Tools {
		tools = append(tools, map[string]any{
			"name":        t.Alias,
			"description": t.Description,
			"inputSchema": t.InputSchema,
			"isReadTool":  false,
		})
	}
	return map[string]any{
		"name":        outpostConnectorName,
		"description": "Tools provided by the connected QuickRelay client",
		"tools":       tools,
	}
}

func (r *outpostToolRegistry) resolveName(wireName, devName string) (string, bool) {
	if r == nil {
		return "", false
	}
	w := strings.TrimSpace(wireName)
	if w == "" {
		return "", false
	}
	if orig, ok := r.AliasToOrig[w]; ok {
		return orig, true
	}
	normPrefix := strings.ReplaceAll(strings.TrimSpace(devName), " ", "_")
	if normPrefix == "" {
		return "", false
	}
	// 仅接受本次注册可枚举的精确名称集合（短名 alias、本设备前缀完整名或 64 字符哈希截断完整名），拒绝其他设备或附加前缀。
	for alias, orig := range r.AliasToOrig {
		if w == normPrefix+"_"+alias || w == outpostModelToolNameForAlias(devName, alias) {
			return orig, true
		}
	}
	return "", false
}

// decodeCall 将云端下发的原生 tool_call 名称和参数解包为客户端原始工具名与标准紧凑 JSON 字符串参数。
func (r *outpostToolRegistry) decodeCall(wireName string, rawArgs json.RawMessage, devName string) (origName, argsJSON string, err error) {
	origName, ok := r.resolveName(wireName, devName)
	if !ok {
		return "", "", fmt.Errorf("unknown outpost tool %q", wireName)
	}
	alias := r.OrigToAlias[origName]
	argsJSON, err = unwrapOutpostToolArgs(alias, rawArgs)
	if err != nil {
		return "", "", err
	}
	return origName, argsJSON, nil
}

// unwrapOutpostToolArgs 兼容解包 AIDP `params.arguments` 包裹层或直接 JSON 参数对象，并剥离 `_quickwork` 内部字段。
func unwrapOutpostToolArgs(expectedAlias string, rawArgs json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(rawArgs))
	if trimmed == "" || trimmed == "null" {
		return "{}", nil
	}
	var top map[string]any
	if err := json.Unmarshal(rawArgs, &top); err != nil {
		// 少数情况上游可能把 JSON 序列化成字符串
		var strVal string
		if sErr := json.Unmarshal(rawArgs, &strVal); sErr == nil {
			if err2 := json.Unmarshal([]byte(strVal), &top); err2 != nil {
				return "", fmt.Errorf("invalid tool arguments JSON string: %w", err2)
			}
		} else {
			return "", fmt.Errorf("invalid tool arguments: %w", err)
		}
	}
	if top == nil {
		return "{}", nil
	}
	if pRaw, hasParams := top["params"]; hasParams && len(top) == 1 {
		paramsMap, ok := pRaw.(map[string]any)
		if !ok {
			return "", errors.New("tool params envelope is not an object")
		}
		if pname, hasName := paramsMap["name"].(string); hasName && expectedAlias != "" && pname != expectedAlias {
			return "", fmt.Errorf("tool params name mismatch: got %q want %q", pname, expectedAlias)
		}
		if argsAny, hasInner := paramsMap["arguments"]; hasInner {
			b, err := json.Marshal(argsAny)
			if err != nil {
				return "", err
			}
			return canonicalToolArgs(string(b)), nil
		}
		cleaned := make(map[string]any, len(paramsMap))
		for k, v := range paramsMap {
			if k == "_quickwork" || k == "name" {
				continue
			}
			cleaned[k] = v
		}
		b, err := json.Marshal(cleaned)
		if err != nil {
			return "", err
		}
		return canonicalToolArgs(string(b)), nil
	}
	delete(top, "_quickwork")
	b, err := json.Marshal(top)
	if err != nil {
		return "", err
	}
	return canonicalToolArgs(string(b)), nil
}

func (r *outpostToolRegistry) routingInstructions(outpostID, devName, choiceMode, choiceName string, parallelTools bool) string {
	if r.empty() {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n# Client tool routing\n")
	fmt.Fprintf(&b, "This session executes client-owned tools on virtual device %s (%s). Always invoke these registered tools for local workspace files, search, shell commands, or client actions; NEVER substitute Quick's remote sandbox/container tools (such as run_python_on_device, device_terminal) or cloud VFS. Client file paths belong to the local workspace and are passed as arguments to these tools.\nTool name mapping:\n",
		outpostID, devName)
	for _, t := range r.Tools {
		fmt.Fprintf(&b, "- %s: %s (wire name %s)\n", t.OriginalName, outpostModelToolNameForAlias(devName, t.Alias), t.Alias)
	}
	b.WriteString("\nWhen dedicated file or search tools (such as Read, Glob, Grep, Edit, Write, or MultiEdit) are listed above, prefer them for reading, searching, creating, and editing workspace files instead of Bash/shell commands (cat, grep, rg, find, ls, sed, awk, echo/heredoc), because shell commands trigger manual approval prompts in the client.\n")
	switch choiceMode {
	case "required":
		b.WriteString("\nYou MUST invoke at least one of the registered client tools above before answering.\n")
	case "function":
		if strings.TrimSpace(choiceName) != "" {
			alias := r.OrigToAlias[choiceName]
			if alias == "" {
				alias = outpostToolAlias(choiceName)
			}
			fmt.Fprintf(&b, "\nYou MUST invoke client tool %q (%s) for this turn.\n", choiceName, outpostModelToolNameForAlias(devName, alias))
		}
	}
	if !parallelTools {
		b.WriteString("Invoke at most ONE client tool per step; wait for its result before calling another.\n")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// QBS HTTP 调用层 (putOutpost / unregisterOutpost / invokeAgent store)
// ---------------------------------------------------------------------------

func qbsAppVersion(appVer string) string {
	if strings.TrimSpace(appVer) != "" {
		return strings.TrimSpace(appVer)
	}
	return "1.2.4660"
}

func qbsCall(ctx context.Context, sess *Session, appVersion, kind string, eventBody any) (map[string]any, error) {
	if sess == nil || strings.TrimSpace(sess.TenantURL) == "" || strings.TrimSpace(sess.jwt()) == "" {
		return nil, fmt.Errorf("%w: missing tenant_url or jwt", ErrOutpostUnsupported)
	}
	if isGatewayProvider(sess.Provider) {
		return nil, fmt.Errorf("%w: gateway provider", ErrOutpostUnsupported)
	}
	u, err := url.Parse(strings.TrimSpace(sess.TenantURL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("%w: invalid tenant_url", ErrOutpostUnsupported)
	}
	u.Path = "/integration/quick-work/qbs-event"
	u.RawQuery = ""
	u.Fragment = ""

	reqPayload := map[string]any{
		"eventType": kind,
		"eventBody": map[string]any{kind: eventBody},
	}
	bodyBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+sess.jwt())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Amazon Quick/"+qbsAppVersion(appVersion))
	req.Header.Set("X-QuickDsk-mdid", "user-conversation")
	req.Header.Set("x-amzn-quick-atype", "ufg")

	client := outpostHTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, outpostMaxHTTPResponseSize))
	if err != nil {
		return nil, err
	}

	retryAfter := parseRetryAfterHeader(resp.Header.Get("Retry-After"), time.Now())
	errCode := strings.TrimSpace(resp.Header.Get("x-amzn-errortype"))

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, &UpstreamError{
			StatusCode: http.StatusUnauthorized,
			Code:       firstNonEmptyStr(errCode, "Unauthorized"),
			Err:        fmt.Errorf("qbs-event %s HTTP 401: %s", kind, truncateText(string(raw), 240)),
		}
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
		return nil, fmt.Errorf("%w: qbs-event %s HTTP %d", ErrOutpostUnsupported, kind, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rawText := string(raw)
		lower := strings.ToLower(rawText)
		if resp.StatusCode == http.StatusBadRequest && (strings.Contains(lower, "unknown event") || strings.Contains(lower, "unsupported") || strings.Contains(lower, "not supported")) {
			return nil, fmt.Errorf("%w: qbs-event %s HTTP %d: %s", ErrOutpostUnsupported, kind, resp.StatusCode, truncateText(rawText, 240))
		}
		return nil, &UpstreamError{
			StatusCode: resp.StatusCode,
			Code:       errCode,
			RetryAfter: retryAfter,
			Err:        fmt.Errorf("qbs-event %s HTTP %d: %s", kind, resp.StatusCode, truncateText(rawText, 300)),
		}
	}

	var outer map[string]any
	if err := json.Unmarshal(raw, &outer); err != nil {
		return nil, fmt.Errorf("qbs-event %s invalid JSON: %w", kind, err)
	}
	if errMsg, _ := outer["errorMessage"].(string); strings.TrimSpace(errMsg) != "" {
		lower := strings.ToLower(errMsg)
		if strings.Contains(lower, "unsupported") || strings.Contains(lower, "unknown event") || strings.Contains(lower, "not supported") {
			return nil, fmt.Errorf("%w: %s", ErrOutpostUnsupported, errMsg)
		}
		return nil, &UpstreamError{
			StatusCode: http.StatusBadGateway,
			Code:       errCode,
			Err:        fmt.Errorf("qbs-event %s error: %s", kind, errMsg),
		}
	}
	respMap, _ := outer["eventResponse"].(map[string]any)
	if respMap == nil {
		return nil, fmt.Errorf("%w: qbs-event %s missing eventResponse", ErrOutpostUnsupported, kind)
	}
	innerAny, exists := respMap[kind+"Response"]
	if !exists || innerAny == nil {
		return map[string]any{}, nil
	}
	switch v := innerAny.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return map[string]any{}, nil
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(v), &parsed); err != nil {
			return map[string]any{"raw": v}, nil
		}
		return parsed, nil
	case map[string]any:
		return v, nil
	default:
		return map[string]any{}, nil
	}
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func qbsPutOutpost(ctx context.Context, sess *Session, appVersion, outpostID, devName string, reg *outpostToolRegistry) error {
	caps := []any{}
	if reg != nil && !reg.empty() {
		caps = append(caps, reg.capability())
	}
	_, err := qbsCall(ctx, sess, appVersion, "putOutpost", map[string]any{
		"outpostId":    outpostID,
		"name":         devName,
		"outpostType":  "DESKTOP",
		"capabilities": caps,
		"status":       "ONLINE",
	})
	return err
}

// qbsUnregisterOutpost 释放虚拟设备，脱离父上下文取消以确保断开或报错后也能完成清理（最多重试 1 次）。
func qbsUnregisterOutpost(sess *Session, appVersion, outpostID string) error {
	if sess == nil || strings.TrimSpace(outpostID) == "" || strings.TrimSpace(sess.TenantURL) == "" {
		return nil
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := qbsCall(cctx, sess, appVersion, "unregisterOutpost", map[string]any{
			"outpostId": outpostID,
		})
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if errors.Is(err, ErrOutpostUnsupported) {
			return err
		}
	}
	return lastErr
}

func qbsInvokeAgentStore(ctx context.Context, sess *Session, appVersion, outpostID, workingID, method, path string, body any) error {
	resp, err := qbsCall(ctx, sess, appVersion, "invokeAgent", map[string]any{
		"path":             path,
		"queryParams":      map[string]any{"_method": method},
		"body":             body,
		"outpostId":        outpostID,
		"workingSessionId": workingID,
	})
	if err != nil {
		return err
	}
	statusNum := 0
	switch v := resp["status"].(type) {
	case float64:
		statusNum = int(v)
	case int:
		statusNum = v
	case json.Number:
		if n, convErr := v.Int64(); convErr == nil {
			statusNum = int(n)
		}
	}
	if statusNum == 404 || statusNum == 405 || statusNum == 501 {
		return fmt.Errorf("%w: invokeAgent %s HTTP %d", ErrOutpostUnsupported, path, statusNum)
	}
	if statusNum < 200 || statusNum >= 300 {
		return fmt.Errorf("invokeAgent %s returned status %d", path, statusNum)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 结构化历史导入 (store/sessions + events/batch)
// ---------------------------------------------------------------------------

type outpostPendingCall struct {
	MessageID string
	ModelName string
}

// messageHasBinaryMedia 检查单条消息是否包含需要带外二进制上传的图片或 PDF/非纯文本文档。
func messageHasBinaryMedia(m Message) bool {
	parts, ok := m.Content.([]any)
	if !ok {
		return false
	}
	for _, part := range parts {
		pm, ok := part.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := pm["type"].(string)
		switch strings.ToLower(strings.TrimSpace(typ)) {
		case "image_url", "input_image", "image", "document", "file":
			return true
		}
	}
	return false
}

func outpostEventID(prefix, threadID, key string) string {
	return prefix + "-" + sha256LowerHex(threadID+"\x00"+key)[:32]
}

// buildOutpostHistoryEvents 将先前的完整对话消息切片转为 Quick 服务端原生事件序列。
// 遇到未闭合工具调用、孤立 tool_result 或多模态附件时返回 errOutpostHistoryNotImportable。
func buildOutpostHistoryEvents(msgs []Message, threadID, devName string) ([]map[string]any, error) {
	trimmed := nonLeadingMessages(msgs)
	if len(trimmed) == 0 {
		return nil, nil
	}
	pending := make(map[string]outpostPendingCall)
	seenCalls := make(map[string]bool)
	var events []map[string]any
	push := func(kind, messageID string, data map[string]any) {
		idx := len(events)
		id := outpostEventID("evt", threadID, fmt.Sprintf("%d:%s:%s", idx, kind, messageID))
		events = append(events, map[string]any{
			"id":                     id,
			"type":                   kind,
			"session_id":             threadID,
			"message_id":             messageID,
			"thread_id":              nil,
			"root_group_id":          nil,
			"server_sequence_number": nil,
			"data":                   data,
		})
	}

	pendingUserMsgID := ""
	var pendingToolMsgs []Message
	flushToolResults := func() error {
		if len(pendingToolMsgs) == 0 {
			return nil
		}
		for _, tm := range pendingToolMsgs {
			callID := strings.TrimSpace(tm.ToolCallID)
			call, ok := pending[callID]
			if !ok || callID == "" {
				return fmt.Errorf("%w: orphan tool_result id=%q", errOutpostHistoryNotImportable, callID)
			}
			delete(pending, callID)
			rawText := contentText(tm.Content)
			status := "success"
			if strings.HasPrefix(rawText, "[tool error] ") {
				status = "error"
				rawText = strings.TrimPrefix(rawText, "[tool error] ")
			}
			push("tool_result", call.MessageID, map[string]any{
				"tool_use_id": callID,
				"tool_name":   call.ModelName,
				"result":      rawText,
				"status":      status,
			})
			push("tool_result_projection", call.MessageID, map[string]any{
				"tool_use_id": callID,
				"tool_name":   call.ModelName,
				"result":      rawText,
				"content":     []map[string]any{{"text": rawText}},
				"status":      status,
			})
		}
		pendingToolMsgs = nil
		pendingUserMsgID = ""
		return nil
	}

	for idx, m := range trimmed {
		if messageHasBinaryMedia(m) {
			return nil, fmt.Errorf("%w: message %d contains media attachment", errOutpostHistoryNotImportable, idx)
		}
		role := strings.ToLower(strings.TrimSpace(m.Role))
		msgID := outpostEventID("msg", threadID, fmt.Sprintf("%d:%s", idx, role))
		switch role {
		case "assistant":
			if err := flushToolResults(); err != nil {
				return nil, err
			}
			if len(pending) > 0 {
				return nil, fmt.Errorf("%w: assistant precedes unresolved tool_calls", errOutpostHistoryNotImportable)
			}
			push("message_assistant_start", msgID, map[string]any{})
			text := cleanQuickTextMarkers(contentText(m.Content))
			calls := make([]ToolCall, 0, len(m.ToolCalls)+1)
			calls = append(calls, m.ToolCalls...)
			if m.FunctionCall != nil && len(calls) == 0 {
				calls = append(calls, ToolCall{
					ID:       fmt.Sprintf("call_%d", idx),
					Type:     "function",
					Function: *m.FunctionCall,
				})
			}
			for ci, tc := range calls {
				callID := strings.TrimSpace(tc.ID)
				origName := strings.TrimSpace(tc.Function.Name)
				if callID == "" {
					callID = fmt.Sprintf("call_%d_%d", idx, ci)
				}
				if origName == "" || seenCalls[callID] {
					return nil, fmt.Errorf("%w: invalid or duplicate tool call id=%q", errOutpostHistoryNotImportable, callID)
				}
				seenCalls[callID] = true
				var argObj any = map[string]any{}
				if rawArgs := strings.TrimSpace(tc.Function.Arguments); rawArgs != "" {
					if err := json.Unmarshal([]byte(rawArgs), &argObj); err != nil {
						return nil, fmt.Errorf("%w: unmarshal tool arguments: %v", errOutpostHistoryNotImportable, err)
					}
					if _, isObj := argObj.(map[string]any); !isObj {
						argObj = map[string]any{"value": argObj}
					}
				}
				mName := outpostModelToolName(devName, origName)
				push("tool_start", msgID, map[string]any{
					"tool_use_id": callID,
					"tool_name":   mName,
				})
				push("tool_arguments", msgID, map[string]any{
					"tool_use_id": callID,
					"arguments":   argObj,
				})
				pending[callID] = outpostPendingCall{
					MessageID: msgID,
					ModelName: mName,
				}
			}
			if strings.TrimSpace(text) == "" && len(calls) == 0 {
				return nil, fmt.Errorf("%w: empty assistant message", errOutpostHistoryNotImportable)
			}
			push("message_assistant_complete", msgID, map[string]any{
				"full_content": text,
			})
		case "tool":
			if pendingUserMsgID == "" {
				pendingUserMsgID = msgID
			}
			pendingToolMsgs = append(pendingToolMsgs, m)
		case "user":
			if err := flushToolResults(); err != nil {
				return nil, err
			}
			if len(pending) > 0 {
				return nil, fmt.Errorf("%w: user turn missing prior tool results", errOutpostHistoryNotImportable)
			}
			text := contentText(m.Content)
			if strings.TrimSpace(text) == "" {
				return nil, fmt.Errorf("%w: empty user message", errOutpostHistoryNotImportable)
			}
			push("message_user", msgID, map[string]any{
				"content":     text,
				"attachments": []any{},
			})
		case "system", "developer":
			// 会话中间动态追加的 developer 提示折叠进 user 事件，避免丢失规则
			if err := flushToolResults(); err != nil {
				return nil, err
			}
			if len(pending) > 0 {
				return nil, fmt.Errorf("%w: developer note precedes pending tool results", errOutpostHistoryNotImportable)
			}
			if text := strings.TrimSpace(contentText(m.Content)); text != "" {
				push("message_user", msgID, map[string]any{
					"content":     "<system_update>\n" + text + "\n</system_update>",
					"attachments": []any{},
				})
			}
		default:
			return nil, fmt.Errorf("%w: unsupported role %q", errOutpostHistoryNotImportable, role)
		}
	}
	if err := flushToolResults(); err != nil {
		return nil, err
	}
	if len(pending) > 0 {
		return nil, fmt.Errorf("%w: unresolved tool_use at end of history", errOutpostHistoryNotImportable)
	}
	return events, nil
}

// importOutpostHistory 创建会话记录并按每批 100 条分片写入结构化历史事件。
func importOutpostHistory(ctx context.Context, sess *Session, appVer, outpostID, workingID, threadID, title string, keepSessions bool, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	if strings.TrimSpace(title) == "" {
		title = "QuickRelay conversation"
	}
	if err := qbsInvokeAgentStore(ctx, sess, appVer, outpostID, workingID, "POST", "store/sessions", map[string]any{
		"session_id":   threadID,
		"title":        title,
		"session_type": "main",
		"incognito":    !keepSessions,
	}); err != nil {
		return err
	}
	baseTS := float64(time.Now().UnixNano()) / 1e9
	stamped := make([]map[string]any, len(events))
	for i, ev := range events {
		cp := make(map[string]any, len(ev)+1)
		for k, v := range ev {
			cp[k] = v
		}
		cp["timestamp"] = baseTS + float64(i)*0.000001
		stamped[i] = cp
	}
	batchPath := "store/sessions/" + threadID + "/events/batch"
	for start := 0; start < len(stamped); start += outpostHistoryBatchSize {
		end := start + outpostHistoryBatchSize
		if end > len(stamped) {
			end = len(stamped)
		}
		if err := qbsInvokeAgentStore(ctx, sess, appVer, outpostID, workingID, "POST", batchPath, map[string]any{
			"events":          stamped[start:end],
			"update_metadata": true,
		}); err != nil {
			return err
		}
	}
	return nil
}

// outpostTurnInput 描述从 OpenAI/Anthropic 消息数组中拆解出的虚拟 Outpost 当前轮输入与可导入历史前缀。
type outpostTurnInput struct {
	SystemPrompt       string
	HistoryMessages    []Message
	HistoryPrefixHash  string
	TurnUserText       string
	TurnToolResults    []outpostToolResultItem
	RawTailHasTools    bool
	LatestUserPreview  string
}

type outpostToolResultItem struct {
	ToolCallID string
	Text       string
	IsError    bool
}

// extractOutpostTurnInput 把完整的 req.Messages 按“历史前缀 + 本轮待处理用户话语/工具结果”拆分，
// 供原生 tool_result 原位续写或冷启动 events/batch 历史恢复使用。
func extractOutpostTurnInput(msgs []Message) (outpostTurnInput, error) {
	var out outpostTurnInput
	if len(msgs) == 0 {
		return out, errors.New("messages must not be empty")
	}
	var sysParts []string
	idx := 0
	for idx < len(msgs) {
		role := strings.ToLower(strings.TrimSpace(msgs[idx].Role))
		if role != "system" && role != "developer" {
			break
		}
		if txt := strings.TrimSpace(contentText(msgs[idx].Content)); txt != "" {
			sysParts = append(sysParts, txt)
		}
		idx++
	}
	nonLead := msgs[idx:]
	if len(nonLead) == 0 {
		return out, errors.New("no non-system messages")
	}

	// 从尾部回溯本轮最新的用户回合（可能是一连串 tool 结果 + 同轮附加的 user 文本）
	tailStart := len(nonLead)
	for tailStart > 0 {
		role := strings.ToLower(strings.TrimSpace(nonLead[tailStart-1].Role))
		if role != "user" && role != "tool" && role != "developer" && role != "system" {
			break
		}
		tailStart--
	}
	if tailStart == len(nonLead) {
		return out, errors.New("last message must be user or tool")
	}

	out.HistoryMessages = nonLead[:tailStart]
	out.HistoryPrefixHash = hashMessagesPrefix(out.HistoryMessages)

	var userTexts []string
	for _, m := range nonLead[tailStart:] {
		if messageHasBinaryMedia(m) {
			return out, fmt.Errorf("%w: current turn contains media attachment", errOutpostHistoryNotImportable)
		}
		role := strings.ToLower(strings.TrimSpace(m.Role))
		switch role {
		case "tool":
			rawText := contentText(m.Content)
			isErr := false
			if strings.HasPrefix(rawText, "[tool error] ") {
				isErr = true
				rawText = strings.TrimPrefix(rawText, "[tool error] ")
			}
			out.RawTailHasTools = true
			out.TurnToolResults = append(out.TurnToolResults, outpostToolResultItem{
				ToolCallID: strings.TrimSpace(m.ToolCallID),
				Text:       rawText,
				IsError:    isErr,
			})
		case "user":
			if txt := strings.TrimSpace(contentText(m.Content)); txt != "" {
				userTexts = append(userTexts, txt)
			}
		case "system", "developer":
			if txt := strings.TrimSpace(contentText(m.Content)); txt != "" {
				sysParts = append(sysParts, txt)
			}
		}
	}
	out.SystemPrompt = strings.Join(sysParts, "\n\n")
	out.TurnUserText = strings.Join(userTexts, "\n\n")
	if len(out.TurnToolResults) == 0 && strings.TrimSpace(out.TurnUserText) == "" {
		return out, errors.New("user turn has no text or tool results")
	}
	return out, nil
}

func hashMessagesPrefix(msgs []Message) string {
	if len(msgs) == 0 {
		return ""
	}
	h := sha256.New()
	for _, m := range msgs {
		h.Write([]byte(messageFingerprint(m)))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// outpostToolIDSetMatch 校验本轮传入的 tool_result ID 集合是否与挂起 WS 正在等待的 ID 集合完全一致（无缺失也无多出）。
func outpostToolIDSetMatch(pending []string, items []outpostToolResultItem) bool {
	if len(pending) == 0 || len(pending) != len(items) {
		return false
	}
	want := make(map[string]int, len(pending))
	for _, id := range pending {
		want[id]++
	}
	got := make(map[string]int, len(items))
	for _, it := range items {
		if it.ToolCallID == "" {
			return false
		}
		got[it.ToolCallID]++
	}
	if len(want) != len(got) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// cleanQuickTextMarkers 供历史回放前清洗常见 Quick 内部行首引用标记（如 [assistant] / [main]），
// 同时严格保护代码块内部示例文本不受影响。
func cleanQuickTextMarkers(text string) string {
	cleaned, _ := cleanQuickOutput(text, nil, nil)
	return cleaned
}

// filterOutpostToolCalls 依据 tool_choice 与 parallel_tool_calls 对原生下发的工具调用集合执行客户端策略对齐。
func filterOutpostToolCalls(calls []ToolCallState, choiceMode, choiceName string, parallel bool) []ToolCallState {
	if len(calls) == 0 {
		return nil
	}
	out := make([]ToolCallState, 0, len(calls))
	for _, c := range calls {
		if choiceMode == "function" && choiceName != "" && c.Name != choiceName {
			continue
		}
		out = append(out, c)
		if !parallel {
			break
		}
	}
	return out
}

// sortedMapKeys 仅用于调试与测试日志有序输出键名（不含敏感内容）。
func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
