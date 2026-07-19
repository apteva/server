package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestChannelChat_RealLLM_Codex_ServerAndCoreRestartConversation is the
// process-boundary persistence test. It boots the real apteva-server binary,
// lets that process spawn a real apteva-core using Codex, creates and uses a
// saved conversation, terminates the whole server/core tree, then boots both
// binaries again from the same data directory and continues the same chat.
//
// Opt-in:
//
//	APTEVA_RUN_REAL_LLM_TESTS=1 APTEVA_SERVER_BIN=/path/to/apteva-server APTEVA_CORE_BIN=/path/to/apteva-core \
//	  go test -run TestChannelChat_RealLLM_Codex_ServerAndCoreRestartConversation -v -timeout 8m
func TestChannelChat_RealLLM_Codex_ServerAndCoreRestartConversation(t *testing.T) {
	requireRealLLMTests(t)
	serverPath := findServerBinary(t)
	corePath := findCoreBinary(t)
	providerState := loadOpenAICodexProviderState(t)

	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "apteva.db")
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatalf("generate server secret: %v", err)
	}
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("open restart store: %v", err)
	}
	user, err := store.CreateUser("restart-codex@example.com", "x")
	if err != nil {
		t.Fatalf("create restart user: %v", err)
	}
	project, err := store.CreateProject(user.ID, "Restart Project", "", "")
	if err != nil {
		t.Fatalf("create restart project: %v", err)
	}
	providerJSON, _ := json.Marshal(providerState)
	encrypted, err := Encrypt(secret, string(providerJSON))
	if err != nil {
		t.Fatalf("encrypt restart provider: %v", err)
	}
	if _, err := store.CreateProvider(user.ID, 15, "llm", "OpenAI Codex", encrypted); err != nil {
		t.Fatalf("create restart provider: %v", err)
	}
	agent, err := store.CreateAgent(user.ID, "restart-conversation-codex-under-test", strings.Join([]string{
		"# Role",
		"You answer operator messages in the current Apteva dashboard conversation.",
		"# Rules",
		"When asked for an exact reply, send it exactly once through channels_send(channel=\"current\", ...), then return idle.",
	}, "\n"), "autonomous", `{"include_apteva_server":false,"include_channels":true}`, project.ID)
	if err != nil {
		t.Fatalf("create restart agent: %v", err)
	}
	apiKey := "apt_restart_codex_test_key"
	if _, err := store.CreateAPIKey(user.ID, "restart e2e", HashAPIKey(apiKey), apiKey[:8]); err != nil {
		t.Fatalf("create restart API key: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close seeded restart store: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve restart server port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	processEnv := childEnvWithOverrides(os.Environ(), map[string]string{
		"PORT":                    strconv.Itoa(port),
		"DB_PATH":                 dbPath,
		"DATA_DIR":                dataDir,
		"CORE_CMD":                corePath,
		"SERVER_SECRET":           hex.EncodeToString(secret),
		"REGISTRATION_MODE":       "locked",
		"APTEVA_CONFIG":           "",
		"APTEVA_HOME":             "",
		"APTEVA_PUBLIC_URL":       baseURL,
		"APTEVA_CLONE_QUARANTINE": "0",
	})

	proc := startRestartServerProcess(t, serverPath, processEnv, baseURL)
	t.Cleanup(func() { proc.stop(t) })
	startRestartAgent(t, baseURL, apiKey, agent.ID)

	conversation := createRestartConversation(t, baseURL, apiKey, project.ID, agent.ID)
	stream := openRestartConversationStream(t, baseURL, apiKey, conversation.ID)
	postRestartConversationMessage(t, baseURL, apiKey, conversation.ID, "Reply exactly BEFORE RESTART.", project.ID)
	waitForRestartConversationReply(t, baseURL, apiKey, conversation.ID, "BEFORE RESTART", 120*time.Second)
	_ = stream.Close()

	firstCorePID := waitForPersistedCorePID(t, dbPath, user.ID, agent.ID, 15*time.Second)
	proc.stop(t)
	waitForProcessExit(t, firstCorePID, 15*time.Second)

	proc = startRestartServerProcess(t, serverPath, processEnv, baseURL)
	startRestartAgent(t, baseURL, apiKey, agent.ID)
	stream = openRestartConversationStream(t, baseURL, apiKey, conversation.ID)
	t.Cleanup(func() { _ = stream.Close() })
	postRestartConversationMessage(t, baseURL, apiKey, conversation.ID, "Reply exactly AFTER RESTART.", project.ID)
	replies := waitForRestartConversationReply(t, baseURL, apiKey, conversation.ID, "AFTER RESTART", 120*time.Second)

	secondCorePID := waitForPersistedCorePID(t, dbPath, user.ID, agent.ID, 15*time.Second)
	if firstCorePID == secondCorePID {
		t.Fatalf("core process was not replaced across full restart: pid=%d", firstCorePID)
	}
	joined := strings.Join(replies, "\n")
	if !strings.Contains(joined, "BEFORE RESTART") || !strings.Contains(joined, "AFTER RESTART") {
		t.Fatalf("saved conversation did not retain both sides of restart: %q", replies)
	}
}

