package sshwatch

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Gap reports that a source could not resume from its stored cursor (journal
// vacuumed/rotated, log file rotated away or truncated, malformed cursor).
type Gap struct {
	Reason string
	From   time.Time
	To     time.Time
	Cursor string
}

// Sink receives records from a Source, in order.
type Sink interface {
	Line(ctx context.Context, l Line) error
	Gap(ctx context.Context, g Gap) error
}

// Source streams sshd records starting after cursor ("" means: start from
// the backfill window). Run blocks until ctx is done or the source fails.
type Source interface {
	Name() string
	Run(ctx context.Context, cursor string, sink Sink) error
}

const maxRecordBytes = 64 << 10

// JournalSource follows journald via `journalctl --output=json --follow`.
type JournalSource struct {
	Binary      string
	Backfill    time.Duration
	Identifiers []string
	Now         func() time.Time

	mu      sync.Mutex
	warning string
}

func (s *JournalSource) Name() string { return "journal" }

// Warning returns a non-fatal access problem (e.g. no systemd-journal group).
func (s *JournalSource) Warning() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.warning
}

func (s *JournalSource) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *JournalSource) matches() []string {
	ids := s.Identifiers
	if len(ids) == 0 {
		ids = []string{"sshd", "sshd-session", "sshd-auth"}
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		// Matches on the same field are ORed by journalctl.
		out[i] = "SYSLOG_IDENTIFIER=" + id
	}
	return out
}

func (s *JournalSource) since(t time.Time) string {
	return "--since=@" + strconv.FormatInt(t.Unix(), 10)
}

