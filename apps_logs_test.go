package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
