package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	sdk "github.com/apteva/app-sdk"
)

// threadProviderCore fakes the parts of Core that thread pins touch. Its rules
// are Core's: a POST for a thread that exists answers "exists" and ignores the
// provider, and a POST naming a provider it doesn't hold answers 400.
type threadProviderCore struct {
	mu          sync.Mutex
	held        map[string]bool
	parent      string
	rows        map[string]map[string]any
	calls       []string
	bodies      []map[string]any
	ignoresPins bool           // a Core from before pins existed
	raceWinner  map[string]any // appears after the server's first look
	failReads   bool           // GET /threads fails once a thread has been created
}

func newThreadProviderCore(held ...string) *threadProviderCore {
	core := &threadProviderCore{held: map[string]bool{}, parent: "openai", rows: map[string]map[string]any{}}
	for _, name := range held {
		core.held[name] = true
	}
	return core
}

func threadEventIDs(body map[string]any) []string {
	ids := []string{}
	events, _ := body["events"].([]any)
	for _, event := range events {
		ids = append(ids, event.(map[string]any)["id"].(string))
	}
	return ids
}

func (c *threadProviderCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, r.Method+" "+r.URL.Path)
	threadID := strings.TrimPrefix(r.URL.Path, "/threads/")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/config":
		_ = json.NewEncoder(w).Encode(map[string]any{"directive": "main"})
	case r.Method == http.MethodGet && r.URL.Path == "/threads":
		if c.failReads && len(c.rows) > 0 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		rows := []map[string]any{{"id": "main", "directive": "main", "provider": c.parent}}
		for _, row := range c.rows {
			rows = append(rows, row)
		}
		_ = json.NewEncoder(w).Encode(rows)
	case r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.bodies = append(c.bodies, body)
		if c.raceWinner != nil {
			c.rows[threadID] = c.raceWinner
			c.raceWinner = nil
		}
		if _, exists := c.rows[threadID]; exists {
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "exists", "events": map[string]any{"accepted": threadEventIDs(body)}})
			return
		}
		provider, _ := body["provider"].(string)
		row := map[string]any{"id": threadID, "directive": "main" + fmt.Sprint(body["directive_suffix"]), "provider": c.parent}
		if provider != "" && !c.ignoresPins {
			if !c.held[provider] {
				http.Error(w, fmt.Sprintf("unknown provider %q (available: %s)", provider, c.parent), http.StatusBadRequest)
				return
			}
			row["provider"], row["provider_pin"] = provider, provider
		}
		if mcp, ok := body["mcp"].([]any); ok {
			row["mcp_names"] = mcp
		}
		c.rows[threadID] = row
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "created", "events": map[string]any{"accepted": threadEventIDs(body)}})
	case r.Method == http.MethodPut:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.bodies = append(c.bodies, body)
		c.rows[threadID]["directive"] = "main" + fmt.Sprint(body["directive_suffix"])
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "updated", "events": map[string]any{"accepted": threadEventIDs(body)}})
	case r.Method == http.MethodDelete:
		delete(c.rows, threadID)
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "unexpected request", http.StatusNotFound)
	}
}

func (c *threadProviderCore) writes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	writes := []string{}
	for _, call := range c.calls {
		if !strings.HasPrefix(call, "GET ") {
			writes = append(writes, call)
		}
	}
	return writes
}

func (c *threadProviderCore) allCalls() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.calls, ",")
}

func (c *threadProviderCore) lastBody(t *testing.T) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		t.Fatal("core received no thread write")
	}
	return c.bodies[len(c.bodies)-1]
}

type threadProviderFixture struct {
	s         *Server
	core      *threadProviderCore
	agent     *Agent
	installID int64
}

// Each fixture's pool holds OpenAI and Anthropic for the agent's project.
func newThreadProviderFixture(t *testing.T, core *threadProviderCore) *threadProviderFixture {
	t.Helper()
	coreServer := httptest.NewServer(core)
	t.Cleanup(coreServer.Close)
	parsed, _ := url.Parse(coreServer.URL)
	_, portText, _ := net.SplitHostPort(parsed.Host)
	port, _ := strconv.Atoi(portText)
	f := newThreadProviderFixtureOnPort(t, port)
	f.core = core
	return f
}

