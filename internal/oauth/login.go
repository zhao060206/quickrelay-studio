package oauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"quickrelay-studio/internal/store"
)

// LoginOptions controls one browser login.
type LoginOptions struct {
	Provider    string
	Stage       string
	Region      string
	Port        int
	Timeout     time.Duration
	AppVersion  string
	OpenBrowser bool
	Logf        func(format string, args ...any)
}

type loginFlow struct {
	opts  LoginOptions
	store *store.Store

	provider    string
	stage       string
	region      string
	port        int
	redirectURI string
	state       string
	nonce       string
	verifier    string
	challenge   string

	discovery     discovery
	clientID      string
	secret        string
	oidcBase      string
	discoveryDone bool
	authorizeURL  string

	done chan struct{}
	once sync.Once
	acct *store.Account
	fail error
}

// Login runs the native browser OAuth flow and stores the resulting account.
// It returns once the browser callback completes, fails, or the context ends.
func Login(ctx context.Context, opts LoginOptions, st *store.Store) (*store.Account, error) {
	opts = normalizeLoginOptions(opts)
	provider := normalizedProvider(opts.Provider)
	stage := normalizedStage(opts.Stage)

	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", opts.Port))
	if err != nil {
		lnFallback, fbErr := net.Listen("tcp4", "127.0.0.1:0")
		if fbErr != nil {
			return nil, fmt.Errorf("cannot listen on 127.0.0.1:%d: %w (stop the process using that port and retry)", opts.Port, err)
		}
		fbPort := lnFallback.Addr().(*net.TCPAddr).Port
		opts.Logf("callback port %d is busy (%v), automatically switched to free port %d", opts.Port, err, fbPort)
		ln = lnFallback
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	f, err := newLoginFlow(opts, st, port)
	if err != nil {
		return nil, err
	}

	srv := &http.Server{
		Handler:           http.HandlerFunc(f.handle),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			f.finish(nil, err)
		}
	}()
	// 同步监听 IPv6 环回地址 [::1]，防止 Windows Edge 将 localhost 解析至 ::1 时出现拒绝连接
	if ln6, err6 := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port)); err6 == nil {
		defer ln6.Close()
		go func() {
			if sErr := srv.Serve(ln6); sErr != nil && !errors.Is(sErr, http.ErrServerClosed) {
				opts.Logf("ipv6 loopback callback listener stopped: %v", sErr)
			}
		}()
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	loginURL := discoveryURL(provider, stage, f.region, f.nonce, port, opts.AppVersion)
	opts.Logf("provider=%s stage=%s listening=127.0.0.1:%d", provider, stage, port)
	opts.Logf("Open this URL to sign in:")
	opts.Logf("%s", loginURL)
	opts.Logf("waiting for login (timeout %s)", opts.Timeout)
	if opts.OpenBrowser {
		if err := openBrowser(loginURL); err != nil {
			opts.Logf("browser launch failed: %v", err)
		}
	}

	waitCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	select {
	case <-f.done:
		if f.fail != nil {
			return nil, f.fail
		}
		return f.acct, nil
	case <-waitCtx.Done():
		return nil, waitError(waitCtx)
	}
}

func normalizeLoginOptions(opts LoginOptions) LoginOptions {
	if opts.Port <= 0 {
		opts.Port = 18080
	}
	if opts.Timeout <= 0 {
		opts.Timeout = time.Hour
	}
	if opts.AppVersion == "" {
		opts.AppVersion = "1.2.4660"
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	return opts
}

func newLoginFlow(opts LoginOptions, st *store.Store, port int) (*loginFlow, error) {
	state, err := randomToken()
	if err != nil {
		return nil, err
	}
	nonce, err := randomToken()
	if err != nil {
		return nil, err
	}
	verifier, err := randomToken()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(verifier))
	provider := normalizedProvider(opts.Provider)
	return &loginFlow{
		opts:        opts,
		store:       st,
		provider:    provider,
		stage:       normalizedStage(opts.Stage),
		region:      strings.TrimSpace(opts.Region),
		port:        port,
		redirectURI: redirectURI(provider, port),
		state:       state,
		nonce:       nonce,
		verifier:    verifier,
		challenge:   base64.RawURLEncoding.EncodeToString(sum[:]),
		done:        make(chan struct{}),
	}, nil
}

func waitError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("timed out waiting for browser callback")
	}
	return ctx.Err()
}

func (f *loginFlow) finish(acct *store.Account, err error) {
	f.once.Do(func() {
		f.acct = acct
		f.fail = err
		close(f.done)
	})
}

