package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// trustedAppPrincipal captures a delegated application user only after the
// ordinary server auth middleware has replaced all client-supplied principal
// headers. A platform operator (session or API key) is deliberately not an
// application user: its validated X-Apteva-Operator-ID remains the authority
// forwarded to the sidecar.
func (s *Server) trustedAppPrincipal(r *http.Request, projectID string) *sdk.TrustedPrincipal {
	if r == nil {
		return nil
	}
	userID := getUserID(r)
	subjectType := strings.TrimSpace(r.Header.Get("X-Apteva-Subject-Type"))
	subjectID := strings.TrimSpace(r.Header.Get("X-Apteva-Subject-ID"))
	if subjectType == "" || subjectID == "" {
		return nil
	}
	principal := &sdk.TrustedPrincipal{
		Version:          sdk.TrustedPrincipalVersion,
		UserID:           userID,
		SubjectType:      subjectType,
		SubjectID:        subjectID,
		SubjectEmail:     strings.TrimSpace(r.Header.Get("X-Apteva-Subject-Email")),
		OrganizationID:   strings.TrimSpace(r.Header.Get("X-Apteva-Organization-ID")),
		OrganizationSlug: strings.TrimSpace(r.Header.Get("X-Apteva-Organization-Slug")),
		ProjectID:        projectID,
		ExpiresAt:        time.Now().Add(time.Minute).Unix(),
	}
	if userID > 0 {
		if user, err := s.store.GetUserByID(userID); err == nil && user != nil {
			principal.Email = user.Email
			if principal.SubjectEmail == "" && principal.SubjectType == "user" {
				principal.SubjectEmail = user.Email
			}
		}
	}
	return principal
}

func setTrustedAppPrincipalHeaders(req *http.Request, token string, principal *sdk.TrustedPrincipal) {
	if req == nil || token == "" || principal == nil {
		return
	}
	raw, err := json.Marshal(principal)
	if err != nil {
		return
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = mac.Write([]byte(payload))
	req.Header.Set(sdk.HeaderTrustedPrincipal, payload)
	req.Header.Set(sdk.HeaderTrustedPrincipalSignature, hex.EncodeToString(mac.Sum(nil)))
	// Keep the established Caller fields populated for older target SDKs.
	if principal.SubjectType != "" {
		req.Header.Set("X-Apteva-Subject-Type", principal.SubjectType)
		req.Header.Set("X-Apteva-Subject-ID", principal.SubjectID)
		req.Header.Set("X-Apteva-Subject-Email", principal.SubjectEmail)
	}
	if principal.OrganizationID != "" {
		req.Header.Set("X-Apteva-Organization-ID", principal.OrganizationID)
	}
	if principal.OrganizationSlug != "" {
		req.Header.Set("X-Apteva-Organization-Slug", principal.OrganizationSlug)
	}
	if principal.ProjectID != "" {
		req.Header.Set("X-Apteva-Project-ID", principal.ProjectID)
	}
}
