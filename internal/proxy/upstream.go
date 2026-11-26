package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"quickrelay-studio/internal/config"
	"quickrelay-studio/internal/store"
)

const (
	defaultWSPath        = "/streaming/quickwork-chat-bidirectional-stream"
	defaultGatewayWSPath = "/ws/service/SpaceNeedleConversationService/operation/ChatBidirectionalStream"
	maxUserMessageBytes  = 63 * 1024
)

// wsKeepaliveInterval 为 WebSocket 保活心跳周期，用于刷新上游 95 秒无应用层帧超时计时。
var (
	wsDialHandshakeTimeout       = 12 * time.Second
	wsKeepaliveInterval          = 25 * time.Second
	wsReadIdleTimeout            = 2 * time.Minute
	wsReadIdleTimeoutMaxThinking = 5 * time.Minute
	wsCompleteGracePeriod        = 5 * time.Second
	wsCompleteGraceMaxThinking   = 10 * time.Second
	wsNativeToolTimeout          = 45 * time.Second
	wsNativeToolTotalTimeout     = 90 * time.Second
)

// ErrUserMessageTooLarge means the user_message frame cannot fit under the
// upstream 64 KB per-frame limit even after trimming older prompt text.
var ErrUserMessageTooLarge = errors.New("user_message exceeds the upstream 64 KB frame limit")

type Upstream struct {
	cfg *config.Config
}

func NewUpstream(cfg *config.Config) *Upstream {
	return &Upstream{cfg: cfg}
}

// ChatInput is one upstream turn.
type ChatInput struct {
	Prompt            string
	HistoryPrompt     string // 按消息边界拆分的先前历史文本（空表示无先前历史或无需拆分）
	CurrentTurn       string // 按消息边界拆分的当前轮完整输入（空表示等价于 Prompt）
	CurrentTurnIsTool bool   // 当前轮是否全部为客户端工具/函数回传结果
	LatestUserText    string
	Images            []ImageAttachment
	Model             string
	ThinkingEffort    string
	ThreadID          string
	Title             string
	SystemPrompt      string // 兼容旧调用方直接传入完整指令文本
	Guidelines        string // 工作区规范（<workspace_guidelines>），可在超帧时压缩或转为附件
	ToolProtocol      string // 客户端工具协议与紧凑 JSON 工具表（<client_tool_protocol>），主帧内严格保护不裁剪
	WorkMode          string // 工作场景模式：general | document | coding
	ChatAgentID       string
	CreateSession     bool
	DeleteSession     bool
	ClientToolsActive bool
	// OnDispatched 当 WebSocket 建连成功并已发出 user_message 帧、进入等待模型响应阶段时触发，用于解锁 SSE 首包预热计时。
	OnDispatched func()
}

// ToolCallState is a native tool invocation reported by the QuickWork agent.
type ToolCallState struct {
	ID        string
	Name      string
	Arguments string
}

// Usage aggregates turn_usage events across a multi-step turn.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CacheReadTokens  int
	CacheWriteTokens int
	// 单步上下文快照字段：保留单轮内部最后一步真实上下文占用，防止多步累加虚报给客户端
	LastPromptTokens     int
	LastCacheReadTokens  int
	LastCacheWriteTokens int
	HasLastStepContext   bool
	CostUSD              *float64
	Credits              []store.CreditUsage
	// ResolvedModelID 记录上游 turn_usage 返回的真实底层模型代号
	ResolvedModelID string
}

func (u *Usage) add(v Usage) {
	stepPrompt := max(v.PromptTokens, 0)
	stepCacheRead := max(v.CacheReadTokens, 0)
	stepCacheWrite := max(v.CacheWriteTokens, 0)

	u.PromptTokens += stepPrompt
	u.CompletionTokens += max(v.CompletionTokens, 0)
	u.TotalTokens += stepPrompt + max(v.CompletionTokens, 0)
	u.CacheReadTokens += stepCacheRead
	u.CacheWriteTokens += stepCacheWrite
	if stepPrompt+stepCacheRead+stepCacheWrite > 0 {
		u.LastPromptTokens = stepPrompt
		u.LastCacheReadTokens = stepCacheRead
		u.LastCacheWriteTokens = stepCacheWrite
		u.HasLastStepContext = true
	}
	if v.CostUSD != nil && *v.CostUSD >= 0 && !math.IsNaN(*v.CostUSD) && !math.IsInf(*v.CostUSD, 0) {
		if u.CostUSD == nil {
			value := 0.0
			u.CostUSD = &value
		}
		*u.CostUSD += *v.CostUSD
	}
	if m := strings.TrimSpace(v.ResolvedModelID); m != "" {
		u.ResolvedModelID = m
	}
}

// ContextSnapshot 返回当前轮最后一步真实上下文输入构成（供返回给客户端判断窗口占用）。
func (u Usage) ContextSnapshot() (promptTokens, cacheReadTokens, cacheWriteTokens int) {
	if u.HasLastStepContext {
		return u.LastPromptTokens, u.LastCacheReadTokens, u.LastCacheWriteTokens
	}
	promptTokens = u.PromptTokens
	cacheReadTokens = u.CacheReadTokens
	cacheWriteTokens = u.CacheWriteTokens
	if promptTokens == 0 && u.TotalTokens > u.CompletionTokens {
		diff := u.TotalTokens - u.CompletionTokens - cacheReadTokens - cacheWriteTokens
		if diff > 0 {
			promptTokens = diff
		}
	}
	return promptTokens, cacheReadTokens, cacheWriteTokens
}

// StreamEvent is a text delta or a native tool call from the upstream agent.
type StreamEvent struct {
	Type     string
	Text     string
	ToolCall *ToolCallState
}

type EventFunc func(StreamEvent) error

type ChatResult struct {
	Text      string
	Thinking  string
	ToolCalls []ToolCallState
	Usage     Usage
	ThreadID  string
}

