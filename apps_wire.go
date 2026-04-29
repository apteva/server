package main

import (
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	sdk "github.com/apteva/app-sdk"
	"github.com/apteva/server/apps/channelchat"
	"github.com/apteva/server/apps/framework"
)

// Bridges between apteva-server internals and the Apteva Apps
// framework. The framework is designed to know nothing about
// Server/Store/InstanceManager — this file is where we translate.

// startApps constructs the framework Registry, loads every built-in
// app, runs their migrations + OnMount, and mounts their HTTP routes
// on the api mux. Called once from the server boot sequence.
//
// Returns the registry so the caller can arrange Stop on shutdown
// and call NotifyInstanceAttach/Detach as instances come and go.
func (s *Server) startApps(apiMux *http.ServeMux) (*framework.Registry, error) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	reg := framework.NewRegistry(s.store.db, logger)

	// Built-in apps mounted in-process via the legacy framework.
	// Tasks + status moved to standalone repos (github.com/apteva/app-tasks,
	// app-status) — they are now distributed via the sidecar Apps system
	// (see apps_loader.go) and no longer compiled in here. Channelchat
	// stays in-process for now because it's deeply tied to the channel
	// dispatch infrastructure; it'll graduate to a sidecar in a follow-up.
	resolver := &serverResolver{srv: s}
	apps := []framework.App{
		channelchat.New(resolver),
	}
	for _, a := range apps {
		if err := reg.Load(a); err != nil {
			return nil, fmt.Errorf("load app: %w", err)
		}
	}
	if err := reg.Start(); err != nil {
		return nil, fmt.Errorf("start apps: %w", err)
	}
	// Seed apps + app_installs rows for every built-in so they show up
	// alongside sidecar apps in the dashboard's Installed tab. Idempotent
	// across restarts: INSERT OR IGNORE keys off (name) and (app_id,
	// project_id='') so re-seeding is a no-op once the rows exist.
	s.seedBuiltinInstalls(reg)
	// Mount each app's HTTP routes under /api/apps/<slug>/...
	reg.MountHTTP(apiMux, s.authMiddleware)

	// Hook into per-instance startup so new instances get app
	// channels registered automatically. Runs inside
	// InstanceManager.Start after the CLI bridge is registered —
	// while im.mu is held write-locked. The hook path must NOT
	// touch any InstanceManager accessor that takes the mutex
	// (GetPort, GetCoreAPIKey, etc.); we pass the Instance
	// directly so everything we need is already in hand.
	s.instances.PostChannelsInit = func(inst *Instance, ic *InstanceChannels) {
		s.attachAppChannelsDuringStart(reg, inst, ic)
	}

	// Fan NotifyInstanceAttach for every instance that's already
	// running (server restart case — instances persist across
	// restarts in our model, apps need to see them). This also
	// ensures default chat rows exist for pre-existing instances
	// before the dashboard asks for them.
	s.notifyAppsAboutExistingInstances(reg)

	return reg, nil
}

// attachAppChannelsDuringStart runs INSIDE InstanceManager.Start while
// im.mu is held — so it builds the app-facing InstanceInfo from the
// Instance pointer directly rather than looking it up via accessors
// that would re-acquire the mutex and deadlock. Port and CoreAPIKey
// are left zero-valued: the core process hasn't been spawned yet,
// and the app facets that fire at attach time (OnInstanceAttach,
// Channel factory Build) don't need them. Anything that needs them
// happens later, through the registry's NotifyInstanceAttach path.
func (s *Server) attachAppChannelsDuringStart(reg *framework.Registry, inst *Instance, ic *InstanceChannels) {
	info := framework.InstanceInfo{
		ID:        inst.ID,
		Name:      inst.Name,
		UserID:    inst.UserID,
		ProjectID: inst.ProjectID,
	}
	for _, app := range reg.Loaded() {
		ctx := reg.AppCtxFor(app.Manifest().Slug)
		if err := app.OnInstanceAttach(ctx, info); err != nil {
			continue
		}
		for _, factory := range app.Channels() {
			ch, err := factory.Build(ctx, info)
			if err != nil {
				continue
			}
			ic.registry.Register(ch)
		}
	}
}

