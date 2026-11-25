package oauth

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"quickrelay-studio/internal/store"
)

type BrowserLoginStatus struct {
	Stage     string `json:"stage"`
	URL       string `json:"url,omitempty"`
	ExpiresAt int64  `json:"expires_at"`
	AccountID int64  `json:"account_id,omitempty"`
}

type BrowserLogin struct {
	mu       sync.Mutex
	submitMu sync.Mutex
	flow     *loginFlow
	status   BrowserLoginStatus
	deadline time.Time
	cancel   context.CancelFunc
}

func NewBrowserLogin(opts LoginOptions, st *store.Store) (*BrowserLogin, error) {
	if st == nil {
		return nil, errors.New("account store is required")
	}
	opts = normalizeLoginOptions(opts)
	if opts.Port > 65535 {
		return nil, errors.New("callback port must be 1-65535")
	}
	opts.Region = strings.ToLower(strings.TrimSpace(opts.Region))
	if len(opts.Region) > 64 || strings.Trim(opts.Region, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
		return nil, errors.New("invalid discovery region")
	}
	opts.OpenBrowser = false
	opts.Logf = func(string, ...any) {}
	flow, err := newLoginFlow(opts, st, opts.Port)
	if err != nil {
		return nil, errors.New("could not initialize browser login")
	}
	deadline := time.Now().Add(opts.Timeout)
	return &BrowserLogin{
		flow:     flow,
		deadline: deadline,
		status: BrowserLoginStatus{
			Stage:     "discovery",
			URL:       discoveryURL(flow.provider, flow.stage, flow.region, flow.nonce, flow.port, opts.AppVersion),
			ExpiresAt: deadline.Unix(),
		},
	}, nil
}

func (login *BrowserLogin) Status() BrowserLoginStatus {
	login.mu.Lock()
	defer login.mu.Unlock()
	return login.statusLocked()
}

func (login *BrowserLogin) Cancel() BrowserLoginStatus {
	login.mu.Lock()
	defer login.mu.Unlock()
	status := login.statusLocked()
	if status.Stage == "discovery" || status.Stage == "authorize" {
		login.closeLocked("cancelled")
	}
	return login.status
}

func (login *BrowserLogin) Submit(ctx context.Context, callbackURL string) (BrowserLoginStatus, error) {
	if !login.submitMu.TryLock() {
		return login.Status(), errors.New("a callback is already being processed")
	}
	defer login.submitMu.Unlock()

	login.mu.Lock()
	status := login.statusLocked()
	if err := browserLoginClosed(status.Stage); err != nil {
		login.mu.Unlock()
		return status, err
	}
	flow := login.flow
	operationCtx, cancel := context.WithDeadline(ctx, login.deadline)
	login.cancel = cancel
	login.mu.Unlock()

	next, err := submitBrowserCallback(operationCtx, flow, callbackURL)
	requestErr := operationCtx.Err()
	cancel()

	login.mu.Lock()
	defer login.mu.Unlock()
	login.cancel = nil
	status = login.statusLocked()
	if closedErr := browserLoginClosed(status.Stage); closedErr != nil {
		return status, closedErr
	}
	if err != nil {
		if requestErr != nil {
			return status, errors.New("callback request cancelled or timed out")
		}
		return status, err
	}
	next.ExpiresAt = status.ExpiresAt
	login.status = next
	if next.Stage == "complete" {
		login.flow = nil
	}
	return login.status, nil
}

func (login *BrowserLogin) statusLocked() BrowserLoginStatus {
	if (login.status.Stage == "discovery" || login.status.Stage == "authorize") && !time.Now().Before(login.deadline) {
		login.closeLocked("expired")
	}
	return login.status
}

func (login *BrowserLogin) closeLocked(stage string) {
	if login.cancel != nil {
		login.cancel()
		login.cancel = nil
	}
	login.flow = nil
	login.status.Stage = stage
	login.status.URL = ""
}

func browserLoginClosed(stage string) error {
	switch stage {
	case "complete":
		return errors.New("browser login is already complete")
	case "expired":
		return errors.New("browser login expired; start a new login")
	case "cancelled":
		return errors.New("browser login was cancelled")
	default:
		return nil
	}
}

