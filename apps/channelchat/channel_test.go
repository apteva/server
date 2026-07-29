package channelchat

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apteva/server/apps/framework"
	_ "modernc.org/sqlite"
)

func TestChatChannelReportAndAlertPublishToUserStream(t *testing.T) {
	db := openChannelTestDB(t, false)
	defer db.Close()

	st := newStore(db)
	h := newHub()
	userCh, _, cancel := h.subscribeUser(99)
	defer cancel()
	ch := &chatChannel{
		chatID: defaultChatID(285),
		userID: 99,
		store:  st,
		hub:    h,
	}
	if _, err := st.EnsureDefaultChat(285); err != nil {
		t.Fatalf("EnsureDefaultChat: %v", err)
	}

	if _, err := ch.SendReport(framework.ReportRequest{Title: "Pantry check", Summary: "Milk is on the list."}); err != nil {
		t.Fatalf("SendReport: %v", err)
	}
	report := readUserMessage(t, userCh)
	if got := report.Components[0].Name; got != "report-card" {
		t.Fatalf("report component=%q, want report-card", got)
	}

	if err := ch.Status("Pantry failed: auth expired", "warning"); err != nil {
		t.Fatalf("Status: %v", err)
	}
	alert := readUserMessage(t, userCh)
	if got := alert.Components[0].Name; got != "alert-card" {
		t.Fatalf("alert component=%q, want alert-card", got)
	}
}

