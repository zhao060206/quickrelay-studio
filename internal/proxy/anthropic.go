package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type anthropicTool struct {
	Type        string `json:"type,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthropicContentBlock struct {
	Type      string                `json:"type"`
	Text      string                `json:"text,omitempty"`
	Thinking  string                `json:"thinking,omitempty"`
	ID        string                `json:"id,omitempty"`
	Name      string                `json:"name,omitempty"`
	Input     json.RawMessage       `json:"input,omitempty"`
	ToolUseID string                `json:"tool_use_id,omitempty"`
	Content   json.RawMessage       `json:"content,omitempty"`
	IsError   bool                  `json:"is_error,omitempty"`
	Source    *anthropicImageSource `json:"source,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

type anthropicMessagesRequest struct {
	Model        string                 `json:"model"`
	MaxTokens    int                    `json:"max_tokens,omitempty"`
	System       json.RawMessage        `json:"system,omitempty"`
	Messages     []anthropicMessage     `json:"messages"`
	Tools        []anthropicTool        `json:"tools,omitempty"`
	ToolChoice   any                    `json:"tool_choice,omitempty"`
	Thinking     *anthropicThinking     `json:"thinking,omitempty"`
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
	Effort       string                 `json:"effort,omitempty"`
	Stream       bool                   `json:"stream,omitempty"`
}

// mapAnthropicModel 将 Claude Code 与 Anthropic 客户端传入的模型代号智能路由至对应上游档位：
// 若存在用户在智能体配置中定义的自定义别名映射，则优先命中自定义路由；
// 否则默认将 Haiku 走极速模式 fast，Opus/Sonnet 走主模型 smart。
func (s *Server) mapAnthropicModel(model string) string {
	lower := strings.ToLower(strings.TrimSpace(model))
	if lower == "" {
		return s.cfg.DefaultModel
	}
	if target, ok := s.cfg.LookupModelAlias(lower); ok {
		return s.mapBuiltinOrModel(target)
	}
	switch {
	case strings.Contains(lower, "haiku"):
		return "fast"
	case strings.Contains(lower, "opus"), strings.Contains(lower, "sonnet"), strings.Contains(lower, "smart"):
		return "smart"
	case strings.Contains(lower, "balanced"):
		return "balanced"
	case strings.Contains(lower, "auto"):
		return "auto"
	default:
		return s.mapModel(model)
	}
}

func extractAnthropicSystem(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []anthropicContentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, blk := range blocks {
			if strings.TrimSpace(blk.Text) != "" {
				parts = append(parts, blk.Text)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

func convertAnthropicToolChoice(raw any) (choice any, parallel *bool) {
	m, ok := raw.(map[string]any)
	if !ok {
		return raw, nil
	}
	if disableParallel, ok := m["disable_parallel_tool_use"].(bool); ok && disableParallel {
		v := false
		parallel = &v
	}
	tp, _ := m["type"].(string)
	switch strings.ToLower(strings.TrimSpace(tp)) {
	case "none":
		return "none", parallel
	case "any":
		return "required", parallel
	case "tool":
		name, _ := m["name"].(string)
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": strings.TrimSpace(name)},
		}, parallel
	default:
		return "auto", parallel
	}
}

func resolveAnthropicThinkingEffort(req *anthropicMessagesRequest) string {
	if req.OutputConfig != nil && strings.TrimSpace(req.OutputConfig.Effort) != "" {
		return strings.TrimSpace(req.OutputConfig.Effort)
	}
	if strings.TrimSpace(req.Effort) != "" {
		return strings.TrimSpace(req.Effort)
	}
	if req.Thinking == nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(req.Thinking.Type)) {
	case "disabled":
		return "none"
	case "adaptive":
		return "high"
	case "enabled":
		b := req.Thinking.BudgetTokens
		switch {
		case b <= 0:
			return "medium"
		case b <= 4096:
			return "low"
		case b <= 16000:
			return "medium"
		default:
			return "high"
		}
	default:
		return ""
	}
}

func convertAnthropicToolResultContent(raw json.RawMessage, isError bool) any {
	prefix := ""
	if isError {
		prefix = "[tool error] "
	}
	if len(raw) == 0 {
		return prefix + "(empty)"
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return prefix + s
	}
	var blocks []anthropicContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return prefix + strings.TrimSpace(string(raw))
	}
	var parts []any
	if prefix != "" {
		parts = append(parts, map[string]any{"type": "text", "text": prefix})
	}
	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			if blk.Text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": blk.Text})
			}
		case "image":
			if p := anthropicImagePart(blk.Source); p != nil {
				parts = append(parts, p)
			}
		}
	}
	if len(parts) == 0 {
		return prefix + "(empty)"
	}
	return parts
}

