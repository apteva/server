package host

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func callbackMarker(t *testing.T, binary, suffix string) {
	t.Helper()
	if err := os.WriteFile(binary+suffix, nil, 0600); err != nil {
		t.Fatal(err)
	}
}

func callbackPID(binary string) int {
	data, _ := os.ReadFile(binary + ".callback-seen")
	pid, _ := strconv.Atoi(string(data))
	return pid
}

func TestColdStartupRoutesAppCallbacksBeforeReady(t *testing.T) {
	h, _, url, _ := fixture(t, func(h *Host) { callbackMarker(t, h.cfg.ServerBinary, ".startup-callback") })
	active := h.Snapshot().ActiveSlot
	if active == nil || callbackPID(active.Candidate.Binary) != active.PID {
		t.Fatal("cold boot became ready without completing its app startup callback")
	}
	response, err := http.Get(url + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("cold boot never published readiness")
	}
}

func TestControllerCrashCallbackRecoveryKeepsUserTrafficGated(t *testing.T) {
	h, _, url, _ := fixture(t, func(h *Host) { callbackMarker(t, h.cfg.ServerBinary, ".startup-callback") })
	old := h.Snapshot().ActiveSlot
	callbackMarker(t, old.Candidate.Binary, ".hold-readiness")
	if err := h.active.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(6 * time.Second)
	for pid := callbackPID(old.Candidate.Binary); pid == 0 || pid == old.PID; pid = callbackPID(old.Candidate.Binary) {
		if time.Now().After(deadline) {
			t.Fatal("recovery deadlocked before completing its startup callback")
		}
		time.Sleep(10 * time.Millisecond)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	// The backend still owns authentication during the startup exception.
	response, err := client.Get(url + "/api/apps/callback/whoami")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatal("startup exception bypassed app authentication")
	}
	userDone := make(chan error, 1)
	go func() {
		response, err := client.Post(url+"/write", "text/plain", strings.NewReader("after-ready"))
		if err == nil {
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				err = fmt.Errorf("user traffic status %d", response.StatusCode)
			}
		}
		userDone <- err
	}()
	select {
	case err := <-userDone:
		t.Fatalf("ordinary request escaped the readiness gate: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	data, _ := os.ReadFile(h.cfg.DBPath)
	if string(data) != "initial\n" {
		t.Fatal("user mutation reached a backend before readiness")
	}
	if err := os.Remove(old.Candidate.Binary + ".hold-readiness"); err != nil {
		t.Fatal(err)
	}
	if err := <-userDone; err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(h.cfg.DBPath)
	if string(data) != "initial\nafter-ready\n" {
		t.Fatalf("queued mutation was lost or replayed: %q", data)
	}
}

func TestColdStartupFailureClearsCallbackRoutingAndReapsCandidate(t *testing.T) {
	h, _, _, _ := fixture(t)
	pauseControllerForRecovery(t, h)
	callbackMarker(t, h.cfg.ServerBinary, ".startup-callback")
	callbackMarker(t, h.cfg.ServerBinary, ".fail-after-callback")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if _, _, err := h.recoverSlot(ctx); err == nil {
		t.Fatal("failed cold activation accepted")
	}
	if callbackPID(h.cfg.ServerBinary) == 0 {
		t.Fatal("failure did not exercise an app startup callback")
	}
	h.router.mu.Lock()
	starting, gate := h.router.starting, h.router.gate
	h.router.mu.Unlock()
	if starting != nil || gate == nil {
		t.Fatal("failed cold activation retained callback routing or opened ingress")
	}
	h.slotsMu.Lock()
	slots := append([]*slot(nil), h.slots...)
	h.slotsMu.Unlock()
	for _, s := range slots {
		select {
		case <-s.done:
		default:
			t.Fatal("failed cold activation left a controller running")
		}
	}
	data, _ := os.ReadFile(h.cfg.DBPath)
	if string(data) != "initial\n" {
		t.Fatal("failed activation changed persistent data")
	}
}

func pauseControllerForRecovery(t *testing.T, h *Host) {
	t.Helper()
	h.mu.Lock()
	h.deploying = true
	h.mu.Unlock()
	h.router.Pause()
	if err := h.router.Drain(context.Background(), h.active.backend); err != nil {
		t.Fatal(err)
	}
	h.stopSlot(h.active, true)
	h.mu.Lock()
	h.state.ActiveSlot, h.state.PendingSlot = nil, nil
	h.mu.Unlock()
}

func TestRecordedSlotRecoveryUnblocksStartupCallbacks(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending=%t", pending), func(t *testing.T) {
			h, _, _, candidate := fixture(t)
			// Simulate a host dying after /activate, while a controller waits for
			// WhoAmI. Prevent the fixture supervisor starting a competing recovery.
			pauseControllerForRecovery(t, h)
			callbackMarker(t, candidate.Binary, ".startup-callback")
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			s, err := h.startSlot(ctx, candidate)
			if err != nil {
				t.Fatal(err)
			}
			if err := h.prepare(ctx, s); err != nil {
				t.Fatal(err)
			}
			client := UnixClient(s.socket)
			defer client.CloseIdleConnections()
			if err := Call(ctx, client, "POST", "/activate", nil, nil); err != nil {
				t.Fatal(err)
			}
			h.mu.Lock()
			h.state.ActiveSlot, h.state.PendingSlot = nil, nil
			if pending {
				h.state.PendingSlot = recordSlot(s)
			} else {
				h.state.ActiveSlot = recordSlot(s)
			}
			h.mu.Unlock()
			recovered, adopted, err := h.recoverSlot(ctx)
			if err != nil || !adopted || recovered.cmd.Process.Pid != s.cmd.Process.Pid {
				t.Fatalf("recorded controller was not recovered in place: adopted=%t err=%v", adopted, err)
			}
			if callbackPID(candidate.Binary) != s.cmd.Process.Pid {
				t.Fatal("recorded slot never completed its startup callback")
			}
			h.router.mu.Lock()
			gated := h.router.gate != nil && h.router.starting == recovered.backend
			h.router.mu.Unlock()
			if !gated {
				t.Fatal("recorded recovery published ordinary traffic prematurely")
			}
		})
	}
}

