package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const sessionCookie = "quickproxy_admin"

type adminSession struct {
	csrf    string
	expires time.Time
}

type loginAttempts struct {
	count int
	reset time.Time
}

func (s *Server) configureAuth() error {
	password := os.Getenv("QUICKPROXY_ADMIN_PASSWORD")
	if password == "" {
		return nil
	}
	if len(password) < 12 || len(password) > 1024 {
		return errors.New("admin password must contain between 12 and 1024 bytes")
	}
	s.passwordHash = sha256.Sum256([]byte(password))
	s.configured = true
	return nil
}

func randomToken() (string, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(token[:]), nil
}

func sameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") &&
		parsed.User == nil && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == "" &&
		strings.EqualFold(parsed.Host, r.Host) && (r.TLS == nil || parsed.Scheme == "https")
}

func validCSRF(expected, actual string) bool {
	return actual != "" && subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) == 1
}

func (s *Server) cleanupLocked(now time.Time) {
	for token, session := range s.sessions {
		if !now.Before(session.expires) {
			delete(s.sessions, token)
		}
	}
	for id, entry := range s.logins {
		_, ownerExists := s.sessions[entry.owner]
		if !ownerExists || entry.flow.Status().ExpiresAt+300 <= now.Unix() {
			entry.flow.Cancel()
			delete(s.logins, id)
		}
	}
	for address, attempts := range s.attempts {
		if !now.Before(attempts.reset) {
			delete(s.attempts, address)
		}
	}
}

func (s *Server) sessionFromRequest(r *http.Request) ([32]byte, adminSession, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || len(cookie.Value) != 43 {
		return [32]byte{}, adminSession{}, false
	}
	token := sha256.Sum256([]byte(cookie.Value))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(time.Now())
	session, ok := s.sessions[token]
	return token, session, ok
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	_, session, ok := s.sessionFromRequest(r)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false, "configured": s.configured})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "configured": s.configured, "csrf_token": session.csrf})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.configured {
		writeError(w, http.StatusServiceUnavailable, "admin_not_configured", "Configure the server admin password environment variable and restart before signing in")
		return
	}
	address, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		address = r.RemoteAddr
	}
	now := time.Now()
	s.mu.Lock()
	s.cleanupLocked(now)
	attempts := s.attempts[address]
	if attempts.count >= 5 || (attempts.count == 0 && len(s.attempts) >= 1024) {
		s.mu.Unlock()
		w.Header().Set("Retry-After", "300")
		writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many sign-in attempts; try again in five minutes")
		return
	}
	if attempts.count == 0 {
		attempts.reset = now.Add(5 * time.Minute)
	}
	attempts.count++
	s.attempts[address] = attempts
	s.mu.Unlock()
	var input struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &input, 4<<10) {
		return
	}
	passwordHash := sha256.Sum256([]byte(input.Password))
	input.Password = ""
	if subtle.ConstantTimeCompare(s.passwordHash[:], passwordHash[:]) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "Invalid management password")
		return
	}
	token, err := randomToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_failed", "Could not create a management session")
		return
	}
	csrf, err := randomToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_failed", "Could not create a management session")
		return
	}
	expires := now.Add(s.cfg.WebUI.SessionTTL.Duration())
	s.mu.Lock()
	var oldToken [32]byte
	if old, err := r.Cookie(sessionCookie); err == nil {
		oldToken = sha256.Sum256([]byte(old.Value))
	}
	_, replacing := s.sessions[oldToken]
	if len(s.sessions) >= 128 && !replacing {
		s.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "session_limit", "Too many active management sessions; try again later")
		return
	}
	if replacing {
		s.removeSessionLocked(oldToken)
	}
	s.sessions[sha256.Sum256([]byte(token))] = adminSession{csrf: csrf, expires: expires}
	delete(s.attempts, address)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/api",
		Expires:  expires,
		MaxAge:   int(s.cfg.WebUI.SessionTTL.Duration().Seconds()),
		HttpOnly: true,
		Secure:   secureRequest(r),
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "configured": true, "csrf_token": csrf})
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.HasPrefix(r.Header.Get("Origin"), "https://")
}

func (s *Server) removeSessionLocked(owner [32]byte) {
	delete(s.sessions, owner)
	for id, entry := range s.logins {
		if entry.owner == owner {
			entry.flow.Cancel()
			delete(s.logins, id)
		}
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	owner := r.Context().Value(sessionContextKey{}).([32]byte)
	s.mu.Lock()
	s.removeSessionLocked(owner)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/api", MaxAge: -1,
		Expires: time.Unix(1, 0), HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
