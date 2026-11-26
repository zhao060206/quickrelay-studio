package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"quickrelay-studio/internal/store"
)

const (
	maxAccountsImportBytes = 8 << 20 // 8 MB
	maxAccountsImportCount = 500
	defaultGatewayWSPath   = "/ws/service/SpaceNeedleConversationService/operation/ChatBidirectionalStream"
)

type importAccountItemResult struct {
	Index        int    `json:"index"`
	Status       string `json:"status"`
	ID           int64  `json:"id,omitempty"`
	Email        string `json:"email,omitempty"`
	Error        string `json:"error,omitempty"`
	Refreshed    bool   `json:"refreshed,omitempty"`
	RefreshError string `json:"refresh_error,omitempty"`
}

func (s *Server) handleAccountsImport(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "invalid_content_type", "Content-Type must be application/json")
		return
	}

	rawBody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAccountsImportBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Import payload exceeds 8 MB limit")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_json", "Could not read JSON request body")
		}
		return
	}

	dec := json.NewDecoder(bytes.NewReader(rawBody))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON payload")
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_json", "Request must contain a single JSON value")
		return
	}

	rawItems, autoRefresh, errMsg := unwrapImportPayload(root)
	if errMsg != "" {
		writeError(w, http.StatusBadRequest, "invalid_import", errMsg)
		return
	}
	if len(rawItems) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_import", "Import payload contains no accounts")
		return
	}
	if len(rawItems) > maxAccountsImportCount {
		writeError(w, http.StatusBadRequest, "invalid_import", fmt.Sprintf("At most %d accounts may be imported at once", maxAccountsImportCount))
		return
	}

	ctx := r.Context()
	existingList, err := s.store.List(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "Could not read existing accounts")
		return
	}
	type accountUniqueKey struct {
		email     string
		tenantURL string
	}
	existing := make(map[accountUniqueKey]bool, len(existingList))
	for _, a := range existingList {
		existing[accountUniqueKey{
			email:     strings.ToLower(strings.TrimSpace(a.Email)),
			tenantURL: strings.TrimSpace(a.TenantURL),
		}] = true
	}

	var (
		imported int
		updated  int
		failed   int
	)
	results := make([]importAccountItemResult, 0, len(rawItems))

	for idx, rawItem := range rawItems {
		itemMap, ok := rawItem.(map[string]any)
		if !ok || len(itemMap) == 0 {
			failed++
			results = append(results, importAccountItemResult{
				Index:  idx,
				Status: "failed",
				Error:  "Account entry must be a non-empty JSON object",
			})
			continue
		}

		acct, convErr := normalizeImportedAccount(itemMap)
		if convErr != nil {
			failed++
			results = append(results, importAccountItemResult{
				Index:  idx,
				Status: "failed",
				Email:  strings.TrimSpace(pickImportString(itemMap, "email", "emailAddress", "user_email", "username", "userName")),
				Error:  convErr.Error(),
			})
			continue
		}

		uKey := accountUniqueKey{
			email:     strings.ToLower(strings.TrimSpace(acct.Email)),
			tenantURL: strings.TrimSpace(acct.TenantURL),
		}
		existedBefore := existing[uKey]
		wantDisabled := acct.Disabled

		if err := s.store.Save(ctx, acct); err != nil {
			failed++
			results = append(results, importAccountItemResult{
				Index:  idx,
				Status: "failed",
				Email:  acct.Email,
				Error:  "Failed to save account: " + err.Error(),
			})
			continue
		}
		existing[uKey] = true
		if wantDisabled {
			if err := s.store.SetDisabled(ctx, acct.ID, true); err == nil {
				acct.Disabled = true
			}
		}

		itemRes := importAccountItemResult{
			Index: idx,
			ID:    acct.ID,
			Email: acct.Email,
		}
		if existedBefore {
			updated++
			itemRes.Status = "updated"
		} else {
			imported++
			itemRes.Status = "imported"
		}

		if autoRefresh && s.accounts != nil && strings.TrimSpace(acct.RefreshToken) != "" && !acct.Disabled {
			refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			refreshedAcct, rErr := s.accounts.RefreshOne(refreshCtx, acct.ID)
			cancel()
			if rErr != nil {
				itemRes.RefreshError = rErr.Error()
			} else if refreshedAcct != nil {
				itemRes.Refreshed = true
				if refreshedAcct.Email != "" {
					itemRes.Email = refreshedAcct.Email
				}
			}
		}
		results = append(results, itemRes)
	}

	list, err := s.store.List(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "Accounts imported, but could not reload account list")
		return
	}
	views := make([]accountView, 0, len(list))
	for _, a := range list {
		views = append(views, publicAccount(a))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total":    len(rawItems),
		"imported": imported,
		"updated":  updated,
		"failed":   failed,
		"results":  results,
		"accounts": views,
	})
}

