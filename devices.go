package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const devicePairingTTL = 10 * time.Minute

type deviceHardware struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Description string `json:"description,omitempty"`
}

func (s *Server) registerDeviceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/devices/pair/start", s.handleDevicePairStart)
	mux.HandleFunc("/devices/pair/poll", s.handleDevicePairPoll)
	mux.HandleFunc("/devices/pair/request", s.authMiddleware(s.handleDevicePairRequest))
	mux.HandleFunc("/devices/pair/approve", s.authMiddleware(s.handleDevicePairDecision))
	mux.HandleFunc("/devices/pair/deny", s.authMiddleware(s.handleDevicePairDecision))
	mux.HandleFunc("/devices/link", s.authMiddleware(s.handleDeviceLink))
	mux.HandleFunc("/devices", s.authMiddleware(s.handleAccountDevices))
	mux.HandleFunc("/devices/", s.authMiddleware(s.handleAccountDevice))
}

func deviceJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(out); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		http.Error(w, "expected one JSON object", http.StatusBadRequest)
		return false
	}
	return true
}

func deviceMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != method {
		w.Header().Set("Allow", method)
		http.Error(w, method+" only", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

// Bound the limiter itself as well as each bucket. Pairing endpoints are public.
func (s *Server) allowDeviceRate(w http.ResponseWriter, r *http.Request, operation string, limit int) bool {
	s.deviceRateMu.Lock()
	defer s.deviceRateMu.Unlock()
	now := time.Now()
	if s.deviceRates == nil {
		s.deviceRates = map[string][]time.Time{}
	}
	for key, ts := range s.deviceRates {
		if len(ts) == 0 || now.Sub(ts[len(ts)-1]) >= time.Minute {
			delete(s.deviceRates, key)
		}
	}
	key := operation + ":" + clientIP(r)
	recent := s.deviceRates[key][:0]
	for _, at := range s.deviceRates[key] {
		if now.Sub(at) < time.Minute {
			recent = append(recent, at)
		}
	}
	if len(recent) >= limit || (len(s.deviceRates) >= 4096 && len(recent) == 0) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many pairing requests", http.StatusTooManyRequests)
		return false
	}
	s.deviceRates[key] = append(recent, now)
	return true
}

func normalizeDeviceCode(code string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))
}

func newDeviceCode() string {
	const alphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	for i := range raw {
		raw[i] = alphabet[int(raw[i])%len(alphabet)]
	}
	return string(raw[:4]) + "-" + string(raw[4:])
}

func (s *Server) handleDevicePairStart(w http.ResponseWriter, r *http.Request) {
	if !deviceMethod(w, r, http.MethodPost) || !s.allowDeviceRate(w, r, "start", 10) {
		return
	}
	var body struct {
		Name     string           `json:"name"`
		Version  string           `json:"version"`
		Hardware []deviceHardware `json:"hardware"`
	}
	if !deviceJSON(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || utf8.RuneCountInString(body.Name) > 80 || len(body.Version) > 80 || len(body.Hardware) > 64 {
		http.Error(w, "device name, version or hardware is invalid", 400)
		return
	}
	seen := map[string]bool{}
	for _, h := range body.Hardware {
		if h.ID == "" || len(h.ID) > 80 || seen[h.ID] || h.Kind == "" || len(h.Kind) > 80 || len(h.Description) > 512 {
			http.Error(w, "invalid hardware declaration", 400)
			return
		}
		seen[h.ID] = true
	}
	if body.Hardware == nil {
		body.Hardware = []deviceHardware{}
	}
	hardware, _ := json.Marshal(body.Hardware)
	now := time.Now()
	if _, err := s.store.dbExecContext(r.Context(), "DELETE FROM device_pairings WHERE expires_at<?", now.Unix()); err != nil {
		http.Error(w, "pairing unavailable", 503)
		return
	}
	pairingID := generateToken(32)
	var code string
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		code = newDeviceCode()
		_, err = s.store.dbExecContext(r.Context(), "INSERT INTO device_pairings(id_hash,user_code,name,version,hardware,expires_at) VALUES(?,?,?,?,?,?)",
			HashAPIKey(pairingID), normalizeDeviceCode(code), body.Name, body.Version, string(hardware), now.Add(devicePairingTTL).Unix())
		if err == nil {
			break
		}
	}
	if err != nil {
		http.Error(w, "pairing unavailable", 503)
		return
	}
	base := strings.TrimRight(requestReachableBaseURL(r, s.publicBaseURL()), "/")
	writeJSON(w, map[string]any{"pairing_id": pairingID, "user_code": code, "verify_url": base + "/settings?tab=devices&code=" + url.QueryEscape(code), "expires_in": int(devicePairingTTL.Seconds()), "interval": 2})
}

