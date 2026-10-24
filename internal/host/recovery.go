package host

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// SlotRecord is stored only in the installation's mode-0600 journal. ProxyKey
// authenticates the private loopback hop and is redacted from status responses.
type SlotRecord struct {
	Candidate Candidate `json:"candidate"`
	Address   string    `json:"address"`
	Socket    string    `json:"socket"`
	PID       int       `json:"pid"`
	ProxyKey  string    `json:"proxy_key,omitempty"`
}

func recordSlot(s *slot) *SlotRecord {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	return &SlotRecord{Candidate: s.candidate, Address: s.backend.url.String(), Socket: s.socket, PID: s.cmd.Process.Pid, ProxyKey: s.backend.secret}
}
func (h *Host) adoptSlot(ctx context.Context, record *SlotRecord) (*slot, error) {
	if record == nil || record.PID < 1 || record.ProxyKey == "" || filepath.Dir(record.Socket) != filepath.Join(h.cfg.Home, "host") {
		return nil, errors.New("no verifiable slot record")
	}
	backend, err := newBackend(record.Address)
	if err != nil {
		return nil, err
	}
	ip, _, err := net.SplitHostPort(backend.url.Host)
	if err != nil || !net.ParseIP(ip).IsLoopback() {
		return nil, errors.New("slot is not loopback")
	}
	backend.secret = record.ProxyKey
	// The director captures the current backend field through setSecret, so a
	// recovered connection uses its original per-slot key, not a newly minted one.
	backend.setSecret(record.ProxyKey)
	client := UnixClient(record.Socket)
	client.Timeout = time.Second
	defer client.CloseIdleConnections()
	var status struct {
		ProxyIdentity string `json:"proxy_identity"`
		Protocol      int    `json:"protocol"`
		Version       string `json:"version"`
		PID           int    `json:"pid"`
	}
	if err = Call(ctx, client, "GET", "/status", nil, &status); err != nil {
		return nil, err
	}
	identity := fmt.Sprintf("%x", sha256.Sum256([]byte(record.ProxyKey)))
	if status.Protocol != Protocol || status.PID != record.PID || status.Version != record.Candidate.Version || status.ProxyIdentity != identity {
		return nil, errors.New("recorded process identity no longer matches")
	}
	process, err := os.FindProcess(record.PID)
	if err != nil {
		return nil, err
	}
	s := &slot{candidate: record.Candidate, backend: backend, socket: record.Socket, cmd: &exec.Cmd{Process: process}, done: make(chan struct{})}
	h.slotsMu.Lock()
	h.slots = append(h.slots, s)
	h.slotsMu.Unlock()
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		defer close(s.done)
		failures := 0
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			var current struct {
				PID int `json:"pid"`
			}
			err := Call(ctx, client, "GET", "/status", nil, &current)
			cancel()
			if err != nil || current.PID != record.PID {
				failures++
				if failures == 3 {
					s.err = fmt.Errorf("recovered server %s exited", strconv.Itoa(record.PID))
					return
				}
			} else {
				failures = 0
			}
		}
	}()
	return s, nil
}
func (h *Host) recoverSlot(ctx context.Context) (*slot, bool, error) {
	saved := h.Snapshot()
	var active, pending *slot
	if saved.ActiveSlot != nil {
		active, _ = h.adoptSlot(ctx, saved.ActiveSlot)
	}
	if saved.PendingSlot != nil {
		pending, _ = h.adoptSlot(ctx, saved.PendingSlot)
	}
	if active != nil {
		// A standby candidate from a crashed updater must not outlive recovery of
		// the committed controller. Only its own attested PID may be signalled.
		h.stopSlot(pending, true)
		if err := h.waitReady(ctx, active); err != nil {
			return nil, false, err
		}
		return active, true, nil
	}
	if pending != nil {
		if err := h.waitReady(ctx, pending); err == nil {
			return pending, true, nil
		}
		h.stopSlot(pending, true)
	}
	candidate := Candidate{Binary: saved.Binary, Version: saved.Version}
	if saved.Version != "" && !saved.CleanStop {
		identity, err := h.diskIdentity(ctx, candidate)
		if err != nil {
			return nil, false, err
		}
		candidate.ExistingSchema = true
		candidate.SchemaHash = identity.SchemaHash
		candidate.SecretHash = identity.SecretHash
	}
	s, err := h.startSlot(ctx, candidate)
	if err != nil {
		return nil, false, err
	}
	if err = h.prepare(ctx, s); err == nil {
		h.mu.Lock()
		h.state.PendingSlot = recordSlot(s)
		err = h.saveLocked()
		h.mu.Unlock()
	}
	if err == nil {
		err = h.activate(ctx, s)
	}
	if err != nil {
		h.stopSlot(s, true)
		return nil, false, err
	}
	return s, false, nil
}

func (h *Host) recoverController() error {
	h.mu.Lock()
	if h.deploying {
		h.mu.Unlock()
		return nil
	}
	h.deploying = true
	h.mu.Unlock()
	defer func() { h.mu.Lock(); h.deploying = false; h.mu.Unlock() }()
	h.router.Pause()
	ctx, cancel := context.WithTimeout(h.ctx, h.cfg.StartupTimeout)
	defer cancel()
	recovered, _, err := h.recoverSlot(ctx)
	if err != nil {
		return fmt.Errorf("controller recovery failed without restoring database files: %w", err)
	}
	h.mu.Lock()
	h.active = recovered
	h.state.Binary = recovered.candidate.Binary
	h.state.Version = recovered.candidate.Version
	h.state.ActiveSlot = recordSlot(recovered)
	h.state.PendingSlot = nil
	err = h.saveLocked()
	h.mu.Unlock()
	if err != nil {
		return err
	}
	h.router.Publish(recovered.backend)
	return nil
}
