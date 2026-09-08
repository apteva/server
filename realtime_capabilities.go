package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	sdk "github.com/apteva/app-sdk"
	"github.com/apteva/server/apps/framework"
)

// ThreadRealtimeCapabilities never upgrades grants into provider evidence.
// Both reads share a deadline. Read failures are diagnostic state, so callers
// must never interpret them as failure of a prior successful spawn.
func (r *serverResolver) ThreadRealtimeCapabilities(inst framework.InstanceInfo, threadID string) *sdk.RealtimeCapabilities {
	return r.threadRealtimeCapabilities(context.Background(), inst, threadID)
}

func (r *serverResolver) threadRealtimeCapabilities(ctx context.Context, inst framework.InstanceInfo, threadID string) *sdk.RealtimeCapabilities {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	unknown := &sdk.RealtimeCapabilities{ThreadID: threadID, Status: "unknown"}
	if inst.Port == 0 {
		unknown.Error = "agent is not running"
		return unknown
	}
	var snapshot sdk.RealtimeCapabilities
	err := readRealtimeCapabilityJSON(ctx, inst, "/threads/"+url.PathEscape(threadID)+"/capabilities", &snapshot)
	if err == nil && snapshot.Version == 1 && snapshot.ThreadID == threadID {
		switch snapshot.Status {
		case "unknown", "pending", "failed":
			snapshot.PresentationVerified = false
		case "ready":
			if !snapshot.Verified() {
				snapshot.Status = "unknown"
				snapshot.PresentationVerified = false
				snapshot.Error = "core readiness evidence is incomplete or stale"
			}
		default:
			snapshot.Status = "unknown"
			snapshot.PresentationVerified = false
			snapshot.Error = "unrecognized core capability status"
		}
		return &snapshot
	}
	if err != nil {
		unknown.Error = "core capability evidence unavailable: " + err.Error()
	} else {
		unknown.Error = "unsupported or mismatched core capability response"
	}
	// Compatibility with cores exposing only stored grants via /threads.
	var rows []threadIDRow
	if err := readRealtimeCapabilityJSON(ctx, inst, "/threads", &rows); err != nil {
		return unknown
	}
	for _, row := range rows {
		if row.ID == threadID {
			unknown.GrantsVerified = true
			unknown.GrantedTools = nonNilStrings(append([]string(nil), row.Tools...))
			unknown.GrantedMCP = nonNilStrings(append([]string(nil), row.MCPNames...))
			break
		}
	}
	return unknown
}

func readRealtimeCapabilityJSON(ctx context.Context, inst framework.InstanceInfo, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", inst.Port, path), nil)
	if err != nil {
		return err
	}
	if inst.CoreAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+inst.CoreAPIKey)
	}
	resp, err := coreProxyClient.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("capability read timed out")
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("capability read canceled")
		default:
			return fmt.Errorf("core unavailable")
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 4<<20 {
		return fmt.Errorf("capability response too large")
	}
	return json.Unmarshal(data, out)
}

func applyRealtimeCapabilityResult(result *sdk.RealtimeSpawnResult, capabilities *sdk.RealtimeCapabilities) {
	result.Capabilities = capabilities
	result.CapabilitiesVerified = capabilities.Verified()
	result.EffectiveTools = nil
	result.EffectiveMCP = nil
	if result.CapabilitiesVerified {
		result.EffectiveTools = nonNilStrings(append([]string(nil), capabilities.PresentedTools...))
		result.EffectiveMCP = nonNilStrings(append([]string(nil), capabilities.ConnectedMCP...))
	}
}

// Read only: checking readiness neither starts an agent nor renews an audio token.
func (s *Server) handleCallbackRealtimeCapabilities(w http.ResponseWriter, r *http.Request, installID int64, threadID string) {
	agentID, err := strconv.ParseInt(r.URL.Query().Get("agent_id"), 10, 64)
	if err != nil || agentID <= 0 || strings.TrimSpace(threadID) == "" || strings.Contains(threadID, "/") || len(threadID) > 128 {
		http.Error(w, "valid agent_id and thread_id required", http.StatusBadRequest)
		return
	}
	agent, err := s.callbackAgentForInstall(r, installID, agentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	inst := framework.InstanceInfo{ID: agent.ID, Port: s.agents.GetPort(agent.ID), CoreAPIKey: s.agents.GetCoreAPIKey(agent.ID)}
	writeJSON(w, s.resolver().threadRealtimeCapabilities(r.Context(), inst, threadID))
}