func newThreadProviderFixtureOnPort(t *testing.T, port int) *threadProviderFixture {
	t.Helper()
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	agent, err := s.store.CreateAgent(1, "pin-target", "main", "autonomous", "{}", "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	s.agents.processes[agent.ID] = &runningAgent{port: port, coreAPIKey: "core-key", reattached: true}
	manifest := sdk.Manifest{Schema: sdk.SchemaCurrent, Name: "pin-owner",
		Requires: sdk.Requires{Permissions: []sdk.Permission{sdk.PermThreadsWrite}}}
	installID := seedInstallWithBindings(t, s, "pin-owner", manifest, nil)
	f := &threadProviderFixture{s: s, agent: agent, installID: installID}
	f.addProvider(t, "OpenAI", "proj-1")
	f.addProvider(t, "Anthropic", "proj-1")
	return f
}

func (f *threadProviderFixture) addProvider(t *testing.T, name, projectID string) {
	t.Helper()
	encrypted, err := Encrypt(f.s.secret, `{"api_key":"sk-do-not-leak"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.store.CreateProvider(1, 0, "llm", name, encrypted, projectID); err != nil {
		t.Fatal(err)
	}
}

func (f *threadProviderFixture) post(route string, body any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/apps/callback/threads/"+route, strings.NewReader(string(raw)))
	req.Header.Set("X-Apteva-App-Install-ID", itoa(f.installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	f.s.handleAppCallback(rec, req)
	return rec
}

// request runs one route and returns what both results share.
func (f *threadProviderFixture) request(route string, req sdk.ThreadSpawnRequest) (*httptest.ResponseRecorder, *sdk.ThreadProviderStatus) {
	req.AgentID = f.agent.ID
	var rec *httptest.ResponseRecorder
	if route == "ensure" {
		rec = f.post(route, sdk.ThreadEnsureRequest{ThreadSpawnRequest: req, ProfileHash: "profile-1"})
	} else {
		rec = f.post(route, req)
	}
	var result struct {
		Provider *sdk.ThreadProviderStatus `json:"provider"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &result)
	return rec, result.Provider
}

var threadProviderRoutes = []string{"spawn", "ensure"}

func TestThreadProviderOmittedKeepsTheCallsAndPayloadsItHadBefore(t *testing.T) {
	for route, wantCalls := range map[string]string{
		"spawn":  "POST /threads/t",
		"ensure": "GET /threads,POST /threads/t",
	} {
		t.Run(route, func(t *testing.T) {
			core := newThreadProviderCore()
			f := newThreadProviderFixture(t, core)
			rec, status := f.request(route, sdk.ThreadSpawnRequest{ThreadID: "t", MCP: []string{"tasks"}})
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if got := core.allCalls(); got != wantCalls {
				t.Fatalf("core calls = %s, want %s", got, wantCalls)
			}
			if _, sent := core.lastBody(t)["provider"]; sent {
				t.Fatalf("sent a provider nobody asked for: %v", core.lastBody(t))
			}
			if status != nil {
				t.Fatalf("status = %+v, want none", status)
			}
		})
	}
}

func TestPlatformInfoAdvertisesThreadProviderSelection(t *testing.T) {
	f := newThreadProviderFixture(t, newThreadProviderCore())
	req := httptest.NewRequest(http.MethodGet, "/apps/callback/platform-info", nil)
	req.Header.Set("X-Apteva-App-Install-ID", itoa(f.installID))
	req.Header.Set("X-User-ID", "1")
	rec := httptest.NewRecorder()
	f.s.handleAppCallback(rec, req)
	var info sdk.PlatformInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("status=%d body=%s: %v", rec.Code, rec.Body.String(), err)
	}
	if info.ThreadProviderSelection != sdk.ThreadProviderSelectionVersion {
		t.Fatalf("capability = %d, want %d", info.ThreadProviderSelection, sdk.ThreadProviderSelectionVersion)
	}
}

