package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

type runtimeResourceRecord struct {
	Revision int64              `json:"revision"`
	Policy   sdk.ResourcePolicy `json:"policy"`
}

func (m *appRuntimeManager) resourcePolicy(id int64) (runtimeResourceRecord, error) {
	var r runtimeResourceRecord
	var raw string
	e := m.s.store.db.QueryRow(`SELECT revision,policy_json FROM app_runtime_resource_policies WHERE install_id=?`, id).Scan(&r.Revision, &raw)
	if errors.Is(e, sql.ErrNoRows) {
		return r, nil
	}
	if e != nil {
		return r, e
	}
	e = json.Unmarshal([]byte(raw), &r.Policy)
	return r, e
}
func (m *appRuntimeManager) resourceManifest(id int64) (sdk.Manifest, error) {
	var raw string
	var manifest sdk.Manifest
	e := m.s.store.db.QueryRow(`SELECT COALESCE(NULLIF(i.manifest_json,''),a.manifest_json) FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE i.id=?`, id).Scan(&raw)
	if e == nil {
		e = json.Unmarshal([]byte(raw), &manifest)
	}
	return manifest, e
}
func (s *Server) handleRuntimeResources(w http.ResponseWriter, r *http.Request, id int64) {
	if _, ok := s.requirePlatformAdmin(w, r); !ok {
		return
	}
	m := s.appRuntimeManager()
	manifest, e := m.resourceManifest(id)
	if e != nil {
		http.Error(w, "installation unavailable", 404)
		return
	}
	current, e := m.resourcePolicy(id)
	if e != nil {
		http.Error(w, "resource policy unavailable", 503)
		return
	}
	if r.Method == http.MethodPut {
		var next runtimeResourceRecord
		if runtimeDecode(r, &next) != nil {
			http.Error(w, "invalid policy", 400)
			return
		}
		resolved, resolveErr := sdk.ResolveResources(manifest.Runtime.ResourcePolicy, &next.Policy)
		e = resolveErr
		if e == nil {
			e = validateHostResourcePolicy(resolved)
		}
		if e != nil {
			http.Error(w, e.Error(), 400)
			return
		}
		if next.Revision != current.Revision {
			http.Error(w, "policy changed; reload before saving", 409)
			return
		}
		// Compare-and-swap also covers the initial insert. Saving never restarts apps.
		result, e := s.store.db.Exec(`INSERT INTO app_runtime_resource_policies(install_id,revision,policy_json) VALUES(?,1,?) ON CONFLICT(install_id) DO UPDATE SET revision=revision+1,policy_json=excluded.policy_json WHERE revision=?`, id, runtimeJSON(next.Policy), next.Revision)
		if e != nil {
			http.Error(w, "policy persistence failed", 500)
			return
		}
		n, e := result.RowsAffected()
		if e != nil || n != 1 {
			http.Error(w, "policy changed; reload", 409)
			return
		}
		current = runtimeResourceRecord{Revision: next.Revision + 1, Policy: next.Policy}
	} else if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	resolved, e := sdk.ResolveResources(manifest.Runtime.ResourcePolicy, &current.Policy)
	if e != nil {
		http.Error(w, e.Error(), 409)
		return
	}
	// Effective values are execution evidence, never copied from desired settings.
	writeJSON(w, map[string]any{"contract": sdk.ResourcePolicyVersion, "revision": current.Revision, "policy": current.Policy, "resolved": resolved, "declaration": manifest.Runtime.ResourcePolicy, "apply": "next_deployment", "capabilities": localResourceCapabilities(), "note": "Saved without restarting. Existing executions retain their policy; inspect their runtime resources for effective socket values."})
}
func runtimeResourceEnvelope(d runtimeDeployment) sdk.ResourceEnvelope {
	return sdk.ResourceEnvelope{InstallID: strconv.FormatInt(d.InstallID, 10), DeploymentID: d.ID, Generation: strconv.FormatInt(d.Generation, 10), Revision: d.ResourceRevision, Policy: d.Resources}
}

// Live cooperative updates are explicit and fenced to the active execution.
// Process settings remain the backend's responsibility and are never claimed
// applied by an SDK acknowledgement.
func (s *Server) handleRuntimeResourceExecution(w http.ResponseWriter, r *http.Request, id int64) {
	if _, ok := s.requirePlatformAdmin(w, r); !ok {
		return
	}
	m := s.appRuntimeManager()
	d, e := m.active(id)
	if e != nil {
		http.Error(w, "active managed execution unavailable", 409)
		return
	}
	if e = runtimeSecureURL(d.Execution.Endpoint); e != nil {
		http.Error(w, "execution endpoint unavailable", 503)
		return
	}
	method := http.MethodGet
	var body io.Reader
	if r.Method == http.MethodPost {
		var input struct {
			Revision     int64  `json:"revision"`
			DeploymentID string `json:"deployment_id"`
		}
		if runtimeDecode(r, &input) != nil || input.DeploymentID != d.ID {
			http.Error(w, "execution changed; reload", 409)
			return
		}
		saved, e := m.resourcePolicy(id)
		if e != nil {
			http.Error(w, "policy unavailable", 503)
			return
		}
		if saved.Revision != input.Revision {
			http.Error(w, "policy changed; reload", 409)
			return
		}
		resolved, e := sdk.ResolveResources(d.Manifest.Runtime.ResourcePolicy, &saved.Policy, d.Placement.Resources)
		if e != nil {
			http.Error(w, e.Error(), 409)
			return
		}
		// Keep process policy unchanged in the execution envelope; its pending diff
		// remains visible in the saved installation policy.
		if e = validateHostResourcePolicy(resolved); e != nil {
			http.Error(w, e.Error(), 400)
			return
		}
		resolved.Process = d.Resources.Process
		env := runtimeResourceEnvelope(d)
		env.Policy = resolved
		env.Revision = saved.Revision
		body = strings.NewReader(runtimeJSON(env))
		method = http.MethodPut
	} else if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	token, e := m.token(d)
	if e != nil {
		http.Error(w, "execution credential unavailable", 503)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, method, runtimeEndpoint(d.Execution.Endpoint, "/_apteva/runtime/app/resources"), body)
	if e != nil {
		http.Error(w, "invalid execution", 503)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, e := m.http.Do(req)
	if e != nil {
		http.Error(w, "execution resource control unavailable", 503)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		http.Error(w, "execution did not accept resource request", response.StatusCode)
		return
	}
	var status json.RawMessage
	if e = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&status); e != nil {
		http.Error(w, "invalid runtime resource status", 502)
		return
	}
	writeJSON(w, map[string]any{"deployment_id": d.ID, "generation": d.Generation, "runtime": status, "process_apply": "next_deployment", "process_observed": d.Execution.ProcessResources})
}

// Host-wide ceilings are set by the Server operator, never by an app/browser.
func validateHostResourcePolicy(p sdk.ResourcePolicy) error {
	raw := os.Getenv("APTEVA_RESOURCE_CEILINGS")
	if raw == "" {
		return nil
	}
	var c sdk.ResourceCeilings
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&c) != nil || dec.Decode(new(any)) != io.EOF {
		return fmt.Errorf("invalid host resource ceilings")
	}
	return c.Check(p)
}
