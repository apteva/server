package channelchat

import (
	"context"
	"testing"
	"time"
)

func TestConversationListsWithOneDatabaseConnection(t *testing.T) {
	for _, name := range []string{"agent", "project", "subject", "unused"} {
		t.Run(name, func(t *testing.T) {
			db := openChannelTestDB(t, true)
			defer db.Close()
			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(1)
			if _, err := db.Exec(`
INSERT INTO agents(id,user_id,name,project_id) VALUES(285,99,'Agent','p');
INSERT INTO channel_chat_chats(id,agent_id,project_id,owner_user_id,thread_id,subject_type,subject_id)
 VALUES('conv-pool',285,'p',99,'chat-conv-pool','website_user','customer');
INSERT INTO channel_chat_participants(chat_id,agent_id,is_lead) VALUES('conv-pool',285,1),('conv-pool',286,0);`); err != nil {
				t.Fatal(err)
			}
			st := newStore(db)
			type result struct {
				chats []Chat
				err   error
			}
			done := make(chan result, 1)
			go func() {
				var r result
				switch name {
				case "agent":
					r.chats, r.err = st.ListChatsForAgent(285)
				case "project":
					r.chats, r.err = st.ListConversations(99, "p", false, "")
				case "subject":
					r.chats, r.err = st.ListChatsForAgentSubject(285, "website_user", "customer")
				case "unused":
					r.chats, r.err = st.UnusedConversationThreads()
				}
				done <- r
			}()
			select {
			case r := <-done:
				if r.err != nil || len(r.chats) != 1 || len(r.chats[0].AgentIDs) != 2 {
					t.Fatalf("listing lost conversation participants: %+v %v", r.chats, r.err)
				}
				if db.Stats().InUse != 0 {
					t.Fatal("listing retained a cursor")
				}
			case <-time.After(time.Second):
				// Release a regressed nested-query waiter before test cleanup.
				db.SetMaxOpenConns(2)
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = db.PingContext(ctx)
				select {
				case <-done:
				case <-ctx.Done():
				}
				t.Fatal("listing borrowed a second connection with its cursor open")
			}
		})
	}
}