func TestThreadProviderIsForwardedAndReported(t *testing.T) {
	for _, route := range threadProviderRoutes {
		for _, requested := range []string{"anthropic", " Anthropic "} {
			t.Run(route+"/"+requested, func(t *testing.T) {
				core := newThreadProviderCore("anthropic")
				f := newThreadProviderFixture(t, core)
				rec, status := f.request(route, sdk.ThreadSpawnRequest{
					ThreadID: "t", Provider: requested, Events: []sdk.ThreadEvent{{ID: "e1", Message: "go"}},
				})
				if rec.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
				if got := core.lastBody(t)["provider"]; got != "anthropic" {
					t.Fatalf("core got provider %v", got)
				}
				if status == nil || status.Pin != "anthropic" || status.Active != "anthropic" || status.Missing {
					t.Fatalf("status = %+v", status)
				}
			})
		}
	}
}

func TestThreadProviderOutsideThePoolIsRefusedBeforeCore(t *testing.T) {
	for name, requested := range map[string]string{
		"unconfigured":    "google",
		"made up":         "not-a-provider",
		"realtime":        "openai-realtime",
		"another project": "xai",
	} {
		for _, route := range threadProviderRoutes {
			t.Run(name+"/"+route, func(t *testing.T) {
				core := newThreadProviderCore("google", "xai", "openai-realtime")
				f := newThreadProviderFixture(t, core)
				f.addProvider(t, "xAI", "proj-2")
				rec, status := f.request(route, sdk.ThreadSpawnRequest{ThreadID: "t", ProjectID: "proj-1", Provider: requested})
				if rec.Code != http.StatusUnprocessableEntity {
					t.Fatalf("status=%d body=%s, want 422", rec.Code, rec.Body.String())
				}
				if got := core.writes(); len(got) != 0 {
					t.Fatalf("core was written to: %v", got)
				}
				if status != nil {
					t.Fatalf("status = %+v", status)
				}
				if scope, _ := f.s.store.AgentThreadProjectForUser(1, f.agent.ID, "t"); scope != "" {
					t.Fatalf("left scope %q behind for a thread that doesn't exist", scope)
				}
			})
		}
	}
}

// The running Core is the last word: its pool can be narrower than the server's.
func TestThreadProviderCoreRefusalIsReportedAsUnavailable(t *testing.T) {
	for _, route := range threadProviderRoutes {
		t.Run(route, func(t *testing.T) {
			core := newThreadProviderCore() // holds nothing the app can name
			f := newThreadProviderFixture(t, core)
			rec, _ := f.request(route, sdk.ThreadSpawnRequest{ThreadID: "t", ProjectID: "proj-1", Provider: "anthropic"})
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "unknown provider") {
				t.Fatalf("status=%d body=%s, want 422 with Core's reason", rec.Code, rec.Body.String())
			}
			if scope, _ := f.s.store.AgentThreadProjectForUser(1, f.agent.ID, "t"); scope != "" {
				t.Fatalf("left scope %q behind", scope)
			}
		})
	}
}

func TestThreadProviderNeedsThreadsWrite(t *testing.T) {
	for _, route := range threadProviderRoutes {
		t.Run(route, func(t *testing.T) {
			core := newThreadProviderCore("anthropic")
			f := newThreadProviderFixture(t, core)
			if _, err := f.s.store.db.Exec(`UPDATE app_installs SET permissions_json='[]' WHERE id=?`, f.installID); err != nil {
				t.Fatal(err)
			}
			rec, _ := f.request(route, sdk.ThreadSpawnRequest{ThreadID: "t", Provider: "anthropic"})
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s, want 403", rec.Code, rec.Body.String())
			}
			if got := core.allCalls(); got != "" {
				t.Fatalf("core was called: %s", got)
			}
		})
	}
}

