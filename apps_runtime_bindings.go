package main

// Apps opt in through an authenticated POST /_runtime/bindings declaration.
// Effective bindings stay authorized until the app reports its removed
// resources drained. No provider or app-specific logic belongs in the platform.
import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/apteva/app-sdk"
)

const liveBindingsPath = "/_runtime/bindings"

func supportsLiveBindings(m *sdk.Manifest) bool {
	for _, r := range m.Provides.HTTPRoutes {
		if r.Prefix == liveBindingsPath && r.Method == http.MethodPost && !r.NoAuth {
			return true
		}
	}
	return false
}
func (s *Server) bindingUpdateLock(id int64) *sync.Mutex {
	v, _ := s.liveBindingLocks.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}
func (s *Server) ensureLiveBindingTable() error {
	_, err := s.store.db.Exec(`CREATE TABLE IF NOT EXISTS app_runtime_binding_updates (
 install_id INTEGER PRIMARY KEY REFERENCES app_installs(id) ON DELETE CASCADE,
 change_id TEXT NOT NULL,
 previous_json TEXT NOT NULL,
 desired_json TEXT NOT NULL,
 phase TEXT NOT NULL DEFAULT 'prepare',
 last_error TEXT NOT NULL DEFAULT ''
 )`)
	return err
}

type liveBindingChange struct {
	ID                                 int64
	ChangeID, Previous, Desired, Phase string
}
type liveBindingReply struct {
	Ready        bool    `json:"ready"`
	ChangeID     string  `json:"change_id"`
	RetainedWork int     `json:"retained_work"`
	Draining     []int64 `json:"draining_connections"`
}
type liveBindingDenied struct{ Status int }

func (e *liveBindingDenied) Error() string {
	return fmt.Sprintf("app declined live binding update (HTTP %d)", e.Status)
}

func (s *Server) callLiveBindings(change liveBindingChange, phase string) (*liveBindingReply, error) {
	var previous, desired map[string]any
	if json.Unmarshal([]byte(change.Previous), &previous) != nil || json.Unmarshal([]byte(change.Desired), &desired) != nil {
		return nil, errors.New("invalid saved bindings")
	}
	payload, _ := json.Marshal(map[string]any{"phase": phase, "change_id": change.ChangeID, "previous": previous, "desired": desired})
	if s.installedApps == nil {
		return nil, errors.New("app runtime unavailable")
	}
	runtime := s.installedApps.Get(change.ID)
	if runtime == nil || runtime.SidecarURL == "" || runtime.Token == "" {
		return nil, errors.New("app runtime unavailable")
	}
	req, err := http.NewRequest(http.MethodPost, runtime.SidecarURL+liveBindingsPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, []byte(runtime.Token))
	mac.Write(payload)
	req.Header.Set("Authorization", "Bearer "+runtime.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Apteva-Runtime-Signature", hex.EncodeToString(mac.Sum(nil)))
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("app runtime request failed")
	}
	defer response.Body.Close()
	if response.StatusCode == 400 || response.StatusCode == 403 || response.StatusCode == 404 || response.StatusCode == 405 || response.StatusCode == 422 {
		return nil, &liveBindingDenied{response.StatusCode}
	}
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("app runtime HTTP %d", response.StatusCode)
	}
	var result liveBindingReply
	if json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result) != nil || result.ChangeID != change.ChangeID {
		return nil, errors.New("invalid app runtime acknowledgement")
	}
	return &result, nil
}

// Caller holds the install's update mutex, shared with the bindings editor.
func (s *Server) advanceLiveBindings(change liveBindingChange) (*liveBindingReply, error) {
	result, err := s.callLiveBindings(change, change.Phase)
	if err != nil {
		return nil, err
	}
	if change.Phase == "prepare" {
		if !result.Ready {
			effective, err := liveEffectiveBindings(change)
			if err != nil {
				return nil, err
			}
			if _, err = s.store.db.Exec(`UPDATE app_installs SET integration_bindings=? WHERE id=? AND COALESCE(integration_bindings,'{}')<>?`, effective, change.ID, effective); err != nil {
				return nil, err
			}
			return result, nil
		}
		// Change authorization atomically with the durable commit phase. If the
		// reply is lost, resume commit rather than preparing against new bindings.
		tx, err := s.store.db.Begin()
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		if _, err = tx.Exec(`UPDATE app_installs SET integration_bindings=?,has_pending_options=0 WHERE id=?`, change.Desired, change.ID); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(`UPDATE app_runtime_binding_updates SET phase='commit',last_error='' WHERE install_id=? AND change_id=?`, change.ID, change.ChangeID); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		result, err = s.callLiveBindings(change, "commit")
		if err != nil {
			return nil, err
		}
	}
	if !result.Ready {
		return nil, errors.New("app is not ready to commit")
	}
	s.cleanupInactiveIntegrationWebhooks(change.ID, false)
	_, err = s.store.db.Exec(`DELETE FROM app_runtime_binding_updates WHERE install_id=? AND change_id=?`, change.ID, change.ChangeID)
	if err == nil {
		s.recomputePendingOptions()
	}
	return result, err
}

