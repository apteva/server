package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

type credentialRefresh func(map[string]string, func(map[string]string) error) error

// Serialize only token refresh, never ordinary integration requests. The file
// lock coordinates the server and its stdio gateway processes. Credential CAS
// also protects against an operator reconnecting the account during refresh.
func (s *Server) refreshConnectionCredentials(id int64, credentials map[string]string, refresh func(map[string]string) error) error {
	mu := &s.store.credentialLocks[uint64(id)%64]
	mu.Lock()
	defer mu.Unlock()
	if s.store.path != "" {
		dir := filepath.Join(filepath.Dir(s.store.path), ".credential-locks")
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(dir, filepath.Base(s.store.path)+"-"+strconv.FormatInt(id, 10)), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		deadline := time.Now().Add(30 * time.Second)
		for {
			err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				break
			}
			if err != syscall.EWOULDBLOCK || time.Now().After(deadline) {
				return fmt.Errorf("credential refresh busy: %w", err)
			}
			time.Sleep(20 * time.Millisecond)
		}
		defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}
	var original string
	if err := s.store.db.QueryRow("SELECT encrypted_credentials FROM connections WHERE id=?", id).Scan(&original); err != nil {
		return err
	}
	plain, err := Decrypt(s.secret, original)
	if err != nil {
		return err
	}
	latest := map[string]string{}
	if err = json.Unmarshal([]byte(plain), &latest); err != nil {
		return err
	}
	advanced := false
	for _, key := range []string{"access_token", "refresh_token", "refreshToken", "token_expires_at", "expires_at"} {
		if latest[key] != credentials[key] {
			advanced = true
		}
		credentials[key] = latest[key]
	}
	for key, value := range latest {
		credentials[key] = value
	}
	if advanced {
		return nil
	}
	if err := refresh(credentials); err != nil {
		return err
	}
	raw, err := json.Marshal(credentials)
	if err != nil {
		return err
	}
	encrypted, err := Encrypt(s.secret, string(raw))
	if err != nil {
		return err
	}
	result, err := s.store.db.Exec("UPDATE connections SET encrypted_credentials=? WHERE id=? AND encrypted_credentials=?", encrypted, id, original)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("connection credentials changed during refresh")
	}
	return nil
}

func (s *Server) executeConnectionToolWithRefresh(id int64, app *AppTemplate, tool *AppToolDef, credentials map[string]string, input map[string]any, environmentID string, _ onCredsRefresh) (*ExecuteResult, error) {
	return executeIntegrationToolWithRefresh(app, tool, credentials, input, environmentID, nil, func(c map[string]string, fn func(map[string]string) error) error {
		return s.refreshConnectionCredentials(id, c, fn)
	})
}
