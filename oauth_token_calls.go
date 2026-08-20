package main

// oauth_token_calls.go — the non-RFC-6749 corners of the OAuth engine.
//
// Three providers' worth of divergence, expressed declaratively so a new
// one is a catalog edit rather than a Go change:
//
//   - the token object arrives nested instead of at the top level
//     (Instagram: {"data":[{"access_token":…}]})
//   - the code grant returns a short-lived token that needs a second
//     call to become durable (Instagram: 1 hour → 60 days)
//   - refresh is a GET that re-presents the CURRENT access token,
//     because no refresh_token exists (Instagram, Threads)
//
// Threads is byte-identical to Instagram on a different host, which is
// why none of this is keyed on slug.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"
)

// oauthTokenExpirySkew is how early a token counts as expiring. Wide
// enough that a long agent run started just under the wire still holds a
// valid token when it finishes.
const oauthTokenExpirySkew = 10 * time.Minute

// unwrapTokenResponse walks a dotted path into a decoded token response.
// Numeric segments index arrays. Returns the input untouched when the
// path is empty, so the standard top-level shape costs nothing.
func unwrapTokenResponse(raw map[string]any, path string) (map[string]any, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return raw, nil
	}
	var cursor any = raw
	for _, segment := range strings.Split(path, ".") {
		if index, err := strconv.Atoi(segment); err == nil {
			list, ok := cursor.([]any)
			if !ok || index < 0 || index >= len(list) {
				return nil, fmt.Errorf("token_response_path %q: no element %d", path, index)
			}
			cursor = list[index]
			continue
		}
		obj, ok := cursor.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("token_response_path %q: %q is not an object", path, segment)
		}
		cursor, ok = obj[segment]
		if !ok {
			return nil, fmt.Errorf("token_response_path %q: no key %q", path, segment)
		}
	}
	out, ok := cursor.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("token_response_path %q does not address an object", path)
	}
	return out, nil
}

// runOAuthTokenCall performs a declared follow-up token request and
// returns the decoded response as a flat string map, applying the same
// TokenResponsePath unwrapping the primary exchange uses.
func runOAuthTokenCall(
	call *OAuthTokenCall, cfg *OAuthConfig,
	credentials map[string]string, clientID, clientSecret string,
) (map[string]string, error) {
	if call == nil || strings.TrimSpace(call.URL) == "" {
		return nil, fmt.Errorf("token call has no url")
	}

	credentialKey := call.Credential
	if credentialKey == "" {
		credentialKey = "access_token"
	}
	token := credentials[credentialKey]
	if token == "" {
		return nil, fmt.Errorf("no %s in credentials", credentialKey)
	}

	params := neturl.Values{}
	for k, v := range call.Params {
		params.Set(k, v)
	}
	params.Set("access_token", token)
	// Per-call, not per-app: Instagram's long-lived exchange requires the
	// secret and its refresh call rejects it outright.
	if call.SendClientSecret {
		if clientSecret == "" {
			return nil, fmt.Errorf("token call requires client_secret but none is available")
		}
		params.Set("client_secret", clientSecret)
	}

	method := strings.ToUpper(strings.TrimSpace(call.Method))
	if method == "" {
		method = http.MethodGet
	}

	var req *http.Request
	var err error
	if method == http.MethodGet {
		endpoint := call.URL
		if strings.Contains(endpoint, "?") {
			endpoint += "&" + params.Encode()
		} else {
			endpoint += "?" + params.Encode()
		}
		req, err = http.NewRequest(method, endpoint, nil)
	} else {
		req, err = http.NewRequest(method, call.URL, strings.NewReader(params.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	}
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1_000_000))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("token call %s: http %d: %s", call.URL, resp.StatusCode, string(body))
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("token call json decode: %w", err)
	}
	path := ""
	if cfg != nil {
		path = cfg.TokenResponsePath
	}
	unwrapped, err := unwrapTokenResponse(raw, path)
	if err != nil {
		return nil, err
	}

	out := map[string]string{}
	for k, v := range unwrapped {
		out[k] = fmt.Sprint(v)
	}
	if out["access_token"] == "" {
		return nil, fmt.Errorf("no access_token in token call response: %s", string(body))
	}
	return out, nil
}

// applyTokenExpiry records when the token stops working, from the
// provider's expires_in (seconds).
//
// Nothing stored an absolute expiry before this: refresh was on-401
// only, which is too late for a provider whose expired tokens cannot be
// refreshed at all. Instagram's 60-day token is the case that forces the
// issue — a connection idle past 60 days is dead permanently, and the
// only way to notice in time is to know when it dies.
func applyTokenExpiry(credentials map[string]string, tokens map[string]string, now time.Time) {
	raw := strings.TrimSpace(tokens["expires_in"])
	if raw == "" {
		return
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || seconds <= 0 {
		return
	}
	credentials["expires_at"] = now.Add(time.Duration(seconds) * time.Second).
		UTC().Format(time.RFC3339)
}

// oauthTokenNeedsRefresh reports whether a stored token is at or past
// its expiry, within the skew. Unknown expiry means "assume fine" — most
// catalog entries never report expires_in, and treating silence as
// expiry would refresh every call.
func oauthTokenNeedsRefresh(credentials map[string]string, skew time.Duration) bool {
	raw := strings.TrimSpace(credentials["expires_at"])
	if raw == "" {
		return false
	}
	expiry, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return false
	}
	return time.Until(expiry) <= skew
}

// oauthCanRefresh reports whether this app has any way to refresh.
// A declared cfg.Refresh needs no refresh_token — that is the whole
// point of it, since Instagram never issues one.
func oauthCanRefresh(cfg *OAuthConfig, credentials map[string]string) bool {
	if cfg == nil {
		return false
	}
	if cfg.Refresh != nil {
		return true
	}
	return credentials["refresh_token"] != "" || credentials["refreshToken"] != ""
}
