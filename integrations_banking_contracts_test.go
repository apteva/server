package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBankingPlaidOptionsContract(t *testing.T) {
	app := openBankingCatalog(t, "plaid")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		options, ok := body["options"].(map[string]any)
		if !ok {
			t.Fatal("missing options")
		}
		if options["count"] != float64(20) || options["offset"] != float64(50) || body["access_token"] != "item" || body["client_id"] != "client" || body["secret"] != "secret" {
			t.Errorf("wrong payload: %#v", body)
		}
		if _, ok := body["account_ids"]; ok {
			t.Error("account_ids leaked to root")
		}
		if _, ok := body["count"]; ok {
			t.Error("count leaked to root")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"transactions":[],"total_transactions":100}`)
	}))
	defer srv.Close()
	app.BaseURL = srv.URL
	for i := range app.Tools {
		if app.Tools[i].Name == "get_transactions" {
			result, err := executeIntegrationTool(app, &app.Tools[i], map[string]string{"client_id": "client", "secret": "secret"}, map[string]any{"access_token": "item", "start_date": "2026-09-01", "end_date": "2026-09-15", "account_ids": []any{"a"}, "count": 20, "offset": 50}, "")
			if err != nil || !result.Success {
				t.Fatalf("request failed: %#v %v", result, err)
			}
			return
		}
	}
	t.Fatal("tool missing")
}

func TestBankingTellerPaymentContract(t *testing.T) {
	app := openBankingCatalog(t, "teller")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Idempotency-Key") != "intent-1" {
			t.Error("wrong headers")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		payee, ok := body["payee"].(map[string]any)
		if !ok || payee["scheme"] != "zelle" || payee["address"] != "recipient@example.com" {
			t.Errorf("wrong payee: %#v", body)
		}
		if _, ok := body["idempotency_key"]; ok {
			t.Error("idempotency key leaked to body")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"connect_token":"mfa-challenge"}`)
	}))
	defer srv.Close()
	app.BaseURL = srv.URL
	for i := range app.Tools {
		if app.Tools[i].Name == "create_payment" {
			result, err := executeIntegrationTool(app, &app.Tools[i], map[string]string{"username": "token", "password": "x"}, map[string]any{"account_id": "a", "amount": "10.00", "payee": map[string]any{"scheme": "zelle", "address": "recipient@example.com"}, "idempotency_key": "intent-1"}, "")
			if err != nil || !result.Success {
				t.Fatalf("request failed: %#v %v", result, err)
			}
			raw, _ := json.Marshal(result.Data)
			if string(raw) != `{"connect_token":"mfa-challenge"}` {
				t.Fatalf("lost MFA response: %s", raw)
			}
			return
		}
	}
	t.Fatal("tool missing")
}
