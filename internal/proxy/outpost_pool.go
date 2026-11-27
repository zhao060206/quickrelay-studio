package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"quickrelay-studio/internal/config"
	"quickrelay-studio/internal/store"
)

const (
	defaultOutpostIdleTTL     = 10 * time.Minute
	defaultOutpostMaxSessions = 16
	outpostMaxBacklogEvents   = 128
)

var (
	outpostParallelToolWindow       = 180 * time.Millisecond
	outpostParallelActiveStepWindow = 600 * time.Millisecond
	outpostKeepaliveInterval        = 15 * time.Second
)

func effectiveOutpostKeepalive() time.Duration {
	d := outpostKeepaliveInterval
	if d <= 0 {
		d = 15 * time.Second
	}
	if wsKeepaliveInterval > 0 && wsKeepaliveInterval < d {
		d = wsKeepaliveInterval
	}
	return d
}

type outpostInboundPacket struct {
	Events []string
	Err    error
}

// outpostSession 表示一条与上游 AIDP WebSocket 及虚拟 Outpost 绑定的可挂起会话。
// 当模型发起原生 tool_call 时，会话挂起在内存池中并保持心跳与事件缓冲，等待客户端回传 tool_result 原位续写。
type outpostSession struct {
	id          string
	pool        *outpostPool
	convKey     string
	accountID   int64
	chatAgentID string
	appVersion  string
	sess        *Session

	outpostID string
	threadID  string
	workingID string
	devName   string

	conn    *websocket.Conn
	writeMu sync.Mutex

	eventsCh chan outpostInboundPacket
	readDone chan struct{}

	tools              *outpostToolRegistry
	registered         bool
	pendingCalls       []string
	emittedTextPrefix  string
	currentMessageID   string
	expectedPrefixHash string
	committedMessages  int
	lastUsedAt         time.Time
	backlog            []map[string]any

	idleStopOnce sync.Once
	idleStop     chan struct{}
	idleDone     chan struct{}

	closeOnce sync.Once

	readErrMu   sync.Mutex
	lastReadErr error
}

func (s *outpostSession) setReadErr(err error) {
	if s == nil || err == nil {
		return
	}
	s.readErrMu.Lock()
	if s.lastReadErr == nil {
		s.lastReadErr = err
	}
	s.readErrMu.Unlock()
}

func (s *outpostSession) getReadErr() error {
	if s == nil {
		return nil
	}
	s.readErrMu.Lock()
	defer s.readErrMu.Unlock()
	return s.lastReadErr
}

func (s *outpostSession) sendKeepalive() error {
	if err := s.sendFrame(map[string]any{"type": "ping", "payload": map[string]any{}}); err != nil {
		return err
	}
	s.writeMu.Lock()
	if s.conn != nil {
		_ = s.conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(5*time.Second))
	}
	s.writeMu.Unlock()
	return nil
}

func (s *outpostSession) sendPong() {
	s.writeMu.Lock()
	if s.conn != nil {
		_ = s.conn.WriteControl(websocket.PongMessage, []byte("pong"), time.Now().Add(3*time.Second))
	}
	s.writeMu.Unlock()
}

func (s *outpostSession) sendFrame(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	frame := aidpEncode(string(raw))
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.conn == nil {
		return errors.New("outpost websocket closed")
	}
	return s.conn.WriteMessage(websocket.BinaryMessage, frame)
}

func (s *outpostSession) startReader() {
	s.eventsCh = make(chan outpostInboundPacket, 256)
	s.readDone = make(chan struct{})
	go func() {
		defer close(s.readDone)
		defer close(s.eventsCh)
		for {
			msgType, data, err := s.conn.ReadMessage()
			if err != nil {
				s.setReadErr(err)
				select {
				case s.eventsCh <- outpostInboundPacket{Err: err}:
				default:
				}
				return
			}
			var frames []string
			if msgType == websocket.TextMessage {
				frames = []string{string(data)}
			} else {
				var decErr error
				frames, decErr = aidpDecode(data)
				if decErr != nil && len(frames) == 0 {
					s.setReadErr(decErr)
					select {
					case s.eventsCh <- outpostInboundPacket{Err: decErr}:
					default:
					}
					return
				}
			}
			if len(frames) == 0 {
				continue
			}
			s.eventsCh <- outpostInboundPacket{Events: frames}
		}
	}()
}

// stopIdleMonitor 停止空闲挂起期的保活与缓冲协程，在复用或销毁会话前必须调用。
func (s *outpostSession) stopIdleMonitor() {
	if s == nil {
		return
	}
	s.idleStopOnce.Do(func() {
		if s.idleStop != nil {
			close(s.idleStop)
		}
	})
	if s.idleDone != nil {
		<-s.idleDone
	}
}

// closeAndUnregister 关闭 WebSocket、尽力发送 stop_generation 并反注册虚拟 Outpost。
// 若 syncUnregister 为 true 则等待 unregisterOutpost 完成（用于优雅停机与测试校验）。
func (s *outpostSession) closeAndUnregister(syncUnregister bool) {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		if strings.TrimSpace(s.currentMessageID) != "" && s.conn != nil {
			_ = s.sendFrame(map[string]any{
				"type": "stop_generation",
				"payload": map[string]any{
					"message_id": s.currentMessageID,
					"thread_id":  s.threadID,
				},
			})
		}
		if s.conn != nil {
			_ = s.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "outpost closed"), time.Now().Add(time.Second))
			_ = s.conn.Close()
		}
		if s.readDone != nil {
			select {
			case <-s.readDone:
			case <-time.After(2 * time.Second):
			}
		}
		if s.registered && strings.TrimSpace(s.outpostID) != "" {
			doUnreg := func() {
				if err := qbsUnregisterOutpost(s.sess, s.appVersion, s.outpostID); err != nil {
					if !errors.Is(err, ErrOutpostUnsupported) {
						log.Printf("outpost unregister failed outpost=%s thread=%s: %v", s.outpostID, s.threadID, err)
					}
					return
				}
				log.Printf("outpost unregistered outpost=%s thread=%s", s.outpostID, s.threadID)
			}
			if syncUnregister {
				doUnreg()
			} else {
				go doUnreg()
			}
		}
	})
}

func isOutpostIdleBufferedEvent(typ string) bool {
	switch typ {
	case "tool_call", "permission_request", "message_start",
		"thinking_delta", "thinking_complete",
		"text_delta", "text_complete",
		"turn_usage", "acp_metadata", "message_complete",
		"turn_aborted", "session_error", "stream_error", "turn_failed",
		"error", "exception":
		return true
	default:
		return strings.HasSuffix(typ, "Exception")
	}
}

