package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

type notificationPreviewPreferences struct {
	MobilePreview string `json:"mobile_preview"`
}

type notificationPushPreview struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

func notificationPreviewPreference(db notificationDB, uid int64) (notificationPreviewPreferences, error) {
	p := notificationPreviewPreferences{MobilePreview: "hidden"}
	err := db.QueryRow(`SELECT mobile_preview FROM user_notification_preview_preferences WHERE user_id=?`, uid).Scan(&p.MobilePreview)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	return p, err
}

func (s *Server) handleNotificationPreviews(w http.ResponseWriter, r *http.Request, uid int64) {
	if r.Method == http.MethodGet {
		p, err := notificationPreviewPreference(s.store.db, uid)
		if err != nil {
			http.Error(w, "notification settings unavailable", 500)
			return
		}
		writeJSON(w, p)
		return
	}
	if r.Method != http.MethodPut {
		http.Error(w, "GET or PUT required", 405)
		return
	}
	var p notificationPreviewPreferences
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&p) != nil || decoder.Decode(&struct{}{}) != io.EOF || (p.MobilePreview != "hidden" && p.MobilePreview != "content") {
		http.Error(w, "mobile_preview must be hidden or content", 400)
		return
	}
	_, err := s.store.db.Exec(`INSERT INTO user_notification_preview_preferences(user_id,mobile_preview) VALUES(?,?) ON CONFLICT(user_id) DO UPDATE SET mobile_preview=excluded.mobile_preview`, uid, p.MobilePreview)
	if err != nil {
		http.Error(w, "could not save notification settings", 500)
		return
	}
	writeJSON(w, p)
}

// Notification templates are plain text. Collapse control characters and bound
// UTF-8 before sending any user-authorized preview beyond the originating server.
func notificationPreviewText(value string, maximum int) string {
	value = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)), " ")
	if len(value) <= maximum {
		return value
	}
	value = value[:maximum-3]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return strings.TrimSpace(value) + "…"
}

func (s *Server) notificationPushPreview(uid int64, n userNotification) *notificationPushPreview {
	p, err := notificationPreviewPreference(s.store.db, uid)
	if err != nil || p.MobilePreview != "content" {
		return nil
	} // Fail closed.
	title := notificationPreviewText(n.Title, 240)
	if title == "" {
		title = "Apteva"
	}
	return &notificationPushPreview{Title: title, Body: notificationPreviewText(n.Body, 1024)}
}
