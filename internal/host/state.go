// Package host owns the stable ingress and serializes versioned server handoffs.
// It never replaces application databases or copies a preflight snapshot back.
package host

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Protocol stays 1: hosts compare it exactly, so a bump would make every
// existing host refuse every newer server. New behaviour is advertised as
// capabilities instead, which older hosts and CLIs ignore.
const Protocol = 1

const (
	// CapabilityControlToken: the server sends the host control token on
	// control-socket calls (process starts, listeners).
	CapabilityControlToken = "control_token"
	// CapabilityRestartMigration: the host can deploy a release that changes
	// stored schema by stopping the current server and booting the new one.
	CapabilityRestartMigration = "restart_migration"
	// CapabilityUpdateCheck: the server's --preflight accepts
	// APTEVA_UPDATE_CHECK_DIR and checks migrations on a reduced copy of the
	// database, writing a backup only when asked.
	CapabilityUpdateCheck = "update_check"
)

// ServerCapabilities is what a server reports from --host-protocol.
var ServerCapabilities = []string{CapabilityControlToken, CapabilityUpdateCheck}

// HostCapabilities is what the host reports from GET /status.
var HostCapabilities = []string{CapabilityRestartMigration}

type Deployment struct {
	ID        string    `json:"id"`
	Phase     string    `json:"phase"`
	Message   string    `json:"message,omitempty"`
	Version   string    `json:"version"`
	Previous  string    `json:"previous,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	// Check is the candidate's database check, once it has run.
	Check *CheckSummary `json:"check,omitempty"`
}

type State struct {
	CleanStop   bool                       `json:"clean_stop,omitempty"`
	HostBinary  string                     `json:"host_binary"`
	Listeners   map[string]ListenerRequest `json:"listeners,omitempty"`
	ActiveSlot  *SlotRecord                `json:"active_slot,omitempty"`
	PendingSlot *SlotRecord                `json:"pending_slot,omitempty"`
	DataDir     string                     `json:"data_dir"`
	HTTPAddr    string                     `json:"http_addr,omitempty"`
	HTTPSAddr   string                     `json:"https_addr,omitempty"`
	Protocol    int                        `json:"protocol"`
	Home        string                     `json:"home"`
	DBPath      string                     `json:"db_path"`
	ListenAddr  string                     `json:"listen_addr"`
	PID         int                        `json:"pid"`
	Binary      string                     `json:"binary"`
	Version     string                     `json:"version"`
	Job         *Deployment                `json:"job,omitempty"`
	// Capabilities is filled only in status responses, never persisted.
	Capabilities []string `json:"capabilities,omitempty"`
}

func StatePath(home string) string  { return filepath.Join(home, "host", "state.json") }
func SocketPath(home string) string { return filepath.Join(home, "host", "control.sock") }

// NextServerPath holds a request, written by `apteva update --restart` while
// it restarts an older host that cannot run restart migrations itself, for
// the next host start to boot this server fresh so it migrates. The request
// is honoured only after a clean stop and removed once the server serves.
func NextServerPath(home string) string { return filepath.Join(home, "host", "next-server.json") }

func WriteState(path string, state State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".state-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		err = json.NewEncoder(f).Encode(state)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func ReadState(home string) (State, error) {
	var s State
	b, err := os.ReadFile(StatePath(home))
	if err != nil {
		return s, err
	}
	if err = json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	if s.Protocol != Protocol || s.Home != filepath.Clean(home) || !filepath.IsAbs(s.DBPath) {
		return s, fmt.Errorf("invalid host identity")
	}
	return s, nil
}
