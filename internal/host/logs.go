package host

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// The stable host, rather than a replaceable server slot, owns copy/rotation.
// Children inherit an actual pipe FD; exec.Cmd.Wait does not wait for copy
// goroutines or close that FD while surviving cores are still writing to it.
type processLog struct {
	reader, writer *os.File
	done           chan struct{}
}

type logSink struct {
	writer     *boundedLog
	references int
}

func (h *Host) logPipe(path string, maxBytes int64) (*processLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	h.logsMu.Lock()
	if h.logSinks == nil {
		h.logSinks = map[string]*logSink{}
	}
	sink := h.logSinks[path]
	if sink == nil {
		writer, err := newBoundedLog(path, maxBytes)
		if err != nil {
			h.logsMu.Unlock()
			return nil, err
		}
		sink = &logSink{writer: writer}
		h.logSinks[path] = sink
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		if sink.references == 0 {
			sink.writer.Close()
			delete(h.logSinks, path)
		}
		h.logsMu.Unlock()
		return nil, err
	}
	sink.references++
	pipe := &processLog{reader: reader, writer: writer, done: make(chan struct{})}
	h.logs = append(h.logs, pipe)
	h.logsMu.Unlock()
	go func() {
		defer close(pipe.done)
		defer reader.Close()
		defer func() {
			h.logsMu.Lock()
			defer h.logsMu.Unlock()
			sink.references--
			if sink.references == 0 {
				sink.writer.Close()
				delete(h.logSinks, path)
			}
		}()
		_, _ = io.Copy(sink.writer, reader)
	}()
	return pipe, nil
}
func (h *Host) closeLogs() {
	h.logsMu.Lock()
	logs := append([]*processLog(nil), h.logs...)
	h.logsMu.Unlock()
	for _, log := range logs {
		_ = log.writer.Close()
		_ = log.reader.Close()
		<-log.done
	}
}

type boundedLog struct {
	mu          sync.Mutex
	file        *os.File
	path        string
	size, limit int64
}

func newBoundedLog(path string, limit int64) (*boundedLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	s, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &boundedLog{file: f, path: path, size: s.Size(), limit: limit}, nil
}
func (l *boundedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size > 0 && l.size+int64(len(p)) > l.limit {
		if err := l.file.Close(); err != nil {
			return 0, err
		}
		_ = os.Remove(l.path + ".2")
		_ = os.Rename(l.path+".1", l.path+".2")
		if err := os.Rename(l.path, l.path+".1"); err != nil {
			return 0, fmt.Errorf("rotate process log: %w", err)
		}
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return 0, err
		}
		l.file = f
		l.size = 0
	}
	n, err := l.file.Write(p)
	l.size += int64(n)
	return n, err
}
func (l *boundedLog) Close() error { l.mu.Lock(); defer l.mu.Unlock(); return l.file.Close() }