func (s *outpostSession) startIdleMonitor(idleTTL time.Duration) {
	if s == nil || s.pool == nil {
		return
	}
	if idleTTL <= 0 {
		idleTTL = defaultOutpostIdleTTL
	}
	s.idleStopOnce = sync.Once{}
	s.idleStop = make(chan struct{})
	s.idleDone = make(chan struct{})

	go func() {
		defer close(s.idleDone)
		keepaliveInterval := effectiveOutpostKeepalive()
		pingTicker := time.NewTicker(keepaliveInterval)
		defer pingTicker.Stop()
		idleTimer := time.NewTimer(idleTTL)
		defer idleTimer.Stop()

		for {
			select {
			case <-s.idleStop:
				return
			case <-idleTimer.C:
				s.pool.evictSessionIfMatches(s.convKey, s.id, false)
				return
			case <-pingTicker.C:
				if err := s.sendKeepalive(); err != nil {
					s.pool.evictSessionIfMatches(s.convKey, s.id, false)
					return
				}
			case pkt, ok := <-s.eventsCh:
				if !ok || pkt.Err != nil {
					s.pool.evictSessionIfMatches(s.convKey, s.id, false)
					return
				}
				for _, frameStr := range pkt.Events {
					var ev map[string]any
					if json.Unmarshal([]byte(frameStr), &ev) != nil {
						continue
					}
					if tid, _ := ev["thread_id"].(string); tid != "" && tid != s.threadID {
						continue
					}
					typ, _ := ev["type"].(string)
					if !isOutpostIdleBufferedEvent(typ) {
						continue
					}
					if len(s.backlog) >= outpostMaxBacklogEvents {
						s.pool.evictSessionIfMatches(s.convKey, s.id, false)
						return
					}
					s.backlog = append(s.backlog, ev)
				}
			}
		}
	}()
}

// outpostPool 管理挂起态虚拟 Outpost WebSocket 会话池，实现跨 HTTP 轮次的原位续写、容量控制与优雅清理。
type outpostPool struct {
	mu          sync.Mutex
	idle        map[string]*outpostSession
	active      map[string]*outpostSession // key = session.id
	idleTTL     time.Duration
	maxSessions int
	closing     bool
}

func newOutpostPool(idleTTL time.Duration, maxSessions int) *outpostPool {
	if idleTTL <= 0 {
		idleTTL = defaultOutpostIdleTTL
	}
	if maxSessions <= 0 {
		maxSessions = defaultOutpostMaxSessions
	}
	return &outpostPool{
		idle:        make(map[string]*outpostSession),
		active:      make(map[string]*outpostSession),
		idleTTL:     idleTTL,
		maxSessions: maxSessions,
	}
}

func (p *outpostPool) checkout(convKey string) *outpostSession {
	if p == nil || convKey == "" {
		return nil
	}
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return nil
	}
	s := p.idle[convKey]
	if s != nil {
		delete(p.idle, convKey)
		p.active[s.id] = s
	}
	p.mu.Unlock()
	if s != nil {
		s.stopIdleMonitor()
	}
	return s
}

// checkoutByThreadID 按显式 thread_id 检出仍处于挂起等待状态的虚拟 Outpost 会话（用于显式 X-Thread-Id 回传工具结果）。
func (p *outpostPool) checkoutByThreadID(threadID string) *outpostSession {
	threadID = strings.TrimSpace(threadID)
	if p == nil || threadID == "" {
		return nil
	}
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return nil
	}
	var matchedKey string
	var matched *outpostSession
	for k, s := range p.idle {
		if s.threadID == threadID && (matched == nil || s.lastUsedAt.After(matched.lastUsedAt)) {
			matchedKey = k
			matched = s
		}
	}
	if matched != nil {
		delete(p.idle, matchedKey)
		p.active[matched.id] = matched
	}
	p.mu.Unlock()
	if matched != nil {
		matched.stopIdleMonitor()
	}
	return matched
}

func (p *outpostPool) registerActive(s *outpostSession) {
	if p == nil || s == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active == nil {
		p.active = make(map[string]*outpostSession)
	}
	p.active[s.id] = s
}

func (p *outpostPool) releaseActive(s *outpostSession) {
	if p == nil || s == nil {
		return
	}
	p.mu.Lock()
	delete(p.active, s.id)
	p.mu.Unlock()
}

func (p *outpostPool) park(s *outpostSession) {
	if p == nil || s == nil {
		return
	}
	var evicted []*outpostSession
	p.mu.Lock()
	delete(p.active, s.id)
	if p.closing || s.convKey == "" {
		p.mu.Unlock()
		s.closeAndUnregister(p.closing)
		return
	}
	if prev := p.idle[s.convKey]; prev != nil && prev.id != s.id {
		delete(p.idle, s.convKey)
		evicted = append(evicted, prev)
	}
	for len(p.idle) >= p.maxSessions {
		var victimKey string
		var oldest time.Time
		for k, item := range p.idle {
			if victimKey == "" || item.lastUsedAt.Before(oldest) {
				victimKey = k
				oldest = item.lastUsedAt
			}
		}
		if victimKey == "" {
			break
		}
		evicted = append(evicted, p.idle[victimKey])
		delete(p.idle, victimKey)
	}
	s.lastUsedAt = time.Now()
	p.idle[s.convKey] = s
	idleTTL := p.idleTTL
	p.mu.Unlock()

	for _, ev := range evicted {
		go func(v *outpostSession) {
			v.stopIdleMonitor()
			v.closeAndUnregister(false)
		}(ev)
	}
	s.startIdleMonitor(idleTTL)
}

func (p *outpostPool) evictSessionIfMatches(convKey, sessionID string, syncUnreg bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	cur := p.idle[convKey]
	if cur == nil || cur.id != sessionID {
		p.mu.Unlock()
		return
	}
	delete(p.idle, convKey)
	p.mu.Unlock()
	cur.closeAndUnregister(syncUnreg)
}

// InvalidateKey 关闭并反注册指定会话键关联的挂起或活跃虚拟 Outpost。
func (p *outpostPool) InvalidateKey(convKey string) {
	if p == nil || strings.TrimSpace(convKey) == "" {
		return
	}
	var targets []*outpostSession
	p.mu.Lock()
	if s := p.idle[convKey]; s != nil {
		delete(p.idle, convKey)
		targets = append(targets, s)
	}
	for id, s := range p.active {
		if s.convKey == convKey {
			delete(p.active, id)
			targets = append(targets, s)
		}
	}
	p.mu.Unlock()
	for _, s := range targets {
		s.stopIdleMonitor()
		s.closeAndUnregister(false)
	}
}

// InvalidateAll 清空并反注册全部虚拟 Outpost 会话。
func (p *outpostPool) InvalidateAll() {
	if p == nil {
		return
	}
	var targets []*outpostSession
	p.mu.Lock()
	for k, s := range p.idle {
		delete(p.idle, k)
		targets = append(targets, s)
	}
	for id, s := range p.active {
		delete(p.active, id)
		targets = append(targets, s)
	}
	p.mu.Unlock()
	for _, s := range targets {
		s.stopIdleMonitor()
		s.closeAndUnregister(false)
	}
}

