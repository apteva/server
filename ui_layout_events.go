package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

// uiLayoutChange describes one saved change to a user's UI layout. It carries
// identifiers only, never layout content.
type uiLayoutChange struct {
	UserID    int64
	ProjectID string // empty for All-projects and whole-layout saves
	Scope     string // "project", "global" or "layout" (whole document replaced)
	PageID    string // set when the change belongs to one custom page
	Surface   string // set when one surface was written
	Change    string // "created", "updated" or "deleted"
	Revision  int64
}

const (
	uiLayoutEventApp    = "pages"
	uiLayoutEventTopic  = "page.updated"
	uiPageSurfacePrefix = "page."
)

// uiLayoutSnapshot is the canonical form of a layout document, taken before a
// writer mutates it so the writer can tell a real change from a repeat.
func uiLayoutSnapshot(document any) []byte {
	raw, _ := json.Marshal(document)
	return raw
}

// uiLayoutSaved reports a committed layout write to the server. A write that
// left the document identical (an idempotent retry) reports nothing.
func (s *Store) uiLayoutSaved(before, after []byte, change uiLayoutChange) {
	if s.uiLayoutChanged == nil || bytes.Equal(before, after) {
		return
	}
	if change.PageID == "" && strings.HasPrefix(change.Surface, uiPageSurfacePrefix) {
		change.PageID = strings.TrimPrefix(change.Surface, uiPageSurfacePrefix)
	}
	s.uiLayoutChanged(change)
}

// publishUILayoutChange puts page.updated on the app event bus. Project
// changes go to that project's lane. All-projects and whole-layout changes
// have no project, so they reach only the cross-project lanes.
func (s *Server) publishUILayoutChange(change uiLayoutChange) {
	if s.appBus == nil {
		return
	}
	data, _ := json.Marshal(map[string]any{
		"user_id":    change.UserID,
		"project_id": change.ProjectID,
		"scope":      change.Scope,
		"page_id":    change.PageID,
		"surface":    change.Surface,
		"change":     change.Change,
		"revision":   change.Revision,
	})
	s.appBus.Publish(uiLayoutEventApp, change.ProjectID, 0, uiLayoutEventTopic, data)
}
