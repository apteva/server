package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	googleOAuthTokenURL    = "https://oauth2.googleapis.com/token"
	firebaseMessagingScope = "https://www.googleapis.com/auth/firebase.messaging"
)

func init() { RegisterSigner(newGoogleServiceAccountSigner()) }

type googleServiceAccountCredentials struct {
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
}

type googleAccessToken struct {
	Value     string
	ExpiresAt time.Time
}

type googleServiceAccountSigner struct {
	client *http.Client
	mu     sync.Mutex
	cache  map[string]googleAccessToken
}

func newGoogleServiceAccountSigner() *googleServiceAccountSigner {
	return &googleServiceAccountSigner{
		client: &http.Client{Timeout: 12 * time.Second},
		cache:  make(map[string]googleAccessToken),
	}
}

func (s *googleServiceAccountSigner) Name() string { return "google_service_account" }

func (s *googleServiceAccountSigner) Sign(
	ctx context.Context,
	req *http.Request,
	_ []byte,
	creds map[string]string,
	params map[string]any,
) ([]byte, error) {
	rawServiceAccount := strings.TrimSpace(creds["service_account_json"])
	if rawServiceAccount == "" {
		authorization := strings.TrimSpace(req.Header.Get("Authorization"))
		if authorization == "" || strings.EqualFold(authorization, "Bearer") {
			return nil, fmt.Errorf("OAuth access token or legacy service_account_json is required")
		}
		return nil, nil
	}

	serviceAccount, err := parseGoogleServiceAccount(rawServiceAccount)
	if err != nil {
		return nil, err
	}
	tokenURL := googleOAuthTokenURL
	if value, ok := params["token_url"].(string); ok && strings.TrimSpace(value) != "" {
		tokenURL = strings.TrimSpace(value)
	}
	scope := firebaseMessagingScope
	if value, ok := params["scope"].(string); ok && strings.TrimSpace(value) != "" {
		scope = strings.TrimSpace(value)
	}
	token, err := s.accessToken(ctx, serviceAccount, tokenURL, scope)
	if err != nil {
		return nil, err
	}

	for _, placeholder := range []string{"/projects/-/", "/projects/{project_id}/"} {
		if strings.Contains(req.URL.Path, placeholder) {
			req.URL.Path = strings.Replace(
				req.URL.Path,
				placeholder,
				"/projects/"+url.PathEscape(serviceAccount.ProjectID)+"/",
				1,
			)
			break
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil, nil
}

func parseGoogleServiceAccount(raw string) (*googleServiceAccountCredentials, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("service_account_json is required")
	}
	var credentials googleServiceAccountCredentials
	if err := json.Unmarshal([]byte(raw), &credentials); err != nil {
		return nil, fmt.Errorf("service_account_json must be the JSON key downloaded from Firebase: %w", err)
	}
	credentials.ProjectID = strings.TrimSpace(credentials.ProjectID)
	credentials.PrivateKeyID = strings.TrimSpace(credentials.PrivateKeyID)
	credentials.ClientEmail = strings.TrimSpace(credentials.ClientEmail)
	if credentials.ProjectID == "" || credentials.ClientEmail == "" || strings.TrimSpace(credentials.PrivateKey) == "" {
		return nil, fmt.Errorf("service_account_json is missing project_id, client_email, or private_key")
	}
	if strings.ContainsAny(credentials.ProjectID, "/?#") {
		return nil, fmt.Errorf("service account project_id is invalid")
	}
	return &credentials, nil
}

func (s *googleServiceAccountSigner) accessToken(
	ctx context.Context,
	credentials *googleServiceAccountCredentials,
	tokenURL string,
	scope string,
) (string, error) {
	cacheKeyBytes := sha256.Sum256([]byte(
		credentials.ProjectID + "\x00" + credentials.ClientEmail + "\x00" +
			credentials.PrivateKeyID + "\x00" + credentials.PrivateKey + "\x00" +
			tokenURL + "\x00" + scope,
	))
	cacheKey := base64.RawURLEncoding.EncodeToString(cacheKeyBytes[:])

	s.mu.Lock()
	if cached, ok := s.cache[cacheKey]; ok && time.Until(cached.ExpiresAt) > 2*time.Minute {
		s.mu.Unlock()
		return cached.Value, nil
	}
	s.mu.Unlock()

	privateKey, err := parseGoogleRSAPrivateKey(credentials.PrivateKey)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	assertion, err := signJWT(
		map[string]any{
			"alg": "RS256",
			"typ": "JWT",
			"kid": credentials.PrivateKeyID,
		},
		map[string]any{
			"iss":   credentials.ClientEmail,
			"sub":   credentials.ClientEmail,
			"aud":   tokenURL,
			"scope": scope,
			"iat":   now.Unix(),
			"exp":   now.Add(time.Hour).Unix(),
		},
		func(message []byte) ([]byte, error) {
			digest := sha256.Sum256(message)
			return rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
		},
	)
	if err != nil {
		return "", fmt.Errorf("sign Google service account assertion: %w", err)
	}

	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		tokenURL,
		bytes.NewBufferString(form.Encode()),
	)
	if err != nil {
		return "", fmt.Errorf("create Google OAuth token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := s.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("exchange Google service account assertion: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("read Google OAuth token response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("Google OAuth token exchange returned HTTP %d: %s",
			response.StatusCode, strings.TrimSpace(string(data)))
	}
	var tokenResponse struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   any    `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &tokenResponse); err != nil {
		return "", fmt.Errorf("decode Google OAuth token response: %w", err)
	}
	tokenResponse.AccessToken = strings.TrimSpace(tokenResponse.AccessToken)
	if tokenResponse.AccessToken == "" {
		return "", fmt.Errorf("Google OAuth token response did not include access_token")
	}
	expiresIn := googleExpiresIn(tokenResponse.ExpiresIn)
	if expiresIn <= 0 {
		expiresIn = time.Hour
	}
	s.mu.Lock()
	s.cache[cacheKey] = googleAccessToken{
		Value:     tokenResponse.AccessToken,
		ExpiresAt: now.Add(expiresIn),
	}
	s.mu.Unlock()
	return tokenResponse.AccessToken, nil
}

func parseGoogleRSAPrivateKey(raw string) (*rsa.PrivateKey, error) {
	normalized := strings.TrimSpace(raw)
	normalized = strings.ReplaceAll(normalized, `\r\n`, "\n")
	normalized = strings.ReplaceAll(normalized, `\n`, "\n")
	normalized = strings.ReplaceAll(normalized, `\r`, "\n")
	block, _ := pem.Decode([]byte(normalized))
	if block == nil {
		return nil, fmt.Errorf("service account private_key must be a PEM encoded RSA key")
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if key, ok := parsed.(*rsa.PrivateKey); ok {
			return key, nil
		}
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("service account private_key must be an RSA private key")
}

func googleExpiresIn(value any) time.Duration {
	switch typed := value.(type) {
	case float64:
		return time.Duration(typed) * time.Second
	case json.Number:
		seconds, _ := typed.Int64()
		return time.Duration(seconds) * time.Second
	case string:
		seconds, _ := strconv.ParseInt(typed, 10, 64)
		return time.Duration(seconds) * time.Second
	default:
		return 0
	}
}
