package sshwatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingSink struct {
	mu    sync.Mutex
	lines []Line
	gaps  []Gap
}

func (s *recordingSink) Line(_ context.Context, l Line) error {
	s.mu.Lock()
	s.lines = append(s.lines, l)
	s.mu.Unlock()
	return nil
}

func (s *recordingSink) Gap(_ context.Context, g Gap) error {
	s.mu.Lock()
	s.gaps = append(s.gaps, g)
	s.mu.Unlock()
	return nil
}

func (s *recordingSink) snapshot() ([]Line, []Gap) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Line(nil), s.lines...), append([]Gap(nil), s.gaps...)
}

func (s *recordingSink) sshMessages() []string {
	lines, _ := s.snapshot()
	var out []string
	for _, l := range lines {
		if isSSHIdent(l.Ident) {
			out = append(out, l.Message)
		}
	}
	return out
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// runSource runs src until want ssh records (and gaps) arrived, then stops it.
func runSource(t *testing.T, src Source, cursor string, wantSSH, wantGaps int) *recordingSink {
	t.Helper()
	sink := &recordingSink{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- src.Run(ctx, cursor, sink) }()
	eventually(t, "source records", func() bool {
		_, gaps := sink.snapshot()
		return len(sink.sshMessages()) >= wantSSH && len(gaps) >= wantGaps
	})
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done
	return sink
}

func lastCursor(s *recordingSink) string {
	lines, _ := s.snapshot()
	return lines[len(lines)-1].Cursor
}

func authLine(ts, msg string) string {
	return ts + " host1 sshd[100]: " + msg + "\n"
}

func TestFileSourceRotationAndTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.log")
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	src := &FileSource{Path: path, Poll: 10 * time.Millisecond, Backfill: 7 * 24 * time.Hour, Now: func() time.Time { return now }}
	content := authLine("2026-10-01T00:00:00Z", "Failed password for root from 192.0.2.99 port 1 ssh2") + // older than backfill
		authLine("2026-11-16T10:00:00Z", "Failed password for root from 192.0.2.1 port 1 ssh2") +
		"2026-11-16T10:00:01Z host1 CRON[5]: pam_unix(cron:session): session opened for user root\n" +
		authLine("2026-11-16T10:00:02Z", "Accepted publickey for root from 192.0.2.2 port 2 ssh2: ED25519 "+sampleFP)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	first := runSource(t, src, "", 2, 0)
	if got := first.sshMessages(); len(got) != 2 || !strings.Contains(got[0], "192.0.2.1") {
		t.Fatalf("first run %q", got)
	}
	cursor := lastCursor(first)

	// Lines written to the old file after our read, then rotation.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString(authLine("2026-11-16T10:01:00Z", "Failed password for root from 192.0.2.3 port 3 ssh2"))
	f.Close()
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte(authLine("2026-11-16T10:02:00Z", "Failed password for root from 192.0.2.4 port 4 ssh2")), 0600)
	second := runSource(t, src, cursor, 2, 0)
	if got := second.sshMessages(); len(got) != 2 || !strings.Contains(got[0], "192.0.2.3") || !strings.Contains(got[1], "192.0.2.4") {
		t.Fatalf("after rotation %q", got)
	}
	if _, gaps := second.snapshot(); len(gaps) != 0 {
		t.Fatalf("unexpected gaps %+v", gaps)
	}
	// Re-reading the same position yields the same dedupe key.
	again := runSource(t, src, cursor, 2, 0)
	l1, _ := second.snapshot()
	l2, _ := again.snapshot()
	if l1[0].Key == "" || l1[0].Key != l2[0].Key {
		t.Fatalf("keys differ on re-read: %q vs %q", l1[0].Key, l2[0].Key)
	}
	cursor = lastCursor(second)

	// Rotated file gone (compressed/deleted) => gap.
	os.Remove(path + ".1")
	os.Rename(path, path+".1")
	os.Remove(path + ".1")
	os.WriteFile(path, []byte(authLine("2026-11-16T11:00:00Z", "Failed password for root from 192.0.2.5 port 5 ssh2")), 0600)
	third := runSource(t, src, cursor, 1, 1)
	if _, gaps := third.snapshot(); !strings.Contains(gaps[0].Reason, "rotated") {
		t.Fatalf("gaps %+v", gaps)
	}
	cursor = lastCursor(third)

	// Truncation in place => gap.
	os.WriteFile(path, []byte("x\n"), 0600)
	fourth := runSource(t, src, cursor, 0, 1)
	if _, gaps := fourth.snapshot(); !strings.Contains(gaps[0].Reason, "truncated") {
		t.Fatalf("gaps %+v", gaps)
	}

	// Malformed cursor => gap, then backfill from the start.
	fifth := runSource(t, src, "garbage", 0, 1)
	if _, gaps := fifth.snapshot(); !strings.Contains(gaps[0].Reason, "malformed") {
		t.Fatalf("gaps %+v", gaps)
	}
}

