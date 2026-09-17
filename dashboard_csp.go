package main

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"

	"golang.org/x/net/html"
)

// Rewrite only CSP meta tags, leaving scripts/import maps byte-for-byte intact
// so their existing CSP hashes remain valid. Every enforcing meta policy must
// allow the destinations: a second, looser response header cannot override it.
func dashboardHTMLWithOrigins(document []byte, origins []string) ([]byte, error) {
	if len(origins) == 0 {
		return document, nil
	}
	validated, err := normalizeDashboardConnectOrigins(origins)
	if err != nil {
		return nil, err
	}
	z := html.NewTokenizer(bytes.NewReader(document))
	var out bytes.Buffer
	found := false
	for {
		kind := z.Next()
		if kind == html.ErrorToken {
			if z.Err() != io.EOF {
				return nil, z.Err()
			}
			break
		}
		raw := append([]byte(nil), z.Raw()...)
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
			out.Write(raw)
			continue
		}
		token := z.Token()
		if token.Data != "meta" {
			out.Write(raw)
			continue
		}
		isCSP := false
		content := -1
		for i, attr := range token.Attr {
			if attr.Key == "http-equiv" && strings.EqualFold(attr.Val, "Content-Security-Policy") {
				isCSP = true
			}
			if attr.Key == "content" {
				content = i
			}
		}
		if !isCSP {
			out.Write(raw)
			continue
		}
		if content < 0 {
			return nil, fmt.Errorf("dashboard CSP meta is missing content")
		}
		directives := strings.Split(token.Attr[content].Val, ";")
		connected := false
		for i, directive := range directives {
			fields := strings.Fields(directive)
			if len(fields) == 0 || !strings.EqualFold(fields[0], "connect-src") {
				continue
			}
			connected = true
			sources := []string{fields[0]}
			seen := map[string]bool{}
			for _, value := range fields[1:] {
				if value != "'none'" {
					sources = append(sources, value)
					seen[value] = true
				}
			}
			for _, origin := range validated {
				if !seen[origin] {
					sources = append(sources, origin)
					seen[origin] = true
				}
			}
			directives[i] = strings.Join(sources, " ")
		}
		if !connected {
			return nil, fmt.Errorf("dashboard CSP has no connect-src directive")
		}
		token.Attr[content].Val = strings.Join(directives, ";")
		out.WriteString(token.String())
		found = true
	}
	if !found {
		return nil, fmt.Errorf("dashboard CSP meta not found")
	}
	return out.Bytes(), nil
}

// dashboardSPAHandler is shared by disk and embedded dashboards. Dynamic HTML
// never uses ServeFile's modification time / 304 logic: a registration change
// must be visible on reload even when the underlying index.html is unchanged.
func (s *Server) dashboardSPAHandler(files fs.FS) http.Handler {
	assets := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "GET or HEAD only", http.StatusMethodNotAllowed)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" && path != "index.html" {
			if info, err := fs.Stat(files, path); err == nil && !info.IsDir() {
				setStaticCacheHeaders(w, path, false)
				assets.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		document, err := fs.ReadFile(files, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		origins, err := s.dashboardConnectOrigins()
		if err == nil {
			document, err = dashboardHTMLWithOrigins(document, origins)
		}
		if err != nil {
			http.Error(w, "Could not prepare dashboard security policy", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.Method != http.MethodHead {
			_, _ = w.Write(document)
		}
	})
}
