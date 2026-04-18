package framework

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Registry is the top-level handle the server holds. It loads apps at
// boot, wires their HTTP routes, runs migrations, starts workers, and
// fans instance lifecycle notifications out to every loaded app.
type Registry struct {
	mu         sync.RWMutex
	apps       map[string]*loadedApp // slug → loaded
	bus        *AppBus
	db         *sql.DB
	logger     *slog.Logger
	rootCtx    context.Context
	cancelRoot context.CancelFunc
}

type loadedApp struct {
	slug       string
	manifest   Manifest
	app        App
	ctx        *AppCtx
	supervisor *Supervisor
	unsubs     []func()
}

// AuthMiddleware is the type apteva-server's authMiddleware has; we
// accept it as a value so the framework doesn't import server internals.
type AuthMiddleware func(http.HandlerFunc) http.HandlerFunc

// NewRegistry constructs an empty registry. Call Load once per app,
// then Start to run migrations + OnMount + Workers in one pass.
func NewRegistry(db *sql.DB, logger *slog.Logger) *Registry {
	ctx, cancel := context.WithCancel(context.Background())
	return &Registry{
		apps:       make(map[string]*loadedApp),
		bus:        NewAppBus(ctx, logger),
		db:         db,
		logger:     logger,
		rootCtx:    ctx,
		cancelRoot: cancel,
	}
}

// Bus exposes the AppBus for apteva-server to publish platform-level
// events (e.g. instance.started) that apps can subscribe to.
func (r *Registry) Bus() *AppBus { return r.bus }

// Load registers an app. Does NOT run its migrations or OnMount yet —
// Start does that. Splitting the phases lets multiple apps be loaded
// and then booted in one atomic pass, which matters if app A depends
// on app B's schema during its own OnMount.
func (r *Registry) Load(app App) error {
	m := app.Manifest()
	if m.Slug == "" {
		return fmt.Errorf("app manifest missing slug")
	}
	if strings.ContainsAny(m.Slug, " _.") {
		return fmt.Errorf("app slug %q must be kebab-case (letters, digits, '-')", m.Slug)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.apps[m.Slug]; exists {
		return fmt.Errorf("app %q already loaded", m.Slug)
	}
	ctx := &AppCtx{
		Slug:     m.Slug,
		Manifest: m,
		DB:       r.db,
		Bus:      r.bus,
		Logger:   r.logger.With("app", m.Slug),
		Ctx:      r.rootCtx,
		CallApp:  r.makeCallApp(),
	}
	r.apps[m.Slug] = &loadedApp{
		slug:       m.Slug,
		manifest:   m,
		app:        app,
		ctx:        ctx,
		supervisor: NewSupervisor(r.rootCtx, ctx),
	}
	return nil
}

// Start runs migrations, OnMount, and worker supervision for every
// loaded app in load order. Errors abort the sequence (apps already
// started are left running — caller should Stop + exit).
func (r *Registry) Start() error {
	r.mu.RLock()
	apps := make([]*loadedApp, 0, len(r.apps))
	for _, a := range r.apps {
		apps = append(apps, a)
	}
	r.mu.RUnlock()

	for _, a := range apps {
		if err := RunMigrations(r.db, a.slug, a.app.Migrations()); err != nil {
			return fmt.Errorf("migrations for %q: %w", a.slug, err)
		}
		if err := a.app.OnMount(a.ctx); err != nil {
			return fmt.Errorf("OnMount %q: %w", a.slug, err)
		}
		// Subscribe EventHandlers.
		for _, h := range a.app.EventHandlers() {
			cancel := r.bus.Subscribe(h.Topic, a.ctx, h.Handler)
			a.unsubs = append(a.unsubs, cancel)
		}
		// Launch workers.
		for _, w := range a.app.Workers() {
			a.supervisor.Start(w)
		}
		r.logger.Info("app started",
			"slug", a.slug,
			"version", a.manifest.Version,
			"workers", len(a.app.Workers()),
			"handlers", len(a.app.EventHandlers()),
		)
	}
	return nil
}

// Stop gracefully shuts down every app: cancel subscriptions, call
// OnUnmount, stop workers. Safe to call multiple times.
func (r *Registry) Stop(timeout time.Duration) {
	r.mu.RLock()
	apps := make([]*loadedApp, 0, len(r.apps))
	for _, a := range r.apps {
		apps = append(apps, a)
	}
	r.mu.RUnlock()

	for _, a := range apps {
		for _, u := range a.unsubs {
			u()
		}
		a.unsubs = nil
		_ = a.app.OnUnmount(a.ctx)
		a.supervisor.Stop(timeout)
	}
	r.cancelRoot()
}

// MountHTTP collects every loaded app's HTTPRoutes and registers them
// under /api/apps/<slug><path> on the given mux. Routes with NoAuth
// skip the wrapper; everything else is wrapped by authMW.
//
// Also mounts /api/apps/manifest which returns the list of loaded
// apps with their manifests — dashboard uses this to decide what UI
// slots to render.
func (r *Registry) MountHTTP(mux *http.ServeMux, authMW AuthMiddleware) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// apteva-server mounts its api mux at /api with a StripPrefix
	// wrapper, so inside the mux handlers see paths WITHOUT the
	// /api prefix. Routes here must match that — i.e. just
	// /apps/<slug>/... — or the dispatcher falls through to the
	// SPA catch-all and the caller gets "404 page not found".
	mux.HandleFunc("/apps/manifest", authMW(r.handleManifest))

	// Group routes by (path, NoAuth) so we register ONE http handler
	// per unique path and dispatch by method inside. Works on Go's
	// classic ServeMux where duplicate HandleFunc calls on the same
	// pattern panic at registration time. Each bucket may contain
	// multiple method→handler entries.
	type bucket struct {
		noAuth   bool
		methods  map[string]func(http.ResponseWriter, *http.Request, *AppCtx)
		anyAny   func(http.ResponseWriter, *http.Request, *AppCtx) // for Method=""
		appCtx   *AppCtx
	}
	buckets := map[string]*bucket{}
	for _, a := range r.apps {
		prefix := "/apps/" + a.slug
		for _, route := range a.app.HTTPRoutes() {
			full := prefix + route.Path
			b, ok := buckets[full]
			if !ok {
				b = &bucket{methods: map[string]func(http.ResponseWriter, *http.Request, *AppCtx){}, appCtx: a.ctx, noAuth: route.NoAuth}
				buckets[full] = b
			}
			method := strings.ToUpper(route.Method)
			if method == "" {
				b.anyAny = route.Handler
			} else {
				b.methods[method] = route.Handler
			}
		}
	}
	for path, b := range buckets {
		b := b
		h := func(w http.ResponseWriter, req *http.Request) {
			if fn, ok := b.methods[strings.ToUpper(req.Method)]; ok {
				fn(w, req, b.appCtx)
				return
			}
			if b.anyAny != nil {
				b.anyAny(w, req, b.appCtx)
				return
			}
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		if b.noAuth {
			mux.HandleFunc(path, h)
		} else {
			mux.HandleFunc(path, authMW(h))
		}
	}
}

func (r *Registry) handleManifest(w http.ResponseWriter, _ *http.Request) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Manifest, 0, len(r.apps))
	for _, a := range r.apps {
		out = append(out, a.manifest)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// AppCtxFor returns the AppCtx of a loaded app by slug, for the
// small number of places apteva-server needs to call into an app
// directly (e.g. the per-instance channel attach loop).
func (r *Registry) AppCtxFor(slug string) *AppCtx {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if a, ok := r.apps[slug]; ok {
		return a.ctx
	}
	return nil
}

// AppFor returns the App interface for a loaded slug.
func (r *Registry) AppFor(slug string) App {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if a, ok := r.apps[slug]; ok {
		return a.app
	}
	return nil
}

// Loaded returns every loaded app in load order. Used by the
// per-instance attach loop.
func (r *Registry) Loaded() []App {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]App, 0, len(r.apps))
	for _, a := range r.apps {
		out = append(out, a.app)
	}
	return out
}