func (f *loginFlow) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.URL.Path {
	case "/favicon.ico":
		w.WriteHeader(http.StatusNoContent)
	case "/callback":
		f.handleDiscovery(w, r)
	case "/", "/index.html":
		f.handleCode(w, r)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (f *loginFlow) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	location, err := f.processDiscovery(r.Context(), r.URL.Query())
	if err != nil {
		f.failPage(w, statusOf(err), err.Error())
		return
	}
	http.Redirect(w, r, location, http.StatusFound)
}

type stepError struct {
	status  int
	message string
}

func (e *stepError) Error() string { return e.message }

func stepErrorf(status int, format string, args ...any) error {
	return &stepError{status: status, message: fmt.Sprintf(format, args...)}
}

func statusOf(err error) int {
	var se *stepError
	if errors.As(err, &se) {
		return se.status
	}
	return http.StatusInternalServerError
}

// processDiscovery validates the browser discovery callback and returns the
// upstream authorize URL the user must open next.
func (f *loginFlow) processDiscovery(ctx context.Context, q url.Values) (string, error) {
	gotNonce := q.Get("nonce")
	if gotNonce == "" || subtle.ConstantTimeCompare([]byte(gotNonce), []byte(f.nonce)) != 1 {
		return "", stepErrorf(http.StatusBadRequest, "discovery nonce mismatch")
	}
	if v := q.Get("error"); v != "" {
		return "", stepErrorf(http.StatusBadRequest, "login failed: %s %s", v, q.Get("error_description"))
	}
	if f.discoveryDone {
		return f.authorizeURL, nil
	}

	if isEnterpriseDiscovery(f.provider) {
		f.discovery = discovery{
			AuthEndpoint:  firstQuery(q, "authEndpoint", "auth_endpoint"),
			TokenEndpoint: firstQuery(q, "tokenEndpoint", "token_endpoint"),
			IssuerURL:     firstQuery(q, "issuer"),
			ClientID:      firstQuery(q, "clientId", "client_id"),
			ClientSecret:  firstQuery(q, "clientSecret", "client_secret"),
			TenantURL:     firstQuery(q, "tenantUrl", "tenant_url"),
			Region:        firstQuery(q, "region"),
		}
		if f.discovery.AuthEndpoint == "" || f.discovery.TokenEndpoint == "" {
			return "", stepErrorf(http.StatusBadRequest, "discovery response is missing authEndpoint/tokenEndpoint")
		}
		f.clientID = f.discovery.ClientID
		f.secret = f.discovery.ClientSecret
		f.opts.Logf("discovery: tenant=%s region=%s", emptyAs(f.discovery.TenantURL, "-"), emptyAs(f.discovery.Region, "-"))

		params := url.Values{
			"response_type":         {"code"},
			"client_id":             {f.clientID},
			"redirect_uri":          {f.redirectURI},
			"scope":                 {enterpriseScope},
			"state":                 {f.state},
			"code_challenge":        {f.challenge},
			"code_challenge_method": {"S256"},
			"access_type":           {"offline"},
			"prompt":                {"consent"},
		}
		f.authorizeURL = appendQuery(f.discovery.AuthEndpoint, params)
		f.discoveryDone = true
		return f.authorizeURL, nil
	}

	f.discovery = discovery{
		IssuerURL: firstQuery(q, "issuerUrl", "issuer_url", "issuer"),
		TenantURL: firstQuery(q, "tenantUrl", "tenant_url"),
		IDCRegion: firstQuery(q, "idcRegion", "idc_region"),
	}
	if f.discovery.IssuerURL == "" {
		return "", stepErrorf(http.StatusBadRequest, "discovery callback did not provide issuerUrl")
	}
	idcRegion := f.discovery.IDCRegion
	if idcRegion == "" {
		idcRegion = f.region
	}
	if idcRegion == "" {
		idcRegion = "us-east-1"
	}
	f.discovery.IDCRegion = idcRegion
	f.discovery.Region = f.region

	client, err := f.registerIDCClient(ctx, idcRegion)
	if err != nil {
		return "", stepErrorf(http.StatusBadGateway, "client registration failed: %v", err)
	}
	f.clientID = client.id
	f.secret = client.secret
	if f.clientID == "" {
		return "", stepErrorf(http.StatusBadGateway, "client registration returned no clientId")
	}
	f.opts.Logf("discovery: tenant=%s idc_region=%s", emptyAs(f.discovery.TenantURL, "-"), idcRegion)

	params := url.Values{
		"response_type":         {"code"},
		"client_id":             {f.clientID},
		"redirect_uri":          {f.redirectURI},
		"scope":                 {strings.Join(scopesFor(f.stage), " ")},
		"state":                 {f.state},
		"code_challenge":        {f.challenge},
		"code_challenge_method": {"S256"},
	}
	f.authorizeURL = appendQuery(f.oidcEndpoint(idcRegion)+"/authorize", params)
	f.discoveryDone = true
	return f.authorizeURL, nil
}

type clientCredentials struct {
	id     string
	secret string
}