// Shutdown 在代理停机阶段同步关闭所有挂起虚拟设备并等待反注册完成。
func (p *outpostPool) Shutdown(ctx context.Context) {
	if p == nil {
		return
	}
	var targets []*outpostSession
	p.mu.Lock()
	p.closing = true
	for k, s := range p.idle {
		delete(p.idle, k)
		targets = append(targets, s)
	}
	for id, s := range p.active {
		delete(p.active, id)
		targets = append(targets, s)
	}
	p.mu.Unlock()
	if len(targets) == 0 {
		return
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	for _, s := range targets {
		wg.Add(1)
		go func(sess *outpostSession) {
			defer wg.Done()
			sess.stopIdleMonitor()
			sess.closeAndUnregister(true)
		}(s)
	}
	go func() {
		wg.Wait()
		close(done)
	}()
	if ctx == nil {
		<-done
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// ---------------------------------------------------------------------------
// 单轮执行状态机：建连/复用/冷导入 -> 发帧 -> 原生 tool_call & 文本流读取
// ---------------------------------------------------------------------------

func dialOutpostWS(ctx context.Context, upstream *Upstream, sess *Session, outpostID, workingID string) (*websocket.Conn, error) {
	sessCopy := *sess
	sessCopy.OutpostID = outpostID
	sessCopy.WorkingSessionID = workingID
	wsURL, headers, gateway, err := upstream.dialInfo(&sessCopy)
	if err != nil {
		return nil, err
	}
	if gateway {
		return nil, fmt.Errorf("%w: gateway transport", ErrOutpostUnsupported)
	}
	parsed, err := url.Parse(wsURL)
	if err == nil {
		q := parsed.Query()
		q.Set("outpostId", outpostID)
		q.Set("workingSessionId", workingID)
		q.Set("channel", "prod")
		parsed.RawQuery = q.Encode()
		wsURL = parsed.String()
	}
	headers.Set("User-Agent", "Amazon Quick/"+qbsAppVersion(upstream.cfg.AppVersion))

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
		return nil, &UpstreamError{
			StatusCode: status,
			RetryAfter: retryAfter,
			Err:        fmt.Errorf("outpost WebSocket dial failed%s: %w", detail, err),
		}
	}
	return conn, nil
}

func (p *outpostPool) openSession(
	ctx context.Context,
	turn *preparedTurn,
	sess *Session,
	threadID string,
	reg *outpostToolRegistry,
	importHistory bool,
	historyMsgs []Message,
) (*outpostSession, error) {
	if strings.TrimSpace(threadID) == "" {
		threadID = uuid()
	}
	outpostID := uuid()
	workingID := uuid()
	devName := outpostDeviceName(threadID)
	appVer := qbsAppVersion(turn.server.cfg.AppVersion)

	var events []map[string]any
	if importHistory && len(historyMsgs) > 0 {
		var hErr error
		events, hErr = buildOutpostHistoryEvents(historyMsgs, threadID, devName)
		if hErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrOutpostUnsupported, hErr)
		}
	}

	conn, err := dialOutpostWS(ctx, turn.server.upstream, sess, outpostID, workingID)
	if err != nil {
		return nil, err
	}
	osess := &outpostSession{
		id:          uuid(),
		pool:        p,
		convKey:     turn.convKey(),
		accountID:   turn.usedAccount.ID,
		chatAgentID: turn.chatAgentID,
		appVersion:  appVer,
		sess:        sess,
		outpostID:   outpostID,
		threadID:    threadID,
		workingID:   workingID,
		devName:     devName,
		conn:        conn,
		tools:       reg,
	}
	osess.startReader()

	cleanupOnErr := true
	defer func() {
		if cleanupOnErr {
			osess.closeAndUnregister(false)
		}
	}()

	if err := osess.sendFrame(map[string]any{
		"type": "session_connect",
		"payload": map[string]any{
			"actor_id":          sess.ActorARN,
			"surfaceType":       sess.Surface,
			"outpostId":         outpostID,
			"appVersion":        appVer,
			"acp_first_text":    1,
			"user_notification": 1,
		},
	}); err != nil {
		return nil, err
	}
	userMap := map[string]any{
		"role": "member",
	}
	if strings.TrimSpace(sess.DisplayName) != "" {
		userMap["name"] = sess.DisplayName
	}
	if strings.TrimSpace(sess.Email) != "" {
		userMap["email"] = sess.Email
	}
	if strings.TrimSpace(sess.ActorARN) != "" {
		userMap["user_arn"] = sess.ActorARN
	}
	if err := osess.sendFrame(map[string]any{
		"type": "federate_status",
		"payload": map[string]any{
			"user": userMap,
		},
	}); err != nil {
		return nil, err
	}

	// 等待服务端返回 connected 握手确认帧
	connectWait := wsDialHandshakeTimeout
	if connectWait <= 0 {
		connectWait = 12 * time.Second
	}
	connDeadline := time.NewTimer(connectWait)
	defer connDeadline.Stop()
	connected := false
	for !connected {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-connDeadline.C:
			return nil, &UpstreamError{StatusCode: http.StatusGatewayTimeout, Code: "Timeout", Err: errors.New("outpost session startup timed out")}
		case pkt, ok := <-osess.eventsCh:
			if !ok || pkt.Err != nil {
				if pkt.Err != nil {
					return nil, fmt.Errorf("outpost connect read: %w", pkt.Err)
				}
				return nil, errors.New("outpost socket closed before connected")
			}
			for _, frameStr := range pkt.Events {
				var ev map[string]any
				if json.Unmarshal([]byte(frameStr), &ev) != nil {
					continue
				}
				typ, _ := ev["type"].(string)
				switch typ {
				case "connected":
					connected = true
				case "error", "exception":
					return nil, parseOutpostErrorEvent(ev)
				}
			}
		}
	}

	// 注册虚拟设备及工具能力
	if !reg.empty() {
		osess.registered = true
		if err := qbsPutOutpost(ctx, sess, appVer, outpostID, devName, reg); err != nil {
			return nil, err
		}
	}

	// 若需恢复冷历史或中断的 tool_result 历史，先写入服务端事件存储再绑定线程
	if len(events) > 0 {
		if err := importOutpostHistory(ctx, sess, appVer, outpostID, workingID, threadID, turn.in.Title, turn.server.cfg.KeepSessions, events); err != nil {
			return nil, err
		}
	}

	if err := osess.sendFrame(map[string]any{
		"type": "session_connect",
		"payload": map[string]any{
			"session_id":        threadID,
			"focus_only":        true,
			"incognito":         !turn.server.cfg.KeepSessions,
			"surfaceType":       sess.Surface,
			"outpostId":         outpostID,
			"appVersion":        appVer,
			"acp_first_text":    1,
			"user_notification": 1,
		},
	}); err != nil {
		return nil, err
	}

	p.registerActive(osess)
	cleanupOnErr = false
	return osess, nil
}

func (s *outpostSession) ensureToolsRegistered(ctx context.Context, reg *outpostToolRegistry) error {
	if s.tools != nil && reg != nil && s.tools.ToolsHash == reg.ToolsHash && s.registered {
		return nil
	}
	s.tools = reg
	if reg == nil || reg.empty() {
		return nil
	}
	s.registered = true
	return qbsPutOutpost(ctx, s.sess, s.appVersion, s.outpostID, s.devName, reg)
}

