package oauth

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"quickrelay-studio/internal/store"
)

// applySession fills an account's runtime session fields from newly obtained
// tokens. The account's provider, tenant URL and OAuth client fields must be
// populated first.
func applySession(a *store.Account, t tokens, info UserInfo) error {
	access := strings.TrimSpace(t.AccessToken)
	idToken := strings.TrimSpace(t.IDToken)
	if idToken == "" {
		// Refresh responses are not guaranteed to repeat the ID token. Keep
		// the previous one so the WebSocket handshake does not silently fall
		// back to the access token.
		idToken = strings.TrimSpace(a.IDToken)
	}
	jwt := idToken
	if jwt == "" {
		jwt = access
	}
	if jwt == "" {
		return fmt.Errorf("token response has no id_token/access_token")
	}

	tenant := strings.TrimSpace(a.TenantURL)
	if tenant == "" {
		tenant = dataPlane(a.BuildStage)
	}
	tenant = strings.TrimRight(tenant, "/")
	if !strings.Contains(tenant, "://") {
		tenant = "https://" + tenant
	}
	parsed, err := url.Parse(tenant)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("cannot derive ws_host from tenant_url %q", tenant)
	}

	claims := decodeJWT(idToken)
	actorARN := strings.TrimSpace(info.UserARN)
	if actorARN == "" {
		if sub := claimString(claims, "sub"); strings.HasPrefix(sub, "arn:") {
			actorARN = sub
		}
	}
	region := strings.TrimSpace(a.Region)
	if region == "" {
		region = claimString(claims, "region")
	}
	if region == "" {
		region = "us-east-1"
	}

	expiresAt := int64(0)
	if t.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second).Unix()
	}
	if exp := claimInt64(claims, "exp"); exp > 0 {
		// The WebSocket uses the ID token, so refresh before whichever token
		// expires first.
		if expiresAt == 0 || exp < expiresAt {
			expiresAt = exp
		}
	}
	refreshToken := strings.TrimSpace(t.RefreshToken)
	if refreshToken == "" {
		refreshToken = strings.TrimSpace(a.RefreshToken)
	}
	if expiresAt == 0 && refreshToken != "" {
		// No expiry metadata: refresh conservatively rather than keeping a
		// dead session forever.
		expiresAt = time.Now().Add(50 * time.Minute).Unix()
	}

	accountID := info.AWSAccountID
	if accountID == "" {
		accountID = claimString(claims, "aws_account_id")
	}

	a.Email = prefer(info.Email, a.Email)
	a.DisplayName = prefer(info.DisplayName, a.DisplayName)
	a.TenantURL = tenant
	a.WSHost = parsed.Host
	a.AccessToken = access
	a.RefreshToken = refreshToken
	a.IDToken = idToken
	a.ActorARN = prefer(actorARN, a.ActorARN)
	a.AWSAccountID = prefer(accountID, a.AWSAccountID)
	a.AccountName = prefer(info.AccountName, a.AccountName)
	a.Region = region
	a.ExpiresAt = expiresAt
	a.ObtainedAt = time.Now().Unix()
	a.LastError = ""
	if isGatewayProvider(a.Provider) {
		a.WSPath = gatewayPath
	} else {
		a.WSPath = ""
	}
	body, err := json.Marshal(info)
	if err != nil {
		return err
	}
	a.UserInfo = string(body)
	return nil
}

func prefer(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}
