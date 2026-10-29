package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// stubIntegrationHostLookup makes every guarded lookup answer with ips, the
// way an attacker-controlled DNS name rebinds to an internal address after
// passing the creation-time check.
func stubIntegrationHostLookup(t *testing.T, ips ...string) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	prev := integrationHostLookup
	integrationHostLookup = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		calls.Add(1)
		var out []net.IPAddr
		for _, ip := range ips {
			out = append(out, net.IPAddr{IP: net.ParseIP(ip)})
		}
		return out, nil
	}
	t.Cleanup(func() { integrationHostLookup = prev })
	return &calls
}

func countingServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestIsNonPublicIP(t *testing.T) {
	for _, ip := range []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254",
		"0.0.0.0", "0.1.2.3", "100.64.0.1", "100.127.255.254", "224.0.0.1",
		"::1", "::", "fe80::1", "fc00::1", "ff02::1", "::ffff:127.0.0.1", "::ffff:169.254.169.254",
	} {
		if !isNonPublicIP(net.ParseIP(ip)) {
			t.Errorf("%s should be refused", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "93.184.215.14", "100.63.255.255", "100.128.0.1", "2606:4700:4700::1111"} {
		if isNonPublicIP(net.ParseIP(ip)) {
			t.Errorf("%s should be allowed", ip)
		}
	}
}

func TestIntegrationURLHostIsTemplated(t *testing.T) {
	cases := map[string]bool{
		"{{site_url}}/wp-json/wp/v2":             true,
		"{{site_url}}":                           true,
		"https://{{subdomain}}.zendesk.com/api":  true,
		"https://email.{{region}}.amazonaws.com": true,
		"{{scheme}}://example.com":               true,
		"https://api.stripe.com/v1":              false,
		"https://api.twilio.com/{{account_sid}}": false,
		"https://api.example.com":                false,
		"":                                       false,
	}
	for template, want := range cases {
		if got := integrationURLHostIsTemplated(template); got != want {
			t.Errorf("integrationURLHostIsTemplated(%q) = %v, want %v", template, got, want)
		}
	}
}

func TestPublicIntegrationHostIPsDropsPrivateAnswers(t *testing.T) {
	stubIntegrationHostLookup(t, "127.0.0.1", "93.184.215.14", "169.254.169.254")
	ips, err := publicIntegrationHostIPs(context.Background(), "mixed.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 1 || ips[0].String() != "93.184.215.14" {
		t.Fatalf("ips = %v, want only the public address", ips)
	}
}

func wordpressRESTFixture() (*AppTemplate, *AppToolDef) {
	tool := AppToolDef{Name: "list_posts", Method: "GET", Path: "/posts"}
	app := &AppTemplate{Slug: "wordpress-dial-guard-test", Name: "WordPress", BaseURL: "{{site_url}}/wp-json/wp/v2", Tools: []AppToolDef{tool}}
	return app, &app.Tools[0]
}

// A templated base_url whose host later resolves to loopback must be refused
// at dial time, before any byte reaches the internal service.
func TestTemplatedRESTBaseURLRefusesRebindingToPrivateAddress(t *testing.T) {
	t.Setenv(allowPrivateIntegrationHostsEnv, "")
	srv, hits := countingServer(t)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	for _, rebound := range []string{"127.0.0.1", "169.254.169.254", "10.0.0.5", "100.64.0.1"} {
		lookups := stubIntegrationHostLookup(t, rebound)
		app, tool := wordpressRESTFixture()
		_, err := executeIntegrationTool(app, tool, map[string]string{"site_url": "http://wp.rebind.example.test:" + port}, map[string]any{}, "")
		if err == nil || !errors.Is(err, errNonPublicIntegrationHost) {
			t.Fatalf("rebound to %s: err = %v, want non-public refusal", rebound, err)
		}
		if lookups.Load() == 0 {
			t.Fatalf("rebound to %s: guard did not resolve the host at dial time", rebound)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("internal server was reached %d times", hits.Load())
	}
}

func TestTemplatedRESTBaseURLRefusesLiteralLoopback(t *testing.T) {
	t.Setenv(allowPrivateIntegrationHostsEnv, "")
	srv, hits := countingServer(t)
	app, tool := wordpressRESTFixture()
	_, err := executeIntegrationTool(app, tool, map[string]string{"site_url": srv.URL}, map[string]any{}, "")
	if !errors.Is(err, errNonPublicIntegrationHost) {
		t.Fatalf("err = %v, want non-public refusal", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("loopback server was reached %d times", hits.Load())
	}
}

// Local dev: a self-hosted WordPress on localhost works once the operator
// opts out explicitly.
func TestTemplatedRESTBaseURLOptOutAllowsLocalhost(t *testing.T) {
	t.Setenv(allowPrivateIntegrationHostsEnv, "1")
	srv, hits := countingServer(t)
	app, tool := wordpressRESTFixture()
	result, err := executeIntegrationTool(app, tool, map[string]string{"site_url": srv.URL}, map[string]any{}, "")
	if err != nil || !result.Success {
		t.Fatalf("opt-out call failed: result=%+v err=%v", result, err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
}

// Fixed vendor base URLs keep the plain transport: no guard lookup, and a
// loopback test server is still reachable.
func TestFixedRESTBaseURLIsNotGuarded(t *testing.T) {
	t.Setenv(allowPrivateIntegrationHostsEnv, "")
	lookups := stubIntegrationHostLookup(t, "127.0.0.1")
	srv, hits := countingServer(t)
	tool := AppToolDef{Name: "ping", Method: "GET", Path: "/ping/{{account_sid}}"}
	app := &AppTemplate{Slug: "fixed-dial-guard-test", Name: "Fixed", BaseURL: srv.URL, Tools: []AppToolDef{tool}}
	result, err := executeIntegrationTool(app, &app.Tools[0], map[string]string{"account_sid": "AC1"}, map[string]any{}, "")
	if err != nil || !result.Success {
		t.Fatalf("fixed call failed: result=%+v err=%v", result, err)
	}
	if hits.Load() != 1 || lookups.Load() != 0 {
		t.Fatalf("hits=%d lookups=%d, want 1 hit and no guard lookup", hits.Load(), lookups.Load())
	}
}

// With an egress proxy the dial targets the proxy, so the request host is
// checked when the proxy is chosen instead.
func TestGuardedTransportWithProxyChecksRequestHost(t *testing.T) {
	t.Setenv(allowPrivateIntegrationHostsEnv, "")
	stubIntegrationHostLookup(t, "169.254.169.254")
	proxy, proxyHits := countingServer(t)
	proxyURL, _ := neturl.Parse(proxy.URL)
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyURL(proxyURL)
	guardIntegrationTransport(tr)
	_, err := (&http.Client{Transport: tr}).Get("http://metadata.rebind.example.test/latest/meta-data")
	if !errors.Is(err, errNonPublicIntegrationHost) {
		t.Fatalf("err = %v, want non-public refusal", err)
	}
	if proxyHits.Load() != 0 {
		t.Fatalf("proxy was reached %d times", proxyHits.Load())
	}
}

func TestRemoteMCPProbeGuardedOnlyForUserSuppliedHosts(t *testing.T) {
	t.Setenv(allowPrivateIntegrationHostsEnv, "")
	srv, hits := countingServer(t)

	if _, err := probeRemoteMCPContext(withUserSuppliedHost(context.Background()), srv.URL+"/mcp", nil); !errors.Is(err, errNonPublicIntegrationHost) {
		t.Fatalf("guarded probe err = %v, want non-public refusal", err)
	}
	if _, err := callRemoteMCPToolContext(withUserSuppliedHost(context.Background()), srv.URL+"/mcp", "x", nil, nil); !errors.Is(err, errNonPublicIntegrationHost) {
		t.Fatalf("guarded call err = %v, want non-public refusal", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("guarded requests reached the server %d times", hits.Load())
	}

	// Unguarded (fixed vendor URL, apps bridge) still dials loopback.
	_, _ = probeRemoteMCPContext(context.Background(), srv.URL+"/mcp", nil)
	if hits.Load() == 0 {
		t.Fatal("unguarded probe never reached the server")
	}
}

func TestMCPManagerStartGuardsRemoteRows(t *testing.T) {
	t.Setenv(allowPrivateIntegrationHostsEnv, "")
	srv, hits := countingServer(t)
	m := NewMCPManager()
	m.guardRemote = func(*MCPServerRecord) bool { return true }
	_, err := m.Start(&MCPServerRecord{ID: 9101, Source: "remote", Transport: "http", URL: srv.URL + "/mcp"}, nil)
	if !errors.Is(err, errNonPublicIntegrationHost) {
		t.Fatalf("start err = %v, want non-public refusal", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("guarded start reached the server %d times", hits.Load())
	}
}

func TestRemoteMCPHostIsUserSupplied(t *testing.T) {
	s := newTestServer(t)
	ensureTestAdmin(t, s)
	s.secret = testSecret()
	s.catalog = NewAppCatalog()
	s.catalog.Register(&AppTemplate{Slug: "wordpress-mcp", Name: "WordPress", Kind: "remote_mcp", MCP: &RemoteMcpConfig{Transport: "http", URL: "{{site_url}}/wp-json/mcp/mcp-adapter-default-server"}})
	s.catalog.Register(&AppTemplate{Slug: "hubspot-mcp", Name: "HubSpot", Kind: "remote_mcp", MCP: &RemoteMcpConfig{Transport: "http", URL: "https://mcp-eu1.hubspot.com/mcp"}})
	creds, _ := json.Marshal(map[string]string{"token": "t"})
	enc, _ := Encrypt(s.secret, string(creds))
	connFor := func(slug string) int64 {
		conn, err := s.store.CreateConnection(1, slug, slug, slug, "api_key", enc, "")
		if err != nil {
			t.Fatal(err)
		}
		return conn.ID
	}
	wp, hub := connFor("wordpress-mcp"), connFor("hubspot-mcp")
	cases := []struct {
		name   string
		record *MCPServerRecord
		want   bool
	}{
		{"templated url", &MCPServerRecord{Source: "remote", ConnectionID: wp}, true},
		{"fixed vendor url", &MCPServerRecord{Source: "remote", ConnectionID: hub}, false},
		{"missing connection", &MCPServerRecord{Source: "remote", ConnectionID: 999999}, true},
		{"remote without connection", &MCPServerRecord{Source: "remote"}, false},
		{"apps bridge", &MCPServerRecord{Source: "app", ConnectionID: wp}, false},
	}
	for _, tc := range cases {
		if got := s.remoteMCPHostIsUserSupplied(tc.record); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
