package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestEval_InEnvironment_RealLLM_MediaVideoProcessor proves the full
// event-driven media workflow through the Environment eval runner:
// bind media to an agent, derive an Environment, auto-install media's storage
// dependency, subscribe the transient Environment agent to media.completed, seed
// five local video uploads in one burst, and let OpenAI Codex drive the agent
// through real media tools.
//
// Gated: skips without OpenAI Codex provider auth, the core binary, app sources,
// or the local sample fixture. Run:
//
//	go test -run TestEval_InEnvironment_RealLLM_MediaVideoProcessor -v -timeout 900s
func TestEval_InEnvironment_RealLLM_MediaVideoProcessor(t *testing.T) {
	runMediaVideoProcessorEval(t, 5, true)
}

func TestEval_InEnvironment_RealLLM_MediaVideoProcessor_SmallBatchOnMain(t *testing.T) {
	for _, sourceVideoCount := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d_videos", sourceVideoCount), func(t *testing.T) {
			runMediaVideoProcessorEval(t, sourceVideoCount, false)
		})
	}
}

func runMediaVideoProcessorEval(t *testing.T, sourceVideoCount int, wantWorkerThreads bool) {
	t.Helper()
	providerState := loadOpenAICodexProviderState(t)
	corePath := findCoreBinary(t)
	mediaSrc := findAppSource(t, "media")
	_ = findAppSource(t, "storage")
	fixtureDir := filepath.Join(mediaSrc, "scenarios")
	fixturePath := filepath.Join(fixtureDir, "fixtures", "sample-5s.mp4")
	if _, err := filepath.Abs(fixturePath); err != nil {
		t.Fatalf("resolve fixture: %v", err)
	}
	if !localSampleFileExists(fixturePath) {
		t.Skipf("media sample fixture not found at %s", fixturePath)
	}

	directive := `You process incoming videos using the media app.

When you receive media.completed app events for source videos in /incoming/:
1. Process every new source video, even if multiple events arrive close together.
2. Use media_extract_frame once per source video to create a screenshot.
3. Use media_extract_reel once per source video to create a short vertical reel.
4. Use media_get_render until each render is ok or failed.
5. Reply with every source file_id and each output_file_id.

Ignore media.completed events for your own render outputs in /renders/ or /.media/.`
	s, userID, agent := setupRealServerWithProviderState(t, corePath, "media-video-processor", directive, 15, "llm", "OpenAI Codex", providerState)
	t.Cleanup(func() { s.agents.StopAll(3 * time.Second) })

	if err := prewarmMetaAgent(t, s, userID, 45*time.Second); err != nil {
		t.Fatalf("prewarm meta-agent: %v", err)
	}

	// Bind only media. Environment dependency expansion should install storage
	// from media's requires.apps, then bind media -> storage inside the Environment.
	seedBoundApp(t, s, "media", "", agent.ID)

	maxTurns := 20
	if sourceVideoCount > 1 {
		maxTurns = 28
	}
	if sourceVideoCount >= 5 {
		maxTurns = 40
	}
	goals := []string{
		fmt.Sprintf("The agent reacts to all %s for the %s seeded source video%s.", mediaCompletedEventText(sourceVideoCount), countLabel(sourceVideoCount), pluralS(sourceVideoCount)),
		fmt.Sprintf("The agent creates a screenshot/frame and a short vertical reel for each of the %s source video%s.", countLabel(sourceVideoCount), pluralS(sourceVideoCount)),
		"The agent polls render status and reports completed output file IDs.",
	}
	if wantWorkerThreads {
		goals = append(goals,
			"The main thread spawns one worker thread per source video instead of processing the whole burst on main.",
			"Each worker owns one assigned source video and reports completed output file IDs back to main.",
		)
	} else {
		goals = append(goals,
			"The agent handles this small batch on the main thread without spawning worker threads.",
		)
	}
	ev, err := s.store.CreateAgentEval(Eval{
		AgentID:     agent.ID,
		Name:        fmt.Sprintf("process %s incoming video%s after media.completed", countLabel(sourceVideoCount), pluralS(sourceVideoCount)),
		Source:      "user",
		MaxTurns:    maxTurns,
		Description: fmt.Sprintf("Wait for the %s for the %s uploaded sample video%s. Treat them as incoming source video%s. For each source video, create one screenshot/frame and one short vertical reel, poll every render job, and report each source file_id plus output_file_id values.", mediaCompletedEventText(sourceVideoCount), countLabel(sourceVideoCount), pluralS(sourceVideoCount), pluralS(sourceVideoCount)),
		Goals:       goals,
	})
	if err != nil {
		t.Fatalf("create eval: %v", err)
	}

	timeout := 5 * time.Minute
	if sourceVideoCount >= 5 {
		timeout = 8 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	run, err := s.runEval(ctx, userID, agent, ev, RunOptions{
		UseEnvironment: true,
		SeedAfterSpawn: true,
		SeedBaseDir:    fixtureDir,
		AppEventSubscriptions: []RunAppEventSubscription{
			{App: "media", Topic: "media.completed", ThreadID: "main"},
		},
		SeedPlan: mediaBurstSeedPlan(sourceVideoCount),
	})
	if err != nil {
		t.Fatalf("runEval: %v", err)
	}

	t.Logf("status=%s turns=%d", run.Status, run.TurnsUsed)
	for _, turn := range run.Trajectory.Turns {
		switch {
		case turn.ToolCall != nil:
			t.Logf("  TOOL %s.%s args=%s resp=%.220s", turn.ToolCall.App, turn.ToolCall.Tool,
				string(turn.ToolCall.Args), string(turn.ToolCall.Response))
		case turn.Role == "agent" && turn.Content != "":
			t.Logf("  AGENT %.220s", turn.Content)
		}
	}
	if run.Verdict != nil {
		t.Logf("verdict=%s reasoning=%.260s", run.Verdict.Overall, run.Verdict.Reasoning)
	}
	if run.Status == "error" {
		t.Fatalf("eval errored (infra, not agent behavior): %s", run.ErrorMessage)
	}

	frameSources := map[string]bool{}
	reelSources := map[string]bool{}
	outputIDs := map[string]bool{}
	renderPolls := 0
	spawnCalls := 0
	for _, turn := range run.Trajectory.Turns {
		if turn.ToolCall == nil {
			continue
		}
		name := strings.ToLower(turn.ToolCall.App + " " + turn.ToolCall.Tool)
		switch {
		case strings.Contains(name, "spawn"):
			spawnCalls++
		case strings.Contains(name, "media_extract_frame"):
			if fid := fileIDFromToolArgs(turn.ToolCall.Args); fid != "" {
				frameSources[fid] = true
			}
		case strings.Contains(name, "media_extract_reel"):
			if fid := fileIDFromToolArgs(turn.ToolCall.Args); fid != "" {
				reelSources[fid] = true
			}
		case strings.Contains(name, "media_get_render"):
			renderPolls++
			for _, id := range outputFileIDsFromRenderResponse(turn.ToolCall.Response) {
				outputIDs[id] = true
			}
		}
	}
	if wantWorkerThreads && spawnCalls < sourceVideoCount {
		trajJSON, _ := json.Marshal(run.Trajectory)
		t.Fatalf("agent did not spawn one worker per source video (spawn_calls=%d want>=%d).\nTrajectory: %s",
			spawnCalls, sourceVideoCount, trajJSON)
	}
	if !wantWorkerThreads && spawnCalls != 0 {
		trajJSON, _ := json.Marshal(run.Trajectory)
		t.Fatalf("agent spawned worker threads for a small batch (spawn_calls=%d want=0).\nTrajectory: %s",
			spawnCalls, trajJSON)
	}
	wantRenders := sourceVideoCount * 2
	if (len(frameSources) > 0 || len(reelSources) > 0 || renderPolls > 0 || len(outputIDs) > 0) &&
		(len(frameSources) < sourceVideoCount || len(reelSources) < sourceVideoCount || renderPolls < wantRenders || len(outputIDs) < wantRenders) {
		trajJSON, _ := json.Marshal(run.Trajectory)
		t.Fatalf("agent exposed an incomplete main-thread media workflow (frame_sources=%v reel_sources=%v render_polls=%d output_ids=%v).\nTrajectory: %s",
			keysOf(frameSources), keysOf(reelSources), renderPolls, keysOf(outputIDs), trajJSON)
	}
	if run.Status != "pass" {
		t.Fatalf("media Environment workflow completed deterministically but judge status=%s", run.Status)
	}
}

func countLabel(n int) string {
	switch n {
	case 1:
		return "one"
	case 2:
		return "two"
	case 3:
		return "three"
	case 4:
		return "four"
	case 5:
		return "five"
	default:
		return strconv.Itoa(n)
	}
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func mediaCompletedEventText(n int) string {
	if n == 1 {
		return "media.completed event"
	}
	return "media.completed events"
}

func mediaBurstSeedPlan(n int) []SeedCall {
	out := make([]SeedCall, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("sample-%c.mp4", 'a'+rune(i))
		out = append(out, SeedCall{
			App:  "storage",
			Tool: "files_upload",
			File: "./fixtures/sample-5s.mp4",
			Input: map[string]any{
				"folder":       "/incoming/",
				"content_type": "video/mp4",
				"name":         name,
			},
		})
	}
	return out
}

func fileIDFromToolArgs(raw json.RawMessage) string {
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return ""
	}
	return stringish(args["file_id"])
}

func outputFileIDsFromRenderResponse(raw json.RawMessage) []string {
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil
	}
	render, _ := resp["render"].(map[string]any)
	if render == nil {
		return nil
	}
	if id := stringish(render["output_file_id"]); id != "" {
		return []string{id}
	}
	return nil
}

func stringish(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strings.TrimSuffix(strings.TrimSuffix(strconv.FormatFloat(x, 'f', 3, 64), "0"), ".")
	default:
		return ""
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func localSampleFileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
