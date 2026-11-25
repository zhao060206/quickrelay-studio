package proxy

import (
	"net/http"
	"strings"
)

// APIKeyAuth validates local proxy API keys from config.json. An empty key
// list disables authentication for local-only deployments.
type APIKeyAuth struct {
	keys map[string]bool
}

func NewAPIKeyAuth(keys []string) *APIKeyAuth {
	set := make(map[string]bool, len(keys))
	for _, key := range keys {
		if key = strings.TrimSpace(key); key != "" {
			set[key] = true
		}
	}
	return &APIKeyAuth{keys: set}
}

func tokenFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
			return strings.TrimSpace(h[7:])
		}
	}
	if h := r.Header.Get("X-Api-Key"); h != "" {
		return strings.TrimSpace(h)
	}
	return ""
}

// Allowed reports whether the request may use the proxy.
func (a *APIKeyAuth) Allowed(r *http.Request) bool {
	if len(a.keys) == 0 {
		return true
	}
	token := tokenFromRequest(r)
	if token == "" {
		return false
	}
	return a.keys[token]
}
