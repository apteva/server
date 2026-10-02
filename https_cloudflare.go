package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

func (n *instanceHTTPSManager) cloudflareToken(c instanceHTTPSConfig) (string, error) {
	if c.TokenEncrypted != "" {
		return Decrypt(n.server.secret, c.TokenEncrypted)
	}
	if c.ConnectionID == 0 {
		return "", errors.New("choose an existing Cloudflare connection or supply a token with Zone Read and DNS Edit for this domain")
	}
	conn, encrypted, err := n.server.store.GetConnection(c.OwnerID, c.ConnectionID)
	if err != nil || conn == nil || conn.AppSlug != "cloudflare" || conn.Status != "active" || isAppOwnedConnection(*conn) {
		return "", errors.New("the selected Cloudflare connection is unavailable or not owned by this administrator")
	}
	raw, err := Decrypt(n.server.secret, encrypted)
	if err != nil {
		return "", err
	}
	var creds map[string]string
	if err := json.Unmarshal([]byte(raw), &creds); err != nil {
		return "", err
	}
	if creds["token"] == "" {
		return "", errors.New("the Cloudflare connection has no API token")
	}
	return creds["token"], nil
}
func cloudflareHTTPSRequest(ctx context.Context, token, method, path string, body any, out any) error {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.cloudflare.com/client/v4"+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("could not reach the Cloudflare API")
	}
	defer resp.Body.Close()
	var envelope struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
		Errors  []struct {
			Code int `json:"code"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&envelope); err != nil {
		return fmt.Errorf("Cloudflare returned an invalid response (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode >= 300 || !envelope.Success {
		return fmt.Errorf("Cloudflare rejected %s (HTTP %d); check the token's zone and DNS permissions", strings.Split(path, "?")[0], resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(envelope.Result, out)
	}
	return nil
}
func cloudflareHTTPSZone(ctx context.Context, token, hostname string) (string, error) {
	for name := hostname; strings.Contains(name, "."); name = name[strings.Index(name, ".")+1:] {
		var zones []struct{ ID, Name string }
		if err := cloudflareHTTPSRequest(ctx, token, "GET", "/zones?name="+url.QueryEscape(name), nil, &zones); err != nil {
			return "", err
		}
		for _, zone := range zones {
			if zone.Name == name {
				return zone.ID, nil
			}
		}
	}
	return "", errors.New("no accessible Cloudflare zone covers this hostname; the token needs Zone Read and DNS Edit")
}
func (n *instanceHTTPSManager) prepareCloudflare(ctx context.Context, c *instanceHTTPSConfig) error {
	token, err := n.cloudflareToken(*c)
	if err != nil {
		return err
	}
	zone, err := cloudflareHTTPSZone(ctx, token, c.Hostname)
	if err != nil {
		return err
	}
	needsIssue := true
	if c.CertificateEncrypted != "" {
		raw, err := Decrypt(n.server.secret, c.CertificateEncrypted)
		if err == nil {
			pair, err := validateInstanceCertificate(c.Hostname, raw, raw)
			if err == nil && time.Until(pair.Leaf.NotAfter) > 30*24*time.Hour {
				needsIssue = false
			}
		}
	}
	if needsIssue {
		bundle, err := n.issueCloudflareCertificate(ctx, *c, token, zone)
		if err != nil {
			return err
		}
		encrypted, err := Encrypt(n.server.secret, bundle)
		if err != nil {
			return err
		}
		c.CertificateEncrypted = encrypted
		// Retain a successfully issued certificate even if strict-mode or public
		// reachability checks fail, avoiding unnecessary issuance on retry.
		n.mu.Lock()
		if n.state.Pending != nil && n.state.Pending.Hostname == c.Hostname {
			copy := *c
			n.state.Pending = &copy
		} else if n.state.Active != nil && n.state.Active.Hostname == c.Hostname {
			copy := *c
			n.state.Active = &copy
		}
		err = n.saveLocked()
		n.mu.Unlock()
		if err != nil {
			return err
		}
	}
	if c.SetStrict {
		if err := cloudflareHTTPSRequest(ctx, token, "PATCH", "/zones/"+zone+"/settings/ssl", map[string]string{"value": "strict"}, nil); err != nil {
			return fmt.Errorf("certificate is ready, but switching the Cloudflare zone to Full (strict) failed: %w. Set it in Cloudflare and retry without this option", err)
		}
		c.SetStrict = false
	}
	var ssl struct {
		Value string `json:"value"`
	}
	if err := cloudflareHTTPSRequest(ctx, token, "GET", "/zones/"+zone+"/settings/ssl", nil, &ssl); err != nil {
		n.warning("Cloudflare SSL mode could not be read with this token. Confirm Full (strict) in Cloudflare; origin and public HTTPS are checked separately.")
	} else if ssl.Value != "strict" {
		return errors.New("the certificate is ready. Set Cloudflare SSL/TLS to Full (strict), then retry. Alternatively allow this setup to change the zone setting")
	}
	ranges, err := cloudflareHTTPSProxyRanges(ctx)
	if err != nil {
		return err
	}
	c.TrustedProxies = ranges
	return nil
}

// DNS validation uses the same ACME protocol as native ingress. Only the
// challenge adapter differs; certificates still terminate in Apteva's TLS
// listener, and renewal is owned by the server.
func (n *instanceHTTPSManager) issueCloudflareCertificate(ctx context.Context, c instanceHTTPSConfig, token, zone string) (string, error) {
	directory := strings.TrimSpace(os.Getenv("APTEVA_ACME_DIRECTORY_URL"))
	if directory == "" {
		directory = autocert.DefaultACMEDirectory
	}
	var key *ecdsa.PrivateKey
	if encrypted := n.server.store.GetSetting("instance_https_acme_account"); encrypted != "" {
		raw, err := Decrypt(n.server.secret, encrypted)
		if err != nil {
			return "", err
		}
		block, _ := pem.Decode([]byte(raw))
		if block == nil {
			return "", errors.New("ACME account key is invalid")
		}
		parsed, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return "", err
		}
		key = parsed
	} else {
		var err error
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return "", err
		}
		der, _ := x509.MarshalECPrivateKey(key)
		encrypted, err := Encrypt(n.server.secret, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})))
		if err != nil {
			return "", err
		}
		if err := n.server.store.SetSetting("instance_https_acme_account", encrypted); err != nil {
			return "", err
		}
	}
	client := &acme.Client{Key: key, DirectoryURL: directory, HTTPClient: &http.Client{Timeout: 30 * time.Second}}
	account := &acme.Account{}
	if c.Email != "" {
		account.Contact = []string{"mailto:" + c.Email}
	}
	if _, err := client.Register(ctx, account, func(string) bool { return c.AcceptTerms }); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return "", fmt.Errorf("ACME account registration: %w", err)
	}
	order, err := client.AuthorizeOrder(ctx, []acme.AuthzID{{Type: "dns", Value: c.Hostname}})
	if err != nil {
		return "", err
	}
	for _, authURL := range order.AuthzURLs {
		auth, err := client.GetAuthorization(ctx, authURL)
		if err != nil {
			return "", err
		}
		if auth.Status == acme.StatusValid {
			continue
		}
		var challenge *acme.Challenge
		for _, ch := range auth.Challenges {
			if ch.Type == "dns-01" {
				challenge = ch
				break
			}
		}
		if challenge == nil {
			return "", errors.New("certificate authority did not offer DNS validation")
		}
		value, err := client.DNS01ChallengeRecord(challenge.Token)
		if err != nil {
			return "", err
		}
		recordName := "_acme-challenge." + c.Hostname
		// Do not follow a delegated CNAME into an unrelated zone.
		if canonical, err := net.DefaultResolver.LookupCNAME(ctx, recordName); err == nil && !strings.EqualFold(strings.TrimSuffix(canonical, "."), recordName) {
			return "", fmt.Errorf("%s is delegated by CNAME; remove the delegation or use the existing certificate option", recordName)
		}
		var record struct {
			ID string `json:"id"`
		}
		if err := cloudflareHTTPSRequest(ctx, token, "POST", "/zones/"+zone+"/dns_records", map[string]any{"type": "TXT", "name": recordName, "content": value, "ttl": 60}, &record); err != nil {
			return "", err
		}
		if record.ID == "" {
			return "", errors.New("Cloudflare did not return the challenge record ID")
		}
		defer func(id string) {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := cloudflareHTTPSRequest(cleanup, token, "DELETE", "/zones/"+zone+"/dns_records/"+url.PathEscape(id), nil, nil); err != nil {
				n.warning("A temporary ACME TXT record could not be removed; check _acme-challenge." + c.Hostname)
			}
		}(record.ID)
		n.phase("dns_validation", "Waiting for the temporary DNS validation record")
		for {
			txts, err := net.DefaultResolver.LookupTXT(ctx, recordName)
			found := false
			if err == nil {
				for _, txt := range txts {
					if txt == value {
						found = true
					}
				}
			}
			if found {
				break
			}
			select {
			case <-ctx.Done():
				return "", errors.New("DNS validation record did not propagate in time; retry after DNS caches expire")
			case <-time.After(5 * time.Second):
			}
		}
		if _, err := client.Accept(ctx, challenge); err != nil {
			return "", err
		}
		if _, err := client.WaitAuthorization(ctx, authURL); err != nil {
			return "", fmt.Errorf("DNS validation failed: %w", err)
		}
	}
	ready, err := client.WaitOrder(ctx, order.URI)
	if err != nil {
		return "", err
	}
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: c.Hostname}, DNSNames: []string{c.Hostname}}, certKey)
	if err != nil {
		return "", err
	}
	chain, _, err := client.CreateOrderCert(ctx, ready.FinalizeURL, csr, true)
	if err != nil {
		return "", err
	}
	keyDER, _ := x509.MarshalECPrivateKey(certKey)
	bundle := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	for _, der := range chain {
		bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	if _, err := validateInstanceCertificate(c.Hostname, string(bundle), string(bundle)); err != nil {
		return "", err
	}
	return string(bundle), nil
}

func cloudflareHTTPSProxyRanges(ctx context.Context) ([]string, error) {
	var ranges struct {
		IPv4 []string `json:"ipv4_cidrs"`
		IPv6 []string `json:"ipv6_cidrs"`
	}
	if err := cloudflareHTTPSRequest(ctx, "", "GET", "/ips", nil, &ranges); err != nil {
		return nil, err
	}
	all := append(ranges.IPv4, ranges.IPv6...)
	if len(all) == 0 {
		return nil, errors.New("Cloudflare returned no trusted proxy ranges")
	}
	for _, raw := range all {
		p, err := netip.ParsePrefix(raw)
		if err != nil || p.Bits() == 0 {
			return nil, errors.New("Cloudflare returned an invalid proxy range")
		}
	}
	return all, nil
}
