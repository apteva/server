package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const fakeCoreConfigError = "invalid core config: decode config: json: cannot unmarshal bool into Go struct field Config.realtime_voice_mcp of type []string"

// A core that cannot load its config exits at once. The start reports Core's
// own error right away instead of waiting out the startup timeout, and the
// agent stays stopped with that error on record.
func TestStartReportsCoreExitDuringStart(t *testing.T) {
	s := newTestServer(t)
	registerAndLogin(t, s)
	script := filepath.Join(t.TempDir(), "fake-core")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'apteva-core starting' >&2\necho '"+fakeCoreConfigError+"' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	s.agents.coreCmd = script
	encrypted, err := Encrypt(s.secret, `{"api_key":"test"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.CreateProvider(1, 0, "llm", "OpenAI", encrypted); err != nil {
		t.Fatal(err)
	}
	agent, err := s.store.CreateAgent(1, "bad-config", "directive", "autonomous", `{"include_channels":false,"include_apteva_server":false}`, "")
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	w := httptest.NewRecorder()
	s.handleStartInstance(w, authedRequest(t, http.MethodPost, fmt.Sprintf("/instances/%d/start", agent.ID), "", nil))
	if took := time.Since(started); took > time.Second {
		t.Fatalf("start took %s, want under a second", took)
	}
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", w.Code, w.Body.String())
	}
	var body struct {
		Error     string `json:"error"`
		CoreError string `json:"core_error"`
		ExitCode  int    `json:"exit_code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error != "core exited during start" || body.ExitCode != 1 || !strings.HasSuffix(body.CoreError, fakeCoreConfigError) {
		t.Fatalf("body = %+v", body)
	}

	if s.agents.IsRunning(agent.ID) {
		t.Fatal("agent is running after its core exited")
	}
	got, err := s.store.GetAgentByID(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "stopped" || got.Pid != 0 {
		t.Fatalf("saved status=%q pid=%d, want stopped", got.Status, got.Pid)
	}
	s.enrichAgentRuntime(got)
	record := got.LastStartError
	if record == nil || record.ExitCode == nil || *record.ExitCode != 1 || !strings.HasSuffix(record.CoreError, fakeCoreConfigError) || !strings.Contains(record.Message, fakeCoreConfigError) {
		t.Fatalf("last_start_error = %+v", record)
	}

	// A start that works clears the record.
	s.agents.noteStartResult(agent.ID, nil)
	if s.agents.LastStartError(agent.ID) != nil {
		t.Fatal("a successful start left the last start error in place")
	}
}

// The follower copies Core's stderr into the server log line by line, keeps
// the last lines, and empties the file once it has grown past the cap.
func TestCoreStderrFollowerCopiesAndCaps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core-stderr", "agent_1.log")
	w, r, err := createCoreStderr(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	out := &lockedBuffer{}
	f := &coreStderr{file: r, stop: make(chan struct{}), done: make(chan struct{}), out: out}
	go f.run()

	fmt.Fprint(w, "\x1b[2mfirst\x1b[0m line\nsecond ")
	waitFor(t, func() bool { return strings.Contains(out.String(), "first") })
	if strings.Contains(out.String(), "second") {
		t.Fatal("copied an unfinished line")
	}
	fmt.Fprint(w, "line\n")
	big := strings.Repeat("x", 1023) + "\n"
	for written := 0; written <= coreStderrMaxBytes; written += len(big) {
		fmt.Fprint(w, big)
	}
	waitFor(t, func() bool {
		info, err := os.Stat(path)
		return err == nil && info.Size() < coreStderrMaxBytes
	})
	fmt.Fprint(w, "last words")
	tail := f.finish()
	if !strings.HasSuffix(tail, "\nlast words") {
		t.Fatalf("tail ends %q", tail[max(0, len(tail)-40):])
	}
	if got := out.String(); !strings.HasPrefix(got, "\x1b[2mfirst\x1b[0m line\nsecond line\n") || !strings.HasSuffix(got, "last words\n") {
		t.Fatalf("server log copy starts %q", got[:min(len(got), 60)])
	}

	// A new core gets a new file; the old follower's file is left alone.
	w2, r2, err := createCoreStderr(path)
	if err != nil {
		t.Fatal(err)
	}
	w2.Close()
	r2.Close()
	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		t.Fatalf("new stderr file: size=%v err=%v", info, err)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
