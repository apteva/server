package main

// Lets apps pin a thread they spawn to a provider from the agent's pool. Core
// owns the pin: it stores it, restores it after a restart and keeps the worker
// running if the provider goes away. The server adds what Core can't see, which
// is whether this agent's owner and project may currently use that provider, and
// refuses to move a thread that is already running on another one.

import (
	"errors"
	"fmt"
	"log"
	"net/http"

	sdk "github.com/apteva/app-sdk"
	"github.com/apteva/server/apps/framework"
)

// threadProviderChoice is an app's request for a provider. The zero value means
// the app didn't choose, and everything behaves as it did before pins existed.
type threadProviderChoice struct {
	name string
	// refuseNew says why a new thread can't use this provider. It doesn't apply
	// to a thread that already holds this pin: Core keeps such a thread running
	// while its provider is away, and refusing would stop its event delivery. It
	// is a func so that delivering events to a live thread doesn't load the pool.
	refuseNew func() error
}

// threadProviderUnavailableError means no thread was created because the agent
// can't use the provider.
type threadProviderUnavailableError struct {
	provider string
	reason   string
}

func (e *threadProviderUnavailableError) Error() string {
	return fmt.Sprintf("provider %q is not available to this agent: %s", e.provider, e.reason)
}

// threadProviderConflictError means the thread exists on a different provider.
type threadProviderConflictError struct {
	threadID  string
	requested string
	current   string
}

func (e *threadProviderConflictError) Error() string {
	return fmt.Sprintf("thread %q already uses provider %q, not %q; running threads are not moved", e.threadID, e.current, e.requested)
}

func (s *Server) threadProviderChoiceFor(agent *Agent, requested string) threadProviderChoice {
	name := providerKeyFromName(requested)
	if name == "" {
		return threadProviderChoice{}
	}
	return threadProviderChoice{name: name, refuseNew: func() error { return s.threadProviderRefusal(agent, name) }}
}

func (c threadProviderChoice) refusalForNewThread() error {
	if c.refuseNew == nil {
		return nil
	}
	return c.refuseNew()
}

// threadProviderRefusal checks the pool this agent's owner and project may use
// right now. That can be narrower than what a running Core still holds, and only
// Core can confirm what it holds, so a request passes both checks.
func (s *Server) threadProviderRefusal(agent *Agent, name string) error {
	if isRealtimeProviderType(name) {
		return &threadProviderUnavailableError{provider: name, reason: "realtime providers are chosen through spawn-realtime"}
	}
	pool, excluded := eligibleProviderPoolWithExclusions(s.GetProviderPool(agent.UserID, agent.ProjectID))
	for _, info := range pool {
		if providerKeyFromName(info.Type) == name {
			return nil
		}
	}
	for _, exclusion := range excluded {
		if exclusion.Provider == name {
			return &threadProviderUnavailableError{provider: name, reason: exclusion.Reason}
		}
	}
	return &threadProviderUnavailableError{provider: name, reason: "not configured for this user and project"}
}

// conflictWith compares against the pin, not the provider the thread is on, so
// a pin whose provider is away still matches. A thread with no pin matches only
// if it is already on that provider, which also covers Core versions that don't
// report pins.
func (c threadProviderChoice) conflictWith(threadID string, row threadIDRow) error {
	if c.name == "" {
		return nil
	}
	current := row.ProviderPin
	if current == "" {
		current = row.Provider
	}
	if current == c.name {
		return nil
	}
	return &threadProviderConflictError{threadID: threadID, requested: c.name, current: current}
}

func threadProviderStatus(row threadIDRow) *sdk.ThreadProviderStatus {
	if row.Provider == "" && row.ProviderPin == "" {
		return nil
	}
	return &sdk.ThreadProviderStatus{Pin: row.ProviderPin, Active: row.Provider, Missing: row.ProviderMissing}
}

