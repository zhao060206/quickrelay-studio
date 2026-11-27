package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quickrelay-studio/internal/accounts"
	"quickrelay-studio/internal/config"
	"quickrelay-studio/internal/store"
	"quickrelay-studio/internal/version"
)

var (
	sseKeepaliveInterval = 15 * time.Second
	maxThinkingPrestart  = 45 * time.Second
)

type Server struct {
	cfg      *config.Config
	accounts *accounts.Manager
	store    *store.Store
	tokens   *APIKeyAuth
	upstream *Upstream
	sessions *conversationStore
	outposts *outpostPool
}

func NewServer(cfg *config.Config, mgr *accounts.Manager) *Server {
	var db *store.Store
	if mgr != nil {
		db = mgr.Store()
	}
	return &Server{
		cfg:      cfg,
		accounts: mgr,
		store:    db,
		tokens:   NewAPIKeyAuth(cfg.APIKeys),
		upstream: NewUpstream(cfg),
		sessions: newConversationStore(cfg.SessionTTL.Duration(), 256, db),
		outposts: newOutpostPool(cfg.OutpostIdleTTL.Duration(), cfg.OutpostMaxSessions),
	}
}

// Close 在服务停机时优雅关闭并反注册全部挂起的虚拟 Outpost 设备。
func (s *Server) Close(ctx context.Context) {
	if s == nil || s.outposts == nil {
		return
	}
	s.outposts.Shutdown(ctx)
}

// DeleteConversation 同步清除指定会话的内存映射、挂起 Outpost 与持久化记录。
func (s *Server) DeleteConversation(ctx context.Context, key string) error {
	if s == nil {
		return nil
	}
	if s.outposts != nil {
		s.outposts.InvalidateKey(key)
	}
	if s.sessions == nil {
		return nil
	}
	return s.sessions.invalidateKey(ctx, key)
}

