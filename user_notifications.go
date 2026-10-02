package main

// User notifications consume the same durable app-event transaction as agent
// and app subscriptions. No channel/chat dependency, and no sidecar may change
// another user's preferences using its installation token.
import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
)

type notificationDB interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
	Exec(string, ...any) (sql.Result, error)
}
type userNotificationSubscription struct {
	InstallID int64                    `json:"install_id"`
	Type      string                   `json:"type"`
	ProjectID string                   `json:"project_id"`
	Key       string                   `json:"key"` // empty = type defaults; named = resource follow
	Filters   map[string]string        `json:"filters"`
	Channels  sdk.NotificationChannels `json:"channels"`
}
type userNotification struct {
	ID        int64                    `json:"id"`
	InstallID int64                    `json:"install_id"`
	ProjectID string                   `json:"project_id"`
	Type      string                   `json:"type"`
	App       string                   `json:"app"`
	AppName   string                   `json:"app_name"`
	Icon      string                   `json:"icon"`
	IconStyle string                   `json:"icon_style"`
	Title     string                   `json:"title"`
	Body      string                   `json:"body"`
	URL       string                   `json:"url"`
	Group     string                   `json:"group"`
	CreatedAt string                   `json:"created_at"`
	Read      bool                     `json:"read"`
	Channels  sdk.NotificationChannels `json:"channels"`
}
type notificationSource struct {
	InstallID    int64                          `json:"install_id"`
	App          string                         `json:"app"`
	Name         string                         `json:"name"`
	Icon         string                         `json:"icon"`
	IconStyle    string                         `json:"icon_style"`
	Topic        string                         `json:"topic"`
	Definition   sdk.NotificationSpec           `json:"definition"`
	Subscription userNotificationSubscription   `json:"subscription"`
	Follows      []userNotificationSubscription `json:"follows"`
}

