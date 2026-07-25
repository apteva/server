package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

const integrationWebhookSignatureTolerance = 5 * time.Minute

type appIntegrationWebhookRecord struct {
	ID                int64
	InstallID         int64
	Role              string
	ConnectionID      int64
	ProviderSlug      string
	CallbackPath      string
	CallbackURL       string
	EventsJSON        string
	ExternalWebhookID string
	SecretEncrypted   string
	Status            string
	LastError         string
	RegisteredAt      time.Time
	UpdatedAt         time.Time
}

func (s *Server) handleCallbackIntegrationWebhooks(w http.ResponseWriter, r *http.Request, parts []string) {
	if r.Method != http.MethodPost || len(parts) != 1 {
		http.Error(w, "POST /integration-webhooks/ensure or /verify", http.StatusMethodNotAllowed)
		return
	}
	switch parts[0] {
	case "ensure":
		s.handleEnsureIntegrationWebhook(w, r)
	case "verify":
		s.handleVerifyIntegrationWebhook(w, r)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *Server) handleEnsureIntegrationWebhook(w http.ResponseWriter, r *http.Request) {
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if !installHasPermission(s, installID, sdk.PermConnectionsExecute) {
		http.Error(w, "missing permission: "+string(sdk.PermConnectionsExecute), http.StatusForbidden)
		return
	}
	var req sdk.IntegrationWebhookEnsureRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	status, err := s.ensureIntegrationWebhook(r, installID, req)
	if err != nil {
		http.Error(w, err.Error(), integrationWebhookHTTPStatus(err))
		return
	}
	writeJSON(w, status)
}

func (s *Server) handleVerifyIntegrationWebhook(w http.ResponseWriter, r *http.Request) {
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var req sdk.IntegrationWebhookVerifyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.Role = strings.TrimSpace(req.Role)
	if req.Role == "" || req.Payload == "" || req.Signature == "" {
		http.Error(w, "role, payload, and signature required", http.StatusBadRequest)
		return
	}
	record, err := s.integrationWebhookByInstallRole(installID, req.Role)
	if err != nil {
		if errors.Is(err, errIntegrationWebhookNotFound) {
			http.Error(w, "integration webhook is not registered", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "load integration webhook: "+err.Error(), http.StatusInternalServerError)
		return
	}
	boundRole, bound := installBoundConnection(s, installID, record.ConnectionID)
	if !bound || boundRole != record.Role {
		http.Error(w, "integration webhook binding is no longer active", http.StatusServiceUnavailable)
		return
	}
	secret, err := Decrypt(s.secret, record.SecretEncrypted)
	if err != nil {
		http.Error(w, "decrypt integration webhook secret", http.StatusInternalServerError)
		return
	}
	switch record.ProviderSlug {
	case "stripe":
		if err := verifyStripeIntegrationSignature([]byte(req.Payload), req.Signature, secret, time.Now().UTC()); err != nil {
			http.Error(w, "webhook verification failed: "+err.Error(), http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "webhook verification unsupported for provider "+record.ProviderSlug, http.StatusNotImplemented)
		return
	}
	if !json.Valid([]byte(req.Payload)) {
		http.Error(w, "verified webhook payload is not valid json", http.StatusBadRequest)
		return
	}
	writeJSON(w, sdk.IntegrationWebhookVerifyResult{
		Provider: record.ProviderSlug,
		Event:    json.RawMessage(req.Payload),
	})
}

var errIntegrationWebhookNotFound = errors.New("integration webhook not found")

func (s *Server) ensureIntegrationWebhook(r *http.Request, installID int64, req sdk.IntegrationWebhookEnsureRequest) (*sdk.IntegrationWebhookStatus, error) {
	s.integrationWebhookMu.Lock()
	defer s.integrationWebhookMu.Unlock()

	req.Role = strings.TrimSpace(req.Role)
	req.CallbackPath = strings.TrimSpace(req.CallbackPath)
	if req.ConnectionID <= 0 || req.Role == "" || req.CallbackPath == "" || len(req.Events) == 0 {
		return nil, errors.New("connection_id, role, callback_path, and events required")
	}
	boundRole, bound := installBoundConnection(s, installID, req.ConnectionID)
	if !bound || boundRole != req.Role {
		return nil, errors.New("connection is not bound to the requested role")
	}
	dep, err := installRoleDep(s, installID, req.Role)
	if err != nil {
		return nil, err
	}
	if dep == nil || dep.Kind == "app" {
		return nil, errors.New("requested role is not a declared integration dependency")
	}
	userID := getUserID(r)
	conn, encCreds, err := s.store.GetConnection(userID, req.ConnectionID)
	if err != nil || conn == nil {
		return nil, errors.New("bound connection not found")
	}
	if len(dep.CompatibleSlugs) > 0 && !contains(dep.CompatibleSlugs, conn.AppSlug) {
		return nil, fmt.Errorf("connection slug %q is not compatible with role %q", conn.AppSlug, req.Role)
	}
	app := s.catalog.Get(conn.AppSlug)
	if app == nil || app.Webhooks == nil || app.Webhooks.Registration == nil {
		return nil, errors.New("integration does not support automatic webhook registration")
	}
	reg := app.Webhooks.Registration
	if reg.ManualSetup != "" {
		return nil, errors.New(reg.ManualSetup)
	}
	if reg.IDField == "" || reg.ResponseSecretField == "" {
		return nil, errors.New("integration webhook registration must declare id_field and response_secret_field")
	}
	callbackURL, err := s.integrationWebhookCallbackURL(installID, req.CallbackPath)
	if err != nil {
		return nil, err
	}
	events := normalizeWebhookEvents(req.Events)
	allowedEvents := make(map[string]bool, len(app.Webhooks.Events))
	for _, event := range app.Webhooks.Events {
		allowedEvents[event.Name] = true
	}
	for _, event := range events {
		if !allowedEvents[event] {
			return nil, fmt.Errorf("webhook event %q is not declared by integration %q", event, conn.AppSlug)
		}
	}
	if len(events) == 0 {
		return nil, errors.New("events must contain at least one declared integration event")
	}
	eventsJSONBytes, _ := json.Marshal(events)
	eventsJSON := string(eventsJSONBytes)
	if current, err := s.integrationWebhookByInstallRole(installID, req.Role); err == nil &&
		current.ConnectionID == req.ConnectionID &&
		current.CallbackURL == callbackURL &&
		current.EventsJSON == eventsJSON &&
		current.Status == "ready" &&
		current.ExternalWebhookID != "" &&
		current.SecretEncrypted != "" {
		return integrationWebhookStatus(current), nil
	}

	plain, err := Decrypt(s.secret, encCreds)
	if err != nil {
		return nil, errors.New("decrypt connection credentials")
	}
	credentials := map[string]string{}
	if err := json.Unmarshal([]byte(plain), &credentials); err != nil {
		return nil, errors.New("decode connection credentials")
	}
	input := map[string]any{}
	for k, v := range reg.Extra {
		input[k] = v
	}
	setField(input, reg.URLField, callbackURL)
	if reg.EventsField != "" {
		setField(input, reg.EventsField, events)
	}
	registerTool := &AppToolDef{
		Name:        "__platform_register_webhook",
		Method:      reg.Method,
		Path:        reg.Path,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}
	if reg.ContentType != "" {
		registerTool.Headers = map[string]string{"Content-Type": reg.ContentType}
	}
	result, err := executeIntegrationTool(app, registerTool, credentials, input, "")
	if err != nil {
		return nil, fmt.Errorf("register provider webhook: %w", err)
	}
	if result == nil || !result.Success || result.Status < 200 || result.Status >= 300 {
		return nil, fmt.Errorf("register provider webhook failed (HTTP %d)", integrationResultStatus(result))
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		return nil, errors.New("register provider webhook returned a non-object response")
	}
	externalID := extractJSONPath(data, reg.IDField)
	signingSecret := extractJSONPath(data, reg.ResponseSecretField)
	if externalID == "" || signingSecret == "" {
		return nil, errors.New("provider did not return webhook id and signing secret")
	}
	encryptedSecret, err := Encrypt(s.secret, signingSecret)
	if err != nil {
		return nil, errors.New("encrypt webhook signing secret")
	}
	old, _ := s.integrationWebhookByInstallRole(installID, req.Role)
	now := time.Now().UTC()
	res, err := s.store.db.Exec(`
		INSERT INTO app_integration_webhooks
			(install_id, role, connection_id, provider_slug, callback_path,
			 callback_url, events_json, external_webhook_id, secret_encrypted,
			 status, last_error, registered_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'ready', '', ?, ?)
		ON CONFLICT(install_id, role) DO UPDATE SET
			connection_id=excluded.connection_id,
			provider_slug=excluded.provider_slug,
			callback_path=excluded.callback_path,
			callback_url=excluded.callback_url,
			events_json=excluded.events_json,
			external_webhook_id=excluded.external_webhook_id,
			secret_encrypted=excluded.secret_encrypted,
			status='ready',
			last_error='',
			registered_at=excluded.registered_at,
			updated_at=excluded.updated_at`,
		installID, req.Role, req.ConnectionID, conn.AppSlug, req.CallbackPath,
		callbackURL, eventsJSON, externalID, encryptedSecret, now, now)
	if err != nil {
		_ = deleteExternalIntegrationWebhook(app, reg, credentials, externalID)
		return nil, fmt.Errorf("persist integration webhook: %w", err)
	}
	id, _ := res.LastInsertId()
	record, err := s.integrationWebhookByInstallRole(installID, req.Role)
	if err != nil {
		return nil, err
	}
	if record.ID == 0 {
		record.ID = id
	}
	if old != nil && old.ExternalWebhookID != "" &&
		(old.ExternalWebhookID != externalID || old.ConnectionID != req.ConnectionID) {
		s.deleteOldIntegrationWebhook(userID, old)
	}
	return integrationWebhookStatus(record), nil
}

func (s *Server) integrationWebhookCallbackURL(installID int64, callbackPath string) (string, error) {
	if !strings.HasPrefix(callbackPath, "/") || strings.Contains(callbackPath, "..") ||
		strings.ContainsAny(callbackPath, "?#") {
		return "", errors.New("callback_path must be an absolute app path without query, fragment, or traversal")
	}
	manifest, err := installManifest(s, installID)
	if err != nil || manifest == nil {
		return "", errors.New("install manifest not found")
	}
	allowed := false
	for _, route := range manifest.Provides.HTTPRoutes {
		if route.NoAuth && appRouteMatches(route.Prefix, callbackPath) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", errors.New("callback_path must match a no_auth route declared by the app")
	}
	base := strings.TrimRight(s.publicBaseURL(), "/")
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("platform public URL is not configured")
	}
	appName := s.callerAppName(installID)
	if appName == "" {
		return "", errors.New("calling app name not found")
	}
	return base + "/api/apps/" + url.PathEscape(appName) + callbackPath, nil
}

func normalizeWebhookEvents(events []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(events))
	for _, event := range events {
		event = strings.TrimSpace(event)
		if event == "" || seen[event] {
			continue
		}
		seen[event] = true
		out = append(out, event)
	}
	sort.Strings(out)
	return out
}

func integrationResultStatus(result *ExecuteResult) int {
	if result == nil {
		return 0
	}
	return result.Status
}

func (s *Server) integrationWebhookByInstallRole(installID int64, role string) (*appIntegrationWebhookRecord, error) {
	var record appIntegrationWebhookRecord
	err := s.store.db.QueryRow(`
		SELECT id, install_id, role, connection_id, provider_slug, callback_path,
		       callback_url, events_json, external_webhook_id, secret_encrypted,
		       status, last_error, registered_at, updated_at
		FROM app_integration_webhooks
		WHERE install_id=? AND role=?`, installID, role).Scan(
		&record.ID, &record.InstallID, &record.Role, &record.ConnectionID,
		&record.ProviderSlug, &record.CallbackPath, &record.CallbackURL,
		&record.EventsJSON, &record.ExternalWebhookID, &record.SecretEncrypted,
		&record.Status, &record.LastError, &record.RegisteredAt, &record.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errIntegrationWebhookNotFound
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

func integrationWebhookStatus(record *appIntegrationWebhookRecord) *sdk.IntegrationWebhookStatus {
	if record == nil {
		return nil
	}
	return &sdk.IntegrationWebhookStatus{
		ID: record.ID, ConnectionID: record.ConnectionID, Role: record.Role,
		Provider: record.ProviderSlug, CallbackURL: record.CallbackURL,
		ExternalID: record.ExternalWebhookID, Status: record.Status,
		LastError: record.LastError, RegisteredAt: record.RegisteredAt,
		UpdatedAt: record.UpdatedAt,
	}
}

func (s *Server) deleteOldIntegrationWebhook(userID int64, old *appIntegrationWebhookRecord) {
	conn, encCreds, err := s.store.GetConnection(userID, old.ConnectionID)
	if err != nil || conn == nil {
		return
	}
	app := s.catalog.Get(conn.AppSlug)
	if app == nil || app.Webhooks == nil || app.Webhooks.Registration == nil {
		return
	}
	plain, err := Decrypt(s.secret, encCreds)
	if err != nil {
		return
	}
	credentials := map[string]string{}
	if json.Unmarshal([]byte(plain), &credentials) != nil {
		return
	}
	if err := deleteExternalIntegrationWebhook(app, app.Webhooks.Registration, credentials, old.ExternalWebhookID); err != nil {
		log.Printf("[APP-WEBHOOK] cleanup install=%d role=%s external_id=%s: %v",
			old.InstallID, old.Role, old.ExternalWebhookID, err)
	}
}

// cleanupInactiveIntegrationWebhooks removes platform-owned provider
// endpoints when an app role is explicitly unbound or the app is
// uninstalled. A connection swap is intentionally left in place until
// the app calls ensure again; ensure creates the replacement first and
// then removes the old endpoint, avoiding a delivery gap.
func (s *Server) cleanupInactiveIntegrationWebhooks(installID int64, deleteAll bool) {
	var userID int64
	if err := s.store.db.QueryRow(
		`SELECT installed_by FROM app_installs WHERE id=?`, installID,
	).Scan(&userID); err != nil {
		return
	}
	bindings := bindingsForInstall(s, installID)
	rows, err := s.store.db.Query(`
		SELECT id, install_id, role, connection_id, provider_slug, callback_path,
		       callback_url, events_json, external_webhook_id, secret_encrypted,
		       status, last_error, registered_at, updated_at
		FROM app_integration_webhooks WHERE install_id=?`, installID)
	if err != nil {
		return
	}
	defer rows.Close()
	var stale []*appIntegrationWebhookRecord
	for rows.Next() {
		var record appIntegrationWebhookRecord
		if err := rows.Scan(
			&record.ID, &record.InstallID, &record.Role, &record.ConnectionID,
			&record.ProviderSlug, &record.CallbackPath, &record.CallbackURL,
			&record.EventsJSON, &record.ExternalWebhookID, &record.SecretEncrypted,
			&record.Status, &record.LastError, &record.RegisteredAt, &record.UpdatedAt,
		); err != nil {
			continue
		}
		raw, present := bindings[record.Role]
		ids, defaultID := appBindingIDs(raw)
		if deleteAll || !present || (len(ids) == 0 && defaultID == 0) {
			stale = append(stale, &record)
		}
	}
	_ = rows.Close()
	for _, record := range stale {
		s.deleteOldIntegrationWebhook(userID, record)
		if _, err := s.store.db.Exec(`DELETE FROM app_integration_webhooks WHERE id=?`, record.ID); err != nil {
			log.Printf("[APP-WEBHOOK] delete record install=%d role=%s: %v", installID, record.Role, err)
		}
	}
}

func deleteExternalIntegrationWebhook(app *AppTemplate, reg *WebhookRegConfig, credentials map[string]string, externalID string) error {
	if reg == nil || reg.DeletePath == "" || externalID == "" {
		return nil
	}
	method := reg.DeleteMethod
	if method == "" {
		method = http.MethodDelete
	}
	path := strings.ReplaceAll(reg.DeletePath, "{id}", url.PathEscape(externalID))
	tool := &AppToolDef{
		Name: "__platform_delete_webhook", Method: method, Path: path,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}
	result, err := executeIntegrationTool(app, tool, credentials, map[string]any{}, "")
	if err != nil {
		return err
	}
	if result == nil || !result.Success || result.Status < 200 || result.Status >= 300 {
		return fmt.Errorf("delete provider webhook failed (HTTP %d)", integrationResultStatus(result))
	}
	return nil
}

func verifyStripeIntegrationSignature(payload []byte, header, secret string, now time.Time) error {
	var timestamp int64
	var signatures []string
	for _, part := range strings.Split(header, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch key {
		case "t":
			timestamp, _ = strconv.ParseInt(value, 10, 64)
		case "v1":
			signatures = append(signatures, value)
		}
	}
	if timestamp == 0 || len(signatures) == 0 {
		return errors.New("invalid Stripe-Signature header")
	}
	signedAt := time.Unix(timestamp, 0)
	if delta := now.Sub(signedAt); delta > integrationWebhookSignatureTolerance || delta < -integrationWebhookSignatureTolerance {
		return errors.New("Stripe-Signature timestamp outside tolerance")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(payload)
	expected := mac.Sum(nil)
	for _, signature := range signatures {
		got, err := hex.DecodeString(signature)
		if err == nil && hmac.Equal(got, expected) {
			return nil
		}
	}
	return errors.New("Stripe-Signature HMAC mismatch")
}

func integrationWebhookHTTPStatus(err error) int {
	message := err.Error()
	switch {
	case strings.Contains(message, "not bound"), strings.Contains(message, "not compatible"):
		return http.StatusForbidden
	case strings.Contains(message, "credentials"), strings.Contains(message, "provider webhook"):
		return http.StatusBadGateway
	default:
		return http.StatusBadRequest
	}
}
