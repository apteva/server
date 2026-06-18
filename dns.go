package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

const (
	dnsPermRead  = "platform.dns.read"
	dnsPermWrite = "platform.dns.write"
)

type delegatedDNSConfig struct {
	FleetURL string
	Token    string
	TenantID string
	Project  string
}

func currentDelegatedDNSConfig() delegatedDNSConfig {
	return delegatedDNSConfig{
		FleetURL: strings.TrimRight(strings.TrimSpace(os.Getenv("APTEVA_DELEGATED_DNS_FLEET_URL")), "/"),
		Token:    strings.TrimSpace(os.Getenv("APTEVA_DELEGATED_DNS_TOKEN")),
		TenantID: strings.TrimSpace(os.Getenv("APTEVA_DELEGATED_DNS_TENANT_ID")),
		Project:  strings.TrimSpace(os.Getenv("APTEVA_DELEGATED_DNS_PROJECT_ID")),
	}
}

func (c delegatedDNSConfig) enabled() bool {
	return c.FleetURL != "" && c.Token != "" && c.TenantID != ""
}

func (s *Server) ListPlatformDNSGrants() ([]sdk.DomainGrant, error) {
	cfg := currentDelegatedDNSConfig()
	if !cfg.enabled() {
		return []sdk.DomainGrant{}, nil
	}
	args := map[string]any{"tenant_id": cfg.TenantID}
	if cfg.Project != "" {
		args["_project_id"] = cfg.Project
	}
	var out struct {
		Grants []struct {
			ID       int64  `json:"id"`
			Domain   string `json:"domain"`
			Wildcard bool   `json:"wildcard"`
			Status   string `json:"status"`
		} `json:"grants"`
	}
	if err := callDelegatedFleetTool(cfg, "tenant_domain_list", args, &out); err != nil {
		return nil, err
	}
	grants := make([]sdk.DomainGrant, 0, len(out.Grants))
	for _, g := range out.Grants {
		if strings.TrimSpace(g.Domain) == "" || g.Status == "revoked" {
			continue
		}
		status := g.Status
		if status == "" {
			status = "active"
		}
		grants = append(grants, sdk.DomainGrant{
			ID:          fmt.Sprintf("fleet:%d", g.ID),
			Domain:      normalizeDNSDomainLoose(g.Domain),
			Wildcard:    g.Wildcard,
			Status:      status,
			Source:      "delegated:fleet",
			Actions:     []string{"dns.record.upsert", "dns.record.delete"},
			RecordTypes: []string{"A", "AAAA", "CNAME", "TXT", "MX", "SRV", "CAA"},
		})
	}
	return grants, nil
}

func (s *Server) UpsertPlatformDNSRecord(req sdk.DNSRecordRequest) (*sdk.DNSRecordResult, error) {
	clean, err := normalizeDNSRecordRequest(req, true)
	if err != nil {
		return nil, err
	}
	if err := s.validateDNSGrant(clean); err != nil {
		return nil, err
	}
	cfg := currentDelegatedDNSConfig()
	if !cfg.enabled() {
		return nil, errors.New("no delegated DNS backend configured")
	}
	args := delegatedDNSArgs(cfg, clean)
	var out sdk.DNSRecordResult
	if err := callDelegatedFleetTool(cfg, "tenant_domain_record_set", args, &out); err != nil {
		return nil, err
	}
	if out.Domain == "" {
		out.Domain = clean.Domain
	}
	if out.Name == "" {
		out.Name = clean.Name
	}
	if out.Type == "" {
		out.Type = clean.Type
	}
	out.OK = out.OK || out.Error == ""
	out.Backend = "delegated:fleet"
	if out.Action == "" {
		out.Action = "upserted"
	}
	return &out, nil
}

func (s *Server) DeletePlatformDNSRecord(req sdk.DNSRecordRequest) (*sdk.DNSRecordResult, error) {
	clean, err := normalizeDNSRecordRequest(req, false)
	if err != nil {
		return nil, err
	}
	if err := s.validateDNSGrant(clean); err != nil {
		return nil, err
	}
	cfg := currentDelegatedDNSConfig()
	if !cfg.enabled() {
		return nil, errors.New("no delegated DNS backend configured")
	}
	args := delegatedDNSArgs(cfg, clean)
	var out sdk.DNSRecordResult
	if err := callDelegatedFleetTool(cfg, "tenant_domain_record_delete", args, &out); err != nil {
		return nil, err
	}
	if out.Domain == "" {
		out.Domain = clean.Domain
	}
	if out.Name == "" {
		out.Name = clean.Name
	}
	if out.Type == "" {
		out.Type = clean.Type
	}
	out.OK = out.OK || out.Error == ""
	out.Backend = "delegated:fleet"
	if out.Action == "" {
		out.Action = "deleted"
	}
	return &out, nil
}

