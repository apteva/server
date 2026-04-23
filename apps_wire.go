package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/apteva/server/apps/channelchat"
	"github.com/apteva/server/apps/framework"
	"github.com/apteva/server/apps/status"
	"github.com/apteva/server/apps/tasks"
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

	// Load every built-in app. When we add a second, it goes on this
	// list — that's the only place you touch to onboard an app.
	resolver := &serverResolver{srv: s}
	apps := []framework.App{
		channelchat.New(resolver),
		tasks.New(resolver),
		status.New(resolver),
	}
	for _, a := range apps {
		if err := reg.Load(a); err != nil {
			return nil, fmt.Errorf("load app: %w", err)
		}
	}
	if err := reg.Start(); err != nil {
		return nil, fmt.Errorf("start apps: %w", err)
	}
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
