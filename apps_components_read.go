package main

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
)

func (s *Server) readComponentDefinitions(ctx context.Context, ids []int64) (map[int64]appComponentDeclarations, error) {
	out := map[int64]appComponentDeclarations{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.store.dbQueryContext(ctx, `SELECT d.install_id,d.name,d.declaration_json FROM app_component_declarations d JOIN app_installs i ON i.id=d.install_id WHERE d.parent_version=i.version AND d.install_id IN (`+strings.TrimRight(strings.Repeat("?,", len(ids)), ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name, raw string
		if err := rows.Scan(&id, &name, &raw); err != nil {
			return nil, err
		}
		var def appComponentDeclaration
		if err := json.Unmarshal([]byte(raw), &def); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = appComponentDeclarations{}
		}
		out[id][name] = def
	}
	return out, rows.Err()
}
func (s *Server) readComponentChildren(ctx context.Context, ids []int64) (map[int64]map[string]appComponentSummary, error) {
	out := map[int64]map[string]appComponentSummary{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.store.dbQueryContext(ctx, `SELECT owner_install_id,component_name,id,status,version FROM app_installs WHERE owner_install_id IN (`+strings.TrimRight(strings.Repeat("?,", len(ids)), ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var parent int64
		var c appComponentSummary
		if err := rows.Scan(&parent, &c.Name, &c.InstallID, &c.Status, &c.Version); err != nil {
			return nil, err
		}
		if out[parent] == nil {
			out[parent] = map[string]appComponentSummary{}
		}
		out[parent][c.Name] = c
	}
	return out, rows.Err()
}
func (s *Server) componentSummariesBatch(ctx context.Context, parents []int64) (map[int64][]appComponentSummary, error) {
	ctx, cancel := s.store.databaseContext(ctx)
	defer cancel()
	out := map[int64][]appComponentSummary{}
	for start := 0; start < len(parents); start += 200 {
		end := min(start+200, len(parents))
		ids := parents[start:end]
		defs, err := s.readComponentDefinitions(ctx, ids)
		if err != nil {
			return nil, err
		}
		children, err := s.readComponentChildren(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if children[id] == nil {
				children[id] = map[string]appComponentSummary{}
			}
			for name, def := range defs[id] {
				c := children[id][name]
				c.Name = name
				c.Protocol = def.Protocol
				c.Required = def.Required
				c.Permissions = def.Manifest.Requires.Permissions
				if c.InstallID == 0 {
					c.Status = "not_configured"
					c.Version = def.Manifest.Version
				}
				children[id][name] = c
			}
			items := []appComponentSummary{}
			for _, c := range children[id] {
				if _, ok := defs[id][c.Name]; !ok {
					c.Status = "declaration_removed"
				}
				if c.InstallID != 0 {
					v := s.appRuntimeManager().deploymentSummaryContext(ctx, c.InstallID)
					c.Deployment = &v
				}
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				items = append(items, c)
			}
			sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
			out[id] = items
		}
	}
	return out, nil
}
