package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	"quickrelay-studio/internal/accounts"
)

var (
	ErrEmptyResponse            = errors.New("upstream returned no visible text or tool calls")
	ErrInvalidAttachment        = errors.New("invalid attachment")
	ErrRemoteSandboxToolBlocked = errors.New("remote cloud sandbox tool blocked")
	ErrThreadAgentMismatch      = errors.New("explicit X-Thread-Id conflicts with current chat_agent_id or has unknown agent ownership; start a new conversation with a fresh X-Thread-Id")
	ErrExplicitThreadConflict   = errors.New("explicit X-Thread-Id cannot be safely resumed on the current native outpost session; start a new conversation with a fresh X-Thread-Id")
	ErrOutpostToolPolicyMismatch = errors.New("upstream tool call did not match requested tool_choice policy")
)

// RemoteToolBlockedError 表示模型在启用客户端工具桥接时越权调用了远端云端沙箱/搜索/执行工具并被实时阻断。
type RemoteToolBlockedError struct {
	ToolName string
	Reason   string
}

func (e *RemoteToolBlockedError) Error() string {
	name := strings.TrimSpace(e.ToolName)
	if name == "" {
		name = "unknown"
	}
	reason := strings.TrimSpace(e.Reason)
	if reason == "" {
		return fmt.Sprintf("%s: %s — use local client tools instead of AWS Quick remote sandbox tools", ErrRemoteSandboxToolBlocked.Error(), name)
	}
	return fmt.Sprintf("%s: %s (%s) — use local client tools instead of AWS Quick remote sandbox tools", ErrRemoteSandboxToolBlocked.Error(), name, reason)
}

func (e *RemoteToolBlockedError) Unwrap() error {
	return ErrRemoteSandboxToolBlocked
}

type UpstreamError struct {
	StatusCode int
	Code       string
	RetryAfter time.Duration
	Err        error
}

func (err *UpstreamError) Error() string {
	if err.Err != nil {
		return err.Err.Error()
	}
	return "upstream request failed"
}

func (err *UpstreamError) Unwrap() error {
	return err.Err
}

type retryAction int

const (
	retryStop retryAction = iota
	retryTemporary
	retryRefresh
	retryNextAccount
	retryNewSession
)

func retryActionFor(err error) retryAction {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrUserMessageTooLarge) || errors.Is(err, ErrInvalidAttachment) || errors.Is(err, ErrRemoteSandboxToolBlocked) || errors.Is(err, ErrThreadAgentMismatch) || errors.Is(err, ErrExplicitThreadConflict) || errors.Is(err, ErrOutpostToolPolicyMismatch) {
		return retryStop
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return retryTemporary
	}
	if errors.Is(err, ErrEmptyResponse) {
		return retryTemporary
	}
	var upstreamErr *UpstreamError
	if errors.As(err, &upstreamErr) {
		if action, known := retryActionForCode(upstreamErr.Code); known {
			return action
		}
		if action, known := retryActionForMessage(upstreamErr.Error(), upstreamErr.StatusCode); known {
			return action
		}
		switch {
		case upstreamErr.StatusCode == http.StatusUnauthorized:
			return retryRefresh
		case upstreamErr.StatusCode == http.StatusForbidden || upstreamErr.StatusCode == http.StatusPaymentRequired:
			return retryNextAccount
		case upstreamErr.StatusCode == http.StatusRequestTimeout || upstreamErr.StatusCode == http.StatusTooManyRequests:
			return retryTemporary
		case upstreamErr.StatusCode >= 500 && upstreamErr.StatusCode <= 599:
			return retryTemporary
		case upstreamErr.StatusCode >= 400 && upstreamErr.StatusCode <= 499:
			return retryStop
		}
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		switch closeErr.Code {
		case websocket.CloseNormalClosure, websocket.CloseNoStatusReceived,
			websocket.CloseAbnormalClosure, websocket.CloseGoingAway, websocket.CloseInternalServerErr,
			websocket.CloseServiceRestart, websocket.CloseTryAgainLater:
			return retryTemporary
		default:
			return retryStop
		}
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return retryStop
		}
		if networkErr.Timeout() || networkErr.Temporary() || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) {
			return retryTemporary
		}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.EPIPE) {
		return retryTemporary
	}
	return retryStop
}

