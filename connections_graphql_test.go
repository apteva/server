package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func executeGraphQLFixture(t *testing.T, payload string, responseError *ResponseErrorDef, responsePath *string) *ExecuteResult {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/graphql-response+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(upstream.Close)

	result, err := executeIntegrationTool(
		&AppTemplate{Slug: "graphql-test", BaseURL: upstream.URL},
		&AppToolDef{
			Name:          "operation",
			Method:        http.MethodPost,
			Path:          "/graphql",
			InputSchema:   map[string]any{"type": "object", "properties": map[string]any{}},
			ResponseError: responseError,
			ResponsePath:  responsePath,
		},
		nil,
		map[string]any{},
		"",
	)
	if err != nil {
		t.Fatalf("executeIntegrationTool: %v", err)
	}
	return result
}

func TestExecuteIntegrationToolGraphQLTopLevelErrorsPreservePartialData(t *testing.T) {
	result := executeGraphQLFixture(t, `{
		"data":{"inventoryLevelAdjust":{"id":"adjustment-1"}},
		"errors":[{"message":"Inventory item was not found","path":["inventoryLevelAdjust"]}]
	}`, &ResponseErrorDef{Type: "graphql"}, nil)

	if result.Success || result.Status != http.StatusOK {
		t.Fatalf("result=%+v, want semantic failure with upstream status 200", result)
	}
	data, ok := result.Data.(map[string]any)
	if !ok || data["error"] != "upstream_graphql_error" || data["message"] != "Inventory item was not found" {
		t.Fatalf("normalized error=%#v", result.Data)
	}
	partial, ok := data["partial_data"].(map[string]any)
	if !ok || partial["inventoryLevelAdjust"] == nil {
		t.Fatalf("partial_data=%#v", data["partial_data"])
	}
	details, ok := data["details"].([]any)
	if !ok || len(details) != 1 {
		t.Fatalf("details=%#v", data["details"])
	}
}

func TestExecuteIntegrationToolGraphQLConfiguredUserErrors(t *testing.T) {
	path := "data.integrationOrderCreate"
	result := executeGraphQLFixture(t, `{
		"data":{"integrationOrderCreate":{"order":null,"userErrors":[{"field":["items"],"message":"Inventory item was not found"}]}}
	}`, &ResponseErrorDef{
		Type:  "graphql",
		Paths: []string{"errors", "data.integrationOrderCreate.userErrors"},
	}, &path)

	if result.Success || result.Status != http.StatusOK {
		t.Fatalf("result=%+v, want configured userErrors failure", result)
	}
	data := result.Data.(map[string]any)
	if data["message"] != "Inventory item was not found" {
		t.Fatalf("normalized error=%#v", data)
	}
}

func TestExecuteIntegrationToolGraphQLEmptyErrorsAllowResponseExtraction(t *testing.T) {
	path := "data.integrationOrderCreate"
	result := executeGraphQLFixture(t, `{
		"data":{"integrationOrderCreate":{"order":{"id":"order-1"},"userErrors":[]}},
		"errors":[]
	}`, &ResponseErrorDef{
		Type:  "graphql",
		Paths: []string{"errors", "data.integrationOrderCreate.userErrors"},
	}, &path)

	if !result.Success || result.Status != http.StatusOK {
		t.Fatalf("result=%+v, want success", result)
	}
	data, ok := result.Data.(map[string]any)
	if !ok || data["order"] == nil {
		t.Fatalf("extracted data=%#v", result.Data)
	}
}

func TestExecuteIntegrationToolMissingResponsePathIsContractFailure(t *testing.T) {
	for name, payload := range map[string]string{
		"missing":    `{"data":{}}`,
		"null":       `{"data":{"integrationOrderCreate":null}}`,
		"non-object": `{"data":"unexpected"}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := "data.integrationOrderCreate"
			result := executeGraphQLFixture(t, payload, nil, &path)
			if result.Success || result.Status != http.StatusOK {
				t.Fatalf("result=%+v, want HTTP 200 contract failure", result)
			}
			data, ok := result.Data.(map[string]any)
			if !ok || data["error"] != "response contract violation" {
				t.Fatalf("contract error=%#v", result.Data)
			}
		})
	}
}

func TestExecuteIntegrationToolGraphQLErrorPathMustBeArray(t *testing.T) {
	result := executeGraphQLFixture(t, `{"data":{},"errors":{"message":"wrong shape"}}`, &ResponseErrorDef{Type: "graphql"}, nil)
	if result.Success {
		t.Fatalf("result=%+v, want contract failure", result)
	}
	data := result.Data.(map[string]any)
	if data["error"] != "response contract violation" || !strings.Contains(fmt.Sprint(data["detail"]), "contain an array") {
		t.Fatalf("contract error=%#v", data)
	}
}

func TestMCPGraphQLFixedDocumentIsHiddenAndSemanticFailureIsError(t *testing.T) {
	const fixedDocument = "mutation HiddenCreateOrder { integrationOrderCreate { userErrors { message } } }"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode GraphQL request: %v", err)
		}
		if body["query"] != fixedDocument {
			t.Fatalf("query=%#v, want fixed catalog document", body["query"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"integrationOrderCreate":null},"errors":[{"message":"Order rejected"}]}`))
	}))
	defer upstream.Close()

	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.catalog = NewAppCatalog()
	s.catalog.Register(&AppTemplate{
		Slug:    "graphql-mcp-test",
		Name:    "GraphQL MCP Test",
		BaseURL: upstream.URL,
		Tools: []AppToolDef{{
			Name:        "create_order",
			Description: "Create an order",
			Method:      http.MethodPost,
			Path:        "/graphql",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"external_id": map[string]any{"type": "string"},
				},
			},
			RequestTransform: &RequestTransformDef{
				Type:      "json_wrap",
				Fields:    []string{},
				Constants: map[string]any{"query": fixedDocument},
				IncludeFields: map[string]string{
					"external_id": "variables.input.externalId",
				},
			},
			ResponseError: &ResponseErrorDef{Type: "graphql"},
		}},
	})
	encrypted, err := Encrypt(s.secret, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := s.store.CreateConnection(1, "graphql-mcp-test", "GraphQL MCP Test", "Orders", "bearer", encrypted, "")
	if err != nil {
		t.Fatal(err)
	}

	call := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp/connection/"+strconv.FormatInt(conn.ID, 10), strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:41234"
		rec := httptest.NewRecorder()
		authorizeTestMCPRequest(s, req)
		s.handleMCPEndpoint(rec, req)
		return rec
	}

	listed := call(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if listed.Code != http.StatusOK {
		t.Fatalf("tools/list status=%d body=%s", listed.Code, listed.Body.String())
	}
	if strings.Contains(listed.Body.String(), fixedDocument) || strings.Contains(listed.Body.String(), `"query"`) {
		t.Fatalf("fixed GraphQL document leaked into MCP schema: %s", listed.Body.String())
	}

	called := call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"create_order","arguments":{"external_id":"external-1"}}}`)
	if called.Code != http.StatusOK {
		t.Fatalf("tools/call status=%d body=%s", called.Code, called.Body.String())
	}
	var response struct {
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(called.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Result.IsError {
		t.Fatalf("MCP response did not expose semantic GraphQL failure: %s", called.Body.String())
	}
}
