package main

import (
	"strings"
	"testing"
	"time"
)

func TestBuildGhostAdminJWT(t *testing.T) {
	token, err := buildGhostAdminJWT("abc123:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", time.Unix(1700000000, 0))
	if err != nil {
		t.Fatalf("buildGhostAdminJWT returned error: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT parts, got %d", len(parts))
	}
	if !strings.Contains(parts[0], "eyJ") {
		t.Fatalf("unexpected header encoding: %s", parts[0])
	}
}
