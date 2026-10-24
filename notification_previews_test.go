package main

import (
	"context"
	"encoding/json"
	sdk "github.com/apteva/app-sdk"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func previewRequest(t *testing.T, s *Server, uid int64, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("X-User-ID", strconv.FormatInt(uid, 10))
	w := httptest.NewRecorder()
	s.handleUserNotifications(w, r)
	return w
}

func TestNotificationPreviewPreferenceIsPrivateAndBackwardCompatible(t *testing.T) {
	s := newTestServer(t)
	u, _ := s.store.CreateUser("preview@test.local", "hash")
	other, _ := s.store.CreateUser("other@test.local", "hash")
	w := previewRequest(t, s, u.ID, "GET", "/notifications/previews", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"hidden"`) {
		t.Fatal(w.Code, w.Body)
	}
	for _, body := range []string{`{"mobile_preview":"anything"}`, `{"mobile_preview":"content","unexpected":true}`, `{"mobile_preview":"content"}{}`} {
		if w := previewRequest(t, s, u.ID, "PUT", "/notifications/previews", body); w.Code != 400 {
			t.Fatal(w.Code, w.Body)
		}
	}
	if w := previewRequest(t, s, u.ID, "PUT", "/notifications/previews", `{"mobile_preview":"content"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	// Older clients still replace only the old channel preference document.
	if w := previewRequest(t, s, u.ID, "PUT", "/notifications/preferences", `{"in_app":true,"mobile":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	p, err := notificationPreviewPreference(s.store.db, u.ID)
	if err != nil || p.MobilePreview != "content" {
		t.Fatal(p, err)
	}
	p, err = notificationPreviewPreference(s.store.db, other.ID)
	if err != nil || p.MobilePreview != "hidden" {
		t.Fatal(p, err)
	}
	r := httptest.NewRequest("PUT", "/notifications/previews", strings.NewReader(`{"mobile_preview":"content"}`))
	r.Header.Set("X-User-ID", strconv.FormatInt(u.ID, 10))
	r.Header.Set("X-Apteva-App-Install-ID", "1")
	w = httptest.NewRecorder()
	s.handleUserNotifications(w, r)
	if w.Code != 403 {
		t.Fatal("app token changed preview policy", w.Code)
	}
	// Running migrations twice must preserve both old preferences and new policy.
	if err := s.store.migrateUserNotifications(); err != nil {
		t.Fatal(err)
	}
	p, _ = notificationPreviewPreference(s.store.db, u.ID)
	if p.MobilePreview != "content" {
		t.Fatal(p)
	}
}

func TestNotificationPreviewDeliveryUsesRenderedContentAndCurrentPolicy(t *testing.T) {
	s, uid := newMobilePushTestServer(t)
	s.store.db.Exec(`UPDATE users SET role='admin' WHERE id=?`, uid)
	relay := &mobilePushRelayRecorder{}
	httpRelay := httptest.NewServer(http.HandlerFunc(relay.handler))
	defer httpRelay.Close()
	t.Setenv("APTEVA_PUSH_RELAY_URL", httpRelay.URL)
	if w := registerMobilePushForTest(t, s, uid, strings.Repeat("a", 64)); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	device, err := s.mobilePushSubscriptionByInstallation("ios-installation-123")
	if err != nil {
		t.Fatal(err)
	}
	install := seedRunningInstall(t, s, "preview-app", "project-mobile", sdk.Manifest{Name: "preview-app", Version: "1.0.0"}, nil)
	if w := previewRequest(t, s, uid, "PUT", "/notifications/preferences", `{"in_app":true,"mobile":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	add := func(key string) int64 {
		r, err := s.store.db.Exec(`INSERT INTO user_notifications(user_id,install_id,project_id,type,event_key,app,title,body,url,group_key,channels,created_at) VALUES(?,?,'project-mobile','test',?,'preview-app','Info in Kimi','Second test alert requested by the user. No action is required.','/apps/preview-app/page','test','{"in_app":true,"mobile":true}',?)`, uid, install, key, time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := r.LastInsertId()
		return id
	}
	add("hidden")
	s.deliverAppNotificationPush(context.Background(), device)
	if len(relay.deliveryBodies) != 1 || relay.deliveryBodies[0]["preview"] != nil {
		t.Fatal("hidden preview leaked", relay.deliveryBodies)
	}
	previewRequest(t, s, uid, "PUT", "/notifications/previews", `{"mobile_preview":"content"}`)
	id := add("visible")
	s.deliverAppNotificationPush(context.Background(), device)
	if len(relay.deliveryBodies) != 2 {
		t.Fatal(relay.deliveryBodies)
	}
	b := relay.deliveryBodies[1]
	p, ok := b["preview"].(map[string]any)
	if !ok || p["title"] != "Info in Kimi" || p["body"] != "Second test alert requested by the user. No action is required." || b["item_id"] != strconv.FormatInt(id, 10) {
		t.Fatal(b)
	}
	// A queued retry must reevaluate disclosure, not retain an old content snapshot.
	relay.deliveryStatus = 503
	add("retry")
	s.deliverAppNotificationPush(context.Background(), device)
	previewRequest(t, s, uid, "PUT", "/notifications/previews", `{"mobile_preview":"hidden"}`)
	s.store.db.Exec(`UPDATE user_notification_push SET next_attempt=0 WHERE status='pending'`)
	relay.deliveryStatus = 0
	s.deliverAppNotificationPush(context.Background(), device)
	if relay.deliveryBodies[len(relay.deliveryBodies)-1]["preview"] != nil {
		t.Fatal("retry ignored privacy change")
	}
	var count int
	s.store.db.QueryRow(`SELECT count(*) FROM user_notifications`).Scan(&count)
	if count != 3 {
		t.Fatal("history changed", count)
	}
	// Revoked project access must suppress text AND delivery, even with previews on.
	previewRequest(t, s, uid, "PUT", "/notifications/previews", `{"mobile_preview":"content"}`)
	relay.rejectPreviews = true
	add("older-relay")
	beforeLegacy := len(relay.deliveryBodies)
	s.deliverAppNotificationPush(context.Background(), device)
	if len(relay.deliveryBodies) != beforeLegacy+2 || relay.deliveryBodies[len(relay.deliveryBodies)-1]["preview"] != nil {
		t.Fatal("legacy relay did not retain generic delivery")
	}
	add("forbidden")
	s.store.db.Exec(`UPDATE users SET role='user' WHERE id=?`, uid)
	s.store.db.Exec(`DELETE FROM project_members WHERE user_id=?`, uid)
	before := len(relay.deliveryBodies)
	s.deliverAppNotificationPush(context.Background(), device)
	if len(relay.deliveryBodies) != before {
		t.Fatal("unauthorized delivery")
	}
}

func TestNotificationPreviewTextBoundsAndFailsClosed(t *testing.T) {
	s := newTestServer(t)
	u, _ := s.store.CreateUser("bounded@test.local", "hash")
	n := userNotification{Title: strings.Repeat("😀", 300), Body: strings.Repeat("秘密\n\x00", 600)}
	if s.notificationPushPreview(u.ID, n) != nil {
		t.Fatal("default was not private")
	}
	previewRequest(t, s, u.ID, "PUT", "/notifications/previews", `{"mobile_preview":"content"}`)
	p := s.notificationPushPreview(u.ID, n)
	if p == nil || len(p.Title) > 240 || len(p.Body) > 1024 || !utf8.ValidString(p.Body) || strings.ContainsAny(p.Body, "\n\x00") {
		t.Fatal(p)
	}
	encoded, _ := json.Marshal(p)
	if !json.Valid(encoded) {
		t.Fatal("invalid JSON")
	}
	s.store.db.Exec(`DROP TABLE user_notification_preview_preferences`)
	if s.notificationPushPreview(u.ID, n) != nil {
		t.Fatal("storage failure leaked content")
	}
}
