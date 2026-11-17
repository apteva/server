package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	integrationGrokBuildSlug       = "grok-build"
	integrationGrokBuildIssuer     = "https://auth.x.ai"
	integrationGrokBuildClientID   = "b1a00492-073a-47ea-816f-4c329264a828"
	integrationGrokBuildRuntimeURL = "https://cli-chat-proxy.grok.com/v1"
	grokBuildClientVersion         = "1.0.38"
	grokBuildDeviceGrantType       = "urn:ietf:params:oauth:grant-type:device_code"
)

var (
	integrationGrokBuildDeviceCodeURL = integrationGrokBuildIssuer + "/oauth2/device/code"
	integrationGrokBuildTokenURL      = integrationGrokBuildIssuer + "/oauth2/token"
	grokBuildSessionHTTPClient        = &http.Client{Timeout: 20 * time.Second}
)

type connectionDeviceAuthorization struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               int
	Interval                int
}

type connectionDevicePollResult struct {
	Status          string
	Credentials     map[string]string
	Error           string
	NextPollSeconds int
}

// connectionSessionAuthDriver keeps subscription-backed provider details out
// of the connection handlers. OpenAI Codex remains one driver with its
// existing wire protocol; Grok Build uses the RFC 8628 device flow. Future
// session providers can be added without teaching the handlers another set of
// token fields or response codes.
type connectionSessionAuthDriver interface {
	Start(context.Context) (*connectionDeviceAuthorization, error)
	Poll(context.Context, *connectionDeviceAuthSession) (*connectionDevicePollResult, error)
	NeedsRefresh(map[string]string, time.Duration) bool
	Refresh(context.Context, map[string]string) error
	RuntimeToken(map[string]string) (map[string]any, error)
}

func connectionSessionAuthDriverFor(slug string) connectionSessionAuthDriver {
	switch strings.ToLower(strings.TrimSpace(slug)) {
	case integrationOpenAICodexSlug:
		return openAICodexSessionAuthDriver{}
	case integrationGrokBuildSlug:
		return grokBuildSessionAuthDriver{}
	default:
		return nil
	}
}

type openAICodexSessionAuthDriver struct{}

func (openAICodexSessionAuthDriver) Start(ctx context.Context) (*connectionDeviceAuthorization, error) {
	var payload struct {
		UserCode     string                   `json:"user_code"`
		DeviceAuthID string                   `json:"device_auth_id"`
		ExpiresIn    connectionDeviceLooseInt `json:"expires_in"`
		Interval     connectionDeviceLooseInt `json:"interval"`
	}
	if err := postConnectionDeviceJSON(ctx, integrationOpenAICodexDeviceUserCodeURL, map[string]string{"client_id": integrationOpenAICodexClientID}, &payload); err != nil {
		return nil, err
	}
	if payload.UserCode == "" || payload.DeviceAuthID == "" {
		return nil, fmt.Errorf("OpenAI Codex device auth response was incomplete")
	}
	return &connectionDeviceAuthorization{
		DeviceCode:      payload.DeviceAuthID,
		UserCode:        payload.UserCode,
		VerificationURI: integrationOpenAICodexIssuer + "/codex/device",
		ExpiresIn:       int(payload.ExpiresIn),
		Interval:        int(payload.Interval),
	}, nil
}