// shouldResetConversation 判断单轮请求失败后是否真正需要销毁既有会话映射：
// 用户取消（Esc）、单轮超时、全账号冷却、附件校验失败或已有成功历史记录时的临时错误/单轮内容拦截均保留 thread_id 与已落盘指纹，
// 仅当上游明确返回会话过期/失效或当前账号不可用需要切换账号时才清理重建。
func shouldResetConversation(conv *conversation, err error) bool {
	if err == nil || conv == nil {
		return false
	}
	if errors.Is(err, ErrRemoteSandboxToolBlocked) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, accounts.ErrAllAccountsCoolingDown) || errors.Is(err, ErrInvalidAttachment) {
		return false
	}
	action := retryActionFor(err)
	if action == retryNewSession || action == retryNextAccount {
		return true
	}
	if conv.threadID != "" && len(conv.msgHashes) > 0 {
		return false
	}
	return true
}

func retryActionForCode(code string) (retryAction, bool) {
	normalized := strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(code)))
	normalized = strings.TrimSuffix(normalized, "exception")
	switch normalized {
	case "expiredtoken", "tokenexpired", "invalidtoken", "invalidaccesstoken", "accesstokenexpired", "idtokenexpired",
		"unauthorized", "unauthenticated", "notauthenticated", "authenticationfailed":
		return retryRefresh, true
	case "accessdenied", "forbidden", "permissiondenied", "insufficientcredits", "insufficientquota", "quotaexceeded",
		"resourceexhausted", "creditsexhausted", "subscriptionrequired", "paymentrequired", "attachmenttransportunsupported":
		return retryNextAccount, true
	case "sessionnotfound", "threadnotfound", "conversationnotfound", "invalidsession", "sessionexpired",
		"invalidthread", "threadexpired", "sessiondoesnotexist":
		return retryNewSession, true
	case "validation", "validationerror", "invalidargument", "invalidrequest", "invalidrequesterror", "badrequest",
		"malformedrequest", "prompttoolarge", "messagetoolarge", "requesttoolarge", "contextlengthexceeded",
		"attachmentblockedbydlp":
		return retryStop, true
	case "throttling", "throttled", "toomanyrequests", "ratelimitexceeded", "ratelimited", "serviceunavailable", "unavailable",
		"internalservererror", "internalerror", "timeout", "requesttimeout", "gatewaytimeout", "upstreamtimeout",
		"overloaded", "serverbusy", "temporarilyunavailable":
		return retryTemporary, true
	default:
		return retryStop, false
	}
}

// retryActionForMessage 根据无错误码上游报错文本判断是否可安全切换账号重试；
// 内容安全拦截类提示严禁重试，避免拿同一段内容连续触发多个账号风控。
func retryActionForMessage(msg string, status int) (retryAction, bool) {
	lower := strings.ToLower(strings.TrimSpace(msg))
	if lower == "" {
		return retryStop, false
	}
	for _, stopPhrase := range []string{
		"content filter",
		"automated check",
		"guardrail",
		"policy",
		"blocked",
	} {
		if strings.Contains(lower, stopPhrase) {
			return retryStop, true
		}
	}
	if status == 0 || status >= 500 || status == http.StatusTooManyRequests || status == http.StatusRequestTimeout {
		for _, transientPhrase := range []string{
			"something went wrong",
			"temporary issue",
			"please try again",
			"try again later",
			"temporarily unavailable",
			"overloaded",
			"service unavailable",
			"rate limit",
			"throttl",
		} {
			if strings.Contains(lower, transientPhrase) {
				return retryTemporary, true
			}
		}
	}
	return retryStop, false
}

