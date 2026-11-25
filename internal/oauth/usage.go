package oauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"quickrelay-studio/internal/config"
	"quickrelay-studio/internal/store"
)

var (
	ErrUsageUnavailable  = errors.New("official account usage is unavailable")
	ErrUsageUnauthorized = errors.New("official account usage authorization failed")
)

type UsageBucket struct {
	Used     *float64 `json:"used"`
	Limit    *float64 `json:"limit"`
	ResetsAt int64    `json:"resets_at"`
	Warning  string   `json:"warning,omitempty"`
}

type UsageSnapshot struct {
	Source           string       `json:"source"`
	Unit             string       `json:"unit"`
	Monthly          *UsageBucket `json:"monthly"`
	OverageMonthly   *UsageBucket `json:"overage_monthly"`
	SubscriptionType string       `json:"subscription_type,omitempty"`
}

type UsageFetchError struct {
	StatusCode int
	RetryAfter time.Duration
}

func (failure *UsageFetchError) Error() string {
	switch {
	case failure.StatusCode == http.StatusTooManyRequests:
		return "official account usage is temporarily rate limited"
	case failure.StatusCode >= 500:
		return "official account usage service is temporarily unavailable"
	case failure.StatusCode > 0:
		return "official account usage request failed"
	default:
		return "official account usage transport failed"
	}
}

func FetchUsage(ctx context.Context, account *store.Account, cfg *config.Config) (*UsageSnapshot, error) {
	if account == nil || cfg == nil {
		return nil, ErrUsageUnavailable
	}
	provider := strings.ToLower(strings.TrimSpace(account.Provider))
	if provider != "social" && provider != "enterprise" && provider != "gateway" && provider != "social_gateway" {
		return nil, ErrUsageUnavailable
	}
	if account.JWT() == "" {
		return nil, ErrUsageUnauthorized
	}
	endpoint, err := usageEndpoint(account)
	if err != nil {
		return nil, err
	}
	query := map[string]string{"_method": http.MethodGet}
	if cfg.Surface != "" {
		query["surfaceType"] = cfg.Surface
	}
	if cfg.AppVersion != "" {
		query["appVersion"] = cfg.AppVersion
	}
	body := []byte("{}")
	if isGatewayProvider(provider) {
		accountID := strings.TrimSpace(account.AWSAccountID)
		if len(accountID) != 12 {
			return nil, ErrUsageUnavailable
		}
		for _, digit := range accountID {
			if digit < '0' || digit > '9' {
				return nil, ErrUsageUnavailable
			}
		}
		params := make(url.Values, len(query)+3)
		for name, value := range query {
			params.Set(name, value)
		}
		params.Set("innerPath", "profile/usage")
		if cfg.WorkingSessionID != "" {
			params.Set("workingSessionId", cfg.WorkingSessionID)
		}
		if cfg.OutpostID != "" {
			params.Set("outpostId", cfg.OutpostID)
		}
		endpoint += "/accounts/" + accountID + "/invocations/rpc?" + params.Encode()
	} else {
		invocation := map[string]any{
			"path":        "profile/usage",
			"queryParams": query,
			"body":        map[string]any{},
		}
		if cfg.WorkingSessionID != "" {
			invocation["workingSessionId"] = cfg.WorkingSessionID
		}
		if cfg.OutpostID != "" {
			invocation["outpostId"] = cfg.OutpostID
		}
		body, err = json.Marshal(map[string]any{
			"eventType": "invokeAgent",
			"eventBody": map[string]any{"invokeAgent": invocation},
		})
		if err != nil {
			return nil, &UsageFetchError{}
		}
		endpoint += "/integration/quick-work/qbs-event"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &UsageFetchError{}
	}
	traceID, err := usageTraceID()
	if err != nil {
		return nil, &UsageFetchError{}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+account.JWT())
	request.Header.Set("x-amzn-qs-internal-trace-id", traceID)
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, usageTransportError(ctx)
	}
	defer response.Body.Close()
	if err := usageStatusError(response.StatusCode, response.Header); err != nil {
		return nil, err
	}
	const maxBody = 1 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return nil, usageTransportError(ctx)
	}
	if len(data) > maxBody {
		return nil, &UsageFetchError{StatusCode: http.StatusBadGateway}
	}
	return parseUsageSnapshot(data)
}

func usageEndpoint(account *store.Account) (string, error) {
	host := strings.TrimSpace(account.TenantURL)
	if host == "" {
		host = strings.TrimSpace(account.WSHost)
	}
	if strings.ContainsAny(host, "?#") {
		return "", ErrUsageUnavailable
	}
	switch {
	case strings.HasPrefix(host, "wss://"):
		host = "https://" + strings.TrimPrefix(host, "wss://")
	case strings.HasPrefix(host, "https://"):
	default:
		if strings.Contains(host, "://") {
			return "", ErrUsageUnavailable
		}
		host = "https://" + host
	}
	parsed, err := url.Parse(host)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawPath != "" || parsed.RawQuery != "" ||
		parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", ErrUsageUnavailable
	}
	parsed.Path = ""
	return parsed.String(), nil
}

