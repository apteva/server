package sshwatch

import (
	"fmt"
	"strings"
	"time"
)

// History answers the stateful questions of the failure-burst rule from
// already-stored events, so the rule survives restarts and ignores replays.
type History interface {
	// FailuresSince counts stored failed/invalid_user ssh_auth events from ip
	// observed at or after since.
	FailuresSince(ip string, since time.Time) (int, error)
	// BurstAlertedSince reports whether a failure_burst alert for ip was
	// raised for an event observed at or after since.
	BurstAlertedSince(ip string, since time.Time) (bool, error)
}

// Evaluate applies the deterministic rules to ev (which is not yet stored).
// It never calls a model or the network.
func Evaluate(ev Event, p Policy, h History) ([]Alert, error) {
	var alerts []Alert
	add := func(rule, severity, format string, args ...any) {
		alerts = append(alerts, Alert{Rule: rule, Severity: severity, Message: fmt.Sprintf(format, args...)})
	}
	from := ev.SourceIP
	if from == "" {
		from = "unknown source"
	}
	switch ev.Kind {
	case KindSSHAuth:
		switch ev.Result {
		case ResultAccepted:
			// (a) Approval of the source is independent of the credential: a
			// known key used from an unexpected network is still an alert.
			if !prefixesContain(p.ApprovedSources, ev.SourceIP) {
				add(RuleUnapprovedSource, SeverityHigh, "SSH login for %s from unapproved source %s (%s)", ev.User, from, methodLabel(ev))
			}
			// (b) Root logins must be explicitly allowed; RootApprovedSources
			// narrows where from (empty means any source).
			if ev.User == "root" {
				if !p.RootLoginAllowed {
					add(RuleRootLogin, SeverityCritical, "root SSH login from %s while root login is not allowed", from)
				} else if len(p.RootApprovedSources) > 0 && !prefixesContain(p.RootApprovedSources, ev.SourceIP) {
					add(RuleRootLogin, SeverityCritical, "root SSH login from %s outside root approved sources", from)
				}
			}
			// (c) Public keys must be on the approved list.
			if ev.Method == "publickey" && !containsString(p.ApprovedFingerprints, ev.Fingerprint) {
				fp := ev.Fingerprint
				if fp == "" {
					fp = "unknown fingerprint"
				}
				add(RuleUnknownKey, SeverityHigh, "SSH login for %s from %s with unapproved key %s %s", ev.User, from, ev.KeyType, fp)
			}
			// (d) Interactive secrets are never an expected login method.
			if ev.Method == "password" || strings.HasPrefix(ev.Method, "keyboard-interactive") {
				add(RulePasswordLogin, SeverityHigh, "SSH %s login for %s from %s", ev.Method, ev.User, from)
			}
		case ResultFailed, ResultInvalidUser:
			// (f) Failure burst from one source within the window. The event
			// being evaluated is not stored yet, hence +1.
			if ev.SourceIP == "" || h == nil || p.FailureBurstThreshold < 1 {
				break
			}
			window := time.Duration(p.FailureBurstWindowSeconds) * time.Second
			since := ev.ObservedAt.Add(-window)
			n, err := h.FailuresSince(ev.SourceIP, since)
			if err != nil {
				return nil, err
			}
			if n+1 >= p.FailureBurstThreshold {
				already, err := h.BurstAlertedSince(ev.SourceIP, since)
				if err != nil {
					return nil, err
				}
				if !already {
					add(RuleFailureBurst, SeverityMedium, "%d failed SSH attempts from %s within %s", n+1, ev.SourceIP, window)
				}
			}
		}
	case KindAuthorizedKeysChange:
		// (e) Any change to authorized_keys. A baseline inventory alerts only
		// when it contains keys that are not approved.
		if ev.Result == ResultBaseline {
			var unapproved []string
			for _, k := range ev.KeysAdded {
				if !containsString(p.ApprovedFingerprints, k.Fingerprint) {
					unapproved = append(unapproved, k.Fingerprint)
				}
			}
			if len(unapproved) > 0 {
				add(RuleAuthorizedKeysChange, SeverityMedium, "%s contains %d unapproved key(s) at first inspection: %s", ev.Path, len(unapproved), strings.Join(limitStrings(unapproved, 5), ", "))
			}
			break
		}
		severity := SeverityMedium
		if len(ev.KeysAdded) > 0 {
			severity = SeverityHigh
			for _, k := range ev.KeysAdded {
				if !containsString(p.ApprovedFingerprints, k.Fingerprint) {
					severity = SeverityCritical
				}
			}
		}
		add(RuleAuthorizedKeysChange, severity, "%s changed: %d key(s) added, %d removed", ev.Path, len(ev.KeysAdded), len(ev.KeysRemoved))
	case KindCollectorGap:
		add(RuleCollectorGap, SeverityMedium, "SSH log coverage gap: %s", ev.Message)
	case KindPolicyChanged:
		add(RulePolicyChanged, SeverityMedium, "SSH monitoring policy changed")
	}
	return alerts, nil
}

func methodLabel(ev Event) string {
	if ev.Method == "publickey" && ev.Fingerprint != "" {
		return "publickey " + ev.Fingerprint
	}
	if ev.Method == "" {
		return "unknown method"
	}
	return ev.Method
}

func containsString(list []string, s string) bool {
	if s == "" {
		return false
	}
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func limitStrings(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	return append(append([]string(nil), in[:n]...), fmt.Sprintf("… %d more", len(in)-n))
}
