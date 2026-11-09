package main

import (
	"errors"
	"net/http"
	"regexp"
)

var youtubeReconnectChannel = regexp.MustCompile(`^UC[A-Za-z0-9_-]{22}$`)
var reconnectNumericID = regexp.MustCompile(`^[0-9]{1,32}$`)
var reconnectOpaqueID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func validReconnectIdentities(slug string, expected []oauthReconnectIdentity) bool {
	if len(expected) == 0 || len(expected) > 100 || (slug != "facebook-api" && len(expected) != 1) {
		return false
	}
	for _, identity := range expected {
		var valid bool
		switch slug {
		case "youtube-api":
			valid = identity.Kind == "youtube" && youtubeReconnectChannel.MatchString(identity.ID)
		case "facebook-api":
			valid = (identity.Kind == "facebook" || identity.Kind == "instagram") && reconnectNumericID.MatchString(identity.ID)
		case "twitter-api":
			valid = identity.Kind == "twitter" && reconnectNumericID.MatchString(identity.ID)
		case "tiktok-api":
			valid = identity.Kind == "tiktok" && reconnectOpaqueID.MatchString(identity.ID)
		case "pinterest":
			valid = identity.Kind == "pinterest" && reconnectNumericID.MatchString(identity.ID)
		}
		if !valid {
			return false
		}
	}
	return true
}

func verifyOAuthReconnectIdentities(r *http.Request, app *AppTemplate, tokens map[string]string, expected []oauthReconnectIdentity) (bool, error) {
	if app.Slug == "facebook-api" {
		return verifyOAuthReconnectPages(r, app, tokens, expected)
	}
	name, input := "", map[string]any{}
	switch app.Slug {
	case "youtube-api":
		name, input = "get_my_channel", map[string]any{"part": "id"}
	case "twitter-api":
		name = "get_me"
	case "tiktok-api":
		name, input = "get_user_info", map[string]any{"fields": "open_id"}
	case "pinterest":
		name = "get_user_account"
	}
	body, err := readOAuthReconnectTool(r, app, tokens, name, input)
	if err != nil {
		return false, err
	}
	id := ""
	switch app.Slug {
	case "youtube-api":
		items, _ := body["items"].([]any)
		if len(items) == 1 {
			item, _ := items[0].(map[string]any)
			id, _ = item["id"].(string)
		}
	case "twitter-api":
		user, _ := body["data"].(map[string]any)
		id, _ = user["id"].(string)
	case "tiktok-api":
		data, _ := body["data"].(map[string]any)
		user, _ := data["user"].(map[string]any)
		id, _ = user["open_id"].(string)
	case "pinterest":
		id, _ = body["id"].(string)
	}
	if id == "" {
		return false, errors.New("provider did not return a unique account ID")
	}
	return id == expected[0].ID, nil
}

func verifyOAuthReconnectPages(r *http.Request, app *AppTemplate, tokens map[string]string, expected []oauthReconnectIdentity) (bool, error) {
	remaining := map[oauthReconnectIdentity]bool{}
	for _, item := range expected {
		remaining[item] = true
	}
	input := map[string]any{"fields": "id,access_token,instagram_business_account{id}", "limit": 100}
	for page := 0; page < 10; page++ {
		body, err := readOAuthReconnectTool(r, app, tokens, "list_pages", input)
		if err != nil {
			return false, err
		}
		rows, ok := body["data"].([]any)
		if !ok {
			return false, errors.New("provider did not return Page identities")
		}
		for _, row := range rows {
			entry, _ := row.(map[string]any)
			if token, _ := entry["access_token"].(string); token == "" {
				continue
			}
			id, _ := entry["id"].(string)
			delete(remaining, oauthReconnectIdentity{Kind: "facebook", ID: id})
			ig, _ := entry["instagram_business_account"].(map[string]any)
			id, _ = ig["id"].(string)
			delete(remaining, oauthReconnectIdentity{Kind: "instagram", ID: id})
		}
		if len(remaining) == 0 {
			return true, nil
		}
		paging, _ := body["paging"].(map[string]any)
		next, _ := paging["next"].(string)
		cursors, _ := paging["cursors"].(map[string]any)
		after, _ := cursors["after"].(string)
		if next == "" || after == "" {
			return false, nil
		}
		input["after"] = after
	}
	return false, errors.New("could not verify all linked Pages within the read limit")
}

func readOAuthReconnectTool(r *http.Request, app *AppTemplate, tokens map[string]string, name string, input map[string]any) (map[string]any, error) {
	for i := range app.Tools {
		if app.Tools[i].Name != name {
			continue
		}
		res, err := executeIntegrationTool(app, &app.Tools[i], tokens, input, "", r.Context())
		if err == nil && res != nil && res.Success {
			if body, ok := res.Data.(map[string]any); ok && body != nil {
				return body, nil
			}
		}
		break
	}
	return nil, errors.New("provider identity check failed")
}
