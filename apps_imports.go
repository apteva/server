package main

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type importSourceDef map[string]any

// handleGetInstallImports returns the declarative import sources an app
// manifest exposes. Import execution is intentionally separate from app
// settings: this is operational UI, not static config.
func (s *Server) handleGetInstallImports(w http.ResponseWriter, r *http.Request) {
	installID, err := parseInstallIDFromImportsPath(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	manifest, err := installManifest(s, installID)
	if err != nil || manifest == nil {
		http.Error(w, "install not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"imports": manifest.Imports})
}

// handleRunInstallImport performs one manual import run and streams progress as
// newline-delimited JSON. v1 supports the tabular integration recipe shape used
// by Tables' Airtable import definition.
func (s *Server) handleRunInstallImport(w http.ResponseWriter, r *http.Request) {
	installID, sourceID, err := parseInstallImportRunPath(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	manifest, err := installManifest(s, installID)
	if err != nil || manifest == nil {
		http.Error(w, "install not found", http.StatusNotFound)
		return
	}
	source, err := importSourceByID(manifest.Imports, sourceID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	var body struct {
		ConnectionID int64  `json:"connection_id"`
		ProjectID    string `json:"project_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.ConnectionID <= 0 {
		http.Error(w, "connection_id required", http.StatusBadRequest)
		return
	}
	projectID, err := installProjectForImport(s, installID, body.ProjectID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	userID := getUserID(r)
	conn, _, err := s.store.GetConnection(userID, body.ConnectionID)
	if err != nil || conn == nil {
		http.Error(w, "connection not found", http.StatusNotFound)
		return
	}
	if want := stringPath(source, "integration"); want == "" || conn.AppSlug != want {
		http.Error(w, fmt.Sprintf("connection is %q, import source requires %q", conn.AppSlug, stringPath(source, "integration")), http.StatusBadRequest)
		return
	}
	if conn.ProjectID != "" && projectID != "" && conn.ProjectID != projectID {
		http.Error(w, "connection is scoped to another project", http.StatusForbidden)
		return
	}
	target := s.installedApps.Get(installID)
	if target == nil || target.SidecarURL == "" {
		http.Error(w, "app install is not running", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	emit := func(event map[string]any) {
		event["ts"] = time.Now().UTC().Format(time.RFC3339)
		_ = json.NewEncoder(w).Encode(event)
		if flusher != nil {
			flusher.Flush()
		}
	}

	stats, runErr := s.runTabularIntegrationImport(r, importRunCtx{
		InstallID:   installID,
		AppName:     manifest.Name,
		ProjectID:   projectID,
		Connection:  conn,
		SourceID:    sourceID,
		Source:      source,
		TargetMCP:   strings.TrimRight(target.SidecarURL, "/") + "/mcp",
		TargetToken: target.Token,
		Emit:        emit,
	})
	if runErr != nil {
		emit(map[string]any{"type": "error", "message": runErr.Error()})
		return
	}
	emit(map[string]any{"type": "done", "stats": stats})
}

type importRunCtx struct {
	InstallID   int64
	AppName     string
	ProjectID   string
	Connection  *Connection
	SourceID    string
	Source      importSourceDef
	TargetMCP   string
	TargetToken string
	Emit        func(map[string]any)
}

type importStats struct {
	Bases    int `json:"bases"`
	Tables   int `json:"tables"`
	Rows     int `json:"rows"`
	Inserted int `json:"inserted"`
	Updated  int `json:"updated"`
}

func (s *Server) runTabularIntegrationImport(r *http.Request, ctx importRunCtx) (importStats, error) {
	var stats importStats
	ctx.Emit(map[string]any{"type": "started", "source_id": ctx.SourceID})

	baseTool := stringPath(ctx.Source, "discover", "bases", "tool")
	baseResultPath := stringPath(ctx.Source, "discover", "bases", "result_path")
	baseCursorArg := stringPath(ctx.Source, "discover", "bases", "pagination", "cursor_arg")
	baseCursorPath := stringPath(ctx.Source, "discover", "bases", "pagination", "cursor_result_path")
	tableTool := stringPath(ctx.Source, "discover", "tables", "tool")
	tableResultPath := stringPath(ctx.Source, "discover", "tables", "result_path")
	recordTool := stringPath(ctx.Source, "read", "records", "tool")
	recordResultPath := stringPath(ctx.Source, "read", "records", "result_path")
	recordCursorArg := stringPath(ctx.Source, "read", "records", "pagination", "cursor_arg")
	recordCursorPath := stringPath(ctx.Source, "read", "records", "pagination", "cursor_result_path")
	writeTool := stringPath(ctx.Source, "write", "tool")
	if baseTool == "" || tableTool == "" || recordTool == "" || writeTool == "" {
		return stats, fmt.Errorf("import source %q is missing required tool declarations", ctx.SourceID)
	}

	baseInput := map[string]any{}
	for {
		ctx.Emit(map[string]any{"type": "step", "message": "Discovering bases"})
		baseData, err := s.executeImportIntegrationTool(r, ctx.Connection.ID, baseTool, baseInput)
		if err != nil {
			return stats, err
		}
		bases := arrayAt(baseData, baseResultPath)
		for _, rawBase := range bases {
			base := mapAtAny(rawBase)
			baseID := stringValue(base["id"])
			baseName := stringValue(base["name"])
			if baseID == "" {
				continue
			}
			stats.Bases++
			ctx.Emit(map[string]any{"type": "base", "id": baseID, "name": baseName})

			schemaInput := map[string]any{"base_id": baseID}
			schemaData, err := s.executeImportIntegrationTool(r, ctx.Connection.ID, tableTool, schemaInput)
			if err != nil {
				return stats, err
			}
			tables := arrayAt(schemaData, tableResultPath)
			for _, rawTable := range tables {
				table := mapAtAny(rawTable)
				tableID := stringValue(table["id"])
				tableName := stringValue(table["name"])
				if tableID == "" {
					continue
				}
				destName := importTableName(baseName, tableName, baseID, tableID)
				fields := arrayAt(table, "fields")
				columns, fieldMap := importColumnsFromAirtable(fields, ctx.Source)
				if err := s.ensureImportTable(ctx, destName, columns); err != nil {
					return stats, err
				}
				stats.Tables++
				ctx.Emit(map[string]any{"type": "table", "base": baseName, "table": tableName, "destination": destName, "status": "importing"})

				offset := ""
				page := 0
				tableRows := 0
				for {
					page++
					recordInput := map[string]any{"base_id": baseID, "table_id": tableID, "pageSize": 100}
					if offset != "" && recordCursorArg != "" {
						recordInput[recordCursorArg] = offset
					}
					recordData, err := s.executeImportIntegrationTool(r, ctx.Connection.ID, recordTool, recordInput)
					if err != nil {
						return stats, err
					}
					records := arrayAt(recordData, recordResultPath)
					rows := make([]any, 0, len(records))
					for _, rawRecord := range records {
						record := mapAtAny(rawRecord)
						row := airtableRecordToTableRow(record, fieldMap, ctx.Connection.ID, baseID, baseName, tableID, tableName)
						rows = append(rows, row)
					}
					if len(rows) > 0 {
						writeInput := map[string]any{
							"table":       destName,
							"key":         arrayAt(ctx.Source, "write.key"),
							"rows":        rows,
							"_project_id": ctx.ProjectID,
						}
						var out map[string]any
						if err := callInstalledAppToolResult(ctx, writeTool, writeInput, &out); err != nil {
							return stats, err
						}
						stats.Rows += len(rows)
						tableRows += len(rows)
						stats.Inserted += intNumber(out["inserted"])
						stats.Updated += intNumber(out["updated"])
					}
					ctx.Emit(map[string]any{"type": "page", "base": baseName, "table": tableName, "page": page, "rows": len(rows), "total_rows": stats.Rows})
					next := stringAt(recordData, recordCursorPath)
					if next == "" {
						break
					}
					offset = next
				}
				ctx.Emit(map[string]any{"type": "table_done", "base": baseName, "table": tableName, "rows": tableRows})
			}
		}
		next := stringAt(baseData, baseCursorPath)
		if next == "" || baseCursorArg == "" {
			break
		}
		baseInput[baseCursorArg] = next
	}
	return stats, nil
}

func (s *Server) executeImportIntegrationTool(r *http.Request, connID int64, toolName string, input map[string]any) (map[string]any, error) {
	userID := getUserID(r)
	conn, encCreds, err := s.store.GetConnection(userID, connID)
	if err != nil || conn == nil {
		return nil, fmt.Errorf("connection not found")
	}
	app := s.catalog.Get(conn.AppSlug)
	if app == nil {
		return nil, fmt.Errorf("integration app not in catalog: %s", conn.AppSlug)
	}
	var tool *AppToolDef
	prefix := s.store.CanonicalMCPNameForConnection(conn.ID)
	for i, t := range app.Tools {
		if t.Name == toolName || prefix+"_"+t.Name == toolName || conn.AppSlug+"_"+t.Name == toolName {
			tool = &app.Tools[i]
			break
		}
	}
	if tool == nil {
		return nil, fmt.Errorf("tool %q not found on %s", toolName, conn.AppSlug)
	}
	plain, err := Decrypt(s.secret, encCreds)
	if err != nil {
		return nil, fmt.Errorf("decrypt connection: %w", err)
	}
	var credentials map[string]string
	_ = json.Unmarshal([]byte(plain), &credentials)
	resolved, err := s.resolveConnectionContext(userID, app, credentials, input)
	if err != nil {
		s.recordIntegrationUsage(integrationUsageFromResult(conn, 0, "imports", tool.Name, input, nil, err))
		return nil, err
	}
	persistTargetID := connID
	if resolved.MasterConnID != 0 {
		persistTargetID = resolved.MasterConnID
	}
	persist := func(updated map[string]string) error {
		blob, err := json.Marshal(updated)
		if err != nil {
			return err
		}
		enc, err := Encrypt(s.secret, string(blob))
		if err != nil {
			return err
		}
		return s.store.UpdateConnectionCredentials(persistTargetID, enc)
	}
	environmentID := r.Header.Get("X-Apteva-Environment-Id")
	if environmentID == "" {
		environmentID = r.Header.Get("X-Apteva-Environment-Id")
	}
	result, err := executeIntegrationToolWithRefresh(resolved.App, tool, resolved.Credentials, resolved.Input, environmentID, persist)
	if err != nil {
		s.recordIntegrationUsage(integrationUsageFromResult(conn, 0, "imports", tool.Name, input, nil, err))
		return nil, err
	}
	s.recordIntegrationUsage(integrationUsageFromResult(conn, 0, "imports", tool.Name, input, result, nil))
	if result == nil || !result.Success {
		return nil, fmt.Errorf("%s failed: %v", toolName, result)
	}
	if m, ok := result.Data.(map[string]any); ok {
		return m, nil
	}
	b, _ := json.Marshal(result.Data)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("%s returned non-object data", toolName)
	}
	return out, nil
}

func callInstalledAppToolResult(ctx importRunCtx, tool string, input map[string]any, out any) error {
	raw, err := callAppMCPTool(ctx.TargetMCP, ctx.TargetToken, tool, input)
	if err != nil {
		return err
	}
	var content struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError,omitempty"`
	}
	if err := json.Unmarshal(raw, &content); err == nil && len(content.Content) > 0 {
		if content.IsError {
			return fmt.Errorf("%s: %s", tool, content.Content[0].Text)
		}
		return json.Unmarshal([]byte(content.Content[0].Text), out)
	}
	return json.Unmarshal(raw, out)
}

func (s *Server) ensureImportTable(ctx importRunCtx, name string, columns []map[string]any) error {
	var desc struct {
		Columns []struct {
			Name string `json:"name"`
		} `json:"columns"`
	}
	err := callInstalledAppToolResult(ctx, "tables_describe", map[string]any{"name": name, "_project_id": ctx.ProjectID}, &desc)
	if err != nil {
		var out map[string]any
		createErr := callInstalledAppToolResult(ctx, "tables_create", map[string]any{
			"name":        name,
			"columns":     columns,
			"_project_id": ctx.ProjectID,
		}, &out)
		if createErr != nil {
			return createErr
		}
		ctx.Emit(map[string]any{"type": "table", "destination": name, "status": "created"})
		return nil
	}
	existing := map[string]bool{}
	for _, c := range desc.Columns {
		existing[c.Name] = true
	}
	for _, col := range columns {
		colName := stringValue(col["name"])
		if colName == "" || existing[colName] {
			continue
		}
		input := map[string]any{"name": name, "add": col, "_project_id": ctx.ProjectID}
		var out map[string]any
		if err := callInstalledAppToolResult(ctx, "tables_alter", input, &out); err != nil {
			return err
		}
		existing[colName] = true
		ctx.Emit(map[string]any{"type": "column", "destination": name, "column": colName, "status": "added"})
	}
	return nil
}

func parseInstallIDFromImportsPath(path string) (int64, error) {
	rest := strings.TrimPrefix(path, "/apps/installs/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[1] != "imports" {
		return 0, fmt.Errorf("invalid imports path")
	}
	return strconv.ParseInt(parts[0], 10, 64)
}

func parseInstallImportRunPath(path string) (int64, string, error) {
	rest := strings.TrimPrefix(path, "/apps/installs/")
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || parts[1] != "imports" || parts[3] != "run" {
		return 0, "", fmt.Errorf("invalid import run path")
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, "", err
	}
	return id, parts[2], nil
}

func importSourceByID(imports map[string]any, id string) (importSourceDef, error) {
	for _, raw := range arrayAt(imports, "sources") {
		src := mapAtAny(raw)
		if stringValue(src["id"]) == id {
			return importSourceDef(src), nil
		}
	}
	return nil, fmt.Errorf("import source %q not found", id)
}

func installProjectForImport(s *Server, installID int64, fallback string) (string, error) {
	var projectID string
	if err := s.store.db.QueryRow(`SELECT COALESCE(project_id, '') FROM app_installs WHERE id = ?`, installID).Scan(&projectID); err != nil {
		return "", err
	}
	if projectID == "" {
		projectID = fallback
	}
	if projectID == "" {
		return "", fmt.Errorf("project_id required for global installs")
	}
	return projectID, nil
}

func importColumnsFromAirtable(fields []any, source importSourceDef) ([]map[string]any, map[string]string) {
	columns := []map[string]any{}
	used := map[string]bool{}
	fieldMap := map[string]string{}
	metadata := arrayAt(source, "schema.metadata_columns")
	for _, raw := range metadata {
		col := mapAtAny(raw)
		name := stringValue(col["name"])
		if name != "" {
			used[name] = true
		}
	}
	for _, raw := range fields {
		field := mapAtAny(raw)
		name := stringValue(field["name"])
		if name == "" {
			continue
		}
		colName := uniqueImportIdentifier(importIdentifier(name), used)
		used[colName] = true
		fieldMap[name] = colName
		columns = append(columns, map[string]any{
			"name":     colName,
			"type":     airtableFieldType(stringValue(field["type"])),
			"nullable": true,
		})
	}
	for _, raw := range metadata {
		col := mapAtAny(raw)
		name := stringValue(col["name"])
		if name == "" {
			continue
		}
		columns = append(columns, map[string]any{
			"name":     name,
			"type":     stringValue(col["type"]),
			"nullable": true,
		})
	}
	return columns, fieldMap
}

func airtableRecordToTableRow(record map[string]any, fieldMap map[string]string, connID int64, baseID, baseName, tableID, tableName string) map[string]any {
	row := map[string]any{}
	fields := mapAtAny(record["fields"])
	for sourceName, colName := range fieldMap {
		if v, ok := fields[sourceName]; ok {
			row[colName] = v
		}
	}
	row["source_connection_id"] = strconv.FormatInt(connID, 10)
	row["source_base_id"] = baseID
	row["source_base_name"] = baseName
	row["source_table_id"] = tableID
	row["source_table_name"] = tableName
	row["source_record_id"] = stringValue(record["id"])
	row["source_synced_at"] = time.Now().UTC().Format(time.RFC3339)
	row["source_deleted"] = false
	hashRaw, _ := json.Marshal(fields)
	sum := sha1.Sum(hashRaw)
	row["source_hash"] = fmt.Sprintf("%x", sum[:])
	return row
}

func airtableFieldType(t string) string {
	switch t {
	case "number", "percent", "currency", "rating", "duration", "count":
		return "number"
	case "checkbox":
		return "bool"
	case "singleLineText", "multilineText", "richText", "email", "url", "phoneNumber", "singleSelect", "date", "dateTime", "createdTime", "lastModifiedTime", "autoNumber", "barcode", "button":
		return "text"
	case "multipleSelects", "multipleRecordLinks", "multipleAttachments", "lookup", "multipleLookupValues":
		return "json"
	default:
		return "json"
	}
}

func importTableName(baseName, tableName, baseID, tableID string) string {
	name := "airtable_" + importIdentifier(baseName) + "_" + importIdentifier(tableName)
	if len(name) <= 64 {
		return name
	}
	suffix := "_" + shortID(baseID) + "_" + shortID(tableID)
	prefixMax := 64 - len(suffix)
	if prefixMax < 1 {
		return "airtable_" + shortID(baseID) + "_" + shortID(tableID)
	}
	return strings.TrimRight(name[:prefixMax], "_") + suffix
}

var nonIdentChars = regexp.MustCompile(`[^a-z0-9_]+`)

func importIdentifier(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonIdentChars.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if s == "" {
		s = "value"
	}
	if s[0] < 'a' || s[0] > 'z' {
		s = "v_" + s
	}
	if len(s) > 64 {
		s = strings.TrimRight(s[:64], "_")
	}
	return s
}

func uniqueImportIdentifier(base string, used map[string]bool) string {
	if !used[base] {
		return base
	}
	for i := 2; ; i++ {
		suffix := "_" + strconv.Itoa(i)
		max := 64 - len(suffix)
		candidate := strings.TrimRight(base[:min(len(base), max)], "_") + suffix
		if !used[candidate] {
			return candidate
		}
	}
}

func shortID(s string) string {
	if len(s) <= 8 {
		return importIdentifier(s)
	}
	return importIdentifier(s[len(s)-8:])
}

func mapAtAny(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func arrayAt(v any, path string) []any {
	cur := v
	for _, part := range strings.Split(path, ".") {
		if part == "" {
			continue
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	if arr, ok := cur.([]any); ok {
		return arr
	}
	return nil
}

func stringAt(v any, path string) string {
	cur := v
	for _, part := range strings.Split(path, ".") {
		if part == "" {
			continue
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[part]
	}
	return stringValue(cur)
}

func stringPath(m map[string]any, parts ...string) string {
	var cur any = m
	for _, part := range parts {
		next, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = next[part]
	}
	return stringValue(cur)
}

func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func intNumber(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}