func anthropicImagePart(src *anthropicImageSource) map[string]any {
	if src == nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(src.Type)) {
	case "base64":
		if strings.TrimSpace(src.Data) == "" {
			return nil
		}
		media := strings.TrimSpace(src.MediaType)
		if media == "" {
			media = "image/png"
		}
		return map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": "data:" + media + ";base64," + strings.TrimSpace(src.Data),
			},
		}
	case "url":
		if strings.TrimSpace(src.URL) == "" {
			return nil
		}
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": strings.TrimSpace(src.URL)},
		}
	default:
		return nil
	}
}

// anthropicToChatRequest 将 Anthropic Messages API 请求完整无损地转换为代理内部 ChatRequest 模型。
func anthropicToChatRequest(ar *anthropicMessagesRequest) ChatRequest {
	var msgs []Message
	if sysText := extractAnthropicSystem(ar.System); strings.TrimSpace(sysText) != "" {
		msgs = append(msgs, Message{Role: "system", Content: sysText})
	}

	for _, am := range ar.Messages {
		role := strings.ToLower(strings.TrimSpace(am.Role))
		if role == "" {
			role = "user"
		}
		var plain string
		if json.Unmarshal(am.Content, &plain) == nil {
			msgs = append(msgs, Message{Role: role, Content: plain})
			continue
		}
		var blocks []anthropicContentBlock
		if json.Unmarshal(am.Content, &blocks) != nil {
			msgs = append(msgs, Message{Role: role, Content: strings.TrimSpace(string(am.Content))})
			continue
		}

		if role == "assistant" {
			var textParts []string
			var calls []ToolCall
			for _, blk := range blocks {
				switch blk.Type {
				case "text":
					if blk.Text != "" {
						textParts = append(textParts, blk.Text)
					}
				case "tool_use":
					args := normalizeArguments(blk.Input)
					calls = append(calls, ToolCall{
						ID:   blk.ID,
						Type: "function",
						Function: FunctionCall{
							Name:      blk.Name,
							Arguments: args,
						},
					})
				}
			}
			msgs = append(msgs, Message{
				Role:      "assistant",
				Content:   strings.Join(textParts, "\n"),
				ToolCalls: calls,
			})
			continue
		}

		var pendingParts []any
		flushUser := func() {
			if len(pendingParts) == 0 {
				return
			}
			msgs = append(msgs, Message{Role: role, Content: pendingParts})
			pendingParts = nil
		}
		for _, blk := range blocks {
			switch blk.Type {
			case "tool_result":
				flushUser()
				msgs = append(msgs, Message{
					Role:       "tool",
					ToolCallID: blk.ToolUseID,
					Content:    convertAnthropicToolResultContent(blk.Content, blk.IsError),
				})
			case "text":
				if blk.Text != "" {
					pendingParts = append(pendingParts, map[string]any{"type": "text", "text": blk.Text})
				}
			case "image":
				if imgPart := anthropicImagePart(blk.Source); imgPart != nil {
					pendingParts = append(pendingParts, imgPart)
				}
			}
		}
		flushUser()
	}

	var tools []Tool
	for _, t := range ar.Tools {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		params := t.InputSchema
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools = append(tools, Tool{
			Type: "function",
			Function: ToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}

	toolChoice, parallel := convertAnthropicToolChoice(ar.ToolChoice)
	var reasoning *ReasoningOptions
	if eff := resolveAnthropicThinkingEffort(ar); eff != "" {
		reasoning = &ReasoningOptions{Effort: eff}
	}

	return ChatRequest{
		Model:             ar.Model,
		Messages:          msgs,
		Stream:            ar.Stream,
		Tools:             tools,
		ToolChoice:        toolChoice,
		ParallelToolCalls: parallel,
		Reasoning:         reasoning,
	}
}

// anthropicResponseModel 优先回传客户端原始请求模型名，避免路由别名（如 smart）导致客户端误判 200K 上下文上限。
func anthropicResponseModel(requestedModel, fallback string) string {
	if m := strings.TrimSpace(requestedModel); m != "" {
		return m
	}
	return fallback
}

func anthropicUsageMap(u Usage) map[string]any {
	inputTokens, cacheReadTokens, cacheWriteTokens := u.ContextSnapshot()
	return map[string]any{
		"input_tokens":                inputTokens,
		"output_tokens":               u.CompletionTokens,
		"cache_read_input_tokens":     cacheReadTokens,
		"cache_creation_input_tokens": cacheWriteTokens,
	}
}

func decodeToolInputObj(arguments string) any {
	var parsed any
	if strings.TrimSpace(arguments) != "" && json.Unmarshal([]byte(arguments), &parsed) == nil && parsed != nil {
		return parsed
	}
	return map[string]any{}
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": message,
		},
	})
}

