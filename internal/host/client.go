package host

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/apteva/server/internal/sshwatch"
)

// UnixClient returns an HTTP client for a host or slot Unix socket. Requests
// carry the host control token automatically: APTEVA_HOST_CONTROL_TOKEN when
// set, else control.token next to the socket.
func UnixClient(socket string) *http.Client {
	base := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &http.Client{Transport: &controlTokenTransport{base: base, socket: socket}, Timeout: 30 * time.Second}
}

type controlTokenTransport struct {
	base   *http.Transport
	socket string
}

func (t *controlTokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Header.Get(ControlTokenHeader) == "" {
		if token := clientControlToken(t.socket); token != "" {
			r = r.Clone(r.Context())
			r.Header.Set(ControlTokenHeader, token)
		}
	}
	return t.base.RoundTrip(r)
}

func (t *controlTokenTransport) CloseIdleConnections() { t.base.CloseIdleConnections() }

// clientControlToken is resolved per request so a token rotated by a host
// restart is picked up without rebuilding clients.
func clientControlToken(socket string) string {
	if token := strings.TrimSpace(os.Getenv(ControlTokenEnv)); token != "" {
		return token
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(socket), controlTokenFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func Call(ctx context.Context, client *http.Client, method, path string, body, output any) error {
	var data io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		data = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://host"+path, data)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("host %s: HTTP %d: %s", path, resp.StatusCode, b)
	}
	if output != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(output)
	}
	return nil
}

// SecurityEvents pages SSH security events with Seq > after (limit <= 0
// selects the host default of 500; the host caps it at 2000).
func SecurityEvents(ctx context.Context, client *http.Client, after int64, limit int) (sshwatch.EventsPage, error) {
	q := url.Values{}
	q.Set("after", strconv.FormatInt(after, 10))
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var page sshwatch.EventsPage
	err := Call(ctx, client, http.MethodGet, "/security/events?"+q.Encode(), nil, &page)
	return page, err
}

// SecurityHealth returns the SSH collector health.
func SecurityHealth(ctx context.Context, client *http.Client) (sshwatch.Health, error) {
	var h sshwatch.Health
	err := Call(ctx, client, http.MethodGet, "/security/health", nil, &h)
	return h, err
}

// GetSecurityPolicy returns the SSH monitoring policy (webhook secret redacted).
func GetSecurityPolicy(ctx context.Context, client *http.Client) (sshwatch.Policy, error) {
	var p sshwatch.Policy
	err := Call(ctx, client, http.MethodGet, "/security/policy", nil, &p)
	return p, err
}

// PutSecurityPolicy validates, persists and applies a policy; it returns the
// applied policy (webhook secret redacted). An empty AlertWebhookSecret keeps
// the stored secret unless AlertWebhookURL is cleared.
func PutSecurityPolicy(ctx context.Context, client *http.Client, policy sshwatch.Policy) (sshwatch.Policy, error) {
	var p sshwatch.Policy
	err := Call(ctx, client, http.MethodPut, "/security/policy", policy, &p)
	return p, err
}
