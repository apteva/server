package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type deviceTestFixture struct {
	s    *Server
	mux  *http.ServeMux
	user *User
	key  string
}

func newDeviceTest(t *testing.T) deviceTestFixture {
	t.Helper()
	s := newTestServer(t)
	user, err := s.store.CreateUser("node-owner@test.com", "unused")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.store.db.Exec("UPDATE users SET role='user' WHERE id=?", user.ID); err != nil {
		t.Fatal(err)
	}
	key := "device-owner-test-key"
	if _, err = s.store.CreateAPIKey(user.ID, "device test", HashAPIKey(key), "test"); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.registerDeviceRoutes(mux)
	mux.HandleFunc("/auth/me", s.handleMe)
	mux.HandleFunc("/auth/logout", s.handleLogout)
	mux.HandleFunc("/projects", s.authMiddleware(s.handleListProjects))
	mux.HandleFunc("/projects/", s.authMiddleware(s.handleProject))
	mux.HandleFunc("/identity", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"user_id": getUserID(r), "operator": r.Header.Get("X-Apteva-Operator-ID")})
	}))
	return deviceTestFixture{s: s, mux: mux, user: user, key: key}
}
func deviceTestRequest(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func (f deviceTestFixture) start(t *testing.T) (string, string) {
	t.Helper()
	w := deviceTestRequest(t, f.mux, "POST", "/devices/pair/start", "", map[string]any{"name": "Desk CM5", "version": "0.1.0", "hardware": []map[string]string{{"id": "lamp", "kind": "relay"}}})
	if w.Code != 200 {
		t.Fatalf("start: %d", w.Code)
	}
	var out struct {
		ID   string `json:"pairing_id"`
		Code string `json:"user_code"`
		URL  string `json:"verify_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ID == "" || out.Code == "" || !strings.Contains(out.URL, "tab=devices") {
		t.Fatal("missing pairing contract fields")
	}
	return out.ID, out.Code
}
func (f deviceTestFixture) pair(t *testing.T, project, agent string) (string, string) {
	t.Helper()
	id, code := f.start(t)
	w := deviceTestRequest(t, f.mux, "POST", "/devices/pair/approve", f.key, map[string]string{"code": code, "project_id": project, "agent_id": agent})
	if w.Code != 200 {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		DeviceID string `json:"device_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	w = deviceTestRequest(t, f.mux, "POST", "/devices/pair/poll", "", map[string]string{"pairing_id": id})
	var poll struct {
		Status string `json:"status"`
		Token  string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &poll); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || poll.Status != "approved" || !strings.HasPrefix(poll.Token, "nd_") {
		t.Fatal("pairing did not issue a credential")
	}
	return result.DeviceID, poll.Token
}
func TestAccountDevicePairingRetryAndAuthentication(t *testing.T) {
	f := newDeviceTest(t)
	pairID, code := f.start(t)
	poll := func() map[string]any {
		w := deviceTestRequest(t, f.mux, "POST", "/devices/pair/poll", "", map[string]string{"pairing_id": pairID})
		if w.Code != 200 {
			t.Fatalf("poll: %d", w.Code)
		}
		var v map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return v
	}
	if poll()["status"] != "pending" {
		t.Fatal("new request is not pending")
	}
	if w := deviceTestRequest(t, f.mux, "POST", "/devices/pair/approve", "", map[string]string{"code": code}); w.Code != 401 {
		t.Fatal("unauthenticated approval accepted")
	}
	if w := deviceTestRequest(t, f.mux, "GET", "/devices/pair/request?code="+code, f.key, nil); w.Code != 200 {
		t.Fatalf("lookup: %d", w.Code)
	}
	w := deviceTestRequest(t, f.mux, "POST", "/devices/pair/approve", f.key, map[string]string{"code": code})
	if w.Code != 200 {
		t.Fatalf("approve: %d", w.Code)
	}
	a, b := poll(), poll()
	token, _ := a["token"].(string)
	if token == "" || token != b["token"] {
		t.Fatal("lost poll response cannot recover the same credential")
	}
	w = deviceTestRequest(t, f.mux, "POST", "/devices/pair/approve", f.key, map[string]string{"code": code})
	if w.Code != 200 {
		t.Fatalf("approval retry: %d", w.Code)
	}
	var count int
	if err := f.s.store.db.QueryRow("SELECT count(*) FROM account_devices").Scan(&count); err != nil || count != 1 {
		t.Fatalf("approval created duplicate devices: %d %v", count, err)
	}
	var hash, encrypted, idHash string
	if err := f.s.store.db.QueryRow("SELECT token_hash FROM account_devices").Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if err := f.s.store.db.QueryRow("SELECT credential,id_hash FROM device_pairings").Scan(&encrypted, &idHash); err != nil {
		t.Fatal(err)
	}
	if hash != HashAPIKey(token) || encrypted == token || idHash == pairID {
		t.Fatal("credentials stored as plaintext")
	}
	w = deviceTestRequest(t, f.mux, "GET", "/identity", token, nil)
	if w.Code != 200 {
		t.Fatalf("device cannot use regular API: %d", w.Code)
	}
	var identity struct {
		UserID int64 `json:"user_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &identity)
	if identity.UserID != f.user.ID {
		t.Fatal("wrong device principal")
	}
	if w = deviceTestRequest(t, f.mux, "GET", "/auth/me", token, nil); w.Code != 200 {
		t.Fatalf("device profile: %d", w.Code)
	}
	w = deviceTestRequest(t, f.mux, "GET", "/devices", token, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), token) || strings.Contains(w.Body.String(), hash) || strings.Contains(w.Body.String(), encrypted) {
		t.Fatal("device listing exposed credentials or failed")
	}
}

func TestAccountDeviceAccessAcrossProjectsAndIsolation(t *testing.T) {
	f := newDeviceTest(t)
	a, _ := f.s.store.CreateProject(f.user.ID, "A", "", "")
	b, _ := f.s.store.CreateProject(f.user.ID, "B", "", "")
	other, _ := f.s.store.CreateUser("other-node@test.com", "unused")
	foreign, _ := f.s.store.CreateProject(other.ID, "Private", "", "")
	id, token := f.pair(t, a.ID, "")
	for _, p := range []string{a.ID, b.ID} {
		if w := deviceTestRequest(t, f.mux, "GET", "/projects/"+p, token, nil); w.Code != 200 {
			t.Fatalf("account device cannot access another permitted project: %d", w.Code)
		}
	}
	if w := deviceTestRequest(t, f.mux, "GET", "/projects/"+foreign.ID, token, nil); w.Code != 403 {
		t.Fatalf("cross-project access: %d", w.Code)
	}
	if err := f.s.store.AddProjectMember(foreign.ID, f.user.ID, ProjectViewer, other.ID); err != nil {
		t.Fatal(err)
	}
	if w := deviceTestRequest(t, f.mux, "GET", "/projects/"+foreign.ID, token, nil); w.Code != 200 {
		t.Fatal("newly granted project unavailable")
	}
	if w := deviceTestRequest(t, f.mux, "PUT", "/projects/"+foreign.ID, token, map[string]string{"name": "forbidden"}); w.Code != 403 {
		t.Fatal("viewer performed a mutation")
	}
	if err := f.s.store.RemoveProjectMember(foreign.ID, f.user.ID); err != nil {
		t.Fatal(err)
	}
	if w := deviceTestRequest(t, f.mux, "GET", "/projects/"+foreign.ID, token, nil); w.Code != 403 {
		t.Fatal("removed membership retained")
	}
	otherKey := "other-device-test"
	_, _ = f.s.store.CreateAPIKey(other.ID, "other", HashAPIKey(otherKey), "test")
	for _, method := range []string{"GET", "PATCH", "DELETE"} {
		w := deviceTestRequest(t, f.mux, method, "/devices/"+id, otherKey, map[string]string{"name": "stolen"})
		if w.Code != 404 {
			t.Fatalf("cross-user device %s: %d", method, w.Code)
		}
	}
	w := deviceTestRequest(t, f.mux, "POST", "/devices/pair/approve", f.key, map[string]string{"code": "unknown", "project_id": foreign.ID})
	if w.Code != 403 {
		t.Fatalf("unauthorized default project accepted: %d", w.Code)
	}
	// Query credentials and forged identity headers never grant device access.
	r := httptest.NewRequest("GET", "/identity?api_key="+token, nil)
	r.Header.Set("X-User-ID", strconv.FormatInt(other.ID, 10))
	r.Header.Set("X-Apteva-Operator-ID", strconv.FormatInt(other.ID, 10))
	w = httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("query credential or forged user header accepted")
	}
	r = httptest.NewRequest("GET", "/identity", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-User-ID", strconv.FormatInt(other.ID, 10))
	w = httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), fmt.Sprintf("\"user_id\":%d", f.user.ID)) {
		t.Fatal("device principal can be spoofed")
	}
}

func TestAccountDeviceExpiredDeniedAndRevoked(t *testing.T) {
	f := newDeviceTest(t)
	pairID, code := f.start(t)
	_, _ = f.s.store.db.Exec("UPDATE device_pairings SET expires_at=? WHERE id_hash=?", time.Now().Unix()-1, HashAPIKey(pairID))
	if w := deviceTestRequest(t, f.mux, "POST", "/devices/pair/approve", f.key, map[string]string{"code": code}); w.Code != 404 {
		t.Fatal("expired pairing approved")
	}
	w := deviceTestRequest(t, f.mux, "POST", "/devices/pair/poll", "", map[string]string{"pairing_id": pairID})
	if !strings.Contains(w.Body.String(), "expired") {
		t.Fatal("expired pairing not reported")
	}
	pairID, code = f.start(t)
	if w = deviceTestRequest(t, f.mux, "POST", "/devices/pair/deny", f.key, map[string]string{"code": code}); w.Code != 200 {
		t.Fatal("deny failed")
	}
	w = deviceTestRequest(t, f.mux, "POST", "/devices/pair/poll", "", map[string]string{"pairing_id": pairID})
	if !strings.Contains(w.Body.String(), "denied") {
		t.Fatal("denial not reported")
	}
	id, token := f.pair(t, "", "")
	if w = deviceTestRequest(t, f.mux, "DELETE", "/devices/"+id, f.key, nil); w.Code != 200 {
		t.Fatal("revoke failed")
	}
	if w = deviceTestRequest(t, f.mux, "GET", "/identity", token, nil); w.Code != 401 {
		t.Fatal("revoked credential accepted")
	}
	_, token = f.pair(t, "", "")
	if err := f.s.store.DeleteSessionsForUserExcept(f.user.ID, "retained-browser"); err != nil {
		t.Fatal(err)
	}
	if w = deviceTestRequest(t, f.mux, "GET", "/identity", token, nil); w.Code != 401 {
		t.Fatal("session sweep left device authenticated")
	}
	_, token = f.pair(t, "", "")
	if w = deviceTestRequest(t, f.mux, "POST", "/auth/logout", token, nil); w.Code != 200 {
		t.Fatal("device logout failed")
	}
	if w = deviceTestRequest(t, f.mux, "GET", "/identity", token, nil); w.Code != 401 {
		t.Fatal("device logout left credential valid")
	}
}
func deviceReadFrame(t *testing.T, conn *websocket.Conn, typ string) deviceFrame {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var f deviceFrame
		if err := conn.ReadJSON(&f); err != nil {
			t.Fatalf("read %s: %v", typ, err)
		}
		if f.Type == typ {
			return f
		}
	}
}
func deviceWriteFrame(t *testing.T, conn *websocket.Conn, typ, id string, data any) {
	t.Helper()
	raw, _ := json.Marshal(data)
	if err := conn.WriteJSON(deviceFrame{Type: typ, ID: id, Data: raw}); err != nil {
		t.Fatal(err)
	}
}
func deviceConnect(t *testing.T, base, token string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/devices/link", http.Header{"Authorization": []string{"Bearer " + token}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	deviceWriteFrame(t, conn, "hello", "", map[string]any{"protocol": "1", "name": "Desk", "version": "test", "tools": []map[string]any{{"name": "lamp_set", "input_schema": map[string]string{"type": "object"}, "confirm": "hold"}}})
	deviceReadFrame(t, conn, "welcome")
	deviceReadFrame(t, conn, "agents")
	return conn
}

func TestAccountDeviceWebsocketSelectionReplacementAndRevocation(t *testing.T) {
	f := newDeviceTest(t)
	a, _ := f.s.store.CreateProject(f.user.ID, "A", "", "")
	b, _ := f.s.store.CreateProject(f.user.ID, "B", "", "")
	agentA, _ := f.s.store.CreateAgent(f.user.ID, "Agent A", "", "autonomous", "{}", a.ID)
	agentB, _ := f.s.store.CreateAgent(f.user.ID, "Agent B", "", "autonomous", "{}", b.ID)
	other, _ := f.s.store.CreateUser("foreign-agent@test.com", "unused")
	private, _ := f.s.store.CreateProject(other.ID, "Foreign", "", "")
	foreignAgent, _ := f.s.store.CreateAgent(other.ID, "Foreign", "", "autonomous", "{}", private.ID)
	id, token := f.pair(t, a.ID, strconv.FormatInt(agentA.ID, 10))
	server := httptest.NewServer(f.mux)
	defer server.Close()
	conn := deviceConnect(t, server.URL, token)
	deviceWriteFrame(t, conn, "select_agent", "switch", map[string]string{"agent_id": strconv.FormatInt(agentB.ID, 10)})
	welcome := deviceReadFrame(t, conn, "welcome")
	var selected struct {
		Project string `json:"project_id"`
		Agent   string `json:"agent_id"`
	}
	_ = json.Unmarshal(welcome.Data, &selected)
	if selected.Project != b.ID || selected.Agent != strconv.FormatInt(agentB.ID, 10) || welcome.ID != "switch" {
		t.Fatal("selection did not change account-wide context")
	}
	deviceWriteFrame(t, conn, "select_agent", "forbidden", map[string]string{"agent_id": strconv.FormatInt(foreignAgent.ID, 10)})
	if deviceReadFrame(t, conn, "error").ID != "forbidden" {
		t.Fatal("unauthorized selection not rejected")
	}
	w := deviceTestRequest(t, f.mux, "GET", "/devices", f.key, nil)
	if !strings.Contains(w.Body.String(), "\"connected\":true") {
		t.Fatal("connected status missing")
	}
	replacement := deviceConnect(t, server.URL, token)
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var ignored deviceFrame
	if err := conn.ReadJSON(&ignored); err == nil {
		t.Fatal("old connection survived replacement")
	}
	if w = deviceTestRequest(t, f.mux, "DELETE", "/devices/"+id, f.key, nil); w.Code != 200 {
		t.Fatal("revocation failed")
	}
	_ = replacement.SetReadDeadline(time.Now().Add(time.Second))
	err := replacement.ReadJSON(&ignored)
	if !websocket.IsCloseError(err, 4003) {
		t.Fatalf("revocation did not close socket immediately: %v", err)
	}
	_, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/devices/link", http.Header{"Authorization": []string{"Bearer " + token}})
	if err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatal("revoked device reconnected")
	}
}
func TestAccountDeviceWebsocketInvokeAndCancellation(t *testing.T) {
	f := newDeviceTest(t)
	id, token := f.pair(t, "", "")
	server := httptest.NewServer(f.mux)
	defer server.Close()
	conn := deviceConnect(t, server.URL, token)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- deviceTestRequest(t, f.mux, "POST", "/devices/"+id+"/invoke", f.key, map[string]any{"tool": "lamp_set", "args": map[string]bool{"on": true}})
	}()
	invoke := deviceReadFrame(t, conn, "invoke")
	if invoke.ID == "" {
		t.Fatal("uncorrelated invocation")
	}
	deviceWriteFrame(t, conn, "result", invoke.ID, map[string]any{"ok": false, "denied": true, "error": "local confirmation denied"})
	select {
	case w := <-done:
		if w.Code != 200 || !strings.Contains(w.Body.String(), "\"denied\":true") {
			t.Fatal("local denial lost")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("result not forwarded")
	}
	go func() {
		done <- deviceTestRequest(t, f.mux, "POST", "/devices/"+id+"/invoke", f.key, map[string]any{"tool": "lamp_set", "args": map[string]bool{"on": true}})
	}()
	deviceReadFrame(t, conn, "invoke")
	f.s.closeDeviceLink(id, "device revoked")
	select {
	case w := <-done:
		if w.Code != 502 {
			t.Fatalf("pending call status: %d", w.Code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending call not cancelled")
	}
}
func TestAccountDevicePairingRateAndMalformedHello(t *testing.T) {
	f := newDeviceTest(t)
	for i := 0; i < 10; i++ {
		f.start(t)
	}
	if w := deviceTestRequest(t, f.mux, "POST", "/devices/pair/start", "", map[string]string{"name": "overflow"}); w.Code != 429 {
		t.Fatal("pairing start is not limited")
	}
	// Fresh fixture so its independent limiter is not exhausted.
	f = newDeviceTest(t)
	_, token := f.pair(t, "", "")
	server := httptest.NewServer(f.mux)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/devices/link", http.Header{"Authorization": []string{"Bearer " + token}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	deviceWriteFrame(t, conn, "hello", "", map[string]string{"protocol": "99"})
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, _, err = conn.ReadMessage()
	if !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatal("unsupported protocol accepted")
	}
}

func TestAccountDeviceStorePersistsCredential(t *testing.T) {
	f := newDeviceTest(t)
	id, token := f.pair(t, "", "")
	db, err := NewStore(f.s.store.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	d, err := db.accountDeviceForToken(token)
	if err != nil || d.ID != id {
		t.Fatal("credential did not survive store reopen")
	}
}

func TestAccountDeviceWebsocketAppProxyUsesUserProjectAndRole(t *testing.T) {
	f := newDeviceTest(t)
	project, _ := f.s.store.CreateProject(f.user.ID, "Widgets", "", "")
	other, _ := f.s.store.CreateUser("widget-other@test.com", "unused")
	foreign, _ := f.s.store.CreateProject(other.ID, "Private widgets", "", "")
	_, token := f.pair(t, project.ID, "")
	f.s.installedApps = NewInstalledAppsRegistry()
	calls := 0
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer sidecar-test" || r.Header.Get("X-Apteva-Operator-ID") != strconv.FormatInt(f.user.ID, 10) || r.Header.Get("X-Apteva-Project-ID") != project.ID {
			t.Error("proxy lost validated identity")
		}
		if strings.Contains(r.Header.Get("X-Apteva-Original-Authorization"), token) {
			t.Error("device credential forwarded to app")
		}
		writeJSON(w, map[string]bool{"ok": true})
	}))
	defer sidecar.Close()
	f.s.installedApps.Add(&InstalledApp{InstallID: 913, AppName: "widget-app", SidecarURL: sidecar.URL, Token: "sidecar-test"})
	server := httptest.NewServer(f.mux)
	defer server.Close()
	conn := deviceConnect(t, server.URL, token)
	send := func(requestID, projectID, method string) int {
		t.Helper()
		deviceWriteFrame(t, conn, "app_request", requestID, map[string]any{"app": "widget-app", "path": "/summary", "method": method, "project_id": projectID})
		frame := deviceReadFrame(t, conn, "app_response")
		var response struct {
			Status int `json:"status"`
		}
		if err := json.Unmarshal(frame.Data, &response); err != nil {
			t.Fatal(err)
		}
		if frame.ID != requestID {
			t.Fatal("lost request correlation")
		}
		return response.Status
	}
	if status := send("allowed", project.ID, "GET"); status != 200 {
		t.Fatalf("app request: %d", status)
	}
	if status := send("foreign", foreign.ID, "GET"); status != 403 {
		t.Fatalf("foreign app request: %d", status)
	}
	if err := f.s.store.AddProjectMember(foreign.ID, f.user.ID, ProjectViewer, other.ID); err != nil {
		t.Fatal(err)
	}
	if status := send("viewer-write", foreign.ID, "POST"); status != 403 {
		t.Fatalf("viewer mutation: %d", status)
	}
	if calls != 1 {
		t.Fatalf("unauthorized requests reached app: %d", calls)
	}
}

func TestAccountDeviceProfileExplicitCredentialOverridesCookie(t *testing.T) {
	f := newDeviceTest(t)
	_, token := f.pair(t, "", "")
	other, _ := f.s.store.CreateUser("cookie-owner@test.com", "unused")
	if err := f.s.store.CreateSession("ambient-cookie", other.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, credential := range []string{token, "nd_invalid"} {
		r := httptest.NewRequest("GET", "/auth/me", nil)
		r.AddCookie(&http.Cookie{Name: cookieName, Value: "ambient-cookie"})
		r.Header.Set("Authorization", "Bearer "+credential)
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r)
		if credential == "nd_invalid" {
			if w.Code != 401 {
				t.Fatal("invalid device credential fell back to cookie")
			}
		} else {
			var profile struct {
				ID int64 `json:"user_id"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &profile); err != nil {
				t.Fatal(err)
			}
			if w.Code != 200 || profile.ID != f.user.ID {
				t.Fatal("ambient cookie replaced device identity")
			}
		}
	}
}

func TestAccountDeviceCancelledRequestCancelsOnlyItsInvocation(t *testing.T) {
	f := newDeviceTest(t)
	id, token := f.pair(t, "", "")
	server := httptest.NewServer(f.mux)
	defer server.Close()
	conn := deviceConnect(t, server.URL, token)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest("POST", "/devices/"+id+"/invoke", strings.NewReader("{\"tool\":\"lamp_set\",\"args\":{\"on\":true}}")).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+f.key)
	done := make(chan struct{})
	go func() { defer close(done); f.mux.ServeHTTP(httptest.NewRecorder(), request) }()
	invoke := deviceReadFrame(t, conn, "invoke")
	cancel()
	cancelled := deviceReadFrame(t, conn, "cancel_invoke")
	if cancelled.ID != invoke.ID {
		t.Fatal("cancellation lost invocation identity")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled request stayed pending")
	}
	// The connection remains usable after cancelling one call.
	deviceWriteFrame(t, conn, "select_project", "still-connected", map[string]string{"project_id": ""})
	if deviceReadFrame(t, conn, "welcome").ID != "still-connected" {
		t.Fatal("cancellation disconnected the device")
	}
}
