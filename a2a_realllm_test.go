package main

// a2a_realllm_test.go — level-3 tests for the a2a app: REAL apteva-core
// agents driven by a REAL LLM, exchanging work through the REAL a2a
// sidecar (built from local source) inside an isolated Environment.
//
// Two scenarios:
//
//   TestA2A_RealLLM_DelegatedNoteRoundTrip
//     Coordinator discovers the worker over agents_discover, delegates
//     "record a note" via agent_ask; the worker does REAL work (a row in
//     the in-environment notes app's SQLite) and agent_reply's. Proof is
//     deterministic state: the a2a task ledger reaches completed AND the
//     note row exists with the requested title.
//
//   TestA2A_RealLLM_InputRequiredClarificationLoop
//     The coordinator's brief deliberately omits the note body. The
//     worker must push the task to input_required with a question; the
//     coordinator answers via an agent_send follow-up; the worker then
//     completes. Exercises the full A2A clarification state machine
//     with two live LLMs.
//
// Gated like every real-LLM test:
//   APTEVA_RUN_REAL_LLM_TESTS=1 go test -run TestA2A_RealLLM -v -timeout 1200s
// Requires ../core/apteva-core, ../apps/mcp/a2a, ../apps/mcp/notes, and
// OpenCode Go provider auth (env or local Apteva store).

import (
	"bytes"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type a2aRealHarness struct {
	server      *Server
	environment *Environment
	coordinator *EnvironmentAgent
	worker      *EnvironmentAgent
}

func setupA2ARealHarness(t *testing.T, coordinatorDirective, workerDirective string) *a2aRealHarness {
	t.Helper()
	requireRealLLMTests(t)
	corePath := findCoreBinary(t)
	a2aSrc := findAppSource(t, "a2a")
	notesSrc := findAppSource(t, "notes")

	model := "glm-5.2"
	if override := strings.TrimSpace(os.Getenv("OPENCODE_GO_REAL_LLM_MODEL_OVERRIDE")); override != "" {
		model = override
	}
	key := loadOpenCodeGoAPIKey(t)
	providerState := map[string]any{
		"OPENCODE_GO_API_KEY": key,
		"model_large":         model,
		"model_medium":        model,
		"model_small":         model,
	}

	s, userID, coordinatorRow := setupRealServerWithProviderState(t, corePath,
		"Coordinator", coordinatorDirective, 13, "llm", "OpenCode Go", providerState)
	workerRow, err := s.store.CreateAgent(userID, "Worker", workerDirective, "autonomous", "{}", "")
	if err != nil {
		t.Fatalf("create worker source agent: %v", err)
	}

	environment, err := s.environments.Create(EnvironmentSpec{
		ID:         fmt.Sprintf("env-a2a-%d", time.Now().UnixNano()),
		GatewayURL: "http://127.0.0.1:" + s.port,
		AppSrcDirs: map[string]string{
			"a2a":   a2aSrc,
			"notes": notesSrc,
		},
		NetworkMode:  EdgePassthrough,
		HealthBudget: 3 * time.Minute, // first run builds both sidecars from source
	})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	t.Cleanup(environment.Stop)

	coordinator, err := s.SpawnAgentInEnvironment(environment, EnvironmentAgentSpec{
		UserID: userID, Source: coordinatorRow, Alias: "coordinator",
	})
	if err != nil {
		t.Fatalf("spawn coordinator: %v", err)
	}
	t.Cleanup(coordinator.Stop)
	worker, err := s.SpawnAgentInEnvironment(environment, EnvironmentAgentSpec{
		UserID: userID, Source: workerRow, Alias: "worker",
	})
	if err != nil {
		t.Fatalf("spawn worker: %v", err)
	}
	t.Cleanup(worker.Stop)

	return &a2aRealHarness{server: s, environment: environment, coordinator: coordinator, worker: worker}
}

// postCoordinatorEvent injects an operator event into the coordinator's
// live core, the same way subscriptions and channels deliver events.
func (h *a2aRealHarness) postCoordinatorEvent(t *testing.T, message string) {
	t.Helper()
	body := fmt.Sprintf(`{"message": %q}`, message)
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/event", h.coordinator.Port), bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("build event request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.coordinator.APIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post coordinator event: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("post coordinator event: status %d", resp.StatusCode)
	}
}

