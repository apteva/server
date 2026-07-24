package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServerHealthReportsStartingUntilBootCompletes(t *testing.T) {
	rec := httptest.NewRecorder()
	writeServerHealth(rec, false)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("starting status=%d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var starting map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &starting); err != nil {
		t.Fatal(err)
	}
	if starting["ok"] != false || starting["status"] != "starting" {
		t.Fatalf("starting response=%v", starting)
	}

	rec = httptest.NewRecorder()
	writeServerHealth(rec, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("ready status=%d, want %d", rec.Code, http.StatusOK)
	}
	var ready map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &ready); err != nil {
		t.Fatal(err)
	}
	if ready["ok"] != true {
		t.Fatalf("ready response=%v", ready)
	}
}