func (openAICodexSessionAuthDriver) Poll(ctx context.Context, session *connectionDeviceAuthSession) (*connectionDevicePollResult, error) {
	var codeResp struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	status, body, err := postConnectionDeviceJSONStatus(ctx, integrationOpenAICodexDeviceTokenURL, map[string]string{
		"device_auth_id": session.DeviceAuthID,
		"user_code":      session.UserCode,
	}, &codeResp)
	if err != nil {
		return nil, err
	}
	if status == http.StatusForbidden || status == http.StatusNotFound {
		return &connectionDevicePollResult{Status: "pending", NextPollSeconds: session.Interval}, nil
	}
	if status < 200 || status >= 300 {
		return &connectionDevicePollResult{Status: "failed", Error: strings.TrimSpace(string(body))}, nil
	}
	if codeResp.AuthorizationCode == "" || codeResp.CodeVerifier == "" {
		return &connectionDevicePollResult{Status: "failed", Error: "OpenAI Codex device auth response was incomplete"}, nil
	}
	tokens, err := exchangeConnectionOpenAICodexCode(ctx, codeResp.AuthorizationCode, codeResp.CodeVerifier)
	if err != nil {
		return nil, err
	}
	return &connectionDevicePollResult{Status: "connected", Credentials: buildConnectionOpenAICodexCredentials(tokens)}, nil
}

func (openAICodexSessionAuthDriver) NeedsRefresh(credentials map[string]string, skew time.Duration) bool {
	return connectionOpenAICodexNeedsRefresh(credentials, skew)
}

func (openAICodexSessionAuthDriver) Refresh(ctx context.Context, credentials map[string]string) error {
	return refreshIntegrationOpenAICodexCredentialsContext(ctx, credentials)
}

func (openAICodexSessionAuthDriver) RuntimeToken(credentials map[string]string) (map[string]any, error) {
	token := strings.TrimSpace(credentials["access_token"])
	if token == "" {
		return nil, fmt.Errorf("OpenAI Codex auth is missing access_token")
	}
	return map[string]any{
		"provider":     openAICodexAuthProvider,
		"token_type":   "Bearer",
		"access_token": token,
		"account_id":   credentials["account_id"],
		"expires_at":   connectionCredentialExpiry(credentials),
	}, nil
}

type grokBuildSessionAuthDriver struct{}