func (s *Store) migrateUserNotifications() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS user_notification_preferences (
 user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE, channels TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS user_notification_subscriptions (
 user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 install_id INTEGER NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE,
 type TEXT NOT NULL, project_id TEXT NOT NULL, subscription_key TEXT NOT NULL DEFAULT '',
 filters TEXT NOT NULL DEFAULT '{}', channels TEXT NOT NULL,
 PRIMARY KEY(user_id,install_id,type,project_id,subscription_key));
 CREATE TABLE IF NOT EXISTS user_notifications (
 id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 install_id INTEGER NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE,
 project_id TEXT NOT NULL, type TEXT NOT NULL, event_key TEXT NOT NULL,
 app TEXT NOT NULL, title TEXT NOT NULL, body TEXT NOT NULL, url TEXT NOT NULL, group_key TEXT NOT NULL,
 channels TEXT NOT NULL, created_at TEXT NOT NULL, is_read INTEGER NOT NULL DEFAULT 0, dismissed INTEGER NOT NULL DEFAULT 0,
 UNIQUE(user_id,install_id,type,event_key));
 CREATE INDEX IF NOT EXISTS user_notifications_inbox ON user_notifications(user_id,dismissed,id);
 CREATE TABLE IF NOT EXISTS user_notification_push (
 device_id TEXT NOT NULL REFERENCES mobile_push_subscriptions(id) ON DELETE CASCADE,
 notification_id INTEGER NOT NULL REFERENCES user_notifications(id) ON DELETE CASCADE,
 status TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0, next_attempt INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT '', PRIMARY KEY(device_id,notification_id));`)
	return err
}

func notificationAccess(db notificationDB, uid, install int64, project string) bool {
	var n int
	err := db.QueryRow(`SELECT count(*) FROM app_installs i JOIN users u ON u.id=? WHERE i.id=?
 AND (COALESCE(i.project_id,'')='' OR i.project_id=?)
 AND (u.role='admin' OR (?<>'' AND EXISTS(SELECT 1 FROM project_members pm WHERE pm.project_id=? AND pm.user_id=u.id)) OR (?='' AND i.installed_by=u.id))`, uid, install, project, project, project, project).Scan(&n)
	return err == nil && n == 1
}
func notificationManifest(db notificationDB, install int64) (sdk.Manifest, string, error) {
	var m sdk.Manifest
	var raw, status string
	err := db.QueryRow(`SELECT COALESCE(NULLIF(i.manifest_json,''),a.manifest_json),i.status FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE i.id=?`, install).Scan(&raw, &status)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &m)
	}
	return m, status, err
}
func notificationPreferences(db notificationDB, uid int64) (sdk.NotificationChannels, error) {
	p := sdk.NotificationChannels{InApp: true, Tab: true}
	var raw string
	err := db.QueryRow(`SELECT channels FROM user_notification_preferences WHERE user_id=?`, uid).Scan(&raw)
	if err == sql.ErrNoRows {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	err = json.Unmarshal([]byte(raw), &p)
	return p, err
}
func intersectNotificationChannels(a, b sdk.NotificationChannels) sdk.NotificationChannels {
	return sdk.NotificationChannels{InApp: a.InApp && b.InApp, Tab: a.Tab && b.Tab, Desktop: a.Desktop && b.Desktop, Mobile: a.Mobile && b.Mobile}
}
func anyNotificationChannel(c sdk.NotificationChannels) bool {
	return c.InApp || c.Tab || c.Desktop || c.Mobile
}
func notificationJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
func ensureNotificationDefault(db notificationDB, uid, install int64, project string, n sdk.NotificationSpec) error {
	// Snapshot once. Upgrades must never overwrite a user's choice or quietly
	// change defaults already applied to an existing notification type.
	defaults := sdk.NotificationChannels{InApp: n.Defaults.InApp, Tab: n.Defaults.Tab}
	_, err := db.Exec(`INSERT OR IGNORE INTO user_notification_subscriptions(user_id,install_id,type,project_id,channels) VALUES(?,?,?,?,?)`, uid, install, n.ID, project, notificationJSON(defaults))
	return err
}
func notificationSubscriptions(db notificationDB, uid, install int64, project, kind string) ([]userNotificationSubscription, error) {
	rows, err := db.Query(`SELECT subscription_key,filters,channels FROM user_notification_subscriptions WHERE user_id=? AND install_id=? AND project_id=? AND type=? ORDER BY subscription_key`, uid, install, project, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []userNotificationSubscription{}
	for rows.Next() {
		v := userNotificationSubscription{InstallID: install, ProjectID: project, Type: kind}
		var f, c string
		if err = rows.Scan(&v.Key, &f, &c); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(f), &v.Filters); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(c), &v.Channels); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func notificationValue(data map[string]any, field string) any {
	var v any = data
	for _, part := range strings.Split(field, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[part]
	}
	return v
}
func notificationScalar(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}
func notificationRecipient(n sdk.NotificationSpec, data map[string]any, uid int64) bool {
	if n.Audience == "project" {
		return true
	}
	if n.Audience != "recipients" {
		return false
	}
	value := notificationValue(data, n.RecipientsField)
	want := strconv.FormatInt(uid, 10)
	if list, ok := value.([]any); ok {
		for _, v := range list {
			if notificationScalar(v) == want {
				return true
			}
		}
		return false
	}
	return notificationScalar(value) == want
}
func notificationFiltersMatch(filters map[string]string, data map[string]any, uid int64) bool {
	for k, want := range filters {
		if want == "$me" {
			want = strconv.FormatInt(uid, 10)
		}
		v := notificationValue(data, k)
		if v == nil || notificationScalar(v) != want {
			return false
		}
	}
	return true
}

var notificationPlaceholder = regexp.MustCompile(`\{([^{}]+)\}`)

func renderNotification(template string, data map[string]any, escape bool) string {
	return notificationPlaceholder.ReplaceAllStringFunc(template, func(match string) string {
		v := notificationScalar(notificationValue(data, match[1:len(match)-1]))
		if escape {
			return url.QueryEscape(v)
		}
		r := []rune(v)
		if len(r) > 500 {
			v = string(r[:500])
		}
		return v
	})
}
func notificationLink(app, project string, install int64, template string, data map[string]any) string {
	params := url.Values{}
	if strings.HasPrefix(template, "?") {
		parsed, err := url.ParseQuery(strings.TrimPrefix(renderNotification(template, data, true), "?"))
		if err == nil {
			params = parsed
		}
	}
	params.Set("project_id", project)
	params.Set("install_id", strconv.FormatInt(install, 10))
	return "/apps/" + url.PathEscape(app) + "/page?" + params.Encode()
}

func (s *Server) queueUserNotifications(tx *sql.Tx, ev AppEvent, key string) error {
	m, status, err := notificationManifest(tx, ev.InstallID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if status != "running" {
		return nil
	}
	has := false
	for _, e := range m.Provides.Publishes {
		if e.Notification != nil && eventPatternMatches(e.Name, ev.Topic) {
			has = true
			break
		}
	}
	if !has {
		return nil
	}
	// A corrupt or invalid upgraded manifest must not generate unsafe notifications.
	if err = sdk.ValidateManifest(&m); err != nil {
		return nil
	}
	var data map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(ev.Data)))
	decoder.UseNumber()
	if decoder.Decode(&data) != nil {
		return nil
	}
	rows, err := tx.Query(`SELECT id FROM users WHERE role='admin' OR id IN(SELECT user_id FROM project_members WHERE project_id=?) OR id IN(SELECT installed_by FROM app_installs WHERE id=?)`, ev.ProjectID, ev.InstallID)
	if err != nil {
		return err
	}
	uids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		uids = append(uids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, uid := range uids {
		if !notificationAccess(tx, uid, ev.InstallID, ev.ProjectID) {
			continue
		}
		pref, err := notificationPreferences(tx, uid)
		if err != nil {
			return err
		}
		for _, e := range m.Provides.Publishes {
			n := e.Notification
			if n == nil || !eventPatternMatches(e.Name, ev.Topic) {
				continue
			}
			if err = ensureNotificationDefault(tx, uid, ev.InstallID, ev.ProjectID, *n); err != nil {
				return err
			}
			if !notificationRecipient(*n, data, uid) {
				continue
			}
			subs, err := notificationSubscriptions(tx, uid, ev.InstallID, ev.ProjectID, n.ID)
			if err != nil {
				return err
			}
			channels := sdk.NotificationChannels{}
			blocked := false
			for _, sub := range subs {
				if !notificationFiltersMatch(sub.Filters, data, uid) {
					continue
				}
				// A saved disabled resource follow takes precedence over the broad rule.
				if sub.Key != "" && !anyNotificationChannel(sub.Channels) {
					blocked = true
					break
				}
				channels.InApp = channels.InApp || sub.Channels.InApp
				channels.Tab = channels.Tab || sub.Channels.Tab
				channels.Desktop = channels.Desktop || sub.Channels.Desktop
				channels.Mobile = channels.Mobile || sub.Channels.Mobile
			}
			if blocked {
				continue
			}
			channels = intersectNotificationChannels(channels, pref)
			if !anyNotificationChannel(channels) {
				continue
			}
			group := ""
			if n.GroupBy != "" {
				group = notificationScalar(notificationValue(data, n.GroupBy))
			}
			if group != "" {
				group = fmt.Sprintf("%d:%s:%s:%s", ev.InstallID, ev.ProjectID, n.ID, group)
			} else {
				group = key + ":" + n.ID
			}
			_, err = tx.Exec(`INSERT OR IGNORE INTO user_notifications(user_id,install_id,project_id,type,event_key,app,title,body,url,group_key,channels,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, uid, ev.InstallID, ev.ProjectID, n.ID, key, m.Name, renderNotification(n.Title, data, false), renderNotification(n.Body, data, false), notificationLink(m.Name, ev.ProjectID, ev.InstallID, n.Link, data), group, notificationJSON(channels), time.Now().UTC().Format(time.RFC3339Nano))
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) notificationUser(w http.ResponseWriter, r *http.Request) (int64, bool) {
	uid := getUserID(r)
	// App installation and delegated app identities must not impersonate the
	// installation owner's personal inbox. Browser SDK uses a platform session.
	if uid <= 0 || r.Header.Get("X-Apteva-App-Install-ID") != "" || r.Header.Get("X-Apteva-Subject-Type") != "" {
		http.Error(w, "a platform user session is required", http.StatusForbidden)
		return 0, false
	}
	return uid, true
}
func (s *Server) notificationSources(uid int64, project string, only int64) ([]notificationSource, error) {
	rows, err := s.store.db.Query(`SELECT id FROM app_installs WHERE (COALESCE(project_id,'')='' OR project_id=?) AND status='running'`, project)
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := []notificationSource{}
	for _, id := range ids {
		if only > 0 && id != only {
			continue
		}
		if !notificationAccess(s.store.db, uid, id, project) {
			continue
		}
		m, _, err := notificationManifest(s.store.db, id)
		if err != nil {
			return nil, err
		}
		if sdk.ValidateManifest(&m) != nil {
			continue
		}
		for _, event := range m.Provides.Publishes {
			n := event.Notification
			if n == nil {
				continue
			}
			if err = ensureNotificationDefault(s.store.db, uid, id, project, *n); err != nil {
				return nil, err
			}
			subs, err := notificationSubscriptions(s.store.db, uid, id, project, n.ID)
			if err != nil {
				return nil, err
			}
			v := notificationSource{InstallID: id, App: m.Name, Name: m.DisplayName, Icon: resolveInstalledAppIcon(m.Name, m.Icon, m.Version, id, project), IconStyle: m.IconStyle, Topic: event.Name, Definition: *n, Follows: []userNotificationSubscription{}}
			for _, sub := range subs {
				if sub.Key == "" {
					v.Subscription = sub
				} else {
					v.Follows = append(v.Follows, sub)
				}
			}
			out = append(out, v)
		}
	}
	return out, nil
}
func validateNotificationFilters(n sdk.NotificationSpec, filters map[string]string) bool {
	if len(filters) > len(n.Filters) {
		return false
	}
	for key, value := range filters {
		if len(value) > 256 || value == "" {
			return false
		}
		found := false
		for _, f := range n.Filters {
			if f.Field != key {
				continue
			}
			found = len(f.Options) == 0
			for _, option := range f.Options {
				if option.Value == value {
					found = true
				}
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func (s *Server) handleUserNotifications(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.notificationUser(w, r)
	if !ok {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/notifications")
	fail := func(err error) { http.Error(w, "notification storage unavailable", 500) }
	switch path {
	case "/sources":
		if r.Method != "GET" {
			http.Error(w, "GET required", 405)
			return
		}
		install, _ := strconv.ParseInt(r.URL.Query().Get("install_id"), 10, 64)
		out, err := s.notificationSources(uid, r.URL.Query().Get("project_id"), install)
		if err != nil {
			fail(err)
			return
		}
		writeJSON(w, out)
	case "/delivery-status":
		if r.Method != "GET" {
			http.Error(w, "GET required", 405)
			return
		}
		var pending, failed int
		err := s.store.db.QueryRow(`SELECT COALESCE(SUM(CASE WHEN p.status='pending' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN p.status='failed' THEN 1 ELSE 0 END),0) FROM user_notification_push p JOIN mobile_push_subscriptions d ON d.id=p.device_id WHERE d.user_id=?`, uid).Scan(&pending, &failed)
		if err != nil {
			fail(err)
			return
		}
		writeJSON(w, map[string]any{"pending": pending, "failed": failed})
	case "/preferences":
		if r.Method == "GET" {
			p, err := notificationPreferences(s.store.db, uid)
			if err != nil {
				fail(err)
				return
			}
			writeJSON(w, p)
			return
		}
		if r.Method != "PUT" {
			http.Error(w, "GET or PUT required", 405)
			return
		}
		var p sdk.NotificationChannels
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&p) != nil {
			http.Error(w, "invalid preferences", 400)
			return
		}
		if _, err := s.store.db.Exec(`INSERT INTO user_notification_preferences VALUES(?,?) ON CONFLICT(user_id) DO UPDATE SET channels=excluded.channels`, uid, notificationJSON(p)); err != nil {
			fail(err)
			return
		}
		writeJSON(w, p)
	case "/subscriptions":
		if r.Method != "PUT" && r.Method != "DELETE" {
			http.Error(w, "PUT or DELETE required", 405)
			return
		}
		var sub userNotificationSubscription
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&sub) != nil {
			http.Error(w, "invalid subscription", 400)
			return
		}
		if !notificationAccess(s.store.db, uid, sub.InstallID, sub.ProjectID) {
			http.Error(w, "app or project unavailable", 403)
			return
		}
		if len(sub.Key) > 160 || (sub.Key != "" && !eventTargetKey.MatchString(sub.Key)) {
			http.Error(w, "invalid subscription key", 400)
			return
		}
		if r.Method == "DELETE" { // Explicit reset to app defaults; never used for Unfollow.
			if _, err := s.store.db.Exec(`DELETE FROM user_notification_subscriptions WHERE user_id=? AND install_id=? AND type=? AND project_id=? AND subscription_key=?`, uid, sub.InstallID, sub.Type, sub.ProjectID, sub.Key); err != nil {
				fail(err)
				return
			}
			writeJSON(w, map[string]bool{"ok": true})
			return
		}
		m, _, err := notificationManifest(s.store.db, sub.InstallID)
		if err != nil {
			fail(err)
			return
		}
		var n *sdk.NotificationSpec
		for _, e := range m.Provides.Publishes {
			if e.Notification != nil && e.Notification.ID == sub.Type {
				n = e.Notification
			}
		}
		if n == nil || sdk.ValidateManifest(&m) != nil || !validateNotificationFilters(*n, sub.Filters) || (sub.Key != "" && len(sub.Filters) == 0) {
			http.Error(w, "unknown notification type or invalid filters", 400)
			return
		}
		if sub.Filters == nil {
			sub.Filters = map[string]string{}
		}
		_, err = s.store.db.Exec(`INSERT INTO user_notification_subscriptions VALUES(?,?,?,?,?,?,?) ON CONFLICT(user_id,install_id,type,project_id,subscription_key) DO UPDATE SET filters=excluded.filters,channels=excluded.channels`, uid, sub.InstallID, sub.Type, sub.ProjectID, sub.Key, notificationJSON(sub.Filters), notificationJSON(sub.Channels))
		if err != nil {
			fail(err)
			return
		}
		writeJSON(w, sub)
	case "/stream":
		if r.Method != "GET" {
			http.Error(w, "GET required", 405)
			return
		}
		s.streamUserNotifications(w, r, uid)
	case "", "/":
		if r.Method != "GET" {
			http.Error(w, "GET required", 405)
			return
		}
		before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
		out, err := s.notificationSnapshot(uid, before)
		if err != nil {
			fail(err)
			return
		}
		writeJSON(w, out)
	default:
		id, err := strconv.ParseInt(strings.TrimPrefix(path, "/"), 10, 64)
		if err != nil || id <= 0 {
			http.NotFound(w, r)
			return
		}
		var install int64
		var project, group string
		if s.store.db.QueryRow(`SELECT install_id,project_id,group_key FROM user_notifications WHERE id=? AND user_id=?`, id, uid).Scan(&install, &project, &group) != nil || !notificationAccess(s.store.db, uid, install, project) {
			http.NotFound(w, r)
			return
		}
		if r.Method == "GET" {
			items, err := s.readUserNotifications(uid, 0, id, 1)
			if err != nil {
				fail(err)
				return
			}
			if len(items) == 0 {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, items[0])
			return
		}
		if r.Method != "PATCH" {
			http.Error(w, "GET or PATCH required", 405)
			return
		}
		var input struct {
			Read    *bool `json:"read"`
			Dismiss bool  `json:"dismiss"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&input) != nil {
			http.Error(w, "invalid update", 400)
			return
		}
		read := true
		if input.Read != nil {
			read = *input.Read
		}
		// Snapshot-bounded acknowledgement cannot hide a newer arrival in this group.
		_, err = s.store.db.Exec(`UPDATE user_notifications SET is_read=?,dismissed=CASE WHEN ? THEN 1 ELSE dismissed END WHERE user_id=? AND install_id=? AND project_id=? AND group_key=? AND id<=?`, read, input.Dismiss, uid, install, project, group, id)
		if err != nil {
			fail(err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	}
}

// Same access predicate at list, stream and push time: revocation takes effect
// even for notifications that were created before membership changed.
const notificationReadAccess = ` FROM user_notifications n JOIN app_installs i ON i.id=n.install_id JOIN users u ON u.id=n.user_id WHERE n.user_id=? AND (COALESCE(i.project_id,'')='' OR i.project_id=n.project_id) AND (u.role='admin' OR (n.project_id<>'' AND EXISTS(SELECT 1 FROM project_members pm WHERE pm.project_id=n.project_id AND pm.user_id=u.id)) OR (n.project_id='' AND i.installed_by=u.id))`

func (s *Server) readUserNotifications(uid, before, exact int64, limit int) ([]userNotification, error) {
	query := `SELECT n.id,n.install_id,n.project_id,n.type,n.app,n.title,n.body,n.url,n.group_key,n.created_at,n.is_read,n.channels` + notificationReadAccess
	args := []any{uid}
	if exact > 0 {
		query += " AND n.id=?"
		args = append(args, exact)
	} else {
		query += " AND n.dismissed=0 AND (json_extract(n.channels,'$.in_app')=1 OR json_extract(n.channels,'$.tab')=1 OR json_extract(n.channels,'$.desktop')=1)"
	}
	if before > 0 {
		query += " AND n.id<?"
		args = append(args, before)
	}
	query += " ORDER BY n.id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.store.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	out := []userNotification{}
	for rows.Next() {
		var n userNotification
		var channels string
		if err = rows.Scan(&n.ID, &n.InstallID, &n.ProjectID, &n.Type, &n.App, &n.Title, &n.Body, &n.URL, &n.Group, &n.CreatedAt, &n.Read, &channels); err != nil {
			rows.Close()
			return nil, err
		}
		if err = json.Unmarshal([]byte(channels), &n.Channels); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	pref, err := notificationPreferences(s.store.db, uid)
	if err != nil {
		return nil, err
	}
	cache := map[int64]sdk.Manifest{}
	for i := range out {
		n := &out[i]
		n.Channels = intersectNotificationChannels(n.Channels, pref)
		m, ok := cache[n.InstallID]
		if !ok {
			m, _, err = notificationManifest(s.store.db, n.InstallID)
			if err != nil {
				return nil, err
			}
			cache[n.InstallID] = m
		}
		n.AppName = m.DisplayName
		n.Icon = resolveInstalledAppIcon(m.Name, m.Icon, m.Version, n.InstallID, n.ProjectID)
		n.IconStyle = m.IconStyle
	}
	return out, nil
}
func (s *Server) notificationSnapshot(uid, before int64) (map[string]any, error) {
	items, err := s.readUserNotifications(uid, before, 0, 100)
	if err != nil {
		return nil, err
	}
	pref, err := notificationPreferences(s.store.db, uid)
	if err != nil {
		return nil, err
	}
	var unread, tab int
	err = s.store.db.QueryRow(`SELECT COALESCE(SUM(CASE WHEN json_extract(n.channels,'$.in_app')=1 THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN json_extract(n.channels,'$.tab')=1 THEN 1 ELSE 0 END),0)`+notificationReadAccess+` AND n.dismissed=0 AND n.is_read=0`, uid).Scan(&unread, &tab)
	if err != nil {
		return nil, err
	}
	if !pref.InApp {
		unread = 0
	}
	if !pref.Tab {
		tab = 0
	}
	var next int64
	if len(items) == 100 {
		next = items[len(items)-1].ID
	}
	return map[string]any{"items": items, "unread_count": unread, "tab_count": tab, "next_before": next}, nil
}
func (s *Server) streamUserNotifications(w http.ResponseWriter, r *http.Request, uid int64) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var previous [32]byte
	// Reconnect always sends a full authoritative snapshot, including read and
	// dismissed changes from other devices. This needs no process-local replay ring.
	for {
		snapshot, err := s.notificationSnapshot(uid, 0)
		if err != nil {
			return
		}
		raw, err := json.Marshal(snapshot)
		if err != nil {
			return
		}
		hash := sha256.Sum256(raw)
		if hash != previous {
			if _, err = fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
				return
			}
			previous = hash
		} else {
			if _, err = fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
		}
		flusher.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
