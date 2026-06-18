package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestIntegrationUsageMetricSESRecipients(t *testing.T) {
	conn := &Connection{AppSlug: "aws-ses"}
	qty, unit, _ := integrationUsageMetric(conn, "send_email", map[string]any{
		"Destination": map[string]any{
			"ToAddresses":  []any{"a@example.com", "b@example.com"},
			"CcAddresses":  []any{"c@example.com"},
			"BccAddresses": []any{"d@example.com"},
		},
	}, nil)
	if qty != 4 || unit != "recipient" {
		t.Fatalf("metric = %d %s, want 4 recipient", qty, unit)
	}
}

func TestIntegrationUsageMetricDeepgramDuration(t *testing.T) {
	conn := &Connection{AppSlug: "deepgram"}
	result := &ExecuteResult{
		Success: true,
		Status:  200,
		Data:    json.RawMessage(`{"metadata":{"duration":12.2}}`),
	}
	qty, unit, meta := integrationUsageMetric(conn, "listen", nil, result)
	if qty != 13 || unit != "second" {
		t.Fatalf("metric = %d %s, want 13 second", qty, unit)
	}
	if meta["duration_seconds"] != 13 {
		t.Fatalf("metadata duration_seconds = %#v, want 13", meta["duration_seconds"])
	}
}

func TestIntegrationUsageMetricDeepgramUnknownDuration(t *testing.T) {
	conn := &Connection{AppSlug: "deepgram"}
	qty, unit, _ := integrationUsageMetric(conn, "listen", nil, &ExecuteResult{Success: true, Status: 200, Data: map[string]any{}})
	if qty != 1 || unit != "request" {
		t.Fatalf("metric = %d %s, want 1 request", qty, unit)
	}
}

func TestIntegrationUsageSummaryScopesAndAggregates(t *testing.T) {
	store := newTestStore(t)
	u1, err := store.CreateUser("usage-owner@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	u2, err := store.CreateUser("usage-other@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	conn1, err := store.CreateConnection(u1.ID, "aws-ses", "AWS SES", "SES", "api_key", "enc", "proj-a")
	if err != nil {
		t.Fatal(err)
	}
	conn2, err := store.CreateConnection(u1.ID, "deepgram", "Deepgram", "Deepgram", "api_key", "enc", "proj-b")
	if err != nil {
		t.Fatal(err)
	}
	otherConn, err := store.CreateConnection(u2.ID, "aws-ses", "AWS SES", "SES Other", "api_key", "enc", "proj-a")
	if err != nil {
		t.Fatal(err)
	}

	recordIntegrationUsageDB(store.db, integrationUsageEvent{
		ProjectID:    "proj-a",
		ConnectionID: conn1.ID,
		AppSlug:      "aws-ses",
		Tool:         "send_email",
		Quantity:     2,
		Unit:         "recipient",
		Status:       "success",
	})
	recordIntegrationUsageDB(store.db, integrationUsageEvent{
		ProjectID:    "proj-a",
		ConnectionID: conn1.ID,
		AppSlug:      "aws-ses",
		Tool:         "send_email",
		Quantity:     1,
		Unit:         "recipient",
		Status:       "error",
		Error:        "boom",
	})
	recordIntegrationUsageDB(store.db, integrationUsageEvent{
		ProjectID:    "proj-b",
		ConnectionID: conn2.ID,
		AppSlug:      "deepgram",
		Tool:         "listen",
		Quantity:     12,
		Unit:         "second",
		Status:       "success",
	})
	recordIntegrationUsageDB(store.db, integrationUsageEvent{
		ProjectID:    "proj-a",
		ConnectionID: otherConn.ID,
		AppSlug:      "aws-ses",
		Tool:         "send_email",
		Quantity:     99,
		Unit:         "recipient",
		Status:       "success",
	})

	server := &Server{store: store}
	since := time.Now().Add(-time.Hour)
	rows, err := server.listIntegrationUsageRows(u1.ID, "proj-a", since)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1: %#v", len(rows), rows)
	}
	if got := rows[0]; got.AppSlug != "aws-ses" || got.Tool != "send_email" || got.Quantity != 3 || got.Calls != 2 || got.Errors != 1 {
		t.Fatalf("row = %#v, want aggregated aws-ses send_email quantity=3 calls=2 errors=1", got)
	}

	totals, err := server.listIntegrationUsageTotals(u1.ID, "", since)
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) != 2 {
		t.Fatalf("totals len = %d, want 2: %#v", len(totals), totals)
	}
	var recipientTotal, secondTotal integrationUsageTotal
	for _, total := range totals {
		switch total.Unit {
		case "recipient":
			recipientTotal = total
		case "second":
			secondTotal = total
		}
	}
	if recipientTotal.Quantity != 3 || recipientTotal.Calls != 2 || recipientTotal.Errors != 1 {
		t.Fatalf("recipient total = %#v, want quantity=3 calls=2 errors=1", recipientTotal)
	}
	if secondTotal.Quantity != 12 || secondTotal.Calls != 1 || secondTotal.Errors != 0 {
		t.Fatalf("second total = %#v, want quantity=12 calls=1 errors=0", secondTotal)
	}
}
