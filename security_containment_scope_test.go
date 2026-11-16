package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A private key carrying the security.containment scope (Fleet's tenant
// control key) may call containment and nothing else privileged.
func TestContainmentScopedKeyUnlocksContainmentOnly(t *testing.T) {
	h := newSecurityHarness(t)
	scopedRaw := "sk-" + generateToken(24)
	scoped, err := h.s.store.CreateAPIKey(h.adminID, "fleet-control", HashAPIKey(scopedRaw), scopedRaw[:11],
		APIKeyCreateOptions{Scopes: `["` + containmentKeyScope + `"]`})
	if err != nil {
		t.Fatal(err)
	}
	plainRaw := "sk-" + generateToken(24)
	if _, err := h.s.store.CreateAPIKey(h.adminID, "plain", HashAPIKey(plainRaw), plainRaw[:11]); err != nil {
		t.Fatal(err)
	}
	victimRaw := "sk-" + generateToken(24)
	if _, err := h.s.store.CreateAPIKey(h.userID, "victim", HashAPIKey(victimRaw), victimRaw[:11]); err != nil {
		t.Fatal(err)
	}
	contain := map[string]any{"user_id": h.userID, "sessions": true, "api_keys": true, "reason": "incident 7"}

	// Unscoped key: refused, and told which scope would work.
	w := h.do(t, harnessReq{method: "POST", path: "/security/containment/revoke", bearer: plainRaw, body: contain})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), containmentKeyScope) {
		t.Fatalf("unscoped key: %d %s", w.Code, w.Body.String())
	}

	// Scoped key: containment works.
	w = h.do(t, harnessReq{method: "POST", path: "/security/containment/revoke", bearer: scopedRaw, body: contain})
	if w.Code != http.StatusOK {
		t.Fatalf("scoped key containment: %d %s", w.Code, w.Body.String())
	}
	if w := h.do(t, harnessReq{method: "GET", path: "/whoami", bearer: victimRaw}); w.Code != http.StatusUnauthorized {
		t.Fatal("containment by scoped key did not revoke the target's key")
	}

	// Scoped key: every other privileged action stays session-only.
	enc, _ := Encrypt(h.s.secret, `{"api_key":"provider-secret-value"}`)
	conn, err := h.s.store.CreateConnection(h.adminID, "openai", "OpenAI", "prod", "api_key", enc, "")
	if err != nil {
		t.Fatal(err)
	}
	if w := h.do(t, harnessReq{method: "GET", path: "/connections/" + itoa(conn.ID) + "/credentials", bearer: scopedRaw}); w.Code != http.StatusForbidden {
		t.Fatalf("scoped key exported a credential: %d", w.Code)
	}
	if w := h.do(t, harnessReq{method: "PATCH", path: "/admin/users/" + itoa(h.userID), bearer: scopedRaw, body: map[string]string{"role": "admin"}}); w.Code != http.StatusForbidden {
		t.Fatalf("scoped key changed a platform role: %d %s", w.Code, w.Body.String())
	}
	for _, action := range []string{"credential.export", "permission.platform_role", "containment.user_disable", "ssh.policy_update"} {
		if privilegedActionKeyScopes[action] != "" {
			t.Fatalf("%s must not accept a key scope", action)
		}
	}

	// A revoked scoped key is refused.
	if _, err := h.s.store.RevokeAPIKey(h.adminID, scoped.ID, h.adminID, "test"); err != nil {
		t.Fatal(err)
	}
	if h.s.apiKeyHasScope(scoped.ID, containmentKeyScope) {
		t.Fatal("revoked key still reports its scope")
	}
	if w := h.do(t, harnessReq{method: "POST", path: "/security/containment/revoke", bearer: scopedRaw, body: contain}); w.Code == http.StatusOK {
		t.Fatal("revoked scoped key was accepted")
	}
}

// Granting the containment scope needs the same proof as using it: a
// stolen session or an unscoped key must not mint a scoped key.
func TestContainmentScopeGrantRequiresStepUp(t *testing.T) {
	h := newSecurityHarness(t)
	grant := map[string]any{"name": "fleet-control", "scopes": []string{containmentKeyScope}}

	plainRaw := "sk-" + generateToken(24)
	if _, err := h.s.store.CreateAPIKey(h.adminID, "plain", HashAPIKey(plainRaw), plainRaw[:11]); err != nil {
		t.Fatal(err)
	}
	w := h.do(t, harnessReq{method: "POST", path: "/auth/keys", bearer: plainRaw, body: grant})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), containmentKeyScope) {
		t.Fatalf("unscoped key minted the scope: %d %s", w.Code, w.Body.String())
	}
	// The same key may still mint ordinary keys.
	if w := h.do(t, harnessReq{method: "POST", path: "/auth/keys", bearer: plainRaw, body: map[string]any{"name": "ordinary"}}); w.Code != http.StatusOK {
		t.Fatalf("ordinary key creation broke: %d %s", w.Code, w.Body.String())
	}

	admin := h.login(t, "admin@example.test", "198.51.100.40:1")
	_, _ = h.s.store.db.Exec(`UPDATE sessions SET reauth_at = ? WHERE token = ?`, time.Now().Add(-time.Hour).UTC().Format("2006-01-02 15:04:05"), sessionKey(admin))
	if w := h.do(t, harnessReq{method: "POST", path: "/auth/keys", cookie: admin, body: grant}); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "reauthentication_required") {
		t.Fatalf("stale session minted the scope: %d %s", w.Code, w.Body.String())
	}
	denied := h.events(t, SecurityEventQuery{Action: "credential.scope_grant", Result: securityResultDenied})
	if len(denied) != 2 {
		t.Fatalf("refused grants must be audited: %+v", denied)
	}

	// A freshly reauthenticated session (e.g. tenant setup) may grant it.
	if w := h.do(t, harnessReq{method: "POST", path: "/auth/reauth", cookie: admin, body: map[string]string{"password": harnessPassword}}); w.Code != http.StatusOK {
		t.Fatalf("reauth: %d", w.Code)
	}
	w = h.do(t, harnessReq{method: "POST", path: "/auth/keys", cookie: admin, body: grant})
	if w.Code != http.StatusOK {
		t.Fatalf("fresh session could not mint the scope: %d %s", w.Code, w.Body.String())
	}
	var first struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &first)

	// A scoped key may mint its own successor (Fleet key rotation)...
	w = h.do(t, harnessReq{method: "POST", path: "/auth/keys", bearer: first.Key, body: grant})
	if w.Code != http.StatusOK {
		t.Fatalf("scoped key could not mint its successor: %d %s", w.Code, w.Body.String())
	}
	var next struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &next)
	// ...and the successor can contain.
	contain := map[string]any{"user_id": h.userID, "sessions": true, "reason": "rotation check"}
	if w := h.do(t, harnessReq{method: "POST", path: "/security/containment/revoke", bearer: next.Key, body: contain}); w.Code != http.StatusOK {
		t.Fatalf("successor key cannot contain: %d %s", w.Code, w.Body.String())
	}

	// A public client key can never carry it.
	if w := h.do(t, harnessReq{method: "POST", path: "/auth/keys", cookie: admin, body: map[string]any{"kind": "public_client", "project_id": "p", "scopes": []string{containmentKeyScope}}}); w.Code == http.StatusOK {
		t.Fatal("public client key received a privileged scope")
	}
}
