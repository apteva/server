package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type serverInvocationMode int

const (
	serverModeRun serverInvocationMode = iota
	serverModeVersion
	serverModeHelp
	serverModePreflight
	serverModeMCPProxy
	serverModeMCPGateway
)

type serverInvocation struct {
	mode         serverInvocationMode
	connectionID int64
	userID       int64
}

// parseServerInvocation classifies every supported apteva-server invocation
// before normal startup is allowed to touch boot counters, the database, app
// sidecars, or listening ports. The server is configured through environment
// variables, so an argument not listed here is always an operator error.
func parseServerInvocation(args []string) (serverInvocation, error) {
	if len(args) == 0 {
		return serverInvocation{mode: serverModeRun}, nil
	}

	requireNoExtraArgs := func(command string) error {
		if len(args) != 1 {
			return fmt.Errorf("%s does not accept additional arguments", command)
		}
		return nil
	}

	switch args[0] {
	case "--version", "-v", "version":
		if err := requireNoExtraArgs(args[0]); err != nil {
			return serverInvocation{}, err
		}
		return serverInvocation{mode: serverModeVersion}, nil
	case "--help", "-h", "help":
		if err := requireNoExtraArgs(args[0]); err != nil {
			return serverInvocation{}, err
		}
		return serverInvocation{mode: serverModeHelp}, nil
	case "--preflight":
		if err := requireNoExtraArgs(args[0]); err != nil {
			return serverInvocation{}, err
		}
		return serverInvocation{mode: serverModePreflight}, nil
	case "--mcp-proxy":
		id, err := parsePositiveInternalID(args[1:], "--connection-id")
		if err != nil {
			return serverInvocation{}, err
		}
		return serverInvocation{mode: serverModeMCPProxy, connectionID: id}, nil
	case "--mcp-gateway":
		id, err := parsePositiveInternalID(args[1:], "--user-id")
		if err != nil {
			return serverInvocation{}, err
		}
		return serverInvocation{mode: serverModeMCPGateway, userID: id}, nil
	default:
		return serverInvocation{}, fmt.Errorf("unsupported argument %q", args[0])
	}
}

func parsePositiveInternalID(args []string, name string) (int64, error) {
	if len(args) != 1 {
		return 0, fmt.Errorf("%s requires exactly one %s=<id> argument", strings.TrimPrefix(name, "--"), name)
	}
	prefix := name + "="
	if !strings.HasPrefix(args[0], prefix) {
		return 0, fmt.Errorf("unsupported argument %q", args[0])
	}
	raw := strings.TrimPrefix(args[0], prefix)
	if raw == "" {
		return 0, errors.New(name + " requires a positive integer")
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New(name + " requires a positive integer")
	}
	return id, nil
}

func serverUsage() string {
	return `Usage:
  apteva-server
  apteva-server --version
  apteva-server --preflight

Internal modes:
  apteva-server --mcp-proxy --connection-id=<id>
  apteva-server --mcp-gateway --user-id=<id>`
}