func (f *loginFlow) oidcEndpoint(region string) string {
	if f.oidcBase != "" {
		return strings.TrimRight(f.oidcBase, "/")
	}
	return "https://oidc." + region + ".amazonaws.com"
}

func (f *loginFlow) registerIDCClient(ctx context.Context, idcRegion string) (clientCredentials, error) {
	payload := map[string]any{
		"clientName":   "AmazonQuick-Desktop",
		"clientType":   "public",
		"scopes":       scopesFor(f.stage),
		"grantTypes":   []string{"authorization_code", "refresh_token"},
		"redirectUris": []string{f.redirectURI},
		"issuerUrl":    f.discovery.IssuerURL,
	}
	raw, err := postJSON(ctx, f.oidcEndpoint(idcRegion)+"/client/register", payload, nil)
	if err != nil {
		return clientCredentials{}, err
	}
	return clientCredentials{
		id:     firstValue(raw, "clientId", "client_id"),
		secret: firstValue(raw, "clientSecret", "client_secret"),
	}, nil
}

func (f *loginFlow) handleCode(w http.ResponseWriter, r *http.Request) {
	a, err := f.completeLogin(r.Context(), r.URL.Query())
	if err != nil {
		f.failPage(w, statusOf(err), err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, successHTML(accountLabel(a)))
	f.finish(a, nil)
}

// completeLogin exchanges the OAuth code, fetches user info and stores the
// account. It is shared by the localhost callback server and manual mode.
func (f *loginFlow) completeLogin(ctx context.Context, q url.Values) (*store.Account, error) {
	if v := q.Get("error"); v != "" {
		return nil, stepErrorf(http.StatusBadRequest, "login failed: %s %s", v, q.Get("error_description"))
	}
	gotState := q.Get("state")
	if gotState == "" || subtle.ConstantTimeCompare([]byte(gotState), []byte(f.state)) != 1 {
		return nil, stepErrorf(http.StatusBadRequest, "OAuth state mismatch")
	}
	code := q.Get("code")
	if code == "" {
		return nil, stepErrorf(http.StatusBadRequest, "OAuth callback has no code")
	}

	t, err := f.exchangeCode(ctx, code)
	if err != nil {
		return nil, stepErrorf(http.StatusBadGateway, "token exchange failed: %v", err)
	}
	info, err := fetchUserInfo(ctx, f.provider, f.stage, f.discovery, f.discovery.TenantURL, t.AccessToken)
	if err != nil {
		return nil, stepErrorf(http.StatusBadGateway, "user info failed: %v", err)
	}

	region := f.discovery.Region
	if region == "" {
		region = f.region
	}
	a := &store.Account{
		Provider:      f.provider,
		BuildStage:    f.stage,
		Region:        region,
		TenantURL:     f.discovery.TenantURL,
		ClientID:      f.clientID,
		ClientSecret:  f.secret,
		IssuerURL:     f.discovery.IssuerURL,
		IDCRegion:     f.discovery.IDCRegion,
		AuthEndpoint:  f.discovery.AuthEndpoint,
		TokenEndpoint: f.discovery.TokenEndpoint,
		RedirectURI:   f.redirectURI,
	}
	if a.TokenEndpoint == "" {
		a.TokenEndpoint = f.oidcEndpoint(a.IDCRegion) + "/token"
	}
	if err := applySession(a, t, info); err != nil {
		return nil, stepErrorf(http.StatusBadGateway, "%v", err)
	}
	if err := f.store.Save(ctx, a); err != nil {
		return nil, stepErrorf(http.StatusInternalServerError, "save account: %v", err)
	}

	f.opts.Logf("LOGIN_OK %s account_id=%d", info.Display(), a.ID)
	f.opts.Logf("SESSION_SAVED account=%d ws_host=%s expires=%s", a.ID, a.WSHost, formatEpoch(a.ExpiresAt))
	return a, nil
}

func accountLabel(a *store.Account) string {
	if a == nil {
		return ""
	}
	for _, v := range []string{a.Email, a.DisplayName, a.AccountName} {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (f *loginFlow) exchangeCode(ctx context.Context, code string) (tokens, error) {
	if isEnterpriseDiscovery(f.provider) {
		form := url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"redirect_uri":  {f.redirectURI},
			"client_id":     {f.clientID},
			"code_verifier": {f.verifier},
		}
		if f.secret != "" {
			form.Set("client_secret", f.secret)
		}
		raw, err := postForm(ctx, f.discovery.TokenEndpoint, form)
		if err != nil {
			return tokens{}, err
		}
		return tokensFromMap(raw), nil
	}
	raw, err := postJSON(ctx, f.oidcEndpoint(f.discovery.IDCRegion)+"/token", map[string]any{
		"clientId":     f.clientID,
		"clientSecret": f.secret,
		"grantType":    "authorization_code",
		"code":         code,
		"redirectUri":  f.redirectURI,
		"codeVerifier": f.verifier,
	}, nil)
	if err != nil {
		return tokens{}, err
	}
	return tokensFromMap(raw), nil
}

func (f *loginFlow) failPage(w http.ResponseWriter, status int, message string) {
	f.opts.Logf("LOGIN_FAILED %s", message)
	http.Error(w, message, status)
	f.finish(nil, errors.New(message))
}

func fetchUserInfo(ctx context.Context, provider, stage string, d discovery, tenantURL, accessToken string) (UserInfo, error) {
	if accessToken == "" {
		return UserInfo{}, errors.New("token response has no access_token")
	}
	base := strings.TrimRight(userInfoBase(provider, stage, d, tenantURL), "/")
	if base == "" {
		return UserInfo{}, errors.New("discovery response has no tenant_url")
	}
	raw, err := postJSON(ctx, base+"/integration/quick-work/user-info", map[string]any{}, map[string]string{
		"Authorization": "Bearer " + accessToken,
	})
	if err != nil {
		return UserInfo{}, err
	}
	return normalizeUserInfo(raw), nil
}

type httpResult struct {
	status int
	body   []byte
}

func doRequest(ctx context.Context, method, endpoint string, body io.Reader, headers map[string]string) (httpResult, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return httpResult{}, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return httpResult{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return httpResult{}, err
	}
	return httpResult{status: resp.StatusCode, body: data}, nil
}

func postJSON(ctx context.Context, endpoint string, payload any, headers map[string]string) (map[string]any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	h := map[string]string{"Content-Type": "application/json", "Accept": "application/json"}
	for k, v := range headers {
		h[k] = v
	}
	res, err := doRequest(ctx, http.MethodPost, endpoint, bytes.NewReader(body), h)
	if err != nil {
		return nil, err
	}
	return decodeJSONResponse(endpoint, res)
}

func postForm(ctx context.Context, endpoint string, form url.Values) (map[string]any, error) {
	res, err := doRequest(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Accept":       "application/json",
	})
	if err != nil {
		return nil, err
	}
	return decodeJSONResponse(endpoint, res)
}

func decodeJSONResponse(endpoint string, res httpResult) (map[string]any, error) {
	host := endpoint
	if u, err := url.Parse(endpoint); err == nil {
		host = u.Host
	}
	if res.status < 200 || res.status >= 300 {
		return nil, fmt.Errorf("HTTP %d from %s: %s", res.status, host, safeBody(res.body))
	}
	var raw map[string]any
	if err := json.Unmarshal(res.body, &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON from %s: %w", host, err)
	}
	return raw, nil
}

func tokensFromMap(raw map[string]any) tokens {
	return tokens{
		AccessToken:  firstValue(raw, "access_token", "accessToken"),
		RefreshToken: firstValue(raw, "refresh_token", "refreshToken"),
		IDToken:      firstValue(raw, "id_token", "idToken"),
		ExpiresIn:    firstInt(raw, "expires_in", "expiresIn"),
	}
}

func firstValue(raw map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := raw[key]; ok {
			if s := oneLine(v); s != "" {
				return s
			}
		}
	}
	return ""
}

