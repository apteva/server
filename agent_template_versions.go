package main

// agent_template_versions.go — versioned agent definitions.
//
// A template's behavior (instructions, mode, background memory and
// required apps) is saved as immutable versions, the pattern Processes
// uses for procedures. An agent created from a template keeps a link to
// it and either follows the template's latest version or stays pinned.
//
// A new version replaces an agent's instructions and mode only while they
// still match what the agent last received from the template. Edits made
// by an operator, by an app or by the agent's own learning are never
// overwritten unless an operator explicitly asks for it. Background memory
// and required apps are not applied to existing agents: the first needs a
// restart and the second an install, so they are reported instead.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	errAgentTemplateVersionConflict = errors.New("template changed since the version you started from; reload it and retry")
	errAgentNotFromTemplate         = errors.New("agent is not linked to a template")
)

// Outcomes of moving one agent to a template version.
const (
	agentTemplateApplied        = "applied"
	agentTemplateChangedLocally = "changed_locally"
	agentTemplateUpToDate       = "up_to_date"
)

// agentTemplateBody is the versioned part of a template. Name, icon,
// description, highlights and order are catalog details edited in place.
type agentTemplateBody struct {
	Directive       string        `json:"directive"`
	Mode            string        `json:"mode"`
	Unconscious     bool          `json:"unconscious"`
	RecommendedApps []string      `json:"recommended_apps"`
	Requirements    []Requirement `json:"requirements"`
}

func agentTemplateBodyOf(t AgentTemplate) agentTemplateBody {
	b := agentTemplateBody{
		Directive: t.Directive, Mode: t.Mode, Unconscious: t.Unconscious,
		RecommendedApps: t.RecommendedApps, Requirements: t.Requirements,
	}
	if b.RecommendedApps == nil {
		b.RecommendedApps = []string{}
	}
	if b.Requirements == nil {
		b.Requirements = []Requirement{}
	}
	return b
}

func (b agentTemplateBody) encode() string {
	data, _ := json.Marshal(b)
	return string(data)
}

func (b agentTemplateBody) requiredApps() []string {
	var out []string
	for _, req := range b.Requirements {
		if req.Kind == "app" && req.Required && strings.TrimSpace(req.Slug) != "" {
			out = append(out, req.Slug)
		}
	}
	return out
}

// agentTemplateDirectiveETag identifies instructions independent of
// compiled behavior controls and surrounding whitespace.
func agentTemplateDirectiveETag(directive string) string {
	return directiveETag(strings.TrimSpace(withoutAgentControls(directive)))
}

// ─── Schema ────────────────────────────────────────────────────────

