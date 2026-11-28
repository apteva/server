package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// Asserted subjects.
//
// An app that authenticates its own users (an API gateway, for one) can say
// which end user a call to another app is for. It sends the subject in
// sdk.HeaderAssertSubjectType and sdk.HeaderAssertSubjectID on an
// app-to-app call, and needs the platform.principals.assert permission,
// granted when the app was installed.
//
// The platform does not know that subject. What it signs for the target is
// "this app says the call is for this subject": the principal carries the
// asserting app and install, and the target decides whether to accept
// subjects from that app. The subject type "user" is refused, because it
// means a platform user, whom only the platform can vouch for.

var errAssertedSubjectForbidden = errors.New("missing permission: " + string(sdk.PermPrincipalsAssert))

// assertedSubjectPrincipal builds the principal for an app-to-app call that
// names a subject. It returns nil when the call names none. A caller that
// names one without the permission is refused, not forwarded without it:
// the target would otherwise run the call with no end user at all.
func (s *Server) assertedSubjectPrincipal(r *http.Request, callerInstallID int64, callerAppName, projectID string) (*sdk.TrustedPrincipal, error) {
	rawType := strings.TrimSpace(r.Header.Get(sdk.HeaderAssertSubjectType))
	rawID := strings.TrimSpace(r.Header.Get(sdk.HeaderAssertSubjectID))
	if rawType == "" && rawID == "" {
		return nil, nil
	}
	if !installHasPermission(s, callerInstallID, sdk.PermPrincipalsAssert) {
		return nil, errAssertedSubjectForbidden
	}
	if rawType == "" {
		return nil, errors.New("asserted subject_type is required")
	}
	subjectType, subjectID, err := validateDelegatedSubject(rawType, rawID)
	if err != nil {
		return nil, fmt.Errorf("asserted %w", err)
	}
	if subjectType == "user" {
		return nil, errors.New(`asserted subject_type "user" is reserved for platform users`)
	}
	log.Printf("[APPS-CALL] asserted subject caller=%s caller_install=%d project=%s subject=%s:%s",
		callerAppName, callerInstallID, projectID, subjectType, truncate(subjectID, 80))
	return &sdk.TrustedPrincipal{
		Version:             sdk.TrustedPrincipalVersion,
		SubjectType:         subjectType,
		SubjectID:           subjectID,
		ProjectID:           projectID,
		AssertedByApp:       callerAppName,
		AssertedByInstallID: callerInstallID,
		ExpiresAt:           time.Now().Add(time.Minute).Unix(),
	}, nil
}

// writeAssertedSubjectError answers a refused assertion.
func writeAssertedSubjectError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, errAssertedSubjectForbidden) {
		status = http.StatusForbidden
	}
	http.Error(w, err.Error(), status)
}

// clearAssertedSubjectHeaders removes everything a forwarded request could
// carry that the target might mistake for a platform-signed identity, and
// the raw assertion itself. Call it before attaching the signed principal.
func clearAssertedSubjectHeaders(req *http.Request) {
	for _, header := range []string{
		sdk.HeaderAssertSubjectType, sdk.HeaderAssertSubjectID,
		sdk.HeaderTrustedPrincipal, sdk.HeaderTrustedPrincipalSignature,
		"X-Apteva-Subject-Type", "X-Apteva-Subject-ID", "X-Apteva-Subject-Email",
		"X-Apteva-Organization-ID", "X-Apteva-Organization-Slug",
	} {
		req.Header.Del(header)
	}
}