func (s *Server) handleDevicePairPoll(w http.ResponseWriter, r *http.Request) {
	if !deviceMethod(w, r, http.MethodPost) || !s.allowDeviceRate(w, r, "poll", 120) {
		return
	}
	var body struct {
		PairingID string `json:"pairing_id"`
	}
	if !deviceJSON(w, r, &body) {
		return
	}
	var status, encrypted, deviceID string
	var expires int64
	err := s.store.dbQueryRowContext(r.Context(), "SELECT status,credential,device_id,expires_at FROM device_pairings WHERE id_hash=?", HashAPIKey(body.PairingID)).Scan(&status, &encrypted, &deviceID, &expires)
	if err != nil || expires <= time.Now().Unix() {
		writeJSON(w, map[string]string{"status": "expired"})
		return
	}
	out := map[string]any{"status": status}
	if status == "approved" {
		var revoked int64
		if err := s.store.dbQueryRowContext(r.Context(), "SELECT revoked_at FROM account_devices WHERE id=?", deviceID).Scan(&revoked); err != nil || revoked != 0 {
			writeJSON(w, map[string]string{"status": "denied"})
			return
		}
		if encrypted != "" {
			token, err := Decrypt(s.secret, encrypted)
			if err != nil {
				http.Error(w, "pairing unavailable", 503)
				return
			}
			out["token"] = token
		}
		out["device_id"] = deviceID
	}
	writeJSON(w, out)
}

func (s *Server) handleDevicePairRequest(w http.ResponseWriter, r *http.Request) {
	if !deviceMethod(w, r, http.MethodGet) || !s.allowDeviceRate(w, r, "lookup:"+strconv.FormatInt(getUserID(r), 10), 20) {
		return
	}
	var name, version, hardware, status string
	var expires int64
	err := s.store.dbQueryRowContext(r.Context(), "SELECT name,version,hardware,status,expires_at FROM device_pairings WHERE user_code=? AND status='pending' AND expires_at>?", normalizeDeviceCode(r.URL.Query().Get("code")), time.Now().Unix()).Scan(&name, &version, &hardware, &status, &expires)
	if err != nil {
		http.Error(w, "pairing code not found or expired", 404)
		return
	}
	writeJSON(w, map[string]any{"name": name, "version": version, "hardware": json.RawMessage(hardware), "status": status, "expires_at": expires})
}

func (s *Server) validateDeviceDefaults(w http.ResponseWriter, r *http.Request, projectID, agentID string) bool {
	if projectID != "" {
		if _, _, ok := s.requireProjectAccess(w, r, projectID, ProjectViewer); !ok {
			return false
		}
	}
	if agentID != "" {
		id, err := strconv.ParseInt(agentID, 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "invalid agent ID", 400)
			return false
		}
		a, ok := s.requireAgentAccess(w, r, id, ProjectViewer)
		if !ok {
			return false
		}
		if a.ProjectID != projectID {
			http.Error(w, "agent does not belong to selected project", 400)
			return false
		}
	}
	return true
}