// fakeJournalctl is a shell stand-in for journalctl; real journald is never used.
func fakeJournalctl(t *testing.T) (binary, argsLog string) {
	t.Helper()
	dir := t.TempDir()
	argsLog = filepath.Join(dir, "args.log")
	record := func(cursor, us, msg string) string {
		return fmt.Sprintf(`{"__CURSOR":"%s","__REALTIME_TIMESTAMP":"%s","_HOSTNAME":"h","SYSLOG_IDENTIFIER":"sshd","_PID":"10","MESSAGE":"%s"}`, cursor, us, msg)
	}
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> '" + argsLog + "'\n" +
		"for a in \"$@\"; do case \"$a\" in\n" +
		"  --cursor=bad*) echo 'Failed to seek to cursor: Invalid argument' >&2; exit 1;;\n" +
		"  --cursor=s=present*) printf '%s\\n' '" + record("s=present;t=5f0000000000", "1700000000000000", "Failed password for root from 192.0.2.1 port 1 ssh2") + "'; exit 0;;\n" +
		"  --cursor=*) printf '%s\\n' '" + record("s=newer;t=5f0000000001", "1700000000000001", "Failed password for root from 192.0.2.2 port 2 ssh2") + "'; exit 0;;\n" +
		"esac; done\n" +
		"printf '%s\\n' '" + record("s=newer;t=5f0000000001", "1700000000000001", "Failed password for root from 192.0.2.2 port 2 ssh2") + "'\n" +
		"exec sleep 30\n"
	binary = filepath.Join(dir, "journalctl")
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return binary, argsLog
}

func TestJournalSourceCursorHandling(t *testing.T) {
	binary, argsLog := fakeJournalctl(t)
	now := time.Unix(1700000100, 0)
	src := &JournalSource{Binary: binary, Backfill: time.Hour, Now: func() time.Time { return now }}
	followArgs := func() string {
		b, _ := os.ReadFile(argsLog)
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if strings.Contains(lines[i], "--follow") {
				return lines[i]
			}
		}
		return ""
	}

	// Cursor still present: resume after it, no gap.
	s := runSource(t, src, "s=present;t=5f0000000000", 1, 0)
	if _, gaps := s.snapshot(); len(gaps) != 0 || !strings.Contains(followArgs(), "--after-cursor=s=present") {
		t.Fatalf("gaps %+v args %q", gaps, followArgs())
	}
	if !strings.Contains(followArgs(), "SYSLOG_IDENTIFIER=sshd SYSLOG_IDENTIFIER=sshd-session") {
		t.Fatalf("missing sshd filters: %q", followArgs())
	}

	// Cursor vacuumed: gap bounded by the cursor's realtime, resume at first surviving entry.
	s = runSource(t, src, "s=vacuumed;t=5e0000000000", 1, 1)
	_, gaps := s.snapshot()
	if !strings.Contains(gaps[0].Reason, "not found") || gaps[0].From.IsZero() || !strings.Contains(followArgs(), "--cursor=s=newer") {
		t.Fatalf("gaps %+v args %q", gaps, followArgs())
	}

	// Malformed cursor: gap, resume by time.
	s = runSource(t, src, "bad-cursor", 1, 1)
	_, gaps = s.snapshot()
	if !strings.Contains(gaps[0].Reason, "rejected") || !strings.Contains(followArgs(), "--since=@") {
		t.Fatalf("gaps %+v args %q", gaps, followArgs())
	}

	// First run: backfill window.
	runSource(t, src, "", 1, 0)
	if want := fmt.Sprintf("--since=@%d", now.Add(-time.Hour).Unix()); !strings.Contains(followArgs(), want) {
		t.Fatalf("backfill args %q want %q", followArgs(), want)
	}
}