func unwrapImportPayload(root any) ([]any, bool, string) {
	switch v := root.(type) {
	case []any:
		return v, false, ""
	case map[string]any:
		autoRefresh := pickImportBool(v, "auto_refresh", "autoRefresh")
		for _, wrapperKey := range []string{"accounts", "items", "data"} {
			if raw, exists := v[wrapperKey]; exists {
				arr, ok := raw.([]any)
				if !ok {
					return nil, autoRefresh, fmt.Sprintf("Field %q must be a JSON array", wrapperKey)
				}
				return arr, autoRefresh, ""
			}
		}
		if len(v) == 0 {
			return nil, false, "Import JSON object is empty"
		}
		return []any{v}, autoRefresh, ""
	default:
		return nil, false, "Import payload must be a JSON object, array, or {accounts:[...]} wrapper"
	}
}

func normalizeImportedAccount(raw map[string]any) (*store.Account, error) {
	accessToken := pickImportString(raw, "access_token", "accessToken")
	refreshToken := pickImportString(raw, "refresh_token", "refreshToken")
	idToken := pickImportString(raw, "id_token", "idToken", "jwt")
	if accessToken == "" && idToken == "" && refreshToken == "" {
		return nil, errors.New("missing access_token, id_token, or refresh_token")
	}

	provider := normalizeImportProvider(pickImportString(raw, "provider", "auth_provider", "authProvider", "authMethod"))
	stage := normalizeImportStage(pickImportString(raw, "build_stage", "buildStage", "stage"))

	region := pickImportString(raw, "region", "aws_region", "awsRegion")
	idcRegion := pickImportString(raw, "idc_region", "idcRegion", "sso_region", "ssoRegion")
	if region == "" {
		region = idcRegion
	}

	claims := decodeImportJWT(idToken)
	if claims == nil {
		claims = decodeImportJWT(accessToken)
	}
	if region == "" {
		region = importClaimString(claims, "region")
	}
	if region == "" {
		region = "us-east-1"
	}
	if idcRegion == "" {
		idcRegion = region
	}

	userInfoObj, userInfoStr := extractImportUserInfo(raw)
	email := firstNonEmpty(
		pickImportString(raw, "email", "emailAddress", "user_email", "username", "userName"),
		pickImportString(userInfoObj, "email", "emailAddress", "userName", "user_name", "username"),
		importClaimString(claims, "email"),
	)
	displayName := firstNonEmpty(
		pickImportString(raw, "display_name", "displayName", "name"),
		pickImportString(userInfoObj, "displayName", "display_name", "name"),
	)
	actorARN := firstNonEmpty(
		pickImportString(raw, "actor_arn", "actorArn", "user_arn", "userArn", "profile_arn", "profileArn"),
		pickImportString(userInfoObj, "userArn", "user_arn"),
	)
	if actorARN == "" {
		if sub := importClaimString(claims, "sub"); strings.HasPrefix(sub, "arn:") {
			actorARN = sub
		}
	}
	awsAccountID := firstNonEmpty(
		pickImportString(raw, "aws_account_id", "awsAccountId", "account_id", "accountId"),
		pickImportString(userInfoObj, "awsAccountId", "aws_account_id", "accountId"),
		importClaimString(claims, "aws_account_id"),
	)
	accountName := firstNonEmpty(
		pickImportString(raw, "account_name", "accountName"),
		pickImportString(userInfoObj, "accountName", "account_name"),
	)

	tenantURL := strings.TrimRight(pickImportString(raw, "tenant_url", "tenantUrl", "data_plane", "dataPlane"), "/")
	wsHost := strings.TrimSpace(pickImportString(raw, "ws_host", "wsHost"))
	wsPath := strings.TrimSpace(pickImportString(raw, "ws_path", "wsPath"))

	if tenantURL != "" {
		if !strings.Contains(tenantURL, "://") {
			tenantURL = "https://" + tenantURL
		}
		if wsHost == "" {
			if u, err := url.Parse(tenantURL); err == nil && u.Host != "" {
				wsHost = u.Host
			}
		}
	} else if wsHost != "" {
		cleanHost := strings.TrimPrefix(strings.TrimPrefix(wsHost, "wss://"), "ws://")
		cleanHost = strings.TrimPrefix(strings.TrimPrefix(cleanHost, "https://"), "http://")
		cleanHost = strings.TrimRight(cleanHost, "/")
		if u, err := url.Parse("https://" + cleanHost); err == nil && u.Host != "" {
			wsHost = u.Host
			tenantURL = "https://" + u.Host
		}
	}

	if tenantURL == "" && provider != "enterprise" && provider != "gateway" {
		tenantURL = defaultSocialDataPlane(stage)
		if wsHost == "" {
			if u, err := url.Parse(tenantURL); err == nil {
				wsHost = u.Host
			}
		}
	}
	if wsHost == "" && tenantURL != "" {
		if u, err := url.Parse(tenantURL); err == nil {
			wsHost = u.Host
		}
	}
	if wsPath == "" && (provider == "gateway" || provider == "social_gateway") {
		wsPath = defaultGatewayWSPath
	}

	clientID := pickImportString(raw, "client_id", "clientId")
	clientSecret := pickImportString(raw, "client_secret", "clientSecret")
	issuerURL := pickImportString(raw, "issuer_url", "issuerUrl", "start_url", "startUrl")
	authEndpoint := pickImportString(raw, "auth_endpoint", "authEndpoint")
	tokenEndpoint := pickImportString(raw, "token_endpoint", "tokenEndpoint")
	if tokenEndpoint == "" && (provider == "social" || provider == "social_gateway") {
		tokenEndpoint = "https://oidc." + idcRegion + ".amazonaws.com/token"
	}
	redirectURI := pickImportString(raw, "redirect_uri", "redirectUri")

	expiresAt := pickImportTimestamp(raw, "expires_at", "expiresAt")
	if expiresAt == 0 {
		expiresAt = importClaimInt64(claims, "exp")
	}
	if expiresAt == 0 && refreshToken != "" && accessToken == "" && idToken == "" {
		// 仅提供 refresh_token 时设为 1，使其首次使用时自动刷新。
		expiresAt = 1
	}
	obtainedAt := pickImportTimestamp(raw, "obtained_at", "obtainedAt")
	if obtainedAt == 0 {
		obtainedAt = time.Now().Unix()
	}

	if email == "" {
		email = fallbackImportEmail(actorARN, awsAccountID, displayName, idToken, accessToken, refreshToken)
	}
	if displayName == "" {
		displayName = email
	}

	return &store.Account{
		Email:         email,
		DisplayName:   displayName,
		Provider:      provider,
		BuildStage:    stage,
		Region:        region,
		TenantURL:     tenantURL,
		WSHost:        wsHost,
		WSPath:        wsPath,
		AccessToken:   accessToken,
		RefreshToken:  refreshToken,
		IDToken:       idToken,
		ActorARN:      actorARN,
		AWSAccountID:  awsAccountID,
		AccountName:   accountName,
		ClientID:      clientID,
		ClientSecret:  clientSecret,
		IssuerURL:     issuerURL,
		IDCRegion:     idcRegion,
		AuthEndpoint:  authEndpoint,
		TokenEndpoint: tokenEndpoint,
		RedirectURI:   redirectURI,
		UserInfo:      userInfoStr,
		ExpiresAt:     expiresAt,
		ObtainedAt:    obtainedAt,
		Disabled:      pickImportBool(raw, "disabled"),
	}, nil
}

