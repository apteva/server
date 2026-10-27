package main

import (
	"errors"
	sdk "github.com/apteva/app-sdk"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAgentResourcesPolicyCASAndPersistence(t *testing.T) {
	s := newTestServer(t)
	a := behaviorAgent(t, s, "cautious")
	p := runtimeResourceRecord{Policy: sdk.ResourcePolicy{Process: sdk.ProcessResources{CPUWeight: 200}}}
	if err := s.store.saveAgentResources(a.ID, runtimeResourceRecord{Revision: 7}); err == nil {
		t.Fatal("nonzero revision created a missing policy")
	}
	if err := s.store.saveAgentResources(a.ID, p); err != nil {
		t.Fatal(err)
	}
	if err := s.store.saveAgentResources(a.ID, p); err == nil {
		t.Fatal("stale update accepted")
	}
	if err := s.store.migrateAgentResources(); err != nil {
		t.Fatal(err)
	}
	current, err := s.store.agentResourcePolicy(a.ID)
	if err != nil || current.Revision != 1 || current.Policy.Process.CPUWeight != 200 {
		t.Fatal(current, err)
	}
	loaded, _ := s.store.GetAgentByID(a.ID)
	if loaded.Config != a.Config {
		t.Fatal("resource policy altered model config")
	}
	if err := s.store.saveAgentResources(a.ID, runtimeResourceRecord{Revision: 1}); err != nil {
		t.Fatal(err)
	}
	current, _ = s.store.agentResourcePolicy(a.ID)
	if current.Revision != 2 || !current.Policy.Process.Empty() {
		t.Fatal("reset failed")
	}
}
func TestAgentResourcesAuthorizationAndValidation(t *testing.T) {
	s := newTestServer(t)
	a := behaviorAgent(t, s, "cautious")
	key := "agent-resource-admin"
	if _, err := s.store.CreateAPIKey(a.UserID, "test", HashAPIKey(key), "resources"); err != nil {
		t.Fatal(err)
	}
	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) { s.handleAgentResources(w, r, a.ID) })
	request := func(method, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/agents/1/resources", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler(w, r)
		return w
	}
	if w := request("PUT", "", "{\"revision\":0,\"policy\":{}}"); w.Code == 200 {
		t.Fatal("anonymous mutation")
	}
	if w := request("GET", key, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("PUT", key, `{"revision":0,"policy":{}}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("PUT", key, `{"revision":0,"policy":{}}`); w.Code != 409 {
		t.Fatal("stale revision", w.Code)
	}
	for _, policy := range []string{`{"process":{"cpu_weight":10001}}`, `{"process":{"cpu_quota_percent":50}}`, `{"total_concurrency":2}`, `{"network":{"media":{"dscp":1}}}`} {
		if w := request("PUT", key, `{"revision":1,"policy":`+policy+`}`); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	member, err := s.store.CreateUser("resource-member@example.test", "hash")
	if err != nil {
		t.Fatal(err)
	}
	memberKey := "resource-member-key"
	s.store.CreateAPIKey(member.ID, "member", HashAPIKey(memberKey), "resources")
	if w := request("PUT", memberKey, `{"revision":1,"policy":{}}`); w.Code != 403 {
		t.Fatal("member changed host scheduling", w.Code)
	}
	t.Setenv("APTEVA_RESOURCE_CEILINGS", `{"cpu_weight":150}`)
	if w := request("PUT", key, `{"revision":1,"policy":{"process":{"cpu_weight":200}}}`); w.Code != 400 {
		t.Fatal("ceiling bypass")
	}
	if len(s.agents.processes) != 0 {
		t.Fatal("saving started a core")
	}
}
func TestAgentResourcesLaunchLoadsPolicyAndFailsClosed(t *testing.T) {
	t.Setenv("APTEVA_CGROUP_ROOT", "")
	t.Setenv("APTEVA_APP_CGROUP_ROOT", "")
	s := newTestServer(t)
	a := behaviorAgent(t, s, "cautious")
	s.agents.LoadProcessResources = func(id int64) (runtimeResourceRecord, error) {
		if id != a.ID {
			t.Fatal("wrong agent")
		}
		return runtimeResourceRecord{}, errors.New("policy unavailable")
	}
	if err := s.agents.Start(a, nil, "1", nil, "test"); err == nil || !strings.Contains(err.Error(), "policy unavailable") {
		t.Fatal(err)
	}
	s.agents.LoadProcessResources = func(int64) (runtimeResourceRecord, error) {
		return runtimeResourceRecord{Policy: sdk.ResourcePolicy{Process: sdk.ProcessResources{CPUWeight: 200}}}, nil
	}
	if err := s.agents.Start(a, nil, "1", nil, "test"); err == nil {
		t.Fatal("explicit policy launched unenforced")
	}
	if len(s.agents.processes) != 0 {
		t.Fatal("failed enforcement left a running core")
	}
}
func TestProcessResourceDefaultsAndOwnerIsolation(t *testing.T) {
	t.Setenv("APTEVA_CGROUP_ROOT", "")
	t.Setenv("APTEVA_APP_CGROUP_ROOT", "/legacy-app-root")
	if p := resolveProcessResources(sdk.ProcessResources{}); !p.Empty() {
		t.Fatal("legacy default changed")
	}
	t.Setenv("APTEVA_CGROUP_ROOT", "/generic-root")
	if p := resolveProcessResources(sdk.ProcessResources{}); p.CPUWeight != 100 {
		t.Fatal(p)
	}
	if p := resolveProcessResources(sdk.ProcessResources{CPUWeight: 200}); p.CPUWeight != 200 {
		t.Fatal(p)
	}
	if processResourceRoot() != "/generic-root" {
		t.Fatal("generic root did not take precedence")
	}
	for _, o := range []processResourceOwner{{"agent", "../1"}, {"other", "1"}, {"app", ""}} {
		if _, e := o.prefix(); e == nil {
			t.Fatal("invalid resource identity")
		}
	}
	a, _ := (processResourceOwner{"agent", "1"}).prefix()
	b, _ := (processResourceOwner{"app", "1"}).prefix()
	if a == b {
		t.Fatal("agent/app collision")
	}
}
func TestAgentResourcesWaitCleansOnceAfterExit(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 0.05")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	r := &runningAgent{cmd: cmd, pid: cmd.Process.Pid, done: make(chan struct{}), releaseResources: func() { calls.Add(1) }}
	if calls.Load() != 0 {
		t.Fatal("cleaned live process")
	}
	if err := r.wait(); err != nil {
		t.Fatal(err)
	}
	r.wait()
	if calls.Load() != 1 {
		t.Fatal("cleanup not exactly once")
	}
}
func TestAgentResourcesAdoptedExitWatch(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 0.2")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	identity, err := readRuntimeProcessIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	r := &runningAgent{cmd: cmd, pid: cmd.Process.Pid, reattached: true, resourceIdentity: &identity, releaseResources: func() { close(done) }}
	r.watchResourceExit()
	select {
	case <-done:
		t.Fatal("reattachment cleaned surviving core")
	default:
	}
	cmd.Wait()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("adopted process cleanup not observed")
	}
}
func TestAgentResourcesUnsupportedHostAndDefaultLaunch(t *testing.T) {
	t.Setenv("APTEVA_CGROUP_ROOT", "")
	t.Setenv("APTEVA_APP_CGROUP_ROOT", "")
	cleanup, err := configureProcessResources(exec.Command("/bin/true"), processResourceOwner{"agent", "1"}, sdk.ProcessResources{})
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if runtime.GOOS != "linux" {
		if _, err = configureProcessResources(exec.Command("/bin/true"), processResourceOwner{"agent", "1"}, sdk.ProcessResources{CPUWeight: 200}); err == nil {
			t.Fatal("unsupported host claimed enforcement")
		}
	}
	// Policy stays server-owned even if a normal config JSON contains a lookalike.
	s := newTestServer(t)
	a := behaviorAgent(t, s, "cautious")
	a.Config = `{"resource_policy":{"process":{"cpu_weight":200}}}`
	if err = s.store.UpdateAgent(a); err != nil {
		t.Fatal(err)
	}
	got, err := s.store.agentResourcePolicy(a.ID)
	if err != nil || !got.Policy.Process.Empty() {
		t.Fatal("ordinary config escalated priority")
	}
}
func TestAgentResourcesFailedExecCleanup(t *testing.T) {
	t.Setenv("APTEVA_CGROUP_ROOT", "")
	t.Setenv("APTEVA_APP_CGROUP_ROOT", "")
	s := newTestServer(t)
	a := behaviorAgent(t, s, "cautious")
	a.Config = `{"include_channels":false,"include_apteva_server":false}`
	s.agents = NewAgentManager(t.TempDir(), filepath.Join(t.TempDir(), "missing-core"))
	s.agents.LoadProcessResources = s.store.agentResourcePolicy
	err := s.agents.Start(a, nil, "1", nil, "test")
	if err == nil {
		t.Fatal("missing binary started")
	}
	if len(s.agents.processes) != 0 {
		t.Fatal("failed launch retained process")
	}
}