// migrateAgentTemplateVersions creates the version and link tables, then
// records a version for each built-in template whose content changed.
func (s *Store) migrateAgentTemplateVersions() error {
	tx, err := s.dbBegin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var done int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM server_schema_migrations WHERE version=9`).Scan(&done); err != nil {
		return err
	}
	if done == 0 {
		for _, q := range []string{
			// Immutable template versions. source mirrors the template's
			// source so built-in versions recorded at boot are recognizable.
			`CREATE TABLE IF NOT EXISTS agent_template_versions (
				template_id TEXT    NOT NULL,
				version     INTEGER NOT NULL,
				source      TEXT    NOT NULL DEFAULT 'user',
				body_json   TEXT    NOT NULL,
				created_by  INTEGER NOT NULL DEFAULT 0,
				created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
				PRIMARY KEY (template_id, version)
			)`,
			// One link per agent. version is the template version the agent
			// last received; checked_version the newest one a rollout has
			// evaluated. The base values are what that version set, so local
			// changes can be told apart from template ones.
			`CREATE TABLE IF NOT EXISTS agent_template_links (
				agent_id            INTEGER PRIMARY KEY REFERENCES agents(id) ON DELETE CASCADE,
				template_id         TEXT    NOT NULL,
				version             INTEGER NOT NULL,
				checked_version     INTEGER NOT NULL,
				follow_latest       INTEGER NOT NULL DEFAULT 0,
				base_directive_etag TEXT    NOT NULL,
				base_mode           TEXT    NOT NULL,
				last_error          TEXT    NOT NULL DEFAULT '',
				created_at          DATETIME DEFAULT CURRENT_TIMESTAMP,
				updated_at          DATETIME DEFAULT CURRENT_TIMESTAMP
			)`,
			`CREATE INDEX IF NOT EXISTS idx_agent_template_links_template ON agent_template_links(template_id)`,
			`INSERT INTO server_schema_migrations(version) VALUES(9)`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return fmt.Errorf("agent template versions migration: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.recordBuiltinTemplateVersions()
}

// recordBuiltinTemplateVersions adds a version for each built-in template
// whose content changed since its last recorded version. Built-in rows are
// refreshed from the presets at boot, so a release that changes a preset
// produces a new version here.
func (s *Store) recordBuiltinTemplateVersions() error {
	rows, err := s.dbQuery(agentTemplateSelect + ` WHERE t.source='builtin'`)
	if err != nil {
		return err
	}
	templates, err := collectSQLRows(rows, scanAgentTemplate)
	if err != nil {
		return err
	}
	tx, err := s.dbBegin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range templates {
		if _, _, err := recordAgentTemplateVersion(tx, t, 0); err != nil {
			return fmt.Errorf("record built-in template version %s: %w", t.ID, err)
		}
	}
	return tx.Commit()
}

// recordAgentTemplateVersion stores t's body as a new version unless it
// matches the latest one. A template without versions gets version 1.
func recordAgentTemplateVersion(tx *sql.Tx, t AgentTemplate, createdBy int64) (int, bool, error) {
	body := agentTemplateBodyOf(t).encode()
	var current int
	var currentBody string
	err := tx.QueryRow(`SELECT version, body_json FROM agent_template_versions WHERE template_id=? ORDER BY version DESC LIMIT 1`, t.ID).Scan(&current, &currentBody)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	if err == nil && currentBody == body {
		return current, false, nil
	}
	source := t.Source
	if source == "" {
		source = "user"
	}
	next := current + 1
	if _, err := tx.Exec(`INSERT INTO agent_template_versions(template_id,version,source,body_json,created_by) VALUES(?,?,?,?,?)`,
		t.ID, next, source, body, createdBy); err != nil {
		return 0, false, err
	}
	return next, true, nil
}

// ensureAgentTemplateVersioned gives a template saved before versioning
// its first version and returns the latest version.
func (s *Store) ensureAgentTemplateVersioned(id string) (int, error) {
	tx, err := s.dbBegin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	t, err := scanAgentTemplate(tx.QueryRow(agentTemplateSelect+` WHERE t.id=?`, id))
	if err != nil {
		return 0, err
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM agent_template_versions WHERE template_id=?`, id).Scan(&count); err != nil {
		return 0, err
	}
	if count > 0 {
		var latest int
		err := tx.QueryRow(`SELECT MAX(version) FROM agent_template_versions WHERE template_id=?`, id).Scan(&latest)
		return latest, err
	}
	version, _, err := recordAgentTemplateVersion(tx, t, 0)
	if err != nil {
		return 0, err
	}
	return version, tx.Commit()
}

func (s *Store) latestAgentTemplateVersion(id string) (int, error) {
	var latest sql.NullInt64
	if err := s.dbQueryRow(`SELECT MAX(version) FROM agent_template_versions WHERE template_id=?`, id).Scan(&latest); err != nil {
		return 0, err
	}
	return int(latest.Int64), nil
}

func (s *Store) agentTemplateVersionBody(id string, version int) (agentTemplateBody, error) {
	var raw string
	var body agentTemplateBody
	if err := s.dbQueryRow(`SELECT body_json FROM agent_template_versions WHERE template_id=? AND version=?`, id, version).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return body, fmt.Errorf("template %s has no version %d", id, version)
		}
		return body, err
	}
	err := json.Unmarshal([]byte(raw), &body)
	return body, err
}