func fallbackImportEmail(actorARN, awsAccountID, displayName, idToken, accessToken, refreshToken string) string {
	for _, candidate := range []string{actorARN, awsAccountID, displayName} {
		if strings.TrimSpace(candidate) != "" {
			return strings.TrimSpace(candidate)
		}
	}
	seed := firstNonEmpty(refreshToken, idToken, accessToken)
	sum := sha256.Sum256([]byte(seed))
	return "imported-" + hex.EncodeToString(sum[:5])
}

func defaultSocialDataPlane(stage string) string {
	switch stage {
	case "beta", "integ", "dev":
		return "https://cell001.dp.appintegrations.us-west-2.beta.plato.ai.aws.dev"
	default:
		return "https://cell001.dp.appintegrations.us-east-1.prod.plato.ai.aws.dev"
	}
}

func normalizeImportProvider(raw string) string {
	p := strings.ToLower(strings.TrimSpace(raw))
	switch p {
	case "enterprise", "gateway", "social_gateway", "social":
		return p
	case "builder-id", "builderid", "idc", "":
		return "social"
	default:
		return "social"
	}
}

func normalizeImportStage(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "prod", "beta", "integ", "dev":
		return s
	default:
		return "prod"
	}
}

func extractImportUserInfo(raw map[string]any) (map[string]any, string) {
	for _, key := range []string{"user_info", "userInfo"} {
		v, ok := raw[key]
		if !ok || v == nil {
			continue
		}
		switch typed := v.(type) {
		case string:
			trimmed := strings.TrimSpace(typed)
			if trimmed == "" {
				continue
			}
			var m map[string]any
			if json.Unmarshal([]byte(trimmed), &m) == nil {
				return m, trimmed
			}
			return nil, trimmed
		case map[string]any:
			b, err := json.Marshal(typed)
			if err == nil {
				return typed, string(b)
			}
			return typed, ""
		}
	}
	return nil, ""
}