func anthropicErrorType(status int, err error) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusBadRequest:
		return "invalid_request_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "api_error"
	}
}

// handleAnthropicMessages 处理 Claude Code 及 Anthropic SDK 原生 /v1/messages 请求。
func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "POST required")
		return
	}
	if !s.tokens.Allowed(r) {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid proxy API key")
		return
	}

	var ar anthropicMessagesRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20))
	if err := dec.Decode(&ar); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}

	req := anthropicToChatRequest(&ar)
	turn, err := s.prepareTurn(r, &req, "anthropic", "/v1/messages", "msg_")
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	defer turn.Close()

	if !req.Stream {
		res, lastErr := turn.runChat(nil, nil, nil)
		if lastErr != nil {
			s.sessions.reset(turn.conv)
			status, _ := chatErrorStatus(lastErr)
			turn.finishTurn("failed", "", res, 0, lastErr)
			writeAnthropicError(w, status, anthropicErrorType(status, lastErr), lastErr.Error())
			return
		}
		turn.saveConversation(res)
		text, toolCalls := turn.responseOutput(res)

		var blocks []map[string]any
		if strings.TrimSpace(res.Thinking) != "" {
			blocks = append(blocks, map[string]any{
				"type":      "thinking",
				"thinking":  res.Thinking,
				"signature": "quickrelay",
			})
		}
		if hasVisibleText(text) {
			blocks = append(blocks, map[string]any{
				"type": "text",
				"text": text,
			})
		}
		for _, tc := range toolCalls {
			blocks = append(blocks, map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": decodeToolInputObj(tc.Function.Arguments),
			})
		}
		if len(blocks) == 0 {
			blocks = append(blocks, map[string]any{"type": "text", "text": ""})
		}

		stopReason := "end_turn"
		if len(toolCalls) > 0 {
			stopReason = "tool_use"
		}
		turn.finishTurn("ok", stopReason, res, len(toolCalls), nil)
		respModel := anthropicResponseModel(turn.requestedModel, resolvedResponseModel(turn.model, res.Usage))
		writeJSON(w, http.StatusOK, map[string]any{
			"id":            turn.id,
			"type":          "message",
			"role":          "assistant",
			"model":         respModel,
			"content":       blocks,
			"stop_reason":   stopReason,
			"stop_sequence": nil,
			"usage":         anthropicUsageMap(res.Usage),
		})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "response writer does not support flushing")
		return
	}

	var writeMu sync.Mutex
	started := false
	var lastWriteAt time.Time
	writeEventLocked := func(event string, payload any) {
		if !started {
			started = true
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusOK)
		}
		body, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body)
		flusher.Flush()
		lastWriteAt = time.Now()
	}
	writeEvent := func(event string, payload any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		writeEventLocked(event, payload)
	}
	ensureStarted := func() {
		writeMu.Lock()
		defer writeMu.Unlock()
		if started {
			return
		}
		writeEventLocked("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            turn.id,
				"type":          "message",
				"role":          "assistant",
				"model":         anthropicResponseModel(turn.requestedModel, turn.model),
				"content":       []any{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage": map[string]any{
					"input_tokens":  0,
					"output_tokens": 0,
				},
			},
		})
	}
	isStarted := func() bool {
		writeMu.Lock()
		defer writeMu.Unlock()
		return started
	}
	lastWrite := func() time.Time {
		writeMu.Lock()
		defer writeMu.Unlock()
		return lastWriteAt
	}
	sendAnthropicPing := func() {
		writeMu.Lock()
		defer writeMu.Unlock()
		if !started {
			return
		}
		_, _ = fmt.Fprint(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
		flusher.Flush()
		lastWriteAt = time.Now()
	}

	nextBlockIdx := 0
	openType := ""
	openIdx := -1
	closeOpenBlock := func() {
		if openType == "" {
			return
		}
		if openType == "thinking" {
			writeEvent("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": openIdx,
				"delta": map[string]any{
					"type":      "signature_delta",
					"signature": "quickrelay",
				},
			})
		}
		writeEvent("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": openIdx,
		})
		openType = ""
		openIdx = -1
	}
	emitThinking := func(chunk string) {
		if chunk == "" {
			return
		}
		ensureStarted()
		if openType != "thinking" {
			closeOpenBlock()
			openIdx = nextBlockIdx
			nextBlockIdx++
			openType = "thinking"
			writeEvent("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": openIdx,
				"content_block": map[string]any{
					"type":     "thinking",
					"thinking": "",
				},
			})
		}
		writeEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": openIdx,
			"delta": map[string]any{
				"type":     "thinking_delta",
				"thinking": chunk,
			},
		})
	}

	sawVisibleText := false
	var pendingText strings.Builder
	emittedTools := 0

	emitText := func(chunk string) {
		if chunk == "" {
			return
		}
		if !sawVisibleText {
			pendingText.WriteString(chunk)
			if !hasVisibleText(pendingText.String()) {
				return
			}
			chunk = pendingText.String()
			pendingText.Reset()
			sawVisibleText = true
		}
		ensureStarted()
		if openType != "text" {
			closeOpenBlock()
			openIdx = nextBlockIdx
			nextBlockIdx++
			openType = "text"
			writeEvent("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": openIdx,
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			})
		}
		writeEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": openIdx,
			"delta": map[string]any{
				"type": "text_delta",
				"text": chunk,
			},
		})
	}
	emitToolReply := func(tc ToolCallReply) {
		ensureStarted()
		closeOpenBlock()
		idx := nextBlockIdx
		nextBlockIdx++
		writeEvent("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": idx,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Name,
				"input": map[string]any{},
			},
		})
		args := strings.TrimSpace(tc.Arguments)
		if args == "" {
			args = "{}"
		}
		writeEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": args,
			},
		})
		writeEvent("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": idx,
		})
		emittedTools++
	}

	var bridgeStream *toolBridgeStream
	if turn.bridgeActive {
		bridgeStream = newToolBridgeStream(turn.allowedTools, turn.choiceMode, turn.choiceName, turn.parallelTools)
	}
	quickFilter := newQuickOutputFilter(turn.allowedTools, func(tc ToolCallReply) bool {
		return turn.bridgeActive && emittedTools == 0 && bridgeStream.acceptCall(tc)
	})
	emitFilteredText := func(raw string) {
		cleanText, calls := quickFilter.Feed(raw)
		if cleanText != "" {
			emitText(cleanText)
		}
		for _, tc := range calls {
			emitToolReply(tc)
		}
	}

	stopHB := startSSEHeartbeat(
		turn.ctx,
		turn.needsThinkingHeartbeat(),
		isStarted,
		ensureStarted,
		sendAnthropicPing,
		lastWrite,
	)
	defer stopHB()

	res, lastErr := turn.runChat(func(ev StreamEvent) error {
		switch ev.Type {
		case "thinking":
			emitThinking(ev.Text)
		case "text":
			if ev.Text == "" {
				return nil
			}
			if !turn.bridgeActive {
				emitFilteredText(ev.Text)
				return nil
			}
			outText, outCalls := bridgeStream.Feed(ev.Text)
			if outText != "" {
				emitFilteredText(outText)
			}
			for _, tc := range outCalls {
				emitToolReply(tc)
			}
		}
		return nil
	}, isStarted, func() {
		sawVisibleText = false
		pendingText.Reset()
		emittedTools = 0
		nextBlockIdx = 0
		openType = ""
		openIdx = -1
		if bridgeStream != nil {
			bridgeStream.Reset()
		}
		quickFilter.Reset()
	})
	stopHB()

	if lastErr != nil {
		s.sessions.reset(turn.conv)
		if !isStarted() {
			status, _ := chatErrorStatus(lastErr)
			turn.finishTurn("failed", "", res, 0, lastErr)
			writeAnthropicError(w, status, anthropicErrorType(status, lastErr), lastErr.Error())
			return
		}
		closeOpenBlock()
		turn.finishTurn("failed", "", res, 0, lastErr)
		writeEvent("error", map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": lastErr.Error(),
			},
		})
		return
	}

	turn.saveConversation(res)
	if turn.bridgeActive {
		remText, remCalls := bridgeStream.Flush()
		if remText != "" {
			emitFilteredText(remText)
		}
		for _, tc := range remCalls {
			emitToolReply(tc)
		}
		fText, fCalls := quickFilter.Flush()
		if fText != "" {
			emitText(fText)
		}
		for _, tc := range fCalls {
			emitToolReply(tc)
		}
		if emittedTools == 0 && len(res.ToolCalls) > 0 {
			for _, tc := range limitToolReplies(nativeToolReplies(res.ToolCalls, turn.allowedTools), turn.choiceMode, turn.choiceName, turn.parallelTools) {
				emitToolReply(tc)
			}
		}
		if !sawVisibleText && emittedTools == 0 && hasVisibleText(res.Text) {
			outText, outTools := turn.responseOutput(res)
			if hasVisibleText(outText) {
				emitText(outText)
			}
			for _, tc := range outTools {
				emitToolReply(ToolCallReply{
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}
		}
	} else {
		fText, _ := quickFilter.Flush()
		if fText != "" {
			emitText(fText)
		}
		if !sawVisibleText && hasVisibleText(res.Text) {
			if cleanedRes, _ := cleanQuickOutput(res.Text, nil, nil); hasVisibleText(cleanedRes) {
				emitText(cleanedRes)
			}
		}
	}

	ensureStarted()
	closeOpenBlock()
	stopReason := "end_turn"
	if emittedTools > 0 {
		stopReason = "tool_use"
	}
	turn.finishTurn("ok", stopReason, res, emittedTools, nil)
	writeEvent("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": anthropicUsageMap(res.Usage),
	})
	writeEvent("message_stop", map[string]any{"type": "message_stop"})
}

