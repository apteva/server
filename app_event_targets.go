package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	sdk "github.com/apteva/app-sdk"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// App targets share the subscription dispatcher's durable enqueue/retry loop.
// They have their own table because they are owned by installations, not agents.
func (s *Store) migrateAppEventTargets() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS app_event_targets (
 install_id INTEGER NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE,
 project_id TEXT NOT NULL, subscription_key TEXT NOT NULL, source_install_id INTEGER NOT NULL,
 topic TEXT NOT NULL, revision INTEGER NOT NULL, enabled INTEGER NOT NULL,
 PRIMARY KEY(install_id,project_id,subscription_key));
 CREATE INDEX IF NOT EXISTS app_event_targets_source ON app_event_targets(source_install_id,project_id,enabled);
 CREATE TABLE IF NOT EXISTS app_event_receipts (
 source_install_id INTEGER NOT NULL, event_id TEXT NOT NULL, payload_hash TEXT NOT NULL,
 PRIMARY KEY(source_install_id,event_id));
 CREATE TABLE IF NOT EXISTS app_event_target_outbox (
 id INTEGER PRIMARY KEY AUTOINCREMENT, install_id INTEGER NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE,
 project_id TEXT NOT NULL, subscription_key TEXT NOT NULL, revision INTEGER NOT NULL,
 event_id TEXT NOT NULL, event_json TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending',
 attempts INTEGER NOT NULL DEFAULT 0, next_attempt INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL,
 UNIQUE(install_id,project_id,subscription_key,event_id));
 CREATE INDEX IF NOT EXISTS app_event_target_due ON app_event_target_outbox(status,next_attempt);`)
	return err
}

var eventTargetKey = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,160}$`)
var eventTargetTopic = regexp.MustCompile(`^[a-zA-Z0-9_-]+(\.[a-zA-Z0-9_-]+)*(\.\*)?$`)