// spawnThreadOnProvider is SpawnOpaqueThreadWithEvents with a provider choice.
// It looks at the thread first because Core's POST answers "exists" for a thread
// that is already there and ignores the provider. Without a choice it adds no
// round trip.
func (r *serverResolver) spawnThreadOnProvider(inst framework.InstanceInfo, threadID, directiveSuffix string, tools, mcp []string, ephemeral bool, events []sdk.ThreadEvent, choice threadProviderChoice) (sdk.ThreadSpawnResult, error) {
	if choice.name == "" {
		return r.SpawnOpaqueThreadWithEvents(inst, threadID, directiveSuffix, tools, mcp, ephemeral, events)
	}
	if inst.Port == 0 {
		return sdk.ThreadSpawnResult{}, fmt.Errorf("agent %d has no core port — is it running?", inst.ID)
	}
	current, found, err := r.inspectOpaqueThread(inst, threadID)
	if err != nil {
		return sdk.ThreadSpawnResult{}, err
	}
	if found {
		if err := choice.conflictWith(threadID, current); err != nil {
			return sdk.ThreadSpawnResult{}, err
		}
		result, err := r.SpawnOpaqueThreadWithEvents(inst, threadID, directiveSuffix, tools, mcp, ephemeral, events)
		if err != nil {
			return sdk.ThreadSpawnResult{}, err
		}
		result.Provider = threadProviderStatus(current)
		return result, nil
	}
	if err := choice.refusalForNewThread(); err != nil {
		return sdk.ThreadSpawnResult{}, err
	}
	result, err := r.postOpaqueThread(inst, threadID, directiveSuffix, tools, mcp, ephemeral, events, choice.name)
	if err != nil {
		return sdk.ThreadSpawnResult{}, err
	}
	if result.Status == "exists" {
		// Another caller created it after our look. Core has already queued these
		// events on that thread, so a conflict here can't take them back.
		current, found, err := r.inspectOpaqueThread(inst, threadID)
		if err != nil {
			return sdk.ThreadSpawnResult{}, err
		}
		if found {
			if err := choice.conflictWith(threadID, current); err != nil {
				return sdk.ThreadSpawnResult{}, err
			}
			result.Provider = threadProviderStatus(current)
		}
		return result, nil
	}
	result.Provider, err = r.confirmThreadProvider(inst, threadID, choice.name)
	if err != nil {
		return sdk.ThreadSpawnResult{}, err
	}
	return result, nil
}

// confirmThreadProvider reads a new thread back. Core refuses a provider it
// doesn't have, but a Core process started before that fix keeps running through
// a server upgrade and silently uses its parent's provider instead, so that
// thread is removed rather than left on the wrong one.
//
// A failed read isn't an error. The thread exists and Core accepted the choice,
// so the caller gets a nil status, which means unknown.
func (r *serverResolver) confirmThreadProvider(inst framework.InstanceInfo, threadID, provider string) (*sdk.ThreadProviderStatus, error) {
	row, found, err := r.inspectOpaqueThread(inst, threadID)
	if err != nil {
		log.Printf("[THREAD-PROVIDER] agent=%d thread=%q: could not read back provider: %v", inst.ID, threadID, err)
		return nil, nil
	}
	if !found {
		return nil, nil
	}
	if row.ProviderPin == provider || row.Provider == provider {
		return threadProviderStatus(row), nil
	}
	log.Printf("[THREAD-PROVIDER] agent=%d thread=%q: asked for %q, Core used %q; removing the thread", inst.ID, threadID, provider, row.Provider)
	if err := r.KillThread(inst, threadID); err != nil {
		log.Printf("[THREAD-PROVIDER] agent=%d thread=%q: could not remove the thread: %v", inst.ID, threadID, err)
		return nil, fmt.Errorf("thread %q is running on %q, not %q, and could not be removed: %w", threadID, row.Provider, provider, err)
	}
	return nil, &threadProviderUnavailableError{provider: provider, reason: "the agent's running Core did not apply it"}
}

func writeThreadCallbackError(w http.ResponseWriter, err error) {
	var conflict *threadProviderConflictError
	var unavailable *threadProviderUnavailableError
	switch {
	case errors.As(err, &conflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.As(err, &unavailable):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	default:
		http.Error(w, err.Error(), http.StatusBadGateway)
	}
}
