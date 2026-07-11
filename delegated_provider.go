package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"
)

const delegatedProviderMarker = "_apteva_delegated_provider"

type delegatedProviderCredentials struct {
	GrantID              string
	ControllerExecuteURL string
	ControllerGatewayURL string
	ControllerToken      string
	ControllerInstallID  string
	ParentConnectionID   int64
	Resource             string
	AllowedTools         []string
	AllowedDomains       []string
	AllowedFrom          []string
	Metadata             map[string]string
}

type delegatedUsageEvent struct {
	ProjectID          string
	CallerAppName      string
	GrantID            string
	ConnectionID       int64
	ParentConnectionID int64
	ChildInstallID     int64
	AppSlug            string
	Tool               string
	Resource           string
	Quantity           int
	Unit               string
	Status             string
	Error              string
	Direction          string
}

func parseDelegatedProviderCredentials(plain string) (*delegatedProviderCredentials, bool, error) {
	var raw map[string]string
	if err := json.Unmarshal([]byte(plain), &raw); err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(raw[delegatedProviderMarker]) == "" {
		return nil, false, nil
	}
	parentID, err := strconv.ParseInt(strings.TrimSpace(raw["parent_connection_id"]), 10, 64)
	if err != nil || parentID <= 0 {
		return nil, true, errors.New("delegated provider credentials missing parent_connection_id")
	}
	g := &delegatedProviderCredentials{
		GrantID:              strings.TrimSpace(raw["grant_id"]),
		ControllerExecuteURL: strings.TrimSpace(raw["controller_execute_url"]),
		ControllerGatewayURL: strings.TrimRight(strings.TrimSpace(raw["controller_gateway_url"]), "/"),
		ControllerToken:      strings.TrimSpace(raw["controller_token"]),
		ControllerInstallID:  strings.TrimSpace(raw["controller_install_id"]),
		ParentConnectionID:   parentID,
		Resource:             strings.TrimSpace(raw["resource"]),
		AllowedTools:         parseGrantList(raw["allowed_tools"]),
		AllowedDomains:       normaliseDomainList(parseGrantList(raw["allowed_domains"])),
		AllowedFrom:          parseGrantList(firstNonEmptyDelegated(raw["allowed_from"], raw["allowed_from_addresses"])),
		Metadata:             raw,
	}
	if g.Resource == "" {
		g.Resource = "provider.connection"
	}
	if g.GrantID == "" {
		g.GrantID = fmt.Sprintf("provider:%d", g.ParentConnectionID)
	}
	if g.ControllerToken == "" || (g.ControllerExecuteURL == "" && (g.ControllerGatewayURL == "" || g.ControllerInstallID == "")) {
		return nil, true, errors.New("delegated provider credentials missing controller endpoint")
	}
	return g, true, nil
}

func isDelegatedProviderCredentialsMap(creds map[string]string) bool {
	return strings.TrimSpace(creds[delegatedProviderMarker]) != ""
}