func (s *Server) handleCallbackEventSubscriptions(w http.ResponseWriter, r *http.Request, parts []string) {
	installID, err := requireInstallID(r)
	if err != nil {
		http.Error(w, err.Error(), 401)
		return
	}
	if !installHasPermission(s, installID, sdk.PermEventsSubscribe) {
		http.Error(w, "missing permission platform.events.subscribe", 403)
		return
	}
	project := r.URL.Query().Get("project_id")
	var sub sdk.AppEventSubscription
	if r.Method == http.MethodPost && len(parts) == 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 16384)).Decode(&sub); err != nil {
			http.Error(w, "invalid subscription", 400)
			return
		}
		project = sub.ProjectID
	}
	project, ok := s.appCallProject(w, installID, project)
	if !ok {
		return
	}
	if project == "" {
		http.Error(w, "project_id required", 400)
		return
	}
	if r.Method == http.MethodGet && len(parts) == 1 && parts[0] == "sources" {
		sources := []sdk.AppEventSource{}
		if s.installedApps != nil {
			for _, inst := range s.installedApps.List() {
				if inst.InstallID == installID || !s.eventSourceAllowed(installID, inst.InstallID, project) {
					continue
				}
				sources = append(sources, sdk.AppEventSource{InstallID: inst.InstallID, App: inst.AppName, Name: inst.Manifest.DisplayName, Events: inst.Manifest.Provides.Publishes})
			}
		}
		writeJSON(w, sources)
		return
	}
	if r.Method == http.MethodDelete && len(parts) == 1 && eventTargetKey.MatchString(parts[0]) {
		_, err = s.store.db.Exec(`UPDATE app_event_targets SET enabled=0,revision=revision+1 WHERE install_id=? AND project_id=? AND subscription_key=?`, installID, project, parts[0])
		if err != nil {
			http.Error(w, "subscription storage unavailable", 500)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	if r.Method != http.MethodPost || len(parts) != 0 {
		http.Error(w, "unsupported subscription route", 405)
		return
	}
	sub.ProjectID = project
	if !eventTargetKey.MatchString(sub.Key) || !eventTargetTopic.MatchString(sub.Topic) || sub.Revision < 1 || sub.SourceInstallID <= 0 || sub.SourceInstallID == installID {
		http.Error(w, "invalid subscription", 400)
		return
	}
	if sub.Enabled && !s.eventSourceAllowed(installID, sub.SourceInstallID, project) {
		http.Error(w, "source install unavailable in this project", 403)
		return
	}
	// Equal revisions are idempotent only when the complete request is unchanged.
	result, err := s.store.db.Exec(`INSERT INTO app_event_targets(install_id,project_id,subscription_key,source_install_id,topic,revision,enabled) VALUES(?,?,?,?,?,?,?)
 ON CONFLICT(install_id,project_id,subscription_key) DO UPDATE SET source_install_id=excluded.source_install_id,topic=excluded.topic,revision=excluded.revision,enabled=excluded.enabled
 WHERE excluded.revision>app_event_targets.revision OR (excluded.revision=app_event_targets.revision AND excluded.source_install_id=app_event_targets.source_install_id AND excluded.topic=app_event_targets.topic AND excluded.enabled=app_event_targets.enabled)`, installID, project, sub.Key, sub.SourceInstallID, sub.Topic, sub.Revision, sub.Enabled)
	if err != nil {
		http.Error(w, "subscription storage unavailable", 500)
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		http.Error(w, "subscription revision conflict", 409)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
func (s *Server) eventSourceAllowed(target, source int64, project string) bool {
	var count int
	err := s.store.db.QueryRow(`SELECT count(*) FROM app_installs a JOIN app_installs b ON a.installed_by=b.installed_by WHERE a.id=? AND b.id=? AND (COALESCE(b.project_id,'')='' OR b.project_id=?)`, target, source, project).Scan(&count)
	return err == nil && count == 1
}

// Called inside the same transaction as ordinary agent subscription deliveries.
func queueAppEventTargets(tx *sql.Tx, ev AppEvent, key string) error {
	rows, err := tx.Query(`SELECT install_id,subscription_key,revision,topic FROM app_event_targets WHERE source_install_id=? AND project_id=? AND enabled=1`, ev.InstallID, ev.ProjectID)
	if err != nil {
		return err
	}
	type target struct {
		install  int64
		key      string
		revision int
		topic    string
	}
	targets := []target{}
	for rows.Next() {
		var t target
		if err = rows.Scan(&t.install, &t.key, &t.revision, &t.topic); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, t := range targets {
		if !eventPatternMatches(t.topic, ev.Topic) {
			continue
		}
		event := sdk.Event{Event: sdk.AppBusDeliveryEvent, SourceApp: ev.App, SourceInstallID: ev.InstallID, ProjectID: ev.ProjectID, Data: map[string]any{"event_id": key, "subscription_key": t.key, "revision": t.revision, "topic": ev.Topic, "occurred_at": ev.Time, "data": json.RawMessage(ev.Data)}}
		raw, err := json.Marshal(event)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO app_event_target_outbox(install_id,project_id,subscription_key,revision,event_id,event_json,created_at) VALUES(?,?,?,?,?,?,?)`, t.install, ev.ProjectID, t.key, t.revision, key, string(raw), time.Now().Unix())
		if err != nil {
			return err
		}
	}
	return nil
}
func appEventHash(ev AppEvent) string {
	var data any
	_ = json.Unmarshal(ev.Data, &data)
	raw, _ := json.Marshal([]any{ev.App, ev.ProjectID, ev.Topic, data})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

var errAppEventConflict = errors.New("event_id was reused with different content")

func (d *AppEventDispatcher) drainAppEventTargets(ctx context.Context) {
	rows, err := d.server.store.db.QueryContext(ctx, `SELECT id FROM app_event_target_outbox WHERE status='pending' AND next_attempt<=? ORDER BY id LIMIT 32`, time.Now().UnixMilli())
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
	parallelJobs(ctx, len(ids), 8, func(i int) { d.deliverAppEventTarget(ctx, ids[i]) })
}
func (d *AppEventDispatcher) deliverAppEventTarget(ctx context.Context, id int64) {
	db := d.server.store.db
	claim, err := db.Exec(`UPDATE app_event_target_outbox SET status='sending' WHERE id=? AND status='pending'`, id)
	if err != nil {
		return
	}
	n, _ := claim.RowsAffected()
	if n != 1 {
		return
	}
	var install int64
	var project, key, raw string
	var revision, attempts int
	err = db.QueryRow(`SELECT install_id,project_id,subscription_key,revision,event_json,attempts FROM app_event_target_outbox WHERE id=?`, id).Scan(&install, &project, &key, &revision, &raw, &attempts)
	if err != nil {
		return
	}
	var enabled, currentRevision int
	err = db.QueryRow(`SELECT enabled,revision FROM app_event_targets WHERE install_id=? AND project_id=? AND subscription_key=?`, install, project, key).Scan(&enabled, &currentRevision)
	if err == sql.ErrNoRows || (err == nil && (enabled == 0 || revision != currentRevision)) {
		db.Exec(`UPDATE app_event_target_outbox SET status='canceled' WHERE id=?`, id)
		return
	}
	if err == nil {
		var event sdk.Event
		err = json.Unmarshal([]byte(raw), &event)
		event.DeliveryID = fmt.Sprintf("app-target-%d", id)
		if err == nil && !d.server.eventSourceAllowed(install, event.SourceInstallID, project) {
			err = errors.New("event source no longer accessible")
		}
		if err == nil {
			err = d.server.postAppEventTarget(ctx, install, event)
		}
	}
	if err == nil {
		db.Exec(`UPDATE app_event_target_outbox SET status='delivered',last_error='' WHERE id=?`, id)
		return
	}
	// Retain and retry pending work with capped backoff, including offline apps.
	attempts++
	db.Exec(`UPDATE app_event_target_outbox SET status='pending',attempts=?,next_attempt=?,last_error=? WHERE id=?`, attempts, time.Now().Add(agentEventDeliveryBackoff(attempts)).UnixMilli(), truncate(err.Error(), 1000), id)
}
func (s *Server) postAppEventTarget(parent context.Context, install int64, event sdk.Event) error {
	var target *InstalledApp
	if s.installedApps != nil {
		for _, inst := range s.installedApps.List() {
			if inst.InstallID == install {
				target = inst
				break
			}
		}
	}
	if target == nil || target.SidecarURL == "" {
		return errors.New("subscriber app is offline")
	}
	if !installHasPermission(s, install, sdk.PermEventsSubscribe) {
		return errors.New("subscriber event permission removed")
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(target.SidecarURL, "/")+"/events", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+target.Token)
	req.Header.Set("X-Apteva-App-Install-ID", fmt.Sprint(install))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("subscriber returned HTTP %d", resp.StatusCode)
	}
	return nil
}