func (grokBuildSessionAuthDriver) Start(ctx context.Context) (*connectionDeviceAuthorization, error) {
	status, body, err := postGrokBuildForm(ctx, integrationGrokBuildDeviceCodeURL, map[string]string{
		"client_id": integrationGrokBuildClientID,
		"scope":     "openid profile email offline_access grok-cli:access api:access",
		"referrer":  "grok-build",
	})
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("Grok Build device code request failed with status %d: %s", status, summarizeUpstreamError(body))
	}
	var payload struct {
		DeviceCode              string                   `json:"device_code"`
		UserCode                string                   `json:"user_code"`
		VerificationURI         string                   `json:"verification_uri"`
		VerificationURIComplete string                   `json:"verification_uri_complete"`
		ExpiresIn               connectionDeviceLooseInt `json:"expires_in"`
		Interval                connectionDeviceLooseInt `json:"interval"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode Grok Build device code: %w", err)
	}
	if payload.DeviceCode == "" || payload.UserCode == "" || payload.VerificationURI == "" {
		return nil, fmt.Errorf("Grok Build device auth response was incomplete")
	}
	if err := validateConnectionVerificationURI(payload.VerificationURI); err != nil {
		return nil, err
	}
	if payload.VerificationURIComplete != "" {
		if err := validateConnectionVerificationURI(payload.VerificationURIComplete); err != nil {
			return nil, err
		}
	}
	return &connectionDeviceAuthorization{
		DeviceCode:              payload.DeviceCode,
		UserCode:                payload.UserCode,
		VerificationURI:         payload.VerificationURI,
		VerificationURIComplete: payload.VerificationURIComplete,
		ExpiresIn:               int(payload.ExpiresIn),
		Interval:                int(payload.Interval),
	}, nil
}

func (grokBuildSessionAuthDriver) Poll(ctx context.Context, session *connectionDeviceAuthSession) (*connectionDevicePollResult, error) {
	status, body, err := postGrokBuildForm(ctx, integrationGrokBuildTokenURL, map[string]string{
		"grant_type":  grokBuildDeviceGrantType,
		"device_code": session.DeviceAuthID,
		"client_id":   integrationGrokBuildClientID,
	})
	if err != nil {
		return nil, err
	}
	if status >= 200 && status < 300 {
		var tokens map[string]any
		if err := json.Unmarshal(body, &tokens); err != nil {
			return nil, fmt.Errorf("decode Grok Build token response: %w", err)
		}
		if strings.TrimSpace(stringValue(tokens["access_token"])) == "" {
			return nil, fmt.Errorf("Grok Build token response missing access_token")
		}
		return &connectionDevicePollResult{Status: "connected", Credentials: buildConnectionGrokBuildCredentials(tokens)}, nil
	}
	var upstream struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &upstream)
	detail := strings.TrimSpace(upstream.ErrorDescription)
	if detail == "" {
		detail = strings.TrimSpace(upstream.Error)
	}
	if detail == "" {
		detail = summarizeUpstreamError(body)
	}
	switch upstream.Error {
	case "authorization_pending":
		return &connectionDevicePollResult{Status: "pending", NextPollSeconds: session.Interval}, nil
	case "slow_down":
		return &connectionDevicePollResult{Status: "pending", NextPollSeconds: session.Interval + 5}, nil
	case "expired_token":
		return &connectionDevicePollResult{Status: "expired", Error: detail}, nil
	case "access_denied":
		return &connectionDevicePollResult{Status: "failed", Error: detail}, nil
	default:
		return &connectionDevicePollResult{Status: "failed", Error: detail}, nil
	}
}

func (grokBuildSessionAuthDriver) NeedsRefresh(credentials map[string]string, skew time.Duration) bool {
	return connectionCredentialsNeedRefresh(credentials, skew)
}

func (grokBuildSessionAuthDriver) Refresh(ctx context.Context, credentials map[string]string) error {
	refreshToken := strings.TrimSpace(credentials["refresh_token"])
	if refreshToken == "" {
		return fmt.Errorf("Grok Build connection is missing refresh_token")
	}
	fields := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     integrationGrokBuildClientID,
	}
	for _, key := range []string{"principal_type", "principal_id"} {
		if value := strings.TrimSpace(credentials[key]); value != "" {
			fields[key] = value
		}
	}
	status, body, err := postGrokBuildForm(ctx, integrationGrokBuildTokenURL, fields)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("Grok Build token refresh failed with status %d: %s", status, summarizeUpstreamError(body))
	}
	var tokens map[string]any
	if err := json.Unmarshal(body, &tokens); err != nil {
		return fmt.Errorf("decode Grok Build refresh response: %w", err)
	}
	next := buildConnectionGrokBuildCredentials(tokens)
	if next["access_token"] == "" {
		return fmt.Errorf("Grok Build token response missing access_token")
	}
	if next["refresh_token"] == "" {
		next["refresh_token"] = refreshToken
	}
	for _, key := range []string{"account_id", "account_email", "user_id", "principal_type", "principal_id"} {
		if next[key] == "" {
			next[key] = credentials[key]
		}
	}
	for key, value := range next {
		credentials[key] = value
	}
	return nil
}

func (grokBuildSessionAuthDriver) RuntimeToken(credentials map[string]string) (map[string]any, error) {
	token := strings.TrimSpace(credentials["access_token"])
	if token == "" {
		return nil, fmt.Errorf("Grok Build auth is missing access_token")
	}
	return map[string]any{
		"provider":       integrationGrokBuildSlug,
		"token_type":     "Bearer",
		"access_token":   token,
		"account_id":     credentials["account_id"],
		"account_email":  credentials["account_email"],
		"user_id":        credentials["user_id"],
		"principal_type": credentials["principal_type"],
		"principal_id":   credentials["principal_id"],
		"base_url":       integrationGrokBuildRuntimeURL,
		"expires_at":     connectionCredentialExpiry(credentials),
	}, nil
}

func postGrokBuildForm(ctx context.Context, endpoint string, fields map[string]string) (int, []byte, error) {
	form := url.Values{}
	for key, value := range fields {
		form.Set(key, value)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-grok-client-version", grokBuildClientVersion)
	req.Header.Set("x-grok-client-surface", "ui")
	resp, err := grokBuildSessionHTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, err
}

func buildConnectionGrokBuildCredentials(tokens map[string]any) map[string]string {
	accessToken := strings.TrimSpace(stringValue(tokens["access_token"]))
	refreshToken := strings.TrimSpace(stringValue(tokens["refresh_token"]))
	expiresAt := ""
	if expiresIn := loosePositiveInt64(tokens["expires_in"]); expiresIn > 0 {
		expiresAt = time.Now().UTC().Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339)
	} else if exp, ok := connectionJWTExpiry(accessToken); ok {
		expiresAt = exp.UTC().Format(time.RFC3339)
	}
	claims := connectionJWTClaims(accessToken)
	if idToken := strings.TrimSpace(stringValue(tokens["id_token"])); idToken != "" {
		if idClaims := connectionJWTClaims(idToken); len(idClaims) > 0 {
			for key, value := range idClaims {
				if _, exists := claims[key]; !exists {
					claims[key] = value
				}
			}
		}
	}
	creds := map[string]string{
		"access_token":     accessToken,
		"token":            accessToken,
		"bearer_token":     accessToken,
		"refresh_token":    refreshToken,
		"token_expires_at": expiresAt,
		"expires_at":       expiresAt,
		"last_refresh":     time.Now().UTC().Format(time.RFC3339),
		"auth_provider":    integrationGrokBuildSlug,
		"auth_type":        connectionAuthTypeDeviceCode,
		"runtime_base_url": integrationGrokBuildRuntimeURL,
		"client_version":   grokBuildClientVersion,
	}
	copyTokenString := func(target string, sources ...string) {
		for _, source := range sources {
			if value := strings.TrimSpace(stringValue(tokens[source])); value != "" {
				creds[target] = value
				return
			}
			if value := strings.TrimSpace(stringValue(claims[source])); value != "" {
				creds[target] = value
				return
			}
		}
	}
	copyTokenString("principal_type", "principal_type", "principalType")
	copyTokenString("principal_id", "principal_id", "principalId")
	copyTokenString("user_id", "user_id", "userId", "sub")
	copyTokenString("account_email", "email")
	if creds["user_id"] == "" && creds["principal_id"] != "" {
		// Team and organization grants may omit openid/sub. Grok Build uses
		// the selected principal as x-userid for those sessions.
		creds["user_id"] = creds["principal_id"]
	}
	if creds["principal_id"] != "" {
		creds["account_id"] = creds["principal_id"]
	} else {
		creds["account_id"] = creds["user_id"]
	}
	return creds
}

func connectionCredentialExpiry(credentials map[string]string) string {
	if expiresAt := strings.TrimSpace(credentials["token_expires_at"]); expiresAt != "" {
		return expiresAt
	}
	return strings.TrimSpace(credentials["expires_at"])
}

func connectionCredentialsNeedRefresh(credentials map[string]string, skew time.Duration) bool {
	raw := connectionCredentialExpiry(credentials)
	if raw == "" {
		return false
	}
	expiresAt, err := time.Parse(time.RFC3339, raw)
	return err == nil && time.Until(expiresAt) <= skew
}

func loosePositiveInt64(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int:
		return int64(typed)
	case int64:
		return typed
	case json.Number:
		parsed, _ := typed.Int64()
		return parsed
	case string:
		parsed, _ := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return parsed
	default:
		return 0
	}
}

func validateConnectionVerificationURI(raw string) error {
	if strings.IndexFunc(raw, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return fmt.Errorf("device auth returned an invalid verification URI")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return fmt.Errorf("device auth returned an invalid verification URI")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1") {
		return nil
	}
	return fmt.Errorf("device auth returned an unsupported verification URI")
}