func (h *a2aRealHarness) openAppDB(t *testing.T, app string) *sql.DB {
	t.Helper()
	path, ok := h.environment.AppDBPath(app)
	if !ok {
		t.Fatalf("no db path for in-environment app %q", app)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open %s db: %v", app, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type a2aLedgerMessage struct {
	FromAgentID int64
	ToAgentID   int64
	Body        string
	StatusAfter string
}

func (h *a2aRealHarness) ledger(t *testing.T) (taskStatus string, messages []a2aLedgerMessage) {
	t.Helper()
	db := h.openAppDB(t, "a2a")
	_ = db.QueryRow(`SELECT status FROM a2a_tasks WHERE kind='ask' ORDER BY id LIMIT 1`).Scan(&taskStatus)
	rows, err := db.Query(`SELECT m.from_agent_id, m.to_agent_id, m.body, m.status_after
		FROM a2a_messages m JOIN a2a_tasks t ON t.id = m.task_id
		WHERE t.kind='ask' ORDER BY m.id`)
	if err != nil {
		return taskStatus, nil
	}
	defer rows.Close()
	for rows.Next() {
		var m a2aLedgerMessage
		if rows.Scan(&m.FromAgentID, &m.ToAgentID, &m.Body, &m.StatusAfter) == nil {
			messages = append(messages, m)
		}
	}
	return taskStatus, messages
}

func (h *a2aRealHarness) logTranscript(t *testing.T) {
	t.Helper()
	status, messages := h.ledger(t)
	t.Logf("a2a ask task status: %s", status)
	name := func(id int64) string {
		switch id {
		case h.coordinator.AgentID:
			return "coordinator"
		case h.worker.AgentID:
			return "worker"
		}
		return fmt.Sprintf("agent-%d", id)
	}
	for i, m := range messages {
		t.Logf("  [%d] %s → %s (%s): %s", i+1, name(m.FromAgentID), name(m.ToAgentID), m.StatusAfter, m.Body)
	}
}

func (h *a2aRealHarness) waitFor(t *testing.T, timeout time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(3 * time.Second)
	}
	h.dumpDiagnostics(t)
	t.Fatalf("timed out waiting for %s", what)
}

// dumpDiagnostics surfaces what each core actually did before the
// temp dirs vanish: the a2a ledger plus each core's on-disk log tail.
func (h *a2aRealHarness) dumpDiagnostics(t *testing.T) {
	t.Helper()
	h.logTranscript(t)
	for _, wa := range []*EnvironmentAgent{h.coordinator, h.worker} {
		dir := filepath.Join(h.server.dataDir, "agents", fmt.Sprintf("instance_%d", wa.AgentID))
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Logf("agent %s: no instance dir %s: %v", wa.Alias, dir, err)
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				continue
			}
			lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
			if len(lines) > 80 {
				lines = lines[len(lines)-80:]
			}
			t.Logf("===== %s %s (last %d lines) =====\n%s", wa.Alias, entry.Name(), len(lines), strings.Join(lines, "\n"))
		}
	}
}

const a2aCoordinatorBaseDirective = `# Role
You coordinate work by delegating to peer agents through the a2a tools. You never do the delegated work yourself.
# Operating Rules
- When an operator event tells you to delegate, first call agents_discover, find the peer agent named "worker", then call agent_ask with one complete, self-contained request.
- After the agent_ask receipt, wait for the [a2a] reply event. Do not poll, do not repeat the ask, do not create additional tasks.
- If the worker asks a clarifying question in an [a2a] event, answer it with agent_send using the same task_id.
- When an [a2a] reply with status completed or failed arrives, the job is over: take no further action and go idle.
- No chat channels are connected. Never attempt to message the operator.`

