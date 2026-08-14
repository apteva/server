package main

import (
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	maxminddb "github.com/oschwald/maxminddb-golang/v2"
)

const (
	geoCountryHeader         = "X-Apteva-Country"
	geoCountryDBEnv          = "APTEVA_GEOIP_COUNTRY_DB"
	trustedProxyCIDRsEnv     = "APTEVA_TRUSTED_PROXY_CIDRS"
	geoCountryReloadInterval = time.Minute
)

// countryLookup is deliberately tiny so routing tests can exercise the trust
// boundary without carrying a binary MMDB fixture. The production
// implementation below keeps one thread-safe, memory-mapped Reader open.
type countryLookup interface {
	Country(netip.Addr) (string, bool)
	Close() error
}

type maxMindCountryLookup struct {
	path string

	mu      sync.RWMutex
	reader  *maxminddb.Reader
	modTime time.Time

	lastReloadCheck atomic.Int64
}

type maxMindCountryRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
}

var trustedProxyPrefixCache struct {
	sync.RWMutex
	raw      string
	prefixes []netip.Prefix
}

func newMaxMindCountryLookup(path string) (*maxMindCountryLookup, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("GeoIP country database path is empty")
	}
	reader, err := maxminddb.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		_ = reader.Close()
		return nil, err
	}
	return &maxMindCountryLookup{path: path, reader: reader, modTime: info.ModTime()}, nil
}

// geoCountryLookupFromEnv is fail-open by design. A missing or temporarily
// invalid optional database must never prevent Apteva from serving traffic.
func geoCountryLookupFromEnv() countryLookup {
	path := strings.TrimSpace(os.Getenv(geoCountryDBEnv))
	if path == "" {
		return nil
	}
	lookup, err := newMaxMindCountryLookup(path)
	if err != nil {
		log.Printf("[GEOIP] country lookup disabled: %v", err)
		return nil
	}
	lookup.lastReloadCheck.Store(time.Now().UnixNano())
	log.Printf("[GEOIP] country database loaded path=%s type=%s build=%s",
		path, lookup.reader.Metadata.DatabaseType, lookup.reader.Metadata.BuildTime().UTC().Format(time.RFC3339))
	return lookup
}

func (g *maxMindCountryLookup) Country(ip netip.Addr) (string, bool) {
	if g == nil || !ip.IsValid() || ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() {
		return "", false
	}
	g.maybeReload()

	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.reader == nil {
		return "", false
	}
	result := g.reader.Lookup(ip.Unmap())
	if !result.Found() || result.Err() != nil {
		return "", false
	}
	var record maxMindCountryRecord
	if err := result.Decode(&record); err != nil {
		return "", false
	}
	return normalizeCountryCode(record.Country.ISOCode)
}

// maybeReload notices the atomic file replacement performed by geoipupdate.
// Stat runs at most once per minute; ordinary requests only pay one atomic load.
func (g *maxMindCountryLookup) maybeReload() {
	now := time.Now()
	last := g.lastReloadCheck.Load()
	if last != 0 && now.Sub(time.Unix(0, last)) < geoCountryReloadInterval {
		return
	}
	if !g.lastReloadCheck.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	info, err := os.Stat(g.path)
	if err != nil {
		return
	}

	g.mu.RLock()
	unchanged := !info.ModTime().After(g.modTime)
	g.mu.RUnlock()
	if unchanged {
		return
	}

	reader, err := maxminddb.Open(g.path)
	if err != nil {
		log.Printf("[GEOIP] country database reload failed: %v", err)
		return
	}
	g.mu.Lock()
	old := g.reader
	g.reader = reader
	g.modTime = info.ModTime()
	g.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	log.Printf("[GEOIP] country database reloaded type=%s build=%s",
		reader.Metadata.DatabaseType, reader.Metadata.BuildTime().UTC().Format(time.RFC3339))
}

func (g *maxMindCountryLookup) Close() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.reader == nil {
		return nil
	}
	err := g.reader.Close()
	g.reader = nil
	return err
}

func normalizeCountryCode(raw string) (string, bool) {
	code := strings.ToUpper(strings.TrimSpace(raw))
	if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
		return "", false
	}
	return code, true
}

