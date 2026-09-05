package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	sdk "github.com/apteva/app-sdk"
)

// handleProvisioningApply accepts desired state pushed by a trusted remote
// controller using this Apteva instance's ordinary admin API key. It is the
// push counterpart to managed tenant reconciliation: the same grant and app
// installers are reused, but no controller URL or polling identity is needed.
func (s *Server) handleProvisioningApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "PUT only", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.requirePlatformAdmin(w, r); !ok {
		return
	}
	var body sdk.ManagedProvisioningApplyRequest
	r.Body = http.MaxBytesReader(w, r.Body, managedControlBodyLimit)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateManagedProvisioningRequest(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	requestHash, err := managedProvisioningRequestHash(body)
	if err != nil {
		http.Error(w, "encode provisioning request", http.StatusInternalServerError)
		return
	}
	prior, claimed, err := s.claimManagedProvisioning(body, requestHash)
	if err != nil {
		status := http.StatusConflict
		if !errors.Is(err, errManagedProvisioningConflict) {
			status = http.StatusInternalServerError
		}
		http.Error(w, err.Error(), status)
		return
	}
	if !claimed {
		writeJSON(w, prior)
		return
	}

	result := sdk.ManagedProvisioningApplyResult{
		RequestID:   body.RequestID,
		TenantID:    body.TenantID,
		Status:      "applied",
		Connections: map[string]int64{},
	}
	for _, grant := range body.Grants {
		id, applyErr := s.applyManagedGrant(body.TenantID, grant)
		if applyErr != nil {
			s.failManagedProvisioning(body.RequestID, applyErr)
			http.Error(w, "apply grant "+grant.GrantID+": "+applyErr.Error(), http.StatusBadGateway)
			return
		}
		result.Connections[grant.GrantID] = id
	}
	for _, grantID := range body.RevokedGrantIDs {
		externalID := "managed:" + body.TenantID + ":" + grantID
		if _, err := s.store.db.Exec(`UPDATE connections SET status='failed' WHERE external_id=? AND credential_management='controller'`, externalID); err != nil {
			s.failManagedProvisioning(body.RequestID, err)
			http.Error(w, "revoke grant "+grantID+": "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if body.Bundle != nil {
		result.BundleID = body.Bundle.BundleID
		result.Revision = body.Bundle.Revision
		if err := s.applyManagedBundle(*body.Bundle, result.Connections); err != nil {
			s.failManagedProvisioning(body.RequestID, err)
			http.Error(w, "apply bundle: "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	if err := s.completeManagedProvisioning(body.RequestID, result); err != nil {
		http.Error(w, "record provisioning result", http.StatusInternalServerError)
		return
	}
	s.auditManagedTenant(body.TenantID, "api-key", "provisioning.applied", result.BundleID, "success", body.RequestID)
	writeJSON(w, result)
}

func validateManagedProvisioningRequest(body *sdk.ManagedProvisioningApplyRequest) error {
	if body == nil {
		return errors.New("request is required")
	}
	body.RequestID = strings.TrimSpace(body.RequestID)
	body.TenantID = strings.TrimSpace(body.TenantID)
	if !validManagedID(body.RequestID) || !validManagedID(body.TenantID) {
		return errors.New("valid request_id and tenant_id are required")
	}
	if len(body.Grants) > 128 || len(body.RevokedGrantIDs) > 128 {
		return errors.New("grants and revoked_grant_ids are limited to 128 entries each")
	}
	seen := map[string]bool{}
	for i := range body.Grants {
		grant := &body.Grants[i]
		grant.TenantID = strings.TrimSpace(grant.TenantID)
		grant.GrantID = strings.TrimSpace(grant.GrantID)
		if grant.TenantID != body.TenantID || !validManagedID(grant.GrantID) || grant.AppSlug == "" || grant.ControllerToken == "" || grant.ControllerExecute == "" {
			return fmt.Errorf("grants[%d] is incomplete or belongs to another tenant", i)
		}
		if seen[grant.GrantID] {
			return fmt.Errorf("duplicate grant_id %q", grant.GrantID)
		}
		seen[grant.GrantID] = true
	}
	for i := range body.RevokedGrantIDs {
		body.RevokedGrantIDs[i] = strings.TrimSpace(body.RevokedGrantIDs[i])
		if !validManagedID(body.RevokedGrantIDs[i]) {
			return fmt.Errorf("revoked_grant_ids[%d] is invalid", i)
		}
		if seen[body.RevokedGrantIDs[i]] {
			return fmt.Errorf("grant_id %q cannot be both active and revoked", body.RevokedGrantIDs[i])
		}
	}
	if body.Bundle != nil {
		body.Bundle.TenantID = strings.TrimSpace(body.Bundle.TenantID)
		body.Bundle.BundleID = strings.TrimSpace(body.Bundle.BundleID)
		if body.Bundle.TenantID != body.TenantID || !validManagedID(body.Bundle.BundleID) || body.Bundle.Revision <= 0 {
			return errors.New("bundle must belong to tenant_id and have a valid bundle_id and positive revision")
		}
		if len(body.Bundle.Apps) > 64 {
			return errors.New("bundle apps are limited to 64")
		}
	}
	if len(body.Grants) == 0 && len(body.RevokedGrantIDs) == 0 && body.Bundle == nil {
		return errors.New("at least one grant, revocation, or bundle is required")
	}
	return nil
}

func managedProvisioningRequestHash(body sdk.ManagedProvisioningApplyRequest) (string, error) {
	// Transport/status fields are intentionally omitted. Only desired state
	// participates in conflict detection.
	type desiredBundle struct {
		TenantID string                 `json:"tenant_id"`
		BundleID string                 `json:"bundle_id"`
		Revision int64                  `json:"revision"`
		Apps     []sdk.ManagedBundleApp `json:"apps"`
	}
	var bundle *desiredBundle
	if body.Bundle != nil {
		bundle = &desiredBundle{
			TenantID: body.Bundle.TenantID,
			BundleID: body.Bundle.BundleID,
			Revision: body.Bundle.Revision,
			Apps:     body.Bundle.Apps,
		}
	}
	canonical := struct {
		TenantID        string                               `json:"tenant_id"`
		Grants          []sdk.ManagedConnectionGrantDelivery `json:"grants,omitempty"`
		RevokedGrantIDs []string                             `json:"revoked_grant_ids,omitempty"`
		Bundle          *desiredBundle                       `json:"bundle,omitempty"`
	}{TenantID: body.TenantID, Grants: body.Grants, RevokedGrantIDs: body.RevokedGrantIDs, Bundle: bundle}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	return HashAPIKey(string(raw)), nil
}

var errManagedProvisioningConflict = errors.New("managed provisioning request conflicts with recorded desired state")

func (s *Server) claimManagedProvisioning(body sdk.ManagedProvisioningApplyRequest, requestHash string) (*sdk.ManagedProvisioningApplyResult, bool, error) {
	bundleID := ""
	var revision int64
	if body.Bundle != nil {
		bundleID, revision = body.Bundle.BundleID, body.Bundle.Revision
	}
	var storedHash, status, responseJSON string
	err := s.store.db.QueryRow(`SELECT request_hash,status,response_json FROM managed_provisioning_requests WHERE request_id=?`, body.RequestID).
		Scan(&storedHash, &status, &responseJSON)
	if err == nil {
		if storedHash != requestHash {
			return nil, false, fmt.Errorf("%w: request_id was already used with different content", errManagedProvisioningConflict)
		}
		switch status {
		case "applied":
			var prior sdk.ManagedProvisioningApplyResult
			if json.Unmarshal([]byte(responseJSON), &prior) != nil {
				return nil, false, errors.New("stored provisioning response is invalid")
			}
			return &prior, false, nil
		case "applying":
			return nil, false, fmt.Errorf("%w: request is already applying", errManagedProvisioningConflict)
		default:
			if bundleID != "" {
				var latest int64
				_ = s.store.db.QueryRow(`SELECT COALESCE(MAX(revision),0) FROM managed_provisioning_requests
					WHERE tenant_id=? AND bundle_id=? AND status='applied'`, body.TenantID, bundleID).Scan(&latest)
				if latest > revision {
					return nil, false, fmt.Errorf("%w: bundle revision %d is older than applied revision %d", errManagedProvisioningConflict, revision, latest)
				}
			}
			result, err := s.store.db.Exec(`UPDATE managed_provisioning_requests SET status='applying',last_error='',updated_at=CURRENT_TIMESTAMP WHERE request_id=? AND status=?`, body.RequestID, status)
			if err != nil {
				return nil, false, err
			}
			n, _ := result.RowsAffected()
			if n != 1 {
				return nil, false, fmt.Errorf("%w: request already claimed", errManagedProvisioningConflict)
			}
			return nil, true, nil
		}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	if bundleID != "" {
		var previousID, previousHash, previousStatus, previousResponse string
		err = s.store.db.QueryRow(`SELECT request_id,request_hash,status,response_json FROM managed_provisioning_requests
			WHERE tenant_id=? AND bundle_id=? AND revision=?`, body.TenantID, bundleID, revision).
			Scan(&previousID, &previousHash, &previousStatus, &previousResponse)
		if err == nil {
			if previousHash != requestHash {
				return nil, false, fmt.Errorf("%w: bundle revision was already used with different content", errManagedProvisioningConflict)
			}
			if previousStatus == "applied" {
				var prior sdk.ManagedProvisioningApplyResult
				if json.Unmarshal([]byte(previousResponse), &prior) != nil {
					return nil, false, errors.New("stored provisioning response is invalid")
				}
				return &prior, false, nil
			}
			return nil, false, fmt.Errorf("%w: retry bundle revision with its original request_id %q", errManagedProvisioningConflict, previousID)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, false, err
		}
		var latest int64
		_ = s.store.db.QueryRow(`SELECT COALESCE(MAX(revision),0) FROM managed_provisioning_requests
			WHERE tenant_id=? AND bundle_id=? AND status='applied'`, body.TenantID, bundleID).Scan(&latest)
		if latest > revision {
			return nil, false, fmt.Errorf("%w: bundle revision %d is older than applied revision %d", errManagedProvisioningConflict, revision, latest)
		}
	}
	_, err = s.store.db.Exec(`INSERT INTO managed_provisioning_requests
		(request_id,tenant_id,bundle_id,revision,request_hash,status) VALUES(?,?,?,?,?,'applying')`,
		body.RequestID, body.TenantID, bundleID, revision, requestHash)
	if err != nil {
		return nil, false, err
	}
	return nil, true, nil
}

func (s *Server) failManagedProvisioning(requestID string, applyErr error) {
	_, _ = s.store.db.Exec(`UPDATE managed_provisioning_requests SET status='error',last_error=?,updated_at=CURRENT_TIMESTAMP WHERE request_id=?`, truncate(applyErr.Error(), 2000), requestID)
}

func (s *Server) completeManagedProvisioning(requestID string, result sdk.ManagedProvisioningApplyResult) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = s.store.db.Exec(`UPDATE managed_provisioning_requests SET status='applied',response_json=?,last_error='',updated_at=CURRENT_TIMESTAMP WHERE request_id=?`, string(raw), requestID)
	return err
}
