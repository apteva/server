package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/apteva/app-sdk"
)

const (
	agentLifecyclePollInterval     = time.Second
	agentLifecycleDeliveryInterval = 500 * time.Millisecond
	agentLifecycleBatchLimit       = 256
	agentLifecycleHTTPBodyLimit    = 8 << 20
)

var (
	errAgentEventConflict = errors.New("source event id already exists with different content")
	errAgentEventInvalid  = errors.New("invalid tracked agent event")
)

type agentEventExecution struct {
	ID              int64
	AgentID         int64
	ProjectID       string
	SourceApp       string
	SourceInstallID int64
	SourceEventID   string
	CoreEventID     string
	CoreExecutionID string
	ThreadID        string
	PayloadHash     string
	State           string
	LastSequence    uint64
}

type coreEventLifecycleTransition struct {
	ID                string    `json:"id"`
	Type              string    `json:"type"`
	EventID           string    `json:"event_id"`
	ExecutionID       string    `json:"execution_id"`
	ThreadID          string    `json:"thread_id"`
	ParentExecutionID string    `json:"parent_execution_id,omitempty"`
	Timestamp         time.Time `json:"timestamp"`
	Reason            string    `json:"reason,omitempty"`
	Sequence          uint64    `json:"sequence"`
}

type pendingAgentEventDelivery struct {
	TransitionID    string
	SourceInstallID int64
	PayloadJSON     string
	Attempts        int
}

func agentEventCoreID(installID int64, sourceEventID string) string {
	sum := sha256.Sum256([]byte(sourceEventID))
	return fmt.Sprintf("app:%d:%s", installID, hex.EncodeToString(sum[:]))
}