func TestChatChannelCurrentStatusUpsertsAndStaysOutOfChat(t *testing.T) {
	db := openChannelTestDB(t, true)
	defer db.Close()
	st := newStore(db)
	if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (285, 99, 'Media Agent', 'default')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureDefaultChat(285); err != nil {
		t.Fatal(err)
	}
	h := newHub()
	userCh, _, cancel := h.subscribeUser(99)
	defer cancel()
	ch := &chatChannel{chatID: defaultChatID(285), threadID: "main", agentID: 285, userID: 99, store: st, hub: h}
	progress := 25.0
	first, err := ch.SetCurrentStatus(framework.CurrentStatusRequest{
		Title: "Rendering clips", Detail: "One of four complete", State: "working", Progress: &progress,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = readUserMessage(t, userCh)
	progress = 75
	second, err := ch.SetCurrentStatus(framework.CurrentStatusRequest{
		Title: "Rendering clips", Detail: "Three of four complete", State: "working", Progress: &progress,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = readUserMessage(t, userCh)
	if first.MessageID != second.MessageID {
		t.Fatalf("status row appended instead of updated: first=%d second=%d", first.MessageID, second.MessageID)
	}
	chatRows, err := st.ListRecentMessages(defaultChatID(285), 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(chatRows) != 0 {
		t.Fatalf("status leaked into chat history: %#v", chatRows)
	}
	statuses, err := st.ListCurrentStatuses([]int64{285}, "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Detail != "Three of four complete" ||
		statuses[0].Progress == nil || *statuses[0].Progress != 75 || statuses[0].Message.ThreadID != "main" {
		t.Fatalf("current statuses = %#v", statuses)
	}
	if _, err := db.Exec(`UPDATE channel_chat_messages SET created_at=datetime('now', '-48 hours') WHERE id=?`, first.MessageID); err != nil {
		t.Fatal(err)
	}
	oldWorking, err := st.GetMessage(first.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	oldWorking.Components[0].Props["updated_at"] = time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339Nano)
	if _, err := st.UpdateMessageComponents(first.MessageID, oldWorking.Components); err != nil {
		t.Fatal(err)
	}
	statuses, err = st.ListCurrentStatuses([]int64{285}, "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].State != "working" || !statuses[0].Stale {
		t.Fatalf("old working status should remain visible and stale: %#v", statuses)
	}

	if _, err := ch.SetCurrentStatus(framework.CurrentStatusRequest{
		Title: "Rendering complete", State: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	_ = readUserMessage(t, userCh)
	if _, err := db.Exec(`UPDATE channel_chat_messages SET created_at=datetime('now', '-48 hours') WHERE id=?`, first.MessageID); err != nil {
		t.Fatal(err)
	}
	statuses, err = st.ListCurrentStatuses([]int64{285}, "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].State != "completed" || statuses[0].Stale {
		t.Fatalf("old completed status should remain visible and completed: %#v", statuses)
	}
}

func TestChatChannelCurrentStatusPersistsNormalizesAndClearsNextAction(t *testing.T) {
	db := openChannelTestDB(t, true)
	defer db.Close()
	st := newStore(db)
	if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (285, 99, 'Media Agent', 'default')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureDefaultChat(285); err != nil {
		t.Fatal(err)
	}
	h := newHub()
	userCh, _, cancel := h.subscribeUser(99)
	defer cancel()
	ch := &chatChannel{chatID: defaultChatID(285), threadID: "main", agentID: 285, userID: 99, store: st, hub: h}

	first, err := ch.SetCurrentStatus(framework.CurrentStatusRequest{
		Title:  "Daily sync completed",
		State:  "completed",
		Next:   "Generate the weekly report",
		NextAt: "2026-07-20T11:00:00+02:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = readUserMessage(t, userCh)
	statuses, err := st.ListCurrentStatuses([]int64{285}, "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Next != "Generate the weekly report" || statuses[0].NextAt != "2026-07-20T09:00:00Z" || statuses[0].Message.ThreadID != "main" {
		t.Fatalf("current statuses = %#v", statuses)
	}
	firstUpdatedAt := statuses[0].UpdatedAt
	if firstUpdatedAt.IsZero() {
		t.Fatalf("current status has no explicit updated_at: %#v", statuses[0])
	}

	second, err := ch.SetCurrentStatus(framework.CurrentStatusRequest{
		Title: "No further work planned",
		State: "completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = readUserMessage(t, userCh)
	if second.MessageID != first.MessageID {
		t.Fatalf("status row appended instead of replaced: first=%d second=%d", first.MessageID, second.MessageID)
	}
	statuses, err = st.ListCurrentStatuses([]int64{285}, "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Next != "" || statuses[0].NextAt != "" {
		t.Fatalf("replacement retained stale next action: %#v", statuses)
	}
	if !statuses[0].UpdatedAt.After(firstUpdatedAt) {
		t.Fatalf("replacement updated_at=%s, want after %s", statuses[0].UpdatedAt, firstUpdatedAt)
	}
}

func TestCurrentStatusMonitoringIgnoresConversationScopedLegacyRows(t *testing.T) {
	db := openChannelTestDB(t, true)
	defer db.Close()
	st := newStore(db)
	if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (285, 99, 'Media Agent', 'default')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureDefaultChat(285); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO channel_chat_chats (id, agent_id, title, project_id, owner_user_id, kind, thread_id)
		VALUES ('conv-status-legacy', 285, 'Legacy conversation', 'default', 99, 'direct', 'chat-conv-status-legacy')`); err != nil {
		t.Fatal(err)
	}
	hub := newHub()
	conversation := &chatChannel{
		chatID: "conv-status-legacy", threadID: "chat-conv-status-legacy",
		agentID: 285, userID: 99, store: st, hub: hub,
	}
	if _, err := conversation.SetCurrentStatus(framework.CurrentStatusRequest{
		Title: "Conversation import", State: "working",
	}); err != nil {
		t.Fatal(err)
	}
	statuses, err := st.ListCurrentStatuses([]int64{285}, "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 0 {
		t.Fatalf("conversation-scoped legacy status leaked into global monitoring: %#v", statuses)
	}

	main := &chatChannel{
		chatID: defaultChatID(285), threadID: "main",
		agentID: 285, userID: 99, store: st, hub: hub,
	}
	if _, err := main.SetCurrentStatus(framework.CurrentStatusRequest{
		Title: "Main import", State: "working",
	}); err != nil {
		t.Fatal(err)
	}
	statuses, err = st.ListCurrentStatuses([]int64{285}, "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Title != "Main import" || statuses[0].Message.ThreadID != "main" {
		t.Fatalf("global status is not exclusively main-owned: %#v", statuses)
	}
}

func TestChatChannelFactoryMarksDefaultSinkAsMain(t *testing.T) {
	db := openChannelTestDB(t, true)
	defer db.Close()
	st := newStore(db)
	if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (285, 99, 'Media Agent', 'default')`); err != nil {
		t.Fatal(err)
	}
	factory := &chatChannelFactory{store: st, hub: newHub()}
	built, err := factory.Build(nil, framework.InstanceInfo{ID: 285, UserID: 99})
	if err != nil {
		t.Fatal(err)
	}
	channel, ok := built.(*chatChannel)
	if !ok {
		t.Fatalf("factory built %T, want *chatChannel", built)
	}
	if channel.chatID != defaultChatID(285) || channel.threadID != "main" {
		t.Fatalf("factory sink chat=%q thread=%q, want hidden default sink on main", channel.chatID, channel.threadID)
	}
}

func TestChatChannelCurrentStatusValidatesNextAction(t *testing.T) {
	db := openChannelTestDB(t, false)
	defer db.Close()
	st := newStore(db)
	if _, err := st.EnsureDefaultChat(285); err != nil {
		t.Fatal(err)
	}
	ch := &chatChannel{chatID: defaultChatID(285), store: st, hub: newHub()}
	for _, tc := range []struct {
		name string
		req  framework.CurrentStatusRequest
		want string
	}{
		{
			name: "timestamp without action",
			req:  framework.CurrentStatusRequest{Title: "Waiting", State: "waiting", NextAt: "2026-07-20T09:00:00Z"},
			want: "next_at requires next",
		},
		{
			name: "invalid timestamp",
			req:  framework.CurrentStatusRequest{Title: "Waiting", State: "waiting", Next: "Run report", NextAt: "Monday morning"},
			want: "next_at must be an RFC3339 timestamp",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ch.SetCurrentStatus(tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
	if _, err := ch.SetCurrentStatus(framework.CurrentStatusRequest{
		Title: "Importing contacts", State: "working", Next: "Validate rejected rows",
	}); err != nil {
		t.Fatalf("next without next_at should be accepted: %v", err)
	}
}

func TestCurrentStatusWaitsForParallelMessageWriter(t *testing.T) {
	db := openChannelContentionTestDB(t)
	defer db.Close()
	st := newStore(db)
	chatID := defaultChatID(285)
	if _, err := st.EnsureDefaultChat(285); err != nil {
		t.Fatal(err)
	}
	components := []framework.ChatComponent{{
		App: "channel-chat", Name: "status-card", Props: map[string]any{"title": "Starting", "state": "working"},
	}}
	first, err := st.UpsertCurrentStatus(chatID, 285, "main", "Status: Starting", components)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	writer, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ExecContext(ctx, `
		INSERT INTO channel_chat_messages
			(chat_id, role, content, thread_id, status, components_json, attachments_json)
		VALUES (?, 'agent', 'Visible reply', 'main', 'final', '[]', '[]')`, chatID); err != nil {
		_, _ = writer.ExecContext(ctx, `ROLLBACK`)
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		updated := []framework.ChatComponent{{
			App: "channel-chat", Name: "status-card", Props: map[string]any{"title": "Done", "state": "completed"},
		}}
		result, err := st.UpsertCurrentStatus(chatID, 285, "main", "Status: Done", updated)
		if err == nil && result.ID != first.ID {
			err = fmt.Errorf("status row changed from %d to %d", first.ID, result.ID)
		}
		done <- err
	}()

	select {
	case err := <-done:
		_, _ = writer.ExecContext(ctx, `ROLLBACK`)
		t.Fatalf("status update returned before parallel message committed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := writer.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("status update after parallel message: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("status update did not resume after parallel message committed")
	}

	var statusRows, visibleRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM channel_chat_messages WHERE chat_id=? AND components_json LIKE '%"status-card"%'`, chatID).Scan(&statusRows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM channel_chat_messages WHERE chat_id=? AND content='Visible reply'`, chatID).Scan(&visibleRows); err != nil {
		t.Fatal(err)
	}
	if statusRows != 1 || visibleRows != 1 {
		t.Fatalf("status rows=%d visible rows=%d, want 1 each", statusRows, visibleRows)
	}
}

func TestChatChannelSuppressesImmediateDuplicateRetry(t *testing.T) {
	db := openChannelContentionTestDB(t)
	defer db.Close()
	st := newStore(db)
	chatID := defaultChatID(285)
	if _, err := st.EnsureDefaultChat(285); err != nil {
		t.Fatal(err)
	}
	h := newHub()
	chatEvents, _, cancelChat := h.subscribe(chatID)
	defer cancelChat()
	userEvents, _, cancelUser := h.subscribeUser(99)
	defer cancelUser()
	ch := &chatChannel{chatID: chatID, threadID: "main", agentID: 285, userID: 99, store: st, hub: h}
	const reply = "Done. The weekly report requirement is restored."

	firstReceipt, err := ch.SendWithReceipt(reply, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !firstReceipt.Inserted || firstReceipt.MessageID == 0 {
		t.Fatalf("first receipt=%+v, want newly inserted message", firstReceipt)
	}
	firstChat := readUserMessage(t, chatEvents)
	firstUser := readUserMessage(t, userEvents)
	if firstChat.ID != firstUser.ID {
		t.Fatalf("chat event id=%d user event id=%d", firstChat.ID, firstUser.ID)
	}

	duplicateReceipt, err := ch.SendWithReceipt(reply, nil)
	if err != nil {
		t.Fatal(err)
	}
	if duplicateReceipt.Inserted || duplicateReceipt.MessageID != firstReceipt.MessageID {
		t.Fatalf("duplicate receipt=%+v, want suppressed message %d", duplicateReceipt, firstReceipt.MessageID)
	}
	assertNoChannelMessage(t, chatEvents, "per-chat stream")
	assertNoChannelMessage(t, userEvents, "user stream")
	rows, err := st.ListRecentMessages(chatID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("immediate retry persisted %d visible rows, want 1", len(rows))
	}

	// The same text is legitimate after a new user turn and must not be
	// swallowed by the retry guard.
	if _, err := st.Append(chatID, "user", "Please confirm again", nil, "main", "final", nil); err != nil {
		t.Fatal(err)
	}
	if err := ch.Send(reply); err != nil {
		t.Fatal(err)
	}
	secondChat := readUserMessage(t, chatEvents)
	secondUser := readUserMessage(t, userEvents)
	if secondChat.ID == firstChat.ID || secondUser.ID != secondChat.ID {
		t.Fatalf("new-turn reply ids first=%d chat=%d user=%d", firstChat.ID, secondChat.ID, secondUser.ID)
	}
	rows, err = st.ListRecentMessages(chatID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("new user turn rows=%d, want agent+user+agent", len(rows))
	}
}

func TestChatChannelPersistsLifecyclePhaseAndIncludesItInRetryIdentity(t *testing.T) {
	db := openChannelContentionTestDB(t)
	defer db.Close()
	st := newStore(db)
	chatID := defaultChatID(285)
	if _, err := st.EnsureDefaultChat(285); err != nil {
		t.Fatal(err)
	}
	ch := &chatChannel{
		chatID: chatID, threadID: "chat-" + chatID, agentID: 285,
		userID: 99, store: st, hub: newHub(),
	}

	ack, err := ch.SendWithReceiptAndPhase("Working on it.", nil, "acknowledgement")
	if err != nil || !ack.Inserted {
		t.Fatalf("ack receipt=%+v err=%v", ack, err)
	}
	// Identical copy in a different phase is not an idempotent retry: it has a
	// distinct lifecycle meaning and must reach the transcript.
	final, err := ch.SendWithReceiptAndPhase("Working on it.", nil, "final")
	if err != nil || !final.Inserted || final.MessageID == ack.MessageID {
		t.Fatalf("final receipt=%+v err=%v, ack=%+v", final, err, ack)
	}

	rows, err := st.ListRecentMessages(chatID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d, want 2", len(rows))
	}
	if rows[0].Metadata["phase"] != "acknowledgement" || rows[1].Metadata["phase"] != "final" {
		t.Fatalf("phases=%v, %v", rows[0].Metadata, rows[1].Metadata)
	}

	legacy, err := ch.SendWithReceipt("Legacy final.", nil)
	if err != nil || !legacy.Inserted {
		t.Fatalf("legacy receipt=%+v err=%v", legacy, err)
	}
	legacyMessage, err := st.GetMessage(legacy.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if legacyMessage.Metadata["phase"] != "final" {
		t.Fatalf("legacy metadata=%v, want final", legacyMessage.Metadata)
	}
}

func TestChatChannelSuppressesImmediateReorderedFinalButKeepsAcknowledgement(t *testing.T) {
	db := openChannelContentionTestDB(t)
	defer db.Close()
	st := newStore(db)
	chatID := defaultChatID(285)
	if _, err := st.EnsureDefaultChat(285); err != nil {
		t.Fatal(err)
	}
	ch := &chatChannel{chatID: chatID, threadID: "chat-" + chatID, agentID: 285, userID: 99, store: st, hub: newHub()}

	ack := "I’m updating the daily notification to 10:00 UTC and will confirm once the change is applied."
	firstFinal := "Updated: the notification will now be sent daily at 10:00 UTC, with the exact text: Daily check-in."
	reorderedFinal := "Updated: the daily notification will now be sent at 10:00 UTC with the exact text: Daily check-in."
	if err := ch.Send(ack); err != nil {
		t.Fatal(err)
	}
	firstReceipt, err := ch.SendWithReceipt(firstFinal, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !firstReceipt.Inserted {
		t.Fatal("distinct final was incorrectly suppressed as its acknowledgement")
	}
	duplicateReceipt, err := ch.SendWithReceipt(reorderedFinal, nil)
	if err != nil {
		t.Fatal(err)
	}
	if duplicateReceipt.Inserted || duplicateReceipt.MessageID != firstReceipt.MessageID {
		t.Fatalf("reordered final receipt=%+v, want suppressed message %d", duplicateReceipt, firstReceipt.MessageID)
	}

	// Exact output observed from Codex: a receipt turn changed only the
	// presentation verb and daily/every-day phrasing.
	confirmedFinal := "Confirmed: “Daily check-in.” will now be sent daily at 10:00 UTC."
	updatedFinal := "Updated: “Daily check-in.” will now be sent every day at 10:00 UTC."
	if _, err := st.Append(chatID, "user", "Change the schedule again", nil, "", "final", nil); err != nil {
		t.Fatal(err)
	}
	confirmedReceipt, err := ch.SendWithReceipt(confirmedFinal, nil)
	if err != nil || !confirmedReceipt.Inserted {
		t.Fatalf("confirmed final receipt=%+v err=%v", confirmedReceipt, err)
	}
	updatedReceipt, err := ch.SendWithReceipt(updatedFinal, nil)
	if err != nil {
		t.Fatal(err)
	}
	if updatedReceipt.Inserted || updatedReceipt.MessageID != confirmedReceipt.MessageID {
		t.Fatalf("daily/every-day receipt=%+v, want suppressed message %d", updatedReceipt, confirmedReceipt.MessageID)
	}

	rows, err := st.ListRecentMessages(chatID, 20)
	if err != nil {
		t.Fatal(err)
	}
	contents := make([]string, 0, len(rows))
	for _, row := range rows {
		contents = append(contents, row.Content)
	}
	if len(contents) != 4 || contents[0] != ack || contents[1] != firstFinal || contents[2] != "Change the schedule again" || contents[3] != confirmedFinal {
		t.Fatalf("visible messages=%q, want one final per user turn", contents)
	}
}

func TestHubKeepsChannelActiveAcrossShortRefreshGap(t *testing.T) {
	h := newHub()
	h.subscriberGrace = 20 * time.Millisecond
	_, _, cancel := h.subscribe("default-285")
	if !h.hasSubscribers("default-285") {
		t.Fatal("subscribed chat should be active")
	}
	cancel()
	if !h.hasSubscribers("default-285") {
		t.Fatal("refresh grace should keep chat active briefly")
	}
	time.Sleep(30 * time.Millisecond)
	if h.hasSubscribers("default-285") {
		t.Fatal("closed tab should become inactive after grace")
	}
}

func TestInboxDismissFiltersMessagesAndPersistsProps(t *testing.T) {
	db := openChannelTestDB(t, true)
	defer db.Close()
	st := newStore(db)
	if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (285, 99, 'Test Agent', 'default')`); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	if _, err := st.EnsureDefaultChat(285); err != nil {
		t.Fatalf("EnsureDefaultChat: %v", err)
	}
	msg, err := st.Append(defaultChatID(285), "agent", "Report: Pantry check", nil, "main", "final", []framework.ChatComponent{{
		App:  "channel-chat",
		Name: "report-card",
		Props: map[string]any{
			"title":   "Pantry check",
			"summary": "Milk is on the list.",
		},
	}})
	if err != nil {
		t.Fatalf("append report: %v", err)
	}
	before, err := st.ListReportMessages([]int64{285}, "default", 20)
	if err != nil {
		t.Fatalf("ListReportMessages before: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("reports before dismiss=%d, want 1", len(before))
	}
	components, err := applyInboxDismiss(msg.Components, 99)
	if err != nil {
		t.Fatalf("applyInboxDismiss: %v", err)
	}
	updated, err := st.UpdateMessageComponents(msg.ID, components)
	if err != nil {
		t.Fatalf("UpdateMessageComponents: %v", err)
	}
	props := updated.Components[0].Props
	if props["dismissed"] != true || props["dismissed_at"] == "" || props["dismissed_by"] == nil {
		t.Fatalf("dismiss props not persisted: %#v", props)
	}
	after, err := st.ListReportMessages([]int64{285}, "default", 20)
	if err != nil {
		t.Fatalf("ListReportMessages after: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("reports after dismiss=%d, want 0", len(after))
	}
}

func TestListRecentMessagesReturnsNewestPageChronologically(t *testing.T) {
	db := openChannelTestDB(t, false)
	defer db.Close()
	st := newStore(db)
	chatID := defaultChatID(285)
	if _, err := st.EnsureDefaultChat(285); err != nil {
		t.Fatalf("EnsureDefaultChat: %v", err)
	}
	for i := 1; i <= 7; i++ {
		if _, err := st.Append(chatID, "agent", fmt.Sprintf("message-%d", i), nil, "main", "final", nil); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	rows, err := st.ListRecentMessages(chatID, 3)
	if err != nil {
		t.Fatalf("ListRecentMessages: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows=%d, want 3", len(rows))
	}
	got := []string{rows[0].Content, rows[1].Content, rows[2].Content}
	want := []string{"message-5", "message-6", "message-7"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d=%q, want %q (all=%v)", i, got[i], want[i], got)
		}
	}
}

func openChannelTestDB(t *testing.T, withAgents bool) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "channel-chat-test.db")+"?_pragma=busy_timeout(2000)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE agents (
			id INTEGER PRIMARY KEY,
			user_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			project_id TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE channel_chat_chats (
			id TEXT PRIMARY KEY,
			agent_id INTEGER NOT NULL,
			title TEXT NOT NULL DEFAULT 'Chat',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			last_seen_id INTEGER NOT NULL DEFAULT 0,
			thread_id TEXT NOT NULL DEFAULT '',
			project_id TEXT NOT NULL DEFAULT '',
			owner_user_id INTEGER NOT NULL DEFAULT 0,
			kind TEXT NOT NULL DEFAULT 'direct',
			archived_at DATETIME
		);
		CREATE TABLE channel_chat_participants (
			chat_id TEXT NOT NULL,
			agent_id INTEGER NOT NULL,
			is_lead INTEGER NOT NULL DEFAULT 0,
			joined_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (chat_id, agent_id)
		);
		CREATE TABLE channel_chat_messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id TEXT NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			user_id INTEGER,
			agent_id INTEGER,
			thread_id TEXT,
			status TEXT NOT NULL DEFAULT 'final',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			components_json TEXT NOT NULL DEFAULT '[]',
			attachments_json TEXT NOT NULL DEFAULT '[]',
			metadata_json TEXT NOT NULL DEFAULT '{}',
			client_message_id TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE channel_chat_deliveries (
			message_id INTEGER NOT NULL,
			agent_id INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			delivered_at DATETIME,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (message_id, agent_id)
		);
	`); err != nil {
		db.Close()
		t.Fatalf("create schema: %v", err)
	}
	_ = withAgents
	return db
}

func openChannelContentionTestDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "channel-chat.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(2000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(`CREATE TABLE agents (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL, name TEXT NOT NULL, project_id TEXT NOT NULL DEFAULT '')`); err != nil {
		db.Close()
		t.Fatalf("create agents schema: %v", err)
	}
	if err := framework.RunMigrations(db, "channel-chat", New(nil).Migrations()); err != nil {
		db.Close()
		t.Fatalf("apply channel migrations: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO agents (id, user_id, name, project_id) VALUES (285, 99, 'Test Agent', 'default')`); err != nil {
		db.Close()
		t.Fatalf("seed agent: %v", err)
	}
	return db
}

func readUserMessage(t *testing.T, ch <-chan Message) Message {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for user-stream message")
		return Message{}
	}
}

func assertNoChannelMessage(t *testing.T, ch <-chan Message, stream string) {
	t.Helper()
	select {
	case m := <-ch:
		t.Fatalf("%s received duplicate message id=%d content=%q", stream, m.ID, m.Content)
	case <-time.After(75 * time.Millisecond):
	}
}
