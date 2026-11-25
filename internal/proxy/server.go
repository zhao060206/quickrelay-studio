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
	"time"

	"quickrelay-studio/internal/accounts"
	"quickrelay-studio/internal/config"
	"quickrelay-studio/internal/store"
	"quickrelay-studio/internal/version"
)

var (
	sseKeepaliveInterval = 15 * time.Second
	maxThinkingPrestart  = 20 * time.Second
)

type Server struct {
	cfg      *config.Config
	accounts *accounts.Manager
	store    *store.Store
	tokens   *APIKeyAuth
	upstream *Upstream
	sessions *conversationStore
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
	}
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
	server *Server
	ctx    context.Context
	cancel context.CancelFunc
	conv   *conversation

	id             string
	created        int64
	protocol       string
	endpoint       string
	req            *ChatRequest
	model          string
	requestedModel string

	allowedTools  map[string]bool
	choiceMode    string
	choiceName    string
	parallelTools bool
	bridgeActive  bool
	baseHash      string
	fullGuide     string
	fullPrompt    string
	fullImages    []ImageAttachment
	fullLoaded    bool
	fallbackText  string

	in                     ChatInput
	requestedThinkingEffort string
	resume                 bool
	startedAt   time.Time
	attempts    int
	maxAttempts int
	usedAccount *store.Account

	blockedAccounts   map[int64]bool
	attemptedAccounts map[int64]bool
	refreshedAccounts map[int64]bool
	pinnedFirst       bool
	preferredAccount  int64
}