// buildOutpostUserMessagePayload 构造原生 Outpost 上行 user_message 帧的完整载荷（含 custom_prompt、工具文档与路由指令）。
// 若含冗余技能列表导致轻微超过 63 KiB 帧上限，优先无损压缩 <available_skills>，绝不截断用户正文或工具文档。
func buildOutpostUserMessagePayload(turn *preparedTurn, sess *Session, reg *outpostToolRegistry, outpostID, devName, threadID, systemPrompt, turnText string) map[string]any {
	buildCP := func(sys string) string {
		var cp strings.Builder
		if strings.TrimSpace(sys) != "" {
			cp.WriteString(strings.TrimSpace(sys))
		}
		cp.WriteString("\n\n# API Client Presentation\nThis conversation runs through an API gateway, not the desktop UI. Use standard Markdown/plain text and NEVER output <decision>, <option>, or <decisions> UI tags.\n")
		if reg != nil && reg.PromptDocs != "" {
			cp.WriteString(reg.PromptDocs)
		}
		if reg != nil {
			cp.WriteString(reg.routingInstructions(outpostID, devName, turn.choiceMode, turn.choiceName, turn.parallelTools))
		}
		return cp.String()
	}

	payload := map[string]any{
		"surfaceType":        sess.Surface,
		"outpostId":          outpostID,
		"appVersion":         qbsAppVersion(turn.server.cfg.AppVersion),
		"thread_id":          threadID,
		"message_id":         uuid(),
		"text":               turnText,
		"custom_prompt":      buildCP(systemPrompt),
		"agent_mode":         turn.in.Model,
		"auto_routing":       false,
		"web_search_enabled": false,
	}
	if effort := strings.TrimSpace(turn.in.ThinkingEffort); effort != "" {
		payload["thinking_effort"] = effort
	}
	if id := strings.TrimSpace(turn.chatAgentID); id != "" {
		payload["chat_agent_id"] = id
	}
	if tz := strings.TrimSpace(turn.server.cfg.Timezone); tz != "" {
		payload["user_timezone"] = tz
	}
	if loc := strings.TrimSpace(turn.server.cfg.Locale); loc != "" {
		payload["user_locale"] = loc
	}
	if n, err := userMessageBytes(payload); err == nil && n > maxUserMessageBytes && strings.TrimSpace(systemPrompt) != "" {
		curSys := systemPrompt
		if compacted := compactSkillsSection(curSys); len(compacted) < len(curSys) {
			curSys = compacted
			payload["custom_prompt"] = buildCP(curSys)
			if n2, err2 := userMessageBytes(payload); err2 == nil && n2 <= maxUserMessageBytes {
				return payload
			}
		}
		if indexed := compactSkillsIndexOnly(systemPrompt); len(indexed) < len(curSys) {
			payload["custom_prompt"] = buildCP(indexed)
		}
	}
	return payload
}

// checkOutpostUserMessageBudget 校验完整序列化后的原生 user_message 帧是否超过上游 63 KiB 预算；
// 在 auto 模式下返回 ErrOutpostUnsupported 以便无损回退文本桥，强制 native_outpost 模式返回 ErrUserMessageTooLarge。
func checkOutpostUserMessageBudget(payload map[string]any, toolMode string) error {
	n, err := userMessageBytes(payload)
	if err != nil {
		return err
	}
	if n <= maxUserMessageBytes {
		return nil
	}
	if config.NormalizeToolMode(toolMode) == config.ToolModeAuto {
		return fmt.Errorf("%w: %w (%d bytes > %d)", ErrOutpostUnsupported, ErrUserMessageTooLarge, n, maxUserMessageBytes)
	}
	return fmt.Errorf("%w: %d bytes in native_outpost user_message", ErrUserMessageTooLarge, n)
}

