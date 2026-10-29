package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Hosts that come from a connection's credentials (a customer's own
// WordPress site, a self-hosted API) are checked when the connection is
// created, but DNS can change afterwards and templated REST base URLs are
// not checked at all. The guard below runs at dial time instead: it
// resolves the host, drops every non-public address and dials one of the
// vetted IPs directly, so a later rebinding cannot reach loopback, private
// or metadata addresses (127.0.0.1:5280, 169.254.169.254).
//
// Fixed vendor URLs never go through it. Set
// APTEVA_ALLOW_PRIVATE_INTEGRATION_HOSTS=1 to turn it off, e.g. for a
// self-hosted WordPress on http://localhost during development.
const allowPrivateIntegrationHostsEnv = "APTEVA_ALLOW_PRIVATE_INTEGRATION_HOSTS"

var errNonPublicIntegrationHost = errors.New("resolves only to non-public addresses")

func privateIntegrationHostsAllowed() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(allowPrivateIntegrationHostsEnv))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

var cgnatRange = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}
var thisNetworkRange = &net.IPNet{IP: net.IPv4(0, 0, 0, 0), Mask: net.CIDRMask(8, 32)}

// isNonPublicIP reports loopback, private, link-local, unspecified,
// multicast, CGNAT (100.64/10) and 0/8 addresses, including their
// IPv4-mapped IPv6 forms.
func isNonPublicIP(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || cgnatRange.Contains(ip) || thisNetworkRange.Contains(ip)
}

// integrationHostLookup is swapped by tests to simulate rebinding.
var integrationHostLookup = net.DefaultResolver.LookupIPAddr

func publicIntegrationHostIPs(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := integrationHostLookup(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, fmt.Errorf("integration host %s cannot be resolved", host)
	}
	var public []net.IP
	for _, addr := range addrs {
		if !isNonPublicIP(addr.IP) {
			public = append(public, addr.IP)
		}
	}
	if len(public) == 0 {
		return nil, fmt.Errorf("integration host %s %w", host, errNonPublicIntegrationHost)
	}
	return public, nil
}

// guardedIntegrationDialContext dials only public addresses of the target
// host. The address checked is the address dialed, so there is no window
// between check and connect.
func guardedIntegrationDialContext(dialer *net.Dialer) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if privateIntegrationHostsAllowed() {
			return dialer.DialContext(ctx, network, address)
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := publicIntegrationHostIPs(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range ips {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
}

// guardIntegrationTransport applies the guard to a transport. With an egress
// proxy the dial goes to the operator's proxy, which may well be local, so
// the request host is checked when the proxy is chosen instead.
func guardIntegrationTransport(tr *http.Transport) {
	if tr.Proxy != nil {
		proxy := tr.Proxy
		tr.Proxy = func(req *http.Request) (*neturl.URL, error) {
			if !privateIntegrationHostsAllowed() {
				if _, err := publicIntegrationHostIPs(req.Context(), req.URL.Hostname()); err != nil {
					return nil, err
				}
			}
			return proxy(req)
		}
		return
	}
	tr.DialContext = guardedIntegrationDialContext(&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second})
}

// integrationURLHostIsTemplated reports whether a URL template takes its
// scheme or host from credentials ("{{site_url}}/wp-json/wp/v2",
// "https://{{subdomain}}.example.com"). Placeholders in the path only do
// not move the request to another host.
func integrationURLHostIsTemplated(template string) bool {
	authority := template
	if i := strings.Index(authority, "://"); i >= 0 {
		if strings.Contains(authority[:i], "{{") {
			return true
		}
		authority = authority[i+3:]
	}
	if i := strings.Index(authority, "/"); i >= 0 {
		authority = authority[:i]
	}
	return strings.Contains(authority, "{{")
}

type userSuppliedHostKey struct{}

// withUserSuppliedHost marks a remote MCP request whose URL came from
// connection credentials; remoteMCPHTTPClient then guards its dials.
func withUserSuppliedHost(ctx context.Context) context.Context {
	return context.WithValue(ctx, userSuppliedHostKey{}, true)
}

var guardedRemoteMCPTransport = sync.OnceValue(func() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	guardIntegrationTransport(tr)
	return tr
})

// remoteMCPHTTPClient returns the probe / tools/call client. Redirects are
// re-issued by the caller, so they dial through the same transport.
func remoteMCPHTTPClient(ctx context.Context, timeout time.Duration) *http.Client {
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if guarded, _ := ctx.Value(userSuppliedHostKey{}).(bool); guarded {
		client.Transport = guardedRemoteMCPTransport()
	}
	return client
}

// remoteMCPHostIsUserSupplied reports whether a remote MCP row's URL was
// resolved from its connection's credentials. Rows whose connection or
// catalog entry cannot be found are treated as user supplied.
func (s *Server) remoteMCPHostIsUserSupplied(record *MCPServerRecord) bool {
	if record == nil || record.Source != "remote" || record.ConnectionID == 0 {
		return false
	}
	if s.store == nil || s.catalog == nil {
		return true
	}
	var slug string
	if err := s.store.db.QueryRow("SELECT app_slug FROM connections WHERE id = ?", record.ConnectionID).Scan(&slug); err != nil {
		return true
	}
	app := s.catalog.Get(slug)
	if app == nil || app.MCP == nil {
		return true
	}
	return integrationURLHostIsTemplated(app.MCP.URL)
}

func (s *Server) remoteMCPContext(ctx context.Context, record *MCPServerRecord) context.Context {
	if s.remoteMCPHostIsUserSupplied(record) {
		return withUserSuppliedHost(ctx)
	}
	return ctx
}