// isRateLimitTemporary 判断同属 retryTemporary 的故障是否由速率/并发限流触发。
// 命中限流时应立即换号避让；网络抖动/握手超时等非限流故障则优先在原绑定账号原地重连一次保缓存。
func isRateLimitTemporary(err error) bool {
	if err == nil {
		return false
	}
	var upstreamErr *UpstreamError
	if errors.As(err, &upstreamErr) {
		if upstreamErr.StatusCode == http.StatusTooManyRequests {
			return true
		}
		normalized := strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(upstreamErr.Code)))
		normalized = strings.TrimSuffix(normalized, "exception")
		switch normalized {
		case "throttling", "throttled", "toomanyrequests", "ratelimitexceeded", "ratelimited":
			return true
		}
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "rate limit") || strings.Contains(lower, "throttl")
}

func invalidChatRequest(err error) bool {
	if errors.Is(err, ErrUserMessageTooLarge) || errors.Is(err, ErrInvalidAttachment) || errors.Is(err, ErrThreadAgentMismatch) || errors.Is(err, ErrExplicitThreadConflict) {
		return true
	}
	var upstreamErr *UpstreamError
	if !errors.As(err, &upstreamErr) {
		return false
	}
	if action, known := retryActionForCode(upstreamErr.Code); known {
		return action == retryStop
	}
	switch upstreamErr.StatusCode {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

func chatErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, ErrThreadAgentMismatch):
		return http.StatusConflict, "thread_agent_mismatch"
	case errors.Is(err, ErrExplicitThreadConflict):
		return http.StatusConflict, "thread_conflict"
	case errors.Is(err, ErrOutpostToolPolicyMismatch):
		return http.StatusBadGateway, "upstream_tool_mismatch"
	case errors.Is(err, ErrRemoteSandboxToolBlocked):
		return http.StatusBadGateway, "remote_sandbox_tool_blocked"
	case errors.Is(err, accounts.ErrAllAccountsCoolingDown):
		return http.StatusTooManyRequests, "rate_limit_exceeded"
	case errors.Is(err, accounts.ErrNoAccounts), errors.Is(err, accounts.ErrNoAvailableAccounts):
		return http.StatusUnauthorized, "upstream_session_unavailable"
	case errors.Is(err, ErrUserMessageTooLarge):
		return http.StatusBadRequest, "request_too_large"
	case invalidChatRequest(err):
		return http.StatusBadRequest, "invalid_request_error"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "upstream_timeout"
	default:
		return http.StatusBadGateway, "upstream_error"
	}
}

// parseRetryAfterHeader 解析标准 HTTP Retry-After 头（支持十进制秒数与 HTTP-date 格式）。
func parseRetryAfterHeader(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(raw, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(raw); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// rateLimitCooldownFor 计算明确限流触发的账号冷却时长：优先采用上游可信 Retry-After，否则使用短默认值。
func rateLimitCooldownFor(err error) time.Duration {
	var upstreamErr *UpstreamError
	if errors.As(err, &upstreamErr) && upstreamErr.RetryAfter > 0 {
		d := upstreamErr.RetryAfter
		if d > accounts.MaxRateLimitCooldown {
			return accounts.MaxRateLimitCooldown
		}
		return d
	}
	return accounts.DefaultRateLimitCooldown
}

// writeCooldownRetryHeader 在全账号均处于冷却期返回 429 时附带标准 Retry-After 秒数头。
func writeCooldownRetryHeader(w http.ResponseWriter, err error) {
	var cdErr *accounts.CooldownError
	if !errors.As(err, &cdErr) || cdErr.RetryAfter <= 0 {
		return
	}
	secs := int((cdErr.RetryAfter + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
}

func waitForRetry(ctx context.Context, attempt int) error {
	delay := 200 * time.Millisecond
	for count := 1; count < attempt && delay < 2*time.Second; count++ {
		delay *= 2
	}
	if delay > 2*time.Second {
		delay = 2 * time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