func (s *Server) executeDelegatedProviderTool(installID, connID int64, conn *Connection, grant *delegatedProviderCredentials, toolName string, input map[string]any) (*ExecuteResult, error) {
	if grant == nil {
		return nil, errors.New("delegated provider grant required")
	}
	if err := grant.validate(conn.AppSlug, toolName, input); err != nil {
		s.recordDelegatedProviderUsage(delegatedUsageEvent{
			GrantID:            grant.GrantID,
			ConnectionID:       connID,
			ParentConnectionID: grant.ParentConnectionID,
			ChildInstallID:     installID,
			AppSlug:            conn.AppSlug,
			Tool:               toolName,
			Resource:           grant.Resource,
			Quantity:           usageQuantity(toolName, input),
			Status:             "denied",
			Error:              err.Error(),
			Direction:          "child",
		})
		return nil, err
	}

	payload, _ := json.Marshal(map[string]any{"tool": toolName, "input": input})
	url := delegatedProviderExecuteURL(grant)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+grant.ControllerToken)
	if grant.ControllerInstallID != "" {
		req.Header.Set("X-Apteva-App-Install-ID", grant.ControllerInstallID)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Apteva-Delegated-Grant-ID", grant.GrantID)
	req.Header.Set("X-Apteva-Delegated-Connection-ID", strconv.FormatInt(connID, 10))
	req.Header.Set("X-Apteva-Delegated-Parent-Connection-ID", strconv.FormatInt(grant.ParentConnectionID, 10))
	req.Header.Set("X-Apteva-Delegated-Child-Install-ID", strconv.FormatInt(installID, 10))
	req.Header.Set("X-Apteva-Delegated-Resource", grant.Resource)
	req.Header.Set("X-Apteva-Delegated-App-Slug", conn.AppSlug)
	req.Header.Set("X-Apteva-Delegated-Project-ID", conn.ProjectID)
	if caller := s.callerAppName(installID); caller != "" {
		req.Header.Set("X-Apteva-Delegated-Caller-App", caller)
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		s.recordDelegatedProviderUsage(delegatedUsageEvent{
			GrantID: grant.GrantID, ConnectionID: connID, ParentConnectionID: grant.ParentConnectionID,
			ChildInstallID: installID, AppSlug: conn.AppSlug, Tool: toolName, Resource: grant.Resource,
			Quantity: usageQuantity(toolName, input), Status: "error", Error: err.Error(), Direction: "child",
		})
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	if resp.StatusCode >= 300 {
		err := fmt.Errorf("delegated provider controller returned %d: %s", resp.StatusCode, truncate(string(raw), 500))
		s.recordDelegatedProviderUsage(delegatedUsageEvent{
			GrantID: grant.GrantID, ConnectionID: connID, ParentConnectionID: grant.ParentConnectionID,
			ChildInstallID: installID, AppSlug: conn.AppSlug, Tool: toolName, Resource: grant.Resource,
			Quantity: usageQuantity(toolName, input), Status: "error", Error: err.Error(), Direction: "child",
		})
		return nil, err
	}

	var result struct {
		Success bool              `json:"success"`
		Status  int               `json:"status"`
		Data    json.RawMessage   `json:"data"`
		Headers map[string]string `json:"headers,omitempty"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode delegated provider response: %w", err)
	}
	status := "success"
	errText := ""
	if !result.Success || result.Status >= 400 {
		status = "error"
		errText = truncate(string(result.Data), 500)
	}
	qty, unit, _ := integrationUsageMetric(conn, toolName, input, &ExecuteResult{Success: result.Success, Status: result.Status, Data: result.Data, Headers: result.Headers})
	s.recordDelegatedProviderUsage(delegatedUsageEvent{
		GrantID: grant.GrantID, ConnectionID: connID, ParentConnectionID: grant.ParentConnectionID,
		ChildInstallID: installID, AppSlug: conn.AppSlug, Tool: toolName, Resource: grant.Resource,
		Quantity: qty, Unit: unit, Status: status, Error: errText, Direction: "child",
	})
	return &ExecuteResult{Success: result.Success, Status: result.Status, Data: result.Data, Headers: result.Headers}, nil
}

func delegatedProviderExecuteURL(grant *delegatedProviderCredentials) string {
	if grant != nil && grant.ControllerExecuteURL != "" {
		return grant.ControllerExecuteURL
	}
	if grant == nil {
		return ""
	}
	return fmt.Sprintf("%s/api/apps/callback/integrations/%d/execute", grant.ControllerGatewayURL, grant.ParentConnectionID)
}

func (g *delegatedProviderCredentials) validate(appSlug, tool string, input map[string]any) error {
	if len(g.AllowedTools) > 0 && !listContainsFold(g.AllowedTools, tool) {
		return fmt.Errorf("tool %q is not allowed by grant %s", tool, g.GrantID)
	}
	if appSlug == "aws-ses" {
		return g.validateSES(input)
	}
	return nil
}

func (g *delegatedProviderCredentials) validateSES(input map[string]any) error {
	if len(g.AllowedDomains) > 0 {
		for _, identity := range collectIdentityValues(input) {
			if !identityCoveredByDomains(identity, g.AllowedDomains) {
				return fmt.Errorf("identity %q is outside delegated domains", identity)
			}
		}
	}
	froms := collectFromValues(input)
	if len(g.AllowedFrom) > 0 {
		for _, from := range froms {
			if !fromCoveredByPatterns(from, g.AllowedFrom) {
				return fmt.Errorf("from address %q is outside delegated grant", from)
			}
		}
	} else if len(g.AllowedDomains) > 0 {
		for _, from := range froms {
			if !identityCoveredByDomains(from, g.AllowedDomains) {
				return fmt.Errorf("from address %q is outside delegated domains", from)
			}
		}
	}
	return nil
}

func collectIdentityValues(input map[string]any) []string {
	var out []string
	for _, key := range []string{"EmailIdentity", "Identity", "MailFromDomain"} {
		if v := delegatedStringValue(input[key]); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func collectFromValues(input map[string]any) []string {
	var out []string
	for _, key := range []string{"FromEmailAddress", "From", "Source", "from"} {
		if v := delegatedStringValue(input[key]); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func identityCoveredByDomains(value string, domains []string) bool {
	domain := strings.ToLower(strings.TrimSpace(extractEmailDomain(value)))
	if domain == "" {
		domain = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(value, ".")))
	}
	for _, allowed := range domains {
		if domain == allowed || strings.HasSuffix(domain, "."+allowed) {
			return true
		}
	}
	return false
}

func fromCoveredByPatterns(value string, patterns []string) bool {
	addr := strings.ToLower(strings.TrimSpace(extractEmailAddress(value)))
	for _, pattern := range patterns {
		p := strings.ToLower(strings.TrimSpace(pattern))
		switch {
		case p == "":
			continue
		case p == "*":
			return true
		case strings.HasPrefix(p, "*@"):
			if strings.HasSuffix(addr, p[1:]) {
				return true
			}
		case strings.HasPrefix(p, "@"):
			if strings.HasSuffix(addr, p) {
				return true
			}
		case addr == p:
			return true
		}
	}
	return false
}

func extractEmailAddress(value string) string {
	value = strings.TrimSpace(value)
	if parsed, err := mail.ParseAddress(value); err == nil && parsed.Address != "" {
		return parsed.Address
	}
	if i := strings.LastIndex(value, "<"); i >= 0 && strings.HasSuffix(value, ">") {
		return strings.TrimSpace(strings.TrimSuffix(value[i+1:], ">"))
	}
	return value
}

func extractEmailDomain(value string) string {
	addr := extractEmailAddress(value)
	if i := strings.LastIndex(addr, "@"); i >= 0 && i < len(addr)-1 {
		return strings.TrimSuffix(addr[i+1:], ".")
	}
	return strings.TrimSuffix(addr, ".")
}

func parseGrantList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var arr []string
	if strings.HasPrefix(raw, "[") && json.Unmarshal([]byte(raw), &arr) == nil {
		return compactStrings(arr)
	}
	return compactStrings(strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\t'
	}))
}

func normaliseDomainList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(v, "."), "*.")))
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func compactStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func listContainsFold(values []string, needle string) bool {
	for _, v := range values {
		if strings.EqualFold(v, needle) {
			return true
		}
	}
	return false
}

func delegatedStringValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case fmt.Stringer:
		return x.String()
	default:
		return ""
	}
}

func firstNonEmptyDelegated(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func usageQuantity(tool string, input map[string]any) int {
	if strings.EqualFold(tool, "send_bulk_email") {
		if list, ok := input["BulkEmailEntries"].([]any); ok && len(list) > 0 {
			return len(list)
		}
	}
	if strings.EqualFold(tool, "send_email") {
		if dest, ok := input["Destination"].(map[string]any); ok {
			n := lenAnyList(dest["ToAddresses"]) + lenAnyList(dest["CcAddresses"]) + lenAnyList(dest["BccAddresses"])
			if n > 0 {
				return n
			}
		}
	}
	return 1
}

func lenAnyList(v any) int {
	switch list := v.(type) {
	case []any:
		return len(list)
	case []string:
		return len(list)
	default:
		return 0
	}
}

func (s *Server) recordDelegatedProviderUsage(ev delegatedUsageEvent) {
	if s == nil || s.store == nil || s.store.db == nil {
		return
	}
	if ev.Quantity <= 0 {
		ev.Quantity = 1
	}
	if ev.Unit == "" {
		ev.Unit = usageUnit(ev.AppSlug, ev.Tool)
	}
	if ev.Resource == "" {
		ev.Resource = "provider.connection"
	}
	if ev.ProjectID == "" {
		switch ev.Direction {
		case "controller":
			ev.ProjectID = s.projectForConnection(ev.ParentConnectionID)
		default:
			ev.ProjectID = s.projectForConnection(ev.ConnectionID)
		}
	}
	if ev.CallerAppName == "" && ev.ChildInstallID > 0 {
		ev.CallerAppName = s.callerAppName(ev.ChildInstallID)
	}
	_, _ = s.store.db.Exec(`
		INSERT INTO delegated_provider_usage
			(grant_id, connection_id, parent_connection_id, child_install_id, app_slug, tool, resource, quantity, status, error, direction)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, ev.GrantID, ev.ConnectionID, ev.ParentConnectionID, ev.ChildInstallID, ev.AppSlug, ev.Tool, ev.Resource, ev.Quantity, ev.Status, truncate(ev.Error, 1000), ev.Direction)
	connID := ev.ConnectionID
	childConnID := ev.ConnectionID
	if ev.Direction == "controller" && ev.ParentConnectionID > 0 {
		connID = ev.ParentConnectionID
	}
	s.recordIntegrationUsage(integrationUsageEvent{
		ProjectID:          ev.ProjectID,
		CallerInstallID:    ev.ChildInstallID,
		CallerAppName:      ev.CallerAppName,
		ConnectionID:       connID,
		ParentConnectionID: ev.ParentConnectionID,
		AppSlug:            ev.AppSlug,
		Tool:               ev.Tool,
		GrantID:            ev.GrantID,
		GrantResource:      ev.Resource,
		ChildInstallID:     ev.ChildInstallID,
		ChildConnectionID:  childConnID,
		Direction:          ev.Direction,
		Quantity:           ev.Quantity,
		Unit:               ev.Unit,
		Status:             ev.Status,
		Error:              ev.Error,
	})
}

func delegatedUsageFromHeaders(r *http.Request, connID int64, conn *Connection, tool string, input map[string]any, status, errText string) (delegatedUsageEvent, bool) {
	grantID := strings.TrimSpace(r.Header.Get("X-Apteva-Delegated-Grant-ID"))
	if grantID == "" {
		return delegatedUsageEvent{}, false
	}
	parentID, _ := strconv.ParseInt(r.Header.Get("X-Apteva-Delegated-Parent-Connection-ID"), 10, 64)
	childInstallID, _ := strconv.ParseInt(r.Header.Get("X-Apteva-Delegated-Child-Install-ID"), 10, 64)
	childConnID, _ := strconv.ParseInt(r.Header.Get("X-Apteva-Delegated-Connection-ID"), 10, 64)
	if parentID == 0 {
		parentID = connID
	}
	appSlug := r.Header.Get("X-Apteva-Delegated-App-Slug")
	if appSlug == "" && conn != nil {
		appSlug = conn.AppSlug
	}
	resource := r.Header.Get("X-Apteva-Delegated-Resource")
	if resource == "" {
		resource = "provider.connection"
	}
	return delegatedUsageEvent{
		ProjectID: r.Header.Get("X-Apteva-Delegated-Project-ID"), CallerAppName: r.Header.Get("X-Apteva-Delegated-Caller-App"),
		GrantID: grantID, ConnectionID: childConnID, ParentConnectionID: parentID, ChildInstallID: childInstallID,
		AppSlug: appSlug, Tool: tool, Resource: resource, Quantity: usageQuantity(tool, input), Status: status,
		Error: errText, Direction: "controller",
	}, true
}
