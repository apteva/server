package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolvedShutdownPolicyRequiresRestartOrUpdateIntent(t *testing.T) {
	s := &Server{dataDir: t.TempDir()}
	if got := s.resolvedShutdownPolicy(agentLifecycleIntent{Policy: "preserve"}); got != "restart" {
		t.Fatalf("undeclared shutdown policy = %q, want restart", got)
	}
	if got := s.resolvedShutdownPolicy(agentLifecycleIntent{Reason: "stop", Policy: "preserve"}); got != "restart" {
		t.Fatalf("explicit stop policy = %q, want restart", got)
	}
	if got := s.resolvedShutdownPolicy(agentLifecycleIntent{Reason: "update", Policy: "preserve"}); got != "preserve" {
		t.Fatalf("update policy = %q, want preserve", got)
	}
	if got := s.resolvedShutdownPolicy(agentLifecycleIntent{Reason: "restart", Policy: "rolling"}); got != "rolling" {
		t.Fatalf("restart policy = %q, want rolling", got)
	}
}

func TestResolvedShutdownPolicyUsesSavedDefaultWhenIntentHasNoOverride(t *testing.T) {
	t.Setenv("APTEVA_AGENT_UPDATE_POLICY", "rolling")
	s := &Server{dataDir: t.TempDir()}
	if got := s.resolvedShutdownPolicy(agentLifecycleIntent{Reason: "update"}); got != "rolling" {
		t.Fatalf("policy = %q, want saved rolling default", got)
	}
}

func TestAgentUpdatePolicyPreservesLegacyDetachSetting(t *testing.T) {
	t.Setenv("APTEVA_AGENT_UPDATE_POLICY", "")
	t.Setenv("APTEVA_AGENT_SHUTDOWN_POLICY", "detach")
	s := &Server{dataDir: t.TempDir()}
	if got := s.agentUpdatePolicy(); got != "preserve" {
		t.Fatalf("policy = %q, want preserve for legacy detach", got)
	}
}

func TestAgentRolloutScope(t *testing.T) {
	for _, tc := range []struct {
		all  bool
		ids  []int64
		want string
	}{
		{all: true, want: "all"},
		{ids: []int64{42}, want: "agent"},
		{ids: []int64{1, 2}, want: "agents"},
		{want: "project"},
	} {
		if got := agentRolloutScope(tc.all, tc.ids); got != tc.want {
			t.Fatalf("scope(%v, %v) = %q, want %q", tc.all, tc.ids, got, tc.want)
		}
	}
}

func TestLifecycleIntentExpiresAndIsRemoved(t *testing.T) {
	dir := t.TempDir()
	s := &Server{dataDir: dir}
	raw := []byte(`{"reason":"update","agent_policy":"preserve","created_at":"2020-01-01T00:00:00Z","expires_at":"2020-01-01T00:01:00Z"}`)
	if err := os.WriteFile(s.lifecycleIntentPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.readLifecycleIntent(false); got.Reason != "" {
		t.Fatalf("expired intent returned: %#v", got)
	}
	if _, err := os.Stat(s.lifecycleIntentPath()); !os.IsNotExist(err) {
		t.Fatalf("expired intent was not removed: %v", err)
	}
}

func TestLifecycleIntentWithoutOverrideKeepsPolicyEmpty(t *testing.T) {
	dir := t.TempDir()
	s := &Server{dataDir: dir}
	now := time.Now().UTC()
	raw, err := json.Marshal(agentLifecycleIntent{Reason: "update", CreatedAt: now, ExpiresAt: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.lifecycleIntentPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.readLifecycleIntent(false); got.Policy != "" {
		t.Fatalf("policy = %q, want no one-run override", got.Policy)
	}
}

func TestAgentRolloutIsSerialAndContinuesAfterFailure(t *testing.T) {
	var active int32
	var maxActive int32
	var mu sync.Mutex
	order := []int64{}
	r := newAgentRolloutCoordinator(func(_ context.Context, id int64) error {
		current := atomic.AddInt32(&active, 1)
		defer atomic.AddInt32(&active, -1)
		for {
			max := atomic.LoadInt32(&maxActive)
			if current <= max || atomic.CompareAndSwapInt32(&maxActive, max, current) {
				break
			}
		}
		mu.Lock()
		order = append(order, id)
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		if id == 2 {
			return errors.New("test failure")
		}
		return nil
	})
	if _, err := r.start([]int64{1, 2, 3}, map[int64]string{1: "one", 2: "two", 3: "three"}, "all", "", "1.2.3", 0); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && r.snapshot().State == "running" {
		time.Sleep(time.Millisecond)
	}
	status := r.snapshot()
	if status.State != "completed" || status.Completed != 2 || status.Failed != 1 {
		t.Fatalf("unexpected rollout result: %#v", status)
	}
	if atomic.LoadInt32(&maxActive) != 1 {
		t.Fatalf("maximum concurrent updates = %d, want 1", maxActive)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("update order = %v", order)
	}
}

func TestAgentRolloutCancellationStopsBeforeNextAgent(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls int32
	r := newAgentRolloutCoordinator(func(ctx context.Context, _ int64) error {
		atomic.AddInt32(&calls, 1)
		close(started)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	})
	if _, err := r.start([]int64{1, 2}, nil, "all", "", "1.2.3", time.Second); err != nil {
		t.Fatal(err)
	}
	<-started
	if !r.stop() {
		t.Fatal("cancel request was not accepted")
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && r.snapshot().State == "running" {
		time.Sleep(time.Millisecond)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("updates called %d times after cancellation, want 1", got)
	}
	if got := r.snapshot().State; got != "cancelled" {
		t.Fatalf("state = %q, want cancelled", got)
	}
}
