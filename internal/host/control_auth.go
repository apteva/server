package host

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	// ControlTokenHeader carries the per-host control token on every control
	// socket request.
	ControlTokenHeader = "X-Apteva-Host-Token"
	// ControlTokenEnv passes the control token to server slots. The
	// APTEVA_HOST_ prefix keeps it out of app process environments.
	ControlTokenEnv  = "APTEVA_HOST_CONTROL_TOKEN"
	controlTokenFile = "control.token"
)

// ControlTokenPath is the mode-0600 file holding the host control token.
func ControlTokenPath(home string) string { return filepath.Join(home, "host", controlTokenFile) }

// prepareControlToken writes a fresh random token at startup. When the
// previous host exited uncleanly with recorded slots, a still-valid token is
// kept so an adopted server slot (started with the old token in its
// environment) can keep calling the control socket.
func (h *Host) prepareControlToken() (string, error) {
	path := ControlTokenPath(h.cfg.Home)
	h.mu.Lock()
	adoptable := h.state.ActiveSlot != nil || h.state.PendingSlot != nil
	h.mu.Unlock()
	if adoptable {
		if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0077 == 0 {
			if b, err := os.ReadFile(path); err == nil {
				if token := strings.TrimSpace(string(b)); validControlToken(token) {
					return token, nil
				}
			}
		}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	if err := writeSecretFile(path, []byte(token+"\n")); err != nil {
		return "", err
	}
	return token, nil
}

func validControlToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}

func writeSecretFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".token-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

type peerCredKey struct{}

// peerCred is the kernel-reported identity of a control socket client.
type peerCred struct {
	uid uint32
	pid int
}

func controlConnContext(ctx context.Context, c net.Conn) context.Context {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return ctx
	}
	cred, err := peerCredentials(uc)
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, peerCredKey{}, cred)
}

// peerAllowed admits only the host's own uid or root.
func peerAllowed(peer, self uint32) bool { return peer == self || peer == 0 }

var errNoPeerCred = errors.New("peer credentials unavailable")

// authorizeControl requires (a) a kernel-verified peer uid equal to the
// host's (or root) and (b) the per-host control token, compared in constant
// time. Missing credentials fail closed.
func (h *Host) authorizeControl(next http.Handler) http.Handler {
	self := uint32(os.Geteuid())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, ok := r.Context().Value(peerCredKey{}).(peerCred)
		if !ok || !peerAllowed(cred.uid, self) {
			http.Error(w, "control socket peer is not permitted", http.StatusForbidden)
			return
		}
		got := r.Header.Get(ControlTokenHeader)
		if h.controlToken == "" || subtle.ConstantTimeCompare([]byte(got), []byte(h.controlToken)) != 1 {
			http.Error(w, "missing or invalid host control token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