func (s *Server) handleDevicePairDecision(w http.ResponseWriter, r *http.Request) {
	if !deviceMethod(w, r, http.MethodPost) || !s.allowDeviceRate(w, r, "decision:"+strconv.FormatInt(getUserID(r), 10), 20) {
		return
	}
	var body struct {
		Code      string `json:"code"`
		ProjectID string `json:"project_id"`
		AgentID   string `json:"agent_id"`
	}
	if !deviceJSON(w, r, &body) {
		return
	}
	approve := strings.HasSuffix(r.URL.Path, "/approve")
	if approve && requestDevice(r.Context()) != nil {
		// A paired device must not mint further device credentials.
		http.Error(w, "device credentials cannot approve pairings", 403)
		return
	}
	if approve && !s.validateDeviceDefaults(w, r, body.ProjectID, body.AgentID) {
		return
	}
	tx, err := s.store.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "pairing unavailable", 503)
		return
	}
	defer tx.Rollback()
	var name, version, hardware, status, existingDevice string
	var expires int64
	var owner sql.NullInt64
	err = tx.QueryRow("SELECT name,version,hardware,status,expires_at,user_id,device_id FROM device_pairings WHERE user_code=?", normalizeDeviceCode(body.Code)).Scan(&name, &version, &hardware, &status, &expires, &owner, &existingDevice)
	if err != nil || expires <= time.Now().Unix() {
		http.Error(w, "pairing code not found or expired", 404)
		return
	}
	userID := getUserID(r)
	if status != "pending" {
		if approve && status == "approved" && owner.Int64 == userID {
			writeJSON(w, map[string]string{"status": "approved", "device_id": existingDevice})
			return
		}
		http.Error(w, "pairing request already decided", 409)
		return
	}
	if !approve {
		_, err = tx.Exec("UPDATE device_pairings SET status='denied',user_id=? WHERE user_code=?", userID, normalizeDeviceCode(body.Code))
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			http.Error(w, "pairing unavailable", 503)
			return
		}
		writeJSON(w, map[string]string{"status": "denied"})
		return
	}
	deviceID := "node_" + generateToken(12)
	token := "nd_" + generateToken(32)
	encrypted, err := Encrypt(s.secret, token)
	if err == nil {
		_, err = tx.Exec("INSERT INTO account_devices(id,user_id,name,version,hardware,token_hash,project_id,agent_id,created_at) VALUES(?,?,?,?,?,?,?,?,?)", deviceID, userID, name, version, hardware, HashAPIKey(token), body.ProjectID, body.AgentID, time.Now().Unix())
	}
	if err == nil {
		_, err = tx.Exec("UPDATE device_pairings SET status='approved',user_id=?,device_id=?,credential=? WHERE user_code=?", userID, deviceID, encrypted, normalizeDeviceCode(body.Code))
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		http.Error(w, "pairing unavailable", 503)
		return
	}
	if d, e := s.store.accountDevice(deviceID, userID); e == nil {
		_, _ = s.ensureDeviceMCP(d)
	}
	ev := s.securityEventFromRequest(r, "credential", "device.pair_approve", securityResultSuccess)
	ev.TargetType, ev.TargetID = "device", deviceID
	ev.Severity = securitySeverityNotice
	ev.Details = map[string]any{"name": name, "project_id": body.ProjectID}
	s.recordSecurity(r, ev)
	writeJSON(w, map[string]string{"status": "approved", "device_id": deviceID})
}

func (s *Server) handleAccountDevices(w http.ResponseWriter, r *http.Request) {
	if !deviceMethod(w, r, http.MethodGet) {
		return
	}
	rows, err := s.store.dbQueryContext(r.Context(), "SELECT "+deviceColumns+" FROM account_devices WHERE user_id=? ORDER BY created_at DESC,id", getUserID(r))
	if err != nil {
		http.Error(w, "devices unavailable", 503)
		return
	}
	defer rows.Close()
	devices := []AccountDevice{}
	current := requestDevice(r.Context())
	for rows.Next() {
		d, err := scanAccountDevice(rows)
		if err != nil {
			http.Error(w, "devices unavailable", 503)
			return
		}
		_, d.Connected = s.deviceLinks.Load(d.ID)
		d.Current = current != nil && current.ID == d.ID
		devices = append(devices, *d)
	}
	if rows.Err() != nil {
		http.Error(w, "devices unavailable", 503)
		return
	}
	writeJSON(w, map[string]any{"devices": devices})
}

