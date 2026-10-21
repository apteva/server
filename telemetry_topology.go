package main

import (
	"context"
	"encoding/json"
	sdk "github.com/apteva/app-sdk"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (s *Server) handleTelemetryTopology(w http.ResponseWriter, r *http.Request, p telemetryPrincipal, path string) {
	if !p.agents {
		http.Error(w, "missing agent inspection permission", 403)
		return
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(path, "/agents/"), "/topology")
	id, e := strconv.ParseInt(raw, 10, 64)
	if e != nil || id <= 0 {
		http.Error(w, "invalid agent_id", 400)
		return
	}
	scope, e := s.telemetryAgents(p, sdk.TelemetryQuery{AgentID: id, ProjectID: p.project})
	if e != nil {
		http.NotFound(w, r)
		return
	}
	a := scope[id]
	out := sdk.AgentTopology{ServerID: s.telemetryServerID(), AgentID: id, RuntimeID: a.runtime, Threads: []sdk.ObservedThread{}, Attachments: []sdk.ObservedAttachment{}}
	agent, e := s.store.GetAgentByID(id)
	var port int
	var key string
	if a.runtimeAgent != nil {
		port = a.runtimeAgent.Port
		key = a.runtimeAgent.APIKey
	} else if s.agents != nil {
		port = s.agents.GetPort(id)
		key = s.agents.GetCoreAPIKey(id)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if port > 0 {
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:"+strconv.Itoa(port)+"/threads", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		if response, err := (&http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req); err == nil {
			defer response.Body.Close()
			if response.StatusCode == 200 {
				var rows []struct {
					ID     string `json:"id"`
					Parent string `json:"parent_id"`
					Rate   string `json:"rate"`
				}
				if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&rows) == nil && len(rows) <= 1000 {
					out.ThreadsAvailable = true
					for _, row := range rows {
						if len(row.ID) > 256 || len(row.Parent) > 256 {
							continue
						}
						status := ""
						if runtimeContains([]string{"running", "stopped", "sleeping", "waiting", "paused", "idle"}, row.Rate) {
							status = row.Rate
						}
						out.Threads = append(out.Threads, sdk.ObservedThread{ID: row.ID, ParentID: row.Parent, Status: status})
					}
				}
			}
		}
	} else if e == nil {
		// A stopped agent's saved config contains explicit parent relationships.
		var cfg struct {
			Threads []sdk.ObservedThread `json:"threads"`
		}
		if json.Unmarshal([]byte(agent.Config), &cfg) == nil {
			out.ThreadsAvailable = true
			out.Threads = append(out.Threads, sdk.ObservedThread{ID: "main", Status: "stopped"})
			for _, t := range cfg.Threads {
				t.Status = "stopped"
				out.Threads = append(out.Threads, t)
			}
		}
	}
	// Reuse actual live/saved MCP configuration; include only identities and
	// explicit tool filters. A missing tool filter is unknown, not "all tools".
	bound := map[int64]bool{}
	bindingRows, bindErr := s.store.db.QueryContext(r.Context(), `SELECT b.install_id FROM app_agent_bindings b JOIN app_installs i ON i.id=b.install_id WHERE b.agent_id=? AND (COALESCE(i.project_id,'')='' OR i.project_id=?)`, id, a.project)
	if bindErr == nil {
		for bindingRows.Next() {
			var boundID int64
			if bindingRows.Scan(&boundID) == nil {
				bound[boundID] = true
			}
		}
		bindingRows.Close()
	}
	if agent != nil && s.agents != nil {
		servers, err := s.currentAgentMCPServers(agent, 0)
		if err == nil {
			out.AttachmentsAvailable = true
			for _, entry := range servers {
				name, _ := entry["name"].(string)
				if name == "" || len(name) > 256 {
					continue
				}
				attachment := sdk.ObservedAttachment{Name: name, Tools: []string{}}
				if mcpID, ok := localMCPServerIDFromConfig(entry); ok {
					record, err := s.store.GetMCPServerByIDUnscoped(mcpID)
					if err != nil || record == nil || record.ProjectID != "" && record.ProjectID != a.project {
						continue
					}
					if record.Source == "app" {
						attachment.InstallID, _ = strconv.ParseInt(strings.TrimPrefix(record.UpstreamID, "app:"), 10, 64)
					}
					for _, tool := range record.AllowedTools {
						if telemetryIdentifier(tool) {
							attachment.Tools = append(attachment.Tools, tool)
						}
					}
				}
				for _, filter := range []string{"tools", "allowed_tools"} {
					if raw, ok := entry[filter].([]any); ok && len(raw) > 0 {
						attachment.Tools = []string{}
						for _, v := range raw {
							if name, ok := v.(string); ok && telemetryIdentifier(name) {
								attachment.Tools = append(attachment.Tools, name)
							}
						}
						attachment.ToolsAvailable = true
						break
					}
				}
				if attachment.InstallID == 0 {
					candidate := appInstallIDFromMCPConfig(entry)
					if bound[candidate] {
						attachment.InstallID = candidate
					}
				}
				if len(attachment.Tools) > 0 {
					attachment.ToolsAvailable = true
				}
				out.Attachments = append(out.Attachments, attachment)
			}
		}
	}
	sort.Slice(out.Attachments, func(i, j int) bool { return out.Attachments[i].Name < out.Attachments[j].Name })
	writeJSON(w, out)
}
