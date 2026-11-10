package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func decodeCatalogMCPResult(status int, contentType string, body []byte, headers map[string]string, authArgs map[string]any) *ExecuteResult {
	failure := func(status int, message string) *ExecuteResult {
		for _, value := range authArgs {
			if secret, ok := value.(string); ok && secret != "" {
				message = strings.ReplaceAll(message, secret, "[redacted]")
			}
		}
		return &ExecuteResult{Status: status, Data: map[string]any{"error": message}, Headers: headers}
	}
	if status < 200 || status >= 300 {
		return failure(status, fmt.Sprintf("MCP endpoint returned HTTP %d", status))
	}

	payload, err := decodeMCPResponseBody(contentType, body)
	var rpc jsonRPCResponse
	if err != nil || json.Unmarshal(payload, &rpc) != nil || rpc.JSONRPC != "2.0" || rpc.ID != 1 {
		return failure(http.StatusBadGateway, "Invalid MCP response")
	}
	if rpc.Error != nil {
		if rpc.Error.Code == -32001 {
			return failure(http.StatusUnauthorized, rpc.Error.Message)
		}
		return failure(http.StatusBadGateway, rpc.Error.Message)
	}
	if len(rpc.Result) == 0 || rpc.Result[0] != '{' {
		return failure(http.StatusBadGateway, "Missing MCP tool result")
	}
	data, err := unwrapMCPToolResult(rpc.Result)
	if err != nil {
		return failure(http.StatusUnprocessableEntity, err.Error())
	}
	return &ExecuteResult{Success: true, Status: status, Data: data, Headers: headers}
}
