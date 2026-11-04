package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// Runs the whole path against a real Core and two instrumented providers, so a
// pin is proven by which endpoint the worker's request reached, not by what any
// API reports. It needs a Core binary, so it doesn't run by default:
//
//	APTEVA_TEST_CORE_BIN=/path/to/apteva-core go test . -run TestThreadProviderAgainstRealCore -v

// recordingLLM is an OpenAI-compatible endpoint that remembers each request.
// Requests are attributed by the sender's system prompt. Searching whole bodies
// would mislead: a parent's context lists its child threads and their directives,
// so a worker's directive legitimately reaches the parent's provider that way.
type recordingLLM struct {
	mu       sync.Mutex
	requests []recordedLLMRequest
	URL      string
}

type recordedLLMRequest struct {
	system string
	raw    string
}

func newRecordingLLM(t *testing.T) *recordingLLM {
	t.Helper()
	llm := &recordingLLM{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &body)
		request := recordedLLMRequest{raw: string(raw)}
		for _, message := range body.Messages {
			if text, ok := message.Content.(string); ok && message.Role == "system" {
				request.system += text
			}
		}
		llm.mu.Lock()
		llm.requests = append(llm.requests, request)
		llm.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`+"\n\n"+
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n"+
			"data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	llm.URL = server.URL
	return llm
}

// fromThread counts the requests a worker sent here that contain marker. An
// empty marker counts all of them.
func (l *recordingLLM) fromThread(threadID, marker string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for _, request := range l.requests {
		if strings.Contains(request.system, `(id="`+threadID+`")`) && strings.Contains(request.raw, marker) {
			count++
		}
	}
	return count
}

// fromMain counts requests from the parent, which has no sub-thread identity.
func (l *recordingLLM) fromMain() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for _, request := range l.requests {
		if !strings.Contains(request.system, "SUB-THREAD") {
			count++
		}
	}
	return count
}

func (l *recordingLLM) waitForMain(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if l.fromMain() > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the parent's provider never received a request from the parent")
}

func (l *recordingLLM) waitFor(t *testing.T, name, threadID, marker string) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if l.fromThread(threadID, marker) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never received a request from %s containing %q", name, threadID, marker)
}

// realCore runs the binary the way the server does: scoped environment, its own
// data directory, config.json on disk.
type realCore struct {
	bin, dir string
	port     int
	env      []string
	cmd      *exec.Cmd
	exited   chan struct{}
	log      *os.File
}

func (c *realCore) start(t *testing.T) {
	t.Helper()
	c.cmd = exec.Command(c.bin, "--headless")
	c.cmd.Dir = c.dir
	c.cmd.Env = c.env
	c.cmd.Stdout, c.cmd.Stderr = c.log, c.log
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	c.exited = exited
	cmd := c.cmd
	go func() { _ = cmd.Wait(); close(exited) }()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			t.Fatalf("core exited at startup:\n%s", c.logTail())
		default:
		}
		if resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", c.port)); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("core not healthy:\n%s", c.logTail())
}

func (c *realCore) stop() {
	if c.cmd == nil || c.cmd.Process == nil {
		return
	}
	_ = c.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-c.exited:
	case <-time.After(10 * time.Second):
		_ = c.cmd.Process.Kill()
		<-c.exited
	}
}

func (c *realCore) logTail() string {
	raw, _ := os.ReadFile(c.log.Name())
	if len(raw) > 4000 {
		raw = raw[len(raw)-4000:]
	}
	return string(raw)
}

func (c *realCore) thread(t *testing.T, id string) (threadIDRow, bool) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/threads", c.port), nil)
	req.Header.Set("Authorization", "Bearer core-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rows []threadIDRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == id {
			return row, true
		}
	}
	return threadIDRow{}, false
}

func (c *realCore) putProviders(t *testing.T, providers []map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"providers": providers})
	req, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("http://127.0.0.1:%d/config", c.port), strings.NewReader(string(raw)))
	req.Header.Set("Authorization", "Bearer core-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT /config: %d %s", resp.StatusCode, body)
	}
}

func TestThreadProviderAgainstRealCore(t *testing.T) {
	bin := os.Getenv("APTEVA_TEST_CORE_BIN")
	if bin == "" {
		t.Skip("set APTEVA_TEST_CORE_BIN to a Core binary to run this")
	}
	parent, other := newRecordingLLM(t), newRecordingLLM(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	dir := t.TempDir()
	config, _ := json.Marshal(map[string]any{
		"directive": "You are the main thread.",
		"providers": []map[string]any{{"name": "ollama", "default": true}, {"name": "managed"}},
	})
	if err := os.WriteFile(filepath.Join(dir, "config.json"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(t.TempDir(), "core.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	core := &realCore{bin: bin, dir: dir, port: port, log: logFile, env: append(coreProcessBaseEnv(dir),
		"API_PORT="+strconv.Itoa(port), "NO_TUI=1", "NO_CONSOLE=1", "APTEVA_API_KEY=core-key",
		"OLLAMA_HOST="+parent.URL, "APTEVA_MANAGED_LLM_URL="+other.URL,
	)}
	core.start(t)
	defer core.stop()

	f := newThreadProviderFixtureOnPort(t, port)
	f.addProvider(t, "Ollama", "proj-1")
	f.addProvider(t, "Managed", "proj-1")

	const pinned, pinnedMarker = "worker-pinned", "MARKER-PINNED-WORKER"
	ensure := func(threadID, provider, suffix string, events ...sdk.ThreadEvent) (*httptest.ResponseRecorder, *sdk.ThreadProviderStatus) {
		return f.request("ensure", sdk.ThreadSpawnRequest{
			ThreadID: threadID, Provider: provider, DirectiveSuffix: suffix, MCP: []string{}, Events: events,
		})
	}
	event := func(id, message string) sdk.ThreadEvent { return sdk.ThreadEvent{ID: id, Message: message} }

	// A pin sends the worker to the chosen endpoint and nowhere else.
	rec, status := ensure(pinned, "managed", " "+pinnedMarker, event("e1", "first task"))
	if rec.Code != http.StatusOK {
		t.Fatalf("ensure: %d %s\n%s", rec.Code, rec.Body.String(), core.logTail())
	}
	if status == nil || status.Pin != "managed" || status.Active != "managed" || status.Missing {
		t.Fatalf("status = %+v", status)
	}
	other.waitFor(t, "chosen provider", pinned, pinnedMarker)
	if got := parent.fromThread(pinned, ""); got != 0 {
		t.Fatalf("%d of the pinned worker's requests reached the parent's provider", got)
	}

	// The parent stays where it was, and no pin leaks onto it.
	if main, found := core.thread(t, "main"); !found || main.ProviderPin != "" {
		t.Fatalf("main thread = %+v", main)
	}
	parent.waitForMain(t)
	if got := other.fromMain(); got != 0 {
		t.Fatalf("%d of the parent's requests reached the worker's provider", got)
	}

	// Spawn takes the same path.
	rec, status = f.request("spawn", sdk.ThreadSpawnRequest{
		ThreadID: "worker-spawned", Provider: "managed", DirectiveSuffix: " MARKER-SPAWNED", MCP: []string{}, Events: []sdk.ThreadEvent{event("s1", "go")},
	})
	if rec.Code != http.StatusOK || status == nil || status.Pin != "managed" {
		t.Fatalf("spawn: %d %s", rec.Code, rec.Body.String())
	}
	other.waitFor(t, "chosen provider", "worker-spawned", "MARKER-SPAWNED")
	if got := parent.fromThread("worker-spawned", ""); got != 0 {
		t.Fatalf("%d of the spawned worker's requests reached the parent's provider", got)
	}

	// Without a choice a worker runs where it always did, unpinned.
	rec, status = ensure("worker-default", "", " MARKER-DEFAULT", event("d1", "go"))
	if rec.Code != http.StatusOK {
		t.Fatalf("default ensure: %d %s", rec.Code, rec.Body.String())
	}
	if status != nil {
		t.Fatalf("status for a thread nobody pinned = %+v", status)
	}
	parent.waitFor(t, "parent's provider", "worker-default", "MARKER-DEFAULT")
	if got := other.fromThread("worker-default", ""); got != 0 {
		t.Fatalf("%d of the unpinned worker's requests reached the other provider", got)
	}
	if row, _ := core.thread(t, "worker-default"); row.ProviderPin != "" || row.Provider != "ollama" {
		t.Fatalf("unpinned row = %+v", row)
	}

	// The server's pool refuses first, and the real Core refuses what it doesn't hold.
	if rec, _ := ensure("worker-refused", "google", "", event("r1", "go")); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("outside pool: %d %s", rec.Code, rec.Body.String())
	}
	f.addProvider(t, "Google", "proj-1")
	rec, _ = ensure("worker-refused", "google", "", event("r2", "go"))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "unknown provider") {
		t.Fatalf("refused by Core: %d %s", rec.Code, rec.Body.String())
	}
	if _, found := core.thread(t, "worker-refused"); found {
		t.Fatal("a refused request left a thread behind")
	}

	// Asking for another provider changes nothing, including queued events.
	rec, _ = ensure(pinned, "ollama", " "+pinnedMarker, event("c1", "EVENT-CONFLICT"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflict: %d %s", rec.Code, rec.Body.String())
	}
	if row, _ := core.thread(t, pinned); row.ProviderPin != "managed" {
		t.Fatalf("conflict moved the thread: %+v", row)
	}

	// Repeating the choice delivers to the same endpoint.
	if rec, _ := ensure(pinned, "managed", " "+pinnedMarker, event("e2", "EVENT-REPEAT")); rec.Code != http.StatusOK {
		t.Fatalf("repeat: %d %s", rec.Code, rec.Body.String())
	}
	other.waitFor(t, "chosen provider", pinned, "EVENT-REPEAT")

	// So does a profile repair, whether or not the app names the provider.
	const repaired = " " + pinnedMarker + " MARKER-REPAIRED"
	rec, status = ensure(pinned, "", repaired, event("e3", "EVENT-REPAIR"))
	if rec.Code != http.StatusOK || status == nil || status.Pin != "managed" {
		t.Fatalf("repair: %d %s", rec.Code, rec.Body.String())
	}
	other.waitFor(t, "chosen provider", pinned, "EVENT-REPAIR")
	if row, _ := core.thread(t, pinned); row.ProviderPin != "managed" || row.Provider != "managed" {
		t.Fatalf("repair changed the provider: %+v", row)
	}

	// A restart restores the pin from Core's own state.
	core.stop()
	core.start(t)
	row, found := core.thread(t, pinned)
	if !found || row.ProviderPin != "managed" || row.Provider != "managed" {
		t.Fatalf("after restart: found=%v row=%+v\n%s", found, row, core.logTail())
	}
	if rec, _ := ensure(pinned, "managed", repaired, event("e4", "EVENT-AFTER-RESTART")); rec.Code != http.StatusOK {
		t.Fatalf("after restart: %d %s", rec.Code, rec.Body.String())
	}
	other.waitFor(t, "chosen provider", pinned, "EVENT-AFTER-RESTART")
	if got := parent.fromThread(pinned, ""); got != 0 {
		t.Fatalf("%d of the pinned worker's requests reached the parent's provider by the end of the restart", got)
	}

	// With the provider gone the worker keeps running on the parent's. The pin
	// stays, repeating it still delivers, and the app can see what happened.
	core.putProviders(t, []map[string]any{{"name": "ollama", "default": true}})
	rec, status = ensure(pinned, "managed", repaired, event("e5", "EVENT-WHILE-MISSING"))
	if rec.Code != http.StatusOK {
		t.Fatalf("while missing: %d %s", rec.Code, rec.Body.String())
	}
	if status == nil || status.Pin != "managed" || status.Active != "ollama" || !status.Missing {
		t.Fatalf("while missing status = %+v", status)
	}
	parent.waitFor(t, "parent's provider", pinned, "EVENT-WHILE-MISSING")

	// When it comes back the worker returns to it.
	core.putProviders(t, []map[string]any{{"name": "ollama", "default": true}, {"name": "managed"}})
	rec, status = ensure(pinned, "managed", repaired, event("e6", "EVENT-AFTER-RETURN"))
	if rec.Code != http.StatusOK {
		t.Fatalf("after return: %d %s", rec.Code, rec.Body.String())
	}
	if status == nil || status.Active != "managed" || status.Missing {
		t.Fatalf("after return status = %+v", status)
	}
	other.waitFor(t, "chosen provider", pinned, "EVENT-AFTER-RETURN")
}
