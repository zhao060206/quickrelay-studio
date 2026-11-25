package admin

import (
	"context"
	"net/http"
	"strings"
	"time"

	"quickrelay-studio/internal/oauth"
)

func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Provider string `json:"provider"`
		Stage    string `json:"stage"`
		Region   string `json:"region"`
	}
	if !decodeJSON(w, r, &input, 4<<10) {
		return
	}
	if input.Provider == "" {
		input.Provider = s.cfg.OAuth.Provider
	}
	if input.Stage == "" {
		input.Stage = s.cfg.OAuth.BuildStage
	}
	if input.Region == "" {
		input.Region = s.cfg.OAuth.Region
	}
	input.Provider = strings.ToLower(strings.TrimSpace(input.Provider))
	input.Stage = strings.ToLower(strings.TrimSpace(input.Stage))
	input.Region = strings.ToLower(strings.TrimSpace(input.Region))
	switch input.Provider {
	case "social", "enterprise", "gateway", "social_gateway":
	default:
		writeError(w, http.StatusBadRequest, "invalid_provider", "Unknown OAuth provider")
		return
	}
	switch input.Stage {
	case "prod", "beta", "integ", "dev":
	default:
		writeError(w, http.StatusBadRequest, "invalid_stage", "Unknown OAuth build stage")
		return
	}
	if len(input.Region) > 64 || strings.Trim(input.Region, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
		writeError(w, http.StatusBadRequest, "invalid_region", "Region may only contain lowercase letters, digits and hyphens")
		return
	}
	flow, err := oauth.NewBrowserLogin(oauth.LoginOptions{
		Provider: input.Provider, Stage: input.Stage, Region: input.Region,
		Port: s.cfg.OAuth.CallbackPort, Timeout: s.cfg.OAuth.Timeout.Duration(), AppVersion: s.cfg.AppVersion,
	}, s.store)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "login_failed", "Could not start browser sign-in")
		return
	}
	id, err := randomToken()
	if err != nil {
		flow.Cancel()
		writeError(w, http.StatusInternalServerError, "login_failed", "Could not start browser sign-in")
		return
	}
	owner := r.Context().Value(sessionContextKey{}).([32]byte)
	s.mu.Lock()
	s.cleanupLocked(time.Now())
	if _, ok := s.sessions[owner]; !ok {
		s.mu.Unlock()
		flow.Cancel()
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in to the management console")
		return
	}
	owned := 0
	for existingID, entry := range s.logins {
		status := entry.flow.Status()
		if status.Stage != "discovery" && status.Stage != "authorize" {
			delete(s.logins, existingID)
			continue
		}
		if entry.owner == owner {
			owned++
		}
	}
	if len(s.logins) >= 32 || owned >= 4 {
		s.mu.Unlock()
		flow.Cancel()
		writeError(w, http.StatusTooManyRequests, "login_limit", "Cancel an existing browser sign-in before starting another")
		return
	}
	s.logins[id] = loginEntry{owner: owner, flow: flow}
	s.mu.Unlock()
	writeLoginStatus(w, http.StatusCreated, id, flow.Status())
}

func (s *Server) browserLogin(w http.ResponseWriter, r *http.Request) (*oauth.BrowserLogin, bool) {
	id := r.PathValue("id")
	owner := r.Context().Value(sessionContextKey{}).([32]byte)
	s.mu.Lock()
	entry, ok := s.logins[id]
	s.mu.Unlock()
	if !ok || entry.owner != owner {
		writeError(w, http.StatusNotFound, "login_not_found", "Browser sign-in not found; start a new sign-in")
		return nil, false
	}
	return entry.flow, true
}

func (s *Server) handleLoginStatus(w http.ResponseWriter, r *http.Request) {
	flow, ok := s.browserLogin(w, r)
	if !ok {
		return
	}
	writeLoginStatus(w, http.StatusOK, r.PathValue("id"), flow.Status())
}

func (s *Server) handleLoginCallback(w http.ResponseWriter, r *http.Request) {
	flow, ok := s.browserLogin(w, r)
	if !ok {
		return
	}
	var input struct {
		URL string `json:"url"`
	}
	if !decodeJSON(w, r, &input, 128<<10) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout.Duration())
	defer cancel()
	status, err := flow.Submit(ctx, input.URL)
	input.URL = ""
	if err != nil {
		writeError(w, http.StatusBadRequest, "callback_failed", err.Error())
		return
	}
	writeLoginStatus(w, http.StatusOK, r.PathValue("id"), status)
}

func (s *Server) handleLoginCancel(w http.ResponseWriter, r *http.Request) {
	flow, ok := s.browserLogin(w, r)
	if !ok {
		return
	}
	status := flow.Cancel()
	s.mu.Lock()
	delete(s.logins, r.PathValue("id"))
	s.mu.Unlock()
	writeLoginStatus(w, http.StatusOK, r.PathValue("id"), status)
}

func writeLoginStatus(w http.ResponseWriter, status int, id string, state oauth.BrowserLoginStatus) {
	writeJSON(w, status, struct {
		ID string `json:"id"`
		oauth.BrowserLoginStatus
	}{ID: id, BrowserLoginStatus: state})
}
