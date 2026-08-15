package channelchat

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/apteva/server/apps/framework"
)

func TestDelegatedRESTActionsAreEnforcedByChannelChat(t *testing.T) {
	app := &App{}
	calls := 0
	handler := app.wrap(func(w http.ResponseWriter, _ *http.Request, _ *framework.AppCtx) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	})
	request := func(method, path, scopes string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("X-Apteva-Subject-Type", "user")
		req.Header.Set("X-Apteva-Subject-ID", "123")
		req.Header.Set("X-Apteva-Scopes", scopes)
		rec := httptest.NewRecorder()
		handler(rec, req, nil)
		return rec
	}

	allowed := request(http.MethodPost, "/api/apps/channel-chat/chats",
		`[{"type":"app_user","app":"channel-chat","actions":["chat.create"]}]`)
	if allowed.Code != http.StatusNoContent || calls != 1 {
		t.Fatalf("allowed status=%d calls=%d body=%s", allowed.Code, calls, allowed.Body.String())
	}
	wrongAction := request(http.MethodGet, "/apps/channel-chat/chats",
		`[{"type":"app_user","app":"channel-chat","actions":["chat.create"]}]`)
	if wrongAction.Code != http.StatusForbidden || calls != 1 {
		t.Fatalf("wrong action status=%d calls=%d", wrongAction.Code, calls)
	}
	unsupported := request(http.MethodDelete, "/apps/channel-chat/chats/conv-1",
		`[{"type":"app_user","app":"channel-chat","actions":["chat.update"]}]`)
	if unsupported.Code != http.StatusForbidden || calls != 1 {
		t.Fatalf("unsupported route status=%d calls=%d", unsupported.Code, calls)
	}
}
