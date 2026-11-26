package proxy

import (
	"fmt"
	"strings"

	"quickrelay-studio/internal/config"
	"quickrelay-studio/internal/store"
)

// Session is the runtime view of one upstream account used by the WebSocket
// transport.
type Session struct {
	JWT              string
	WSHost           string
	WSPath           string
	TenantURL        string
	Provider         string
	ActorARN         string
	Email            string
	DisplayName      string
	Surface          string
	OutpostID        string
	WorkingSessionID string
}

func (s *Session) jwt() string {
	if s == nil {
		return ""
	}
	return s.JWT
}

// SessionFromAccount merges an account with server-side handshake defaults.
func SessionFromAccount(a *store.Account, cfg *config.Config) *Session {
	surface := cfg.Surface
	if surface == "" {
		surface = "QUICK_WORK_DESKTOP_WINDOWS"
	}
	return &Session{
		JWT:              a.JWT(),
		WSHost:           a.WSHost,
		WSPath:           a.WSPath,
		TenantURL:        a.TenantURL,
		Provider:         a.Provider,
		ActorARN:         a.ActorARN,
		Email:            a.Email,
		DisplayName:      a.DisplayName,
		Surface:          surface,
		OutpostID:        cfg.OutpostID,
		WorkingSessionID: cfg.WorkingSessionID,
	}
}

func (s *Session) validate() error {
	if strings.TrimSpace(s.JWT) == "" {
		return fmt.Errorf("account has no jwt/id_token")
	}
	if strings.TrimSpace(s.WSHost) == "" {
		return fmt.Errorf("account has no ws_host")
	}
	return nil
}