func delegatedDNSArgs(cfg delegatedDNSConfig, req sdk.DNSRecordRequest) map[string]any {
	args := map[string]any{
		"tenant_id": cfg.TenantID,
		"domain":    req.Domain,
		"name":      req.Name,
		"type":      req.Type,
	}
	if req.Value != "" {
		args["value"] = req.Value
	}
	if req.TTL > 0 {
		args["ttl"] = req.TTL
	}
	if cfg.Project != "" {
		args["_project_id"] = cfg.Project
	}
	return args
}

func (s *Server) validateDNSGrant(req sdk.DNSRecordRequest) error {
	grants, err := s.ListPlatformDNSGrants()
	if err != nil {
		return err
	}
	fqdn := composeDNSFQDN(req.Domain, req.Name)
	for _, g := range grants {
		if strings.EqualFold(g.Status, "revoked") || strings.EqualFold(g.Status, "disabled") {
			continue
		}
		if dnsGrantCovers(g.Domain, fqdn, g.Wildcard) {
			return nil
		}
	}
	return fmt.Errorf("no DNS grant covers %s", fqdn)
}

func normalizeDNSRecordRequest(req sdk.DNSRecordRequest, requireValue bool) (sdk.DNSRecordRequest, error) {
	domain := normalizeDNSDomainLoose(req.Domain)
	if domain == "" {
		return req, errors.New("domain required")
	}
	if err := validateDNSName(domain); err != nil {
		return req, fmt.Errorf("domain: %w", err)
	}
	name := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(req.Name, ".")))
	if name == "" {
		name = "@"
	}
	if strings.ContainsAny(name, " \t\r\n/?:#") {
		return req, fmt.Errorf("invalid record name %q", req.Name)
	}
	fqdn := composeDNSFQDN(domain, name)
	if err := validateDNSName(fqdn); err != nil {
		return req, fmt.Errorf("record name: %w", err)
	}
	rtype := strings.ToUpper(strings.TrimSpace(req.Type))
	if !allowedDNSRecordType(rtype) {
		return req, fmt.Errorf("unsupported DNS record type %q", req.Type)
	}
	value := strings.TrimSpace(req.Value)
	if requireValue && value == "" {
		return req, errors.New("value required")
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = 600
	}
	if ttl < 60 {
		ttl = 60
	}
	req.Domain = domain
	req.Name = name
	req.Type = rtype
	req.Value = value
	req.TTL = ttl
	return req, nil
}

func normalizeDNSDomainLoose(s string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(strings.TrimSuffix(s, "."))), "*.")
}

func validateDNSName(name string) error {
	if name == "" || !strings.Contains(name, ".") {
		return errors.New("must be a fully-qualified DNS name")
	}
	if net.ParseIP(name) != nil {
		return errors.New("must be a DNS name, not an IP literal")
	}
	labels := strings.Split(name, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return fmt.Errorf("invalid label %q", label)
		}
		for _, r := range label {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
				return fmt.Errorf("invalid character %q", r)
			}
		}
	}
	return nil
}

func allowedDNSRecordType(t string) bool {
	switch t {
	case "A", "AAAA", "CNAME", "TXT", "MX", "SRV", "CAA":
		return true
	default:
		return false
	}
}

func composeDNSFQDN(domain, name string) string {
	domain = normalizeDNSDomainLoose(domain)
	name = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(name, ".")))
	if name == "" || name == "@" {
		return domain
	}
	return name + "." + domain
}

func dnsGrantCovers(grantDomain, fqdn string, wildcard bool) bool {
	grantDomain = normalizeDNSDomainLoose(grantDomain)
	fqdn = normalizeDNSDomainLoose(fqdn)
	return fqdn == grantDomain || (wildcard && strings.HasSuffix(fqdn, "."+grantDomain))
}

func callDelegatedFleetTool(cfg delegatedDNSConfig, tool string, args map[string]any, out any) error {
	u, err := url.Parse(cfg.FleetURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("invalid delegated DNS fleet URL %q", cfg.FleetURL)
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      tool,
			"arguments": args,
		},
	})
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(cfg.FleetURL, "/")+"/mcp", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("delegated DNS %s: %w", tool, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("delegated DNS %s: http %d: %s", tool, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var env struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("delegated DNS %s: decode envelope: %w", tool, err)
	}
	if env.Error != nil {
		return fmt.Errorf("delegated DNS %s: %s", tool, env.Error.Message)
	}
	if out == nil || len(env.Result.Content) == 0 || env.Result.Content[0].Text == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(env.Result.Content[0].Text), out); err != nil {
		return fmt.Errorf("delegated DNS %s: decode result: %w", tool, err)
	}
	return nil
}
