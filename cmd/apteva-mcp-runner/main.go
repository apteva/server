package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/apteva/server/internal/managedmcp"
	"github.com/dop251/goja"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type gatewayClient struct {
	baseURL string
	token   string
	client  *http.Client
	env     map[string]string
}

func main() {
	workspace := strings.TrimSpace(os.Getenv("APTEVA_MCP_WORKSPACE"))
	if workspace == "" {
		fmt.Fprintln(os.Stderr, "APTEVA_MCP_WORKSPACE is required")
		os.Exit(2)
	}
	def, err := managedmcp.Load(workspace)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	gateway := &gatewayClient{
		baseURL: strings.TrimRight(strings.TrimSpace(os.Getenv("APTEVA_MCP_GATEWAY_URL")), "/"),
		token:   strings.TrimSpace(os.Getenv("APTEVA_MCP_TOKEN")),
		client:  &http.Client{Timeout: 60 * time.Second},
		env:     environment(),
	}
	if gateway.baseURL == "" || gateway.token == "" {
		fmt.Fprintln(os.Stderr, "managed MCP gateway configuration is missing")
		os.Exit(2)
	}

	tools := make(map[string]managedmcp.Tool, len(def.Tools))
	for _, tool := range def.Tools {
		tools[tool.Name] = tool
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	writer := bufio.NewWriter(os.Stdout)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			writeResponse(writer, rpcResponse{JSONRPC: "2.0", ID: nil, Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		if req.ID == nil {
			continue
		}
		resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "initialize":
			resp.Result = map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "apteva-managed-mcp", "version": "1"},
			}
		case "ping":
			resp.Result = map[string]any{}
		case "tools/list":
			out := make([]map[string]any, 0, len(def.Tools))
			for _, tool := range def.Tools {
				row := map[string]any{
					"name":        tool.Name,
					"description": tool.Description,
					"inputSchema": tool.InputSchema,
				}
				if len(tool.OutputSchema) > 0 {
					row["outputSchema"] = tool.OutputSchema
				}
				out = append(out, row)
			}
			resp.Result = map[string]any{"tools": out}
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				resp.Error = &rpcError{Code: -32602, Message: "invalid tools/call parameters"}
				break
			}
			tool, ok := tools[params.Name]
			if !ok {
				resp.Error = &rpcError{Code: -32601, Message: "unknown tool: " + params.Name}
				break
			}
			result, err := execute(tool, params.Arguments, gateway)
			if err != nil {
				resp.Result = map[string]any{
					"isError": true,
					"content": []map[string]any{{"type": "text", "text": err.Error()}},
				}
				break
			}
			raw, err := json.Marshal(result)
			if err != nil {
				resp.Error = &rpcError{Code: -32603, Message: err.Error()}
				break
			}
			resp.Result = map[string]any{
				"content":           []map[string]any{{"type": "text", "text": string(raw)}},
				"structuredContent": result,
			}
		default:
			resp.Error = &rpcError{Code: -32601, Message: "method not found"}
		}
		writeResponse(writer, resp)
	}
	if err := scanner.Err(); err != nil {
		log.Printf("stdin: %v", err)
	}
}

func execute(tool managedmcp.Tool, input map[string]any, gateway *gatewayClient) (any, error) {
	runtime := goja.New()
	timer := time.AfterFunc(20*time.Second, func() {
		runtime.Interrupt("tool execution timed out after 20s")
	})
	defer timer.Stop()
	apteva := runtime.NewObject()
	_ = apteva.Set("integration", func(call goja.FunctionCall) goja.Value {
		alias, name, args := callArgs(runtime, call)
		out, err := gateway.call("integrations", alias, name, args)
		if err != nil {
			panic(runtime.NewGoError(err))
		}
		return runtime.ToValue(out)
	})
	_ = apteva.Set("app", func(call goja.FunctionCall) goja.Value {
		alias, name, args := callArgs(runtime, call)
		out, err := gateway.call("apps", alias, name, args)
		if err != nil {
			panic(runtime.NewGoError(err))
		}
		return runtime.ToValue(out)
	})
	_ = apteva.Set("env", func(call goja.FunctionCall) goja.Value {
		return runtime.ToValue(gateway.env[call.Argument(0).String()])
	})
	_ = apteva.Set("log", func(call goja.FunctionCall) goja.Value {
		values := make([]any, 0, len(call.Arguments))
		for _, arg := range call.Arguments {
			values = append(values, arg.Export())
		}
		log.Print(values...)
		return goja.Undefined()
	})

	value, err := runtime.RunString(managedmcp.HandlerProgram(tool.Code))
	if err != nil {
		return nil, err
	}
	fn, ok := goja.AssertFunction(value)
	if !ok {
		return nil, errors.New("handler did not compile to a function")
	}
	result, err := fn(goja.Undefined(), runtime.ToValue(input), apteva)
	if err != nil {
		return nil, err
	}
	return result.Export(), nil
}

func callArgs(runtime *goja.Runtime, call goja.FunctionCall) (string, string, map[string]any) {
	if len(call.Arguments) < 2 {
		panic(runtime.NewTypeError("alias and tool are required"))
	}
	alias := strings.TrimSpace(call.Argument(0).String())
	tool := strings.TrimSpace(call.Argument(1).String())
	args := map[string]any{}
	if len(call.Arguments) > 2 && !goja.IsUndefined(call.Argument(2)) && !goja.IsNull(call.Argument(2)) {
		exported := call.Argument(2).Export()
		if typed, ok := exported.(map[string]any); ok {
			args = typed
		} else {
			raw, _ := json.Marshal(exported)
			if err := json.Unmarshal(raw, &args); err != nil {
				panic(runtime.NewTypeError("tool input must be an object"))
			}
		}
	}
	if alias == "" || tool == "" {
		panic(runtime.NewTypeError("alias and tool are required"))
	}
	return alias, tool, args
}

func (g *gatewayClient) call(kind, alias, tool string, input map[string]any) (any, error) {
	raw, _ := json.Marshal(map[string]any{"tool": tool, "input": input})
	req, err := http.NewRequest(http.MethodPost, g.baseURL+"/"+kind+"/"+alias+"/call", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+g.token)
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", kind, alias, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var envelope struct {
		Data any `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode gateway response: %w", err)
	}
	return envelope.Data, nil
}

func writeResponse(writer *bufio.Writer, response rpcResponse) {
	raw, _ := json.Marshal(response)
	_, _ = writer.Write(raw)
	_ = writer.WriteByte('\n')
	_ = writer.Flush()
}

func environment() map[string]string {
	out := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok && !strings.HasPrefix(key, "APTEVA_MCP_") {
			out[key] = value
		}
	}
	return out
}
