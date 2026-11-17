package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	sdk "github.com/apteva/app-sdk"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type ingressFixture struct {
	mu                     sync.Mutex
	phase, dep, hash, salt string
	generation             int64
	applies, rollbacks     int
	lose                   bool
	wrongPublic            bool
}

func (f *ingressFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	action := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if action == "describe" {
		writeJSON(w, map[string]any{"contract": runtimeIngressContract, "id": "opaque-edge", "adoption": true, "rollback": true})
		return
	}
	if r.Header.Get("Authorization") == "" {
		http.Error(w, "auth", 401)
		return
	}
	var req runtimeIngressIntent
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "invalid", 400)
		return
	}
	if req.Contract != runtimeIngressContract || len(req.Routes) != 2 || req.Routes[0].Path != "/browser/*" || req.Routes[1].Path != "/carrier/*" {
		http.Error(w, "wrong normalized routes", 400)
		return
	}
	plan := runtimeHash([]byte(runtimeJSON(struct {
		Host, Upstream, Deployment, Salt string
		Generation                       int64
	}{req.Hostname, req.Upstream, req.DeploymentID, f.salt, req.Generation})))
	switch action {
	case "prepare":
		f.phase = "prepared"
		f.dep = req.DeploymentID
		f.generation = req.Generation
		writeJSON(w, map[string]any{"phase": "prepared", "host_key": "provider-host-identity", "options": req.Options})
	case "inspect":
		writeJSON(w, map[string]any{"plan_sha256": plan, "host_key": "provider-host-identity", "maintenance_window_required": true})
	case "adopt", "publish":
		if !req.MaintenanceWindow || req.PlanSHA256 != plan {
			http.Error(w, "stale or unquiesced", 409)
			return
		}
		f.applies++
		f.phase = "active"
		f.dep = req.DeploymentID
		f.generation = req.Generation
		f.hash = plan
		if f.lose {
			f.lose = false
			http.Error(w, "lost response", 503)
			return
		}
		writeJSON(w, map[string]any{"phase": "active"})
	case "rollback":
		f.rollbacks++
		f.phase = "released"
		writeJSON(w, map[string]any{"phase": "released"})
	case "unpublish":
		writeJSON(w, map[string]any{"phase": "active"})
	case "status":
		if f.phase == "" {
			http.Error(w, "unmanaged", 404)
			return
		}
		writeJSON(w, map[string]any{"phase": f.phase, "deployment_id": f.dep, "generation": f.generation, "source_matches": true, "plan_sha256": f.hash})
	default:
		http.Error(w, "unknown", 404)
	}
}
func genericIngressSetup(t *testing.T) (*Server, *appRuntimeManager, *runtimeBackendFixture, *ingressFixture, int64, runtimeIngressAdoptionRequest) {
	t.Helper()
	s, m, backend, id, p := runtimeTestSetup(t)
	fixture := &ingressFixture{}
	public := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		dep := fixture.dep
		wrong := fixture.wrongPublic
		fixture.mu.Unlock()
		if wrong {
			dep = "wrong-owner"
		}
		w.Write([]byte(dep))
	}))
	t.Cleanup(public.Close)
	roots := x509.NewCertPool()
	roots.AddCert(public.Certificate())
	dialer := &net.Dialer{}
	m.http.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "example.com:443" {
			address = public.Listener.Addr().String()
		}
		return dialer.DialContext(ctx, network, address)
	}}
	manifest, e := m.resourceManifest(id)
	if e != nil {
		t.Fatal(e)
	}
	manifest.Runtime.Ingress = &sdk.RuntimeIngress{Routes: []sdk.RuntimeIngressRoute{{Path: "/browser/*"}, {Path: "/carrier/*"}}, Endpoints: map[string]string{"browser": "/browser", "callbacks": "/carrier"}, BrowserPath: "/browser"}
	if _, e = s.store.db.Exec(`UPDATE app_installs SET manifest_json=? WHERE id=?`, runtimeJSON(manifest), id); e != nil {
		t.Fatal(e)
	}
	op, e := m.begin(id, "deploy", "initial", p, &manifest)
	if e != nil {
		t.Fatal(e)
	}
	if done := waitRuntimeOperation(t, m, op); done.Status != "succeeded" {
		t.Fatal(done.Error)
	}
	d, e := m.active(id)
	if e != nil {
		t.Fatal(e)
	}
	backend.mu.Lock()
	execution := backend.executions[d.ID]
	execution.IngressUpstream = "http://127.0.0.1:55001"
	backend.executions[d.ID] = execution
	backend.mu.Unlock()
	// Deliberately a different provider installation, target namespace, and backend
	// ID. The compute backend does not implement managed ingress at all.
	edge := httptest.NewServer(http.HandlerFunc(fixture.serve))
	t.Cleanup(edge.Close)
	edgeManifest := sdk.Manifest{Name: "independent-edge-provider", Version: "1.0.0", Provides: sdk.Provides{HTTPRoutes: []sdk.RouteSpec{{Prefix: "/_apteva/ingress/providers/opaque-edge"}}}}
	provider := seedRunningInstall(t, s, edgeManifest.Name, "", edgeManifest, nil)
	if _, e = s.store.db.Exec(`UPDATE app_installs SET sidecar_url_override=? WHERE id=?`, edge.URL, provider); e != nil {
		t.Fatal(e)
	}

	input := runtimeIngressAdoptionRequest{Hostname: "example.com", Provider: runtimeIngressProviderRef{InstallID: provider, ID: "opaque-edge", Target: runtimeResourceRef{InstallID: provider, Type: "edge.host", ID: "host-b"}, Options: map[string]any{"opaque_option": "kept-by-provider"}}}
	return s, m, backend, fixture, id, input
}
func reviewedIngress(t *testing.T, m *appRuntimeManager, id int64, r runtimeIngressAdoptionRequest) runtimeIngressAdoptionRequest {
	t.Helper()
	plan, e := m.inspectIngressAdoption(id, r)
	if e != nil {
		t.Fatal(e)
	}
	r.PlanSHA256 = plan.PlanSHA256
	r.ProviderPlanSHA256 = plan.ProviderPlanSHA256
	r.HostKey = plan.HostKey
	r.MaintenanceWindow = true
	r.IdempotencyKey = "adopt-reviewed"
	return r
}
func TestGenericIngressIndependentBackendReadOnlyReviewAndRollback(t *testing.T) {
	s, m, backend, f, id, input := genericIngressSetup(t)
	if backend.caps.ManagedIngress {
		t.Fatal("fixture unexpectedly depends on backend ingress")
	}
	original, _ := m.active(id)
	input = reviewedIngress(t, m, id, input)
	var n int
	s.store.db.QueryRow(`SELECT COUNT(*) FROM app_runtime_ingress_bindings`).Scan(&n)
	if n != 0 {
		t.Fatal("inspection wrote bindings")
	}
	s.store.db.QueryRow(`SELECT COUNT(*) FROM app_runtime_operations`).Scan(&n)
	if n != 1 {
		t.Fatal("inspection created operations", n)
	}
	op, e := m.beginIngress(id, "adopt", input)
	if e != nil {
		t.Fatal(e)
	}
	if done := waitRuntimeOperation(t, m, op); done.Status != "succeeded" {
		t.Fatal(done.Error)
	}
	adopted, _ := m.active(id)
	if adopted.Placement.Ingress.Provider.InstallID == adopted.Placement.Backend.InstallID || adopted.Execution.BrowserEndpoint != "https://example.com/browser" {
		t.Fatal("provider was coupled to compute backend", adopted)
	}
	retry, e := m.beginIngress(id, "adopt", input)
	if e != nil || retry.ID != op.ID {
		t.Fatal("duplicate changed operation", retry, e)
	}
	f.mu.Lock()
	applies := f.applies
	f.mu.Unlock()
	if applies != 1 {
		t.Fatal("duplicate proxy application", applies)
	}
	input.IdempotencyKey = "rollback-reviewed"
	rollback, e := m.beginIngress(id, "rollback", input)
	if e != nil {
		t.Fatal(e)
	}
	if done := waitRuntimeOperation(t, m, rollback); done.Status != "succeeded" {
		t.Fatal(done.Error)
	}
	restored, _ := m.active(id)
	if runtimeJSON(restored.Placement) != runtimeJSON(original.Placement) || restored.Execution.BrowserEndpoint != original.Execution.BrowserEndpoint {
		t.Fatal("rollback lost prior placement/endpoints", restored.Placement, original.Placement)
	}
}
func TestGenericIngressStalePlanAndFencedAppCannotPublish(t *testing.T) {
	_, m, backend, f, id, input := genericIngressSetup(t)
	input = reviewedIngress(t, m, id, input)
	f.mu.Lock()
	f.salt = "configuration-changed"
	f.mu.Unlock()
	op, e := m.beginIngress(id, "adopt", input)
	if e != nil {
		t.Fatal(e)
	}
	if done := waitRuntimeOperation(t, m, op); done.Status != "failed" {
		t.Fatal("stale plan succeeded")
	}
	f.mu.Lock()
	calls := f.applies
	f.mu.Unlock()
	if calls != 0 {
		t.Fatal("stale plan reached mutation")
	}
	d, _ := m.active(id)
	backend.mu.Lock()
	backend.admitting[d.ID] = false
	backend.mu.Unlock()
	if _, e = m.inspectIngressAdoption(id, input); e == nil {
		t.Fatal("fenced app received adoption plan")
	}
}
func TestGenericIngressLostResponseAndPublicVerificationRollback(t *testing.T) {
	for _, lose := range []bool{true, false} {
		t.Run(map[bool]string{true: "lost-response", false: "verification"}[lose], func(t *testing.T) {
			_, m, _, f, id, input := genericIngressSetup(t)
			input = reviewedIngress(t, m, id, input)
			f.mu.Lock()
			f.lose = lose
			f.wrongPublic = !lose
			f.mu.Unlock()
			op, e := m.beginIngress(id, "adopt", input)
			if e != nil {
				t.Fatal(e)
			}
			done := waitRuntimeOperation(t, m, op)
			if done.Status != "failed" {
				t.Fatal("expected observable failure", done)
			}
			if lose {
				if e = m.retryOperation(op.ID, id); e != nil {
					t.Fatal(e)
				}
				if retry := waitRuntimeOperation(t, m, op); retry.Status != "succeeded" {
					t.Fatal(retry.Error)
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				if f.applies != 1 {
					t.Fatal("lost response replayed cutover", f.applies)
				}
			} else {
				f.mu.Lock()
				defer f.mu.Unlock()
				if f.rollbacks != 1 || f.phase != "released" {
					t.Fatal("failed HTTPS was not rolled back")
				}
			}
		})
	}
}
func TestGenericIngressExpiredMaintenanceCannotStartCutover(t *testing.T) {
	s, m, _, f, id, input := genericIngressSetup(t)
	input = reviewedIngress(t, m, id, input)
	d, err := m.ingressActive(id, "", true)
	if err != nil {
		t.Fatal(err)
	}
	intent, e := m.ingressIntent(d, input.Provider, input.Hostname, "expired")
	if e != nil {
		t.Fatal(e)
	}
	intent.HostKey = input.HostKey
	intent.PlanSHA256 = input.ProviderPlanSHA256
	op := runtimeOperation{ID: "expired", InstallID: id, Kind: "ingress-adopt", Status: "running", DeploymentID: d.ID, PreviousID: d.ID, IdempotencyKey: "expired", Ingress: &runtimeIngressOperation{Request: input, Intent: intent, MaintenanceUntil: time.Now().Add(-time.Second).Unix()}}
	_, e = s.store.db.Exec(`INSERT INTO app_runtime_operations(id,install_id,kind,status,owner,lease_until,idempotency_key,fingerprint,record_json) VALUES(?,?,?,'running',?,?,?,'',?)`, op.ID, id, op.Kind, m.owner, time.Now().Add(time.Minute).Unix(), op.IdempotencyKey, runtimeJSON(op))
	if e != nil {
		t.Fatal(e)
	}
	if e = m.runIngressOperation(&op); e == nil || !strings.Contains(e.Error(), "expired") {
		t.Fatal(e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applies != 0 {
		t.Fatal("expired attestation caused mutation")
	}
}

func TestGenericIngressFreshSetupUsesSameServerPublication(t *testing.T) {
	s, m, _, f, id, input := genericIngressSetup(t)
	d, e := m.ingressActive(id, "", true)
	if e != nil {
		t.Fatal(e)
	}
	d.Placement.Ingress = &runtimeIngressSpec{Hostname: input.Hostname, Provider: &input.Provider}
	backend, e := m.backend(d)
	if e != nil {
		t.Fatal(e)
	}
	remote := backend.(*remoteRuntimeBackend)
	remote.registration.Caps.IngressUpstream = true
	if e = m.prepareProviderIngress(&d, remote); e != nil {
		t.Fatal(e)
	}
	binding, e := m.ingressBinding(id)
	if e != nil || binding.Phase != "prepared" {
		t.Fatal(binding, e)
	}
	op := runtimeOperation{ID: "fresh-publication", InstallID: id, Kind: "readmit", Status: "running", DeploymentID: d.ID, PreviousID: d.ID, IdempotencyKey: "fresh-publication"}
	_, e = s.store.db.Exec(`INSERT INTO app_runtime_operations(id,install_id,kind,status,owner,lease_until,idempotency_key,fingerprint,record_json) VALUES(?,?,?,'running',?,?,?,'',?)`, op.ID, id, op.Kind, m.owner, time.Now().Add(time.Minute).Unix(), op.IdempotencyKey, runtimeJSON(op))
	if e != nil {
		t.Fatal(e)
	}
	if e = m.commitExecutionOwnership(d, op); e != nil {
		t.Fatal(e)
	}
	if e = m.confirmExecutionAdmission(d, op); e != nil {
		t.Fatal(e)
	}
	if e = m.publishIngress(d, op); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applies != 1 || f.phase != "active" {
		t.Fatal("fresh preparation did not use the same provider publication", f.applies, f.phase)
	}
}

func TestGenericIngressFullFreshDeploymentUsesProviderHint(t *testing.T) {
	s, m, backend, id, p := runtimeTestSetup(t)
	edge := &ingressFixture{}
	public := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		edge.mu.Lock()
		defer edge.mu.Unlock()
		w.Write([]byte(edge.dep))
	}))
	defer public.Close()
	roots := x509.NewCertPool()
	roots.AddCert(public.Certificate())
	dialer := &net.Dialer{}
	m.http.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "example.com:443" {
			address = public.Listener.Addr().String()
		}
		return dialer.DialContext(ctx, network, address)
	}}
	original := backend.server.Config.Handler
	backend.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_apteva/ingress/providers/opaque-edge/") {
			if strings.HasSuffix(r.URL.Path, "/inspect-target") {
				writeJSON(w, map[string]any{"public_ipv4": "203.0.113.10", "host_key": "provider-host-identity"})
				return
			}
			edge.serve(w, r)
			return
		}
		recorder := httptest.NewRecorder()
		original.ServeHTTP(recorder, r)
		var execution map[string]any
		if recorder.Code == 200 && json.Unmarshal(recorder.Body.Bytes(), &execution) == nil && execution["handle"] != nil {
			execution["ingress_upstream"] = "http://127.0.0.1:55001"
			writeJSON(w, execution)
			return
		}
		for k, v := range recorder.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(recorder.Code)
		w.Write(recorder.Body.Bytes())
	})
	backend.caps.IngressUpstream = true
	backend.caps.IngressProvider = &runtimeIngressProviderHint{ID: "opaque-edge"}
	providerManifest, e := installManifest(s, p.Backend.InstallID)
	if e != nil {
		t.Fatal(e)
	}
	providerManifest.Provides.HTTPRoutes = []sdk.RouteSpec{{Prefix: "/_apteva/ingress/providers/opaque-edge"}}
	if _, e = s.store.db.Exec(`UPDATE app_installs SET manifest_json=? WHERE id=?`, runtimeJSON(providerManifest), p.Backend.InstallID); e != nil {
		t.Fatal(e)
	}
	if e = m.registerBackend(runtimeBackendRegistration{InstallID: p.Backend.InstallID, ID: p.Backend.ID, Endpoint: runtimeControlPrefix + "backends/" + p.Backend.ID}); e != nil {
		t.Fatal(e)
	}
	dns := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&call)
		if call.Params.Name != "domain_records_list" {
			t.Error("unexpected DNS mutation", call.Params.Name)
			http.Error(w, "unexpected", 400)
			return
		}
		result := runtimeJSON(map[string]any{"records": []any{map[string]any{"id": "existing", "name": "example.com", "type": "A", "value": "203.0.113.10"}}})
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": result}}}})
	}))
	defer dns.Close()
	dnsID := runtimeFixtureInstall(t, s, "dns-fixture")
	s.store.db.Exec(`UPDATE app_installs SET sidecar_url_override=? WHERE id=?`, dns.URL, dnsID)
	manifest, e := m.resourceManifest(id)
	if e != nil {
		t.Fatal(e)
	}
	manifest.Runtime.Ingress = &sdk.RuntimeIngress{Routes: []sdk.RuntimeIngressRoute{{Path: "/browser/*"}, {Path: "/carrier/*"}}, Endpoints: map[string]string{"browser": "/browser", "callbacks": "/carrier"}, BrowserPath: "/browser"}
	p.Ingress = &runtimeIngressSpec{Hostname: "example.com", Domain: "example.com", DNSInstallID: dnsID}
	op, e := m.begin(id, "deploy", "fresh-default-provider", p, &manifest)
	if e != nil {
		t.Fatal(e)
	}
	if done := waitRuntimeOperation(t, m, op); done.Status != "succeeded" {
		t.Fatal(done.Error)
	}
	deployed, e := m.active(id)
	if e != nil {
		t.Fatal(e)
	}
	if deployed.Placement.Ingress.Provider.ID != "opaque-edge" || deployed.Execution.BrowserEndpoint != "https://example.com/browser" {
		t.Fatalf("provider default/endpoint mismatch: ingress=%s browser=%q", runtimeJSON(deployed.Placement.Ingress), deployed.Execution.BrowserEndpoint)
	}
	backend.mu.Lock()
	events := append([]string{}, backend.events...)
	backend.mu.Unlock()
	for _, event := range events {
		if strings.Contains(event, "publish-ingress") {
			t.Fatal("compute backend still received public route orchestration", events)
		}
	}
	next := manifest
	next.Version = "2.0.0"
	upgrade, e := m.begin(id, "upgrade", "fresh-provider-upgrade", deployed.Placement, &next)
	if e != nil {
		t.Fatal(e)
	}
	if done := waitRuntimeOperation(t, m, upgrade); done.Status != "succeeded" {
		t.Fatal(done.Error)
	}
	old, e := m.loadDeployment(deployed.ID)
	if e != nil || !old.Execution.StoppedConfirmed {
		t.Fatal("public predecessor was not confirmed stopped", e)
	}
	edge.mu.Lock()
	defer edge.mu.Unlock()
	if edge.phase != "active" || edge.applies != 2 {
		t.Fatal("fresh one-flow publication failed", edge.phase, edge.applies)
	}
}