func TestFailedRecordedSlotRecoveryCleansUpAndRecoversCommittedVersion(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending=%t", pending), func(t *testing.T) {
			h, _, _, candidate := fixture(t)
			pauseControllerForRecovery(t, h)
			callbackMarker(t, candidate.Binary, ".startup-callback")
			callbackMarker(t, candidate.Binary, ".fail-after-callback")
			callbackMarker(t, h.cfg.ServerBinary, ".startup-callback")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s, err := h.startSlot(ctx, candidate)
			if err != nil {
				t.Fatal(err)
			}
			if err := h.prepare(ctx, s); err != nil {
				t.Fatal(err)
			}
			client := UnixClient(s.socket)
			defer client.CloseIdleConnections()
			if err := Call(ctx, client, "POST", "/activate", nil, nil); err != nil {
				t.Fatal(err)
			}
			h.mu.Lock()
			if pending {
				h.state.PendingSlot = recordSlot(s)
			} else {
				h.state.ActiveSlot = recordSlot(s)
			}
			h.mu.Unlock()
			recovered, adopted, err := h.recoverSlot(ctx)
			if pending {
				if err != nil || adopted || recovered.candidate.Version != "1.0.0" || callbackPID(h.cfg.ServerBinary) != recovered.cmd.Process.Pid {
					t.Fatalf("committed version was not recovered after pending failure: adopted=%t err=%v", adopted, err)
				}
			} else if err == nil || recovered != nil {
				t.Fatal("failed committed slot was accepted")
			}
			h.router.mu.Lock()
			starting, gated := h.router.starting, h.router.gate != nil
			h.router.mu.Unlock()
			if !gated || (pending && starting != recovered.backend) || (!pending && starting != nil) {
				t.Fatal("failed recovered slot retained startup routing or opened ingress")
			}
			select {
			case <-s.done:
			default:
				t.Fatal("failed recorded controller was not reaped")
			}
		})
	}
}

func TestDeploymentAndRollbackWithAppStartupCallbacks(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("activation-failure=%t", fail), func(t *testing.T) {
			h, client, _, candidate := fixture(t, func(h *Host) { callbackMarker(t, h.cfg.ServerBinary, ".startup-callback") })
			callbackMarker(t, candidate.Binary, ".startup-callback")
			if fail {
				callbackMarker(t, candidate.Binary, ".fail-after-callback")
			}
			if err := Call(context.Background(), client, "POST", "/deploy", candidate, nil); err != nil {
				t.Fatal(err)
			}
			state := waitJob(t, client)
			phase, version := "succeeded", "2.0.0"
			if fail {
				phase, version = "rolled_back", "1.0.0"
			}
			if state.Job.Phase != phase || state.Version != version || callbackPID(state.ActiveSlot.Candidate.Binary) != state.ActiveSlot.PID {
				t.Fatalf("startup callback deployment/rollback failed: %+v", state.Job)
			}
			h.router.mu.Lock()
			published := h.router.gate == nil && h.router.starting == nil && h.router.active == h.active.backend
			h.router.mu.Unlock()
			if !published {
				t.Fatal("serving slot was not published after verified readiness")
			}
		})
	}
}

func TestFailedActivationClearsStartupBackend(t *testing.T) {
	h, _, _, candidate := fixture(t)
	callbackMarker(t, candidate.Binary, ".fail-activation")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, err := h.startSlot(ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.prepare(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := h.activate(ctx, s); err == nil {
		t.Fatal("failed activation accepted")
	}
	h.router.mu.Lock()
	starting, active := h.router.starting, h.router.active
	h.router.mu.Unlock()
	if starting != nil || active != h.active.backend {
		t.Fatal("failed candidate retained startup routing or displaced the serving backend")
	}
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("failed candidate was not reaped")
	}
}

func TestStartupCallbackExceptionDoesNotTrustForwardingHeaders(t *testing.T) {
	router := NewRouter()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "unexpected") }))
	defer upstream.Close()
	b, err := newBackend(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	router.SetStarting(b)
	for _, tc := range []struct{ remote, path string }{
		{"198.51.100.2:1234", "/api/apps/callback/whoami"},
		{"127.0.0.1:1234", "/api/agents"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		request := httptest.NewRequest(http.MethodGet, tc.path, nil).WithContext(ctx)
		request.RemoteAddr = tc.remote
		request.Header.Set("X-Forwarded-For", "127.0.0.1")
		request.Header.Set("X-Apteva-Host-Remote-Addr", "127.0.0.1:1")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		cancel()
		if recorder.Body.Len() != 0 || ctx.Err() == nil {
			t.Fatal("ordinary or spoofed external request bypassed the readiness gate")
		}
	}
}
