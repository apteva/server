package main

import (
	"net/http"
	"strings"
)

// App-owned CORS for public routes.
//
// An app marks a no_auth route with `cors: app` in its manifest to answer
// CORS for that route itself. This is for callers the app cannot list in
// advance: a page served in a sandbox, whose origin is "null", or a public
// API called from arbitrary sites. On such a route the platform:
//
//   - forwards CORS preflights to the sidecar for any origin and adds no
//     CORS headers of its own;
//   - never lets the response carry Access-Control-Allow-Credentials;
//   - ignores the caller's platform session cookie, so the request is not
//     authenticated as that user, is not subject to the session CSRF check,
//     and does not carry the cookie to the sidecar.
//
// The cookie is the one credential a browser attaches by itself, which is
// what makes it unsafe to pair with an arbitrary origin. A token the caller
// sends explicitly is handled as on any other public route.

// appOwnsCORSForRequest reports whether the request targets a running
// install's route that declares `cors: app`. It reads only the in-memory
// app registry.
func (s *Server) appOwnsCORSForRequest(r *http.Request) bool {
	if s == nil || s.installedApps == nil || r == nil {
		return false
	}
	appName, appPath, ok := splitAppProxyPath(r.URL.Path)
	if !ok || isAppManagementRoute(appName) {
		return false
	}
	strippedPath, installID, hasInstallSelector, err := splitAppInstallSelector(appPath)
	if err != nil {
		return false
	}
	var entry *InstalledApp
	if hasInstallSelector {
		entry = s.installedApps.Get(installID)
		if entry == nil || entry.AppName != appName {
			return false
		}
	} else {
		entry = s.installedAppForRequest(appName, r)
		strippedPath = appPath
	}
	return appProxyRouteOwnsCORS(entry, strippedPath, corsRequestedMethod(r))
}

func appProxyRouteOwnsCORS(entry *InstalledApp, path, method string) bool {
	if entry == nil {
		return false
	}
	for _, route := range entry.Manifest.Provides.HTTPRoutes {
		if !route.AppOwnsCORS() {
			continue
		}
		if route.Method != "" && !strings.EqualFold(route.Method, method) {
			continue
		}
		if appRouteMatches(route.Prefix, path) {
			return true
		}
	}
	return false
}

// dropSessionCookie removes the platform session cookie from the request
// and leaves every other cookie in place.
func dropSessionCookie(r *http.Request) {
	cookies := r.Cookies()
	kept := make([]string, 0, len(cookies))
	dropped := false
	for _, cookie := range cookies {
		if cookie.Name == cookieName {
			dropped = true
			continue
		}
		kept = append(kept, cookie.Name+"="+cookie.Value)
	}
	if !dropped {
		return
	}
	if len(kept) == 0 {
		r.Header.Del("Cookie")
		return
	}
	r.Header.Set("Cookie", strings.Join(kept, "; "))
}
