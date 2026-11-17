package sshwatch

import (
	"strings"
	"testing"
	"time"
)

const sampleFP = "SHA256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU"

func TestParseMessage(t *testing.T) {
	cases := []struct {
		name, msg                      string
		ok                             bool
		kind, result, user, ip, method string
		port                           int
		keyType, fingerprint           string
	}{
		{name: "accepted publickey", msg: "Accepted publickey for root from 1.2.3.4 port 51234 ssh2: ED25519 " + sampleFP,
			ok: true, kind: KindSSHAuth, result: ResultAccepted, user: "root", ip: "1.2.3.4", port: 51234, method: "publickey", keyType: "ED25519", fingerprint: sampleFP},
		{name: "accepted certificate", msg: "Accepted publickey for deploy from 10.0.0.9 port 40000 ssh2: ED25519-CERT " + sampleFP + " ID ci-runner from 9.9.9.9 port 1 (serial 7) CA ED25519 SHA256:abc",
			ok: true, kind: KindSSHAuth, result: ResultAccepted, user: "deploy", ip: "10.0.0.9", port: 40000, method: "publickey", keyType: "ED25519-CERT", fingerprint: sampleFP},
		{name: "accepted ipv6", msg: "Accepted publickey for git from 2001:db8::1 port 2222 ssh2: RSA " + sampleFP,
			ok: true, kind: KindSSHAuth, result: ResultAccepted, user: "git", ip: "2001:db8::1", port: 2222, method: "publickey", keyType: "RSA", fingerprint: sampleFP},
		{name: "accepted ipv4-mapped", msg: "Accepted password for alice from ::ffff:192.0.2.10 port 2222 ssh2",
			ok: true, kind: KindSSHAuth, result: ResultAccepted, user: "alice", ip: "192.0.2.10", port: 2222, method: "password"},
		{name: "accepted password", msg: "Accepted password for alice from 10.0.0.5 port 2222 ssh2",
			ok: true, kind: KindSSHAuth, result: ResultAccepted, user: "alice", ip: "10.0.0.5", port: 2222, method: "password"},
		{name: "accepted keyboard-interactive", msg: "Accepted keyboard-interactive/pam for bob from 10.0.0.6 port 2223 ssh2",
			ok: true, kind: KindSSHAuth, result: ResultAccepted, user: "bob", ip: "10.0.0.6", port: 2223, method: "keyboard-interactive/pam"},
		{name: "failed password", msg: "Failed password for root from 1.2.3.4 port 22 ssh2",
			ok: true, kind: KindSSHAuth, result: ResultFailed, user: "root", ip: "1.2.3.4", port: 22, method: "password"},
		{name: "failed password invalid user", msg: "Failed password for invalid user admin from 1.2.3.4 port 41234 ssh2",
			ok: true, kind: KindSSHAuth, result: ResultInvalidUser, user: "admin", ip: "1.2.3.4", port: 41234, method: "password"},
		{name: "failed publickey", msg: "Failed publickey for root from 2001:db8::7 port 22 ssh2: ECDSA " + sampleFP,
			ok: true, kind: KindSSHAuth, result: ResultFailed, user: "root", ip: "2001:db8::7", port: 22, method: "publickey", keyType: "ECDSA", fingerprint: sampleFP},
		{name: "invalid user", msg: "Invalid user admin from 1.2.3.4 port 41234",
			ok: true, kind: KindSSHAuth, result: ResultInvalidUser, user: "admin", ip: "1.2.3.4", port: 41234},
		{name: "invalid user no port", msg: "Invalid user oracle from 198.51.100.4",
			ok: true, kind: KindSSHAuth, result: ResultInvalidUser, user: "oracle", ip: "198.51.100.4"},
		{name: "invalid user spoofed source", msg: "Invalid user x from 6.6.6.6 port 1 from 1.2.3.4 port 5",
			ok: true, kind: KindSSHAuth, result: ResultInvalidUser, user: "x from 6.6.6.6 port 1", ip: "1.2.3.4", port: 5},
		{name: "connection closed authenticating", msg: "Connection closed by authenticating user root 1.2.3.4 port 51234 [preauth]",
			ok: true, kind: KindSSHAuth, result: ResultFailed, user: "root", ip: "1.2.3.4", port: 51234},
		{name: "connection closed invalid", msg: "Connection closed by invalid user test 2001:db8::2 port 60000 [preauth]",
			ok: true, kind: KindSSHAuth, result: ResultInvalidUser, user: "test", ip: "2001:db8::2", port: 60000},
		{name: "pam session opened", msg: "pam_unix(sshd:session): session opened for user root(uid=0) by (uid=0)",
			ok: true, kind: KindSSHSession, result: ResultOpened, user: "root"},
		{name: "pam session opened old", msg: "pam_unix(sshd:session): session opened for user deploy by (uid=0)",
			ok: true, kind: KindSSHSession, result: ResultOpened, user: "deploy"},
		{name: "pam session closed", msg: "pam_unix(sshd:session): session closed for user root",
			ok: true, kind: KindSSHSession, result: ResultClosed, user: "root"},
		{name: "disconnected user", msg: "Disconnected from user root 1.2.3.4 port 51234",
			ok: true, kind: KindSSHSession, result: ResultDisconnected, user: "root", ip: "1.2.3.4", port: 51234},
		{name: "disconnected authenticating", msg: "Disconnected from authenticating user root 1.2.3.4 port 51234 [preauth]",
			ok: true, kind: KindSSHAuth, result: ResultFailed, user: "root", ip: "1.2.3.4", port: 51234},
		{name: "disconnected invalid", msg: "Disconnected from invalid user ubnt 1.2.3.4 port 3 [preauth]",
			ok: true, kind: KindSSHAuth, result: ResultInvalidUser, user: "ubnt", ip: "1.2.3.4", port: 3},
		{name: "max auth tries", msg: "error: maximum authentication attempts exceeded for root from 1.2.3.4 port 22 ssh2 [preauth]",
			ok: true, kind: KindSSHAuth, result: ResultFailed, user: "root", ip: "1.2.3.4", port: 22},
		{name: "not allowed", msg: "User bob from 1.2.3.4 not allowed because not listed in AllowUsers",
			ok: true, kind: KindSSHAuth, result: ResultFailed, user: "bob", ip: "1.2.3.4"},
		{name: "received disconnect ignored", msg: "Received disconnect from 1.2.3.4 port 51234:11: disconnected by user", ok: false},
		{name: "server listening ignored", msg: "Server listening on 0.0.0.0 port 22.", ok: false},
		{name: "preauth close without user ignored", msg: "Connection closed by 1.2.3.4 port 5 [preauth]", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := ParseMessage(tc.msg)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			got := []any{ev.Kind, ev.Result, ev.User, ev.SourceIP, ev.SourcePort, ev.Method, ev.KeyType, ev.Fingerprint}
			want := []any{tc.kind, tc.result, tc.user, tc.ip, tc.port, tc.method, tc.keyType, tc.fingerprint}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("field %d = %v, want %v (event %+v)", i, got[i], want[i], ev)
				}
			}
			if ev.Message == "" {
				t.Fatal("message missing")
			}
		})
	}
}

