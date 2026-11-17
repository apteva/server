package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSidecarLogWriteFailureKeepsDraining(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stderr.log")
	logf, err := newRotatingLogFile(path, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer logf.Close()
	if _, err := logf.Write([]byte(strings.Repeat("x", 32))); err != nil {
		t.Fatal(err)
	}

	// A nonempty directory at the rotation destination blocks rotation
	// without exhausting disk space.
	if err := os.Mkdir(path+".1", 0700); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(path+".1", "block")
	if err := os.WriteFile(blocker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	var failures []error
	writer := &sidecarLogWriter{sink: logf, report: func(err error) {
		failures = append(failures, err)
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "head", "-c", "1048576", "/dev/zero")
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := cmd.Run(); err != nil {
		t.Fatalf("child failed while logging was unavailable: %v", err)
	}
	if len(failures) != 1 {
		t.Fatalf("reported %d failures, want one per outage", len(failures))
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("recovered")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "recovered" {
		t.Fatalf("log did not recover: %q, %v", got, err)
	}
}

func TestRotatingLogFileCapsGrowth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stderr.log")
	logf, err := newRotatingLogFile(path, 32)
	if err != nil {
		t.Fatalf("newRotatingLogFile: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := logf.Write([]byte("0123456789")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if err := logf.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	assertFileAtMost(t, path, 32)
	assertFileAtMost(t, path+".1", 32)
}

func TestRotatingLogFileKeepsTailOfHugeWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stderr.log")
	logf, err := newRotatingLogFile(path, 8)
	if err != nil {
		t.Fatalf("newRotatingLogFile: %v", err)
	}
	n, err := logf.Write([]byte("0123456789abcdef"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 16 {
		t.Fatalf("write reported %d bytes consumed, want 16", n)
	}
	if err := logf.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(got) != "89abcdef" {
		t.Fatalf("log tail mismatch: got %q", got)
	}
}

func TestRotatingLogFileRotatesExistingOversizedLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stderr.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 40)), 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	logf, err := newRotatingLogFile(path, 32)
	if err != nil {
		t.Fatalf("newRotatingLogFile: %v", err)
	}
	if err := logf.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	assertFileAtMost(t, path, 32)
	assertFileAtMost(t, path+".1", 32)
	got, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read rotated log: %v", err)
	}
	if string(got) != strings.Repeat("x", 32) {
		t.Fatalf("rotated log should keep tail: got %d bytes", len(got))
	}
}

func assertFileAtMost(t *testing.T, path string, max int64) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Size() > max {
		t.Fatalf("%s is %d bytes, want <= %d", path, info.Size(), max)
	}
}