// Chat sends one prompt through the QuickWork chat bidi WebSocket. Text
// deltas are reported through onEvent.
func (u *Upstream) Chat(ctx context.Context, sess *Session, in ChatInput, onEvent EventFunc) (ChatResult, error) {
	if strings.TrimSpace(in.Prompt) == "" && len(in.Images) == 0 {
		return ChatResult{}, fmt.Errorf("prompt is empty")
	}
	threadID := strings.TrimSpace(in.ThreadID)
	if threadID == "" {
		threadID = uuid()
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = titleFor(in.Prompt)
	}
	messageID := uuid()
	traceID := uuid()

	wsURL, headers, gateway, err := u.dialInfo(sess)
	if err != nil {
		return ChatResult{}, err
	}

	dialer := *websocket.DefaultDialer
	handshakeTimeout := wsDialHandshakeTimeout
	if handshakeTimeout <= 0 {
		handshakeTimeout = 12 * time.Second
	}
	dialer.HandshakeTimeout = handshakeTimeout
	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		detail := ""
		status := 0
		var retryAfter time.Duration
		if resp != nil {
			status = resp.StatusCode
			detail = fmt.Sprintf(" HTTP %d", resp.StatusCode)
			retryAfter = parseRetryAfterHeader(resp.Header.Get("Retry-After"), time.Now())
		}
		return ChatResult{}, &UpstreamError{
			StatusCode: status,
			RetryAfter: retryAfter,
			Err:        fmt.Errorf("upstream WebSocket dial failed%s: %w", detail, err),
		}
	}
	defer conn.Close()

	writeMu := &sync.Mutex{}
	encode := func(v any) ([]byte, error) {
		body, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		if gateway {
			frame := gatewayEncode(string(body))
			if frame == nil {
				return nil, fmt.Errorf("encode gateway frame")
			}
			return frame, nil
		}
		// AIDP (enterprise and social) uses the block-style binary envelope:
		// {"eventType":"rawInputEvent","payload":{"payload":base64(json)}}.
		return aidpEncode(string(body)), nil
	}
	messageType := websocket.BinaryMessage
	send := func(v any) error {
		frame, err := encode(v)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(messageType, frame)
	}

	// Keep the socket alive while the agent is thinking or generating long replies.
	stopPing := make(chan struct{})
	defer close(stopPing)
	go func() {
		ticker := time.NewTicker(wsKeepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				// 发送应用层心跳帧，重置 AWS Quick 服务端 95 秒无数据帧超时计时。
				if err := send(map[string]any{"type": "ping", "payload": map[string]any{}}); err != nil {
					return
				}
				writeMu.Lock()
				_ = conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(5*time.Second))
				writeMu.Unlock()
			}
		}
	}()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	// Handshake. The AIDP and Gateway paths use different session_connect shapes.
	if gateway {
		_ = send(map[string]any{"type": "session_connect", "session_id": sess.OutpostID})
	} else {
		_ = send(map[string]any{
			"type": "session_connect",
			"payload": map[string]any{
				"actor_id":               sess.ActorARN,
				"surfaceType":            sess.Surface,
				"outpostId":              sess.OutpostID,
				"appVersion":             u.cfg.AppVersion,
				"external_agent_sign_in": 1,
				"recents_promotion":      1,
			},
		})
	}
	_ = send(map[string]any{"type": "federate_status", "payload": map[string]any{}})
	if in.CreateSession {
		_ = send(map[string]any{
			"type":    "session_create",
			"payload": map[string]any{"session_id": threadID, "title": title},
		})
	}

	guidelines, toolProtocol, legacySystem := resolveTurnSections(in)
	basePrompt := in.Prompt

	payload := map[string]any{
		"text":               renderTurnTextForMode(in.WorkMode, guidelines, toolProtocol, legacySystem, basePrompt),
		"send_origin":        "programmatic",
		"had_physical_input": false,
		"surfaceType":        sess.Surface,
		"outpostId":          sess.OutpostID,
		"appVersion":         u.cfg.AppVersion,
		"traceId":            traceID,
		"message_id":         messageID,
		"thread_id":          threadID,
		"agent_mode":         in.Model,
		"user_timezone":      u.cfg.Timezone,
		"user_locale":        u.cfg.Locale,
		"chat_agent_id":      nil,
		"auto_routing":       false,
		"web_search_enabled": false,
		"pre_approved_tools": []any{},
		"preload_skills":     []any{},
		"panel_edits":        []any{},
		"custom_prompt":      nil,
	}
	if id := strings.TrimSpace(in.ChatAgentID); id != "" {
		payload["chat_agent_id"] = id
	}
	// The service ignores a per-turn "system_prompt" field and treats
	// "custom_prompt" as low-trust saved preferences, so caller instructions are
	// carried at the head of the user_message text instead (see relayPrompt).
	// 优先使用本次请求解析出的思考强度，缺失时回退服务端默认配置。
	effort := strings.TrimSpace(in.ThinkingEffort)
	if effort == "" {
		effort = strings.TrimSpace(u.cfg.ThinkingEffort)
	}
	if effort != "" {
		payload["thinking_effort"] = effort
	}

	// 单条 user_message 帧上限为 64 KB：存在必需附件时必须全部上传成功，
	// 严禁在不支持附件或上传失败时仍派发残缺请求给模型；随后按消息边界保护当前轮意图：
	// 小帧维持零附件，超限优先仅外置先前历史并完整内联当前轮；当轮单独超限且支持附件则上传全文并明确预览；
	// 上传失败或无上传能力且无法无损装入时直接返回错误，绝不发出截半请求。
	if len(in.Images) > 0 && (isGatewayProvider(sess.Provider) || strings.TrimSpace(sess.TenantURL) == "") {
		return ChatResult{}, &UpstreamError{
			StatusCode: http.StatusBadGateway,
			Code:       "AttachmentTransportUnsupported",
			Err:        fmt.Errorf("upstream account does not support attachment uploads (%d required)", len(in.Images)),
		}
	}
	hasUploadedAttachments := len(in.Images) > 0
	var attachments []map[string]any
	var attachFn func(name string, data []byte) (attachmentRef, error)
	if !isGatewayProvider(sess.Provider) && strings.TrimSpace(sess.TenantURL) != "" {
		if len(in.Images) > 0 {
			imgMetas, uploadErr := uploadImageAttachments(ctx, sess, threadID, in.Images)
			if uploadErr != nil {
				return ChatResult{}, uploadErr
			}
			if len(imgMetas) > 0 {
				attachments = append(attachments, imgMetas...)
			}
			if len(attachments) > 0 {
				payload["attachments"] = attachments
			}
		}
		attachFn = func(name string, data []byte) (attachmentRef, error) {
			att, attErr := uploadPromptAttachment(ctx, sess, threadID, name, data)
			if attErr != nil {
				log.Printf("user_message attachment unavailable thread=%s name=%s bytes=%d: %v", threadID, name, len(data), attErr)
				return attachmentRef{}, attErr
			}
			if att.Path == "" {
				return attachmentRef{}, attachErr("attachment upload returned no workspace path for %s", name)
			}
			hasUploadedAttachments = true
			log.Printf("user_message attachment thread=%s bytes=%d name=%s id=%s path=%s",
				threadID, att.Size, att.Name, att.ID, att.Path)
			return att, nil
		}
	}

	preserveEnv := u.cfg == nil || u.cfg.Guardrails.PreserveEnvSummaryEnabled()
	if err := prepareFrameWithinBudgetDetailed(
		payload,
		messageID,
		guidelines,
		toolProtocol,
		legacySystem,
		basePrompt,
		in.HistoryPrompt,
		in.CurrentTurn,
		in.LatestUserText,
		in.CurrentTurnIsTool,
		attachments,
		nil,
		preserveEnv,
		in.WorkMode,
		attachFn,
	); err != nil {
		return ChatResult{}, err
	}
	if n, sizeErr := userMessageBytes(payload); sizeErr != nil {
		return ChatResult{}, sizeErr
	} else if n > maxUserMessageBytes {
		return ChatResult{}, fmt.Errorf("%w: %d bytes after frame preparation", ErrUserMessageTooLarge, n)
	}
	if err := send(map[string]any{"type": "user_message", "payload": payload}); err != nil {
		return ChatResult{}, fmt.Errorf("send user_message: %w", err)
	}
	if in.OnDispatched != nil {
		in.OnDispatched()
	}

	result, err := u.readEventsWithGuard(ctx, conn, gateway, threadID, effort, in.ClientToolsActive, hasUploadedAttachments, onEvent)
	result.ThreadID = threadID
	if in.DeleteSession {
		_ = send(map[string]any{"type": "session_delete", "payload": map[string]any{"session_id": threadID}})
	}
	if err != nil {
		return result, err
	}
	return result, nil
}

func (u *Upstream) dialInfo(sess *Session) (wsURL string, headers http.Header, gateway bool, err error) {
	jwt := sess.jwt()
	if jwt == "" {
		return "", nil, false, fmt.Errorf("missing jwt")
	}
	gateway = isGatewayProvider(sess.Provider)

	host := strings.TrimSpace(sess.WSHost)
	path := strings.TrimSpace(sess.WSPath)
	if path == "" {
		if gateway {
			path = defaultGatewayWSPath
		} else {
			path = defaultWSPath
		}
	}
	switch {
	case strings.HasPrefix(host, "wss://") || strings.HasPrefix(host, "ws://"):
		host = strings.TrimRight(host, "/")
	case strings.HasPrefix(host, "https://"):
		host = "wss://" + strings.TrimPrefix(host, "https://")
	default:
		host = "wss://" + strings.TrimRight(host, "/")
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		wsURL = path
	} else {
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		wsURL = host + path
	}

	parsed, err := url.Parse(wsURL)
	if err != nil {
		return "", nil, false, fmt.Errorf("invalid ws_url: %w", err)
	}
	if !gateway {
		q := parsed.Query()
		if sess.Surface != "" {
			q.Set("surfaceType", sess.Surface)
		}
		if sess.OutpostID != "" {
			q.Set("outpostId", sess.OutpostID)
		}
		if sess.WorkingSessionID != "" {
			q.Set("workingSessionId", sess.WorkingSessionID)
		}
		// Social AIDP transports accept the bearer in the query; enterprise
		// relies on the Authorization header injected below.
		if strings.EqualFold(sess.Provider, "social") {
			q.Set("token", jwt)
		}
		parsed.RawQuery = q.Encode()
	}

	headers = http.Header{}
	headers.Set("Authorization", "Bearer "+jwt)
	headers.Set("Origin", "file://")
	headers.Set("X-Surface-Type", sess.Surface)
	headers.Set("X-Outpost-Id", sess.OutpostID)
	headers.Set("X-Agent-Service-Type", "UNIFIED_QUICK")
	if sess.WorkingSessionID != "" {
		headers.Set("X-Working-Session-Id", sess.WorkingSessionID)
	}
	return parsed.String(), headers, gateway, nil
}

