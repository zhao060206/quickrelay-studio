// Package oauth implements the native Amazon Quick desktop login flow and
// token refresh, without the official desktop app.
package oauth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const (
	gatewayPath     = "/ws/service/SpaceNeedleConversationService/operation/ChatBidirectionalStream"
	enterpriseScope = "openid profile email"
)

var socialDiscovery = map[string]string{
	"prod":  "https://quick.aws.com/sn",
	"beta":  "https://devtest.quicksuite.aws.dev/sn",
	"integ": "https://devtest.quicksuite.aws.dev/sn",
	"dev":   "https://devtest.quicksuite.aws.dev/sn",
}

var socialDataPlane = map[string]string{
	"prod":  "https://cell001.dp.appintegrations.us-east-1.prod.plato.ai.aws.dev",
	"beta":  "https://cell001.dp.appintegrations.us-west-2.beta.plato.ai.aws.dev",
	"integ": "https://cell001.dp.appintegrations.us-west-2.beta.plato.ai.aws.dev",
	"dev":   "https://cell001.dp.appintegrations.us-west-2.beta.plato.ai.aws.dev",
}

var socialScopes = map[string][]string{
	"prod":  {"quicksight:read_write"},
	"beta":  {"quicksight_test:read_write"},
	"integ": {"quicksight_test:read_write"},
	"dev":   {"quicksight_test:read_write"},
}

var enterpriseDiscovery = map[string]string{
	"prod":  "https://quicksight.aws.amazon.com/sn/start/app-integrations/quickwork-enterprise-login",
	"beta":  "https://spaceneedle-alpha.amazon.com/sn/start/app-integrations/quickwork-enterprise-login",
	"integ": "https://spaceneedle-beta.amazon.com/sn/start/app-integrations/quickwork-enterprise-login",
	"dev":   "https://spaceneedle-alpha.amazon.com/sn/start/app-integrations/quickwork-enterprise-login",
}

type discovery struct {
	IssuerURL     string
	TenantURL     string
	IDCRegion     string
	Region        string
	AuthEndpoint  string
	TokenEndpoint string
	ClientID      string
	ClientSecret  string
}

type tokens struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresIn    int64
}

func normalizedProvider(provider string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	switch p {
	case "social", "enterprise", "gateway", "social_gateway":
		return p
	}
	return "social"
}

func normalizedStage(stage string) string {
	s := strings.ToLower(strings.TrimSpace(stage))
	switch s {
	case "prod", "beta", "integ", "dev":
		return s
	}
	return "prod"
}

func isEnterpriseDiscovery(provider string) bool {
	return provider == "enterprise" || provider == "gateway"
}

func isGatewayProvider(provider string) bool {
	return provider == "gateway" || provider == "social_gateway"
}

func dataPlane(stage string) string {
	if v := socialDataPlane[stage]; v != "" {
		return v
	}
	return socialDataPlane["prod"]
}

func scopesFor(stage string) []string {
	if v := socialScopes[stage]; v != nil {
		return v
	}
	return socialScopes["prod"]
}

func discoveryURL(provider, stage, region, nonce string, port int, appVersion string) string {
	surface := url.QueryEscape("desktop:" + nonce + ":" + itoa(port) + ":" + appVersion)
	if isEnterpriseDiscovery(provider) {
		base := enterpriseDiscovery[stage]
		if base == "" {
			base = enterpriseDiscovery["prod"]
		}
		if region != "" && region != "dynamic" && region != "us-east-1" {
			if u, err := url.Parse(base); err == nil {
				u.Host = region + "." + u.Host
				base = u.String()
			}
		}
		return base + "?qwSurface=" + surface
	}
	base := socialDiscovery[stage]
	if base == "" {
		base = socialDiscovery["prod"]
	}
	return base + "?qwSurface=" + surface
}

func redirectURI(provider string, port int) string {
	if provider == "enterprise" || provider == "gateway" {
		return "http://localhost:" + itoa(port)
	}
	return "http://127.0.0.1:" + itoa(port)
}

func userInfoBase(provider, stage string, d discovery, tenantURL string) string {
	if provider == "enterprise" || provider == "gateway" {
		if d.TenantURL != "" {
			return d.TenantURL
		}
		return tenantURL
	}
	tenant := strings.TrimRight(d.TenantURL, "/")
	if strings.Contains(tenant, "dp.appintegrations") {
		return tenant
	}
	return dataPlane(stage)
}

// UserInfo is the normalized /integration/quick-work/user-info payload.
type UserInfo struct {
	Email                     string `json:"email"`
	UserName                  string `json:"user_name"`
	FirstName                 string `json:"first_name"`
	LastName                  string `json:"last_name"`
	DisplayName               string `json:"display_name"`
	UserARN                   string `json:"user_arn"`
	QuickSightExternalUserARN string `json:"quicksight_external_user_arn"`
	Role                      string `json:"role"`
	IdentityType              string `json:"identity_type"`
	AWSAccountID              string `json:"aws_account_id"`
	AccountName               string `json:"account_name"`
	ExternalID                string `json:"external_id"`
	GroupARNsCompressed       string `json:"group_arns_compressed"`
	AccountOwnership          string `json:"account_ownership"`
}

func (u UserInfo) Display() string {
	for _, v := range []string{u.Email, u.UserName, u.DisplayName} {
		if v != "" {
			return v
		}
	}
	return ""
}

func normalizeUserInfo(raw map[string]any) UserInfo {
	if inner, ok := raw["userInfo"].(map[string]any); ok {
		raw = inner
	}
	pick := func(keys ...string) string {
		for _, key := range keys {
			if v, ok := raw[key]; ok {
				if s := oneLine(v); s != "" {
					return s
				}
			}
		}
		return ""
	}
	return UserInfo{
		Email:                     pick("email", "emailAddress"),
		UserName:                  pick("userName", "user_name", "username"),
		FirstName:                 pick("firstName", "first_name"),
		LastName:                  pick("lastName", "last_name"),
		DisplayName:               pick("displayName", "display_name"),
		UserARN:                   pick("userArn", "user_arn"),
		QuickSightExternalUserARN: pick("quicksightExternalUserArn", "quicksight_external_user_arn"),
		Role:                      pick("role"),
		IdentityType:              pick("identityType", "identity_type"),
		AWSAccountID:              pick("awsAccountId", "aws_account_id", "accountId"),
		AccountName:               pick("accountName", "account_name"),
		ExternalID:                pick("externalId", "external_id"),
		GroupARNsCompressed:       pick("groupArnsCompressed", "group_arns_compressed"),
		AccountOwnership:          pick("accountOwnership", "account_ownership"),
	}
}

func oneLine(v any) string {
	if v == nil {
		return ""
	}
	var s string
	switch x := v.(type) {
	case string:
		s = x
	default:
		s = fmt.Sprint(x)
	}
	return strings.Join(strings.Fields(s), " ")
}

func decodeJWT(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	payload := parts[1]
	payload += strings.Repeat("=", (4-len(payload)%4)%4)
	body, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		body, err = base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(body, &claims); err != nil {
		return nil
	}
	return claims
}

func claimString(claims map[string]any, key string) string {
	if claims == nil {
		return ""
	}
	if v, ok := claims[key].(string); ok {
		return v
	}
	return ""
}

func claimInt64(claims map[string]any, key string) int64 {
	if claims == nil {
		return 0
	}
	switch v := claims[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

func itoa(v int) string { return strconv.Itoa(v) }
