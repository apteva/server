package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultMobilePushRelayURL = "https://push.apteva.ai"
	mobilePushPollInterval    = 5 * time.Second
	mobilePushHTTPTimeout     = 12 * time.Second
)

type mobilePushRelayConfig struct {
	DBURL     string
	EnvURL    string
	Effective string
	Source    string
}

type mobilePushSubscription struct {
	ID                  string `json:"id"`
	InstallationID      string `json:"installation_id"`
	UserID              int64  `json:"-"`
	RelayDeviceID       string `json:"relay_device_id"`
	RelayGrantEncrypted string `json:"-"`
	GrantExpiresAt      string `json:"grant_expires_at"`
	Platform            string `json:"platform"`
	BundleID            string `json:"bundle_id"`
	Environment         string `json:"environment"`
	AppVersion          string `json:"app_version,omitempty"`
	DeviceName          string `json:"device_name,omitempty"`
	Status              string `json:"status"`
	LastInboxMessageID  int64  `json:"last_inbox_message_id"`
	LastBadge           int    `json:"last_badge"`
	RetryCount          int    `json:"retry_count"`
	NextRetryAt         string `json:"next_retry_at,omitempty"`
	LastError           string `json:"last_error,omitempty"`
	LastSuccessAt       string `json:"last_success_at,omitempty"`
	LastSeenAt          string `json:"last_seen_at"`
	CreatedAt           string `json:"created_at"`
	UpdatedAt           string `json:"updated_at"`
}

type mobilePushInboxItem struct {
	MessageID int64
	Kind      string
	ProjectID string
}

type mobilePushComponent struct {
	App   string         `json:"app"`
	Name  string         `json:"name"`
	Props map[string]any `json:"props"`
}

type mobilePushRelayError struct {
	Status int
	Body   string
}

func (e *mobilePushRelayError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("push relay returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("push relay returned HTTP %d: %s", e.Status, e.Body)
}

func (s *Server) mobilePushRelayConfig() mobilePushRelayConfig {
	config := mobilePushRelayConfig{
		EnvURL: strings.TrimSpace(os.Getenv("APTEVA_PUSH_RELAY_URL")),
	}
	if s.store != nil {
		config.DBURL = strings.TrimSpace(s.store.GetSetting("push_relay_url"))
	}

	switch {
	case config.DBURL != "":
		config.Effective = config.DBURL
		config.Source = "db"
	case config.EnvURL != "":
		config.Effective = config.EnvURL
		config.Source = "env"
	default:
		config.Effective = defaultMobilePushRelayURL
		config.Source = "default"
	}
	config.Effective = strings.TrimRight(config.Effective, "/")
	return config
}

func (s *Server) mobilePushRelayURL() string {
	return s.mobilePushRelayConfig().Effective
}

func (s *Server) mobilePushInstanceRef() string {
	if s.store == nil {
		return ""
	}
	if value := strings.TrimSpace(s.store.GetSetting("push_instance_ref")); value != "" {
		return value
	}
	value := "instance_" + generateToken(18)
	if err := s.store.SetSetting("push_instance_ref", value); err != nil {
		log.Printf("[MOBILE-PUSH] persist instance reference: %v", err)
	}
	return value
}

func (s *Server) mobilePushUserRef(userID int64) string {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(s.mobilePushInstanceRef()))
	_, _ = mac.Write([]byte(":"))
	_, _ = mac.Write([]byte(strconv.FormatInt(userID, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) handleMobilePushConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]any{
		"enabled":                     s.mobilePushRelayURL() != "",
		"supports_background_refresh": false,
	})
}