func (s *Server) handleAccountDevice(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api"), "/devices/")
	parts := strings.Split(id, "/")
	d, err := s.store.accountDevice(parts[0], getUserID(r))
	if err != nil {
		http.Error(w, "device not found", 404)
		return
	}
	if len(parts) > 1 && s.handleDeviceAgentRoutes(w, r, d, parts[1:]) {
		return
	}
	if len(parts) == 2 && parts[1] == "invoke" {
		s.handleDeviceInvoke(w, r, d)
		return
	}
	if len(parts) != 1 {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		_, d.Connected = s.deviceLinks.Load(d.ID)
		writeJSON(w, d)
	case http.MethodPatch:
		var body struct {
			Name      *string `json:"name"`
			ProjectID *string `json:"project_id"`
			AgentID   *string `json:"agent_id"`
		}
		if !deviceJSON(w, r, &body) {
			return
		}
		if d.RevokedAt != 0 {
			http.Error(w, "device revoked", 409)
			return
		}
		if body.Name != nil {
			d.Name = strings.TrimSpace(*body.Name)
			if d.Name == "" || utf8.RuneCountInString(d.Name) > 80 {
				http.Error(w, "invalid device name", 400)
				return
			}
		}
		if body.ProjectID != nil {
			d.ProjectID = *body.ProjectID
			if body.AgentID == nil {
				d.AgentID = ""
			}
		}
		if body.AgentID != nil {
			d.AgentID = *body.AgentID
		}
		if !s.validateDeviceDefaults(w, r, d.ProjectID, d.AgentID) {
			return
		}
		result, err := s.store.dbExecContext(r.Context(), "UPDATE account_devices SET name=?,project_id=?,agent_id=? WHERE id=? AND user_id=? AND revoked_at=0", d.Name, d.ProjectID, d.AgentID, d.ID, d.UserID)
		if err != nil {
			http.Error(w, "device update failed", 503)
			return
		}
		if n, _ := result.RowsAffected(); n == 0 {
			http.Error(w, "device revoked", 409)
			return
		}
		if value, ok := s.deviceLinks.Load(d.ID); ok {
			// Reconnect after changing defaults so selection and in-flight RPCs
			// cannot retain an outdated context.
			value.(*deviceConnection).close("device settings changed")
		}
		_, _ = s.ensureDeviceMCP(d)
		writeJSON(w, d)
	case http.MethodDelete:
		if err := s.store.revokeAccountDevice(d.ID, d.UserID); err != nil {
			http.Error(w, "device removal failed", 503)
			return
		}
		s.closeDeviceLink(d.ID, "device revoked")
		ev := s.securityEventFromRequest(r, "credential", "device.revoke", securityResultSuccess)
		ev.TargetType, ev.TargetID = "device", d.ID
		ev.Severity = securitySeverityNotice
		s.recordSecurity(r, ev)
		d.RevokedAt = time.Now().Unix()
		_, _ = s.ensureDeviceMCP(d)
		_, _ = s.store.dbExecContext(r.Context(), "UPDATE device_event_outbox SET status='canceled',last_error='device revoked' WHERE device_id=? AND status='pending'", d.ID)
		writeJSON(w, map[string]bool{"revoked": true})
	default:
		w.Header().Set("Allow", "GET, PATCH, DELETE")
		http.Error(w, "GET, PATCH or DELETE only", 405)
	}
}

func (s *Server) authenticateAccountDevice(r *http.Request, token string) (*http.Request, error) {
	// Device credentials are accepted only in an explicit header.
	if r.Header.Get("Authorization") != "Bearer "+token && r.Header.Get("X-API-Key") != token {
		return r, fmt.Errorf("device credential requires an authorization header")
	}
	d, err := s.store.accountDeviceForTokenContext(r.Context(), token)
	if err != nil {
		return r, err
	}
	r.Header.Set("X-User-ID", strconv.FormatInt(d.UserID, 10))
	r.Header.Set("X-Apteva-Operator-ID", strconv.FormatInt(d.UserID, 10))
	noteAuditPrincipal(r, "device", strconv.FormatInt(d.UserID, 10), credentialRef("device", token), 0)
	return r.WithContext(context.WithValue(r.Context(), devicePrincipalKey{}, d)), nil
}