func firstInt(raw map[string]any, keys ...string) int64 {
	for _, key := range keys {
		switch v := raw[key].(type) {
		case float64:
			return int64(v)
		case int64:
			return v
		case json.Number:
			n, _ := v.Int64()
			return n
		case string:
			var n int64
			if _, err := fmt.Sscan(v, &n); err == nil {
				return n
			}
		}
	}
	return 0
}

func firstQuery(q url.Values, keys ...string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(q.Get(key)); v != "" {
			return v
		}
	}
	return ""
}

func appendQuery(endpoint string, params url.Values) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint + "?" + params.Encode()
	}
	q := u.Query()
	for k, vs := range params {
		q.Del(k)
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func safeBody(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

func formatEpoch(v int64) string {
	if v <= 0 {
		return "unknown"
	}
	return time.Unix(v, 0).Local().Format("2006-01-02 15:04:05 MST")
}

func successHTML(who string) string {
	line := ""
	if who != "" {
		line = "<p>" + html.EscapeString(who) + "</p>"
	}
	return `<!doctype html><html><head><meta charset="utf-8"><title>Login complete</title>` +
		`<style>body{font-family:system-ui,sans-serif;max-width:36rem;margin:4rem auto;padding:0 1rem}` +
		`code{background:#eee;padding:.1rem .25rem}</style></head><body>` +
		`<h2>Login complete</h2>` + line +
		`<p>You can close this window and return to the terminal.</p></body></html>`
}

func emptyAs(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func openBrowser(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	case "darwin":
		cmd = exec.Command("open", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}