func TestThreadProviderRepeatIsIdempotentAndStillDelivers(t *testing.T) {
	for _, route := range threadProviderRoutes {
		t.Run(route, func(t *testing.T) {
			core := newThreadProviderCore("anthropic")
			f := newThreadProviderFixture(t, core)
			first := sdk.ThreadSpawnRequest{ThreadID: "t", Provider: "anthropic"}
			if rec, _ := f.request(route, first); rec.Code != http.StatusOK {
				t.Fatalf("first status=%d body=%s", rec.Code, rec.Body.String())
			}
			repeat := first
			repeat.Events = []sdk.ThreadEvent{{ID: "e2", Message: "more"}}
			rec, status := f.request(route, repeat)
			if rec.Code != http.StatusOK {
				t.Fatalf("repeat status=%d body=%s", rec.Code, rec.Body.String())
			}
			body := core.lastBody(t)
			if ids := threadEventIDs(body); len(ids) != 1 || ids[0] != "e2" {
				t.Fatalf("events not delivered: %v", body)
			}
			if _, sent := body["provider"]; sent {
				t.Fatalf("repeat sent the provider again: %v", body)
			}
			if status == nil || status.Pin != "anthropic" {
				t.Fatalf("status = %+v", status)
			}
		})
	}
}

func TestThreadProviderDifferentChoiceConflictsAndChangesNothing(t *testing.T) {
	for _, route := range threadProviderRoutes {
		t.Run(route, func(t *testing.T) {
			core := newThreadProviderCore("anthropic", "openai")
			f := newThreadProviderFixture(t, core)
			core.rows["t"] = map[string]any{"id": "t", "directive": "main old", "provider": "anthropic", "provider_pin": "anthropic"}
			rec, _ := f.request(route, sdk.ThreadSpawnRequest{
				ThreadID: "t", Provider: "openai", DirectiveSuffix: " new", Events: []sdk.ThreadEvent{{ID: "e1", Message: "go"}},
			})
			if rec.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body.String())
			}
			if got := core.writes(); len(got) != 0 {
				t.Fatalf("core was written to: %v", got)
			}
			if core.rows["t"]["directive"] != "main old" {
				t.Fatalf("thread changed: %v", core.rows["t"])
			}
		})
	}
}