type restartServerProcess struct {
	cmd     *exec.Cmd
	logPath string
	done    chan error
}

func startRestartServerProcess(t *testing.T, binary string, env []string, baseURL string) *restartServerProcess {
	t.Helper()
	logFile, err := os.CreateTemp(t.TempDir(), "apteva-restart-server-*.log")
	if err != nil {
		t.Fatalf("create restart server log: %v", err)
	}
	cmd := exec.Command(binary)
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("start restart server: %v", err)
	}
	proc := &restartServerProcess{cmd: cmd, logPath: logFile.Name(), done: make(chan error, 1)}
	go func() {
		proc.done <- cmd.Wait()
		_ = logFile.Close()
	}()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, requestErr := http.Get(baseURL + "/health")
		if requestErr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return proc
			}
		}
		select {
		case waitErr := <-proc.done:
			t.Fatalf("restart server exited during boot: %v\n%s", waitErr, proc.logs())
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	proc.stop(t)
	t.Fatalf("restart server did not become healthy\n%s", proc.logs())
	return nil
}

func (p *restartServerProcess) stop(t *testing.T) {
	t.Helper()
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	select {
	case <-p.done:
		p.cmd = nil
		return
	default:
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	select {
	case <-p.done:
		p.cmd = nil
	case <-time.After(15 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
		t.Fatalf("restart server did not stop gracefully\n%s", p.logs())
	}
}

func (p *restartServerProcess) logs() string {
	data, _ := os.ReadFile(p.logPath)
	if len(data) > 16_000 {
		data = data[len(data)-16_000:]
	}
	return string(data)
}

func restartRequest(t *testing.T, method, url, apiKey string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		payload, _ := json.Marshal(body)
		reader = bytes.NewReader(payload)
	}
	req, _ := http.NewRequest(method, url, reader)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func startRestartAgent(t *testing.T, baseURL, apiKey string, agentID int64) {
	t.Helper()
	resp := restartRequest(t, http.MethodPost, fmt.Sprintf("%s/api/agents/%d/start", baseURL, agentID), apiKey, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("start restart agent status=%d body=%s", resp.StatusCode, body)
	}
}

type restartConversation struct {
	ID string `json:"id"`
}

func createRestartConversation(t *testing.T, baseURL, apiKey, projectID string, agentID int64) restartConversation {
	t.Helper()
	resp := restartRequest(t, http.MethodPost, baseURL+"/api/apps/channel-chat/conversations", apiKey, map[string]any{
		"project_id": projectID, "title": "Restart continuity", "agent_ids": []int64{agentID},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create restart conversation status=%d body=%s", resp.StatusCode, body)
	}
	var conversation restartConversation
	if err := json.NewDecoder(resp.Body).Decode(&conversation); err != nil || conversation.ID == "" {
		t.Fatalf("decode restart conversation: %+v err=%v", conversation, err)
	}
	return conversation
}

func openRestartConversationStream(t *testing.T, baseURL, apiKey, chatID string) io.ReadCloser {
	t.Helper()
	resp := restartRequest(t, http.MethodGet, baseURL+"/api/apps/channel-chat/stream?chat_id="+chatID, apiKey, nil)
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("open restart conversation stream status=%d body=%s", resp.StatusCode, body)
	}
	return resp.Body
}

func postRestartConversationMessage(t *testing.T, baseURL, apiKey, chatID, content, projectID string) {
	t.Helper()
	resp := restartRequest(t, http.MethodPost, baseURL+"/api/apps/channel-chat/messages?chat_id="+chatID, apiKey, map[string]any{
		"content": content,
		"context": map[string]any{
			"source": "dashboard-chat", "route": "/chat/" + chatID, "project_id": projectID,
		},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("post restart message status=%d body=%s", resp.StatusCode, body)
	}
}

func waitForRestartConversationReply(t *testing.T, baseURL, apiKey, chatID, marker string, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var replies []string
	for time.Now().Before(deadline) {
		resp := restartRequest(t, http.MethodGet, baseURL+"/api/apps/channel-chat/messages?chat_id="+chatID+"&limit=100", apiKey, nil)
		var rows []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&rows)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK && decodeErr == nil {
			replies = replies[:0]
			for _, row := range rows {
				if row.Role == "agent" {
					replies = append(replies, row.Content)
				}
			}
			if strings.Contains(strings.Join(replies, "\n"), marker) {
				return replies
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("conversation %s did not receive %q before timeout; replies=%q", chatID, marker, replies)
	return nil
}

func waitForPersistedCorePID(t *testing.T, dbPath string, userID, agentID int64, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		db, err := NewStore(dbPath)
		if err == nil {
			agent, getErr := db.GetAgent(userID, agentID)
			_ = db.Close()
			if getErr == nil && agent.Pid > 0 {
				return agent.Pid
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("agent %d never persisted a core pid", agentID)
	return 0
}

func waitForProcessExit(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("core pid %d survived server shutdown", pid)
}