// needsThinkingHeartbeat 判断当前轮次是否需要开启长思考提前预热与 SSE 保活心跳。
func (t *preparedTurn) needsThinkingHeartbeat() bool {
	if t == nil {
		return false
	}
	eff := strings.TrimSpace(t.in.ThinkingEffort)
	reqEff := strings.TrimSpace(t.requestedThinkingEffort)
	return strings.EqualFold(eff, "max") || strings.EqualFold(reqEff, "max")
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

func startSSEHeartbeat(
	ctx context.Context,
	isMaxThinking bool,
	isStarted func() bool,
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
		prestartDelay = 20 * time.Second
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
					if isMaxThinking && time.Since(beginAt) >= prestartDelay {
						prestart()
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
	prompt, err := formatPrompt(req.Messages)
	if err != nil {
		return nil, err
	}

	var model string
	if protocol == "anthropic" {
		model = s.mapAnthropicModel(req.Model)
	} else {
		model = s.mapModel(req.Model)
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
	bridgePrompt := buildToolBridgePrompt(req, choiceMode, choiceName, parallelTools)
	bridgeActive := bridgePrompt != ""
	leadingGuide := leadingGuidelines(req.Messages)
	fullGuide := callerGuidelines(req.Messages)
	baseHash := conversationBaseHash(leadingGuide, bridgePrompt)

	rawThinkingEffort := resolveThinkingEffort(req, s.cfg.ThinkingEffort)
	thinkingEffort := rawThinkingEffort
	isCompact := isCompactionRequest(req, prompt)
	if guardedEffort, guardReason := applyThinkingGuards(thinkingEffort, isExplicitThinkingOff(req), isCompact, bridgeActive, choiceMode, s.cfg); guardReason != "" {
		log.Printf("chat %s thinking_guard reason=%s effort=%q->%q bridge=%t choice=%q",
			idPrefix, guardReason, thinkingEffort, guardedEffort, bridgeActive, choiceMode)
		thinkingEffort = guardedEffort
	}
	turnTimeout := s.turnTimeoutFor(thinkingEffort)
	ctx, cancel := context.WithTimeout(r.Context(), turnTimeout)
	conv := s.sessions.acquire(conversationKey(r, req, baseHash))

	t := &preparedTurn{
		server:            s,
		ctx:               ctx,
		cancel:            cancel,
		conv:              conv,
		id:                idPrefix + strings.ReplaceAll(uuid(), "-", ""),
		created:           time.Now().Unix(),
		protocol:          protocol,
		endpoint:          endpoint,
		req:               req,
		model:             model,
		allowedTools:      allowedTools,
		choiceMode:        choiceMode,
		choiceName:        choiceName,
		parallelTools:     parallelTools,
		bridgeActive:      bridgeActive,
		baseHash:          baseHash,
		fullGuide:               fullGuide,
		fallbackText:            prompt,
		requestedThinkingEffort: rawThinkingEffort,
		startedAt:               time.Now(),
		maxAttempts:       s.maxAttempts(),
		blockedAccounts:   make(map[int64]bool),
		attemptedAccounts: make(map[int64]bool),
		refreshedAccounts: make(map[int64]bool),
	}

	promptText := ""
	turnGuidelines := ""
	var images []ImageAttachment
	if s.cfg.SessionReuse && conv.baseHash == baseHash {
		if delta, ok := conv.delta(req.Messages); ok {
			deltaGuide := deltaDeveloperGuidelines(delta)
			if deltaPrompt, deltaImages, dErr := formatPromptWithImages(ctx, delta); dErr == nil && (hasVisibleText(deltaPrompt) || len(deltaImages) > 0 || deltaGuide != "") {
				promptText = deltaPrompt
				images = deltaImages
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
		images = t.fullImages
		turnGuidelines = fullGuide
	}

	threadID := strings.TrimSpace(r.Header.Get("X-Thread-Id"))
	t.in = ChatInput{
		Prompt:         promptText,
		LatestUserText: latestTurnPreview(req.Messages),
		Images:         images,
		Model:          model,
		ThinkingEffort: thinkingEffort,
		ThreadID:       threadID,
		Title:          titleFor(lastUserText(req.Messages)),
		Guidelines:     turnGuidelines,
		ToolProtocol:   bridgePrompt,
		ChatAgentID:    s.cfg.ChatAgentID,
		CreateSession:  true,
		DeleteSession:  !s.cfg.SessionReuse && !s.cfg.KeepSessions,
	}
	if t.resume {
		t.in.ThreadID = conv.threadID
		t.in.CreateSession = false
	}
	t.pinnedFirst = t.resume && conv.accountID != 0

	t.requestedModel = strings.TrimSpace(req.Model)
	if t.requestedModel == "" {
		t.requestedModel = "(default)"
	}
	turnSystemChars := len(t.in.Guidelines) + len(t.in.ToolProtocol)
	log.Printf("chat %s start proto=%s model=%s upstream_model=%s thinking_effort=%q timeout=%s stream=%t messages=%d images=%d prompt_chars=%d system_chars=%d reuse=%t thread=%q",
		t.id, protocol, t.requestedModel, model, thinkingEffort, turnTimeout, req.Stream, len(req.Messages), len(t.in.Images), len(promptText), turnSystemChars, t.resume, t.in.ThreadID)

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
	p, imgs, err := formatPromptWithImages(t.ctx, t.req.Messages)
	if err != nil {
		return err
	}
	t.fullPrompt = p
	t.fullImages = imgs
	t.fullLoaded = true
	return nil
}

func (t *preparedTurn) resetConversation() {
	t.server.sessions.reset(t.conv)
	t.resume = false
	t.in.ThreadID = ""
	t.in.CreateSession = true
	t.in.Guidelines = t.fullGuide
	if fErr := t.loadFull(); fErr != nil {
		log.Printf("chat %s reset conversation image load failed: %v", t.id, fErr)
		t.in.Prompt = t.fallbackText
		t.in.Images = nil
		return
	}
	t.in.Prompt = t.fullPrompt
	t.in.Images = t.fullImages
}

func (t *preparedTurn) saveConversation(res ChatResult) {
	if t.server.cfg.SessionReuse && res.ThreadID != "" && t.usedAccount != nil {
		t.server.sessions.save(t.conv, res.ThreadID, t.usedAccount.ID, t.baseHash, t.req.Messages)
	}
}

func (t *preparedTurn) responseOutput(result ChatResult) (string, []ToolCall) {
	text := result.Text
	var replies []ToolCallReply
	if t.bridgeActive {
		text, replies = parseClientToolCalls(text, t.allowedTools)
		if len(replies) == 0 {
			replies = nativeToolReplies(result.ToolCalls, t.allowedTools)
		}
	}
	var uiCalls []ToolCallReply
	text, uiCalls = cleanQuickOutput(text, t.allowedTools, func(tc ToolCallReply) bool {
		return t.bridgeActive && len(replies) == 0 && (t.choiceMode != "function" || t.choiceName == "" || tc.Name == t.choiceName)
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

func (t *preparedTurn) selectAccount() (*store.Account, *Session, error) {
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
				t.blockedAccounts[accountID] = true
				log.Printf("chat %s preferred account %d unavailable; selecting another account", t.id, accountID)
				t.resetConversation()
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
				return nil, nil, selectErr
			}
		}
		sess := SessionFromAccount(acct, t.server.cfg)
		if err := sess.validate(); err != nil {
			t.blockedAccounts[acct.ID] = true
			t.server.accounts.ReportFailure(t.ctx, acct.ID, err)
			log.Printf("chat %s account=%s invalid_session error=%v", t.id, accountLogLabel(acct), err)
			if t.resume {
				t.resetConversation()
			}
			continue
		}
		return acct, sess, nil
	}
}

func (t *preparedTurn) runChat(onEvent EventFunc, streamStarted func() bool, resetOutput func()) (ChatResult, error) {
	var res ChatResult
	var lastErr error
	for t.attempts < t.maxAttempts {
		if t.attempts > 0 {
			if err := waitForRetry(t.ctx, t.attempts); err != nil {
				return res, err
			}
		}
		acct, sess, err := t.selectAccount()
		if err != nil {
			if lastErr != nil && errors.Is(err, accounts.ErrNoAvailableAccounts) {
				return res, lastErr
			}
			return res, err
		}
		if resetOutput != nil {
			resetOutput()
		}
		t.usedAccount = acct
		t.attemptedAccounts[acct.ID] = true
		t.attempts++
		log.Printf("chat %s attempt=%d/%d account=%s", t.id, t.attempts, t.maxAttempts, accountLogLabel(acct))
		attemptStarted := time.Now()
		res, err = t.server.upstream.Chat(t.ctx, sess, t.in, onEvent)
		attemptDuration := time.Since(attemptStarted)
		if err == nil {
			text, toolCalls := t.responseOutput(res)
			if !hasVisibleText(text) && len(toolCalls) == 0 && !hasVisibleText(res.Thinking) {
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
		if !invalidChatRequest(err) && t.ctx.Err() == nil {
			t.server.accounts.ReportFailure(t.ctx, acct.ID, err)
		}
		log.Printf("chat %s attempt=%d/%d account=%s failed error=%v", t.id, t.attempts, t.maxAttempts, accountLogLabel(acct), err)
		if streamStarted != nil && streamStarted() {
			log.Printf("chat %s stream already started; not retrying", t.id)
			return res, err
		}
		if t.ctx.Err() != nil {
			return res, t.ctx.Err()
		}
		action := retryActionFor(err)
		if action == retryStop || t.attempts >= t.maxAttempts {
			return res, err
		}
		wasResume := t.resume
		t.resetConversation()
		switch action {
		case retryRefresh:
			if t.refreshedAccounts[acct.ID] {
				t.blockedAccounts[acct.ID] = true
				break
			}
			t.refreshedAccounts[acct.ID] = true
			refreshed, refreshErr := t.server.accounts.RefreshOne(t.ctx, acct.ID)
			if refreshErr == nil && !refreshed.Disabled {
				t.preferredAccount = acct.ID
			} else {
				t.blockedAccounts[acct.ID] = true
			}
		case retryNextAccount:
			t.blockedAccounts[acct.ID] = true
		case retryNewSession:
			if wasResume {
				t.preferredAccount = acct.ID
				log.Printf("chat %s session resume failed; retrying with full transcript", t.id)
			} else {
				t.blockedAccounts[acct.ID] = true
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
		PromptPreview:    lastUserText(t.req.Messages),
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
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	defer turn.Close()

	if !req.Stream {
		res, lastErr := turn.runChat(nil, nil, nil)
		if lastErr != nil {
			s.sessions.reset(turn.conv)
			status, code := chatErrorStatus(lastErr)
			turn.finishTurn("failed", "", res, 0, lastErr)
			writeOpenAIError(w, status, code, lastErr.Error())
			return
		}
		turn.saveConversation(res)
		text, toolCalls := turn.responseOutput(res)
		message := map[string]any{"role": "assistant"}
		if !hasVisibleText(text) {
			message["content"] = nil
		} else {
			message["content"] = text
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
	var lastWriteAt time.Time
	writeRawLocked := func(payload map[string]any) {
		body, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", body)
		flusher.Flush()
		lastWriteAt = time.Now()
	}
	ensureStartedLocked := func() {
		if started {
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
	prestartSSE := func() {
		writeMu.Lock()
		defer writeMu.Unlock()
		ensureStartedLocked()
	}
	sendKeepalive := func() {
		writeMu.Lock()
		defer writeMu.Unlock()
		if !started {
			return
		}
		_, _ = fmt.Fprint(w, ": keepalive\n\n")
		flusher.Flush()
		lastWriteAt = time.Now()
	}
	writeChunk := func(delta map[string]any, finishReason *string) {
		writePayload(map[string]any{
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
	writeStreamError := func(message string) {
		writePayload(map[string]any{"error": map[string]any{
			"message": message,
			"type":    "upstream_error",
			"code":    "upstream_error",
		}})
	}

	sawVisibleText := false
	var pendingText strings.Builder
	var bridgeStream *toolBridgeStream
	if turn.bridgeActive {
		bridgeStream = newToolBridgeStream(turn.allowedTools, turn.choiceMode, turn.choiceName, turn.parallelTools)
	}
	emittedToolIdx := 0

	emitText := func(chunk string) {
		if chunk == "" {
			return
		}
		if !sawVisibleText {
			pendingText.WriteString(chunk)
			if !hasVisibleText(pendingText.String()) {
				return
			}
			sawVisibleText = true
			writeChunk(map[string]any{"content": pendingText.String()}, nil)
			pendingText.Reset()
			return
		}
		writeChunk(map[string]any{"content": chunk}, nil)
	}
	emitToolReply := func(tc ToolCallReply) {
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
	quickFilter := newQuickOutputFilter(turn.allowedTools, func(tc ToolCallReply) bool {
		return turn.bridgeActive && emittedToolIdx == 0 && bridgeStream.acceptCall(tc)
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
		prestartSSE,
		sendKeepalive,
		lastWrite,
	)
	defer stopHB()

	res, lastErr := turn.runChat(func(ev StreamEvent) error {
		switch ev.Type {
		case "thinking":
			if ev.Text != "" {
				writeChunk(map[string]any{"reasoning_content": ev.Text}, nil)
			}
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
		emittedToolIdx = 0
		if bridgeStream != nil {
			bridgeStream.Reset()
		}
		quickFilter.Reset()
	})
	stopHB()
	if lastErr != nil {
		s.sessions.reset(turn.conv)
		if !isStarted() {
			status, code := chatErrorStatus(lastErr)
			turn.finishTurn("failed", "", res, 0, lastErr)
			writeOpenAIError(w, status, code, lastErr.Error())
			return
		}
		turn.finishTurn("failed", "", res, 0, lastErr)
		writeStreamError(lastErr.Error())
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}
	turn.saveConversation(res)
	finish := "stop"
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
		if emittedToolIdx > 0 {
			finish = "tool_calls"
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