// estimateTextTokens 按 UTF-8 中英文字符分布快速估算近似 token 数量，供本地 count_tokens 零延迟返回。
func estimateTextTokens(text string) int {
	if text == "" {
		return 0
	}
	asciiBytes := 0
	nonASCIIBytes := 0
	for i := 0; i < len(text); i++ {
		if text[i] < 0x80 {
			asciiBytes++
		} else {
			nonASCIIBytes++
		}
	}
	return (asciiBytes+3)/4 + (nonASCIIBytes+1)/2
}

// estimateAnthropicContentTokens 递归估算 Anthropic 消息内容块的 token 消耗，避免把图片或文档的 base64 载荷当成文本误算。
func estimateAnthropicContentTokens(raw json.RawMessage) int {
	return estimateAnthropicContentTokensDepth(raw, 0)
}

func estimateAnthropicContentTokensDepth(raw json.RawMessage, depth int) int {
	if len(raw) == 0 || depth > 4 {
		return 0
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return estimateTextTokens(s)
	}
	var blocks []anthropicContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return estimateTextTokens(string(raw))
	}
	total := 0
	for _, b := range blocks {
		switch b.Type {
		case "image":
			total += 1200
		case "document":
			if b.Source != nil && strings.EqualFold(b.Source.Type, "base64") {
				total += 2000
			} else {
				total += estimateTextTokens(b.Text)
			}
		case "text":
			total += estimateTextTokens(b.Text)
		case "thinking":
			total += estimateTextTokens(b.Thinking)
		case "tool_use":
			total += 12 + estimateTextTokens(b.Name) + estimateTextTokens(string(b.Input))
		case "tool_result":
			total += 12 + estimateAnthropicContentTokensDepth(b.Content, depth+1)
		default:
			total += estimateTextTokens(b.Text) + estimateTextTokens(b.Thinking)
			if len(b.Content) > 0 {
				total += estimateAnthropicContentTokensDepth(b.Content, depth+1)
			}
		}
	}
	return total
}

// handleAnthropicCountTokens 本地零延迟估算 Anthropic 请求 token 数，避免 Claude Code 额外等云端往返。
func (s *Server) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "POST required")
		return
	}
	if !s.tokens.Allowed(r) {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid proxy API key")
		return
	}
	var ar anthropicMessagesRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20))
	if err := dec.Decode(&ar); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}

	total := 16
	total += estimateTextTokens(extractAnthropicSystem(ar.System))
	for _, am := range ar.Messages {
		total += 6
		total += estimateAnthropicContentTokens(am.Content)
	}
	if len(ar.Tools) > 0 {
		if tb, err := json.Marshal(ar.Tools); err == nil {
			total += estimateTextTokens(string(tb))
		}
	}
	if total < 1 {
		total = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": total})
}