// AgentTemplateVersion describes one saved version of a template.
type AgentTemplateVersion struct {
	Version   int       `json:"version"`
	CreatedBy int64     `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) listAgentTemplateVersions(id string) ([]AgentTemplateVersion, error) {
	rows, err := s.dbQuery(`SELECT version, created_by, created_at FROM agent_template_versions WHERE template_id=? ORDER BY version DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentTemplateVersion{}
	for rows.Next() {
		var v AgentTemplateVersion
		var createdAt string
		if err := rows.Scan(&v.Version, &v.CreatedBy, &createdAt); err != nil {
			return nil, err
		}
		v.CreatedAt, _ = parseTime(createdAt)
		out = append(out, v)
	}
	return out, rows.Err()
}

// ─── Links ─────────────────────────────────────────────────────────

type agentTemplateLink struct {
	AgentID           int64
	TemplateID        string
	Version           int
	CheckedVersion    int
	FollowLatest      bool
	BaseDirectiveETag string
	BaseMode          string
	LastError         string
}

func (s *Store) agentTemplateLink(agentID int64) (*agentTemplateLink, error) {
	var l agentTemplateLink
	var follow int
	err := s.dbQueryRow(`SELECT agent_id, template_id, version, checked_version, follow_latest, base_directive_etag, base_mode, last_error
		FROM agent_template_links WHERE agent_id=?`, agentID).Scan(
		&l.AgentID, &l.TemplateID, &l.Version, &l.CheckedVersion, &follow, &l.BaseDirectiveETag, &l.BaseMode, &l.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	l.FollowLatest = follow == 1
	return &l, nil
}

// linkAgentToTemplate records that agentID received version of templateID,
// with baseDirective and baseMode as the values that version set.
func (s *Store) linkAgentToTemplate(agentID int64, templateID string, version int, follow bool, baseDirective, baseMode string) error {
	_, err := s.dbExec(`INSERT INTO agent_template_links
			(agent_id, template_id, version, checked_version, follow_latest, base_directive_etag, base_mode)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET template_id=excluded.template_id, version=excluded.version,
			checked_version=excluded.checked_version, follow_latest=excluded.follow_latest,
			base_directive_etag=excluded.base_directive_etag, base_mode=excluded.base_mode,
			last_error='', updated_at=CURRENT_TIMESTAMP`,
		agentID, templateID, version, version, boolToInt(follow), agentTemplateDirectiveETag(baseDirective), agentMode(baseMode))
	return err
}

// agentModeAllowedFor applies the access policy's autonomy limit, the same
// way agent creation and configuration do.
func (s *Server) agentModeAllowedFor(userID int64, mode string) string {
	mode = agentMode(mode)
	if mode == "autonomous" && s.store.GetPlatformRole(userID) != PlatformAdmin {
		if policy, err := s.loadAccessPolicy(); err != nil || !policy.Capabilities.AutonomousScheduling {
			return "cautious"
		}
	}
	return mode
}

// agentTemplateSelection is a template version chosen for a new agent.
type agentTemplateSelection struct {
	Template *AgentTemplate
	Version  int
	Body     agentTemplateBody
	Follow   bool
}

// resolveAgentTemplateSelection picks the version a new agent starts from.
// Agents from the caller's own templates follow the latest version by
// default; agents from built-in templates stay pinned, so a server release
// never changes running agents unless an operator opted in.
func (s *Server) resolveAgentTemplateSelection(userID int64, templateID string, version *int, follow *bool) (*agentTemplateSelection, error) {
	t, err := s.store.GetAgentTemplate(userID, templateID)
	if err != nil {
		return nil, fmt.Errorf("template %q not found", templateID)
	}
	latest, err := s.store.ensureAgentTemplateVersioned(t.ID)
	if err != nil {
		return nil, fmt.Errorf("template %q versions: %w", templateID, err)
	}
	sel := &agentTemplateSelection{Template: t, Version: latest, Follow: t.Source == "user"}
	if version != nil {
		if *version < 1 || *version > latest {
			return nil, fmt.Errorf("template_version must be between 1 and %d", latest)
		}
		sel.Version = *version
		sel.Follow = false
	}
	if follow != nil {
		if *follow && sel.Version != latest {
			return nil, fmt.Errorf("follow_template needs the latest template version (%d)", latest)
		}
		sel.Follow = *follow
	}
	sel.Body, err = s.store.agentTemplateVersionBody(t.ID, sel.Version)
	if err != nil {
		return nil, err
	}
	return sel, nil
}

// ─── Applying versions ─────────────────────────────────────────────

// agentLiveDirective returns the instructions the agent runs with now:
// Core's copy, which the agent's own learning may have changed, unless the
// server holds a newer edit that has not reached Core yet.
func (s *Server) agentLiveDirective(ctx context.Context, inst *Agent, port int) (string, error) {
	cfg, err := s.behaviorConfig(ctx, inst, port)
	if err != nil {
		return "", err
	}
	state, err := s.store.agentBehaviorState(inst.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	runtime, ok := cfg["directive"].(string)
	if !ok || state.Revision > state.MainRevision {
		return inst.Directive, nil
	}
	return withoutAgentControls(runtime), nil
}

func agentTemplateHasLocalChanges(link *agentTemplateLink, directive, mode string) bool {
	return agentTemplateDirectiveETag(directive) != link.BaseDirectiveETag || agentMode(mode) != link.BaseMode
}

// agentTemplateApplyOptions qualify one move to a template version.
type agentTemplateApplyOptions struct {
	// Overwrite replaces instructions and mode even when they changed
	// since the agent's last template version.
	Overwrite bool
	// Rollout skips the agent unless it still follows the template and has
	// not evaluated this version yet: an operator may have pinned it, or
	// another sweep handled it, since the sweep listed it.
	Rollout bool
	Actor   int64
}

// applyAgentTemplateVersion moves one linked agent to a template version.
// Unless opts.Overwrite is set, it replaces instructions and mode only when
// both still match what the agent last received from the template. Changes
// reach a running agent live through the behavior reconciliation, without a
// restart.
func (s *Server) applyAgentTemplateVersion(ctx context.Context, agentID int64, version int, opts agentTemplateApplyOptions) (string, error) {
	unlock := s.lockAgentConfig(agentID)
	defer unlock()
	inst, err := s.store.GetAgentByID(agentID)
	if err != nil {
		return "", err
	}
	link, err := s.store.agentTemplateLink(agentID)
	if err != nil {
		return "", err
	}
	if link == nil {
		return "", errAgentNotFromTemplate
	}
	if opts.Rollout && (!link.FollowLatest || link.CheckedVersion >= version) {
		return agentTemplateUpToDate, nil
	}
	body, err := s.store.agentTemplateVersionBody(link.TemplateID, version)
	if err != nil {
		return "", err
	}
	port := s.agents.GetPort(agentID)
	if port == 0 && inst.Status == "running" {
		return "", errors.New("agent runtime is starting or stopping; retry shortly")
	}
	current, err := s.agentLiveDirective(ctx, inst, port)
	if err != nil {
		return "", err
	}
	if agentTemplateHasLocalChanges(link, current, inst.Mode) && !opts.Overwrite {
		_, err = s.store.dbExec(`UPDATE agent_template_links SET checked_version=MAX(checked_version,?), last_error='', updated_at=CURRENT_TIMESTAMP WHERE agent_id=?`, version, agentID)
		return agentTemplateChangedLocally, err
	}
	nextDirective := withoutAgentControls(body.Directive)
	nextMode := s.agentModeAllowedFor(inst.UserID, body.Mode)
	if link.Version == version && agentTemplateDirectiveETag(current) == agentTemplateDirectiveETag(nextDirective) && agentMode(inst.Mode) == nextMode {
		_, err = s.store.dbExec(`UPDATE agent_template_links SET checked_version=MAX(checked_version,?), last_error='', updated_at=CURRENT_TIMESTAMP WHERE agent_id=?`, version, agentID)
		return agentTemplateUpToDate, err
	}

	var templateName string
	_ = s.store.dbQueryRow(`SELECT name FROM agent_templates WHERE id=?`, link.TemplateID).Scan(&templateName)
	if templateName == "" {
		templateName = link.TemplateID
	}
	reason := fmt.Sprintf("Template %q version %d", templateName, version)
	var cfg map[string]any
	if json.Unmarshal([]byte(inst.Config), &cfg) != nil || cfg == nil {
		cfg = map[string]any{}
	}
	// The stored config keeps only the authored directive, as on other edits.
	cfg["directive"] = nextDirective
	configJSON, _ := json.Marshal(cfg)

	tx, err := s.store.dbBegin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE agents SET directive=?, mode=?, config=? WHERE id=?`, nextDirective, nextMode, withoutCoreMode(string(configJSON)), agentID); err != nil {
		return "", err
	}
	// The server's edit must win over Core's copy even when the stored row
	// already held these values (Core's learning changed only its own copy).
	if _, err := tx.Exec(`UPDATE agent_behavior_state SET revision=revision+1, last_error='' WHERE agent_id=?`, agentID); err != nil {
		return "", err
	}
	history := func(field string, before, after any) error {
		beforeJSON, _ := json.Marshal(before)
		afterJSON, _ := json.Marshal(after)
		_, err := tx.Exec(`INSERT INTO agent_change_history (agent_id, field, before_json, after_json, reason, source_app_install_id, applied_by_user_id) VALUES (?, ?, ?, ?, ?, 0, ?)`,
			agentID, field, string(beforeJSON), string(afterJSON), reason, opts.Actor)
		return err
	}
	if current != nextDirective {
		if err := history("directive", current, nextDirective); err != nil {
			return "", err
		}
	}
	if agentMode(inst.Mode) != nextMode {
		if err := history("mode", agentMode(inst.Mode), nextMode); err != nil {
			return "", err
		}
	}
	if _, err := tx.Exec(`UPDATE agent_template_links SET version=?, checked_version=MAX(checked_version,?),
			base_directive_etag=?, base_mode=?, last_error='', updated_at=CURRENT_TIMESTAMP WHERE agent_id=?`,
		version, version, agentTemplateDirectiveETag(nextDirective), nextMode, agentID); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	log.Printf("[TEMPLATE] agent=%d template=%s version=%d applied", agentID, link.TemplateID, version)
	updated, err := s.store.GetAgentByID(agentID)
	if err != nil {
		return agentTemplateApplied, nil
	}
	// A failed push stays pending in the behavior state and is retried by
	// the behavior reconciler; the template version itself is saved.
	if err := s.reconcileAgentBehavior(ctx, updated, port); err != nil {
		log.Printf("[TEMPLATE] agent=%d template=%s version=%d: behavior pending: %v", agentID, link.TemplateID, version, err)
	}
	return agentTemplateApplied, nil
}

// ─── Rollout ───────────────────────────────────────────────────────

var agentTemplateRollout struct {
	mu      sync.Mutex
	running bool
	again   bool
}

// requestAgentTemplateRollout starts a rollout in the background, or makes
// the one already running sweep again once it finishes.
func (s *Server) requestAgentTemplateRollout() {
	go s.rolloutAgentTemplates(context.Background())
}

func (s *Server) rolloutAgentTemplates(ctx context.Context) {
	r := &agentTemplateRollout
	r.mu.Lock()
	if r.running {
		r.again = true
		r.mu.Unlock()
		return
	}
	r.running = true
	r.mu.Unlock()
	for {
		s.rolloutAgentTemplatesOnce(ctx)
		r.mu.Lock()
		if !r.again || ctx.Err() != nil {
			r.running, r.again = false, false
			r.mu.Unlock()
			return
		}
		r.again = false
		r.mu.Unlock()
	}
}

// rolloutAgentTemplatesOnce moves every following agent that has not yet
// evaluated its template's latest version. Failures keep the link behind
// and are retried on the next sweep.
func (s *Server) rolloutAgentTemplatesOnce(ctx context.Context) {
	rows, err := s.store.dbQuery(`SELECT l.agent_id, MAX(v.version)
		FROM agent_template_links l JOIN agent_template_versions v ON v.template_id = l.template_id
		WHERE l.follow_latest = 1
		GROUP BY l.agent_id
		HAVING MAX(v.version) > MAX(l.checked_version)
		ORDER BY l.agent_id LIMIT 500`)
	if err != nil {
		log.Printf("[TEMPLATE] rollout scan: %v", err)
		return
	}
	type pending struct {
		agentID int64
		version int
	}
	var due []pending
	for rows.Next() {
		var p pending
		if rows.Scan(&p.agentID, &p.version) == nil {
			due = append(due, p)
		}
	}
	rows.Close()
	for _, p := range due {
		if ctx.Err() != nil {
			return
		}
		outcome, err := s.applyAgentTemplateVersion(ctx, p.agentID, p.version, agentTemplateApplyOptions{Rollout: true})
		if err != nil {
			log.Printf("[TEMPLATE] agent=%d version=%d: %v", p.agentID, p.version, err)
			_, _ = s.store.dbExec(`UPDATE agent_template_links SET last_error=?, updated_at=CURRENT_TIMESTAMP WHERE agent_id=?`, err.Error(), p.agentID)
			continue
		}
		if outcome == agentTemplateChangedLocally {
			log.Printf("[TEMPLATE] agent=%d version=%d not applied: instructions changed locally", p.agentID, p.version)
		}
	}
}

// runAgentTemplateRollouts sweeps at boot and then every minute, which
// also retries agents whose last update failed.
func (s *Server) runAgentTemplateRollouts(ctx context.Context) {
	s.rolloutAgentTemplates(ctx)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.rolloutAgentTemplates(ctx)
		}
	}
}

// ─── Status ────────────────────────────────────────────────────────

type agentTemplateStatus struct {
	Linked          bool     `json:"linked"`
	TemplateID      string   `json:"template_id,omitempty"`
	TemplateName    string   `json:"template_name,omitempty"`
	TemplateSource  string   `json:"template_source,omitempty"`
	Version         int      `json:"version,omitempty"`
	LatestVersion   int      `json:"latest_version,omitempty"`
	FollowLatest    bool     `json:"follow_latest"`
	ChangedLocally  bool     `json:"changed_locally"`
	UpdateAvailable bool     `json:"update_available"`
	MissingApps     []string `json:"missing_apps,omitempty"`
	LastError       string   `json:"last_error,omitempty"`
	Outcome         string   `json:"outcome,omitempty"`
}

func (s *Store) agentBoundAppNames(agentID int64) (map[string]bool, error) {
	rows, err := s.dbQuery(`SELECT DISTINCT a.name FROM app_agent_bindings b
		JOIN app_installs i ON i.id = b.install_id JOIN apps a ON a.id = i.app_id
		WHERE b.agent_id = ? AND b.enabled = 1`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[normalizeAppName(name)] = true
	}
	return out, rows.Err()
}

// agentTemplateStatusFor reports an agent's template link. With live set,
// local changes are judged against Core's current instructions.
func (s *Server) agentTemplateStatusFor(ctx context.Context, inst *Agent, live bool) (agentTemplateStatus, error) {
	link, err := s.store.agentTemplateLink(inst.ID)
	if err != nil || link == nil {
		return agentTemplateStatus{}, err
	}
	st := agentTemplateStatus{
		Linked: true, TemplateID: link.TemplateID, Version: link.Version,
		FollowLatest: link.FollowLatest, LastError: link.LastError,
	}
	_ = s.store.dbQueryRow(`SELECT name, source FROM agent_templates WHERE id=?`, link.TemplateID).Scan(&st.TemplateName, &st.TemplateSource)
	if st.LatestVersion, err = s.store.latestAgentTemplateVersion(link.TemplateID); err != nil {
		return st, err
	}
	st.UpdateAvailable = st.LatestVersion > link.Version
	directive := inst.Directive
	if live {
		port := s.agents.GetPort(inst.ID)
		if port > 0 || inst.Status != "running" {
			if current, err := s.agentLiveDirective(ctx, inst, port); err == nil {
				directive = current
			}
		}
	}
	st.ChangedLocally = agentTemplateHasLocalChanges(link, directive, inst.Mode)
	if st.LatestVersion > 0 {
		if body, err := s.store.agentTemplateVersionBody(link.TemplateID, st.LatestVersion); err == nil {
			if bound, err := s.store.agentBoundAppNames(inst.ID); err == nil {
				for _, slug := range body.requiredApps() {
					if !bound[normalizeAppName(slug)] {
						st.MissingApps = append(st.MissingApps, slug)
					}
				}
			}
		}
	}
	return st, nil
}

// agentTemplateFollower is one agent linked to a template, as listed on
// the template. Local changes are judged from the stored instructions.
type agentTemplateFollower struct {
	AgentID        int64  `json:"agent_id"`
	Name           string `json:"name"`
	ProjectID      string `json:"project_id,omitempty"`
	Version        int    `json:"version"`
	FollowLatest   bool   `json:"follow_latest"`
	ChangedLocally bool   `json:"changed_locally"`
	LastError      string `json:"last_error,omitempty"`
}

// agentTemplateFollowers lists linked agents the user can see: their own
// and those in projects they belong to.
func (s *Store) agentTemplateFollowers(userID int64, templateID string) ([]agentTemplateFollower, error) {
	rows, err := s.dbQuery(`SELECT a.id, a.name, COALESCE(a.project_id,''), a.directive, COALESCE(a.mode,''),
			l.version, l.follow_latest, l.base_directive_etag, l.base_mode, l.last_error
		FROM agent_template_links l JOIN agents a ON a.id = l.agent_id
		WHERE l.template_id = ?
		  AND (a.user_id = ? OR (COALESCE(a.project_id,'') != '' AND a.project_id IN (SELECT project_id FROM project_members WHERE user_id = ?)))
		ORDER BY a.id`, templateID, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []agentTemplateFollower{}
	for rows.Next() {
		var f agentTemplateFollower
		var directive, mode string
		var follow int
		link := agentTemplateLink{}
		if err := rows.Scan(&f.AgentID, &f.Name, &f.ProjectID, &directive, &mode, &f.Version, &follow, &link.BaseDirectiveETag, &link.BaseMode, &f.LastError); err != nil {
			return nil, err
		}
		f.FollowLatest = follow == 1
		f.ChangedLocally = agentTemplateHasLocalChanges(&link, directive, mode)
		out = append(out, f)
	}
	return out, rows.Err()
}

// ─── HTTP: /agents/:id/template ────────────────────────────────────

// handleAgentTemplateLink serves an agent's template link.
//
//	GET    — status: version, latest version, follow mode, local changes.
//	PUT    — {follow_latest?, version?, overwrite_local_changes?}: follow the
//	         latest version, pin to one, or move to one now.
//	DELETE — unlink; the agent keeps its current settings.
func (s *Server) handleAgentTemplateLink(w http.ResponseWriter, r *http.Request, agentID int64) {
	inst, err := s.store.GetAgentByIDContext(r.Context(), agentID)
	if err != nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		st, err := s.agentTemplateStatusFor(r.Context(), inst, true)
		if err != nil {
			http.Error(w, "template status: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, st)
	case http.MethodPut:
		var body struct {
			FollowLatest          *bool `json:"follow_latest"`
			Version               *int  `json:"version"`
			OverwriteLocalChanges bool  `json:"overwrite_local_changes"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		link, err := s.store.agentTemplateLink(agentID)
		if err != nil {
			http.Error(w, "template link: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if link == nil {
			http.Error(w, errAgentNotFromTemplate.Error(), http.StatusNotFound)
			return
		}
		latest, err := s.store.latestAgentTemplateVersion(link.TemplateID)
		if err != nil || latest == 0 {
			http.Error(w, "template versions unavailable", http.StatusConflict)
			return
		}
		if body.Version != nil && (*body.Version < 1 || *body.Version > latest) {
			http.Error(w, fmt.Sprintf("version must be between 1 and %d", latest), http.StatusBadRequest)
			return
		}
		follow := link.FollowLatest
		if body.FollowLatest != nil {
			follow = *body.FollowLatest
		} else if body.Version != nil {
			follow = false // choosing a version pins the agent to it
		}
		if follow && body.Version != nil && *body.Version != latest {
			http.Error(w, fmt.Sprintf("following needs the latest version (%d)", latest), http.StatusBadRequest)
			return
		}
		if follow != link.FollowLatest {
			if _, err := s.store.dbExec(`UPDATE agent_template_links SET follow_latest=?, updated_at=CURRENT_TIMESTAMP WHERE agent_id=?`, boolToInt(follow), agentID); err != nil {
				http.Error(w, "update template link: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		target := 0
		if body.Version != nil {
			target = *body.Version
		} else if follow {
			target = latest
		}
		outcome := ""
		if target > 0 {
			outcome, err = s.applyAgentTemplateVersion(r.Context(), agentID, target, agentTemplateApplyOptions{Overwrite: body.OverwriteLocalChanges, Actor: getUserID(r)})
			if err != nil {
				http.Error(w, "apply template version: "+err.Error(), http.StatusConflict)
				return
			}
		}
		inst, _ = s.store.GetAgentByIDContext(r.Context(), agentID)
		st, err := s.agentTemplateStatusFor(r.Context(), inst, true)
		if err != nil {
			http.Error(w, "template status: "+err.Error(), http.StatusInternalServerError)
			return
		}
		st.Outcome = outcome
		if outcome == agentTemplateChangedLocally {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":  "the agent's instructions or mode changed since its last template version; send overwrite_local_changes to replace them",
				"status": st,
			})
			return
		}
		writeJSON(w, st)
	case http.MethodDelete:
		if _, err := s.store.dbExec(`DELETE FROM agent_template_links WHERE agent_id=?`, agentID); err != nil {
			http.Error(w, "unlink template: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, agentTemplateStatus{})
	default:
		http.Error(w, "GET, PUT, or DELETE", http.StatusMethodNotAllowed)
	}
}

// handleSaveAgentAsTemplate (POST /agents/:id/template/save) creates one of
// the caller's own templates from the agent's current instructions, mode,
// background memory and attached apps, and links the agent to it.
func (s *Server) handleSaveAgentAsTemplate(w http.ResponseWriter, r *http.Request, agentID int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	userID := getUserID(r)
	inst, err := s.store.GetAgentByIDContext(r.Context(), agentID)
	if err != nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	var body struct {
		Name        string   `json:"name"`
		Icon        string   `json:"icon"`
		Description string   `json:"description"`
		Highlights  []string `json:"highlights"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		body.Name = inst.Name
	}
	unlock := s.lockAgentConfig(agentID)
	defer unlock()
	port := s.agents.GetPort(agentID)
	if port == 0 && inst.Status == "running" {
		http.Error(w, "agent runtime is starting or stopping; retry shortly", http.StatusServiceUnavailable)
		return
	}
	directive, err := s.agentLiveDirective(r.Context(), inst, port)
	if err != nil {
		http.Error(w, "read agent instructions: "+err.Error(), http.StatusBadGateway)
		return
	}
	bound, err := s.store.agentBoundAppNames(agentID)
	if err != nil {
		http.Error(w, "read agent apps: "+err.Error(), http.StatusInternalServerError)
		return
	}
	apps := make([]string, 0, len(bound))
	for name := range bound {
		apps = append(apps, name)
	}
	sort.Strings(apps)
	requirements := make([]Requirement, 0, len(apps))
	for _, slug := range apps {
		requirements = append(requirements, Requirement{Kind: "app", Slug: slug, Required: true, Reason: "Attached to " + inst.Name + "."})
	}
	t := AgentTemplate{
		Name: strings.TrimSpace(body.Name), Icon: body.Icon, Description: strings.TrimSpace(body.Description),
		Highlights: body.Highlights, Directive: directive, Mode: agentMode(inst.Mode),
		Unconscious: s.backgroundMemoryEnabled(inst), RecommendedApps: apps, Requirements: requirements,
		SortOrder: 100,
	}
	if t.Icon == "" {
		t.Icon = inst.Icon
	}
	if err := validateWritableAgentTemplate(t); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	created, err := s.store.CreateAgentTemplate(userID, t)
	if err != nil {
		http.Error(w, "create template: "+err.Error(), http.StatusConflict)
		return
	}
	if err := s.store.linkAgentToTemplate(agentID, created.ID, created.Version, true, directive, inst.Mode); err != nil {
		http.Error(w, "link agent to template: "+err.Error(), http.StatusInternalServerError)
		return
	}
	st, _ := s.agentTemplateStatusFor(r.Context(), inst, false)
	s.resolveTemplateLogos(created)
	writeJSON(w, map[string]any{"template": created, "status": st})
}