// attachAppChannelsToInstance is the OUTSIDE-of-Start variant used by
// notifyAppsAboutExistingInstances — safe to use accessors here
// because we are NOT holding im.mu.
func (s *Server) attachAppChannelsToInstance(reg *framework.Registry, instanceID int64, ic *InstanceChannels) {
	info := s.buildInstanceInfo(instanceID)
	if info == nil {
		return
	}
	for _, app := range reg.Loaded() {
		ctx := reg.AppCtxFor(app.Manifest().Slug)
		if err := app.OnInstanceAttach(ctx, *info); err != nil {
			continue
		}
		for _, factory := range app.Channels() {
			ch, err := factory.Build(ctx, *info)
			if err != nil {
				continue
			}
			ic.registry.Register(ch)
		}
	}
}

// buildInstanceInfo assembles the read-only view apps receive. Returns
// nil if the instance row doesn't exist (racy deletion case).
func (s *Server) buildInstanceInfo(instanceID int64) *framework.InstanceInfo {
	row := s.store.db.QueryRow(
		`SELECT id, name, user_id, COALESCE(project_id,'') FROM instances WHERE id = ?`,
		instanceID,
	)
	var info framework.InstanceInfo
	if err := row.Scan(&info.ID, &info.Name, &info.UserID, &info.ProjectID); err != nil {
		return nil
	}
	info.Port = s.instances.GetPort(info.ID)
	info.CoreAPIKey = s.instances.GetCoreAPIKey(info.ID)
	return &info
}

// stopApps is the inverse of startApps. 5s timeout gives workers a
// chance to drain in-flight work.
func (s *Server) stopApps(reg *framework.Registry) {
	if reg == nil {
		return
	}
	reg.Stop(5 * time.Second)
}

// notifyAppsAboutExistingInstances runs the per-instance attach flow
// for every instance in the database, so apps see existing instances
// after a server restart. Covers both already-running instances
// (whose ChannelRegistry was created before the framework existed)
// and stopped ones (so default rows like chat exist ahead of first use).
func (s *Server) notifyAppsAboutExistingInstances(reg *framework.Registry) {
	rows, err := s.store.db.Query(
		`SELECT id FROM instances`,
	)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		info := s.buildInstanceInfo(id)
		if info == nil {
			continue
		}
		// For running instances whose InstanceChannels already
		// exists, attach the channels to that live registry. For
		// stopped ones, OnInstanceAttach still runs (to create
		// default rows), but Build is skipped since there's no
		// registry yet — their channels will be built when they
		// start via PostChannelsInit.
		ic := s.instances.GetChannels(id)
		if ic != nil {
			s.attachAppChannelsToInstance(reg, id, ic)
		} else {
			for _, app := range reg.Loaded() {
				ctx := reg.AppCtxFor(app.Manifest().Slug)
				_ = app.OnInstanceAttach(ctx, *info)
			}
		}
	}
}

// --- InstanceResolver impl ---------------------------------------------

// serverResolver is the Server's implementation of the app-side
// InstanceResolver interface. Every method is a thin wrapper over
// existing Server machinery so app code stays decoupled.
type serverResolver struct {
	srv *Server
}

func (r *serverResolver) OwnedInstance(userID, instanceID int64) (framework.InstanceInfo, error) {
	inst, err := r.srv.store.GetInstance(userID, instanceID)
	if err != nil {
		return framework.InstanceInfo{}, err
	}
	return framework.InstanceInfo{
		ID:         inst.ID,
		Name:       inst.Name,
		UserID:     inst.UserID,
		ProjectID:  inst.ProjectID,
		Port:       r.srv.instances.GetPort(inst.ID),
		CoreAPIKey: r.srv.instances.GetCoreAPIKey(inst.ID),
	}, nil
}

func (r *serverResolver) LookupUserID(req *http.Request) int64 {
	return getUserID(req)
}

// InstanceIDsForUser fans across every project the user owns. Used by
// channel-chat's notifications-tray endpoints to scope global SSE +
// unread-summary to "this user's chats".
func (r *serverResolver) InstanceIDsForUser(userID int64) ([]int64, error) {
	insts, err := r.srv.store.ListInstances(userID, "")
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(insts))
	for _, inst := range insts {
		ids = append(ids, inst.ID)
	}
	return ids, nil
}

