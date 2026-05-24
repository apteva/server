package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// rotatingLogFile is the stdout/stderr sink for local app sidecars.
// It keeps at most one rotated copy, so a noisy app cannot grow an
// unbounded stderr.log while still leaving recent crash output behind.
type rotatingLogFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	file     *os.File
	size     int64
	closed   bool
}

func newRotatingLogFile(path string, maxBytes int64) (*rotatingLogFile, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("log max bytes must be positive")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err == nil && info.Size() >= maxBytes {
		if err := rotateOversizedLogPath(path, maxBytes); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	size := int64(0)
	if info, err := f.Stat(); err == nil {
		size = info.Size()
	}
	return &rotatingLogFile{path: path, maxBytes: maxBytes, file: f, size: size}, nil
}

func (l *rotatingLogFile) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, os.ErrClosed
	}
	consumed := len(p)
	if int64(len(p)) > l.maxBytes {
		p = p[len(p)-int(l.maxBytes):]
	}
	if l.size+int64(len(p)) > l.maxBytes {
		if err := l.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := l.file.Write(p)
	l.size += int64(n)
	if err != nil {
		return n, err
	}
	if n != len(p) {
		return n, fmt.Errorf("short log write")
	}
	return consumed, nil
}

func (l *rotatingLogFile) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	return l.file.Close()
}

func (l *rotatingLogFile) rotateLocked() error {
	if err := l.file.Close(); err != nil {
		return err
	}
	if err := rotateLogPath(l.path); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	l.file = f
	l.size = 0
	return nil
}

func rotateLogPath(path string) error {
	rotated := path + ".1"
	_ = os.Remove(rotated)
	if _, err := os.Stat(path); err == nil {
		return os.Rename(path, rotated)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func rotateOversizedLogPath(path string, keepBytes int64) error {
	rotated := path + ".1"
	tmp := rotated + ".tmp"
	_ = os.Remove(tmp)
	_ = os.Remove(rotated)

	in, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	info, err := in.Stat()
	if err != nil {
		in.Close()
		return err
	}
	start := info.Size() - keepBytes
	if start < 0 {
		start = 0
	}
	if _, err := in.Seek(start, io.SeekStart); err != nil {
		in.Close()
		return err
	}
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		in.Close()
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeOutErr := out.Close()
	closeInErr := in.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeOutErr != nil {
		_ = os.Remove(tmp)
		return closeOutErr
	}
	if closeInErr != nil {
		_ = os.Remove(tmp)
		return closeInErr
	}
	if err := os.Rename(tmp, rotated); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Remove(path)
}
