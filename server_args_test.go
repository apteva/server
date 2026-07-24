package main

import (
	"strings"
	"testing"
)

func TestParseServerInvocationAcceptsSupportedModes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		mode serverInvocationMode
		id   int64
	}{
		{name: "normal server", mode: serverModeRun},
		{name: "version long", args: []string{"--version"}, mode: serverModeVersion},
		{name: "version short", args: []string{"-v"}, mode: serverModeVersion},
		{name: "help", args: []string{"--help"}, mode: serverModeHelp},
		{name: "preflight", args: []string{"--preflight"}, mode: serverModePreflight},
		{name: "mcp proxy", args: []string{"--mcp-proxy", "--connection-id=42"}, mode: serverModeMCPProxy, id: 42},
		{name: "mcp gateway", args: []string{"--mcp-gateway", "--user-id=17"}, mode: serverModeMCPGateway, id: 17},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseServerInvocation(tt.args)
			if err != nil {
				t.Fatalf("parseServerInvocation(%q): %v", tt.args, err)
			}
			if got.mode != tt.mode {
				t.Fatalf("mode=%v, want %v", got.mode, tt.mode)
			}
			switch tt.mode {
			case serverModeMCPProxy:
				if got.connectionID != tt.id {
					t.Fatalf("connectionID=%d, want %d", got.connectionID, tt.id)
				}
			case serverModeMCPGateway:
				if got.userID != tt.id {
					t.Fatalf("userID=%d, want %d", got.userID, tt.id)
				}
			}
		})
	}
}

func TestParseServerInvocationRejectsArgumentsThatWouldOtherwiseStartServer(t *testing.T) {
	tests := [][]string{
		{"--unsupported"},
		{"ingress"},
		{"ingress", "--help"},
		{"--version", "extra"},
		{"--preflight", "extra"},
		{"--mcp-proxy"},
		{"--mcp-proxy", "--connection-id=0"},
		{"--mcp-proxy", "--connection-id=not-a-number"},
		{"--mcp-gateway", "--connection-id=12"},
		{"--mcp-gateway", "--user-id=1", "--extra"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			if got, err := parseServerInvocation(args); err == nil {
				t.Fatalf("parseServerInvocation(%q) unexpectedly succeeded: %+v", args, got)
			}
		})
	}
}