// ForwardEvent pushes a text event into the instance's core /event
// endpoint. Uses the same makeSendEvent helper the slack path uses so
// there's one canonical way text events reach the agent.
func (r *serverResolver) ForwardEvent(inst framework.InstanceInfo, text, threadID string) error {
	if inst.Port == 0 {
		return fmt.Errorf("instance %d has no core port — is it running?", inst.ID)
	}
	send := makeSendEvent(inst.Port, inst.CoreAPIKey)
	send(text, threadID)
	return nil
}

// seedBuiltinInstalls writes one apps + app_installs row per bundled
// app so they appear in the dashboard's Installed tab alongside sidecar
// installs. Status is hard-coded 'running' since the framework already
// started them; uninstall is gated in the dashboard via source='builtin'.
//
// Translation: framework.Manifest is a smaller struct than sdk.Manifest,
// so we synthesize an sdk shape with the bundled metadata. The list
// handler reads manifest_json back through sdk.Manifest, so anything
// the dashboard renders (display_name, description, ui panels) must
// land in the right sdk fields here.
func (s *Server) seedBuiltinInstalls(reg *framework.Registry) {
	for _, app := range reg.Loaded() {
		fm := app.Manifest()
		display := fm.Name
		if display == "" {
			display = fm.Slug
		}
		var panels []sdk.UIPanel
		for _, slot := range fm.UISlots {
			panels = append(panels, sdk.UIPanel{
				Slot:  slot.Slot,
				Label: slot.Title,
				Entry: slot.Entry,
			})
		}
		manifest := sdk.Manifest{
			Schema:      sdk.SchemaCurrent,
			Name:        fm.Slug,
			DisplayName: display,
			Version:     fm.Version,
			Description: fm.Description,
			Author:      "Apteva",
			Scopes:      []sdk.Scope{sdk.ScopeGlobal},
			Provides: sdk.Provides{
				UIPanels: panels,
			},
		}
		manifestJSON, _ := json.Marshal(manifest)

		// INSERT OR IGNORE on apps. SQLite returns lastInsertId=0 when
		// the row already exists, so we re-select to get the id either
		// way. Source='builtin' is the dashboard's signal to hide the
		// uninstall control.
		if _, err := s.store.db.Exec(
			`INSERT OR IGNORE INTO apps (name, source, repo, ref, manifest_json)
			 VALUES (?, 'builtin', '', '', ?)`,
			fm.Slug, string(manifestJSON),
		); err != nil {
			log.Printf("[APPS] seed builtin %s: insert apps: %v", fm.Slug, err)
			continue
		}
		// Always re-write manifest_json — keeps the row in sync with
		// the bundled code if Slug/Name/UISlots changed across versions.
		if _, err := s.store.db.Exec(
			`UPDATE apps SET manifest_json = ? WHERE name = ?`,
			string(manifestJSON), fm.Slug,
		); err != nil {
			log.Printf("[APPS] seed builtin %s: update manifest: %v", fm.Slug, err)
		}
		var appID int64
		if err := s.store.db.QueryRow(
			`SELECT id FROM apps WHERE name = ?`, fm.Slug,
		).Scan(&appID); err != nil {
			log.Printf("[APPS] seed builtin %s: lookup id: %v", fm.Slug, err)
			continue
		}
		// Global install row. UNIQUE(app_id, project_id) makes this a
		// no-op once seeded; on every boot we still bump status back to
		// 'running' since the bundled app is always running.
		if _, err := s.store.db.Exec(
			`INSERT OR IGNORE INTO app_installs
				(app_id, project_id, status, version, upgrade_policy, permissions_json)
			 VALUES (?, '', 'running', ?, 'manual', '[]')`,
			appID, fm.Version,
		); err != nil {
			log.Printf("[APPS] seed builtin %s: insert install: %v", fm.Slug, err)
			continue
		}
		// Don't touch `version` on every boot — that pegs installed
		// to bundled and makes "update available" detection impossible
		// for built-ins. Version flips on explicit upgrade only
		// (POST /api/apps/installs/{id}/upgrade). The seed INSERT above
		// sets the initial version when the row is first created.
		s.store.db.Exec(
			`UPDATE app_installs SET status='running' WHERE app_id=? AND project_id=''`,
			appID,
		)
	}
}