func TestSanitizeMessage(t *testing.T) {
	got := SanitizeMessage("Invalid user a\x1b[31mb from 1.2.3.4 password=hunter2 token=abc")
	if strings.ContainsRune(got, 0x1b) || strings.Contains(got, "hunter2") || strings.Contains(got, "abc") {
		t.Fatalf("not sanitized: %q", got)
	}
	long := SanitizeMessage(strings.Repeat("é", 1000))
	if len(long) > maxMessageLen+len("…") || !strings.HasSuffix(long, "…") {
		t.Fatalf("not truncated: %d", len(long))
	}
}

func TestParseSyslogLine(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	l, ok := ParseSyslogLine("Dec 31 23:59:58 web1 sshd[812]: Accepted publickey for root from 1.2.3.4 port 5 ssh2: ED25519 "+sampleFP, now)
	if !ok || l.Hostname != "web1" || l.Ident != "sshd" || l.PID != 812 || !strings.HasPrefix(l.Message, "Accepted publickey") {
		t.Fatalf("classic: %+v %v", l, ok)
	}
	if l.Time.Year() != 2025 {
		t.Fatalf("year rollover not handled: %v", l.Time)
	}
	l, ok = ParseSyslogLine("2026-11-16T10:00:00.123456+00:00 web1 sshd-session[99]: Failed password for root from 1.2.3.4 port 5 ssh2", now)
	if !ok || l.Ident != "sshd-session" || l.PID != 99 || !l.Time.Equal(time.Date(2026, 11, 16, 10, 0, 0, 123456000, time.UTC)) {
		t.Fatalf("rfc3339: %+v %v", l, ok)
	}
	if _, ok := ParseSyslogLine("garbage", now); ok {
		t.Fatal("garbage parsed")
	}
}

func TestParseJournalRecord(t *testing.T) {
	l, err := ParseJournalRecord([]byte(`{"__CURSOR":"s=a;i=1;t=5f","__REALTIME_TIMESTAMP":"1700000000123456","_HOSTNAME":"h","SYSLOG_IDENTIFIER":"sshd-session","_PID":"77","MESSAGE":"Accepted password for a from 1.2.3.4 port 5 ssh2"}`))
	if err != nil || l.Cursor != "s=a;i=1;t=5f" || l.Ident != "sshd-session" || l.PID != 77 || l.Hostname != "h" || !l.Time.Equal(time.UnixMicro(1700000000123456)) {
		t.Fatalf("%+v %v", l, err)
	}
	l, err = ParseJournalRecord([]byte(`{"__CURSOR":"c","MESSAGE":[73,110,118,97,108,105,100,32,117,115,101,114,32,255,32,102,114,111,109,32,49,46,50,46,51,46,52]}`))
	if err != nil || !strings.HasPrefix(l.Message, "Invalid user ") {
		t.Fatalf("byte array message: %+v %v", l, err)
	}
	if ev, ok := ParseMessage(l.Message); !ok || ev.SourceIP != "1.2.3.4" || strings.ContainsRune(ev.User, 0xff) {
		t.Fatalf("binary message parse: %+v", ev)
	}
	if _, err := ParseJournalRecord([]byte(`{"MESSAGE":"x"}`)); err == nil {
		t.Fatal("record without cursor accepted")
	}
	if got := journalCursorTime("s=abc;i=1;b=x;m=1;t=60a7b2c3d4e5f;x=9"); got.IsZero() {
		t.Fatal("cursor time not parsed")
	}
}
