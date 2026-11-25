package oauth

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"quickrelay-studio/internal/store"
)

// callbackKind classifies a pasted URL by its OAuth parameters.
type callbackKind int

const (
	callbackUnknown callbackKind = iota
	callbackDiscovery
	callbackDiscoveryError
	callbackAuthorize
	callbackCode
	callbackCodeError
	callbackStart
)

func classifyCallback(q url.Values) callbackKind {
	switch {
	case q.Get("code") != "":
		return callbackCode
	case q.Get("error") != "":
		switch {
		case q.Get("nonce") != "":
			return callbackDiscoveryError
		case q.Get("state") != "":
			return callbackCodeError
		default:
			return callbackUnknown
		}
	case q.Get("nonce") != "":
		return callbackDiscovery
	case q.Get("client_id") != "" && (q.Get("response_type") == "code" || q.Get("code_challenge") != ""):
		return callbackAuthorize
	case q.Get("qwSurface") != "":
		return callbackStart
	default:
		return callbackUnknown
	}
}

// LoginManual runs the same OAuth flow without listening for browser
// callbacks. The user opens the discovery URL on any machine, then pastes
// callback URLs from the browser address bar. This supports logging in on a
// remote server whose localhost is not reachable by the browser.
func LoginManual(ctx context.Context, opts LoginOptions, st *store.Store, in io.Reader, out io.Writer) (*store.Account, error) {
	opts = normalizeLoginOptions(opts)
	f, err := newLoginFlow(opts, st, opts.Port)
	if err != nil {
		return nil, err
	}
	return runManualLogin(ctx, opts, f, in, out)
}

func runManualLogin(ctx context.Context, opts LoginOptions, f *loginFlow, in io.Reader, out io.Writer) (*store.Account, error) {
	if out == nil {
		out = io.Discard
	}
	provider := normalizedProvider(opts.Provider)
	stage := normalizedStage(opts.Stage)

	loginURL := discoveryURL(provider, stage, f.region, f.nonce, opts.Port, opts.AppVersion)
	fmt.Fprintf(out, "Open this URL to sign in:\n%s\n", loginURL)
	fmt.Fprintln(out, "After login, paste any URL from the browser address bar; the stage is detected automatically.")
	fmt.Fprintln(out, "(The localhost page may fail to load; the URL in the address bar is what matters.)")
	if opts.OpenBrowser {
		if err := openBrowser(loginURL); err != nil {
			opts.Logf("browser launch failed: %v", err)
		}
	}

	waitCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	lines := scanLines(waitCtx, in)
	for {
		fmt.Fprint(out, "callback URL: ")
		var line string
		var ok bool
		select {
		case line, ok = <-lines:
			if !ok {
				if waitCtx.Err() != nil {
					return nil, manualWaitError(waitCtx.Err())
				}
				return nil, errors.New("input closed before a callback URL was provided")
			}
		case <-waitCtx.Done():
			return nil, manualWaitError(waitCtx.Err())
		}

		q, err := parseCallbackURL(line)
		if err != nil {
			fmt.Fprintf(out, "invalid URL: %v\n", err)
			continue
		}
		switch classifyCallback(q) {
		case callbackDiscovery, callbackDiscoveryError:
			repeated := f.discoveryDone
			authURL, err := f.processDiscovery(waitCtx, q)
			if err != nil {
				fmt.Fprintf(out, "discovery failed: %v\n", err)
				continue
			}
			if repeated {
				fmt.Fprintln(out, "This discovery callback was already processed; discovery is already complete.")
				fmt.Fprintln(out, "Open the authorize URL below and finish signing in:")
			} else {
				fmt.Fprintln(out, "Discovery complete. Open this URL to continue signing in:")
			}
			fmt.Fprintln(out, authURL)
			fmt.Fprintf(out, "Then paste the callback URL containing code=...&state=... (redirect_uri=%s).\n", f.redirectURI)
		case callbackCode, callbackCodeError:
			acct, err := f.completeLogin(waitCtx, q)
			if err != nil {
				fmt.Fprintf(out, "login failed: %v\n", err)
				continue
			}
			fmt.Fprintf(out, "Login complete: %s (account %d)\n", accountLabel(acct), acct.ID)
			return acct, nil
		case callbackAuthorize:
			fmt.Fprintln(out, "This is an authorize URL, not a callback. Open it in the browser and finish signing in.")
			if f.discoveryDone {
				fmt.Fprintln(out, f.authorizeURL)
			} else {
				fmt.Fprintln(out, "Paste the discovery callback URL first (looks like .../callback?nonce=...).")
			}
			fmt.Fprintf(out, "Then paste the URL containing code=...&state=... (redirect_uri=%s).\n", f.redirectURI)
		case callbackStart:
			fmt.Fprintln(out, "This is the discovery URL. Open it and sign in, then paste the resulting .../callback?nonce=... URL.")
		default:
			fmt.Fprintln(out, "URL not recognized. Expected a discovery callback (.../callback?nonce=...) or a code callback (...?code=...&state=...).")
			if f.discoveryDone {
				fmt.Fprintln(out, "If you already opened the authorize URL, paste the URL containing code=...&state=... from the browser address bar.")
			}
		}
	}
}

func manualWaitError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("timed out waiting for callback URL")
	}
	return err
}

func scanLines(ctx context.Context, r io.Reader) <-chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			select {
			case ch <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// parseCallbackURL accepts a full callback URL, a host/path URL, a bare query
// string, or query parameters without '?'. Query and fragment parameters are
// merged so code callbacks from either style are recognized.
func parseCallbackURL(raw string) (url.Values, error) {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, "\"'<>`")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, `\&`, "&")
	if s == "" {
		return nil, errors.New("empty input")
	}
	if strings.HasPrefix(s, "?") {
		return url.ParseQuery(strings.TrimPrefix(s, "?"))
	}
	if !strings.Contains(s, "://") {
		switch {
		case strings.HasPrefix(s, "//"):
			s = "http:" + s
		case strings.HasPrefix(s, "/"):
			s = "http://localhost" + s
		case strings.Contains(s, "/"):
			s = "http://" + s
		case strings.Contains(s, "="):
			s = "http://localhost/?" + s
		default:
			return nil, errors.New("expected a URL or query string")
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	values := u.Query()
	if u.Fragment != "" {
		if fragment, err := url.ParseQuery(u.Fragment); err == nil {
			for key, vals := range fragment {
				if values.Get(key) == "" {
					values[key] = vals
				}
			}
		}
	}
	if len(values) == 0 {
		return nil, errors.New("URL has no query parameters")
	}
	return values, nil
}