func (u *Upstream) readEvents(ctx context.Context, conn *websocket.Conn, gateway bool, threadID, thinkingEffort string, onEvent EventFunc) (ChatResult, error) {
	return u.readEventsWithGuard(ctx, conn, gateway, threadID, thinkingEffort, false, false, onEvent)
}

func (u *Upstream) readEventsWithGuard(ctx context.Context, conn *websocket.Conn, gateway bool, threadID, thinkingEffort string, clientToolsActive, hasUploadedAttachments bool, onEvent EventFunc) (ChatResult, error) {
	isMaxThinking := strings.EqualFold(strings.TrimSpace(thinkingEffort), "max")
	idleReadLimit := wsReadIdleTimeout
	if isMaxThinking && wsReadIdleTimeoutMaxThinking > idleReadLimit {
		idleReadLimit = wsReadIdleTimeoutMaxThinking
	}
	completeGrace := wsCompleteGracePeriod
	if isMaxThinking && wsCompleteGraceMaxThinking > completeGrace {
		completeGrace = wsCompleteGraceMaxThinking
	}
	var result ChatResult
	var text strings.Builder
	var thinking strings.Builder
	sawMessageComplete := false
	turnComplete := false
	var completeDeadline time.Time
	eventCounts := make(map[string]int)

	toolCalls := make(map[string]*ToolCallState)
	toolOrder := make([]string, 0, 2)
	toolSent := make(map[string]bool)
	type creditSnapshot struct {
		unit   string
		amount *float64
	}
	creditSnapshots := make(map[string]creditSnapshot)
	currentMessageID := "turn"
	rawArgs := make(map[string]string)
	argsPartial := make(map[string]bool)

	getTool := func(id, name string) *ToolCallState {
		id = strings.TrimSpace(id)
		if id == "" {
			id = "call_" + strings.ReplaceAll(uuid(), "-", "")
		}
		tc := toolCalls[id]
		if tc == nil {
			tc = &ToolCallState{ID: id}
			toolCalls[id] = tc
			toolOrder = append(toolOrder, id)
		}
		if n := strings.TrimSpace(name); n != "" {
			tc.Name = n
		}
		return tc
	}
	setToolArgs := func(tc *ToolCallState, raw json.RawMessage) {
		if tc == nil || len(raw) == 0 {
			return
		}
		rawText := rawArgumentText(raw)
		if !hasSubstantiveToolArgs(rawText) {
			if hasSubstantiveToolArgs(rawArgs[tc.ID]) {
				return
			}
			if rawArgs[tc.ID] == "" {
				rawArgs[tc.ID] = rawText
			}
			tc.Arguments = normalizeArguments(raw)
			return
		}
		rawArgs[tc.ID] = rawText
		argsPartial[tc.ID] = false
		tc.Arguments = normalizeArguments(raw)
	}
	appendToolArgs := func(tc *ToolCallState, delta string) {
		if tc == nil || delta == "" {
			return
		}
		// tool_progress carries an input fragment. Once a tool has been
		// finalized by tool_arguments/tool_call, later progress deltas are
		// ignored instead of corrupting the JSON.
		if !argsPartial[tc.ID] && strings.TrimSpace(tc.Arguments) != "{}" && strings.TrimSpace(tc.Arguments) != "" {
			return
		}
		rawArgs[tc.ID] += delta
		argsPartial[tc.ID] = true
		tc.Arguments = normalizeArguments(json.RawMessage(rawArgs[tc.ID]))
	}
	emitTool := func(tc *ToolCallState) error {
		if tc == nil || onEvent == nil || toolSent[tc.ID] {
			return nil
		}
		toolSent[tc.ID] = true
		copy := *tc
		return onEvent(StreamEvent{Type: "tool_call", ToolCall: &copy})
	}

	finish := func() (ChatResult, error) {
		result.Text = text.String()
		result.Thinking = thinking.String()
		creditTotals := make(map[string]float64)
		for _, credit := range creditSnapshots {
			if credit.amount != nil {
				creditTotals[credit.unit] += *credit.amount
			}
		}
		units := make([]string, 0, len(creditTotals))
		for unit := range creditTotals {
			units = append(units, unit)
		}
		sort.Strings(units)
		result.Usage.Credits = result.Usage.Credits[:0]
		for _, unit := range units {
			result.Usage.Credits = append(result.Usage.Credits, store.CreditUsage{Unit: unit, Amount: creditTotals[unit]})
		}
		result.ToolCalls = result.ToolCalls[:0]
		for _, id := range toolOrder {
			tc := toolCalls[id]
			if tc == nil {
				continue
			}
			if raw := strings.TrimSpace(rawArgs[id]); raw != "" {
				tc.Arguments = normalizeArguments(json.RawMessage(raw))
			}
			if strings.TrimSpace(tc.Name) == "" {
				tc.Name = "unknown"
			}
			if strings.TrimSpace(tc.Arguments) == "" {
				tc.Arguments = "{}"
			}
			result.ToolCalls = append(result.ToolCalls, *tc)
			if err := emitTool(tc); err != nil {
				return result, err
			}
		}
		if len(eventCounts) > 0 {
			log.Printf("upstream events thread=%s counts=%v thinking_chars=%d text_chars=%d tool_calls=%d",
				threadID, eventCounts, thinking.Len(), text.Len(), len(toolOrder))
		}
		return result, nil
	}

	terminal := map[string]bool{
		"turn_completed": true,
		"turn_ended":     true,
		"stream_ended":   true,
		"chat_complete":  true,
	}
	var pendingFrames []string
	checkedTools := make(map[string]string)
	countedReadIDs := make(map[string]bool)
	countedScriptIDs := make(map[string]bool)
	allowedReadCount := 0
	scriptReadCount := 0
	var nativeToolActiveSince time.Time
	var nativeToolTotalElapsed time.Duration
	lastNativeToolName := ""

	pauseNativeToolTimer := func() {
		if !nativeToolActiveSince.IsZero() {
			nativeToolTotalElapsed += time.Since(nativeToolActiveSince)
			nativeToolActiveSince = time.Time{}
		}
	}

	checkNativeTool := func(tc *ToolCallState, argsFinal bool) error {
		if !clientToolsActive || tc == nil {
			return nil
		}
		if nativeToolActiveSince.IsZero() {
			nativeToolActiveSince = time.Now()
		}
		if strings.TrimSpace(tc.Name) != "" {
			lastNativeToolName = tc.Name
		}
		auditArgs := tc.Arguments
		if raw := rawArgs[tc.ID]; len(raw) > len(auditArgs) || strings.TrimSpace(auditArgs) == "{}" {
			if strings.TrimSpace(raw) != "" {
				auditArgs = raw
			}
		}
		checkKey := fmt.Sprintf("%s\x00%s\x00%t", tc.Name, auditArgs, argsFinal)
		if prevKey, ok := checkedTools[tc.ID]; ok && prevKey == checkKey {
			return nil
		}
		priorReadCount := allowedReadCount
		if countedReadIDs[tc.ID] && priorReadCount > 0 {
			priorReadCount--
		}
		priorScriptCount := scriptReadCount
		if countedScriptIDs[tc.ID] && priorScriptCount > 0 {
			priorScriptCount--
		}
		blocked, reason, isScriptRead := classifyRemoteSandboxTool(tc.Name, auditArgs, argsFinal, hasUploadedAttachments, priorReadCount, priorScriptCount)
		if blocked {
			argPreview := strings.TrimSpace(auditArgs)
			if len(argPreview) > 120 {
				n := 120
				for n > 0 && !utf8.RuneStart(argPreview[n]) {
					n--
				}
				argPreview = argPreview[:n] + "..."
			}
			log.Printf("upstream blocked remote sandbox tool thread=%s tool=%q reason=%s args=%q", threadID, tc.Name, reason, argPreview)
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "remote sandbox tool blocked"), time.Now().Add(time.Second))
			return &RemoteToolBlockedError{ToolName: tc.Name, Reason: reason}
		}
		if strings.TrimSpace(tc.Name) != "" {
			checkedTools[tc.ID] = checkKey
			if isScriptRead {
				if !countedScriptIDs[tc.ID] {
					countedScriptIDs[tc.ID] = true
					scriptReadCount++
				}
			} else if !countedReadIDs[tc.ID] {
				countedReadIDs[tc.ID] = true
				allowedReadCount++
			}
		}
		return nil
	}

	nativeTimeoutLimit := wsNativeToolTimeout
	if nativeTimeoutLimit <= 0 {
		nativeTimeoutLimit = 45 * time.Second
	}
	nativeTotalLimit := wsNativeToolTotalTimeout
	if nativeTotalLimit <= 0 {
		nativeTotalLimit = 90 * time.Second
	}

	for {
		select {
		case <-ctx.Done():
			res, _ := finish()
			return res, ctx.Err()
		default:
		}
		readWait := idleReadLimit
		if turnComplete && !completeDeadline.IsZero() {
			if remaining := time.Until(completeDeadline); remaining > 0 {
				readWait = remaining
			} else {
				return finish()
			}
		}
		if clientToolsActive && !nativeToolActiveSince.IsZero() {
			elapsed := time.Since(nativeToolActiveSince)
			totalElapsed := nativeToolTotalElapsed + elapsed
			if elapsed >= nativeTimeoutLimit || totalElapsed >= nativeTotalLimit {
				log.Printf("upstream blocked stalled remote tool thread=%s tool=%q elapsed=%s total=%s", threadID, lastNativeToolName, elapsed, totalElapsed)
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "remote tool timeout"), time.Now().Add(time.Second))
				res, _ := finish()
				return res, &RemoteToolBlockedError{ToolName: lastNativeToolName, Reason: "remote_tool_timeout"}
			}
			if rem := nativeTimeoutLimit - elapsed; rem < readWait {
				readWait = rem
			}
			if totalRem := nativeTotalLimit - totalElapsed; totalRem < readWait {
				readWait = totalRem
			}
		}
		if len(pendingFrames) == 0 {
			_ = conn.SetReadDeadline(time.Now().Add(readWait))
			msgType, data, err := conn.ReadMessage()
			if err != nil {
				res, emitErr := finish()
				if emitErr != nil {
					return res, emitErr
				}
				if turnComplete {
					return res, nil
				}
				if ctxErr := ctx.Err(); ctxErr != nil {
					return res, ctxErr
				}
				if clientToolsActive && !nativeToolActiveSince.IsZero() {
					elapsed := time.Since(nativeToolActiveSince)
					if elapsed >= nativeTimeoutLimit || nativeToolTotalElapsed+elapsed >= nativeTotalLimit {
						log.Printf("upstream blocked stalled remote tool on read timeout thread=%s tool=%q", threadID, lastNativeToolName)
						return res, &RemoteToolBlockedError{ToolName: lastNativeToolName, Reason: "remote_tool_timeout"}
					}
				}
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseNoStatusReceived) {
					return res, nil
				}
				return res, fmt.Errorf("upstream WebSocket read: %w", err)
			}
			if msgType == websocket.TextMessage {
				pendingFrames = append(pendingFrames, string(data))
			} else {
				var frames []string
				if gateway {
					frames, err = gatewayDecode(data)
				} else {
					frames, err = aidpDecode(data)
				}
				if err != nil {
					log.Printf("upstream frame decode: %v", err)
					if len(frames) == 0 {
						res, emitErr := finish()
						if emitErr != nil {
							return res, emitErr
						}
						return res, err
					}
				}
				pendingFrames = append(pendingFrames, frames...)
			}
		}
		if len(pendingFrames) == 0 {
			continue
		}
		jsonStr := pendingFrames[0]
		pendingFrames = pendingFrames[1:]

		var env struct {
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
			MessageID string          `json:"message_id"`
			Message   string          `json:"message"`
			Code      string          `json:"code"`
			Error     string          `json:"error"`
			ErrorCode string          `json:"error_code"`
		}
		if err := json.Unmarshal([]byte(jsonStr), &env); err != nil {
			continue
		}
		eventCounts[env.Type]++
		switch env.Type {
		case "message_start":
			pauseNativeToolTimer()
			turnComplete = false
			completeDeadline = time.Time{}
			// 读附件或工具回调后的新消息段与前序正文之间插入空行，避免两段文字紧贴粘连。
			if text.Len() > 0 && !strings.HasSuffix(text.String(), "\n") {
				text.WriteString("\n\n")
				if onEvent != nil {
					if err := onEvent(StreamEvent{Type: "text", Text: "\n\n"}); err != nil {
						res, _ := finish()
						return res, err
					}
				}
			}
			var payload struct {
				MessageID string `json:"message_id"`
				ID        string `json:"id"`
			}
			_ = json.Unmarshal(env.Payload, &payload)
			if env.MessageID != "" {
				currentMessageID = env.MessageID
			} else if payload.MessageID != "" {
				currentMessageID = payload.MessageID
			} else if payload.ID != "" {
				currentMessageID = payload.ID
			}
		case "acp_metadata":
			var payload struct {
				CostValue *float64 `json:"cost_value"`
				CostUnit  *string  `json:"cost_unit_plural"`
				MessageID string   `json:"message_id"`
			}
			if err := json.Unmarshal(env.Payload, &payload); err != nil {
				continue
			}
			messageID := currentMessageID
			if env.MessageID != "" {
				messageID = env.MessageID
			} else if payload.MessageID != "" {
				messageID = payload.MessageID
			}
			credit := creditSnapshots[messageID]
			if payload.CostUnit != nil {
				unit := strings.TrimSpace(*payload.CostUnit)
				if len(unit) > 128 || strings.ContainsAny(unit, "\r\n\t\x00") {
					continue
				}
				credit.unit = unit
			}
			if credit.unit == "" {
				credit.unit = "credits"
			}
			if payload.CostValue != nil {
				if *payload.CostValue < 0 || math.IsNaN(*payload.CostValue) || math.IsInf(*payload.CostValue, 0) {
					continue
				}
				credit.amount = payload.CostValue
			}
			creditSnapshots[messageID] = credit
		case "thinking_delta":
			delta := thinkingDeltaText(env.Payload)
			if delta == "" {
				continue
			}
			pauseNativeToolTimer()
			turnComplete = false
			completeDeadline = time.Time{}
			thinking.WriteString(delta)
			if onEvent != nil {
				if err := onEvent(StreamEvent{Type: "thinking", Text: delta}); err != nil {
					res, _ := finish()
					return res, err
				}
			}
		case "thinking_complete":
			if thinking.Len() == 0 {
				full := thinkingDeltaText(env.Payload)
				if full != "" {
					thinking.WriteString(full)
					if onEvent != nil {
						if err := onEvent(StreamEvent{Type: "thinking", Text: full}); err != nil {
							res, _ := finish()
							return res, err
						}
					}
				}
			}
		case "text_delta":
			delta := deltaText(env.Payload)
			if delta == "" {
				continue
			}
			pauseNativeToolTimer()
			turnComplete = false
			completeDeadline = time.Time{}
			text.WriteString(delta)
			if onEvent != nil {
				if err := onEvent(StreamEvent{Type: "text", Text: delta}); err != nil {
					res, _ := finish()
					return res, err
				}
			}
		case "text_complete":
			if text.Len() == 0 {
				var p struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(env.Payload, &p)
				if p.Text != "" {
					text.WriteString(p.Text)
					if onEvent != nil {
						if err := onEvent(StreamEvent{Type: "text", Text: p.Text}); err != nil {
							res, _ := finish()
							return res, err
						}
					}
				}
			}
			sawMessageComplete = true
		case "turn_usage":
			var p struct {
				InputTokens      int      `json:"input_tokens"`
				OutputTokens     int      `json:"output_tokens"`
				TotalTokens      int      `json:"total_tokens"`
				CacheReadTokens  int      `json:"cache_read_tokens"`
				CacheWriteTokens int      `json:"cache_write_tokens"`
				CostUSD          *float64 `json:"cost_usd"`
				ResolvedModelID  string   `json:"resolved_model_id"`
				ServedModelID    string   `json:"served_model_id"`
			}
			if err := json.Unmarshal(env.Payload, &p); err == nil {
				// 优先取 served_model_id，缺失时回退 resolved_model_id
				resolvedModel := strings.TrimSpace(p.ServedModelID)
				if resolvedModel == "" {
					resolvedModel = strings.TrimSpace(p.ResolvedModelID)
				}
				result.Usage.add(Usage{
					PromptTokens:     p.InputTokens,
					CompletionTokens: p.OutputTokens,
					TotalTokens:      p.TotalTokens,
					CacheReadTokens:  p.CacheReadTokens,
					CacheWriteTokens: p.CacheWriteTokens,
					CostUSD:          p.CostUSD,
					ResolvedModelID:  resolvedModel,
				})
			}
		case "tool_start", "tool_arguments", "tool_arguments_edited":
			if env.Type == "tool_start" {
				turnComplete = false
				completeDeadline = time.Time{}
			}
			var p struct {
				ToolName   string          `json:"tool_name"`
				ToolID     string          `json:"tool_id"`
				ToolCallID string          `json:"tool_call_id"`
				Arguments  json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			id := p.ToolID
			if id == "" {
				id = p.ToolCallID
			}
			tc := getTool(id, p.ToolName)
			setToolArgs(tc, p.Arguments)
			argsFinal := env.Type != "tool_start" || hasSubstantiveToolArgs(rawArgs[tc.ID])
			if bErr := checkNativeTool(tc, argsFinal); bErr != nil {
				res, _ := finish()
				return res, bErr
			}
			if err := emitTool(tc); err != nil {
				res, _ := finish()
				return res, err
			}
		case "tool_progress":
			var p struct {
				ToolID     string `json:"tool_id"`
				ToolCallID string `json:"tool_call_id"`
				ToolName   string `json:"tool_name"`
				InputDelta string `json:"input_delta"`
				Progress   struct {
					InputDelta string `json:"input_delta"`
				} `json:"progress"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			id := p.ToolID
			if id == "" {
				id = p.ToolCallID
			}
			tc := getTool(id, p.ToolName)
			delta := p.InputDelta
			if delta == "" {
				delta = p.Progress.InputDelta
			}
			appendToolArgs(tc, delta)
			if bErr := checkNativeTool(tc, false); bErr != nil {
				res, _ := finish()
				return res, bErr
			}
			if err := emitTool(tc); err != nil {
				res, _ := finish()
				return res, err
			}
		case "tool_call", "tool_call_update", "tool_complete":
			var p struct {
				ToolCallID      string          `json:"toolCallId"`
				ToolCallIDSnake string          `json:"tool_call_id"`
				ToolID          string          `json:"tool_id"`
				ToolName        string          `json:"tool_name"`
				Title           string          `json:"title"`
				RawInput        json.RawMessage `json:"rawInput"`
				RawInputSnake   json.RawMessage `json:"raw_input"`
				Arguments       json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			id := p.ToolID
			if id == "" {
				id = p.ToolCallID
			}
			if id == "" {
				id = p.ToolCallIDSnake
			}
			name := p.ToolName
			if name == "" {
				name = p.Title
			}
			tc := getTool(id, name)
			args := p.RawInput
			if len(args) == 0 {
				args = p.RawInputSnake
			}
			if len(args) == 0 {
				args = p.Arguments
			}
			setToolArgs(tc, args)
			if bErr := checkNativeTool(tc, true); bErr != nil {
				res, _ := finish()
				return res, bErr
			}
			if err := emitTool(tc); err != nil {
				res, _ := finish()
				return res, err
			}
		case "message_complete":
			sawMessageComplete = true
			var p struct {
				ContinuationExpected bool `json:"continuation_expected"`
				ToolCalls            []struct {
					ID         string          `json:"id"`
					ToolCallID string          `json:"tool_call_id"`
					Name       string          `json:"name"`
					ToolName   string          `json:"tool_name"`
					Arguments  json.RawMessage `json:"arguments"`
					Function   struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			for _, item := range p.ToolCalls {
				id := item.ID
				if id == "" {
					id = item.ToolCallID
				}
				name := item.Name
				if name == "" {
					name = item.ToolName
				}
				if name == "" {
					name = item.Function.Name
				}
				tc := getTool(id, name)
				args := item.Arguments
				if len(args) == 0 {
					args = item.Function.Arguments
				}
				setToolArgs(tc, args)
				if bErr := checkNativeTool(tc, true); bErr != nil {
					res, _ := finish()
					return res, bErr
				}
				if err := emitTool(tc); err != nil {
					res, _ := finish()
					return res, err
				}
			}
			if p.ContinuationExpected {
				turnComplete = false
				completeDeadline = time.Time{}
			} else {
				turnComplete = true
				completeDeadline = time.Now().Add(completeGrace)
			}
		case "tool_result":
			pauseNativeToolTimer()
			// 内置工具执行返回（例如读取大附件后）若紧接着输出正文，确保前后以空行分隔，避免旁白与回答粘连。
			if text.Len() > 0 && !strings.HasSuffix(text.String(), "\n") {
				text.WriteString("\n\n")
				if onEvent != nil {
					if err := onEvent(StreamEvent{Type: "text", Text: "\n\n"}); err != nil {
						res, _ := finish()
						return res, err
					}
				}
			}
		case "guardrail_blocked":
			log.Printf("upstream guardrail_blocked thread=%s payload=%s", threadID, snippet(env.Payload))
		case "turn_aborted":
			res, _ := finish()
			return res, &UpstreamError{Code: "turn_aborted", Err: errors.New("upstream turn was aborted")}
		case "error", "session_error", "stream_error", "turn_failed":
			res, _ := finish()
			message := env.Message
			if message == "" {
				message = env.Error
			}
			code := env.Code
			if code == "" {
				code = env.ErrorCode
			}
			var detail struct {
				Code       string  `json:"code"`
				ErrorCode  string  `json:"error_code"`
				StatusCode int     `json:"status_code"`
				Status     int     `json:"status"`
				RetryAfter float64 `json:"retry_after"`
				RetryCamel float64 `json:"retryAfter"`
			}
			_ = json.Unmarshal(env.Payload, &detail)
			if code == "" {
				code = detail.Code
				if code == "" {
					code = detail.ErrorCode
				}
			}
			status := detail.StatusCode
			if status == 0 {
				status = detail.Status
			}
			retrySecs := detail.RetryAfter
			if retrySecs <= 0 {
				retrySecs = detail.RetryCamel
			}
			var retryAfter time.Duration
			if retrySecs > 0 {
				retryAfter = time.Duration(retrySecs * float64(time.Second))
			}
			log.Printf("upstream %s code=%q status=%d", env.Type, code, status)
			return res, &UpstreamError{StatusCode: status, Code: code, RetryAfter: retryAfter, Err: fmt.Errorf("upstream %s: %s", env.Type, errorText(env.Payload, message, code))}
		default:
			if terminal[env.Type] {
				return finish()
			}
			if env.Type == "ping" {
				// Best effort pong; ignore errors.
				_ = conn.WriteControl(websocket.PongMessage, []byte("pong"), time.Now().Add(3*time.Second))
			}
			if sawMessageComplete && env.Type == "session_list" {
				continue
			}
		}
	}
}

func deltaText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var p struct {
		Delta string `json:"delta"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	if p.Delta != "" {
		return p.Delta
	}
	return p.Text
}

// thinkingDeltaText 从上游 thinking_delta 或 thinking_complete 事件载荷中提取思考文本增量，
// 兼容 delta 字符串、嵌套对象 {text}、thinking 及 text 字段。
func thinkingDeltaText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var p struct {
		Delta    json.RawMessage `json:"delta"`
		Thinking string          `json:"thinking"`
		Text     string          `json:"text"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	if len(p.Delta) > 0 {
		var ds string
		if err := json.Unmarshal(p.Delta, &ds); err == nil && ds != "" {
			return ds
		}
		var nested struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(p.Delta, &nested); err == nil && nested.Text != "" {
			return nested.Text
		}
	}
	if p.Thinking != "" {
		return p.Thinking
	}
	return p.Text
}

func errorText(raw json.RawMessage, fallback, code string) string {
	if len(raw) > 0 {
		var v any
		if json.Unmarshal(raw, &v) == nil {
			if s := nestedErrorText(v); s != "" {
				return s
			}
			if compact, err := json.Marshal(v); err == nil && len(compact) > 0 {
				return truncateText(string(compact), 800)
			}
		}
		if s := strings.TrimSpace(string(raw)); s != "" {
			return truncateText(s, 800)
		}
	}
	for _, s := range []string{fallback, code} {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return "unknown upstream error"
}

func nestedErrorText(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case map[string]any:
		for _, key := range []string{"message", "error", "detail", "reason", "description", "title"} {
			if s, ok := x[key]; ok {
				if text := nestedErrorText(s); text != "" {
					return text
				}
			}
		}
	case []any:
		for _, item := range x {
			if text := nestedErrorText(item); text != "" {
				return text
			}
		}
	}
	return ""
}

func truncateText(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// resolveTurnSections 区分新式「规范/工具协议分离字段」与旧版单一 SystemPrompt 字段，保持向后兼容。
func resolveTurnSections(in ChatInput) (guidelines, toolProtocol, legacySystem string) {
	if in.Guidelines != "" || in.ToolProtocol != "" {
		return in.Guidelines, in.ToolProtocol, ""
	}
	return "", "", in.SystemPrompt
}

// renderTurnText 根据是否拆分工作区规范与工具协议来拼装本轮发送的完整用户文本。
func renderTurnText(guidelines, toolProtocol, legacySystem, prompt string) string {
	return renderTurnTextForMode(config.WorkModeGeneral, guidelines, toolProtocol, legacySystem, prompt)
}

func renderTurnTextForMode(workMode, guidelines, toolProtocol, legacySystem, prompt string) string {
	if guidelines != "" || toolProtocol != "" {
		return assembleRelayPromptForMode(workMode, guidelines, toolProtocol, prompt)
	}
	return relayPromptForMode(workMode, legacySystem, prompt)
}

// prepareFrameWithinBudget 在不超过上游 64 KB 帧限制的前提下尽量保留完整上下文，并严格优先保护 <client_tool_protocol>：
// 1) 优先压缩 <app-context> 中的冗长技能列表（常规 -> 纯索引），尽量零附件塞入单帧以防诱发云端沙箱工具；
// 2) 若仍超限且正文较长，再将对话正文外置为 prompt-<id8>.txt；
// 3) 仍超限则将完整原始工作区规范上传为 guidelines-<id8>.txt，主帧内仅保留短指引 + 完整工具协议（document 模式下跳过外置，直接精简内联以防风控）。
func prepareFrameWithinBudget(
	payload map[string]any,
	messageID string,
	rawGuidelines, toolProtocol, legacySystem, prompt, latestUser string,
	attachments []map[string]any,
	failedImgs []string,
	attachFn func(name string, data []byte) (attachmentRef, error),
) {
	prepareFrameWithinBudgetWithMode(payload, messageID, rawGuidelines, toolProtocol, legacySystem, prompt, latestUser, attachments, failedImgs, true, config.WorkModeGeneral, attachFn)
}

func prepareFrameWithinBudgetOpts(
	payload map[string]any,
	messageID string,
	rawGuidelines, toolProtocol, legacySystem, prompt, latestUser string,
	attachments []map[string]any,
	failedImgs []string,
	preserveEnvSummary bool,
	attachFn func(name string, data []byte) (attachmentRef, error),
) {
	prepareFrameWithinBudgetWithMode(payload, messageID, rawGuidelines, toolProtocol, legacySystem, prompt, latestUser, attachments, failedImgs, preserveEnvSummary, config.WorkModeGeneral, attachFn)
}

func prepareFrameWithinBudgetWithMode(
	payload map[string]any,
	messageID string,
	rawGuidelines, toolProtocol, legacySystem, prompt, latestUser string,
	attachments []map[string]any,
	failedImgs []string,
	preserveEnvSummary bool,
	workMode string,
	attachFn func(name string, data []byte) (attachmentRef, error),
) {
	_ = prepareFrameWithinBudgetDetailed(
		payload, messageID, rawGuidelines, toolProtocol, legacySystem,
		prompt, "", "", latestUser, false,
		attachments, failedImgs, preserveEnvSummary, workMode, attachFn,
	)
}

func cloneAttachmentSlice(src []map[string]any) []map[string]any {
	if len(src) == 0 {
		return nil
	}
	out := make([]map[string]any, len(src))
	copy(out, src)
	return out
}

// placeholderPromptAttachment 生成用于 dry-run 完整帧体积测算的保守占位附件描述。
func placeholderPromptAttachment(name string) map[string]any {
	return fileAttachmentMeta(attachmentRef{
		Name: name,
		Size: 9999999,
		Path: "/data/s3files/users/000000000000/sessions/00000000-0000-0000-0000-000000000000/workspace/attached_files/" + name,
		ID:   "00000000-0000-0000-0000-000000000000",
	}, attachmentMimeType)
}

// prepareFrameWithinBudgetDetailed 在不超过上游 64 KB 帧限制的前提下，严格按消息边界保护当前轮意图：
// 1) 先尝试零附件压缩工作区技能列表；
// 2) 若有可外置的先前历史且完整当前轮能内联放入 64 KB 帧，仅将先前历史上传为 prompt-*.txt，当前轮逐字完整留在正文；
// 3) 若当轮自身单独超限，则通过一次上传完整原文并明确正文为截断预览；
// 4) 上传失败或无上传能力且无法无损装入时明确报错返回，绝不发出丢失本轮尾部的残缺请求。
func prepareFrameWithinBudgetDetailed(
	payload map[string]any,
	messageID string,
	rawGuidelines, toolProtocol, legacySystem, prompt, historyPrompt, currentTurn, latestUser string,
	currentIsTool bool,
	attachments []map[string]any,
	failedImgs []string,
	preserveEnvSummary bool,
	workMode string,
	attachFn func(name string, data []byte) (attachmentRef, error),
) error {
	buildProbePayload := func(atts []map[string]any, g, p string) map[string]any {
		probe := make(map[string]any, len(payload)+1)
		for k, v := range payload {
			probe[k] = v
		}
		probe["text"] = renderTurnTextForMode(workMode, g, toolProtocol, legacySystem, p)
		if len(atts) > 0 {
			probe["attachments"] = atts
		} else {
			delete(probe, "attachments")
		}
		return probe
	}
	fitsWith := func(atts []map[string]any, g, p string) bool {
		n, err := userMessageBytes(buildProbePayload(atts, g, p))
		return err == nil && n <= maxUserMessageBytes
	}
	applyMain := func(atts []map[string]any, g, p string) {
		payload["text"] = renderTurnTextForMode(workMode, g, toolProtocol, legacySystem, p)
		if len(atts) > 0 {
			payload["attachments"] = atts
		} else {
			delete(payload, "attachments")
		}
	}

	curAtts := cloneAttachmentSlice(attachments)
	curGuidelines := rawGuidelines
	if fitsWith(curAtts, curGuidelines, prompt) {
		return nil
	}

	// 第一步（零附件优先）：先尝试常规技能压缩与二档纯索引压缩，若能塞入 63 KB 主帧则不上传任何文本附件。
	if curGuidelines != "" {
		if compacted := compactSkillsSection(curGuidelines); len(compacted) < len(curGuidelines) {
			curGuidelines = compacted
			applyMain(curAtts, curGuidelines, prompt)
			if fitsWith(curAtts, curGuidelines, prompt) {
				return nil
			}
		}
		if indexed := compactSkillsIndexOnly(rawGuidelines); len(indexed) < len(curGuidelines) {
			curGuidelines = indexed
			applyMain(curAtts, curGuidelines, prompt)
			if fitsWith(curAtts, curGuidelines, prompt) {
				return nil
			}
		}
	}

	// 无附件上传能力（如 gateway / 无 TenantURL）：仅允许缩减工作区规范来挽救工具协议与完整输入；
	// 仍放不下时明确返回 ErrUserMessageTooLarge，严禁截断本轮正文。
	if attachFn == nil {
		if curGuidelines != "" {
			if curText, ok := payload["text"].(string); ok {
				if bodyLen, bErr := userMessageBytes(payload); bErr == nil && bodyLen > maxUserMessageBytes {
					if trimmed, changed := trimWorkspaceGuidelinesOnly(curText, bodyLen-maxUserMessageBytes); changed {
						payload["text"] = trimmed
						if n, nErr := userMessageBytes(payload); nErr == nil && n <= maxUserMessageBytes {
							return nil
						}
					}
				}
			}
		}
		return fmt.Errorf("%w: prompt cannot fit under the 64 KB frame limit without attachment upload", ErrUserMessageTooLarge)
	}

	envSummary := ""
	if preserveEnvSummary && rawGuidelines != "" {
		envSummary = extractEnvSummary(rawGuidelines)
	}
	isDocMode := config.NormalizeWorkMode(workMode) == config.WorkModeDocument

	// shrinkGuidelinesOnDemand 在当前正文已确定的前提下，按模式对超限工作区规范做内联精简或真实外置上传。
	shrinkGuidelinesOnDemand := func(atts []map[string]any, curG, bodyText string, dryRun bool) (string, []map[string]any, bool, error) {
		if fitsWith(atts, curG, bodyText) {
			return curG, atts, true, nil
		}
		if rawGuidelines == "" {
			return curG, atts, false, nil
		}
		if isDocMode {
			g1 := compactGuidelinesInline(envSummary)
			if fitsWith(atts, g1, bodyText) {
				return g1, atts, true, nil
			}
			g2 := compactGuidelinesInline("")
			return g2, atts, fitsWith(atts, g2, bodyText), nil
		}
		var nextG string
		nextAtts := cloneAttachmentSlice(atts)
		if dryRun {
			gName := attachmentGuidelinesName(messageID)
			nextAtts = append(nextAtts, placeholderPromptAttachment(gName))
			nextG = attachmentGuidelinesNoteWithEnv(gName, envSummary)
			if fitsWith(nextAtts, nextG, bodyText) {
				return nextG, nextAtts, true, nil
			}
			if envSummary != "" {
				nextG = attachmentGuidelinesNote(gName)
				if fitsWith(nextAtts, nextG, bodyText) {
					return nextG, nextAtts, true, nil
				}
			}
			return nextG, nextAtts, false, nil
		}
		att, gErr := attachFn(attachmentGuidelinesName(messageID), []byte(rawGuidelines))
		if gErr != nil {
			return curG, atts, false, gErr
		}
		if att.Path == "" {
			return curG, atts, false, attachErr("attachment upload returned empty path for %s", att.Name)
		}
		nextAtts = append(nextAtts, fileAttachmentMeta(att, attachmentMimeType))
		nextG = attachmentGuidelinesNoteWithEnv(att.Name, envSummary)
		if fitsWith(nextAtts, nextG, bodyText) {
			return nextG, nextAtts, true, nil
		}
		if envSummary != "" {
			nextG = attachmentGuidelinesNote(att.Name)
			if fitsWith(nextAtts, nextG, bodyText) {
				return nextG, nextAtts, true, nil
			}
		}
		return nextG, nextAtts, false, nil
	}

	failedNote := ""
	if len(failedImgs) > 0 {
		failedNote = "\n\n[Note: failed to upload image attachment(s): " + strings.Join(failedImgs, "; ") + "]"
	}

	fitOversizedCurrentPreview := func(atts []map[string]any, curG, attName, previewSource string) error {
		nextG, nextAtts, ok, gErr := shrinkGuidelinesOnDemand(atts, curG, attachmentOversizedCurrentTurnPreview(attName, previewSource, currentIsTool, 0)+failedNote, false)
		if gErr != nil {
			return gErr
		}
		if !ok {
			return fmt.Errorf("%w: frame metadata and tool protocol exceed 64 KB even after attachment upload", ErrUserMessageTooLarge)
		}
		for _, previewCap := range []int{4096, 2048, 1200, 600, 200, 0} {
			candidate := attachmentOversizedCurrentTurnPreview(attName, previewSource, currentIsTool, previewCap) + failedNote
			if fitsWith(nextAtts, nextG, candidate) {
				applyMain(nextAtts, nextG, candidate)
				return nil
			}
		}
		return fmt.Errorf("%w: frame exceeds 64 KB even with zero-preview attachment note", ErrUserMessageTooLarge)
	}

	hasSplitHistory := strings.TrimSpace(historyPrompt) != "" && strings.TrimSpace(currentTurn) != ""
	if !hasSplitHistory {
		// 无先前历史（单轮超长对话 / 兼容老 12 参调用）：
		// 优先尝试仅通过压缩/外置工作区规范把完整正文逐字保在主帧内；确系正文自己超限时才上传 prompt 附件。
		if rawGuidelines != "" {
			if _, _, dryOK, _ := shrinkGuidelinesOnDemand(curAtts, curGuidelines, prompt, true); dryOK {
				nextG, nextAtts, ok, gErr := shrinkGuidelinesOnDemand(curAtts, curGuidelines, prompt, false)
				if gErr != nil {
					return gErr
				}
				if ok {
					applyMain(nextAtts, nextG, prompt)
					return nil
				}
			}
		}
		// 当轮自身单独超限：完整原文进附件，正文使用明确标注截断预览的安全说明。
		att, pErr := attachFn(attachmentPromptName(messageID), []byte(prompt))
		if pErr != nil {
			return pErr
		}
		if att.Path == "" {
			return attachErr("attachment upload returned empty path for %s", att.Name)
		}
		curAtts = append(curAtts, fileAttachmentMeta(att, attachmentMimeType))
		previewSource := strings.TrimSpace(currentTurn)
		if previewSource == "" {
			previewSource = strings.TrimSpace(latestUser)
		}
		if previewSource == "" {
			previewSource = prompt
		}
		return fitOversizedCurrentPreview(curAtts, curGuidelines, att.Name, previewSource)
	}

	// 有明确先前历史可外置：先用完整 JSON 序列化 dry-run 判断外置历史后当前轮能否逐字完整留在正文。
	probePromptName := attachmentPromptName(messageID)
	probeAtts := append(cloneAttachmentSlice(curAtts), placeholderPromptAttachment(probePromptName))
	probeBody := attachmentHistoryWithCurrentTurn(probePromptName, currentTurn, currentIsTool) + failedNote
	_, _, inlineFitsDry, _ := shrinkGuidelinesOnDemand(probeAtts, curGuidelines, probeBody, true)

	if inlineFitsDry {
		histAtt, hErr := attachFn(probePromptName, []byte(historyPrompt))
		if hErr != nil {
			return hErr
		}
		if histAtt.Path == "" {
			return attachErr("attachment upload returned empty path for %s", histAtt.Name)
		}
		curAtts = append(curAtts, fileAttachmentMeta(histAtt, attachmentMimeType))
		realBody := attachmentHistoryWithCurrentTurn(histAtt.Name, currentTurn, currentIsTool) + failedNote
		if fitsWith(curAtts, curGuidelines, realBody) {
			applyMain(curAtts, curGuidelines, realBody)
			return nil
		}
		nextG, nextAtts, ok, gErr := shrinkGuidelinesOnDemand(curAtts, curGuidelines, realBody, false)
		if gErr != nil {
			return gErr
		}
		if ok && fitsWith(nextAtts, nextG, realBody) {
			applyMain(nextAtts, nextG, realBody)
			return nil
		}
		return fmt.Errorf("%w: current turn exceeds 64 KB after real attachment metadata serialization", ErrUserMessageTooLarge)
	}

	// 当轮自身单独超限（历史外置且规范最小化后仍放不下完整当前轮）：
	// 一次上传包含历史与完整当前轮的全文以保全所有内容，正文仅发截断预览与读取附件提示。
	fullAtt, fErr := attachFn(probePromptName, []byte(prompt))
	if fErr != nil {
		return fErr
	}
	if fullAtt.Path == "" {
		return attachErr("attachment upload returned empty path for %s", fullAtt.Name)
	}
	curAtts = append(curAtts, fileAttachmentMeta(fullAtt, attachmentMimeType))
	return fitOversizedCurrentPreview(curAtts, curGuidelines, fullAtt.Name, currentTurn)
}

// fitUserMessage keeps the user_message payload under the upstream limit.
// Upstream checks the reassembled user_message JSON, so WebSocket
// fragmentation or multiple user_message frames cannot carry one oversized
// turn: extra frames would become separate turns. Trim within the single frame
// instead. When <client_tool_protocol> is present alongside <workspace_guidelines>,
// guidelines are trimmed first so local tool schemas survive intact.
func fitUserMessage(payload map[string]any) error {
	wrap := func() ([]byte, error) {
		return json.Marshal(map[string]any{"type": "user_message", "payload": payload})
	}
	body, err := wrap()
	if err != nil {
		return err
	}
	if len(body) <= maxUserMessageBytes {
		return nil
	}
	original := len(body)
	text, _ := payload["text"].(string)

	// 0. 兜底保护：若仍存在可裁的工作区规范且主帧含工具协议，优先只缩工作区规范内部而保全 <client_tool_protocol>。
	if strings.Contains(text, "<client_tool_protocol>") {
		if trimmed, ok := trimWorkspaceGuidelinesOnly(text, len(body)-maxUserMessageBytes); ok {
			payload["text"] = trimmed
			if body, err = wrap(); err != nil {
				return err
			}
			if len(body) <= maxUserMessageBytes {
				log.Printf("user_message guidelines trimmed %d -> %d bytes (limit %d)", original, len(body), maxUserMessageBytes)
				return nil
			}
			text = trimmed
		}
	}

	// 1. The prompt text is the normal overflow source; trim its middle first.
	if len(text) > 4096 {
		payload["text"] = shrinkToLimit(text, func(s string) ([]byte, error) {
			payload["text"] = s
			return wrap()
		}, textTrimMarker)
		if body, err = wrap(); err != nil {
			return err
		}
	}

	// 2. If metadata plus text still overflow, shrink text down to the marker.
	if len(body) > maxUserMessageBytes && text != "" {
		payload["text"] = shrinkToLimit(text, func(s string) ([]byte, error) {
			payload["text"] = s
			return wrap()
		}, textTrimMarker)
		if body, err = wrap(); err != nil {
			return err
		}
	}

	if len(body) > maxUserMessageBytes {
		return fmt.Errorf("%w: %d bytes after trimming the prompt", ErrUserMessageTooLarge, len(body))
	}
	log.Printf("user_message trimmed %d -> %d bytes (limit %d)", original, len(body), maxUserMessageBytes)
	return nil
}

// trimWorkspaceGuidelinesOnly 仅裁剪 <workspace_guidelines>...</workspace_guidelines> 内部文本以腾出 over 字节空间，
// 确保后方的 <client_tool_protocol> 与用户请求完全不受影响。
func trimWorkspaceGuidelinesOnly(text string, over int) (string, bool) {
	const openTag = "<workspace_guidelines>"
	const closeTag = "</workspace_guidelines>"
	start := strings.Index(text, openTag)
	if start < 0 {
		return text, false
	}
	innerStart := start + len(openTag)
	endRel := strings.Index(text[innerStart:], closeTag)
	if endRel <= 0 {
		return text, false
	}
	innerEnd := innerStart + endRel
	inner := text[innerStart:innerEnd]
	if len(inner) <= 512 {
		return text, false
	}
	const guideMarker = "\n\n[middle workspace guidelines trimmed to protect tool definitions]\n\n"
	target := len(inner) - over - len(guideMarker)
	if target < 256 {
		target = 256
	}
	if target >= len(inner) {
		return text, false
	}
	head := target / 3
	for head > 0 && !utf8.RuneStart(inner[head]) {
		head--
	}
	tailStart := len(inner) - (target - head)
	if tailStart < head {
		tailStart = head
	}
	for tailStart < len(inner) && !utf8.RuneStart(inner[tailStart]) {
		tailStart++
	}
	return text[:innerStart] + inner[:head] + guideMarker + inner[tailStart:] + text[innerEnd:], true
}

const textTrimMarker = "\n\n[earlier conversation trimmed to fit the upstream 64 KB frame limit]\n\n"

// shrinkToLimit removes bytes from the middle of s until wrap(s) fits. It
// preserves the beginning and the end, so recent context and trailing
// authority notices survive.
func shrinkToLimit(s string, wrap func(string) ([]byte, error), marker string) string {
	out := s
	for range 6 {
		body, err := wrap(out)
		if err != nil || len(body) <= maxUserMessageBytes {
			return out
		}
		over := len(body) - maxUserMessageBytes
		next := shrinkMiddle(out, over, marker)
		if next == out {
			return out
		}
		out = next
	}
	return out
}

// shrinkMiddle removes about `remove` bytes from s while keeping 1/4 of the
// retained text from the head and 3/4 from the tail. Slices are adjusted to
// UTF-8 rune boundaries so the JSON stays valid.
func shrinkMiddle(s string, remove int, marker string) string {
	const minKeep = 4096
	target := len(s) - remove - len(marker)
	if target < minKeep {
		target = minKeep
	}
	if target >= len(s) {
		return s
	}
	head := target / 4
	if head > len(s) {
		head = len(s)
	}
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	tailStart := len(s) - (target - head)
	if tailStart < head {
		tailStart = head
	}
	for tailStart < len(s) && !utf8.RuneStart(s[tailStart]) {
		tailStart++
	}
	return s[:head] + marker + s[tailStart:]
}

func titleFor(prompt string) string {
	clean := strings.Join(strings.Fields(prompt), " ")
	if len(clean) > 60 {
		clean = clean[:60] + "…"
	}
	if clean == "" {
		clean = "New Conversation"
	}
	return clean
}
