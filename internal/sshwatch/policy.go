package sshwatch

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	maxPolicyEntries = 4096
	policyFileName   = "policy.json"
)

// ValidationError reports an invalid policy field.
type ValidationError struct{ Field, Reason string }

func (e *ValidationError) Error() string { return e.Field + ": " + e.Reason }

// Normalize validates p and returns a canonical copy: sources become masked
// prefixes, fingerprints are de-duplicated and defaults are applied.
func (p Policy) Normalize() (Policy, error) {
	out := Policy{
		RootLoginAllowed:          p.RootLoginAllowed,
		FailureBurstThreshold:     p.FailureBurstThreshold,
		FailureBurstWindowSeconds: p.FailureBurstWindowSeconds,
		AlertWebhookURL:           strings.TrimSpace(p.AlertWebhookURL),
		AlertWebhookSecret:        p.AlertWebhookSecret,
	}
	var err error
	if out.ApprovedSources, err = normalizePrefixes("approved_sources", p.ApprovedSources); err != nil {
		return Policy{}, err
	}
	if out.RootApprovedSources, err = normalizePrefixes("root_approved_sources", p.RootApprovedSources); err != nil {
		return Policy{}, err
	}
	if len(p.ApprovedFingerprints) > maxPolicyEntries {
		return Policy{}, &ValidationError{"approved_fingerprints", "too many entries"}
	}
	seen := map[string]bool{}
	out.ApprovedFingerprints = []string{}
	for _, raw := range p.ApprovedFingerprints {
		fp, err := normalizeFingerprint(raw)
		if err != nil {
			return Policy{}, &ValidationError{"approved_fingerprints", err.Error()}
		}
		if !seen[fp] {
			seen[fp] = true
			out.ApprovedFingerprints = append(out.ApprovedFingerprints, fp)
		}
	}
	sort.Strings(out.ApprovedFingerprints)
	if out.FailureBurstThreshold == 0 {
		out.FailureBurstThreshold = DefaultFailureBurstThreshold
	}
	if out.FailureBurstThreshold < 1 || out.FailureBurstThreshold > 100000 {
		return Policy{}, &ValidationError{"failure_burst_threshold", "must be between 1 and 100000"}
	}
	if out.FailureBurstWindowSeconds == 0 {
		out.FailureBurstWindowSeconds = DefaultFailureBurstWindowSeconds
	}
	if out.FailureBurstWindowSeconds < 10 || out.FailureBurstWindowSeconds > 7*86400 {
		return Policy{}, &ValidationError{"failure_burst_window_seconds", "must be between 10 and 604800"}
	}
	if out.AlertWebhookURL != "" {
		if err := validateWebhookURL(out.AlertWebhookURL); err != nil {
			return Policy{}, &ValidationError{"alert_webhook_url", err.Error()}
		}
	} else {
		out.AlertWebhookSecret = ""
	}
	if len(out.AlertWebhookSecret) > 4096 {
		return Policy{}, &ValidationError{"alert_webhook_secret", "too long"}
	}
	return out, nil
}

func normalizePrefixes(field string, in []string) ([]string, error) {
	if len(in) > maxPolicyEntries {
		return nil, &ValidationError{field, "too many entries"}
	}
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range in {
		raw = strings.TrimSpace(raw)
		var prefix netip.Prefix
		if strings.Contains(raw, "/") {
			p, err := netip.ParsePrefix(raw)
			if err != nil {
				return nil, &ValidationError{field, fmt.Sprintf("invalid CIDR %q", raw)}
			}
			if p.Addr().Is4In6() {
				if p.Bits() < 96 {
					return nil, &ValidationError{field, fmt.Sprintf("invalid CIDR %q", raw)}
				}
				p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
			}
			prefix = p.Masked()
		} else {
			a, err := netip.ParseAddr(raw)
			if err != nil || a.Zone() != "" {
				return nil, &ValidationError{field, fmt.Sprintf("invalid IP address %q", raw)}
			}
			a = a.Unmap()
			prefix = netip.PrefixFrom(a, a.BitLen())
		}
		if s := prefix.String(); !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out, nil
}

// normalizeFingerprint accepts "SHA256:<base64>" (padded or not) and returns
// OpenSSH's unpadded form.
func normalizeFingerprint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	b64, ok := strings.CutPrefix(raw, "SHA256:")
	if !ok {
		return "", fmt.Errorf("fingerprint %q must start with SHA256:", raw)
	}
	b64 = strings.TrimRight(b64, "=")
	b, err := base64.RawStdEncoding.DecodeString(b64)
	if err != nil || len(b) != sha256.Size {
		return "", fmt.Errorf("fingerprint %q is not a SHA256 digest", raw)
	}
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(b), nil
}

func validateWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("must be an absolute http(s) URL without credentials")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
		return errors.New("plain http is only allowed for loopback receivers; use https")
	default:
		return errors.New("must use https")
	}
}

// Redacted returns a copy safe to return from read APIs.
func (p Policy) Redacted() Policy {
	p.AlertWebhookSecretSet = p.AlertWebhookSecret != ""
	p.AlertWebhookSecret = ""
	return p
}

func (p Policy) digest() string {
	p.AlertWebhookSecretSet = false
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func prefixesContain(prefixes []string, ip string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap().WithZone("")
	for _, s := range prefixes {
		if p, err := netip.ParsePrefix(s); err == nil && p.Contains(addr) {
			return true
		}
	}
	return false
}

func readPolicyFile(path string) (Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Policy{}, err
	}
	var p Policy
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("policy %s: %w", path, err)
	}
	return p.Normalize()
}

func writePolicyFile(path string, p Policy) error {
	p.AlertWebhookSecretSet = false
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'), 0600)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