func (p *outpostPool) RunTurn(ctx context.Context, turn *preparedTurn, sess *Session, onDispatched func(), onEvent EventFunc) (ChatResult, error) {
	if turn == nil || turn.req == nil || sess == nil || turn.usedAccount == nil {
		return ChatResult{}, fmt.Errorf("%w: missing turn context", ErrOutpostUnsupported)
	}
	input, err := extractOutpostTurnInput(turn.req.Messages)
	if err != nil {
		if errors.Is(err, errOutpostHistoryNotImportable) {
			return ChatResult{}, fmt.Errorf("%w: %v", ErrOutpostUnsupported, err)
		}
		return ChatResult{}, err
	}
	reg := newOutpostToolRegistry(turn.req)
	if reg.empty() {
		return ChatResult{}, fmt.Errorf("%w: no client tools declared", ErrOutpostUnsupported)
	}

	canReuse := turn.server.cfg.SessionReuse && !turn.auxRequest
	convKey := turn.convKey()
	explicitTID := strings.TrimSpace(turn.explicitThreadID)
	toolMode := turn.server.cfg.GetToolMode()

	var osess *outpostSession
	resumingPendingTools := false
	reusingIdleThread := false

	if canReuse && (convKey != "" || explicitTID != "") {
		candidate := p.checkout(convKey)
		if candidate == nil && explicitTID != "" {
			candidate = p.checkoutByThreadID(explicitTID)
		}
		if candidate != nil {
			sameAccount := candidate.accountID == turn.usedAccount.ID && candidate.chatAgentID == turn.chatAgentID
			sameThread := explicitTID == "" || candidate.threadID == explicitTID
			alive := !candidate.isClosed()
			if sameAccount && sameThread && alive {
				if len(candidate.pendingCalls) > 0 {
					if outpostToolIDSetMatch(candidate.pendingCalls, input.TurnToolResults) {
						osess = candidate
						if convKey != "" {
							osess.convKey = convKey
						}
						resumingPendingTools = true
					}
				} else if !input.RawTailHasTools && (candidate.expectedPrefixHash == "" || candidate.expectedPrefixHash == input.HistoryPrefixHash) {
					osess = candidate
					if convKey != "" {
						osess.convKey = convKey
					}
					reusingIdleThread = true
				}
			}
			if osess == nil {
				p.releaseActive(candidate)
				candidate.closeAndUnregister(false)
			}
		}
	}

	importedHistoryWithCurrentTools := false
	usedColdImport := false
	if osess == nil {
		threadID := ""
		var histToImport []Message
		if explicitTID != "" {
			// 调用方显式指定 X-Thread-Id 冷续接时，严禁静默替换为新 UUID；必须验证已知归属与状态一致性。
			if !turn.explicitThreadKnown {
				return ChatResult{}, fmt.Errorf("%w: thread %q has unknown ownership", ErrExplicitThreadConflict, explicitTID)
			}
			if turn.explicitThreadAccountID != 0 && turn.usedAccount.ID != turn.explicitThreadAccountID {
				return ChatResult{}, fmt.Errorf("%w: thread %q belongs to another account", ErrExplicitThreadConflict, explicitTID)
			}
			if input.RawTailHasTools {
				return ChatResult{}, fmt.Errorf("%w: thread %q cannot resume unparked tool_result without replacing thread", ErrExplicitThreadConflict, explicitTID)
			}
			threadID = explicitTID
		} else if len(input.HistoryMessages) > 0 || input.RawTailHasTools {
			if input.RawTailHasTools {
				// 挂起会话已丢失（如重启或换号），把历史连同本轮已完成 tool_result 整体冷导入新云端线程
				histToImport = nonLeadingMessages(turn.req.Messages)
				if strings.TrimSpace(input.TurnUserText) != "" {
					// 最后一轮如果附带额外 user 文本，只导入到工具结果，用户文本作为新 user_message 发出
					histToImport = stripTrailingUserTextsFromNonLeading(nonLeadingMessages(turn.req.Messages))
				}
				importedHistoryWithCurrentTools = len(histToImport) > 0
			} else {
				histToImport = input.HistoryMessages
			}
		}
		usedColdImport = len(histToImport) > 0

		// 冷启动建连前预检完整序列化 user_message 预算：若超限在建连/注册 Outpost 前直接中止，确保 0 帧发往上游。
		previewText := input.TurnUserText
		if importedHistoryWithCurrentTools && strings.TrimSpace(previewText) == "" {
			previewText = "Continue responding to the latest user request using the completed tool results in the conversation history. Those tools have already run; do not repeat completed actions merely because this connection was restored."
		}
		previewThreadID := threadID
		if previewThreadID == "" {
			previewThreadID = "00000000-0000-0000-0000-000000000000"
		}
		previewPayload := buildOutpostUserMessagePayload(turn, sess, reg, "00000000-0000-0000-0000-000000000000", outpostDeviceName(previewThreadID), previewThreadID, input.SystemPrompt, previewText)
		if bErr := checkOutpostUserMessageBudget(previewPayload, toolMode); bErr != nil {
			return ChatResult{}, bErr
		}

		opened, openErr := p.openSession(ctx, turn, sess, threadID, reg, usedColdImport, histToImport)
		if openErr != nil {
			return ChatResult{}, openErr
		}
		osess = opened
	} else {
		osess.sess = sess
		if err := osess.ensureToolsRegistered(ctx, reg); err != nil {
			p.releaseActive(osess)
			osess.closeAndUnregister(false)
			return ChatResult{}, err
		}
	}

	outpostMode := "fresh"
	switch {
	case resumingPendingTools:
		outpostMode = "live_tool_resume"
	case reusingIdleThread:
		outpostMode = "live_turn_resume"
	case usedColdImport:
		outpostMode = "cold_import"
	}
	log.Printf("outpost %s mode=%s thread=%s outpost=%s tools=%d", turn.id, outpostMode, osess.threadID, osess.outpostID, len(reg.Tools))

	turnSucceeded := false
	parkedForToolsOrReuse := false
	defer func() {
		if !turnSucceeded && !parkedForToolsOrReuse {
			p.releaseActive(osess)
			osess.closeAndUnregister(false)
		}
	}()

	if resumingPendingTools {
		for idx, item := range input.TurnToolResults {
			content := []map[string]any{{"type": "text", "text": item.Text}}
			if idx == len(input.TurnToolResults)-1 && strings.TrimSpace(input.TurnUserText) != "" {
				content = append(content, map[string]any{
					"type": "text",
					"text": "[QuickRelay client user update]\n" + strings.TrimSpace(input.TurnUserText) + "\n[/QuickRelay client user update]",
				})
			}
			if err := osess.sendFrame(map[string]any{
				"type": "tool_result",
				"payload": map[string]any{
					"tool_call_id": item.ToolCallID,
					"result": map[string]any{
						"content": content,
						"isError": item.IsError,
					},
					"error": nil,
				},
			}); err != nil {
				return ChatResult{}, err
			}
		}
		osess.pendingCalls = nil
	} else {
		osess.emittedTextPrefix = ""
		turnText := input.TurnUserText
		if importedHistoryWithCurrentTools && strings.TrimSpace(turnText) == "" {
			turnText = "Continue responding to the latest user request using the completed tool results in the conversation history. Those tools have already run; do not repeat completed actions merely because this connection was restored."
		}
		payload := buildOutpostUserMessagePayload(turn, sess, reg, osess.outpostID, osess.devName, osess.threadID, input.SystemPrompt, turnText)
		if bErr := checkOutpostUserMessageBudget(payload, toolMode); bErr != nil {
			return ChatResult{}, bErr
		}
		if err := osess.sendFrame(map[string]any{
			"type":    "user_message",
			"payload": payload,
		}); err != nil {
			return ChatResult{}, err
		}
	}

	if onDispatched != nil {
		onDispatched()
	} else if turn.in.OnDispatched != nil {
		turn.in.OnDispatched()
	}

	res, sawToolCalls, canPark, err := osess.readTurnEvents(ctx, turn, onEvent)
	res.ThreadID = osess.threadID
	if err != nil {
		return res, err
	}

	turnSucceeded = true
	if sawToolCalls {
		// 仅当客户端收到的工具集合与上游等待结果的工具 ID 完全对齐时保留挂起态 WS；否则清理，下轮由冷导入续接。
		if canPark && canReuse && convKey != "" {
			parkedForToolsOrReuse = true
			p.park(osess)
		} else {
			p.releaseActive(osess)
			osess.closeAndUnregister(false)
		}
		return res, nil
	}

	// 普通完整文本回复结束：刷新文本前缀并决定是否保留会话供下一轮继续使用
	osess.emittedTextPrefix = ""
	if canReuse && convKey != "" && !turn.in.DeleteSession {
		nextMsgs := make([]Message, 0, len(nonLeadingMessages(turn.req.Messages))+1)
		nextMsgs = append(nextMsgs, nonLeadingMessages(turn.req.Messages)...)
		nextMsgs = append(nextMsgs, Message{Role: "assistant", Content: res.Text})
		osess.expectedPrefixHash = hashMessagesPrefix(nextMsgs)
		parkedForToolsOrReuse = true
		p.park(osess)
	} else {
		p.releaseActive(osess)
		if turn.in.DeleteSession {
			_ = osess.sendFrame(map[string]any{"type": "session_delete", "payload": map[string]any{"session_id": osess.threadID}})
		}
		osess.closeAndUnregister(false)
	}
	return res, nil
}

func (s *outpostSession) isClosed() bool {
	if s == nil || s.readDone == nil {
		return true
	}
	select {
	case <-s.readDone:
		return true
	default:
		return false
	}
}

func stripTrailingUserTextsFromNonLeading(msgs []Message) []Message {
	end := len(msgs)
	for end > 0 {
		role := strings.ToLower(strings.TrimSpace(msgs[end-1].Role))
		if role == "tool" {
			break
		}
		if role == "user" || role == "developer" || role == "system" {
			end--
			continue
		}
		break
	}
	if end == 0 {
		return msgs
	}
	return msgs[:end]
}