func usageTraceID() (string, error) {
	var identifier [16]byte
	if _, err := rand.Read(identifier[:]); err != nil {
		return "", err
	}
	identifier[6] = identifier[6]&0x0f | 0x40
	identifier[8] = identifier[8]&0x3f | 0x80
	encoded := hex.EncodeToString(identifier[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func usageTransportError(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return &UsageFetchError{}
}

func usageStatusError(status int, headers http.Header) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ErrUsageUnauthorized
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented:
		return ErrUsageUnavailable
	case status >= 200 && status < 300:
		return nil
	default:
		return &UsageFetchError{StatusCode: status, RetryAfter: usageRetryAfter(headers.Get("Retry-After"))}
	}
}

func usageRetryAfter(raw string) time.Duration {
	const maximum = 5 * time.Minute
	seconds, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds >= int64(maximum/time.Second) {
			return maximum
		}
		return time.Duration(seconds) * time.Second
	}
	deadline, err := http.ParseTime(raw)
	if err != nil {
		return 0
	}
	delay := time.Until(deadline)
	if delay < 0 {
		return 0
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

func parseUsageSnapshot(data []byte) (*UsageSnapshot, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, ErrUsageUnavailable
	}
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte{'"'}) {
		var encoded string
		if err := json.Unmarshal(data, &encoded); err != nil {
			return nil, &UsageFetchError{StatusCode: http.StatusBadGateway}
		}
		data = []byte(encoded)
	}
	var outer struct {
		EventResponse *struct {
			InvokeAgentResponse json.RawMessage `json:"invokeAgentResponse"`
		} `json:"eventResponse"`
	}
	if err := json.Unmarshal(data, &outer); err != nil {
		return nil, &UsageFetchError{StatusCode: http.StatusBadGateway}
	}
	if outer.EventResponse != nil && len(outer.EventResponse.InvokeAgentResponse) > 0 {
		data = outer.EventResponse.InvokeAgentResponse
	}
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte{'"'}) {
		var encoded string
		if err := json.Unmarshal(data, &encoded); err != nil {
			return nil, &UsageFetchError{StatusCode: http.StatusBadGateway}
		}
		data = []byte(encoded)
	}
	var envelope struct {
		Type         string            `json:"type"`
		Status       *int              `json:"status"`
		Headers      map[string]string `json:"headers"`
		Body         json.RawMessage   `json:"body"`
		BodyEncoding string            `json:"body_encoding"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, &UsageFetchError{StatusCode: http.StatusBadGateway}
	}
	if envelope.Type == "api_response" {
		status := http.StatusOK
		if envelope.Status != nil {
			status = *envelope.Status
		}
		headers := make(http.Header, len(envelope.Headers))
		for name, value := range envelope.Headers {
			headers.Set(name, value)
		}
		if err := usageStatusError(status, headers); err != nil {
			return nil, err
		}
		data = envelope.Body
		if bytes.HasPrefix(bytes.TrimSpace(data), []byte{'"'}) {
			var encoded string
			if err := json.Unmarshal(data, &encoded); err != nil {
				return nil, &UsageFetchError{StatusCode: http.StatusBadGateway}
			}
			data = []byte(encoded)
		}
		if envelope.BodyEncoding == "base64" {
			decoded, err := base64.StdEncoding.DecodeString(string(data))
			if err != nil {
				return nil, &UsageFetchError{StatusCode: http.StatusBadGateway}
			}
			data = decoded
		} else if envelope.BodyEncoding != "" && envelope.BodyEncoding != "json" {
			return nil, &UsageFetchError{StatusCode: http.StatusBadGateway}
		}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, ErrUsageUnavailable
	}
	var payload struct {
		Monthly        *usageWireBucket `json:"monthly"`
		OverageMonthly *usageWireBucket `json:"overage_monthly"`
		Subscription   *struct {
			SubscriptionType string `json:"subscription_type"`
		} `json:"subscription"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, &UsageFetchError{StatusCode: http.StatusBadGateway}
	}
	monthly, err := normalizeUsageBucket(payload.Monthly)
	if err != nil {
		return nil, err
	}
	overage, err := normalizeUsageBucket(payload.OverageMonthly)
	if err != nil {
		return nil, err
	}
	if monthly == nil && overage == nil {
		return nil, ErrUsageUnavailable
	}
	snapshot := &UsageSnapshot{
		Source: "quickwork_profile_usage", Unit: "minutes", Monthly: monthly, OverageMonthly: overage,
	}
	if payload.Subscription != nil {
		plan := payload.Subscription.SubscriptionType
		if len(plan) <= 128 && strings.Trim(plan, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-") == "" {
			snapshot.SubscriptionType = plan
		}
	}
	return snapshot, nil
}

type usageWireBucket struct {
	Used     *float64 `json:"used"`
	Limit    *float64 `json:"limit"`
	ResetsAt float64  `json:"resets_at"`
	Warning  *struct {
		Severity string `json:"severity"`
	} `json:"warning"`
}

func normalizeUsageBucket(bucket *usageWireBucket) (*UsageBucket, error) {
	if bucket == nil || (bucket.Used == nil && bucket.Limit == nil) {
		return nil, nil
	}
	if (bucket.Used != nil && *bucket.Used < 0) || (bucket.Limit != nil && *bucket.Limit < 0) ||
		bucket.ResetsAt < 0 || bucket.ResetsAt > 253402300799 {
		return nil, &UsageFetchError{StatusCode: http.StatusBadGateway}
	}
	normalized := &UsageBucket{Used: bucket.Used, Limit: bucket.Limit, ResetsAt: int64(bucket.ResetsAt)}
	if bucket.Warning != nil {
		switch bucket.Warning.Severity {
		case "info", "warning", "error":
			normalized.Warning = bucket.Warning.Severity
		}
	}
	return normalized, nil
}