func (s *JournalSource) Run(ctx context.Context, cursor string, sink Sink) error {
	args := []string{"--output=json", "--no-pager", "--all", "--follow"}
	switch {
	case cursor == "":
		args = append(args, s.since(s.now().Add(-s.Backfill)))
	default:
		from := journalCursorTime(cursor)
		first, err := s.first(ctx, cursor)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		switch {
		case err != nil:
			// Malformed or foreign cursor: resume by time and report the gap.
			if err := sink.Gap(ctx, Gap{Reason: "journal rejected the stored cursor: " + err.Error(), From: from, To: s.now(), Cursor: cursor}); err != nil {
				return err
			}
			start := from
			if start.IsZero() || start.Before(s.now().Add(-s.Backfill)) {
				start = s.now().Add(-s.Backfill)
			}
			args = append(args, s.since(start))
		case first == nil:
			// Nothing at or after the cursor although the cursor's own entry
			// would be: it was vacuumed and nothing newer is retained yet.
			if err := sink.Gap(ctx, Gap{Reason: "journal cursor not found (rotated or vacuumed)", From: from, To: s.now(), Cursor: cursor}); err != nil {
				return err
			}
			args = append(args, "--after-cursor="+cursor)
		case first.Cursor != cursor:
			if err := sink.Gap(ctx, Gap{Reason: "journal cursor not found (rotated or vacuumed)", From: from, To: first.Time, Cursor: cursor}); err != nil {
				return err
			}
			args = append(args, "--cursor="+first.Cursor)
		default:
			args = append(args, "--after-cursor="+cursor)
		}
	}
	args = append(args, s.matches()...)
	cmd := exec.CommandContext(ctx, s.Binary, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := &limitedBuffer{max: 4096}
	cmd.Stderr = stderr
	cmd.WaitDelay = 5 * time.Second
	if err = cmd.Start(); err != nil {
		return err
	}
	readErr := s.read(ctx, stdout, sink)
	if readErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	s.noteAccess(stderr.String())
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if readErr != nil {
		return readErr
	}
	if waitErr != nil {
		return fmt.Errorf("journalctl: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	return errors.New("journalctl exited")
}

func (s *JournalSource) noteAccess(stderr string) {
	if strings.Contains(stderr, "insufficient permissions") || strings.Contains(stderr, "not seeing messages from other users") {
		s.mu.Lock()
		s.warning = "journal access is limited; add the collector user to the systemd-journal group"
		s.mu.Unlock()
	}
}

func (s *JournalSource) read(ctx context.Context, r io.Reader, sink Sink) error {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := readBoundedLine(br, 1<<20)
		if len(line) > 0 {
			l, perr := ParseJournalRecord(line)
			if perr == nil {
				if err := sink.Line(ctx, l); err != nil {
					return err
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// first returns the first sshd entry at or after cursor (nil when none).
func (s *JournalSource) first(ctx context.Context, cursor string) (*Line, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := append([]string{"--output=json", "--no-pager", "--all", "--cursor=" + cursor}, s.matches()...)
	cmd := exec.CommandContext(ctx, s.Binary, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &limitedBuffer{max: 4096}
	cmd.Stderr = stderr
	cmd.WaitDelay = 5 * time.Second
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	raw, _ := readBoundedLine(bufio.NewReader(stdout), 1<<20)
	if len(raw) > 0 {
		cancel()
		_ = cmd.Wait()
		s.noteAccess(stderr.String())
		l, err := ParseJournalRecord(raw)
		if err != nil {
			return nil, err
		}
		return &l, nil
	}
	// Output ended without a record: let journalctl exit on its own so its
	// status distinguishes "no entries" from a rejected cursor.
	waitErr := cmd.Wait()
	s.noteAccess(stderr.String())
	if waitErr != nil {
		return nil, fmt.Errorf("%v: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	return nil, nil
}

// readBoundedLine reads one '\n'-terminated line, discarding the remainder of
// lines longer than max.
func readBoundedLine(r *bufio.Reader, max int) ([]byte, error) {
	var out []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(out)+len(chunk) <= max {
			out = append(out, chunk...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimRight(out, "\r\n"), err
	}
}

type limitedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// FileSource tails a syslog file (/var/log/auth.log, /var/log/secure). Its
// cursor is "<dev>:<inode>:<offset>"; after rotation it finishes the rotated
// file (found by inode) before continuing with the new one.
type FileSource struct {
	Path     string
	Poll     time.Duration
	Backfill time.Duration
	Now      func() time.Time
}

func (s *FileSource) Name() string { return "file:" + s.Path }

func (s *FileSource) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

type tailFile struct {
	f        *os.File
	dev, ino uint64
	off      int64
	pending  []byte
	cutoff   time.Time
}

func parseFileCursor(c string) (dev, ino uint64, off int64, ok bool) {
	parts := strings.Split(c, ":")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var err1, err2, err3 error
	dev, err1 = strconv.ParseUint(parts[0], 10, 64)
	ino, err2 = strconv.ParseUint(parts[1], 10, 64)
	off, err3 = strconv.ParseInt(parts[2], 10, 64)
	return dev, ino, off, err1 == nil && err2 == nil && err3 == nil && off >= 0
}

func (s *FileSource) open(path string, off int64, cutoff time.Time) (*tailFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	dev, ino, ok := fileIdentity(fi)
	if !ok {
		f.Close()
		return nil, errors.New("file identity unavailable on this platform")
	}
	if _, err = f.Seek(off, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return &tailFile{f: f, dev: dev, ino: ino, off: off, cutoff: cutoff}, nil
}

func (s *FileSource) findRotated(dev, ino uint64) string {
	candidates := []string{s.Path + ".1", s.Path + ".0"}
	if matches, _ := filepath.Glob(s.Path + "-*"); len(matches) > 0 {
		candidates = append(candidates, matches...)
	}
	for _, c := range candidates {
		fi, err := os.Stat(c)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if d, i, ok := fileIdentity(fi); ok && d == dev && i == ino {
			return c
		}
	}
	return ""
}

func (s *FileSource) Run(ctx context.Context, cursor string, sink Sink) error {
	poll := s.Poll
	if poll <= 0 {
		poll = time.Second
	}
	backfillCutoff := s.now().Add(-s.Backfill)
	var cur *tailFile
	defer func() {
		if cur != nil {
			cur.f.Close()
		}
	}()
	startFresh := func(cutoff time.Time) error {
		t, err := s.open(s.Path, 0, cutoff)
		if err != nil {
			return err
		}
		cur = t
		return nil
	}
	if cursor == "" {
		if err := startFresh(backfillCutoff); err != nil {
			return err
		}
	} else if dev, ino, off, ok := parseFileCursor(cursor); !ok {
		if err := sink.Gap(ctx, Gap{Reason: "log file cursor is malformed", To: s.now(), Cursor: cursor}); err != nil {
			return err
		}
		if err := startFresh(backfillCutoff); err != nil {
			return err
		}
	} else {
		fi, err := os.Stat(s.Path)
		if err != nil {
			return err
		}
		cdev, cino, _ := fileIdentity(fi)
		switch {
		case cdev == dev && cino == ino && fi.Size() >= off:
			if cur, err = s.open(s.Path, off, time.Time{}); err != nil {
				return err
			}
		case cdev == dev && cino == ino:
			if err := sink.Gap(ctx, Gap{Reason: "log file was truncated", To: s.now(), Cursor: cursor}); err != nil {
				return err
			}
			if err := startFresh(time.Time{}); err != nil {
				return err
			}
		default:
			if rotated := s.findRotated(dev, ino); rotated != "" {
				old, err := s.open(rotated, off, time.Time{})
				if err != nil {
					return err
				}
				err = s.drain(ctx, old, sink, true)
				old.f.Close()
				if err != nil {
					return err
				}
			} else if err := sink.Gap(ctx, Gap{Reason: "log file rotated beyond the stored cursor", To: s.now(), Cursor: cursor}); err != nil {
				return err
			}
			if err := startFresh(time.Time{}); err != nil {
				return err
			}
		}
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		if err := s.drain(ctx, cur, sink, false); err != nil {
			return err
		}
		// Rotation or truncation check at EOF.
		if fi, err := os.Stat(s.Path); err == nil {
			dev, ino, _ := fileIdentity(fi)
			if dev != cur.dev || ino != cur.ino {
				if err := s.drain(ctx, cur, sink, true); err != nil {
					return err
				}
				next, err := s.open(s.Path, 0, time.Time{})
				if err == nil {
					cur.f.Close()
					cur = next
					continue
				}
			} else if fi.Size() < cur.off {
				if err := sink.Gap(ctx, Gap{Reason: "log file was truncated", To: s.now(), Cursor: s.cursor(cur)}); err != nil {
					return err
				}
				if _, err := cur.f.Seek(0, io.SeekStart); err != nil {
					return err
				}
				cur.off, cur.pending = 0, nil
				continue
			}
		}
		timer.Reset(poll)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *FileSource) cursor(t *tailFile) string {
	return fmt.Sprintf("%d:%d:%d", t.dev, t.ino, t.off)
}

// drain reads complete lines to EOF. final also flushes an unterminated last
// line (a rotated file receives no more writes).
func (s *FileSource) drain(ctx context.Context, t *tailFile, sink Sink, final bool) error {
	buf := make([]byte, 32<<10)
	for {
		n, err := t.f.Read(buf)
		if n > 0 {
			t.pending = append(t.pending, buf[:n]...)
			for {
				i := bytes.IndexByte(t.pending, '\n')
				if i < 0 {
					if len(t.pending) > maxRecordBytes {
						i = len(t.pending) - 1
					} else {
						break
					}
				}
				raw := t.pending[:i+1]
				t.pending = t.pending[i+1:]
				t.off += int64(len(raw))
				if err := s.emit(ctx, t, raw, sink); err != nil {
					return err
				}
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return err
			}
			if final && len(t.pending) > 0 {
				raw := t.pending
				t.pending = nil
				t.off += int64(len(raw))
				return s.emit(ctx, t, raw, sink)
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

func (s *FileSource) emit(ctx context.Context, t *tailFile, raw []byte, sink Sink) error {
	text := strings.TrimRight(string(raw), "\r\n")
	cursor := s.cursor(t)
	sum := sha256.Sum256(raw)
	l, ok := ParseSyslogLine(text, s.now())
	if !ok || (!t.cutoff.IsZero() && l.Time.Before(t.cutoff)) {
		// Still emitted (without content) so the cursor advances past it.
		l = Line{}
	}
	l.Cursor = cursor
	// Inode numbers are reused; the content digest keeps a recycled inode
	// from colliding with an old file's event IDs.
	l.Key = cursor + ":" + hex.EncodeToString(sum[:8])
	return sink.Line(ctx, l)
}
