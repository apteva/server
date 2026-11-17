package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

type deviceEvent struct {
	ID         string    `json:"event_id,omitempty"`
	Capability string    `json:"capability"`
	Kind       string    `json:"kind"`
	Data       any       `json:"data,omitempty"`
	At         time.Time `json:"at"`
}

var deviceEventIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

var deviceThreadPattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]{0,127}$`)

func validDeviceThreadID(id string) bool { return deviceThreadPattern.MatchString(id) }

// The selected screen agent is intentionally absent from ingestion. Targets are
// captured from subscriptions in the same transaction as the event receipt.
func (s *Server) persistDeviceEvent(d *AccountDevice, frame deviceFrame) (bool, error) {
	var event deviceEvent
	if json.Unmarshal(frame.Data, &event) != nil {
		return false, errors.New("invalid device event")
	}
	if frame.ID != "" {
		if event.ID != "" && event.ID != frame.ID {
			return false, errors.New("event identity mismatch")
		}
		event.ID = frame.ID
	}
	if !deviceEventIDPattern.MatchString(event.ID) || event.At.IsZero() || event.At.After(time.Now().Add(5*time.Minute)) {
		return false, errors.New("event requires a stable ID and valid timestamp")
	}
	declared := []struct {
		Capability string `json:"capability"`
		Kind       string `json:"kind"`
	}{}
	_ = json.Unmarshal(d.Events, &declared)
	known := false
	for _, spec := range declared {
		known = known || (spec.Capability == event.Capability && spec.Kind == event.Kind)
	}
	if !known {
		return false, errors.New("undeclared device event")
	}
	raw, _ := json.Marshal(event)
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	tx, err := s.store.dbBegin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var active int
	if err = tx.QueryRow("SELECT count(*) FROM account_devices WHERE id=? AND revoked_at=0", d.ID).Scan(&active); err != nil {
		return false, err
	}
	if active != 1 {
		return false, errors.New("device revoked")
	}
	var prior string
	err = tx.QueryRow("SELECT payload_hash FROM device_event_receipts WHERE device_id=? AND event_id=?", d.ID, event.ID).Scan(&prior)
	if err == nil {
		if prior != hash {
			return false, errors.New("event ID was reused with a different payload")
		}
		return true, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}
	var pending int
	if err = tx.QueryRow("SELECT count(*) FROM device_event_outbox WHERE device_id=? AND status IN ('pending','sending')", d.ID).Scan(&pending); err != nil {
		return false, err
	}
	if pending >= 10000 {
		return false, errors.New("device event delivery queue is full")
	}
	now := time.Now()
	_, err = tx.Exec("INSERT INTO device_event_receipts(device_id,event_id,payload_hash,event_json,created_at) VALUES(?,?,?,?,?)", d.ID, event.ID, hash, string(raw), now.Unix())
	if err != nil {
		return false, err
	}
	rows, err := tx.Query("SELECT id,agent_id,project_id,thread_id,instruction,cooldown_seconds,max_age_seconds,last_queued FROM device_event_bindings WHERE device_id=? AND enabled=1 AND capability=? AND kind=?", d.ID, event.Capability, event.Kind)
	if err != nil {
		return false, err
	}
	type match struct {
		binding DeviceEventBinding
		last    int64
	}
	matches := []match{}
	for rows.Next() {
		m := match{binding: DeviceEventBinding{DeviceID: d.ID, Capability: event.Capability, Kind: event.Kind, Enabled: true}}
		b := &m.binding
		if err = rows.Scan(&b.ID, &b.AgentID, &b.ProjectID, &b.ThreadID, &b.Instruction, &b.CooldownSeconds, &b.MaxAgeSeconds, &m.last); err != nil {
			rows.Close()
			return false, err
		}
		matches = append(matches, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	for _, m := range matches {
		b := m.binding
		snapshot, _ := json.Marshal(b)
		data, _ := json.Marshal(event.Data)
		message := fmt.Sprintf("[device:%s event:%s.%s event_id:%s]\n%s\nDevice payload: %s", d.ID, event.Capability, event.Kind, event.ID, b.Instruction, data)
		expires := event.At.Add(time.Duration(b.MaxAgeSeconds) * time.Second).Unix()
		status := "pending"
		problem := ""
		if expires <= now.Unix() {
			status = "expired"
			problem = "event exceeded subscription expiry"
		}
		if b.CooldownSeconds > 0 && now.Unix() < m.last+int64(b.CooldownSeconds) {
			status = "suppressed"
			problem = "subscription cooldown"
		}
		_, err = tx.Exec("INSERT INTO device_event_outbox(device_id,event_id,binding_id,binding_json,message,status,expires_at,last_error) VALUES(?,?,?,?,?,?,?,?)", d.ID, event.ID, b.ID, string(snapshot), message, status, expires, problem)
		if err != nil {
			return false, err
		}
		if status == "pending" {
			if _, err = tx.Exec("UPDATE device_event_bindings SET last_queued=? WHERE id=?", now.Unix(), b.ID); err != nil {
				return false, err
			}
		}
	}
	return false, tx.Commit()
}

func (s *Server) runDeviceEvents(ctx context.Context) {
	_, _ = s.store.dbExec("UPDATE device_event_outbox SET status='pending' WHERE status='sending'")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastCleanup := time.Time{}
	for {
		if ctx.Err() != nil {
			return
		}
		s.processDeviceEventBatch(ctx)
		if time.Since(lastCleanup) > time.Hour {
			_, _ = s.store.dbExec("DELETE FROM device_event_receipts WHERE created_at<? AND NOT EXISTS(SELECT 1 FROM device_event_outbox o WHERE o.device_id=device_event_receipts.device_id AND o.event_id=device_event_receipts.event_id AND o.status IN ('pending','sending'))", time.Now().Add(-7*24*time.Hour).Unix())
			lastCleanup = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (s *Server) processDeviceEventBatch(ctx context.Context) {
	now := time.Now().Unix()
	_, _ = s.store.dbExec("UPDATE device_event_outbox SET status='expired',last_error='event exceeded subscription expiry' WHERE status='pending' AND expires_at<=?", now)
	rows, err := s.store.dbQuery("SELECT id FROM device_event_outbox WHERE status='pending' AND next_attempt<=? AND NOT EXISTS(SELECT 1 FROM device_event_outbox earlier WHERE earlier.binding_id=device_event_outbox.binding_id AND earlier.id<device_event_outbox.id AND earlier.status IN ('pending','sending')) ORDER BY id LIMIT 16", now)
	if err != nil {
		return
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	slots := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		slots <- struct{}{}
		wg.Add(1)
		go func(id int64) { defer wg.Done(); defer func() { <-slots }(); s.deliverDeviceEvent(ctx, id) }(id)
	}
	wg.Wait()
}
func (s *Server) deliverDeviceEvent(ctx context.Context, id int64) {
	result, err := s.store.dbExec("UPDATE device_event_outbox SET status='sending',attempts=attempts+1 WHERE id=? AND status='pending' AND next_attempt<=? "+
		"AND NOT EXISTS(SELECT 1 FROM device_event_outbox earlier WHERE earlier.binding_id=device_event_outbox.binding_id AND earlier.id<device_event_outbox.id AND earlier.status IN ('pending','sending'))", id, time.Now().Unix())
	if err != nil {
		return
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return
	}
	var device, eventID, bindingID, snapshot, message string
	var expires int64
	var attempts int
	if err = s.store.dbQueryRow("SELECT device_id,event_id,binding_id,binding_json,message,expires_at,attempts FROM device_event_outbox WHERE id=?", id).Scan(&device, &eventID, &bindingID, &snapshot, &message, &expires, &attempts); err != nil {
		return
	}
	finish := func(status, problem, execution string) {
		_, _ = s.store.dbExec("UPDATE device_event_outbox SET status=?,last_error=?,execution_id=? WHERE id=?", status, problem, execution, id)
	}
	if time.Now().Unix() >= expires {
		finish("expired", "event exceeded subscription expiry", "")
		return
	}
	var captured DeviceEventBinding
	if json.Unmarshal([]byte(snapshot), &captured) != nil {
		finish("failed", "invalid captured subscription", "")
		return
	}
	current, err := s.deviceBinding(bindingID)
	if err != nil && err != sql.ErrNoRows {
		s.retryDeviceEvent(id, attempts, "subscription temporarily unavailable")
		return
	}
	if current == nil || !current.Enabled || current.DeviceID != device || current.AgentID != captured.AgentID || current.ProjectID != captured.ProjectID || current.ThreadID != captured.ThreadID || current.Capability != captured.Capability || current.Kind != captured.Kind {
		finish("canceled", "subscription removed, disabled or retargeted", "")
		return
	}
	var owner int64
	if err = s.store.dbQueryRow("SELECT user_id FROM account_devices WHERE id=?", device).Scan(&owner); err != nil {
		finish("canceled", "device unavailable", "")
		return
	}
	d, err := s.store.accountDevice(device, owner)
	if err != nil {
		s.retryDeviceEvent(id, attempts, "device temporarily unavailable")
		return
	}
	a, err := s.store.GetAgentByID(captured.AgentID)
	if err != nil || !s.canUseDeviceAgent(d, a, captured.ProjectID) {
		finish("canceled", "device or project access revoked", "")
		return
	}
	if s.agents.GetPort(a.ID) == 0 {
		s.retryDeviceEvent(id, attempts, "agent is stopped; waiting until event expiry")
		return
	}
	if captured.ThreadID != "main" {
		exists, e := s.asyncThreadExists(&Subscription{AgentID: a.ID, ThreadID: captured.ThreadID})
		if e != nil {
			s.retryDeviceEvent(id, attempts, "thread temporarily unavailable")
			return
		}
		if !exists {
			finish("canceled", "target thread no longer exists", "")
			return
		}
	}
	deliveryID := "node:" + device + ":" + eventID + ":" + bindingID
	body, _ := json.Marshal(map[string]any{"message": message, "event_id": deliveryID, "thread_id": captured.ThreadID, "track_lifecycle": true})
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(requestCtx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/event", s.agents.GetPort(a.ID)), strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.agents.GetCoreAPIKey(a.ID))
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		s.retryDeviceEvent(id, attempts, "agent delivery unavailable")
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		s.retryDeviceEvent(id, attempts, fmt.Sprintf("agent returned HTTP %d", resp.StatusCode))
		return
	}
	var receipt struct {
		Events struct {
			Executions map[string]string `json:"executions"`
		} `json:"events"`
	}
	if json.Unmarshal(raw, &receipt) != nil || strings.TrimSpace(receipt.Events.Executions[deliveryID]) == "" {
		s.retryDeviceEvent(id, attempts, "agent did not confirm durable event receipt")
		return
	}
	finish("delivered", "", receipt.Events.Executions[deliveryID])
}
func (s *Server) retryDeviceEvent(id int64, attempts int, problem string) {
	delay := time.Duration(1<<min(attempts, 6)) * time.Second
	_, _ = s.store.dbExec("UPDATE device_event_outbox SET status='pending',last_error=?,next_attempt=? WHERE id=?", problem, time.Now().Add(delay).Unix(), id)
}

// A changed schema changes the canonical URL and makes Core reconnect through
// its normal attachment reconciliation. Existing tool grants never expand.
func (s *Server) refreshDeviceToolSchemas(device string) {
	rows, err := s.store.dbQuery("SELECT agent_id FROM device_tool_grants WHERE device_id=?", device)
	if err != nil {
		return
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, aid := range ids {
		d, a, _, err := s.authorizeDeviceToolGrant(device, aid)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://local", nil)
		_ = s.attachDeviceMCP(r, d, a, true)
		cancel()
	}
}