func (s *Server) setLiveBindings(w http.ResponseWriter, id int64, previous, desired map[string]any) {
	if err := s.ensureLiveBindingTable(); err != nil {
		http.Error(w, "runtime update storage unavailable", 500)
		return
	}
	var existing int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_runtime_binding_updates WHERE install_id=?`, id).Scan(&existing); err != nil {
		http.Error(w, "runtime update storage unavailable", 500)
		return
	}
	if existing > 0 {
		http.Error(w, "a binding change is already draining; wait for completion", 409)
		return
	}
	before, _ := json.Marshal(previous)
	after, _ := json.Marshal(desired)
	change := liveBindingChange{id, generateToken(16), string(before), string(after), "prepare"}
	_, err := s.store.db.Exec(`INSERT INTO app_runtime_binding_updates(install_id,change_id,previous_json,desired_json) VALUES(?,?,?,?)`, id, change.ChangeID, change.Previous, change.Desired)
	if err != nil {
		http.Error(w, "save runtime change failed", 500)
		return
	}
	result, err := s.advanceLiveBindings(change)
	var denial *liveBindingDenied
	if errors.As(err, &denial) {
		// Prepare denials guarantee no admission changes. Never restart as a
		// fallback. Commit denials keep the durable entry and old drained state.
		var phase string
		_ = s.store.db.QueryRow(`SELECT phase FROM app_runtime_binding_updates WHERE install_id=?`, id).Scan(&phase)
		if phase == "prepare" {
			_, _ = s.store.db.Exec(`DELETE FROM app_runtime_binding_updates WHERE install_id=?`, id)
			http.Error(w, err.Error(), 409)
			return
		}
	}
	var pending int
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM app_runtime_binding_updates WHERE install_id=?`, id).Scan(&pending)
	response := map[string]any{"ok": true, "bindings": bindingsForInstall(s, id), "desired_bindings": desired, "change_id": change.ChangeID, "pending": pending > 0, "respawned": false, "respawn_err": ""}
	if result != nil {
		response["draining_connections"] = result.Draining
		response["retained_work"] = result.RetainedWork
	}
	if err != nil {
		response["runtime_warning"] = err.Error()
	}
	if pending > 0 {
		s.startLiveBindingWorker(id)
	}
	writeJSON(w, response)
}

func (s *Server) startLiveBindingWorker(id int64) {
	if _, loaded := s.liveBindingWorkers.LoadOrStore(id, true); loaded {
		return
	}
	go func() {
		defer s.liveBindingWorkers.Delete(id)
		for {
			time.Sleep(time.Second)
			lock := s.bindingUpdateLock(id)
			lock.Lock()
			change := liveBindingChange{ID: id}
			err := s.store.db.QueryRow(`SELECT change_id,previous_json,desired_json,phase FROM app_runtime_binding_updates WHERE install_id=?`, id).Scan(&change.ChangeID, &change.Previous, &change.Desired, &change.Phase)
			if err != nil {
				lock.Unlock()
				return
			}
			var status string
			if err = s.store.db.QueryRow(`SELECT status FROM app_installs WHERE id=?`, id).Scan(&status); err != nil || status != "running" {
				lock.Unlock()
				return
			}
			_, err = s.advanceLiveBindings(change)
			if err != nil {
				_, _ = s.store.db.Exec(`UPDATE app_runtime_binding_updates SET last_error=? WHERE install_id=? AND last_error<>?`, err.Error(), id, err.Error())
			}
			lock.Unlock()
		}
	}()
}
func (s *Server) resumeLiveBindingUpdates() {
	if s.ensureLiveBindingTable() != nil {
		return
	}
	rows, err := s.store.db.Query(`SELECT install_id FROM app_runtime_binding_updates`)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		s.startLiveBindingWorker(id)
	}
}

// Desired defaults take effect while removed resources retain authorization.
func liveEffectiveBindings(change liveBindingChange) (string, error) {
	var previous, desired map[string]any
	if json.Unmarshal([]byte(change.Previous), &previous) != nil || json.Unmarshal([]byte(change.Desired), &desired) != nil {
		return "", errors.New("invalid saved bindings")
	}
	for role, raw := range previous {
		old, _ := appBindingIDs(raw)
		next, defaultID := appBindingIDs(desired[role])
		seen := map[int64]bool{}
		for _, id := range next {
			seen[id] = true
		}
		retaining := false
		for _, id := range old {
			if !seen[id] {
				next = append(next, id)
				seen[id] = true
				retaining = true
			}
		}
		if retaining {
			desired[role] = map[string]any{"ids": next, "default_id": defaultID}
		}
	}
	encoded, err := json.Marshal(desired)
	return string(encoded), err
}

func (s *Server) handleGetLiveBindings(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 4 {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid id", 400)
		return
	}
	if s.ensureLiveBindingTable() != nil {
		http.Error(w, "runtime update storage unavailable", 500)
		return
	}
	response := map[string]any{"bindings": bindingsForInstall(s, id), "pending": false}
	var change, desired, phase, lastError string
	err = s.store.db.QueryRow(`SELECT change_id,desired_json,phase,last_error FROM app_runtime_binding_updates WHERE install_id=?`, id).Scan(&change, &desired, &phase, &lastError)
	if err == nil {
		var bindings map[string]any
		if json.Unmarshal([]byte(desired), &bindings) != nil {
			http.Error(w, "invalid runtime state", 500)
			return
		}
		response["pending"] = true
		response["change_id"] = change
		response["desired_bindings"] = bindings
		response["phase"] = phase
		response["runtime_warning"] = lastError
	} else if !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "runtime update storage unavailable", 500)
		return
	}
	writeJSON(w, response)
}
