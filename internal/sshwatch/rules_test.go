package sshwatch

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type fakeHistory struct {
	failures int
	alerted  bool
}

func (f fakeHistory) FailuresSince(string, time.Time) (int, error)      { return f.failures, nil }
func (f fakeHistory) BurstAlertedSince(string, time.Time) (bool, error) { return f.alerted, nil }

func mustPolicy(t *testing.T, p Policy) Policy {
	t.Helper()
	n, err := p.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func rules(alerts []Alert) string {
	var names []string
	for _, a := range alerts {
		names = append(names, a.Rule)
	}
	return strings.Join(names, ",")
}

func TestRules(t *testing.T) {
	base := Policy{ApprovedSources: []string{"10.0.0.0/8", "2001:db8::/32"}, ApprovedFingerprints: []string{sampleFP}}
	rootOK := base
	rootOK.RootLoginAllowed = true
	rootNarrow := rootOK
	rootNarrow.RootApprovedSources = []string{"10.1.0.0/16"}
	accepted := func(user, ip, method, fp string) Event {
		return Event{Kind: KindSSHAuth, Result: ResultAccepted, User: user, SourceIP: ip, Method: method, Fingerprint: fp, ObservedAt: time.Now()}
	}
	failed := Event{Kind: KindSSHAuth, Result: ResultFailed, User: "root", SourceIP: "203.0.113.9", ObservedAt: time.Now()}
	cases := []struct {
		name   string
		policy Policy
		ev     Event
		hist   fakeHistory
		want   string
	}{
		{"known key approved source", base, accepted("deploy", "10.2.3.4", "publickey", sampleFP), fakeHistory{}, ""},
		{"known key unapproved source", base, accepted("deploy", "198.51.100.1", "publickey", sampleFP), fakeHistory{}, RuleUnapprovedSource},
		{"ipv6 approved", base, accepted("deploy", "2001:db8::5", "publickey", sampleFP), fakeHistory{}, ""},
		{"hostname source is unapproved", base, accepted("deploy", "evil.example", "publickey", sampleFP), fakeHistory{}, RuleUnapprovedSource},
		{"unknown key", base, accepted("deploy", "10.2.3.4", "publickey", "SHA256:other"), fakeHistory{}, RuleUnknownKey},
		{"root not allowed", base, accepted("root", "10.2.3.4", "publickey", sampleFP), fakeHistory{}, RuleRootLogin},
		{"root allowed anywhere approved", rootOK, accepted("root", "10.2.3.4", "publickey", sampleFP), fakeHistory{}, ""},
		{"root outside root sources", rootNarrow, accepted("root", "10.2.3.4", "publickey", sampleFP), fakeHistory{}, RuleRootLogin},
		{"root inside root sources", rootNarrow, accepted("root", "10.1.3.4", "publickey", sampleFP), fakeHistory{}, ""},
		{"password", base, accepted("alice", "10.2.3.4", "password", ""), fakeHistory{}, RulePasswordLogin},
		{"keyboard-interactive", base, accepted("alice", "10.2.3.4", "keyboard-interactive/pam", ""), fakeHistory{}, RulePasswordLogin},
		{"everything", Policy{}, accepted("root", "203.0.113.1", "publickey", sampleFP), fakeHistory{}, RuleUnapprovedSource + "," + RuleRootLogin + "," + RuleUnknownKey},
		{"failures below threshold", base, failed, fakeHistory{failures: 8}, ""},
		{"failure burst", base, failed, fakeHistory{failures: 9}, RuleFailureBurst},
		{"failure burst already alerted", base, failed, fakeHistory{failures: 30, alerted: true}, ""},
		{"keys change", base, Event{Kind: KindAuthorizedKeysChange, Result: ResultChanged, KeysRemoved: []AuthorizedKey{{Fingerprint: sampleFP}}}, fakeHistory{}, RuleAuthorizedKeysChange},
		{"keys baseline approved", base, Event{Kind: KindAuthorizedKeysChange, Result: ResultBaseline, KeysAdded: []AuthorizedKey{{Fingerprint: sampleFP}}}, fakeHistory{}, ""},
		{"keys baseline unapproved", base, Event{Kind: KindAuthorizedKeysChange, Result: ResultBaseline, KeysAdded: []AuthorizedKey{{Fingerprint: "SHA256:x"}}}, fakeHistory{}, RuleAuthorizedKeysChange},
		{"gap", base, Event{Kind: KindCollectorGap, Message: "rotated"}, fakeHistory{}, RuleCollectorGap},
		{"session events", base, Event{Kind: KindSSHSession, Result: ResultOpened, User: "root"}, fakeHistory{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			alerts, err := Evaluate(tc.ev, mustPolicy(t, tc.policy), tc.hist)
			if err != nil {
				t.Fatal(err)
			}
			if got := rules(alerts); got != tc.want {
				t.Fatalf("rules = %q, want %q (%+v)", got, tc.want, alerts)
			}
			for _, a := range alerts {
				if a.Severity == "" || a.Message == "" {
					t.Fatalf("incomplete alert %+v", a)
				}
			}
		})
	}
	added, _ := Evaluate(Event{Kind: KindAuthorizedKeysChange, Result: ResultChanged, KeysAdded: []AuthorizedKey{{Fingerprint: "SHA256:x"}}}, mustPolicy(t, base), nil)
	if len(added) != 1 || added[0].Severity != SeverityCritical {
		t.Fatalf("unapproved key added must be critical: %+v", added)
	}
}

func TestPolicyValidation(t *testing.T) {
	p, err := Policy{
		ApprovedSources:      []string{"10.1.2.3/8", "192.0.2.4", " 2001:db8::1/32 ", "::ffff:10.0.0.0/104", "192.0.2.4"},
		ApprovedFingerprints: []string{sampleFP + "=", sampleFP},
		AlertWebhookURL:      "https://alerts.example/hook",
		AlertWebhookSecret:   "s",
	}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.ApprovedSources, " ") != "10.0.0.0/8 192.0.2.4/32 2001:db8::/32" {
		t.Fatalf("sources %v", p.ApprovedSources)
	}
	if len(p.ApprovedFingerprints) != 1 || p.ApprovedFingerprints[0] != sampleFP {
		t.Fatalf("fingerprints %v", p.ApprovedFingerprints)
	}
	if p.FailureBurstThreshold != DefaultFailureBurstThreshold || p.FailureBurstWindowSeconds != DefaultFailureBurstWindowSeconds {
		t.Fatalf("defaults not applied: %+v", p)
	}
	if r := p.Redacted(); r.AlertWebhookSecret != "" || !r.AlertWebhookSecretSet {
		t.Fatalf("not redacted: %+v", r)
	}
	bad := []Policy{
		{ApprovedSources: []string{"10.0.0.0/33"}},
		{ApprovedSources: []string{"not-an-ip"}},
		{RootApprovedSources: []string{"fe80::1%eth0"}},
		{ApprovedFingerprints: []string{"MD5:aa:bb"}},
		{ApprovedFingerprints: []string{"SHA256:short"}},
		{FailureBurstThreshold: -1},
		{FailureBurstWindowSeconds: 5},
		{AlertWebhookURL: "http://alerts.example/hook"},
		{AlertWebhookURL: "ftp://alerts.example/hook"},
		{AlertWebhookURL: "https://user:pw@alerts.example/hook"},
	}
	for _, b := range bad {
		if _, err := b.Normalize(); err == nil {
			t.Fatalf("accepted invalid policy %+v", b)
		}
	}
	if _, err := (Policy{AlertWebhookURL: "http://127.0.0.1:9/hook"}).Normalize(); err != nil {
		t.Fatalf("loopback http rejected: %v", err)
	}
	cleared, _ := Policy{AlertWebhookSecret: "orphan"}.Normalize()
	if cleared.AlertWebhookSecret != "" {
		t.Fatal("secret without webhook retained")
	}
}