// A pin outlives its provider. Refusing the repeat would cut off the thread's
// events exactly when Core is keeping it alive on the parent's provider.
func TestThreadProviderRepeatWorksWhileThePinnedProviderIsGone(t *testing.T) {
	for _, route := range threadProviderRoutes {
		t.Run(route, func(t *testing.T) {
			core := newThreadProviderCore("openai")
			f := newThreadProviderFixture(t, core)
			core.rows["t"] = map[string]any{"id": "t", "directive": "main", "provider": "openai", "provider_pin": "google", "provider_missing": true}
			rec, status := f.request(route, sdk.ThreadSpawnRequest{
				ThreadID: "t", Provider: "google", Events: []sdk.ThreadEvent{{ID: "e1", Message: "go"}},
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if ids := threadEventIDs(core.lastBody(t)); len(ids) != 1 {
				t.Fatalf("events not delivered: %v", core.lastBody(t))
			}
			if status == nil || status.Pin != "google" || status.Active != "openai" || !status.Missing {
				t.Fatalf("status = %+v", status)
			}
		})
	}
}

func TestThreadProviderAgainstAnUnpinnedThreadComparesWhatItIsOn(t *testing.T) {
	for _, route := range threadProviderRoutes {
		for requested, wantCode := range map[string]int{"openai": http.StatusOK, "anthropic": http.StatusConflict} {
			t.Run(route+"/"+requested, func(t *testing.T) {
				core := newThreadProviderCore("anthropic", "openai")
				f := newThreadProviderFixture(t, core)
				core.rows["t"] = map[string]any{"id": "t", "directive": "main", "provider": "openai"}
				rec, _ := f.request(route, sdk.ThreadSpawnRequest{ThreadID: "t", Provider: requested})
				if rec.Code != wantCode {
					t.Fatalf("status=%d body=%s, want %d", rec.Code, rec.Body.String(), wantCode)
				}
			})
		}
	}
}

func TestEnsureProfileRepairNeverSendsAProvider(t *testing.T) {
	for name, requested := range map[string]string{"omitted": "", "same": "anthropic"} {
		t.Run(name, func(t *testing.T) {
			core := newThreadProviderCore("anthropic")
			f := newThreadProviderFixture(t, core)
			core.rows["t"] = map[string]any{"id": "t", "directive": "main old", "provider": "anthropic", "provider_pin": "anthropic"}
			rec, status := f.request("ensure", sdk.ThreadSpawnRequest{ThreadID: "t", DirectiveSuffix: " new", Provider: requested})
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if got := strings.Join(core.writes(), ","); got != "PUT /threads/t" {
				t.Fatalf("writes = %s", got)
			}
			if _, sent := core.lastBody(t)["provider"]; sent {
				t.Fatalf("PUT carried a provider: %v", core.lastBody(t))
			}
			if status == nil || status.Pin != "anthropic" {
				t.Fatalf("status = %+v", status)
			}
		})
	}
}

// A Core started before pins existed ignores the field, and it can outlive a
// server upgrade. The app asked for one provider, so a worker quietly running on
// another is removed and the app is told.
func TestThreadProviderIgnoredByAnOlderCoreRemovesTheThread(t *testing.T) {
	for _, route := range threadProviderRoutes {
		t.Run(route, func(t *testing.T) {
			core := newThreadProviderCore("anthropic")
			core.ignoresPins = true
			f := newThreadProviderFixture(t, core)
			rec, _ := f.request(route, sdk.ThreadSpawnRequest{ThreadID: "t", ProjectID: "proj-1", Provider: "anthropic"})
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status=%d body=%s, want 422", rec.Code, rec.Body.String())
			}
			if _, alive := core.rows["t"]; alive {
				t.Fatal("thread left running on the wrong provider")
			}
			if scope, _ := f.s.store.AgentThreadProjectForUser(1, f.agent.ID, "t"); scope != "" {
				t.Fatalf("left scope %q behind", scope)
			}
		})
	}
}

func TestThreadProviderLostRaceAppliesTheSameRules(t *testing.T) {
	for _, route := range threadProviderRoutes {
		for winner, wantCode := range map[string]int{"anthropic": http.StatusOK, "openai": http.StatusConflict} {
			t.Run(route+"/"+winner, func(t *testing.T) {
				core := newThreadProviderCore("anthropic", "openai")
				core.raceWinner = map[string]any{"id": "t", "directive": "main", "provider": winner, "provider_pin": winner}
				f := newThreadProviderFixture(t, core)
				rec, status := f.request(route, sdk.ThreadSpawnRequest{ThreadID: "t", Provider: "anthropic"})
				if rec.Code != wantCode {
					t.Fatalf("status=%d body=%s, want %d", rec.Code, rec.Body.String(), wantCode)
				}
				if wantCode == http.StatusOK && (status == nil || status.Pin != winner) {
					t.Fatalf("status = %+v", status)
				}
			})
		}
	}
}

// Core accepted the choice and the thread is live, so a failed read-back must
// not fail the call: the handler would strip the scope from a running thread.
func TestThreadProviderFailedReadBackKeepsTheThread(t *testing.T) {
	for _, route := range threadProviderRoutes {
		t.Run(route, func(t *testing.T) {
			core := newThreadProviderCore("anthropic")
			core.failReads = true
			f := newThreadProviderFixture(t, core)
			rec, status := f.request(route, sdk.ThreadSpawnRequest{ThreadID: "t", ProjectID: "proj-1", Provider: "anthropic"})
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if status != nil {
				t.Fatalf("status = %+v, want unknown", status)
			}
			if _, alive := core.rows["t"]; !alive {
				t.Fatal("thread removed")
			}
			if scope, _ := f.s.store.AgentThreadProjectForUser(1, f.agent.ID, "t"); scope != "proj-1" {
				t.Fatalf("scope = %q", scope)
			}
		})
	}
}

func TestThreadProviderResponsesCarryNoCredentials(t *testing.T) {
	core := newThreadProviderCore("anthropic", "openai")
	f := newThreadProviderFixture(t, core)
	for _, requested := range []string{"anthropic", "openai", "google"} {
		rec, _ := f.request("spawn", sdk.ThreadSpawnRequest{ThreadID: "t", Provider: requested})
		if strings.Contains(rec.Body.String(), "sk-do-not-leak") {
			t.Fatalf("%s response leaked a key: %s", requested, rec.Body.String())
		}
	}
	for _, body := range core.bodies {
		raw, _ := json.Marshal(body)
		if strings.Contains(string(raw), "sk-do-not-leak") {
			t.Fatalf("spawn payload carried a key: %s", raw)
		}
	}
}