func submitBrowserCallback(ctx context.Context, flow *loginFlow, callbackURL string) (BrowserLoginStatus, error) {
	if len(callbackURL) > 64<<10 {
		return BrowserLoginStatus{}, errors.New("callback URL is too long")
	}
	if ctx.Err() != nil {
		return BrowserLoginStatus{}, errors.New("callback request cancelled or timed out")
	}
	params, err := parseCallbackURL(callbackURL)
	if err != nil {
		return BrowserLoginStatus{}, errors.New("invalid callback URL")
	}
	switch classifyCallback(params) {
	case callbackDiscovery, callbackDiscoveryError:
		if subtle.ConstantTimeCompare([]byte(params.Get("nonce")), []byte(flow.nonce)) != 1 {
			return BrowserLoginStatus{}, errors.New("discovery nonce mismatch")
		}
		if params.Get("error") != "" {
			return BrowserLoginStatus{}, errors.New("upstream sign-in was rejected")
		}
		if err := validateBrowserDiscovery(params); err != nil {
			return BrowserLoginStatus{}, err
		}
		location, err := flow.processDiscovery(ctx, params)
		if err != nil {
			if statusOf(err) < 500 {
				return BrowserLoginStatus{}, errors.New("invalid discovery callback parameters")
			}
			return BrowserLoginStatus{}, errors.New("upstream login discovery failed")
		}
		if !validBrowserEndpoint(location) {
			return BrowserLoginStatus{}, errors.New("upstream returned an invalid authorize URL")
		}
		return BrowserLoginStatus{Stage: "authorize", URL: location}, nil
	case callbackCode, callbackCodeError:
		if !flow.discoveryDone {
			return BrowserLoginStatus{}, errors.New("submit the discovery callback before the authorization callback")
		}
		if subtle.ConstantTimeCompare([]byte(params.Get("state")), []byte(flow.state)) != 1 {
			return BrowserLoginStatus{}, errors.New("OAuth state mismatch")
		}
		if params.Get("error") != "" {
			return BrowserLoginStatus{}, errors.New("upstream sign-in was rejected")
		}
		account, err := flow.completeLogin(ctx, params)
		if err != nil {
			return BrowserLoginStatus{}, errors.New("upstream login or account persistence failed; start a new login if the code was consumed")
		}
		return BrowserLoginStatus{Stage: "complete", AccountID: account.ID}, nil
	case callbackAuthorize:
		return BrowserLoginStatus{}, errors.New("open the authorize URL in the browser, then submit its callback URL")
	case callbackStart:
		return BrowserLoginStatus{}, errors.New("open the discovery URL in the browser, then submit its callback URL")
	default:
		return BrowserLoginStatus{}, errors.New("unrecognized callback URL; expected a discovery or authorization callback")
	}
}

func validateBrowserDiscovery(params url.Values) error {
	for _, name := range []string{"authEndpoint", "auth_endpoint", "tokenEndpoint", "token_endpoint", "issuerUrl", "issuer_url", "issuer"} {
		if value := params.Get(name); value != "" && !validBrowserEndpoint(value) {
			return errors.New("discovery endpoints must be HTTPS URLs without embedded credentials")
		}
	}
	for _, name := range []string{"tenantUrl", "tenant_url"} {
		value := params.Get(name)
		if value == "" {
			continue
		}
		if !strings.Contains(value, "://") {
			value = "https://" + value
		}
		if !validBrowserEndpoint(value) {
			return errors.New("discovery endpoints must be HTTPS URLs without embedded credentials")
		}
		params.Set(name, value)
	}
	for _, name := range []string{"idcRegion", "idc_region", "region"} {
		value := params.Get(name)
		if len(value) > 64 || strings.Trim(value, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
			return errors.New("invalid discovery region")
		}
	}
	return nil
}

func validBrowserEndpoint(raw string) bool {
	endpoint, err := url.Parse(raw)
	return err == nil && endpoint.Scheme == "https" && endpoint.Hostname() != "" && endpoint.User == nil && endpoint.Opaque == ""
}