func newKey(t *testing.T) (line string, fingerprint string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk))), ssh.FingerprintSHA256(pk)
}

func TestAuthorizedKeysDiff(t *testing.T) {
	k1, fp1 := newKey(t)
	k2, fp2 := newKey(t)
	k3, fp3 := newKey(t)
	before := ParseAuthorizedKeys([]byte("# comment\n" + k1 + " alice@laptop\n" + k2 + " ci\nnot a key\n" + k2 + " duplicate\n"))
	if len(before) != 2 {
		t.Fatalf("parsed %d keys: %+v", len(before), before)
	}
	for _, k := range before {
		if strings.Contains(k.Comment, "AAAA") || k.Type != ssh.KeyAlgoED25519 {
			t.Fatalf("key material retained or wrong type: %+v", k)
		}
	}
	after := ParseAuthorizedKeys([]byte(`from="10.0.0.0/8",command="/bin/backup --token=secret" ` + k2 + " ci\n" + k3 + "\n"))
	added, removed := DiffAuthorizedKeys(before, after)
	fps := func(keys []AuthorizedKey) string {
		var out []string
		for _, k := range keys {
			out = append(out, k.Fingerprint)
		}
		return strings.Join(out, ",")
	}
	// k2 changed options: removed+added; k1 removed; k3 added.
	if !strings.Contains(fps(added), fp2) || !strings.Contains(fps(added), fp3) || len(added) != 2 {
		t.Fatalf("added %v", fps(added))
	}
	if !strings.Contains(fps(removed), fp1) || !strings.Contains(fps(removed), fp2) || len(removed) != 2 {
		t.Fatalf("removed %v", fps(removed))
	}
	for _, k := range after {
		if k.Fingerprint == fp2 && (k.OptionsHash == "" || strings.Contains(k.OptionsHash, "secret")) {
			t.Fatalf("options digest wrong: %+v", k)
		}
	}
	// Comment-only edits do not change the access digest.
	if keysDigest(ParseAuthorizedKeys([]byte(k1+" a\n"))) != keysDigest(ParseAuthorizedKeys([]byte(k1+" b\n"))) {
		t.Fatal("comment change altered digest")
	}
	if userForKeyPath("/home/bob/.ssh/authorized_keys") != "bob" || userForKeyPath("/root/.ssh/authorized_keys") != "root" || userForKeyPath("/etc/ssh/authorized_keys/carol") != "carol" {
		t.Fatal("user derivation")
	}
}