func (s *outpostSession) readTurnEvents(ctx context.Context, turn *preparedTurn, onEvent EventFunc) (ChatResult, bool, bool, error) {
	isMaxThinking := strings.EqualFold(strings.TrimSpace(turn.in.ThinkingEffort), "max")
	idleReadLimit := wsReadIdleTimeout
	if isMaxThinking && wsReadIdleTimeoutMaxThinking > idleReadLimit {
		idleReadLimit = wsReadIdleTimeoutMaxThinking
	}
	keepaliveInterval := effectiveOutpostKeepalive()

	pingTicker := time.NewTicker(keepaliveInterval)
	defer pingTicker.Stop()
	idleTimer := time.NewTimer(idleReadLimit)
	defer idleTimer.Stop()
	resetIdle := func() {
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(idleReadLimit)
	}

	var result ChatResult
	var thinking strings.Builder
	var segText strings.Builder // 当前 HTTP 轮次已经发射过的累计正文（扣除先前 emittedTextPrefix 后）
	var msgText strings.Builder // 当前消息段内已经发射过的正文，用于多段 continuation 场景精确对账
	needSegmentBreak := false
	sawContinuation := false
	turnFinished := false
	var parallelDeadline <-chan time.Time
	var parallelTimer *time.Timer
	armParallelTimer := func(d time.Duration) {
		if d <= 0 {
			return
		}
		if parallelTimer == nil {
			parallelTimer = time.NewTimer(d)
			parallelDeadline = parallelTimer.C
			return
		}
		if !parallelTimer.Stop() {
			select {
			case <-parallelTimer.C:
			default:
			}
		}
		parallelTimer.Reset(d)
		parallelDeadline = parallelTimer.C
	}
	defer func() {
		if parallelTimer != nil {
			parallelTimer.Stop()
		}
	}()

	type creditSnapshot struct {
		unit   string
		amount *float64
	}
	creditSnapshots := make(map[string]creditSnapshot)
	rawToolCalls := make([]ToolCallState, 0, 2)
	seenCallIDs := make(map[string]bool)
	readFailed := false

	emitRawSegText := func(delta string, recordInMsg bool) error {
		if delta == "" {
			return nil
		}
		segText.WriteString(delta)
		if recordInMsg {
			msgText.WriteString(delta)
		}
		if onEvent != nil {
			return onEvent(StreamEvent{Type: "text", Text: delta})
		}
		return nil
	}

	emitSegText := func(delta string) error {
		if delta == "" {
			return nil
		}
		if needSegmentBreak {
			needSegmentBreak = false
			if segText.Len() > 0 && !strings.HasSuffix(segText.String(), "\n") {
				if err := emitRawSegText("\n\n", false); err != nil {
					return err
				}
			}
		}
		return emitRawSegText(delta, true)
	}

	reconcileFullText := func(fullText string) error {
		// 先剥离先前工具轮已发出的累计前缀（若上游重复带回累计文本）
		cur := fullText
		if s.emittedTextPrefix != "" && strings.HasPrefix(cur, s.emittedTextPrefix) {
			cur = cur[len(s.emittedTextPrefix):]
		}
		if cur == "" {
			return nil
		}
		alreadyAll := segText.String()
		if cur == alreadyAll {
			return nil
		}
		if alreadyAll != "" && strings.HasPrefix(cur, alreadyAll) {
			return emitSegText(cur[len(alreadyAll):])
		}
		alreadyMsg := msgText.String()
		if cur == alreadyMsg {
			return nil
		}
		if strings.HasPrefix(cur, alreadyMsg) {
			return emitSegText(cur[len(alreadyMsg):])
		}
		return nil
	}

	handleTextDelta := func(delta string) error {
		if delta == "" {
			return nil
		}
		// 若这是跨工具边界续写后的第一段文本，且上游把前段文本一起补发了，尽量精准去重
		if s.emittedTextPrefix != "" && segText.Len() == 0 {
			if strings.HasPrefix(delta, s.emittedTextPrefix) {
				delta = delta[len(s.emittedTextPrefix):]
			} else if strings.HasPrefix(s.emittedTextPrefix, delta) {
				return nil
			}
		}
		return emitSegText(delta)
	}

	handlePermissionRequest := func(p map[string]any) error {
		connID, _ := p["connectorId"].(string)
		if connID == "" {
			connID, _ = p["connector_id"].(string)
		}
		actID, _ := p["actionId"].(string)
		if actID == "" {
			actID, _ = p["action_id"].(string)
		}
		_, known := s.tools.resolveName(actID, s.devName)
		approved := connID == outpostConnectorName && known
		respType := "permission_denied"
		if approved {
			respType = "tool_approval"
		}
		resp := map[string]any{
			"type":        respType,
			"connectorId": connID,
			"actionId":    actID,
			"scope":       "ALLOW_ONCE",
			"sessionId":   s.threadID,
		}
		for _, k := range []string{"params", "paramsHash", "agentId", "conversationId"} {
			if v, ok := p[k]; ok {
				resp[k] = v
			}
		}
		if err := s.sendFrame(resp); err != nil {
			return err
		}
		if !approved {
			return &RemoteToolBlockedError{ToolName: firstNonEmptyStr(actID, connID), Reason: "unregistered_outpost_permission"}
		}
		return nil
	}

	finalizeTurn := func(hasTools, completedNormally bool) (ChatResult, bool, bool, error) {
		result.Text = segText.String()
		result.Thinking = thinking.String()
		creditTotals := make(map[string]float64)
		for _, c := range creditSnapshots {
			if c.amount != nil {
				creditTotals[c.unit] += *c.amount
			}
		}
		units := make([]string, 0, len(creditTotals))
		for u := range creditTotals {
			units = append(units, u)
		}
		sort.Strings(units)
		result.Usage.Credits = result.Usage.Credits[:0]
		for _, u := range units {
			result.Usage.Credits = append(result.Usage.Credits, store.CreditUsage{Unit: u, Amount: creditTotals[u]})
		}
		if hasTools {
			connectionClosed := readFailed || s.isClosed()
			if turn.explicitThreadID != "" && connectionClosed {
				return result, false, false, errors.New("outpost WebSocket closed before it could accept tool results for the explicit thread")
			}
			filtered := filterOutpostToolCalls(rawToolCalls, turn.choiceMode, turn.choiceName, turn.parallelTools)
			if len(filtered) == 0 {
				// 被策略全部过滤时绝不回退为原始违规调用，直接中止本轮且不挂起会话。
				s.pendingCalls = nil
				result.ToolCalls = nil
				blockedName := "unknown"
				if len(rawToolCalls) > 0 && strings.TrimSpace(rawToolCalls[0].Name) != "" {
					blockedName = strings.TrimSpace(rawToolCalls[0].Name)
				}
				return result, false, false, fmt.Errorf("%w: choice=%s(%q) blocked upstream tool %q", ErrOutpostToolPolicyMismatch, turn.choiceMode, turn.choiceName, blockedName)
			}
			canReuse := turn.server != nil && turn.server.cfg != nil && turn.server.cfg.SessionReuse && !turn.auxRequest
			convKey := turn.convKey()
			canPark := !connectionClosed && len(filtered) == len(rawToolCalls) && canReuse && (convKey != "" || strings.TrimSpace(turn.explicitThreadID) != "")
			if strings.TrimSpace(turn.explicitThreadID) != "" && !canPark {
				// 显式线程无法挂起等待被过滤的工具子集且不允许下轮换线程重导入，必须在向客户端发出调用前直接报错。
				s.pendingCalls = nil
				result.ToolCalls = nil
				return result, false, false, fmt.Errorf("%w: explicit thread %q cannot safely accept filtered tool_result (upstream=%d, allowed=%d)", ErrOutpostToolPolicyMismatch, strings.TrimSpace(turn.explicitThreadID), len(rawToolCalls), len(filtered))
			}
			result.ToolCalls = filtered
			if canPark {
				s.emittedTextPrefix += result.Text
				s.pendingCalls = make([]string, 0, len(filtered))
				for _, tc := range filtered {
					s.pendingCalls = append(s.pendingCalls, tc.ID)
				}
			} else {
				s.pendingCalls = nil
			}
			if onEvent != nil {
				for i := range filtered {
					tcCopy := filtered[i]
					if err := onEvent(StreamEvent{Type: "tool_call", ToolCall: &tcCopy}); err != nil {
						return result, true, canPark, err
					}
				}
			}
			return result, true, canPark, nil
		}
		if completedNormally {
			return result, false, false, turn.validateToolChoice(nil)
		}
		return result, false, false, nil
	}

	processEvent := func(ev map[string]any) (completed bool, err error) {
		if tid, _ := ev["thread_id"].(string); tid != "" && tid != s.threadID {
			return false, nil
		}
		typ, _ := ev["type"].(string)
		payloadBytes, _ := json.Marshal(ev["payload"])
		var p map[string]any
		_ = json.Unmarshal(payloadBytes, &p)

		switch typ {
		case "message_start":
			if mid, _ := ev["message_id"].(string); mid != "" {
				s.currentMessageID = mid
			} else if mid, _ := p["message_id"].(string); mid != "" {
				s.currentMessageID = mid
			}
			msgText.Reset()
			needSegmentBreak = false
			if segText.Len() > 0 && !strings.HasSuffix(segText.String(), "\n") {
				if err := emitRawSegText("\n\n", false); err != nil {
					return false, err
				}
			}
		case "permission_request":
			if err := handlePermissionRequest(p); err != nil {
				return false, err
			}
		case "tool_start", "tool_progress", "tool_arguments":
			// 已有并行 tool_call 等待聚拢时，上游流式生成同轮下一个工具期间自动滑动续期，防止大参数工具中途超时拆分
			if len(rawToolCalls) > 0 && turn.parallelTools {
				armParallelTimer(outpostParallelActiveStepWindow)
			}
		case "tool_call":
			callID, _ := p["tool_call_id"].(string)
			callID = strings.TrimSpace(callID)
			if callID == "" {
				callID = "call_" + strings.ReplaceAll(uuid(), "-", "")
			}
			if seenCallIDs[callID] {
				return false, nil
			}
			wireName, _ := p["tool_name"].(string)
			argRaw, _ := json.Marshal(p["arguments"])
			origName, argsJSON, decErr := s.tools.decodeCall(wireName, argRaw, s.devName)
			if decErr != nil {
				return false, &RemoteToolBlockedError{ToolName: wireName, Reason: decErr.Error()}
			}
			seenCallIDs[callID] = true
			rawToolCalls = append(rawToolCalls, ToolCallState{
				ID:        callID,
				Name:      origName,
				Arguments: argsJSON,
			})
			if !turn.parallelTools {
				return true, nil
			}
			armParallelTimer(outpostParallelToolWindow)
		case "thinking_delta":
			if d := thinkingDeltaText(payloadBytes); d != "" {
				thinking.WriteString(d)
				if onEvent != nil {
					if err := onEvent(StreamEvent{Type: "thinking", Text: d}); err != nil {
						return false, err
					}
				}
			}
		case "thinking_complete":
			if thinking.Len() == 0 {
				if full := thinkingDeltaText(payloadBytes); full != "" {
					thinking.WriteString(full)
					if onEvent != nil {
						if err := onEvent(StreamEvent{Type: "thinking", Text: full}); err != nil {
							return false, err
						}
					}
				}
			}
		case "text_delta":
			if len(rawToolCalls) > 0 {
				// 已有 tool_call 待返回给客户端，不再混入后续冗余正文
				return true, nil
			}
			if d := deltaText(payloadBytes); d != "" {
				if err := handleTextDelta(d); err != nil {
					return false, err
				}
			}
		case "text_complete":
			if len(rawToolCalls) > 0 {
				return true, nil
			}
			if txt, _ := p["text"].(string); txt != "" {
				if err := reconcileFullText(txt); err != nil {
					return false, err
				}
			}
		case "acp_metadata":
			var meta struct {
				CostValue *float64 `json:"cost_value"`
				CostUnit  *string  `json:"cost_unit_plural"`
				MessageID string   `json:"message_id"`
			}
			if json.Unmarshal(payloadBytes, &meta) == nil {
				mid := firstNonEmptyStr(meta.MessageID, s.currentMessageID, "turn")
				c := creditSnapshots[mid]
				if meta.CostUnit != nil {
					unit := strings.TrimSpace(*meta.CostUnit)
					if unit != "" && len(unit) <= 128 && !strings.ContainsAny(unit, "\r\n\t\x00") {
						c.unit = unit
					}
				}
				if c.unit == "" {
					c.unit = "credits"
				}
				if meta.CostValue != nil && *meta.CostValue >= 0 && !math.IsNaN(*meta.CostValue) && !math.IsInf(*meta.CostValue, 0) {
					c.amount = meta.CostValue
				}
				creditSnapshots[mid] = c
			}
		case "turn_usage":
			var tu struct {
				InputTokens      int      `json:"input_tokens"`
				OutputTokens     int      `json:"output_tokens"`
				TotalTokens      int      `json:"total_tokens"`
				CacheReadTokens  int      `json:"cache_read_tokens"`
				CacheWriteTokens int      `json:"cache_write_tokens"`
				CostUSD          *float64 `json:"cost_usd"`
				ResolvedModelID  string   `json:"resolved_model_id"`
				ServedModelID    string   `json:"served_model_id"`
			}
			if json.Unmarshal(payloadBytes, &tu) == nil {
				resolved := strings.TrimSpace(tu.ServedModelID)
				if resolved == "" {
					resolved = strings.TrimSpace(tu.ResolvedModelID)
				}
				result.Usage.add(Usage{
					PromptTokens:     tu.InputTokens,
					CompletionTokens: tu.OutputTokens,
					TotalTokens:      tu.TotalTokens,
					CacheReadTokens:  tu.CacheReadTokens,
					CacheWriteTokens: tu.CacheWriteTokens,
					CostUSD:          tu.CostUSD,
					ResolvedModelID:  resolved,
				})
			}
		case "message_complete":
			var mc struct {
				Text                 string `json:"text"`
				ContinuationExpected bool   `json:"continuation_expected"`
			}
			_ = json.Unmarshal(payloadBytes, &mc)
			if len(rawToolCalls) == 0 && mc.Text != "" {
				if err := reconcileFullText(mc.Text); err != nil {
					return false, err
				}
			}
			// 区分单条消息结束与整轮回答结束：若 continuation_expected=true，保留 sawContinuation 并重置单段对账状态继续读取。
			if len(rawToolCalls) == 0 && mc.ContinuationExpected {
				needSegmentBreak = true
				sawContinuation = true
				msgText.Reset()
				return false, nil
			}
			needSegmentBreak = false
			turnFinished = true
			return true, nil
		case "turn_completed", "turn_ended", "stream_ended", "chat_complete":
			needSegmentBreak = false
			turnFinished = true
			if len(rawToolCalls) == 0 {
				if txt, _ := p["text"].(string); txt != "" {
					if err := reconcileFullText(txt); err != nil {
						return false, err
					}
				}
			}
			return true, nil
		case "turn_aborted":
			return false, &UpstreamError{Code: "turn_aborted", Err: errors.New("upstream turn was aborted")}
		case "error", "exception", "session_error", "stream_error", "turn_failed":
			return false, parseOutpostErrorEvent(ev)
		case "ping":
			s.sendPong()
		default:
			if strings.HasSuffix(typ, "Exception") {
				return false, parseOutpostErrorEvent(ev)
			}
		}
		return false, nil
	}

	// 先消费挂起期间缓存的 backlog 事件
	if len(s.backlog) > 0 {
		buffered := s.backlog
		s.backlog = nil
		for _, ev := range buffered {
			done, err := processEvent(ev)
			if err != nil {
				res, _, _, _ := finalizeTurn(false, false)
				return res, false, false, err
			}
			if done {
				return finalizeTurn(len(rawToolCalls) > 0, true)
			}
		}
	}

	for {
		var pkt outpostInboundPacket
		var ok, havePacket bool
		if len(rawToolCalls) > 0 {
			// 若在并行窗口或单次 packet 结束后已收齐，先快速非阻塞排空当前已到达包
			select {
			case pkt, ok = <-s.eventsCh:
				havePacket = true
			default:
			}
		}

		if !havePacket {
			select {
			case <-ctx.Done():
				res, _, _, _ := finalizeTurn(false, false)
				return res, false, false, ctx.Err()
			case <-parallelDeadline:
				return finalizeTurn(true, true)
			case <-idleTimer.C:
				res, _, _, _ := finalizeTurn(false, false)
				return res, false, false, fmt.Errorf("outpost WebSocket read idle timeout (%s)", idleReadLimit)
			case <-pingTicker.C:
				if err := s.sendKeepalive(); err != nil {
					res, _, _, _ := finalizeTurn(false, false)
					return res, false, false, err
				}
			case pkt, ok = <-s.eventsCh:
			}
		}
		if !ok {
			readFailed = true
			if len(rawToolCalls) > 0 {
				return finalizeTurn(true, true)
			}
			if turnFinished {
				return finalizeTurn(false, true)
			}
			res, _, _, _ := finalizeTurn(false, false)
			cause := s.getReadErr()
			if cause == nil {
				cause = io.ErrUnexpectedEOF
			}
			if sawContinuation {
				return res, false, false, fmt.Errorf("outpost WebSocket closed before continuation completed (%w): %w", ErrOutpostPrematureClose, cause)
			}
			return res, false, false, fmt.Errorf("outpost WebSocket closed unexpectedly (%w): %w", ErrOutpostPrematureClose, cause)
		}
		if pkt.Err != nil {
			readFailed = true
			if len(rawToolCalls) > 0 {
				return finalizeTurn(true, true)
			}
			if websocket.IsCloseError(pkt.Err, websocket.CloseNormalClosure, websocket.CloseNoStatusReceived) {
				if !turnFinished {
					res, _, _, _ := finalizeTurn(false, false)
					if sawContinuation {
						return res, false, false, fmt.Errorf("outpost WebSocket closed before continuation completed (%w): %w", ErrOutpostPrematureClose, pkt.Err)
					}
					return res, false, false, fmt.Errorf("outpost WebSocket closed before turn completed (%w): %w", ErrOutpostPrematureClose, pkt.Err)
				}
				return finalizeTurn(false, true)
			}
			res, _, _, _ := finalizeTurn(false, false)
			return res, false, false, fmt.Errorf("outpost WebSocket read (%w): %w", ErrOutpostPrematureClose, pkt.Err)
		}
		resetIdle()
		for _, frameStr := range pkt.Events {
			var ev map[string]any
			if json.Unmarshal([]byte(frameStr), &ev) != nil {
				continue
			}
			done, err := processEvent(ev)
			if err != nil {
				res, _, _, _ := finalizeTurn(false, false)
				return res, false, false, err
			}
			if done {
				return finalizeTurn(len(rawToolCalls) > 0, true)
			}
		}
		if len(rawToolCalls) > 0 && !turn.parallelTools {
			return finalizeTurn(true, true)
		}
	}
}

