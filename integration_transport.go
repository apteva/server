package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Transport pools belong to a network/TLS identity, never to one tool call.
// Credentials are hashed in the key and are never logged.
var integrationTransports = struct {
	sync.Mutex
	entries map[[32]byte]*http.Transport
	order   [][32]byte
}{entries: make(map[[32]byte]*http.Transport)}

func integrationHTTPClient(app *AppTemplate, credentials map[string]string, timeout time.Duration) (*http.Client, error) {
	proxy, _, err := integrationProxyURL(app)
	if err != nil {
		return nil, err
	}
	var cert, key string
	if mtls := app.Auth.MTLS; mtls != nil {
		cf, kf := strings.TrimSpace(mtls.CertField), strings.TrimSpace(mtls.KeyField)
		if cf == "" {
			cf = "client_certificate_pem"
		}
		if kf == "" {
			kf = "client_private_key_pem"
		}
		cert, key = normalizeCredentialPEM(credentials[cf]), normalizeCredentialPEM(credentials[kf])
	}
	encoded, _ := json.Marshal([]string{fmt.Sprintf("%p", http.DefaultTransport), proxy, cert, key})
	identity := sha256.Sum256(encoded)
	integrationTransports.Lock()
	defer integrationTransports.Unlock()
	if tr := integrationTransports.entries[identity]; tr != nil {
		return &http.Client{Timeout: timeout, Transport: tr}, nil
	}
	client, err := newIntegrationHTTPClient(app, credentials, timeout)
	if err != nil {
		return nil, err
	}
	tr := client.Transport.(*http.Transport)
	tr.MaxIdleConns = 128
	tr.MaxIdleConnsPerHost = 16
	tr.MaxConnsPerHost = 64
	if len(integrationTransports.order) >= 64 {
		old := integrationTransports.order[0]
		integrationTransports.entries[old].CloseIdleConnections()
		delete(integrationTransports.entries, old)
		integrationTransports.order = integrationTransports.order[1:]
	}
	integrationTransports.entries[identity] = tr
	integrationTransports.order = append(integrationTransports.order, identity)
	return client, nil
}

// Header deadlines bound a stalled backend without imposing a total lifetime
// on telemetry streams. Per-request context owns cancellation.
var coreProxyClient = &http.Client{Transport: func() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 15 * time.Second
	tr.MaxIdleConns = 256
	tr.MaxIdleConnsPerHost = 8
	return tr
}()}
