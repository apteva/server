package main

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// A separate outbox preserves legacy inbox push cursors. One receipt per device
// and notification makes delivery retryable across server restarts. The relay
// receives only opaque IDs and generic copy; notification content stays here.
func (s *Server) deliverAppNotificationPush(ctx context.Context, device *mobilePushSubscription) {
	expires, err := time.Parse(time.RFC3339Nano, device.GrantExpiresAt)
	if err != nil || !expires.After(time.Now()) {
		return
	}
	pref, err := notificationPreferences(s.store.db, device.UserID)
	if err != nil || !pref.Mobile {
		return
	}
	_, err = s.store.db.Exec(`INSERT OR IGNORE INTO user_notification_push(device_id,notification_id)
 SELECT ?,n.id FROM user_notifications n WHERE n.user_id=? AND n.is_read=0 AND n.dismissed=0
 AND json_extract(n.channels,'$.mobile')=1 AND datetime(n.created_at)>=datetime(?)
 AND datetime(n.created_at)>datetime('now','-7 days')`, device.ID, device.UserID, device.CreatedAt)
	if err != nil {
		return
	}
	rows, err := s.store.db.Query(`SELECT notification_id,attempts FROM user_notification_push WHERE device_id=? AND status='pending' AND next_attempt<=? ORDER BY notification_id LIMIT 50`, device.ID, time.Now().Unix())
	if err != nil {
		return
	}
	type pending struct {
		id       int64
		attempts int
	}
	jobs := []pending{}
	for rows.Next() {
		var j pending
		if rows.Scan(&j.id, &j.attempts) == nil {
			jobs = append(jobs, j)
		}
	}
	rows.Close()
	grant, err := Decrypt(s.secret, device.RelayGrantEncrypted)
	if err != nil {
		return
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		cancel := func() {
			s.store.db.Exec(`UPDATE user_notification_push SET status='canceled' WHERE device_id=? AND notification_id=?`, device.ID, job.id)
		}
		// Current read state, access and global preferences are checked at send time.
		items, err := s.readUserNotifications(device.UserID, 0, job.id, 1)
		if err != nil {
			return
		}
		if len(items) == 0 {
			cancel()
			continue
		}
		n := items[0]
		var dismissed bool
		if s.store.db.QueryRow(`SELECT dismissed FROM user_notifications WHERE id=?`, n.ID).Scan(&dismissed) != nil {
			return
		}
		if n.Read || dismissed || !n.Channels.Mobile {
			cancel()
			continue
		}
		var badge int
		if err = s.store.db.QueryRow(`SELECT count(*)`+notificationReadAccess+` AND n.dismissed=0 AND n.is_read=0 AND json_extract(n.channels,'$.mobile')=1`, device.UserID).Scan(&badge); err != nil {
			return
		}
		if badge > 9999 {
			badge = 9999
		}
		var result struct {
			Status string `json:"status"`
		}
		err = s.mobilePushRelayRequest(ctx, "POST", "/v1/deliveries", grant, map[string]any{
			"device_id": device.RelayDeviceID, "type": "notification", "item_id": strconv.FormatInt(n.ID, 10), "project_id": n.ProjectID, "badge": badge,
			"idempotency_key": fmt.Sprintf("%s:%s:notification:%d", s.mobilePushInstanceRef(), device.ID, n.ID),
		}, &result)
		if err == nil && result.Status != "sent" && result.Status != "delivered" {
			err = fmt.Errorf("push relay delivery status: %s", result.Status)
		}
		if err != nil {
			attempts := job.attempts + 1
			delay := time.Duration(1<<min(attempts, 10)) * time.Second
			state := "pending"
			if attempts >= 15 {
				state = "failed"
			}
			s.store.db.Exec(`UPDATE user_notification_push SET status=?,attempts=?,next_attempt=?,last_error=? WHERE device_id=? AND notification_id=?`, state, attempts, time.Now().Add(delay).Unix(), truncateMobilePushError(err.Error()), device.ID, n.ID)
			// A pre-upgrade relay may not support this type yet. Never invalidate a
			// valid device grant just because the relay needs the additive update.
			continue
		}
		s.store.db.Exec(`UPDATE user_notification_push SET status='sent',last_error='' WHERE device_id=? AND notification_id=?`, device.ID, n.ID)
	}
}
