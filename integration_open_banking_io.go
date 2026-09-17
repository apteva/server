package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	openbanking "github.com/open-banking-io/clients/go"
)

type openBankingTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t openBankingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(r.Clone(t.ctx))
}

func executeOpenBankingIO(ctx context.Context, tool *AppToolDef, credentials map[string]string, input map[string]any) (*ExecuteResult, error) {
	fail := func(status int, message string) (*ExecuteResult, error) {
		return &ExecuteResult{Success: false, Status: status, Data: map[string]any{"error": message}}, nil
	}
	baseURL := strings.TrimSpace(credentials["api_base_url"])
	if baseURL == "" {
		baseURL = "https://open-banking.io"
	}
	base, err := url.Parse(baseURL)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return fail(400, "A valid HTTPS api_base_url from the credentials bundle is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client, err := openbanking.New(base.String(), credentials["api_key"], credentials["private_key"], &http.Client{
		Timeout: 30 * time.Second, Transport: openBankingTransport{ctx, http.DefaultTransport},
		CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("redirects are disabled") },
	})
	if err != nil {
		return fail(400, "Invalid Open Banking Access credentials or PKCS#8 private key")
	}
	account, _ := input["account_id"].(string)
	if (tool.Name == "list_transactions" || tool.Name == "sync_account") && strings.TrimSpace(account) == "" {
		return fail(400, "account_id is required")
	}
	var data any
	switch tool.Name {
	case "list_accounts":
		data, err = client.GetAccounts()
	case "list_connections":
		data, err = client.GetConnections()
	case "list_transactions":
		q := openbanking.TransactionQuery{}
		q.From, _ = input["from"].(string)
		q.To, _ = input["to"].(string)
		for _, key := range []string{"limit", "offset"} {
			if v, ok := input[key]; ok {
				var n int
				raw := fmt.Sprint(v)
				if _, e := fmt.Sscanf(raw, "%d", &n); e != nil || fmt.Sprint(n) != raw || n < 0 || (key == "limit" && n == 0) {
					return fail(400, "Invalid pagination")
				}
				if key == "limit" {
					q.Limit = &n
				} else {
					q.Offset = &n
				}
			}
		}
		data, err = client.GetTransactions(account, q)
	case "sync_account":
		data, err = client.Sync(account)
	case "sync_all":
		data, err = client.SyncAll()
	default:
		return fail(400, "Unsupported open-banking.io tool")
	}
	if err != nil {
		var syncErr *openbanking.SyncError
		if errors.As(err, &syncErr) {
			headers := map[string]string{}
			if syncErr.RetryAfterSeconds >= 0 {
				headers["retry-after"] = fmt.Sprint(syncErr.RetryAfterSeconds)
			}
			return &ExecuteResult{Success: false, Status: syncErr.Status, Headers: headers, Data: map[string]any{"error": "Bank sync failed", "reason": syncErr.Reason, "bankErrorCode": syncErr.BankErrorCode}}, nil
		}
		return fail(502, "Open Banking Access request or decryption failed. Check credentials, API URL and bank consent.")
	}
	// The Go SDK uses untagged exported fields. Match the TypeScript SDK's
	// camelCase field names rather than expose a runtime-dependent tool contract.
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err = json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return &ExecuteResult{Success: true, Status: 200, Data: openBankingJSON(decoded)}, nil
}

func openBankingJSON(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			key = strings.ReplaceAll(key, "IDs", "Ids")
			key = strings.ReplaceAll(key, "ID", "Id")
			if len(key) > 0 {
				key = strings.ToLower(key[:1]) + key[1:]
			}
			out[key] = openBankingJSON(item)
		}
		return out
	case []any:
		for i := range v {
			v[i] = openBankingJSON(v[i])
		}
		return v
	default:
		return value
	}
}