func remoteRequestIP(r *http.Request) (netip.Addr, bool) {
	if r == nil {
		return netip.Addr{}, false
	}
	raw := strings.TrimSpace(r.RemoteAddr)
	if addrPort, err := netip.ParseAddrPort(raw); err == nil {
		return addrPort.Addr().Unmap(), true
	}
	ip, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

func configuredTrustedProxyPrefixes() []netip.Prefix {
	raw := strings.TrimSpace(os.Getenv(trustedProxyCIDRsEnv))
	trustedProxyPrefixCache.RLock()
	if trustedProxyPrefixCache.raw == raw {
		prefixes := trustedProxyPrefixCache.prefixes
		trustedProxyPrefixCache.RUnlock()
		return prefixes
	}
	trustedProxyPrefixCache.RUnlock()

	prefixes := make([]netip.Prefix, 0)
	for _, value := range strings.Split(raw, ",") {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			continue
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	trustedProxyPrefixCache.Lock()
	trustedProxyPrefixCache.raw = raw
	trustedProxyPrefixCache.prefixes = prefixes
	trustedProxyPrefixCache.Unlock()
	return prefixes
}

func trustedProxyAddress(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	if ip.IsLoopback() {
		return true
	}
	for _, prefix := range configuredTrustedProxyPrefixes() {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

func requestFromConfiguredProxy(r *http.Request) bool {
	ip, ok := remoteRequestIP(r)
	return ok && trustedProxyAddress(ip) && !ip.IsLoopback()
}

// implicitLocalProxyHop reports addresses that can occur between a public
// visitor and a reverse proxy connected to Server over loopback. Local tunnel
// clients such as zrok commonly append both their private overlay address and
// 127.0.0.1 to X-Forwarded-For. Those addresses cannot identify an Internet
// visitor and must not stop the trusted-chain walk.
//
// This is deliberately limited to a loopback peer. A remotely connected proxy
// must declare every trusted intermediary with APTEVA_TRUSTED_PROXY_CIDRS, so
// an arbitrary client cannot make Server walk past an untrusted private hop.
func implicitLocalProxyHop(ip, peer netip.Addr) bool {
	if !ip.IsValid() || !peer.IsValid() || !peer.Unmap().IsLoopback() {
		return false
	}
	ip = ip.Unmap()
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// resolvedClientIP walks a trusted proxy chain from the server backwards and
// returns the first untrusted hop. This resists a browser prepending a forged
// X-Forwarded-For value, while retaining the legacy all-proxies switch for
// existing installations that already isolate the listener at the network.
func resolvedClientIP(r *http.Request) string {
	peer, peerOK := remoteRequestIP(r)
	if !peerOK {
		if r == nil {
			return ""
		}
		return strings.TrimSpace(r.RemoteAddr)
	}
	forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if forwarded == "" || !trustForwardedHeaders(r) {
		return peer.String()
	}

	parts := strings.Split(forwarded, ",")
	if envTruthy(os.Getenv("APTEVA_TRUST_PROXY_HEADERS")) && len(configuredTrustedProxyPrefixes()) == 0 && !peer.IsLoopback() {
		for _, part := range parts {
			if ip, err := netip.ParseAddr(strings.TrimSpace(part)); err == nil {
				return ip.Unmap().String()
			}
		}
		return peer.String()
	}
	var nearestImplicitHop netip.Addr
	for i := len(parts) - 1; i >= 0; i-- {
		ip, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			continue
		}
		ip = ip.Unmap()
		if trustedProxyAddress(ip) {
			continue
		}
		if implicitLocalProxyHop(ip, peer) {
			// Preserve the old result when a local/private address is the only
			// usable forwarded address (for example, a LAN-only deployment).
			if !nearestImplicitHop.IsValid() {
				nearestImplicitHop = ip
			}
			continue
		}
		return ip.String()
	}
	if nearestImplicitHop.IsValid() {
		return nearestImplicitHop.String()
	}
	return peer.String()
}

// applyGeoCountryHeader owns the X-Apteva-Country trust boundary. It always
// removes caller input first, then adds a server-derived ISO code when GeoIP is
// configured and a public client address resolves.
func (s *Server) applyGeoCountryHeader(dst http.Header, source *http.Request) {
	dst.Del(geoCountryHeader)
	if s == nil || s.geoCountry == nil || source == nil {
		return
	}
	ip, err := netip.ParseAddr(clientIP(source))
	if err != nil {
		return
	}
	if country, ok := s.geoCountry.Country(ip.Unmap()); ok {
		dst.Set(geoCountryHeader, country)
	}
}
