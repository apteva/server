package main

import (
	"encoding/json"
	"testing"
)

// TestCatalogMockResponses_Parse asserts the curated mock_response shapes ship
// in the embedded catalog and parse into AppToolDef.MockResponse for the
// common integrations an agent reaches in a Environment. These are what
// executeIntegrationTool serves (instead of hitting the real API) when an
// agent runs inside a Environment — so they must load and be valid JSON objects.
//
// Not gated: pure catalog parse, no core/LLM.
func TestCatalogMockResponses_Parse(t *testing.T) {
	cat := NewAppCatalog()
	if err := cat.LoadFromDir("integrations-catalog"); err != nil {
		t.Fatalf("load catalog: %v", err)
	}

	// app -> tools that must carry a mock_response.
	want := map[string][]string{
		"slack":   {"send_message", "create_channel", "list_channels", "upload_file", "add_reaction"},
		"discord": {"send_message", "send_embed", "create_channel", "get_messages"},
		"github":  {"create_issue", "add_issue_comment", "create_pull", "merge_pull", "get_authenticated_user", "list_issues"},
		"gmail":   {"send_email", "create_draft", "list_messages", "get_message", "create_label"},
		"twilio":  {"send_sms", "send_whatsapp", "make_call", "get_balance", "lookup_phone_number"},
		// Social platforms.
		"facebook-api":  {"post_to_page", "post_photo_to_page", "publish_media_container", "get_page_posts", "list_pages"},
		"youtube-api":   {"upload_video_init", "post_comment", "create_playlist", "get_my_channel", "search"},
		"tiktok-api":    {"post_video", "post_photo", "get_publish_status", "get_user_info", "list_videos"},
		"instagram-api": {"create_media_container", "publish_media_container", "create_comment_reply", "get_user", "get_account_media"},
		"twitter-api":   {"post_tweet", "delete_tweet", "like_tweet", "retweet", "send_dm", "get_me"},
		"linkedin":      {"create_post", "get_profile", "list_posts", "get_company"},
		"pinterest":     {"create_pin", "create_board", "list_pins", "get_user_account"},
		"reddit":        {"submit_post", "comment", "vote", "get_subreddit_posts", "get_user_about"},
		"bluesky":       {"create_post", "get_profile", "get_feed", "search_posts"},
		// Bunny.net CDN + Stream.
		"bunny-cdn":    {"create_pullzone", "purge_cache", "create_storagezone", "upload_file", "list_pullzones"},
		"bunny-stream": {"create_video", "fetch_video", "delete_video", "create_library", "list_videos", "create_collection"},
	}

	for app, tools := range want {
		tmpl := cat.Get(app)
		if tmpl == nil {
			t.Errorf("%s: not in catalog", app)
			continue
		}
		byName := map[string]AppToolDef{}
		for _, td := range tmpl.Tools {
			byName[td.Name] = td
		}
		for _, name := range tools {
			td, ok := byName[name]
			if !ok {
				t.Errorf("%s.%s: tool missing from catalog", app, name)
				continue
			}
			if len(td.MockResponse) == 0 {
				t.Errorf("%s.%s: no mock_response", app, name)
				continue
			}
			// Must be valid JSON (object or array — both are real API shapes).
			var v any
			if err := json.Unmarshal(td.MockResponse, &v); err != nil {
				t.Errorf("%s.%s: mock_response is not valid JSON: %v", app, name, err)
				continue
			}
			switch v.(type) {
			case map[string]any, []any:
			default:
				t.Errorf("%s.%s: mock_response should be an object or array, got %T", app, name, v)
			}
		}
	}
}

func TestCatalogBunnyStreamUpdateVideoMetaTagsSchema(t *testing.T) {
	cat := NewAppCatalog()
	if err := cat.LoadFromDir("integrations-catalog"); err != nil {
		t.Fatalf("load catalog: %v", err)
	}

	tmpl := cat.Get("bunny-stream")
	if tmpl == nil {
		t.Fatal("bunny-stream: not in catalog")
	}

	var tool *AppToolDef
	for i := range tmpl.Tools {
		if tmpl.Tools[i].Name == "update_video" {
			tool = &tmpl.Tools[i]
			break
		}
	}
	if tool == nil {
		t.Fatal("bunny-stream.update_video: tool missing from catalog")
	}

	properties, ok := tool.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("update_video.input_schema.properties has type %T", tool.InputSchema["properties"])
	}
	metaTags, ok := properties["metaTags"].(map[string]any)
	if !ok {
		t.Fatalf("update_video.metaTags has type %T", properties["metaTags"])
	}
	items, ok := metaTags["items"].(map[string]any)
	if !ok {
		t.Fatalf("update_video.metaTags.items has type %T", metaTags["items"])
	}
	if got := items["type"]; got != "object" {
		t.Fatalf("update_video.metaTags.items.type = %v, want object", got)
	}
	itemProperties, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("update_video.metaTags.items.properties has type %T", items["properties"])
	}
	for _, key := range []string{"property", "value"} {
		field, ok := itemProperties[key].(map[string]any)
		if !ok {
			t.Fatalf("update_video.metaTags.items.properties.%s has type %T", key, itemProperties[key])
		}
		if got := field["type"]; got != "string" {
			t.Fatalf("update_video.metaTags.items.properties.%s.type = %v, want string", key, got)
		}
	}
}