func pickImportString(raw map[string]any, keys ...string) string {
	if raw == nil {
		return ""
	}
	for _, key := range keys {
		v, ok := raw[key]
		if !ok || v == nil {
			continue
		}
		switch x := v.(type) {
		case string:
			if s := strings.TrimSpace(x); s != "" {
				return s
			}
		case json.Number:
			if s := strings.TrimSpace(x.String()); s != "" {
				return s
			}
		case float64, int64, int:
			if s := strings.TrimSpace(fmt.Sprint(x)); s != "" {
				return s
			}
		}
	}
	return ""
}

func pickImportBool(raw map[string]any, keys ...string) bool {
	if raw == nil {
		return false
	}
	for _, key := range keys {
		v, ok := raw[key]
		if !ok || v == nil {
			continue
		}
		switch x := v.(type) {
		case bool:
			return x
		case string:
			b, err := strconv.ParseBool(strings.TrimSpace(x))
			if err == nil {
				return b
			}
		}
	}
	return false
}

func pickImportTimestamp(raw map[string]any, keys ...string) int64 {
	if raw == nil {
		return 0
	}
	for _, key := range keys {
		v, ok := raw[key]
		if !ok || v == nil {
			continue
		}
		var n int64
		switch x := v.(type) {
		case json.Number:
			if parsed, err := x.Int64(); err == nil {
				n = parsed
			} else if f, err := x.Float64(); err == nil {
				n = int64(f)
			}
		case float64:
			n = int64(x)
		case int64:
			n = x
		case int:
			n = int64(x)
		case string:
			s := strings.TrimSpace(x)
			if s == "" {
				continue
			}
			if parsed, err := strconv.ParseInt(s, 10, 64); err == nil {
				n = parsed
			} else if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				n = t.Unix()
			} else if t, err := time.Parse(time.RFC3339, s); err == nil {
				n = t.Unix()
			} else if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
				n = t.Unix()
			}
		}
		if n > 1e12 {
			n /= 1000
		}
		if n > 0 {
			return n
		}
	}
	return 0
}

func decodeImportJWT(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 || parts[1] == "" {
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

func importClaimString(claims map[string]any, key string) string {
	if claims == nil {
		return ""
	}
	s, _ := claims[key].(string)
	return strings.TrimSpace(s)
}

func importClaimInt64(claims map[string]any, key string) int64 {
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

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