// ClearConversations 同步清空全部会话的内存映射、挂起 Outpost 与持久化记录。
func (s *Server) ClearConversations(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if s.outposts != nil {
		s.outposts.InvalidateAll()
	}
	if s.sessions == nil {
		return nil
	}
	return s.sessions.invalidateAll(ctx)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/v1/messages", s.handleAnthropicMessages)
	mux.HandleFunc("/v1/messages/count_tokens", s.handleAnthropicCountTokens)
	return s.withCORS(mux)
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Api-Key, X-Thread-Id, X-Conversation-Id, Anthropic-Version, Anthropic-Beta")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"service": "quickrelay-studio",
		"version": version.Current,
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	if !s.tokens.Allowed(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid proxy API key")
		return
	}
	created := time.Now().Unix()
	data := make([]map[string]any, 0, len(s.cfg.Models))
	for _, id := range s.cfg.Models {
		data = append(data, map[string]any{
			"id":       id,
			"object":   "model",
			"created":  created,
			"owned_by": "quickwork",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// preparedTurn 封装一次 OpenAI 或 Anthropic 请求共享的会话上下文、重试路由、输出过滤与日志埋点。
type preparedTurn struct {
	server      *Server
	ctx         context.Context
	cancel      context.CancelFunc
	conv        *conversation
	convKeyStr  string
	convEpoch   uint64
	convVersion uint64

	id             string
	created        int64
	protocol       string
	endpoint       string
	req            *ChatRequest
	model          string
	requestedModel string

	allowedTools          map[string]bool
	choiceMode            string
	choiceName            string
	parallelTools         bool
	bridgeActive          bool
	nativeOutpostActive   bool
	nativeOutpostFallback bool
	baseHash              string
	guideHash             string
	toolHash              string
	resumeBaseHash        string
	fullGuide             string
	fullBridgePrompt      string
	fullPrompt            string
	fullHistory           string
	fullCurrentTurn       string
	fullCurrentIsTool     bool
	fullImages            []ImageAttachment
	fullMessages          []Message
	fullLoaded            bool
	fallbackText          string
	auxRequest            bool
	suggestionMode        bool
	compactRequest        bool

	in                      ChatInput
	chatAgentID             string
	explicitThreadID        string
	explicitThreadKnown     bool
	explicitThreadAccountID int64
	skipConvSave            bool
	requestedThinkingEffort string
	resume                  bool
	startedAt               time.Time
	attempts                int
	maxAttempts             int
	usedAccount             *store.Account

	blockedAccounts     map[int64]bool
	attemptedAccounts   map[int64]bool
	refreshedAccounts   map[int64]bool
	sameAccountRetried  map[int64]bool
	pinnedFirst         bool
	preferredAccount    int64
	remoteToolCorrected bool
	upstreamWaitingNano atomic.Int64
}

func (t *preparedTurn) markUpstreamWaiting() {
	t.upstreamWaitingNano.Store(time.Now().UnixNano())
}

func (t *preparedTurn) clearUpstreamWaiting() {
	t.upstreamWaitingNano.Store(0)
}

func (t *preparedTurn) upstreamWaitingSince() time.Time {
	ns := t.upstreamWaitingNano.Load()
	if ns <= 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// needsThinkingHeartbeat 判断当前轮次是否需要开启长耗时首包预热与 SSE 保活心跳（覆盖 high / max 思考档及图片/文档附件请求）。
func (t *preparedTurn) needsThinkingHeartbeat() bool {
	if t == nil {
		return false
	}
	eff := strings.TrimSpace(t.in.ThinkingEffort)
	reqEff := strings.TrimSpace(t.requestedThinkingEffort)
	return strings.EqualFold(eff, "max") ||
		strings.EqualFold(reqEff, "max") ||
		strings.EqualFold(eff, "high") ||
		strings.EqualFold(reqEff, "high") ||
		len(t.in.Images) > 0
}

func (s *Server) turnTimeoutFor(thinkingEffort string) time.Duration {
	base := s.cfg.RequestTimeout.Duration()
	if base <= 0 {
		base = 15 * time.Minute
	}
	if strings.EqualFold(strings.TrimSpace(thinkingEffort), "max") {
		maxDur := s.cfg.MaxThinkingTimeout.Duration()
		if maxDur <= 0 {
			maxDur = 30 * time.Minute
		}
		if maxDur > base {
			return maxDur
		}
	}
	return base
}

var (
	thinkingPrestreamHold     = 40 * time.Second
	thinkingPrestreamMaxBytes = 8192
)

// prestreamThinkingBuf 在启用客户端工具桥接且 SSE 尚未发出响应头前暂存思考增量，
// 防止模型先思考后误调 AWS 云端工具时因流已开始而无法自动纠偏重发。
type prestreamThinkingBuf struct {
	mu    sync.Mutex
	buf   strings.Builder
	since time.Time
}

func (b *prestreamThinkingBuf) Add(chunk string, maxBytes int, maxHold time.Duration) (string, bool) {
	if chunk == "" {
		return "", false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len() == 0 {
		b.since = time.Now()
	}
	b.buf.WriteString(chunk)
	if (maxBytes > 0 && b.buf.Len() >= maxBytes) || maxHold <= 0 || time.Since(b.since) >= maxHold {
		out := b.buf.String()
		b.buf.Reset()
		b.since = time.Time{}
		return out, true
	}
	return "", false
}

func (b *prestreamThinkingBuf) Drain() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len() == 0 {
		return ""
	}
	out := b.buf.String()
	b.buf.Reset()
	b.since = time.Time{}
	return out
}

func (b *prestreamThinkingBuf) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
	b.since = time.Time{}
}

func startSSEHeartbeat(
	ctx context.Context,
	isMaxThinking bool,
	isStarted func() bool,
	waitingSince func() time.Time,
	prestart func(),
	ping func(),
	lastWrite func() time.Time,
) func() {
	interval := sseKeepaliveInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	prestartDelay := maxThinkingPrestart
	if prestartDelay <= 0 {
		prestartDelay = 45 * time.Second
	}
	tickInterval := interval
	if isMaxThinking && prestartDelay < tickInterval {
		tickInterval = prestartDelay
	}
	if isMaxThinking {
		if step := prestartDelay / 3; step >= 5*time.Millisecond && step < tickInterval {
			tickInterval = step
		}
	}
	if tickInterval < 5*time.Millisecond {
		tickInterval = 5 * time.Millisecond
	}

	beginAt := time.Now()
	done := make(chan struct{})
	var wg sync.WaitGroup
	var stopOnce sync.Once
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(tickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !isStarted() {
					if isMaxThinking {
						// 仅在 WebSocket 建连成功并已发出 user_message 进入上游思考阶段后才计入首包预热延迟，
						// 防止拨号超时期间过早发送 SSE 头部而锁死后续自动重试。
						since := beginAt
						if waitingSince != nil {
							since = waitingSince()
						}
						if !since.IsZero() && time.Since(since) >= prestartDelay {
							prestart()
						}
					}
					continue
				}
				lw := lastWrite()
				if lw.IsZero() || time.Since(lw) >= interval {
					ping()
				}
			}
		}
	}()
	return func() {
		stopOnce.Do(func() {
			close(done)
		})
		wg.Wait()
	}
}

func (s *Server) prepareTurn(r *http.Request, req *ChatRequest, protocol, endpoint, idPrefix string) (*preparedTurn, error) {
	isSuggestion := isSuggestionModeRequest(req)
	isTranscript := !isSuggestion && isTranscriptSideRequest(req)
	fullMessages := req.Messages
	if isSuggestion {
		if trimmed := trimSuggestionMessages(req.Messages); len(trimmed) > 0 {
			fullMessages = trimmed
		}
	} else if isTranscript {
		if trimmed := trimTranscriptMessages(req.Messages); len(trimmed) > 0 {
			fullMessages = trimmed
		}
	}
	prompt, err := formatPrompt(fullMessages)
	if err != nil {
		return nil, err
	}

	var model string
	if protocol == "anthropic" {
		model = s.mapAnthropicModel(req.Model)
	} else {
		model = s.mapModel(req.Model)
	}
	if isTranscript && model != "fast" {
		log.Printf("chat %s model_guard reason=transcript_side_fast model=%q->%q",
			idPrefix, model, "fast")
		model = "fast"
	}

	allowedTools := requestedToolNames(req)
	choiceMode, choiceName := parseToolChoice(req.ToolChoice)
	if choiceMode == "function" && choiceName != "" {
		allowedTools[choiceName] = true
	}
	parallelTools := true
	if req.ParallelToolCalls != nil {
		parallelTools = *req.ParallelToolCalls
	}
	workMode := s.cfg.GetWorkMode()
	isCompact := !isSuggestion && !isTranscript && isCompactionRequest(req, prompt)
	var bridgePrompt string
	var bridgeActive bool
	var leadingGuide string
	var fullGuide string
	var toolHash string
	var guideHash string
	if !isSuggestion {
		if !isCompact {
			bridgePrompt = buildToolBridgePromptForMode(req, choiceMode, choiceName, parallelTools, workMode)
			bridgeActive = bridgePrompt != ""
			toolHash = toolProtocolFingerprint(req, choiceMode, choiceName, parallelTools)
		}
		leadingGuide = leadingGuidelinesForMode(req.Messages, workMode)
		fullGuide = callerGuidelinesForMode(req.Messages, workMode)
		guideHash = shortSHA256(leadingGuide)
	}
	baseHash := encodeBaseHash(guideHash, toolHash)
	isAux := isSuggestion || isAuxiliarySideRequest(req, prompt)

	rawThinkingEffort := resolveThinkingEffort(req, s.cfg.ThinkingEffort)
	thinkingEffort := rawThinkingEffort
	explicitOff := isExplicitThinkingOff(req)
	if isSuggestion {
		if !explicitOff && thinkingEffort != "" && thinkingEffort != "low" {
			log.Printf("chat %s thinking_guard reason=suggestion_mode_downgrade effort=%q->%q",
				idPrefix, thinkingEffort, "low")
			thinkingEffort = "low"
		}
	} else if isTranscript {
		if !explicitOff && thinkingEffort != "low" {
			log.Printf("chat %s thinking_guard reason=transcript_side_downgrade effort=%q->%q",
				idPrefix, thinkingEffort, "low")
			thinkingEffort = "low"
		}
	} else {
		if guardedEffort, guardReason := applyThinkingGuards(thinkingEffort, explicitOff, isCompact, bridgeActive, choiceMode, s.cfg); guardReason != "" {
			log.Printf("chat %s thinking_guard reason=%s effort=%q->%q bridge=%t choice=%q",
				idPrefix, guardReason, thinkingEffort, guardedEffort, bridgeActive, choiceMode)
			thinkingEffort = guardedEffort
		}
		if wmEffort, wmReason := applyWorkModeGuards(thinkingEffort, explicitOff, isCompact, bridgeActive, choiceMode, workMode, hasToolResultHistory(req.Messages)); wmReason != "" {
			log.Printf("chat %s thinking_guard reason=%s effort=%q->%q work_mode=%s",
				idPrefix, wmReason, thinkingEffort, wmEffort, workMode)
			thinkingEffort = wmEffort
		}
	}
	turnTimeout := s.turnTimeoutFor(thinkingEffort)
	ctx, cancel := context.WithTimeout(r.Context(), turnTimeout)
	convKey := conversationKey(r, req, baseHash)
	if isAux && !strings.HasPrefix(convKey, "aux:") {
		convKey = "aux:" + uuid()
	}
	conv, convEpoch, convVer, acqErr := s.sessions.acquire(ctx, convKey)
	if acqErr != nil {
		cancel()
		return nil, acqErr
	}

	effectiveAgentID := strings.TrimSpace(s.cfg.ChatAgentID)
	explicitConvID := strings.TrimSpace(r.Header.Get("X-Conversation-Id"))
	explicitThreadID := strings.TrimSpace(r.Header.Get("X-Thread-Id"))
	if isAux {
		explicitThreadID = ""
	}
	explicitThreadKnown := false
	var explicitThreadAccountID int64
	var explicitTargetConv *conversation
	skipConvSave := false
	// 在选择传输方式之前彻底解决显式 X-Thread-Id 与本地会话/历史的冲突：
	// 禁止在命中其他会话 A 时把 A 的增量或线程 ID 误发给上游，也不把与 B 历史不匹配的请求假作续接 B。
	if !isAux && explicitThreadID != "" {
		if conv.threadID != "" && conv.threadID == explicitThreadID {
			if !conv.matchesChatAgent(effectiveAgentID) {
				s.sessions.release(conv)
				cancel()
				return nil, fmt.Errorf("%w: thread %q", ErrThreadAgentMismatch, explicitThreadID)
			}
			explicitThreadKnown = true
			explicitThreadAccountID = conv.accountID
			explicitTargetConv = conv
		} else {
			rec, found := s.sessions.lookupThreadRecord(ctx, explicitThreadID)
			if found && (!rec.ChatAgentBound || strings.TrimSpace(rec.ChatAgentID) != effectiveAgentID) {
				s.sessions.release(conv)
				cancel()
				return nil, fmt.Errorf("%w: thread %q", ErrThreadAgentMismatch, explicitThreadID)
			}
			if conv.threadID != "" && conv.threadID != explicitThreadID {
				// convKey 已绑定线程 A（例如携带了 A 的 X-Conversation-Id 或匹配 A 的前缀），此时禁止冲突复用或覆盖 A。
				if explicitConvID != "" {
					s.sessions.release(conv)
					cancel()
					return nil, fmt.Errorf("%w: X-Conversation-Id %q is bound to thread %q, conflicts with X-Thread-Id %q", ErrExplicitThreadConflict, explicitConvID, conv.threadID, explicitThreadID)
				}
				if !found || rec == nil {
					s.sessions.release(conv)
					cancel()
					return nil, fmt.Errorf("%w: history matched thread %q rather than explicit X-Thread-Id %q", ErrExplicitThreadConflict, conv.threadID, explicitThreadID)
				}
				candConv := conversationFromRecord(rec)
				explicitThreadKnown = true
				explicitThreadAccountID = rec.AccountID
				explicitTargetConv = candConv
				skipConvSave = true
			} else if found && rec != nil {
				explicitThreadKnown = true
				explicitThreadAccountID = rec.AccountID
				explicitTargetConv = conversationFromRecord(rec)
			}
		}
		if explicitTargetConv != nil && (skipConvSave || len(nonLeadingMessages(req.Messages)) > 1) {
			if _, ok := explicitTargetConv.delta(req.Messages); !ok {
				s.sessions.release(conv)
				cancel()
				return nil, fmt.Errorf("%w: request history does not match explicit X-Thread-Id %q", ErrExplicitThreadConflict, explicitThreadID)
			}
		}
	}
	// 常规会话 Agent 边界校验：切换 Agent（默认<->自定义 / A<->B / 老库无绑定）时仅废弃该条本地复用映射，
	// 强制以全新线程和完整历史、完整工具表重建，不删除 AWS 云端历史也不影响其他会话。
	if s.cfg.SessionReuse && !isAux && explicitThreadID == "" && conv.threadID != "" && !conv.matchesChatAgent(effectiveAgentID) {
		log.Printf("chat %s resetting stale session thread=%q due to chat_agent_id boundary (bound=%t prev=%q cur=%q)",
			idPrefix, conv.threadID, conv.chatAgentBound, conv.chatAgentID, effectiveAgentID)
		s.sessions.reset(conv, convEpoch, convVer)
	}

	t := &preparedTurn{
		server:                  s,
		ctx:                     ctx,
		cancel:                  cancel,
		conv:                    conv,
		convKeyStr:              convKey,
		convEpoch:               convEpoch,
		convVersion:             convVer,
		id:                      idPrefix + strings.ReplaceAll(uuid(), "-", ""),
		created:                 time.Now().Unix(),
		protocol:                protocol,
		endpoint:                endpoint,
		req:                     req,
		model:                   model,
		allowedTools:            allowedTools,
		choiceMode:              choiceMode,
		choiceName:              choiceName,
		parallelTools:           parallelTools,
		bridgeActive:            bridgeActive,
		baseHash:                baseHash,
		guideHash:               guideHash,
		toolHash:                toolHash,
		chatAgentID:             effectiveAgentID,
		explicitThreadID:        explicitThreadID,
		explicitThreadKnown:     explicitThreadKnown,
		explicitThreadAccountID: explicitThreadAccountID,
		skipConvSave:            skipConvSave,
		fullGuide:               fullGuide,
		fullBridgePrompt:        bridgePrompt,
		fullMessages:            fullMessages,
		fallbackText:            prompt,
		auxRequest:              isAux,
		suggestionMode:          isSuggestion,
		compactRequest:          isCompact,
		requestedThinkingEffort: rawThinkingEffort,
		startedAt:               time.Now(),
		maxAttempts:             s.maxAttempts(),
		blockedAccounts:         make(map[int64]bool),
		attemptedAccounts:       make(map[int64]bool),
		refreshedAccounts:       make(map[int64]bool),
		sameAccountRetried:      make(map[int64]bool),
	}

	resumeSource := conv
	if !isAux && explicitThreadID != "" {
		resumeSource = explicitTargetConv
	}
	if resumeSource != nil {
		t.resumeBaseHash = resumeSource.baseHash
	}
	promptText := ""
	promptHistory := ""
	promptCurrent := ""
	promptCurrentIsTool := false
	turnGuidelines := ""
	var images []ImageAttachment
	if s.cfg.SessionReuse && !isAux && resumeSource != nil && resumeSource.threadID != "" {
		if delta, ok := resumeSource.delta(req.Messages); ok {
			deltaGuide := deltaDeveloperGuidelinesForMode(delta, workMode)
			deltaFormatted, deltaImages, dErr := formatResumeDeltaSplitWithImages(ctx, req.Messages, delta)
			if dErr != nil && errors.Is(dErr, ErrInvalidAttachment) {
				t.Close()
				return nil, dErr
			}
			if dErr == nil && (hasVisibleText(deltaFormatted.Full) || len(deltaImages) > 0 || deltaGuide != "") {
				promptText = deltaFormatted.Full
				promptHistory = deltaFormatted.History
				promptCurrent = deltaFormatted.Current
				promptCurrentIsTool = deltaFormatted.CurrentIsTool
				if !isCompact {
					images = deltaImages
				}
				turnGuidelines = deltaGuide
				t.resume = true
			}
		}
	}
	if !t.resume {
		if fErr := t.loadFull(); fErr != nil {
			t.Close()
			return nil, fErr
		}
		promptText = t.fullPrompt
		promptHistory = t.fullHistory
		promptCurrent = t.fullCurrentTurn
		promptCurrentIsTool = t.fullCurrentIsTool
		images = t.fullImages
		turnGuidelines = fullGuide
	}

	// 续聊工具表瘦身：同一 thread_id 内只要工具指纹未变且未到 16 轮周期重锚点，
	// 仅发 ~90 Token 极简提醒（附无引号本地工具名清单），消除每轮重复灌入 ~20,000 Token 的黑洞。
	turnToolProtocol := bridgePrompt
	toolCompact := false
	if t.resume && bridgeActive && toolHash != "" && resumeSource != nil {
		_, prevToolHash := splitBaseHash(resumeSource.baseHash)
		turnCount := len(nonLeadingMessages(req.Messages)) / 2
		needPeriodicAnchor := turnCount > 0 && turnCount%16 == 0
		if prevToolHash == toolHash && !needPeriodicAnchor {
			turnToolProtocol = buildResumeToolReminder(choiceMode, choiceName, parallelTools, collectToolNames(req)...)
			toolCompact = true
		}
	}

	threadID := explicitThreadID
	t.in = ChatInput{
		Prompt:            promptText,
		HistoryPrompt:     promptHistory,
		CurrentTurn:       promptCurrent,
		CurrentTurnIsTool: promptCurrentIsTool,
		LatestUserText:    latestTurnPreview(req.Messages),
		Images:            images,
		Model:             model,
		ThinkingEffort:    thinkingEffort,
		ThreadID:          threadID,
		Title:             titleFor(lastUserText(req.Messages)),
		Guidelines:        turnGuidelines,
		ToolProtocol:      turnToolProtocol,
		WorkMode:          workMode,
		ChatAgentID:       effectiveAgentID,
		CreateSession:     true,
		DeleteSession:     (!s.cfg.SessionReuse && !s.cfg.KeepSessions) || isAux,
		ClientToolsActive: bridgeActive,
	}
	if t.resume && resumeSource != nil {
		t.in.ThreadID = resumeSource.threadID
		t.in.CreateSession = false
	} else if explicitThreadKnown {
		t.in.ThreadID = explicitThreadID
		t.in.CreateSession = false
	}
	var pinnedAccountID int64
	if t.resume && resumeSource != nil {
		pinnedAccountID = resumeSource.accountID
	} else if explicitThreadKnown {
		pinnedAccountID = explicitThreadAccountID
	}
	t.pinnedFirst = t.resume && resumeSource == conv && conv.accountID != 0
	if !t.pinnedFirst && pinnedAccountID != 0 {
		t.preferredAccount = pinnedAccountID
	}

	t.requestedModel = strings.TrimSpace(req.Model)
	if t.requestedModel == "" {
		t.requestedModel = "(default)"
	}
	turnSystemChars := len(t.in.Guidelines) + len(t.in.ToolProtocol)
	log.Printf("chat %s start proto=%s model=%s upstream_model=%s thinking_effort=%q timeout=%s stream=%t messages=%d images=%d prompt_chars=%d system_chars=%d reuse=%t tool_compact=%t thread=%q",
		t.id, protocol, t.requestedModel, model, thinkingEffort, turnTimeout, req.Stream, len(req.Messages), len(t.in.Images), len(promptText), turnSystemChars, t.resume, toolCompact, t.in.ThreadID)

	return t, nil
}

func (t *preparedTurn) Close() {
	if t.conv != nil {
		t.server.sessions.release(t.conv)
		t.conv = nil
	}
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
}

func (t *preparedTurn) loadFull() error {
	if t.fullLoaded {
		return nil
	}
	msgs := t.fullMessages
	if len(msgs) == 0 && t.req != nil {
		msgs = t.req.Messages
	}
	if t.suggestionMode || t.compactRequest {
		p, err := formatPrompt(msgs)
		if err != nil {
			return err
		}
		t.fullPrompt = p
		t.fullHistory = ""
		t.fullCurrentTurn = p
		t.fullCurrentIsTool = false
		t.fullImages = nil
		t.fullLoaded = true
		return nil
	}
	fp, imgs, err := formatPromptSplitWithImages(t.ctx, msgs)
	if err != nil {
		return err
	}
	t.fullPrompt = fp.Full
	t.fullHistory = fp.History
	t.fullCurrentTurn = fp.Current
	t.fullCurrentIsTool = fp.CurrentIsTool
	t.fullImages = imgs
	t.fullLoaded = true
	return nil
}

// switchToFullTranscript 将当前轮切换为新建会话与完整历史/工具表发送，但不提前销毁持久化会话映射，
// 以便在后续重试仍因临时网络故障失败时保留原账号既有缓存，待切号成功后由 saveConversation 自然覆盖。
// 若重放完整历史所需的附件校验失败，明确返回错误以阻断残缺请求。
func (t *preparedTurn) switchToFullTranscript() error {
	if t.explicitThreadID != "" {
		return fmt.Errorf("%w: cannot replace explicit X-Thread-Id %q with a new thread", ErrExplicitThreadConflict, t.explicitThreadID)
	}
	if fErr := t.loadFull(); fErr != nil {
		return fErr
	}
	t.resume = false
	t.in.ThreadID = ""
	t.in.CreateSession = true
	t.in.Guidelines = t.fullGuide
	t.in.ToolProtocol = t.fullBridgePrompt
	t.in.Prompt = t.fullPrompt
	t.in.HistoryPrompt = t.fullHistory
	t.in.CurrentTurn = t.fullCurrentTurn
	t.in.CurrentTurnIsTool = t.fullCurrentIsTool
	t.in.Images = t.fullImages
	return nil
}

func (t *preparedTurn) resetConversation() error {
	if err := t.switchToFullTranscript(); err != nil {
		return err
	}
	t.server.sessions.reset(t.conv, t.convEpoch, t.convVersion)
	return nil
}

func (t *preparedTurn) resetConversationAfterFailure(err error) {
	if t.explicitThreadID == "" && shouldResetConversation(t.conv, err) {
		t.server.sessions.reset(t.conv, t.convEpoch, t.convVersion)
	}
}

func (t *preparedTurn) saveConversation(res ChatResult, outText string, outTools []ToolCall) {
	if t.skipConvSave {
		return
	}
	if t.explicitThreadID != "" && t.conv != nil && t.conv.threadID != "" && t.conv.threadID != t.explicitThreadID {
		return
	}
	if !t.auxRequest && t.server.cfg.SessionReuse && res.ThreadID != "" && t.usedAccount != nil {
		asstHash := assistantOutputFingerprint(outText, outTools)
		gHash, tHash := t.guideHash, t.toolHash
		if gHash == "" && tHash == "" && t.baseHash != "" {
			gHash, tHash = splitBaseHash(t.baseHash)
		}
		savedHash := encodeBaseHashForTransport(gHash, tHash, t.nativeOutpostActive)
		t.server.sessions.saveWithAgent(t.conv, t.convEpoch, t.convVersion, res.ThreadID, t.usedAccount.ID, savedHash, t.chatAgentID, t.req.Messages, asstHash)
	}
}

func (t *preparedTurn) convKey() string {
	if t == nil {
		return ""
	}
	if t.convKeyStr != "" {
		return t.convKeyStr
	}
	if t.conv != nil {
		return t.conv.key
	}
	return ""
}

func (t *preparedTurn) shouldUseNativeOutpost(sess *Session) bool {
	if t == nil || t.server == nil || t.server.outposts == nil || t.server.cfg == nil {
		return false
	}
	mode := t.server.cfg.GetToolMode()
	if mode == config.ToolModeTextBridge {
		return false
	}
	if !t.bridgeActive || t.choiceMode == "none" || t.auxRequest || t.suggestionMode || t.nativeOutpostFallback {
		return false
	}
	if mode == config.ToolModeAuto && t.explicitThreadID != "" && isTextBridgeBaseHash(t.resumeBaseHash) &&
		!t.server.outposts.HasSessionOrThread(t.convKey(), t.explicitThreadID) {
		return false
	}
	hasBlockingMedia := len(t.in.Images) > 0
	if t.req != nil && len(t.req.Messages) > 0 {
		hasBlockingMedia = recentMessagesHaveBinaryMedia(t.req.Messages, outpostRecentMediaWindow)
	}
	if hasBlockingMedia || sess == nil || isGatewayProvider(sess.Provider) || strings.TrimSpace(sess.TenantURL) == "" {
		return false
	}
	return true
}

// detachFromNativeOutpostForTextBridge 在会话从虚拟 Outpost 切往 text_bridge（如工具读取了本地图片或单帧超 63 KiB 自动回退）时，
// 关闭并反注册原挂起 Outpost 连接，强制切换为全新 thread_id 并附带完整历史、完整工具协议与图片附件，
// 彻底杜绝向等待原生 tool_result 的挂起 Outpost 线程投递普通 user_message 造成云端排队死锁。
func (t *preparedTurn) detachFromNativeOutpostForTextBridge() error {
	if t == nil || t.server == nil {
		return nil
	}
	prevThreadID := strings.TrimSpace(t.in.ThreadID)
	hasOutpostConn := t.server.outposts != nil && t.server.outposts.HasSessionOrThread(t.convKey(), prevThreadID)
	prevWasOutpost := hasOutpostConn ||
		(t.resume && isOutpostBaseHash(t.resumeBaseHash)) ||
		(t.resume && t.conv != nil && isOutpostBaseHash(t.conv.baseHash)) ||
		(t.explicitThreadID != "" && isOutpostBaseHash(t.resumeBaseHash))
	if !prevWasOutpost {
		return nil
	}
	if t.explicitThreadID != "" {
		return fmt.Errorf("%w: explicit X-Thread-Id %q is bound to a native outpost session and cannot dispatch via text_bridge", ErrExplicitThreadConflict, t.explicitThreadID)
	}
	if t.server.outposts != nil {
		t.server.outposts.InvalidateKey(t.convKey())
		if prevThreadID != "" {
			t.server.outposts.InvalidateThread(prevThreadID)
		}
	}
	if t.resume || (!t.in.CreateSession && prevThreadID != "") {
		log.Printf("chat %s detaching native_outpost thread=%q for text_bridge; switching to fresh thread with full transcript", t.id, prevThreadID)
		if err := t.switchToFullTranscript(); err != nil {
			return err
		}
	}
	return nil
}

func (t *preparedTurn) responseOutput(result ChatResult) (string, []ToolCall) {
	text := result.Text
	var replies []ToolCallReply
	if t.bridgeActive {
		if t.nativeOutpostActive {
			if len(result.ToolCalls) > 0 {
				replies = nativeToolReplies(result.ToolCalls, t.allowedTools)
			}
		} else {
			text, replies = parseClientToolCalls(text, t.allowedTools)
			if len(replies) == 0 {
				replies = nativeToolReplies(result.ToolCalls, t.allowedTools)
			}
		}
	}
	var uiCalls []ToolCallReply
	text, uiCalls = cleanQuickOutput(text, t.allowedTools, func(tc ToolCallReply) bool {
		return t.bridgeActive && !t.nativeOutpostActive && len(replies) == 0 && (t.choiceMode != "function" || t.choiceName == "" || tc.Name == t.choiceName)
	})
	if len(uiCalls) > 0 && len(replies) == 0 {
		replies = append(replies, uiCalls...)
	}
	if t.bridgeActive {
		replies = limitToolReplies(replies, t.choiceMode, t.choiceName, t.parallelTools)
	} else {
		replies = nil
	}
	return text, replyToolCalls(replies)
}

func (t *preparedTurn) validateToolChoice(calls []ToolCall) error {
	if t.suggestionMode {
		return nil
	}
	switch t.choiceMode {
	case "function":
		for _, call := range calls {
			if call.Function.Name == t.choiceName {
				return nil
			}
		}
		return fmt.Errorf("%w: choice=function(%q) required tool call, but no matching call was returned", ErrOutpostToolPolicyMismatch, t.choiceName)
	case "required":
		if len(calls) == 0 {
			return fmt.Errorf("%w: choice=required expected at least one tool call", ErrOutpostToolPolicyMismatch)
		}
	}
	return nil
}

func (t *preparedTurn) selectAccount() (*store.Account, *Session, error) {
	var pinnedCooldownErr *accounts.CooldownError
	for {
		if err := t.ctx.Err(); err != nil {
			return nil, nil, err
		}
		var acct *store.Account
		var selectErr error
		if t.pinnedFirst || t.preferredAccount != 0 {
			accountID := t.preferredAccount
			t.preferredAccount = 0
			if t.pinnedFirst {
				accountID = t.conv.accountID
				t.pinnedFirst = false
			}
			acct, selectErr = t.server.accounts.NextPinned(t.ctx, accountID)
			if selectErr != nil {
				if t.explicitThreadID != "" {
					return nil, nil, fmt.Errorf("%w: account %d for explicit X-Thread-Id %q is unavailable", ErrExplicitThreadConflict, accountID, t.explicitThreadID)
				}
				var cdErr *accounts.CooldownError
				if errors.As(selectErr, &cdErr) {
					pinnedCooldownErr = cdErr
				}
				t.blockedAccounts[accountID] = true
				log.Printf("chat %s preferred account %d unavailable; selecting another account", t.id, accountID)
				if swErr := t.switchToFullTranscript(); swErr != nil {
					return nil, nil, swErr
				}
				continue
			}
		} else {
			unavailable := make(map[int64]bool, len(t.blockedAccounts)+len(t.attemptedAccounts))
			for accountID := range t.blockedAccounts {
				unavailable[accountID] = true
			}
			for accountID := range t.attemptedAccounts {
				unavailable[accountID] = true
			}
			acct, selectErr = t.server.accounts.NextExcluding(t.ctx, unavailable)
			for accountID := range unavailable {
				if !t.attemptedAccounts[accountID] {
					t.blockedAccounts[accountID] = true
				}
			}
			if errors.Is(selectErr, accounts.ErrNoAvailableAccounts) && len(t.attemptedAccounts) > 0 {
				clear(t.attemptedAccounts)
				acct, selectErr = t.server.accounts.NextExcluding(t.ctx, t.blockedAccounts)
			}
			if selectErr != nil {
				if errors.Is(selectErr, accounts.ErrAllAccountsCoolingDown) {
					return nil, nil, selectErr
				}
				if pinnedCooldownErr != nil {
					return nil, nil, pinnedCooldownErr
				}
				if rem := t.server.accounts.PoolCooldownRemaining(t.ctx); rem > 0 {
					return nil, nil, &accounts.CooldownError{RetryAfter: rem}
				}
				return nil, nil, selectErr
			}
		}
		sess := SessionFromAccount(acct, t.server.cfg)
		if err := sess.validate(); err != nil {
			t.blockedAccounts[acct.ID] = true
			t.server.accounts.ReportFailure(t.ctx, acct.ID, err)
			log.Printf("chat %s account=%s invalid_session error=%v", t.id, accountLogLabel(acct), err)
			if t.resume {
				if rErr := t.resetConversation(); rErr != nil {
					return nil, nil, rErr
				}
			}
			continue
		}
		return acct, sess, nil
	}
}

func (t *preparedTurn) runChat(onEvent EventFunc, streamProgress func() (started, hasOutput bool), resetOutput func(), completeOutput func(ChatResult) ([]ToolCall, error)) (ChatResult, error) {
	var res ChatResult
	var lastErr error
	forceCorrection := false
	for t.attempts < t.maxAttempts || forceCorrection {
		wasForcedCorrection := forceCorrection
		forceCorrection = false
		if t.attempts > 0 && !wasForcedCorrection {
			if err := waitForRetry(t.ctx, t.attempts); err != nil {
				return res, err
			}
		}
		acct, sess, err := t.selectAccount()
		if err != nil {
			if errors.Is(err, accounts.ErrAllAccountsCoolingDown) {
				return res, err
			}
			if lastErr != nil && errors.Is(err, accounts.ErrNoAvailableAccounts) {
				return res, lastErr
			}
			return res, err
		}
		// 终极防线：若当前仍处于续聊状态但选中账号已不是原会话所属账号，
		// 必须强制切换为新会话并携带完整历史与完整工具表，杜绝把旧账号 thread_id 与精简增量发给新账号；
		// 若调用方显式指定 X-Thread-Id 则绝不静默替换新线程，而是直接报 409 冲突。
		if t.explicitThreadID != "" && t.explicitThreadAccountID != 0 && acct.ID != t.explicitThreadAccountID {
			return res, fmt.Errorf("%w: explicit X-Thread-Id %q belongs to account %d, cannot dispatch to account %d", ErrExplicitThreadConflict, t.explicitThreadID, t.explicitThreadAccountID, acct.ID)
		}
		if t.resume && t.conv != nil && t.conv.accountID != 0 && acct.ID != t.conv.accountID {
			if t.explicitThreadID != "" {
				return res, fmt.Errorf("%w: cannot replace explicit X-Thread-Id %q across account switch", ErrExplicitThreadConflict, t.explicitThreadID)
			}
			log.Printf("chat %s switched account %d->%d during resume; rebuilding full transcript", t.id, t.conv.accountID, acct.ID)
			if swErr := t.switchToFullTranscript(); swErr != nil {
				return res, swErr
			}
		}
		if resetOutput != nil {
			resetOutput()
		}
		t.usedAccount = acct
		t.attemptedAccounts[acct.ID] = true
		t.attempts++
		log.Printf("chat %s attempt=%d/%d account=%s", t.id, t.attempts, t.maxAttempts, accountLogLabel(acct))
		attemptStarted := time.Now()
		t.clearUpstreamWaiting()
		attemptIn := t.in
		attemptIn.OnDispatched = t.markUpstreamWaiting
		t.nativeOutpostActive = false
		if t.shouldUseNativeOutpost(sess) {
			t.nativeOutpostActive = true
			res, err = t.server.outposts.RunTurn(t.ctx, t, sess, attemptIn.OnDispatched, onEvent)
			if err != nil && errors.Is(err, ErrOutpostUnsupported) && t.server.cfg.GetToolMode() == config.ToolModeAuto {
				started := false
				if streamProgress != nil {
					started, _ = streamProgress()
				}
				if !started {
					log.Printf("chat %s outpost unsupported (%v); falling back to text_bridge", t.id, err)
					t.nativeOutpostFallback = true
					t.nativeOutpostActive = false
					if dErr := t.detachFromNativeOutpostForTextBridge(); dErr != nil {
						return res, dErr
					}
					if resetOutput != nil {
						resetOutput()
					}
					attemptIn = t.in
					attemptIn.OnDispatched = t.markUpstreamWaiting
					res, err = t.server.upstream.Chat(t.ctx, sess, attemptIn, onEvent)
				}
			}
		} else {
			if dErr := t.detachFromNativeOutpostForTextBridge(); dErr != nil {
				return res, dErr
			}
			attemptIn = t.in
			attemptIn.OnDispatched = t.markUpstreamWaiting
			res, err = t.server.upstream.Chat(t.ctx, sess, attemptIn, onEvent)
		}
		t.clearUpstreamWaiting()
		attemptDuration := time.Since(attemptStarted)
		if err == nil {
			text, toolCalls := t.responseOutput(res)
			if completeOutput != nil {
				toolCalls, err = completeOutput(res)
			}
			if err == nil {
				err = t.validateToolChoice(toolCalls)
			}
			if err == nil && !hasVisibleText(text) && len(toolCalls) == 0 && !hasVisibleText(res.Thinking) {
				err = fmt.Errorf("%w (completion_tokens=%d)", ErrEmptyResponse, res.Usage.CompletionTokens)
			}
		}
		t.server.accounts.RecordCall(t.ctx, acct.ID, accounts.CallUsage{
			Duration:         attemptDuration,
			PromptTokens:     res.Usage.PromptTokens,
			CompletionTokens: res.Usage.CompletionTokens,
			TotalTokens:      res.Usage.TotalTokens,
			CacheReadTokens:  res.Usage.CacheReadTokens,
			CacheWriteTokens: res.Usage.CacheWriteTokens,
			CostUSD:          res.Usage.CostUSD,
			Credits:          res.Usage.Credits,
			Success:          err == nil,
		})
		if err == nil {
			return res, nil
		}
		lastErr = err
		blockedRemoteTool := errors.Is(err, ErrRemoteSandboxToolBlocked)
		if !invalidChatRequest(err) && !blockedRemoteTool && t.ctx.Err() == nil {
			t.server.accounts.ReportFailure(t.ctx, acct.ID, err)
		}
		log.Printf("chat %s attempt=%d/%d account=%s failed error=%v", t.id, t.attempts, t.maxAttempts, accountLogLabel(acct), err)
		if streamProgress != nil {
			if started, hasOutput := streamProgress(); started && (!blockedRemoteTool || hasOutput) {
				log.Printf("chat %s stream already started; not retrying", t.id)
				return res, err
			}
		}
		if t.ctx.Err() != nil {
			return res, t.ctx.Err()
		}
		if blockedRemoteTool {
			if t.explicitThreadID != "" {
				return res, err
			}
			if !t.remoteToolCorrected {
				t.remoteToolCorrected = true
				blockedName := "remote_tool"
				var rErr *RemoteToolBlockedError
				if errors.As(err, &rErr) && strings.TrimSpace(rErr.ToolName) != "" {
					blockedName = strings.TrimSpace(rErr.ToolName)
				}
				if swErr := t.switchToFullTranscript(); swErr != nil {
					return res, swErr
				}
				toolSummaryHint := ""
				if toolSummary := formatToolNamesSummary(collectToolNames(t.req), 480); toolSummary != "" {
					toolSummaryHint = fmt.Sprintf(" Available local tools: %s.", toolSummary)
				}
				correctionHint := fmt.Sprintf("\n\n[IMPORTANT CORRECTION: Your previous attempt invoked a blocked remote cloud tool (%q) and was terminated. Remote cloud sandbox, web search, download, shell, and file-write tools are strictly disabled.%s Perform the required action on the user's local machine by emitting <client_tool_call> from <client_tool_protocol> right now, or answer in plain text without invoking remote tools.]", blockedName, toolSummaryHint)
				t.in.Prompt += correctionHint
				if strings.TrimSpace(t.in.CurrentTurn) != "" {
					t.in.CurrentTurn += correctionHint
				}
				t.preferredAccount = acct.ID
				forceCorrection = true
				log.Printf("chat %s auto-correcting blocked remote tool %q in fresh thread", t.id, blockedName)
				continue
			}
			return res, err
		}
		action := retryActionFor(err)
		if isRateLimitTemporary(err) {
			// 对明确限流账号写入跨请求短暂冷却并屏蔽本轮再选，避免新请求反复命中同一限流账号。
			t.server.accounts.MarkCooldown(acct.ID, rateLimitCooldownFor(err))
			t.blockedAccounts[acct.ID] = true
		}
		if action == retryStop || t.attempts >= t.maxAttempts {
			return res, err
		}
		if t.explicitThreadID != "" {
			switch action {
			case retryRefresh:
				if t.refreshedAccounts[acct.ID] {
					return res, err
				}
				t.refreshedAccounts[acct.ID] = true
				refreshed, refreshErr := t.server.accounts.RefreshOne(t.ctx, acct.ID)
				if refreshErr != nil || refreshed.Disabled {
					return res, err
				}
			case retryTemporary:
				if !t.resume || t.sameAccountRetried[acct.ID] || isRateLimitTemporary(err) {
					return res, err
				}
				t.sameAccountRetried[acct.ID] = true
			default:
				return res, err
			}
			t.preferredAccount = acct.ID
			continue
		}
		wasResume := t.resume
		switch action {
		case retryRefresh:
			if t.refreshedAccounts[acct.ID] {
				if rErr := t.resetConversation(); rErr != nil {
					return res, rErr
				}
				t.blockedAccounts[acct.ID] = true
				break
			}
			t.refreshedAccounts[acct.ID] = true
			refreshed, refreshErr := t.server.accounts.RefreshOne(t.ctx, acct.ID)
			if refreshErr == nil && !refreshed.Disabled {
				// 刷新同一账号 Token 成功时保留既有 thread_id 与增量上下文，不清空会话
				t.preferredAccount = acct.ID
			} else {
				if rErr := t.resetConversation(); rErr != nil {
					return res, rErr
				}
				t.blockedAccounts[acct.ID] = true
			}
		case retryNextAccount:
			if rErr := t.resetConversation(); rErr != nil {
				return res, rErr
			}
			t.blockedAccounts[acct.ID] = true
		case retryNewSession:
			if rErr := t.resetConversation(); rErr != nil {
				return res, rErr
			}
			if wasResume {
				t.preferredAccount = acct.ID
				log.Printf("chat %s session resume failed; retrying with full transcript", t.id)
			} else {
				t.blockedAccounts[acct.ID] = true
			}
		case retryTemporary:
			if wasResume {
				if !t.sameAccountRetried[acct.ID] && !isRateLimitTemporary(err) {
					// 续聊遇网络超时/临时故障时优先在原绑定账号原地重连 1 次，尽力保住云端既有上下文缓存。
					t.sameAccountRetried[acct.ID] = true
					t.preferredAccount = acct.ID
					log.Printf("chat %s temporary error on resumed account %s; retrying same account once to preserve cache", t.id, accountLogLabel(acct))
				} else {
					// 原账号已重连失败或命中 429 限流，切往其他账号前先切换为完整历史与完整工具表重建。
					if swErr := t.switchToFullTranscript(); swErr != nil {
						return res, swErr
					}
					log.Printf("chat %s temporary error after same-account retry or rate limit; switching account with full transcript", t.id)
				}
			}
		}
	}
	return res, lastErr
}

// finishTurn 记录终端标准日志并异步安全地把本次请求明细写入 SQLite request_logs 表。
func (t *preparedTurn) finishTurn(status, finish string, res ChatResult, toolCalls int, err error) {
	logChatDone(t.id, t.startedAt, t.in, t.usedAccount, t.attempts, status, finish, res.Usage, toolCalls, err)
	if t.server.store == nil {
		return
	}
	var acctID int64
	var acctEmail string
	if t.usedAccount != nil {
		acctID = t.usedAccount.ID
		acctEmail = strings.TrimSpace(t.usedAccount.Email)
		if acctEmail == "" {
			acctEmail = strings.TrimSpace(t.usedAccount.DisplayName)
		}
	}
	threadID := strings.TrimSpace(res.ThreadID)
	if threadID == "" {
		threadID = strings.TrimSpace(t.in.ThreadID)
	}
	var costUSD float64
	if res.Usage.CostUSD != nil {
		costUSD = *res.Usage.CostUSD
	}
	totalTok := res.Usage.TotalTokens
	if totalTok == 0 {
		totalTok = res.Usage.PromptTokens + res.Usage.CacheReadTokens + res.Usage.CacheWriteTokens + res.Usage.CompletionTokens
	}
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	entry := store.RequestLog{
		RequestID:        t.id,
		CreatedAt:        t.startedAt.Unix(),
		Protocol:         t.protocol,
		Endpoint:         t.endpoint,
		RequestedModel:   t.requestedModel,
		UpstreamModel:    t.model,
		ResolvedModel:    resolvedResponseModel(t.model, res.Usage),
		ThinkingEffort:   t.in.ThinkingEffort,
		Stream:           t.req.Stream,
		AccountID:        acctID,
		AccountEmail:     acctEmail,
		ThreadID:         threadID,
		SessionReused:    t.resume,
		Status:           status,
		FinishReason:     finish,
		ErrorMessage:     errMsg,
		Attempts:         max(t.attempts, 1),
		DurationMS:       time.Since(t.startedAt).Milliseconds(),
		PromptTokens:     res.Usage.PromptTokens,
		CompletionTokens: res.Usage.CompletionTokens,
		TotalTokens:      totalTok,
		CacheReadTokens:  res.Usage.CacheReadTokens,
		CacheWriteTokens: res.Usage.CacheWriteTokens,
		CostUSD:          costUSD,
		ToolCallsCount:   toolCalls,
		ImagesCount:      len(t.in.Images),
		PromptPreview:    requestLogPreview(t.req.Messages),
	}
	dbCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if insErr := t.server.store.InsertRequestLog(dbCtx, entry); insErr != nil {
		log.Printf("request_log insert failed id=%s: %v", t.id, insErr)
	}
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	if !s.tokens.Allowed(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid proxy API key")
		return
	}

	var req ChatRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20))
	if err := dec.Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if req.N > 1 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "n > 1 is not supported")
		return
	}

	turn, err := s.prepareTurn(r, &req, "openai", "/v1/chat/completions", "chatcmpl-")
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		status, code := chatErrorStatus(err)
		writeOpenAIError(w, status, code, err.Error())
		return
	}
	defer turn.Close()

	if !req.Stream {
		res, lastErr := turn.runChat(nil, nil, nil, nil)
		if lastErr != nil {
			turn.resetConversationAfterFailure(lastErr)
			status, code := chatErrorStatus(lastErr)
			turn.finishTurn("failed", "", res, 0, lastErr)
			writeCooldownRetryHeader(w, lastErr)
			writeOpenAIError(w, status, code, lastErr.Error())
			return
		}
		text, toolCalls := turn.responseOutput(res)
		visibleOut := ""
		if hasVisibleText(text) {
			visibleOut = text
		}
		if turn.ctx.Err() == nil {
			turn.saveConversation(res, visibleOut, toolCalls)
		}
		message := map[string]any{"role": "assistant"}
		if visibleOut == "" {
			message["content"] = nil
		} else {
			message["content"] = visibleOut
		}
		if strings.TrimSpace(res.Thinking) != "" {
			message["reasoning_content"] = res.Thinking
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		}
		finish := "stop"
		if len(toolCalls) > 0 {
			finish = "tool_calls"
		}
		turn.finishTurn("ok", finish, res, len(toolCalls), nil)
		respModel := resolvedResponseModel(turn.model, res.Usage)
		if resolved := strings.TrimSpace(res.Usage.ResolvedModelID); resolved != "" {
			w.Header().Set("X-Resolved-Model", resolved)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      turn.id,
			"object":  "chat.completion",
			"created": turn.created,
			"model":   respModel,
			"choices": []map[string]any{{
				"index":         0,
				"message":       message,
				"finish_reason": finish,
			}},
			"usage": usageMap(res.Usage),
		})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming_unavailable", "response writer does not support flushing")
		return
	}
	var writeMu sync.Mutex
	started := false
	hasOutput := false
	var lastWriteAt time.Time
	var clientWriteErr error
	noteWriteErrLocked := func(wErr error) bool {
		if wErr == nil {
			return false
		}
		if clientWriteErr == nil {
			clientWriteErr = wErr
			if turn.cancel != nil {
				turn.cancel()
			}
		}
		return true
	}
	getClientWriteErr := func() error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return clientWriteErr
	}
	writeRawLocked := func(payload map[string]any) {
		if clientWriteErr != nil {
			return
		}
		body, _ := json.Marshal(payload)
		if _, wErr := fmt.Fprintf(w, "data: %s\n\n", body); noteWriteErrLocked(wErr) {
			return
		}
		flusher.Flush()
		lastWriteAt = time.Now()
	}
	ensureStartedLocked := func() {
		if started || clientWriteErr != nil {
			return
		}
		started = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		writeRawLocked(map[string]any{
			"id":      turn.id,
			"object":  "chat.completion.chunk",
			"created": turn.created,
			"model":   turn.model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{"role": "assistant"},
				"finish_reason": nil,
			}},
		})
	}
	writePayload := func(payload map[string]any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		ensureStartedLocked()
		writeRawLocked(payload)
	}
	streamProgress := func() (bool, bool) {
		writeMu.Lock()
		defer writeMu.Unlock()
		return started, hasOutput
	}
	isStarted := func() bool {
		started, _ := streamProgress()
		return started
	}
	lastWrite := func() time.Time {
		writeMu.Lock()
		defer writeMu.Unlock()
		return lastWriteAt
	}
	sendKeepalive := func() {
		writeMu.Lock()
		defer writeMu.Unlock()
		if !started || clientWriteErr != nil {
			return
		}
		if _, wErr := fmt.Fprint(w, ": keepalive\n\n"); noteWriteErrLocked(wErr) {
			return
		}
		flusher.Flush()
		lastWriteAt = time.Now()
	}
	writeChunk := func(delta map[string]any, finishReason *string) {
		writeMu.Lock()
		defer writeMu.Unlock()
		ensureStartedLocked()
		hasOutput = true
		writeRawLocked(map[string]any{
			"id":      turn.id,
			"object":  "chat.completion.chunk",
			"created": turn.created,
			"model":   turn.model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         delta,
				"finish_reason": finishReason,
			}},
		})
	}
	var thinkBuf prestreamThinkingBuf
	flushPendingThinking := func() {
		if pending := thinkBuf.Drain(); pending != "" {
			writeChunk(map[string]any{"reasoning_content": pending}, nil)
		}
	}
	prestartSSE := func() {
		writeMu.Lock()
		defer writeMu.Unlock()
		ensureStartedLocked()
	}
	writeStreamError := func(err error) {
		_, code := chatErrorStatus(err)
		writePayload(map[string]any{"error": map[string]any{
			"message": err.Error(),
			"type":    code,
			"code":    code,
		}})
	}

	sawVisibleText := false
	var pendingText strings.Builder
	var finalOutText strings.Builder
	var finalOutTools []ToolCall
	var bridgeStream *toolBridgeStream
	if turn.bridgeActive {
		bridgeStream = newToolBridgeStream(turn.allowedTools, turn.choiceMode, turn.choiceName, turn.parallelTools)
	}
	emittedToolIdx := 0
	startedTools := make(map[string]int)

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
		flushPendingThinking()
		writeChunk(map[string]any{"content": chunk}, nil)
		if getClientWriteErr() == nil {
			finalOutText.WriteString(chunk)
		}
	}
	emitToolStart := func(ts ToolCallStart) {
		if ts.ID == "" || ts.Name == "" {
			return
		}
		if _, exists := startedTools[ts.ID]; exists {
			return
		}
		flushPendingThinking()
		idx := emittedToolIdx
		emittedToolIdx++
		startedTools[ts.ID] = idx
		writeChunk(map[string]any{"tool_calls": []map[string]any{{
			"index": idx,
			"id":    ts.ID,
			"type":  "function",
			"function": map[string]any{
				"name":      ts.Name,
				"arguments": "",
			},
		}}}, nil)
	}
	emitToolReply := func(tc ToolCallReply) {
		flushPendingThinking()
		if idx, ok := startedTools[tc.ID]; ok {
			delete(startedTools, tc.ID)
			writeChunk(map[string]any{"tool_calls": []map[string]any{{
				"index": idx,
				"function": map[string]any{
					"arguments": tc.Arguments,
				},
			}}}, nil)
		} else {
			writeChunk(map[string]any{"tool_calls": []map[string]any{{
				"index": emittedToolIdx,
				"id":    tc.ID,
				"type":  "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": tc.Arguments,
				},
			}}}, nil)
			emittedToolIdx++
		}
		if getClientWriteErr() == nil {
			finalOutTools = append(finalOutTools, ToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: FunctionCall{
					Name:      tc.Name,
					Arguments: tc.Arguments,
				},
			})
		}
	}
	quickFilter := newQuickOutputFilter(turn.allowedTools, func(tc ToolCallReply) bool {
		return !turn.nativeOutpostActive && turn.bridgeActive && emittedToolIdx == 0 && bridgeStream.acceptCall(tc)
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
		turn.upstreamWaitingSince,
		prestartSSE,
		sendKeepalive,
		lastWrite,
	)
	defer stopHB()

	res, lastErr := turn.runChat(func(ev StreamEvent) error {
		switch ev.Type {
		case "thinking":
			if ev.Text != "" {
				if _, outputStarted := streamProgress(); turn.bridgeActive && !outputStarted {
					if flushed, shouldFlush := thinkBuf.Add(ev.Text, thinkingPrestreamMaxBytes, thinkingPrestreamHold); shouldFlush {
						writeChunk(map[string]any{"reasoning_content": flushed}, nil)
					}
				} else {
					flushPendingThinking()
					writeChunk(map[string]any{"reasoning_content": ev.Text}, nil)
				}
			}
		case "text":
			if ev.Text == "" {
				return nil
			}
			if !turn.bridgeActive || turn.nativeOutpostActive {
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
			if ts := bridgeStream.PeekPendingStart(); ts != nil {
				emitToolStart(*ts)
			}
		case "tool_call":
			if turn.nativeOutpostActive && ev.ToolCall != nil {
				emitToolReply(ToolCallReply{
					ID:        ev.ToolCall.ID,
					Name:      ev.ToolCall.Name,
					Arguments: ev.ToolCall.Arguments,
				})
			}
		}
		return getClientWriteErr()
	}, streamProgress, func() {
		sawVisibleText = false
		pendingText.Reset()
		thinkBuf.Reset()
		finalOutText.Reset()
		finalOutTools = nil
		emittedToolIdx = 0
		clear(startedTools)
		if bridgeStream != nil {
			bridgeStream.Reset()
		}
		quickFilter.Reset()
	}, func(res ChatResult) ([]ToolCall, error) {
		if err := turn.ctx.Err(); err != nil {
			return nil, err
		}
		if err := getClientWriteErr(); err != nil {
			return nil, err
		}
		if turn.bridgeActive {
			if !turn.nativeOutpostActive {
				remText, remCalls := bridgeStream.Flush()
				if remText != "" {
					emitFilteredText(remText)
				}
				for _, tc := range remCalls {
					emitToolReply(tc)
				}
			}
			fText, fCalls := quickFilter.Flush()
			if fText != "" {
				emitText(fText)
			}
			for _, tc := range fCalls {
				emitToolReply(tc)
			}
			if emittedToolIdx == 0 && len(res.ToolCalls) > 0 {
				for _, tc := range limitToolReplies(nativeToolReplies(res.ToolCalls, turn.allowedTools), turn.choiceMode, turn.choiceName, turn.parallelTools) {
					emitToolReply(tc)
				}
			}
			if !sawVisibleText && emittedToolIdx == 0 && hasVisibleText(res.Text) {
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
		return finalOutTools, getClientWriteErr()
	})
	stopHB()
	if lastErr != nil {
		turn.resetConversationAfterFailure(lastErr)
		if wErr := getClientWriteErr(); wErr != nil {
			turn.finishTurn("failed", "", res, 0, wErr)
			return
		}
		if !isStarted() {
			status, code := chatErrorStatus(lastErr)
			turn.finishTurn("failed", "", res, 0, lastErr)
			writeCooldownRetryHeader(w, lastErr)
			writeOpenAIError(w, status, code, lastErr.Error())
			return
		}
		turn.finishTurn("failed", "", res, 0, lastErr)
		writeStreamError(lastErr)
		if !errors.Is(lastErr, ErrOutpostToolPolicyMismatch) {
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
		}
		return
	}
	finish := "stop"
	if emittedToolIdx > 0 {
		finish = "tool_calls"
	}
	flushPendingThinking()
	if getClientWriteErr() == nil && turn.ctx.Err() == nil {
		turn.saveConversation(res, finalOutText.String(), finalOutTools)
	} else {
		errOut := getClientWriteErr()
		if errOut == nil {
			errOut = turn.ctx.Err()
		}
		turn.finishTurn("failed", "", res, emittedToolIdx, errOut)
		return
	}
	finalModel := resolvedResponseModel(turn.model, res.Usage)
	writePayload(map[string]any{
		"id":      turn.id,
		"object":  "chat.completion.chunk",
		"created": turn.created,
		"model":   finalModel,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": finish,
		}},
	})
	turn.finishTurn("ok", finish, res, emittedToolIdx, nil)
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		writePayload(map[string]any{
			"id":      turn.id,
			"object":  "chat.completion.chunk",
			"created": turn.created,
			"model":   finalModel,
			"choices": []any{},
			"usage":   usageMap(res.Usage),
		})
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// accountLogLabel identifies an account in request logs without exposing
// tokens. Format: id:provider:email.
func accountLogLabel(a *store.Account) string {
	if a == nil {
		return "none"
	}
	name := strings.TrimSpace(a.Email)
	if name == "" {
		name = strings.TrimSpace(a.DisplayName)
	}
	if name == "" {
		name = "-"
	}
	return fmt.Sprintf("%d:%s:%s", a.ID, a.Provider, name)
}

// logChatDone emits one completion line per chat request with the outcome,
// selected account, mapped model, token usage, finish reason, and error.
func logChatDone(id string, startedAt time.Time, in ChatInput, acct *store.Account, attempts int, status, finish string, u Usage, toolCalls int, err error) {
	if finish == "" {
		finish = "-"
	}
	resolvedSuffix := ""
	if resolved := strings.TrimSpace(u.ResolvedModelID); resolved != "" {
		resolvedSuffix = " resolved_model=" + resolved
	}
	line := fmt.Sprintf("chat %s done status=%s account=%s model=%s%s attempts=%d elapsed=%s tokens{prompt=%d completion=%d total=%d cache_read=%d cache_write=%d} finish=%s tool_calls=%d",
		id, status, accountLogLabel(acct), in.Model, resolvedSuffix, attempts, time.Since(startedAt).Round(time.Millisecond),
		u.PromptTokens, u.CompletionTokens, u.TotalTokens, u.CacheReadTokens, u.CacheWriteTokens, finish, toolCalls)
	if err != nil {
		log.Printf("%s error=%v", line, err)
		return
	}
	log.Print(line)
}

// resolvedResponseModel 优先返回上游 turn_usage 报告的实际底层模型代号，缺失时回退为请求模型别名。
func resolvedResponseModel(fallback string, u Usage) string {
	if m := strings.TrimSpace(u.ResolvedModelID); m != "" {
		return m
	}
	return fallback
}

func (s *Server) maxAttempts() int {
	max := s.cfg.MaxAttempts
	if max < 1 {
		max = 1
	}
	return max
}

func (s *Server) mapBuiltinOrModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.TrimPrefix(m, "quickwork/")
	m = strings.TrimPrefix(m, "qw-")
	switch m {
	case "smart", "fast", "balanced", "auto":
		return m
	case "":
		return s.cfg.DefaultModel
	default:
		log.Printf("unknown model %q, using %s", model, s.cfg.DefaultModel)
		return s.cfg.DefaultModel
	}
}

func (s *Server) mapModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.TrimPrefix(m, "quickwork/")
	m = strings.TrimPrefix(m, "qw-")
	switch m {
	case "smart", "fast", "balanced", "auto":
		return m
	case "":
		return s.cfg.DefaultModel
	default:
		if target, ok := s.cfg.LookupModelAlias(m); ok {
			return s.mapBuiltinOrModel(target)
		}
		log.Printf("unknown model %q, using %s", model, s.cfg.DefaultModel)
		return s.cfg.DefaultModel
	}
}

func usageMap(u Usage) map[string]any {
	completion := u.CompletionTokens
	promptDirect, cacheRead, cacheWrite := u.ContextSnapshot()
	prompt := promptDirect + cacheRead + cacheWrite
	total := prompt + completion
	m := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
	}
	if cacheRead > 0 {
		m["prompt_tokens_details"] = map[string]any{"cached_tokens": cacheRead}
	}
	return m
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    code,
			"code":    code,
		},
	})
}