func parseOutpostErrorEvent(ev map[string]any) error {
	payloadBytes, _ := json.Marshal(ev["payload"])
	code, _ := ev["code"].(string)
	if code == "" {
		code, _ = ev["error_code"].(string)
	}
	msg, _ := ev["message"].(string)
	if msg == "" {
		msg, _ = ev["error"].(string)
	}
	var p struct {
		Code      string `json:"code"`
		ErrorCode string `json:"error_code"`
		ErrorType string `json:"error_type"`
		TypeCap   string `json:"errorType"`
		Message   string `json:"message"`
		Error     string `json:"error"`
		Detail    string `json:"detail"`
	}
	if json.Unmarshal(payloadBytes, &p) == nil {
		code = firstNonEmptyStr(code, p.Code, p.ErrorCode, p.ErrorType, p.TypeCap)
		msg = firstNonEmptyStr(msg, p.Message, p.Error, p.Detail)
	}
	if msg == "" {
		msg = errorText(payloadBytes, "upstream outpost error", code)
	}
	status := http.StatusBadGateway
	normCode := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(code))
	switch {
	case strings.Contains(normCode, "unauthorized") || strings.Contains(normCode, "expiredtoken") || strings.Contains(normCode, "invalidtoken"):
		status = http.StatusUnauthorized
	case strings.Contains(normCode, "accessdenied") || strings.Contains(normCode, "forbidden") || strings.Contains(normCode, "quota") || strings.Contains(normCode, "credit"):
		status = http.StatusForbidden
	case strings.Contains(normCode, "throttl") || strings.Contains(normCode, "ratelimit") || strings.Contains(normCode, "toomanyrequests"):
		status = http.StatusTooManyRequests
	case strings.Contains(normCode, "validation") || strings.Contains(normCode, "invalidrequest") || strings.Contains(normCode, "badrequest"):
		status = http.StatusBadRequest
	}
	return &UpstreamError{
		StatusCode: status,
		Code:       code,
		Err:        fmt.Errorf("%s", msg),
	}
}