const a2aWorkerNoteDirective = `# Role
You are a note keeper serving other agents through a2a requests.
# Operating Rules
- When an [a2a] request asks you to record a note, call notes_create with exactly the requested title and body, then call agent_reply for that task_id with the created note id in the message and status "completed".
- The request is only complete when the note exists and the reply is sent. Reply exactly once with status completed per request.
- If the request does not state the note body, you MUST NOT invent one: call agent_reply with status "input_required" asking for the body, then complete the note only after the follow-up answer arrives.
- No chat channels are connected. Never attempt to message the operator.`

func TestA2A_RealLLM_DelegatedNoteRoundTrip(t *testing.T) {
	h := setupA2ARealHarness(t, a2aCoordinatorBaseDirective, a2aWorkerNoteDirective)

	h.postCoordinatorEvent(t,
		`[operator] Delegate this to the worker agent now: record a note titled "A2A Live Test" with body "hello from coordinator". Delegate over a2a; do not do it yourself.`)

	h.waitFor(t, 6*time.Minute, "a2a ask task to complete", func() bool {
		status, _ := h.ledger(t)
		return status == "completed"
	})
	h.logTranscript(t)

	// Real work happened: the note row exists in the notes sidecar's DB.
	notesDB := h.openAppDB(t, "notes")
	var count int
	var body string
	if err := notesDB.QueryRow(`SELECT COUNT(*), COALESCE(MAX(body),'') FROM notes WHERE title = 'A2A Live Test'`).Scan(&count, &body); err != nil {
		t.Fatalf("query notes: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 'A2A Live Test' note, found %d", count)
	}
	if !strings.Contains(body, "hello from coordinator") {
		t.Fatalf("note body = %q, want the delegated content", body)
	}

	// The ledger shows a real two-party exchange addressed correctly.
	_, messages := h.ledger(t)
	if len(messages) < 2 {
		t.Fatalf("expected at least ask + reply in the ledger, got %d", len(messages))
	}
	if messages[0].FromAgentID != h.coordinator.AgentID || messages[0].ToAgentID != h.worker.AgentID {
		t.Fatalf("ask addressed %d→%d, want coordinator→worker", messages[0].FromAgentID, messages[0].ToAgentID)
	}
	final := messages[len(messages)-1]
	if final.FromAgentID != h.worker.AgentID || final.StatusAfter != "completed" {
		t.Fatalf("final ledger message = %+v, want completed reply from worker", final)
	}
}

func TestA2A_RealLLM_InputRequiredClarificationLoop(t *testing.T) {
	h := setupA2ARealHarness(t, a2aCoordinatorBaseDirective, a2aWorkerNoteDirective)

	// The brief withholds the body on purpose: the worker's directive
	// forbids inventing one, forcing the input_required loop.
	h.postCoordinatorEvent(t,
		`[operator] Delegate this to the worker agent now: record a note titled "Clarified Note". You have not been given the body; if the worker asks for it over a2a, answer exactly: the clarified body. Delegate over a2a; do not do it yourself.`)

	h.waitFor(t, 8*time.Minute, "clarification loop to complete", func() bool {
		status, _ := h.ledger(t)
		return status == "completed"
	})
	h.logTranscript(t)

	_, messages := h.ledger(t)
	sawInputRequired := false
	for _, m := range messages {
		if m.FromAgentID == h.worker.AgentID && m.StatusAfter == "input_required" {
			sawInputRequired = true
		}
	}
	if !sawInputRequired {
		t.Fatal("worker never pushed the task to input_required — the clarification loop was skipped")
	}

	notesDB := h.openAppDB(t, "notes")
	var count int
	var body string
	if err := notesDB.QueryRow(`SELECT COUNT(*), COALESCE(MAX(body),'') FROM notes WHERE title = 'Clarified Note'`).Scan(&count, &body); err != nil {
		t.Fatalf("query notes: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 'Clarified Note', found %d", count)
	}
	if !strings.Contains(strings.ToLower(body), "the clarified body") {
		t.Fatalf("note body = %q, want the clarified content from the follow-up", body)
	}
}
