package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func runtimeDirectoryDigest(root string) (string, error) {
	h := sha256.New()
	var total int64
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("static package contains a link or special file")
		}
		total += info.Size()
		if total > runtimeSnapshotLimit {
			return fmt.Errorf("static package exceeds package limit")
		}
		relative, _ := filepath.Rel(root, path)
		sum, err := artifactFileHash(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%s\n", filepath.ToSlash(relative), sum)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}
func (b *localRuntimeBackend) prepareStatic(req runtimeBackendRequest) (runtimeExecution, error) {
	dir, err := b.m.s.resolveStaticInstallDir(&req.Manifest)
	if err != nil {
		return runtimeExecution{}, err
	}
	digest, err := runtimeDirectoryDigest(dir)
	if err != nil {
		return runtimeExecution{}, err
	}
	d, err := b.m.loadDeployment(req.DeploymentID)
	if err != nil {
		return runtimeExecution{}, err
	}
	d.LocalBin = dir
	d.LocalPort = 0
	if err = b.m.saveDeployment(d); err != nil {
		return runtimeExecution{}, err
	}
	return runtimeExecution{Handle: d.ID, State: "prepared", HostKey: "server:local", ReleaseDigest: digest, Platform: localPlatform(), AppCapabilities: runtimeAppCaps{ReplicaSafe: true, WorkerCoordination: true, Routing: "stateless"}}, nil
}
func (b *localRuntimeBackend) inspectStatic(req runtimeBackendRequest) (runtimeExecution, error) {
	d, err := b.m.loadDeployment(req.DeploymentID)
	if err != nil {
		return runtimeExecution{}, err
	}
	out := d.Execution
	if d.Status == "prepared" || d.Status == "preparing" {
		out.State = "prepared"
		out.StoppedConfirmed = true
		out.Ready = false
		return out, nil
	}
	if d.Status == "stopped" || d.Status == "deleted" {
		out.State = "stopped"
		out.Ready = false
		out.StoppedConfirmed = true
		return out, nil
	}
	if _, err = os.Stat(filepath.Join(d.LocalBin, "index.html")); err != nil {
		return out, fmt.Errorf("static package index is missing")
	}
	out.State = "running"
	out.Ready = true
	out.Endpoint = "static://" + d.LocalBin
	return out, nil
}
func newRuntimeStaticHandler(s *Server, e *InstalledApp) http.Handler {
	target, err := url.Parse(e.SidecarURL)
	if err != nil || runtimeSecureURL(e.SidecarURL) != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "static deployment unavailable", 503) })
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	original := proxy.Director
	proxy.Director = func(r *http.Request) {
		original(r)
		r.Header.Del("Cookie")
		r.Header.Del("If-None-Match")
		r.Header.Del("If-Modified-Since")
		r.Header.Set("Authorization", "Bearer "+e.Token)
		r.Header.Set("Accept-Encoding", "identity")
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Del("Set-Cookie")
		if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
			return nil
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20+1))
		if err != nil || len(body) > 2<<20 {
			return fmt.Errorf("remote static HTML exceeds limit")
		}
		cfg := e.Config
		if cfg == nil {
			cfg = map[string]string{}
		}
		installed := map[string]bool{}
		for _, app := range s.installedApps.List() {
			installed[app.AppName] = true
		}
		var names []string
		for name := range installed {
			names = append(names, name)
		}
		branding := map[string]string{}
		if e.Manifest.Provides.UIApp != nil {
			b := e.Manifest.Provides.UIApp.Branding
			branding = map[string]string{"title_template": b.TitleTemplate, "logo": b.Logo, "theme_css": b.ThemeCSS}
		}
		payload := map[string]any{"base": resolveMountPath(&e.Manifest, cfg), "api_base": "/api", "app_name": e.AppName, "install_id": e.InstallID, "default_project": cfg["default_project"], "kiosk_api_key": cfg["kiosk_api_key"], "branding": branding, "installed_apps": names}
		injection := []byte(fmt.Sprintf("<script>window.__APTEVA_APP__=%s;window.__API_BASE__=\"/api\";window.__DEFAULT_PROJECT__=%s;</script>", runtimeJSON(payload), runtimeJSON(cfg["default_project"])))
		body = injectInto(body, injection)
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", fmt.Sprint(len(body)))
		resp.Header.Del("ETag")
		resp.Header.Set("Cache-Control", "no-store")
		return nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead || strings.HasPrefix(r.URL.Path, runtimeControlPrefix) {
			http.Error(w, "not found", 404)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}
