package admin

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"quickrelay-studio/internal/accounts"
	"quickrelay-studio/internal/config"
	"quickrelay-studio/internal/oauth"
	"quickrelay-studio/internal/store"
	"quickrelay-studio/internal/updater"
)

//go:embed static/index.html static/app.css static/app.js
var assets embed.FS

type loginEntry struct {
	owner [32]byte
	flow  *oauth.BrowserLogin
}

type Server struct {
	cfg            *config.Config
	store          *store.Store
	accounts       *accounts.Manager
	routes         *http.ServeMux
	startedAt      time.Time
	addrOverridden bool
	configured     bool
	passwordHash   [32]byte
	mu             sync.Mutex
	sessions       map[[32]byte]adminSession
	attempts       map[string]loginAttempts
	logins         map[string]loginEntry
	configMu        sync.Mutex
	saved           config.Config
	restartNeeded   bool
	agentsMu        sync.Mutex
	localAppDataDir func() string
	updater         *updater.Manager
}

type sessionContextKey struct{}

func NewServer(cfg *config.Config, st *store.Store, mgr *accounts.Manager, addrOverridden bool) (*Server, error) {
	dataDir := filepath.Dir(cfg.Database)
	if strings.TrimSpace(dataDir) == "" || dataDir == "." {
		dataDir = "data"
	}
	server := &Server{
		cfg:            cfg,
		store:          st,
		accounts:       mgr,
		startedAt:      time.Now(),
		addrOverridden: addrOverridden,
		sessions:       make(map[[32]byte]adminSession),
		attempts:       make(map[string]loginAttempts),
		logins:         make(map[string]loginEntry),
		saved:          cfg.FileValues(),
		updater:        updater.NewManager(cfg, dataDir),
	}
	if err := server.configureAuth(); err != nil {
		return nil, err
	}
	server.routes = http.NewServeMux()
	server.routes.HandleFunc("POST /api/auth/logout", server.handleLogout)
	server.routes.HandleFunc("GET /api/status", server.handleStatus)
	server.routes.HandleFunc("GET /api/accounts", server.handleAccounts)
	server.routes.HandleFunc("PATCH /api/accounts/{id}", server.handleAccountUpdate)
	server.routes.HandleFunc("DELETE /api/accounts/{id}", server.handleAccountDelete)
	server.routes.HandleFunc("POST /api/accounts/{id}/refresh", server.handleAccountRefresh)
	server.routes.HandleFunc("POST /api/accounts/refresh", server.handleAccountsRefresh)
	server.routes.HandleFunc("POST /api/accounts/{id}/usage/refresh", server.handleAccountUsageRefresh)
	server.routes.HandleFunc("POST /api/accounts/usage/refresh", server.handleAccountsUsageRefresh)
	server.routes.HandleFunc("POST /api/logins", server.handleLoginStart)
	server.routes.HandleFunc("GET /api/logins/{id}", server.handleLoginStatus)
	server.routes.HandleFunc("POST /api/logins/{id}/callback", server.handleLoginCallback)
	server.routes.HandleFunc("DELETE /api/logins/{id}", server.handleLoginCancel)
	server.routes.HandleFunc("GET /api/config", server.handleConfigGet)
	server.routes.HandleFunc("PUT /api/config", server.handleConfigSave)
	server.routes.HandleFunc("GET /api/analytics/summary", server.handleAnalyticsSummary)
	server.routes.HandleFunc("GET /api/logs/requests", server.handleRequestLogs)
	server.routes.HandleFunc("GET /api/logs/requests/export", server.handleRequestLogsExport)
	server.routes.HandleFunc("DELETE /api/logs/requests", server.handleRequestLogsClear)
	server.routes.HandleFunc("GET /api/conversations", server.handleConversationsList)
	server.routes.HandleFunc("DELETE /api/conversations", server.handleConversationsClear)
	server.routes.HandleFunc("DELETE /api/conversations/{key}", server.handleConversationDelete)
	server.routes.HandleFunc("GET /api/agents", server.handleAgentsList)
	server.routes.HandleFunc("GET /api/agents/claude-desktop", server.handleClaudeDesktopGet)
	server.routes.HandleFunc("POST /api/agents/claude-desktop/preview", server.handleClaudeDesktopPreview)
	server.routes.HandleFunc("POST /api/agents/claude-desktop/apply", server.handleClaudeDesktopApply)
	server.routes.HandleFunc("POST /api/agents/claude-desktop/restore", server.handleClaudeDesktopRestore)
	server.routes.HandleFunc("POST /api/agents/claude-desktop/disable", server.handleClaudeDesktopDisable)
	server.routes.HandleFunc("GET /api/update/check", server.handleUpdateCheck)
	server.routes.HandleFunc("POST /api/update/download", server.handleUpdateDownload)
	server.routes.HandleFunc("GET /api/update/status", server.handleUpdateStatus)
	server.routes.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "Management endpoint not found")
	})
	return server, nil
}

func (s *Server) Configured() bool {
	return s.configured
}

func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range s.logins {
		entry.flow.Cancel()
	}
	clear(s.logins)
	clear(s.sessions)
}

func (s *Server) Handler(proxy http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/v1/") {
			proxy.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			s.serveAPI(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		file := ""
		switch r.URL.Path {
		case "/", "/index.html":
			file = "static/index.html"
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		case "/assets/app.css":
			file = "static/app.css"
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		case "/assets/app.js":
			file = "static/app.js"
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		default:
			http.NotFound(w, r)
			return
		}
		body, err := assets.ReadFile(file)
		if err != nil {
			http.Error(w, "asset unavailable", http.StatusInternalServerError)
			return
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	})
}

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	mutation := r.Method != http.MethodGet && r.Method != http.MethodHead
	if mutation && !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "invalid_origin", "Cross-origin management requests are not allowed")
		return
	}
	switch r.URL.Path {
	case "/api/auth/session":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return
		}
		s.handleSession(w, r)
		return
	case "/api/auth/login":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		s.handleLogin(w, r)
		return
	}
	owner, session, ok := s.sessionFromRequest(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in to the management console")
		return
	}
	if mutation && !validCSRF(session.csrf, r.Header.Get("X-CSRF-Token")) {
		writeError(w, http.StatusForbidden, "invalid_csrf", "Invalid CSRF token; sign in again")
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionContextKey{}, owner))
	s.routes.ServeHTTP(w, r)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, value any, limit int64) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "invalid_content_type", "Content-Type must be application/json")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Request body is too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON request")
		}
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_json", "Request must contain one JSON object")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", allow+" required")
}
