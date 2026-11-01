package main

// agent_project_grant_fanout.go runs one granted, read-only app tool call in
// the agent's own project and every project its grant reaches, and returns a
// single reply with one entry per project.
//
// Each per-project call goes back through handleAppProxy with a concrete
// project_id, so it is routed, authorised, admitted and logged exactly like a
// call the agent made itself. A project that fails does not fail the others.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
)

var errGrantFanOutTooLarge = errors.New("reply too large")

type grantFanOutResult struct {
	ProjectID string `json:"project_id"`
	Project   string `json:"project"`
	Result    any    `json:"result,omitempty"`
	Error     string `json:"error,omitempty"`
	size      int
}

func (s *Server) fanOutGrantCall(w http.ResponseWriter, r *http.Request, request []byte, rpc map[string]any, agent *Agent, g *AgentProjectGrant, home *InstalledApp, tool string) {
	fail := func(err error) { writePlatformGatewayToolError(w, request, err) }
	if !g.toolAllowed(tool) {
		fail(fmt.Errorf("tool %q is not granted outside this agent's project", tool))
		return
	}
	for _, spec := range home.Manifest.Provides.MCPTools {
		if spec.Name == tool && spec.AsyncResult != nil {
			fail(fmt.Errorf("%s finishes asynchronously, so it cannot run in every project at once; name one project", tool))
			return
		}
	}
	readOnly, err := s.appToolReadOnly(home, tool)
	if err != nil {
		fail(fmt.Errorf("could not check whether %s only reads data: %v", tool, err))
		return
	}
	if !readOnly {
		fail(fmt.Errorf("%s is not marked read-only, so it cannot run in every project at once; name one project", tool))
		return
	}

	homeName, _ := s.projectName(agent.ProjectID)
	projects := append([]grantProject{{ID: agent.ProjectID, Name: homeName}}, s.grantProjects(agent, g)...)
	skipped := []grantProject{}
	if len(projects) > grantFanOutLimit {
		skipped = projects[grantFanOutLimit:]
		projects = projects[:grantFanOutLimit]
	}
	results := make([]grantFanOutResult, len(projects))
	slots := make(chan struct{}, grantFanOutWorkers)
	var wg sync.WaitGroup
	for i, p := range projects {
		wg.Add(1)
		go func(i int, p grantProject) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			results[i] = s.runGrantFanOutChild(r, rpc, p)
		}(i, p)
	}
	wg.Wait()

	total, failed := 0, 0
	for i := range results {
		total += results[i].size
		if total > grantFanOutTotalBytes {
			results[i].Result, results[i].Error = nil, "left out: the combined reply would exceed 32 MiB"
		}
		if results[i].Error != "" {
			failed++
		}
	}
	text, err := json.Marshal(map[string]any{"projects": results, "skipped": skipped})
	if err != nil {
		fail(err)
		return
	}
	log.Printf("[PROJECT-GRANT] fanout agent=%d app=%s tool=%s projects=%d failed=%d skipped=%d",
		agent.ID, home.AppName, tool, len(results), failed, len(skipped))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      rpc["id"],
		"result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(text)}},
			"isError": failed == len(results),
		},
	})
}

// runGrantFanOutChild replays the call for one project. The caller's thread
// and tool call id travel in the body, where the proxy reads them.
func (s *Server) runGrantFanOutChild(r *http.Request, rpc map[string]any, p grantProject) grantFanOutResult {
	params, _ := rpc["params"].(map[string]any)
	args, _ := params["arguments"].(map[string]any)
	childArgs := make(map[string]any, len(args)+3)
	for k, v := range args {
		childArgs[k] = v
	}
	childArgs[grantProjectParam] = p.ID
	if thread := r.Header.Get("X-Apteva-Caller-Thread"); thread != "" {
		childArgs["_apteva_caller_thread"] = thread
	}
	if callID := r.Header.Get("X-Apteva-Tool-Call-ID"); callID != "" {
		childArgs["_apteva_tool_call_id"] = callID
	}
	childParams := make(map[string]any, len(params))
	for k, v := range params {
		childParams[k] = v
	}
	childParams["arguments"] = childArgs
	child := make(map[string]any, len(rpc))
	for k, v := range rpc {
		child[k] = v
	}
	child["params"] = childParams
	payload, err := json.Marshal(child)
	if err != nil {
		return grantFanOutResult{ProjectID: p.ID, Project: p.Name, Error: err.Error()}
	}
	sub := r.Clone(r.Context())
	sub.Body = io.NopCloser(bytes.NewReader(payload))
	sub.ContentLength = int64(len(payload))
	sub.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(payload)), nil }
	reply := &grantFanOutWriter{header: http.Header{}}
	s.handleAppProxy(reply, sub)
	return reply.result(p)
}

// grantFanOutWriter buffers one per-project reply, up to 16 MiB.
type grantFanOutWriter struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	overflow bool
}

func (w *grantFanOutWriter) Header() http.Header { return w.header }

func (w *grantFanOutWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *grantFanOutWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.overflow || w.body.Len()+len(p) > grantFanOutChildBytes {
		w.overflow = true
		return 0, errGrantFanOutTooLarge
	}
	return w.body.Write(p)
}

func (w *grantFanOutWriter) Flush() {}

// result turns the app's MCP reply into the project's entry: the tool's
// structured or JSON output as-is, otherwise its text.
func (w *grantFanOutWriter) result(p grantProject) grantFanOutResult {
	out := grantFanOutResult{ProjectID: p.ID, Project: p.Name, size: w.body.Len()}
	if w.overflow {
		out.Error = "reply over 16 MiB"
		return out
	}
	if w.status != 0 && w.status != http.StatusOK {
		out.Error = truncate(strings.TrimSpace(w.body.String()), 500)
		if out.Error == "" {
			out.Error = http.StatusText(w.status)
		}
		return out
	}
	var reply struct {
		Result *struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			StructuredContent any  `json:"structuredContent"`
			IsError           bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if decodeMCPProxyRequest(w.body.Bytes(), &reply) != nil || (reply.Result == nil && reply.Error == nil) {
		out.Error = "unreadable reply"
		return out
	}
	if reply.Error != nil {
		out.Error = reply.Error.Message
		return out
	}
	texts := make([]string, 0, len(reply.Result.Content))
	for _, c := range reply.Result.Content {
		if c.Type == "text" {
			texts = append(texts, c.Text)
		}
	}
	text := strings.Join(texts, "\n")
	switch {
	case reply.Result.IsError:
		out.Error = strings.TrimPrefix(text, "error: ")
	case reply.Result.StructuredContent != nil:
		out.Result = reply.Result.StructuredContent
	default:
		var parsed any
		if decodeMCPProxyRequest([]byte(text), &parsed) == nil {
			out.Result = parsed
		} else {
			out.Result = text
		}
	}
	return out
}
