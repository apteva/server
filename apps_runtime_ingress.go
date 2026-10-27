package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

type runtimeIngressSpec struct {
	Provider     *runtimeIngressProviderRef `json:"provider,omitempty"`
	Hostname     string                     `json:"hostname"`
	DNSInstallID int64                      `json:"dns_app_install_id,omitempty"`
	Domain       string                     `json:"domain,omitempty"`
	DNSProjectID string                     `json:"dns_project_id,omitempty"`
}
type runtimeIngressState struct {
	Spec     *runtimeIngressSpec `json:"spec"`
	RecordID string              `json:"record_id,omitempty"`
	IPv4     string              `json:"ipv4,omitempty"`
	OwnDNS   bool                `json:"owns_dns"`
}

func validateRuntimeIngressSpec(v *runtimeIngressSpec) error {
	if v == nil {
		return nil
	}
	if !regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])$`).MatchString(v.Hostname) || len(v.Hostname) > 253 || !strings.Contains(v.Hostname, ".") || strings.Contains(v.Hostname, "..") {
		return fmt.Errorf("invalid ingress hostname")
	}
	if v.Domain != "" && v.Hostname != v.Domain && !strings.HasSuffix(v.Hostname, "."+v.Domain) {
		return fmt.Errorf("hostname does not belong to DNS domain")
	}
	return nil
}
func (m *appRuntimeManager) dnsTool(ctx context.Context, v *runtimeIngressSpec, name string, args map[string]any) (map[string]any, error) {
	var base string
	if e := m.s.store.db.QueryRow(`SELECT sidecar_url_override FROM app_installs WHERE id=? AND status='running'`, v.DNSInstallID).Scan(&base); e != nil {
		return nil, fmt.Errorf("DNS provider installation unavailable")
	}
	if e := runtimeSecureURL(base); e != nil {
		return nil, e
	}
	token, e := m.s.appInstallToken(v.DNSInstallID)
	if e != nil {
		return nil, e
	}
	args["_project_id"] = v.DNSProjectID
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "runtime-dns", "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/mcp", bytes.NewReader(body))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, e := m.http.Do(req)
	if e != nil {
		return nil, fmt.Errorf("DNS provider unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("DNS provider rejected %s (%d)", name, resp.StatusCode)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if e = json.NewDecoder(io.LimitReader(resp.Body, runtimeResponseLimit)).Decode(&envelope); e != nil {
		return nil, fmt.Errorf("invalid DNS provider response")
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" || len(envelope.Result) == 0 {
		return nil, fmt.Errorf("DNS provider tool %s failed", name)
	}
	value, e := unwrapMCPToolResult(envelope.Result)
	if e != nil {
		return nil, fmt.Errorf("DNS provider tool %s failed", name)
	}
	out, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid DNS provider tool result")
	}
	// A malformed inventory must never look like an empty zone and authorize
	// creating a record over an existing hostname.
	if name == "domain_records_list" {
		if _, ok := out["records"].([]any); !ok {
			return nil, fmt.Errorf("DNS provider returned no record inventory")
		}
	}
	return out, nil
}
func (m *appRuntimeManager) prepareIngress(d *runtimeDeployment, b runtimeBackend) error {
	v := d.Placement.Ingress
	if v == nil {
		return nil
	}

	if v.Provider == nil {
		hint := b.Describe().IngressProvider
		if hint == nil || !runtimeID(hint.ID) {
			return fmt.Errorf("select an independent ingress provider")
		}
		v.Provider = &runtimeIngressProviderRef{InstallID: d.Placement.Backend.InstallID, ID: hint.ID, Target: d.Placement.Target, Options: map[string]any{}}
	}

	if e := sdk.ValidateRuntimeIngress(d.Manifest.Runtime.Ingress); e != nil {
		return e
	}
	if d.Manifest.Runtime.Ingress == nil {
		return fmt.Errorf("release declares no public ingress surfaces")
	}
	// The reservation is durable before any provider operation. It survives
	// restarts and does not allow another installation to claim this hostname.
	if _, e := m.s.store.db.Exec(`INSERT INTO app_runtime_ingress(install_id,hostname,record_json) VALUES(?,?,?) ON CONFLICT(install_id) DO NOTHING`, d.InstallID, v.Hostname, runtimeJSON(v)); e != nil {
		return fmt.Errorf("hostname is reserved by another installation")
	}
	var saved string
	var state runtimeIngressState
	_ = m.s.store.db.QueryRow(`SELECT record_json FROM app_runtime_ingress WHERE install_id=?`, d.InstallID).Scan(&saved)
	_ = json.Unmarshal([]byte(saved), &state)
	state.Spec = v
	var owned string
	if e := m.s.store.db.QueryRow(`SELECT hostname FROM app_runtime_ingress WHERE install_id=?`, d.InstallID).Scan(&owned); e != nil || owned != v.Hostname {
		return fmt.Errorf("installation already owns a different hostname")
	}
	if v.DNSInstallID == 0 {
		rows, e := m.s.store.db.Query(`SELECT id,manifest_json FROM app_installs WHERE status='running'`)
		if e != nil {
			return e
		}
		for rows.Next() {
			var id int64
			var raw string
			var manifest sdk.Manifest
			if rows.Scan(&id, &raw) != nil || json.Unmarshal([]byte(raw), &manifest) != nil {
				continue
			}
			found := 0
			for _, t := range manifest.Provides.MCPTools {
				if t.Name == "domain_records_list" || t.Name == "domain_records_set" {
					found++
				}
			}
			if found == 2 {
				if v.DNSInstallID != 0 {
					rows.Close()
					return fmt.Errorf("select a DNS provider installation")
				}
				v.DNSInstallID = id
			}
		}
		rows.Close()
		if v.DNSInstallID == 0 {
			return fmt.Errorf("no compatible DNS provider installed")
		}
	}
	if v.DNSProjectID == "" {
		if e := m.s.store.db.QueryRow(`SELECT COALESCE(project_id,'') FROM app_installs WHERE id=?`, d.InstallID).Scan(&v.DNSProjectID); e != nil {
			return e
		}
	}
	ctx, cancel := context.WithTimeout(m.ctx, 60*time.Second)
	defer cancel()
	var records map[string]any
	var e error
	if v.Domain != "" {
		records, e = m.dnsTool(ctx, v, "domain_records_list", map[string]any{"domain": v.Domain})
	} else {
		labels := strings.Split(v.Hostname, ".")
		for offset := 1; offset < len(labels)-1 && offset <= 8; offset++ {
			domain := strings.Join(labels[offset:], ".")
			records, e = m.dnsTool(ctx, v, "domain_records_list", map[string]any{"domain": domain})
			if e == nil {
				v.Domain = domain
				break
			}
		}
	}
	if e != nil || v.Domain == "" {
		return fmt.Errorf("unable to resolve managed DNS domain")
	}
	req, e := m.request(*d, runtimeOperation{ID: "ingress-target"}, "inspect-target")
	if e != nil {
		return e
	}
	// Target metadata is provider-owned; public address is inspected before DNS
	// writes, rather than derived from resource IDs or app-specific names.
	info, e := m.inspectTargetForIngress(*d, req)
	if e != nil {
		return e
	}
	if info == "" {
		return fmt.Errorf("backend did not return a public ingress address")
	}
	existing := []map[string]any{}
	if list, ok := records["records"].([]any); ok {
		for _, raw := range list {
			if rec, ok := raw.(map[string]any); ok && rec["name"] == v.Hostname {
				existing = append(existing, rec)
			}
		}
	}
	for _, rec := range existing {
		if rec["type"] == "A" && rec["value"] == info {
			continue
		}
		return fmt.Errorf("hostname has conflicting DNS records; existing records preserved")
	}
	if len(existing) == 0 {
		// Journal creation intent before the call. Reconciliation lists the same
		// name and verifies its target, so a lost response cannot create duplicates.
		if _, e = m.s.store.db.Exec(`UPDATE app_runtime_ingress SET record_json=? WHERE install_id=?`, runtimeJSON(state), d.InstallID); e != nil {
			return e
		}
		sub := strings.TrimSuffix(v.Hostname, "."+v.Domain)
		if v.Hostname == v.Domain {
			sub = "@"
		}
		if _, e = m.dnsTool(ctx, v, "domain_records_set", map[string]any{"domain": v.Domain, "name": sub, "type": "A", "value": info, "ttl": 600}); e != nil {
			return e
		}
		confirmed, err := m.dnsTool(ctx, v, "domain_records_list", map[string]any{"domain": v.Domain})
		if err != nil {
			return err
		}
		if list, ok := confirmed["records"].([]any); ok {
			for _, raw := range list {
				if rec, ok := raw.(map[string]any); ok && rec["name"] == v.Hostname && rec["type"] == "A" && rec["value"] == info {
					state.RecordID, _ = rec["id"].(string)
					state.OwnDNS = state.RecordID != ""
					state.IPv4 = info
					break
				}
			}
		}

	}
	if _, e = m.s.store.db.Exec(`UPDATE app_runtime_ingress SET record_json=? WHERE install_id=?`, runtimeJSON(state), d.InstallID); e != nil {
		return e
	}
	return m.prepareProviderIngress(d, b)
}
func (m *appRuntimeManager) inspectTargetForIngress(d runtimeDeployment, req runtimeBackendRequest) (string, error) {
	if d.Placement.Ingress == nil || d.Placement.Ingress.Provider == nil {
		return "", fmt.Errorf("ingress provider required")
	}
	ref := *d.Placement.Ingress.Provider
	provider, e := m.ingressProvider(ref, req.ProjectID)
	if e != nil {
		return "", e
	}
	options := map[string]any{}
	for key, value := range ref.Options {
		options[key] = value
	}
	options["hostname"] = d.Placement.Ingress.Hostname
	intent := runtimeIngressIntent{Contract: runtimeIngressContract, ControllerID: HashAPIKey("ingress-controller:" + string(m.s.secret)), OperationID: "ingress-target", InstallID: d.InstallID, DeploymentID: d.ID, Generation: d.Generation, ProjectID: req.ProjectID, Target: ref.Target, Hostname: d.Placement.Ingress.Hostname, Routes: d.Manifest.Runtime.Ingress.Routes, Options: options}
	var out struct {
		PublicIPv4 string `json:"public_ipv4"`
	}
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	defer cancel()
	if e = ingressProviderRequest(ctx, provider, "inspect-target", &intent, &out); e != nil {
		return "", e
	}
	if ip := net.ParseIP(out.PublicIPv4); ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() {
		return "", fmt.Errorf("ingress provider did not return a valid public IPv4 address")
	}
	return out.PublicIPv4, nil
}

func (m *appRuntimeManager) publishIngress(d runtimeDeployment, op runtimeOperation) error {
	if err := m.requirePublication(d, op); err != nil {
		return err
	}
	if d.Placement.Ingress == nil {
		return nil
	}
	if d.Placement.Ingress.Provider == nil {
		return fmt.Errorf("existing proxy must be explicitly migrated to an ingress provider")
	}
	return m.publishProviderIngress(d, op)
}

// Explicit deletion releases only the exact DNS record this controller created.
func (m *appRuntimeManager) cleanupIngress(id int64) error {
	if e := m.releaseProviderIngress(id); e != nil {
		return e
	}
	var raw string
	if e := m.s.store.db.QueryRow(`SELECT record_json FROM app_runtime_ingress WHERE install_id=?`, id).Scan(&raw); e != nil {
		return nil
	}
	var state runtimeIngressState
	if json.Unmarshal([]byte(raw), &state) != nil {
		return fmt.Errorf("invalid ingress ownership journal")
	}
	if state.OwnDNS && state.RecordID != "" && state.Spec != nil {
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		defer cancel()
		inventory, e := m.dnsTool(ctx, state.Spec, "domain_records_list", map[string]any{"domain": state.Spec.Domain})
		if e != nil {
			return e
		}
		if rows, ok := inventory["records"].([]any); ok {
			for _, raw := range rows {
				if record, ok := raw.(map[string]any); ok && record["id"] == state.RecordID {
					if record["name"] != state.Spec.Hostname || record["type"] != "A" || record["value"] != state.IPv4 {
						return fmt.Errorf("owned DNS record changed; preserved")
					}
					sub := strings.TrimSuffix(state.Spec.Hostname, "."+state.Spec.Domain)
					if state.Spec.Hostname == state.Spec.Domain {
						sub = "@"
					}
					if _, e = m.dnsTool(ctx, state.Spec, "domain_records_delete", map[string]any{"domain": state.Spec.Domain, "name": sub, "type": "A", "record_id": state.RecordID}); e != nil {
						return e
					}
				}
			}
		}
	}
	_, e := m.s.store.db.Exec(`DELETE FROM app_runtime_ingress WHERE install_id=?`, id)
	return e
}