func agentEventPayloadHash(threadID string, message any) (string, error) {
	raw, err := json.Marshal(struct {
		ThreadID string `json:"thread_id"`
		Message  any    `json:"message"`
	}{ThreadID: threadID, Message: message})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func scanAgentEventExecution(scanner interface{ Scan(...any) error }) (*agentEventExecution, error) {
	var execution agentEventExecution
	err := scanner.Scan(
		&execution.ID, &execution.AgentID, &execution.ProjectID,
		&execution.SourceApp, &execution.SourceInstallID, &execution.SourceEventID,
		&execution.CoreEventID, &execution.CoreExecutionID,
		&execution.ThreadID, &execution.PayloadHash,
		&execution.State, &execution.LastSequence,
	)
	if err != nil {
		return nil, err
	}
	return &execution, nil
}

const agentEventExecutionColumns = `id, agent_id, project_id, source_app, source_install_id,
	source_event_id, core_event_id, core_execution_id, thread_id, payload_hash,
	state, last_sequence`

func (s *Store) prepareAgentEventExecution(agentID int64, projectID, sourceApp string, installID int64, sourceEventID, coreEventID, threadID, payloadHash string) (*agentEventExecution, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT OR IGNORE INTO agent_event_executions
		(agent_id, project_id, source_app, source_install_id, source_event_id, core_event_id, thread_id, payload_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		agentID, projectID, sourceApp, installID, sourceEventID, coreEventID, threadID, payloadHash)
	if err != nil {
		return nil, false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	execution, err := scanAgentEventExecution(tx.QueryRow(`SELECT `+agentEventExecutionColumns+`
		FROM agent_event_executions
		WHERE agent_id=? AND source_install_id=? AND source_event_id=?`, agentID, installID, sourceEventID))
	if err != nil {
		return nil, false, err
	}
	if execution.ProjectID != projectID || execution.CoreEventID != coreEventID || execution.ThreadID != threadID || execution.PayloadHash != payloadHash {
		return nil, false, errAgentEventConflict
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return execution, inserted == 0, nil
}

func (s *Store) completeAgentEventExecution(id int64, coreExecutionID string) error {
	result, err := s.db.Exec(`UPDATE agent_event_executions
		SET core_execution_id=CASE WHEN core_execution_id='' THEN ? ELSE core_execution_id END,
		    updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND (core_execution_id='' OR core_execution_id=?)`, coreExecutionID, id, coreExecutionID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("execution mapping changed concurrently")
	}
	return nil
}

func (s *Server) sendTrackedAgentEvent(ctx context.Context, agent *Agent, installID int64, sourceEventID, threadID string, message any) (*sdk.AgentEventReceipt, error) {
	if agent == nil {
		return nil, fmt.Errorf("%w: agent required", errAgentEventInvalid)
	}
	sourceEventID = strings.TrimSpace(sourceEventID)
	if sourceEventID == "" {
		return nil, fmt.Errorf("%w: source_event_id required", errAgentEventInvalid)
	}
	if len(sourceEventID) > 1024 || strings.ContainsAny(sourceEventID, "\r\n\x00") {
		return nil, fmt.Errorf("%w: source_event_id must be at most 1024 bytes and contain no control separators", errAgentEventInvalid)
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		threadID = "main"
	}
	if strings.Contains(threadID, "/") {
		return nil, fmt.Errorf("%w: invalid thread_id", errAgentEventInvalid)
	}
	payloadHash, err := agentEventPayloadHash(threadID, message)
	if err != nil {
		return nil, fmt.Errorf("hash event payload: %w", err)
	}
	var sourceApp string
	if err := s.store.db.QueryRow(`SELECT a.name FROM app_installs i JOIN apps a ON a.id=i.app_id WHERE i.id=?`, installID).Scan(&sourceApp); err != nil {
		return nil, fmt.Errorf("resolve source app installation: %w", err)
	}
	coreEventID := agentEventCoreID(installID, sourceEventID)
	execution, duplicate, err := s.store.prepareAgentEventExecution(
		agent.ID, agent.ProjectID, sourceApp, installID, sourceEventID, coreEventID, threadID, payloadHash,
	)
	if err != nil {
		return nil, err
	}
	if execution.CoreExecutionID != "" {
		return &sdk.AgentEventReceipt{
			SourceEventID: sourceEventID, ExecutionID: execution.CoreExecutionID,
			Accepted: false, Duplicate: true, ThreadID: threadID,
		}, nil
	}

	port := s.agents.GetPort(agent.ID)
	if port == 0 {
		return nil, errors.New("agent is not running")
	}
	body, err := json.Marshal(map[string]any{
		"message": message, "thread_id": threadID,
		"event_id": coreEventID, "track_lifecycle": true,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/event", port), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key := s.agents.GetCoreAPIKey(agent.ID); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("core event: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read core event receipt: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		if resp.StatusCode == http.StatusConflict {
			return nil, errAgentEventConflict
		}
		return nil, fmt.Errorf("core event http %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	var coreReceipt struct {
		Events struct {
			Accepted   []string          `json:"accepted"`
			Duplicates []string          `json:"duplicates"`
			Executions map[string]string `json:"executions"`
		} `json:"events"`
	}
	if err := json.Unmarshal(responseBody, &coreReceipt); err != nil {
		return nil, fmt.Errorf("decode core event receipt: %w", err)
	}
	coreExecutionID := strings.TrimSpace(coreReceipt.Events.Executions[coreEventID])
	if coreExecutionID == "" {
		return nil, errors.New("core event receipt omitted execution_id")
	}
	if err := s.store.completeAgentEventExecution(execution.ID, coreExecutionID); err != nil {
		return nil, err
	}
	accepted := contains(coreReceipt.Events.Accepted, coreEventID)
	coreDuplicate := contains(coreReceipt.Events.Duplicates, coreEventID)
	return &sdk.AgentEventReceipt{
		SourceEventID: sourceEventID, ExecutionID: coreExecutionID,
		Accepted: accepted && !duplicate, Duplicate: duplicate || coreDuplicate,
		ThreadID: threadID,
	}, nil
}

func isAgentEventLifecycleType(eventType string) bool {
	switch eventType {
	case "event.claimed", "event.active", "event.settled", "event.error":
		return true
	default:
		return false
	}
}

func agentLifecycleTelemetryID(agentID int64, transitionID string) string {
	return fmt.Sprintf("agent:%d:%s", agentID, transitionID)
}

// normalizeAgentLifecycleTelemetryIDs collapses Core's ordinary telemetry copy
// and the durable lifecycle relay onto one Server row. The relay later enriches
// the row with trusted source ownership metadata.
func normalizeAgentLifecycleTelemetryIDs(events []TelemetryEvent) {
	for i := range events {
		if !isAgentEventLifecycleType(events[i].Type) || events[i].AgentID <= 0 {
			continue
		}
		var data struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(events[i].Data, &data) != nil || strings.TrimSpace(data.ID) == "" {
			continue
		}
		events[i].ID = agentLifecycleTelemetryID(events[i].AgentID, strings.TrimSpace(data.ID))
	}
}

func (s *Store) persistAgentLifecycleTransition(agentID int64, transition coreEventLifecycleTransition) (bool, error) {
	if transition.ID == "" || transition.ExecutionID == "" || transition.EventID == "" || !isAgentEventLifecycleType(transition.Type) {
		return false, fmt.Errorf("invalid lifecycle transition")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	execution, err := scanAgentEventExecution(tx.QueryRow(`SELECT `+agentEventExecutionColumns+`
		FROM agent_event_executions
		WHERE agent_id=? AND (core_execution_id=? OR core_event_id=?)
		ORDER BY CASE WHEN core_execution_id=? THEN 0 ELSE 1 END LIMIT 1`,
		agentID, transition.ExecutionID, transition.EventID, transition.ExecutionID))
	if errors.Is(err, sql.ErrNoRows) {
		// Administrator/MCP callers can opt into Core tracking without an app
		// owner. Preserve those transitions in the ordinary telemetry stream and
		// acknowledge them, but never invent an app delivery destination.
		data := map[string]any{
			"id": transition.ID, "type": transition.Type,
			"event_id": transition.EventID, "execution_id": transition.ExecutionID,
			"thread_id": transition.ThreadID, "timestamp": transition.Timestamp,
			"sequence": transition.Sequence,
		}
		if transition.ParentExecutionID != "" {
			data["parent_execution_id"] = transition.ParentExecutionID
		}
		if transition.Reason != "" {
			data["reason"] = transition.Reason
		}
		dataJSON, marshalErr := json.Marshal(data)
		if marshalErr != nil {
			return false, marshalErr
		}
		timestamp := transition.Timestamp
		if timestamp.IsZero() {
			timestamp = time.Now().UTC()
		}
		if _, insertErr := tx.Exec(`INSERT INTO telemetry (id, agent_id, thread_id, type, time, data)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET thread_id=excluded.thread_id, type=excluded.type,
				time=excluded.time, data=excluded.data`,
			agentLifecycleTelemetryID(agentID, transition.ID), agentID, transition.ThreadID,
			transition.Type, timestamp.UTC().Format(time.RFC3339Nano), string(dataJSON)); insertErr != nil {
			return false, insertErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return false, commitErr
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if execution.CoreEventID != transition.EventID {
		return false, fmt.Errorf("lifecycle event id does not match execution ownership")
	}
	if execution.CoreExecutionID != "" && execution.CoreExecutionID != transition.ExecutionID {
		return false, fmt.Errorf("lifecycle execution id does not match ownership")
	}
	if execution.CoreExecutionID == "" {
		if _, err := tx.Exec(`UPDATE agent_event_executions SET core_execution_id=?, updated_at=CURRENT_TIMESTAMP WHERE id=? AND core_execution_id=''`, transition.ExecutionID, execution.ID); err != nil {
			return false, err
		}
	}

	data := map[string]any{
		"id": transition.ID, "type": transition.Type,
		"event_id": transition.EventID, "source_event_id": execution.SourceEventID,
		"execution_id": transition.ExecutionID, "thread_id": transition.ThreadID,
		"timestamp": transition.Timestamp, "sequence": transition.Sequence,
		"source_app": execution.SourceApp, "source_install_id": execution.SourceInstallID,
	}
	if transition.ParentExecutionID != "" {
		data["parent_execution_id"] = transition.ParentExecutionID
	}
	if transition.Reason != "" {
		data["reason"] = transition.Reason
	}
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return false, err
	}
	telemetryID := agentLifecycleTelemetryID(agentID, transition.ID)
	timestamp := transition.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}
	if _, err := tx.Exec(`INSERT INTO telemetry (id, agent_id, thread_id, type, time, data)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET thread_id=excluded.thread_id, type=excluded.type,
			time=excluded.time, data=excluded.data`,
		telemetryID, agentID, transition.ThreadID, transition.Type,
		timestamp.UTC().Format(time.RFC3339Nano), string(dataJSON)); err != nil {
		return false, err
	}
	if transition.Sequence > execution.LastSequence {
		if _, err := tx.Exec(`UPDATE agent_event_executions
			SET state=?, last_sequence=?, updated_at=CURRENT_TIMESTAMP
			WHERE id=? AND last_sequence < ?`, transition.Type, transition.Sequence, execution.ID, transition.Sequence); err != nil {
			return false, err
		}
	}
	payloadData := map[string]any{
		"type": transition.Type, "source_event_id": execution.SourceEventID,
		"execution_id": transition.ExecutionID, "thread_id": transition.ThreadID,
		"timestamp": timestamp.UTC().Format(time.RFC3339Nano),
		"reason":    transition.Reason, "sequence": transition.Sequence,
	}
	if transition.ParentExecutionID != "" {
		payloadData["parent_execution_id"] = transition.ParentExecutionID
	}
	payload, err := json.Marshal(sdk.Event{
		DeliveryID: transition.ID, Event: sdk.AgentEventLifecycleEvent,
		SourceApp: "apteva-server", SourceInstallID: execution.SourceInstallID,
		InstanceID: agentID, ProjectID: execution.ProjectID, Data: payloadData,
	})
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO agent_event_deliveries
		(transition_id, telemetry_id, execution_id, sequence, source_install_id, payload_json)
		VALUES (?, ?, ?, ?, ?, ?)`, transition.ID, telemetryID, transition.ExecutionID, transition.Sequence, execution.SourceInstallID, string(payload)); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) pendingAgentEventDeliveries(limit int) ([]pendingAgentEventDelivery, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT d.transition_id, d.source_install_id, d.payload_json, d.attempts
		FROM agent_event_deliveries d
		WHERE d.delivered_at IS NULL AND d.next_attempt_at <= ?
		  AND NOT EXISTS (
			SELECT 1 FROM agent_event_deliveries earlier
			WHERE earlier.execution_id=d.execution_id
			  AND earlier.sequence < d.sequence
			  AND earlier.delivered_at IS NULL
		  )
		ORDER BY d.next_attempt_at, d.created_at LIMIT ?`, time.Now().UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var deliveries []pendingAgentEventDelivery
	for rows.Next() {
		var delivery pendingAgentEventDelivery
		if err := rows.Scan(&delivery.TransitionID, &delivery.SourceInstallID, &delivery.PayloadJSON, &delivery.Attempts); err != nil {
			return nil, err
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries, rows.Err()
}

func (s *Store) markAgentEventDeliverySucceeded(transitionID string) error {
	_, err := s.db.Exec(`UPDATE agent_event_deliveries
		SET delivered_at=CURRENT_TIMESTAMP, last_error='', updated_at=CURRENT_TIMESTAMP
		WHERE transition_id=?`, transitionID)
	return err
}

func (s *Store) markAgentEventDeliveryFailed(transitionID, lastError string, attempts int, nextAttempt time.Time) error {
	if len(lastError) > 1000 {
		lastError = lastError[:1000]
	}
	_, err := s.db.Exec(`UPDATE agent_event_deliveries
		SET attempts=?, next_attempt_at=?, last_error=?, updated_at=CURRENT_TIMESTAMP
		WHERE transition_id=? AND delivered_at IS NULL`,
		attempts, nextAttempt.UTC().Format(time.RFC3339Nano), lastError, transitionID)
	return err
}

// cleanDeliveredAgentEventDeliveries removes retry bookkeeping only after the
// same safety window used for telemetry. The execution ownership row remains,
// so source_event_id retries stay idempotent after delivery history expires.
func (s *Store) cleanDeliveredAgentEventDeliveries(maxAge time.Duration) (int64, error) {
	cutoff := time.Now().Add(-maxAge).UTC().Format(time.RFC3339Nano)
	result, err := s.db.Exec(`DELETE FROM agent_event_deliveries
		WHERE delivered_at IS NOT NULL AND datetime(delivered_at) < datetime(?)`, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

type AgentEventLifecycleService struct {
	server *Server
	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewAgentEventLifecycleService(server *Server) *AgentEventLifecycleService {
	return &AgentEventLifecycleService{server: server}
}

func (r *AgentEventLifecycleService) Start() {
	if r == nil || r.server == nil {
		return
	}
	r.mu.Lock()
	if r.cancel != nil {
		r.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.wg.Add(2)
	r.mu.Unlock()
	go r.relayLoop(ctx)
	go r.deliveryLoop(ctx)
}

func (r *AgentEventLifecycleService) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	cancel := r.cancel
	r.cancel = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
		r.wg.Wait()
	}
}

func (r *AgentEventLifecycleService) relayLoop(ctx context.Context) {
	defer r.wg.Done()
	ticker := time.NewTicker(agentLifecyclePollInterval)
	defer ticker.Stop()
	for {
		if err := r.relayOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("[AGENT-EVENTS] lifecycle relay: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *AgentEventLifecycleService) relayOnce(ctx context.Context) error {
	agents, err := r.server.store.ListAgentsByStatus("running")
	if err != nil {
		return err
	}
	for i := range agents {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if r.server.agents.GetPort(agents[i].ID) == 0 {
			continue
		}
		if err := r.relayAgent(ctx, &agents[i]); err != nil {
			log.Printf("[AGENT-EVENTS] relay agent=%d: %v", agents[i].ID, err)
		}
	}
	return nil
}

func (r *AgentEventLifecycleService) relayAgent(ctx context.Context, agent *Agent) error {
	port := r.server.agents.GetPort(agent.ID)
	if port == 0 {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/event-lifecycle?limit=%d", port, agentLifecycleBatchLimit), nil)
	if err != nil {
		return err
	}
	if key := r.server.agents.GetCoreAPIKey(agent.ID); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil // older Core without lifecycle support
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("core lifecycle http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var result struct {
		Transitions []coreEventLifecycleTransition `json:"transitions"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, agentLifecycleHTTPBodyLimit)).Decode(&result); err != nil {
		return err
	}
	if len(result.Transitions) > agentLifecycleBatchLimit {
		result.Transitions = result.Transitions[:agentLifecycleBatchLimit]
	}
	ackIDs := make([]string, 0, len(result.Transitions))
	for _, transition := range result.Transitions {
		persisted, err := r.server.store.persistAgentLifecycleTransition(agent.ID, transition)
		if err != nil {
			log.Printf("[AGENT-EVENTS] persist agent=%d transition=%s: %v", agent.ID, transition.ID, err)
			continue
		}
		if persisted {
			ackIDs = append(ackIDs, transition.ID)
		}
	}
	if len(ackIDs) == 0 {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"ack_ids": ackIDs})
	ackReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/event-lifecycle", port), bytes.NewReader(body))
	if err != nil {
		return err
	}
	ackReq.Header.Set("Content-Type", "application/json")
	if key := r.server.agents.GetCoreAPIKey(agent.ID); key != "" {
		ackReq.Header.Set("Authorization", "Bearer "+key)
	}
	ackResp, err := (&http.Client{Timeout: 5 * time.Second}).Do(ackReq)
	if err != nil {
		return err
	}
	defer ackResp.Body.Close()
	if ackResp.StatusCode/100 != 2 {
		ackBody, _ := io.ReadAll(io.LimitReader(ackResp.Body, 4096))
		return fmt.Errorf("core lifecycle ack http %d: %s", ackResp.StatusCode, strings.TrimSpace(string(ackBody)))
	}
	return nil
}

func (r *AgentEventLifecycleService) deliveryLoop(ctx context.Context) {
	defer r.wg.Done()
	ticker := time.NewTicker(agentLifecycleDeliveryInterval)
	defer ticker.Stop()
	for {
		if err := r.deliverOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("[AGENT-EVENTS] delivery: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func agentEventDeliveryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 8 {
		shift = 8
	}
	delay := time.Second * time.Duration(1<<shift)
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}

func (r *AgentEventLifecycleService) deliverOnce(ctx context.Context) error {
	deliveries, err := r.server.store.pendingAgentEventDeliveries(50)
	if err != nil {
		return err
	}
	for _, delivery := range deliveries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := r.deliver(ctx, delivery); err != nil {
			attempts := delivery.Attempts + 1
			_ = r.server.store.markAgentEventDeliveryFailed(
				delivery.TransitionID, err.Error(), attempts,
				time.Now().UTC().Add(agentEventDeliveryBackoff(attempts)),
			)
			continue
		}
		if err := r.server.store.markAgentEventDeliverySucceeded(delivery.TransitionID); err != nil {
			log.Printf("[AGENT-EVENTS] mark delivered transition=%s: %v", delivery.TransitionID, err)
		}
	}
	return nil
}

func (r *AgentEventLifecycleService) deliver(ctx context.Context, delivery pendingAgentEventDelivery) error {
	if r.server.installedApps == nil {
		return errors.New("installed app registry unavailable")
	}
	install := r.server.installedApps.Get(delivery.SourceInstallID)
	if install == nil || strings.TrimSpace(install.SidecarURL) == "" {
		return fmt.Errorf("source app installation %d is not running", delivery.SourceInstallID)
	}
	deliveryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(deliveryCtx, http.MethodPost,
		strings.TrimRight(install.SidecarURL, "/")+"/events", strings.NewReader(delivery.PayloadJSON))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if install.Token != "" {
		req.Header.Set("Authorization", "Bearer "+install.Token)
	}
	req.Header.Set("X-Apteva-App-Install-ID", strconv.FormatInt(install.InstallID, 10))
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("app events http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