// makeCallApp returns a closure every app's AppCtx uses as CallApp.
// Stage 1 has only one app; this is a placeholder that returns
// ErrNoApp for anything but self-calls.
func (r *Registry) makeCallApp() func(slug, fn string, args any) (any, error) {
	return func(slug, fn string, args any) (any, error) {
		r.mu.RLock()
		_, ok := r.apps[slug]
		r.mu.RUnlock()
		if !ok {
			return nil, ErrNoApp
		}
		// Inter-app RPC surface isn't defined yet — stage 2 work.
		// Apps that need it for v1 can use the AppBus pattern.
		return nil, fmt.Errorf("CallApp not implemented (stage 2 work)")
	}
}

// NotifyInstanceAttach fans the OnInstanceAttach hook across all apps.
// Called from apteva-server when an instance comes online.
func (r *Registry) NotifyInstanceAttach(inst InstanceInfo) {
	r.mu.RLock()
	apps := make([]*loadedApp, 0, len(r.apps))
	for _, a := range r.apps {
		apps = append(apps, a)
	}
	r.mu.RUnlock()

	for _, a := range apps {
		if err := a.app.OnInstanceAttach(a.ctx, inst); err != nil && r.logger != nil {
			r.logger.Warn("OnInstanceAttach error",
				"app", a.slug, "instance", inst.ID, "err", err,
			)
		}
	}
}

// NotifyInstanceDetach fans OnInstanceDetach. Called before the
// channel registry is torn down so apps can flush state that
// depends on channels.
func (r *Registry) NotifyInstanceDetach(inst InstanceInfo) {
	r.mu.RLock()
	apps := make([]*loadedApp, 0, len(r.apps))
	for _, a := range r.apps {
		apps = append(apps, a)
	}
	r.mu.RUnlock()

	for _, a := range apps {
		if err := a.app.OnInstanceDetach(a.ctx, inst); err != nil && r.logger != nil {
			r.logger.Warn("OnInstanceDetach error",
				"app", a.slug, "instance", inst.ID, "err", err,
			)
		}
	}
}