func (s *Server) handleMobilePushSubscriptions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listMobilePushSubscriptions(w, r)
	case http.MethodPost:
		s.registerMobilePushSubscription(w, r)
	default:
		http.Error(w, "GET or POST required", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleMobilePushSubscription(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/mobile/push/subscriptions/"), "/")
	if rest == "" {
		http.Error(w, "subscription id required", http.StatusBadRequest)
		return
	}
	parts := strings.Split(rest, "/")
	if len(parts) == 2 && parts[1] == "test" {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		s.testMobilePushSubscription(w, r, parts[0])
		return
	}
	if len(parts) != 1 || r.Method != http.MethodDelete {
		http.Error(w, "DELETE required", http.StatusMethodNotAllowed)
		return
	}
	s.deleteMobilePushSubscription(w, r, parts[0])
}

func (s *Server) registerMobilePushSubscription(w http.ResponseWriter, r *http.Request) {
	relayURL := s.mobilePushRelayURL()
	if relayURL == "" {
		http.Error(w, "mobile push is not configured", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		InstallationID string `json:"installation_id"`
		ProviderToken  string `json:"provider_token"`
		Platform       string `json:"platform"`
		BundleID       string `json:"bundle_id"`
		Environment    string `json:"environment"`
		AppVersion     string `json:"app_version"`
		DeviceName     string `json:"device_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	body.InstallationID = strings.TrimSpace(body.InstallationID)
	body.ProviderToken = strings.TrimSpace(body.ProviderToken)
	body.Platform = strings.ToLower(strings.TrimSpace(body.Platform))
	body.BundleID = strings.TrimSpace(body.BundleID)
	body.Environment = strings.ToLower(strings.TrimSpace(body.Environment))
	body.AppVersion = strings.TrimSpace(body.AppVersion)
	body.DeviceName = strings.TrimSpace(body.DeviceName)
	if !validMobilePushInstallationID(body.InstallationID) {
		http.Error(w, "valid installation_id required", http.StatusBadRequest)
		return
	}
	if body.Platform == "" {
		body.Platform = "ios"
	}
	if body.Platform != "ios" && body.Platform != "android" {
		http.Error(w, "platform must be ios or android", http.StatusBadRequest)
		return
	}
	if len(body.ProviderToken) < 8 || len(body.ProviderToken) > 4096 || strings.ContainsAny(body.ProviderToken, " \t\r\n") {
		http.Error(w, "valid provider_token required", http.StatusBadRequest)
		return
	}
	if !validMobilePushBundleID(body.BundleID) {
		http.Error(w, "valid bundle_id required", http.StatusBadRequest)
		return
	}
	if body.Platform == "android" && body.Environment == "" {
		body.Environment = "production"
	}
	if body.Platform == "android" && body.Environment != "production" {
		http.Error(w, "Android environment must be production", http.StatusBadRequest)
		return
	}
	if body.Platform == "ios" && body.Environment != "sandbox" && body.Environment != "production" {
		http.Error(w, "environment must be sandbox or production", http.StatusBadRequest)
		return
	}
	userID := getUserID(r)
	if userID == 0 {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}

	var relayResponse struct {
		Device struct {
			ID string `json:"id"`
		} `json:"device"`
		Grant     string `json:"grant"`
		ExpiresAt string `json:"expires_at"`
	}
	err := s.mobilePushRelayRequest(r.Context(), http.MethodPost, "/v1/devices/register", "", map[string]any{
		"provider_token": body.ProviderToken,
		"platform":       body.Platform,
		"bundle_id":      body.BundleID,
		"environment":    body.Environment,
		"instance_ref":   s.mobilePushInstanceRef(),
		"user_ref":       s.mobilePushUserRef(userID),
		"app_version":    body.AppVersion,
	}, &relayResponse)
	if err != nil {
		// Do not log the relay response body here: registration is the one
		// request that carries the raw APNs token, and an untrusted/broken
		// relay could echo request fields in its error response.
		log.Printf("[MOBILE-PUSH] relay registration failed user=%d", userID)
		http.Error(w, "push relay registration failed", http.StatusBadGateway)
		return
	}
	relayResponse.Device.ID = strings.TrimSpace(relayResponse.Device.ID)
	relayResponse.Grant = strings.TrimSpace(relayResponse.Grant)
	relayResponse.ExpiresAt = strings.TrimSpace(relayResponse.ExpiresAt)
	if relayResponse.Device.ID == "" || relayResponse.Grant == "" || relayResponse.ExpiresAt == "" {
		http.Error(w, "push relay returned an incomplete registration", http.StatusBadGateway)
		return
	}
	if _, err := time.Parse(time.RFC3339, relayResponse.ExpiresAt); err != nil {
		if _, nanoErr := time.Parse(time.RFC3339Nano, relayResponse.ExpiresAt); nanoErr != nil {
			http.Error(w, "push relay returned an invalid expiry", http.StatusBadGateway)
			return
		}
	}
	encryptedGrant, err := Encrypt(s.secret, relayResponse.Grant)
	if err != nil {
		http.Error(w, "could not protect push registration", http.StatusInternalServerError)
		return
	}
	cursor, err := s.mobilePushUserHighWater(userID)
	if err != nil {
		http.Error(w, "could not initialize inbox cursor", http.StatusInternalServerError)
		return
	}
	subscription, err := s.upsertMobilePushSubscription(&mobilePushSubscription{
		ID:                  "mps_" + generateToken(16),
		InstallationID:      body.InstallationID,
		UserID:              userID,
		RelayDeviceID:       relayResponse.Device.ID,
		RelayGrantEncrypted: encryptedGrant,
		GrantExpiresAt:      relayResponse.ExpiresAt,
		Platform:            body.Platform,
		BundleID:            body.BundleID,
		Environment:         body.Environment,
		AppVersion:          body.AppVersion,
		DeviceName:          body.DeviceName,
		Status:              "active",
		LastInboxMessageID:  cursor,
	})
	if err != nil {
		http.Error(w, "could not save push registration", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"subscription": subscription})
}

func (s *Server) listMobilePushSubscriptions(w http.ResponseWriter, r *http.Request) {
	items, err := s.mobilePushSubscriptionsForUser(getUserID(r))
	if err != nil {
		http.Error(w, "could not list push subscriptions", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"subscriptions": items})
}

func (s *Server) deleteMobilePushSubscription(w http.ResponseWriter, r *http.Request, id string) {
	subscription, err := s.mobilePushSubscriptionForUser(id, getUserID(r))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "push subscription not found", http.StatusNotFound)
			return
		}
		http.Error(w, "could not load push subscription", http.StatusInternalServerError)
		return
	}
	grant, decryptErr := Decrypt(s.secret, subscription.RelayGrantEncrypted)
	remoteRevoked := false
	if decryptErr == nil && s.mobilePushRelayURL() != "" {
		// New relays revoke only this instance grant. A 404 is tolerated so
		// servers remain compatible with the initial relay release, whose
		// device-level DELETE would incorrectly revoke other instances too.
		err = s.mobilePushRelayRequest(r.Context(), http.MethodDelete, "/v1/grants/current", grant, nil, nil)
		remoteRevoked = err == nil
	}
	if _, err := s.store.db.Exec(
		`DELETE FROM mobile_push_subscriptions WHERE id = ? AND user_id = ?`,
		id, getUserID(r),
	); err != nil {
		http.Error(w, "could not delete push subscription", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"deleted": true, "remote_revoked": remoteRevoked})
}

func (s *Server) testMobilePushSubscription(w http.ResponseWriter, r *http.Request, id string) {
	subscription, err := s.mobilePushSubscriptionForUser(id, getUserID(r))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "push subscription not found", http.StatusNotFound)
			return
		}
		http.Error(w, "could not load push subscription", http.StatusInternalServerError)
		return
	}
	grant, err := Decrypt(s.secret, subscription.RelayGrantEncrypted)
	if err != nil {
		http.Error(w, "push grant is unavailable", http.StatusConflict)
		return
	}
	var delivery map[string]any
	err = s.mobilePushRelayRequest(
		r.Context(),
		http.MethodPost,
		"/v1/devices/"+url.PathEscape(subscription.RelayDeviceID)+"/test",
		grant,
		map[string]any{},
		&delivery,
	)
	if err != nil {
		http.Error(w, "test notification failed", http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"delivery": delivery})
}

func validMobilePushInstallationID(value string) bool {
	if len(value) < 8 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func validMobilePushBundleID(value string) bool {
	if len(value) < 3 || len(value) > 255 || !strings.Contains(value, ".") {
		return false
	}
	for _, part := range strings.Split(value, ".") {
		if part == "" {
			return false
		}
		for _, r := range part {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func (s *Server) mobilePushRelayRequest(
	ctx context.Context,
	method, path, grant string,
	input any,
	output any,
) error {
	base := s.mobilePushRelayURL()
	if base == "" {
		return errors.New("mobile push relay is not configured")
	}
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if grant != "" {
		req.Header.Set("Authorization", "Bearer "+grant)
	}
	client := &http.Client{Timeout: mobilePushHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &mobilePushRelayError{Status: resp.StatusCode, Body: safeMobilePushRelayError(data)}
	}
	if output != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, output); err != nil {
			return fmt.Errorf("decode push relay response: %w", err)
		}
	}
	return nil
}

func safeMobilePushRelayError(data []byte) string {
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &body) == nil && strings.TrimSpace(body.Error) != "" {
		return strings.TrimSpace(body.Error)
	}
	value := strings.TrimSpace(string(data))
	if len(value) > 300 {
		value = value[:300]
	}
	return value
}

func (s *Server) upsertMobilePushSubscription(input *mobilePushSubscription) (*mobilePushSubscription, error) {
	_, err := s.store.db.Exec(`
		INSERT INTO mobile_push_subscriptions (
			id, installation_id, user_id, relay_device_id, relay_grant_encrypted,
			grant_expires_at, platform, bundle_id, environment, app_version, device_name,
			status, last_inbox_message_id, last_seen_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(installation_id) DO UPDATE SET
			user_id = excluded.user_id,
			relay_device_id = excluded.relay_device_id,
			relay_grant_encrypted = excluded.relay_grant_encrypted,
			grant_expires_at = excluded.grant_expires_at,
			platform = excluded.platform,
			bundle_id = excluded.bundle_id,
			environment = excluded.environment,
			app_version = excluded.app_version,
			device_name = excluded.device_name,
			status = 'active',
			last_inbox_message_id = CASE
				WHEN mobile_push_subscriptions.user_id = excluded.user_id
				THEN mobile_push_subscriptions.last_inbox_message_id
				ELSE excluded.last_inbox_message_id
			END,
			retry_count = 0,
			next_retry_at = NULL,
			last_error = '',
			last_seen_at = CURRENT_TIMESTAMP,
			updated_at = CURRENT_TIMESTAMP`,
		input.ID, input.InstallationID, input.UserID, input.RelayDeviceID,
		input.RelayGrantEncrypted, input.GrantExpiresAt, input.Platform, input.BundleID,
		input.Environment, input.AppVersion, input.DeviceName, input.LastInboxMessageID,
	)
	if err != nil {
		return nil, err
	}
	return s.mobilePushSubscriptionByInstallation(input.InstallationID)
}

const mobilePushSubscriptionColumns = `
	id, installation_id, user_id, relay_device_id, relay_grant_encrypted,
	grant_expires_at, platform, bundle_id, environment, app_version, device_name,
	status, last_inbox_message_id, last_badge, retry_count,
	COALESCE(next_retry_at, ''), last_error, COALESCE(last_success_at, ''),
	COALESCE(last_seen_at, ''), COALESCE(created_at, ''), COALESCE(updated_at, '')`

type mobilePushScanner interface {
	Scan(dest ...any) error
}

func scanMobilePushSubscription(scanner mobilePushScanner) (*mobilePushSubscription, error) {
	var item mobilePushSubscription
	err := scanner.Scan(
		&item.ID, &item.InstallationID, &item.UserID, &item.RelayDeviceID,
		&item.RelayGrantEncrypted, &item.GrantExpiresAt, &item.Platform, &item.BundleID,
		&item.Environment, &item.AppVersion, &item.DeviceName, &item.Status,
		&item.LastInboxMessageID, &item.LastBadge, &item.RetryCount,
		&item.NextRetryAt, &item.LastError, &item.LastSuccessAt,
		&item.LastSeenAt, &item.CreatedAt, &item.UpdatedAt,
	)
	return &item, err
}

func (s *Server) mobilePushSubscriptionByInstallation(installationID string) (*mobilePushSubscription, error) {
	return scanMobilePushSubscription(s.store.db.QueryRow(
		`SELECT `+mobilePushSubscriptionColumns+`
		   FROM mobile_push_subscriptions WHERE installation_id = ?`,
		installationID,
	))
}

func (s *Server) mobilePushSubscriptionForUser(id string, userID int64) (*mobilePushSubscription, error) {
	return scanMobilePushSubscription(s.store.db.QueryRow(
		`SELECT `+mobilePushSubscriptionColumns+`
		   FROM mobile_push_subscriptions WHERE id = ? AND user_id = ?`,
		id, userID,
	))
}

func (s *Server) mobilePushSubscriptionsForUser(userID int64) ([]mobilePushSubscription, error) {
	rows, err := s.store.db.Query(
		`SELECT `+mobilePushSubscriptionColumns+`
		   FROM mobile_push_subscriptions
		  WHERE user_id = ?
		  ORDER BY created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []mobilePushSubscription{}
	for rows.Next() {
		item, err := scanMobilePushSubscription(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

func (s *Server) activeMobilePushSubscriptions() ([]mobilePushSubscription, error) {
	rows, err := s.store.db.Query(
		`SELECT ` + mobilePushSubscriptionColumns + `
		   FROM mobile_push_subscriptions
		  WHERE status = 'active'
		    AND (next_retry_at IS NULL OR datetime(next_retry_at) <= CURRENT_TIMESTAMP)
		  ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []mobilePushSubscription{}
	for rows.Next() {
		item, err := scanMobilePushSubscription(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

func (s *Server) startMobilePushWorker() {
	if s.mobilePushCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.mobilePushCancel = cancel
	go func() {
		ticker := time.NewTicker(mobilePushPollInterval)
		defer ticker.Stop()
		for {
			if err := s.deliverMobilePushCycle(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("[MOBILE-PUSH] delivery cycle: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Server) stopMobilePushWorker() {
	if s.mobilePushCancel != nil {
		s.mobilePushCancel()
		s.mobilePushCancel = nil
	}
}

func (s *Server) deliverMobilePushCycle(ctx context.Context) error {
	if s.mobilePushRelayURL() == "" {
		return nil
	}
	subscriptions, err := s.activeMobilePushSubscriptions()
	if err != nil {
		return err
	}
	for i := range subscriptions {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.deliverMobilePushSubscription(ctx, &subscriptions[i])
	}
	return nil
}

func (s *Server) deliverMobilePushSubscription(ctx context.Context, subscription *mobilePushSubscription) {
	grantExpires, err := time.Parse(time.RFC3339Nano, subscription.GrantExpiresAt)
	if err != nil || !grantExpires.After(time.Now()) {
		s.invalidateMobilePushSubscription(subscription.ID, "push grant expired")
		return
	}
	highWater, err := s.mobilePushUserHighWater(subscription.UserID)
	if err != nil {
		s.retryMobilePushSubscription(subscription, err)
		return
	}
	if highWater <= subscription.LastInboxMessageID {
		return
	}
	items, err := s.mobilePushInboxItems(
		subscription.UserID,
		subscription.LastInboxMessageID,
		highWater,
	)
	if err != nil {
		s.retryMobilePushSubscription(subscription, err)
		return
	}
	badge, err := s.mobilePushInboxBadge(subscription.UserID)
	if err != nil {
		s.retryMobilePushSubscription(subscription, err)
		return
	}
	grant, err := Decrypt(s.secret, subscription.RelayGrantEncrypted)
	if err != nil {
		s.invalidateMobilePushSubscription(subscription.ID, "stored push grant is unavailable")
		return
	}
	for _, item := range items {
		var delivery struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		key := fmt.Sprintf("%s:%s:%s:%d",
			s.mobilePushInstanceRef(), subscription.ID, item.Kind, item.MessageID)
		err = s.mobilePushRelayRequest(ctx, http.MethodPost, "/v1/deliveries", grant, map[string]any{
			"device_id":       subscription.RelayDeviceID,
			"type":            item.Kind,
			"item_id":         strconv.FormatInt(item.MessageID, 10),
			"project_id":      item.ProjectID,
			"badge":           badge,
			"idempotency_key": key,
		}, &delivery)
		if err != nil {
			if permanentMobilePushError(err) {
				s.invalidateMobilePushSubscription(subscription.ID, err.Error())
			} else {
				s.retryMobilePushSubscription(subscription, err)
			}
			return
		}
	}
	_, err = s.store.db.Exec(`
		UPDATE mobile_push_subscriptions
		   SET last_inbox_message_id = ?,
		       last_badge = ?,
		       retry_count = 0,
		       next_retry_at = NULL,
		       last_error = '',
		       last_success_at = CURRENT_TIMESTAMP,
		       updated_at = CURRENT_TIMESTAMP
		 WHERE id = ?`,
		highWater, badge, subscription.ID,
	)
	if err != nil {
		log.Printf("[MOBILE-PUSH] advance cursor subscription=%s: %v", subscription.ID, err)
	}
}

func permanentMobilePushError(err error) bool {
	var relayErr *mobilePushRelayError
	if !errors.As(err, &relayErr) {
		return false
	}
	switch relayErr.Status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusGone:
		return true
	}
	value := strings.ToLower(relayErr.Body)
	return strings.Contains(value, "baddevicetoken") ||
		strings.Contains(value, "devicetokennotfortopic") ||
		strings.Contains(value, "unregistered") ||
		strings.Contains(value, "sender_id_mismatch") ||
		strings.Contains(value, "grant expired") ||
		strings.Contains(value, "grant revoked")
}

func (s *Server) invalidateMobilePushSubscription(id, reason string) {
	_, err := s.store.db.Exec(`
		UPDATE mobile_push_subscriptions
		   SET status = 'invalid',
		       last_error = ?,
		       next_retry_at = NULL,
		       updated_at = CURRENT_TIMESTAMP
		 WHERE id = ?`,
		truncateMobilePushError(reason), id,
	)
	if err != nil {
		log.Printf("[MOBILE-PUSH] invalidate subscription=%s: %v", id, err)
	}
}

func (s *Server) retryMobilePushSubscription(subscription *mobilePushSubscription, deliveryErr error) {
	retryCount := subscription.RetryCount + 1
	delay := 15 * time.Second
	for i := 1; i < retryCount && delay < 30*time.Minute; i++ {
		delay *= 2
	}
	if delay > 30*time.Minute {
		delay = 30 * time.Minute
	}
	next := time.Now().UTC().Add(delay).Format(time.RFC3339Nano)
	_, err := s.store.db.Exec(`
		UPDATE mobile_push_subscriptions
		   SET retry_count = ?,
		       next_retry_at = ?,
		       last_error = ?,
		       updated_at = CURRENT_TIMESTAMP
		 WHERE id = ?`,
		retryCount, next, truncateMobilePushError(deliveryErr.Error()), subscription.ID,
	)
	if err != nil {
		log.Printf("[MOBILE-PUSH] schedule retry subscription=%s: %v", subscription.ID, err)
	}
}

func truncateMobilePushError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 500 {
		value = value[:500]
	}
	return value
}

func (s *Server) mobilePushUserHighWater(userID int64) (int64, error) {
	var highWater int64
	err := s.store.db.QueryRow(`
		SELECT COALESCE(MAX(m.id), 0)
		  FROM channel_chat_messages m
		  JOIN channel_chat_chats c ON c.id = m.chat_id
		  JOIN agents a ON a.id = COALESCE(m.agent_id, c.agent_id)
		 WHERE a.user_id = ?
		   AND COALESCE(a.kind, 'user') IN ('user', 'platform_helper')`,
		userID,
	).Scan(&highWater)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "no such table") {
		return 0, nil
	}
	return highWater, err
}

func (s *Server) mobilePushInboxItems(userID, afterID, throughID int64) ([]mobilePushInboxItem, error) {
	rows, err := s.store.db.Query(`
		SELECT m.id, COALESCE(a.project_id, ''), COALESCE(m.components_json, '[]')
		  FROM channel_chat_messages m
		  JOIN channel_chat_chats c ON c.id = m.chat_id
		  JOIN agents a ON a.id = COALESCE(m.agent_id, c.agent_id)
		 WHERE a.user_id = ?
		   AND COALESCE(a.kind, 'user') IN ('user', 'platform_helper')
		   AND m.id > ?
		   AND m.id <= ?
		   AND (
		        COALESCE(m.components_json, '[]') LIKE '%"approval-card"%'
		     OR COALESCE(m.components_json, '[]') LIKE '%"report-card"%'
		     OR COALESCE(m.components_json, '[]') LIKE '%"alert-card"%'
		   )
		 ORDER BY m.id`,
		userID, afterID, throughID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []mobilePushInboxItem{}
	for rows.Next() {
		var messageID int64
		var projectID, raw string
		if err := rows.Scan(&messageID, &projectID, &raw); err != nil {
			return nil, err
		}
		kind, actionable := mobilePushInboxKind(raw)
		if actionable {
			items = append(items, mobilePushInboxItem{
				MessageID: messageID,
				Kind:      kind,
				ProjectID: projectID,
			})
		}
	}
	return items, rows.Err()
}

func (s *Server) mobilePushInboxBadge(userID int64) (int, error) {
	rows, err := s.store.db.Query(`
		SELECT COALESCE(m.components_json, '[]')
		  FROM channel_chat_messages m
		  JOIN channel_chat_chats c ON c.id = m.chat_id
		  JOIN agents a ON a.id = COALESCE(m.agent_id, c.agent_id)
		 WHERE a.user_id = ?
		   AND COALESCE(a.kind, 'user') IN ('user', 'platform_helper')
		   AND (
		        COALESCE(m.components_json, '[]') LIKE '%"approval-card"%'
		     OR COALESCE(m.components_json, '[]') LIKE '%"report-card"%'
		     OR COALESCE(m.components_json, '[]') LIKE '%"alert-card"%'
		   )`,
		userID,
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	badge := 0
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return 0, err
		}
		if _, actionable := mobilePushInboxKind(raw); actionable {
			badge++
		}
	}
	return badge, rows.Err()
}

func mobilePushInboxKind(raw string) (string, bool) {
	var components []mobilePushComponent
	if json.Unmarshal([]byte(raw), &components) != nil {
		return "", false
	}
	for _, component := range components {
		if component.App != "channel-chat" || mobilePushComponentDismissed(component.Props) {
			continue
		}
		switch component.Name {
		case "approval-card":
			status, _ := component.Props["status"].(string)
			if status == "" {
				status = "pending"
			}
			return "approval", status == "pending"
		case "report-card":
			return "report", true
		case "alert-card":
			return "alert", true
		}
	}
	return "", false
}

func mobilePushComponentDismissed(props map[string]any) bool {
	if dismissed, _ := props["dismissed"].(bool); dismissed {
		return true
	}
	if dismissedAt, _ := props["dismissed_at"].(string); strings.TrimSpace(dismissedAt) != "" {
		return true
	}
	return false
}
