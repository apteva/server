package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Core writes its stderr to a file the server follows, not to a pipe. A core
// outlives a server restart, and a pipe would close with the old server and
// kill Core on its next write. The server copies each line Core writes into
// its own log, so Core's output still shows up there, and keeps the last few
// lines so a core that exits while starting can say why.

const (
	coreStderrMaxBytes  = 2 << 20 // the file is emptied once copied past this
	coreStderrTailLines = 20
	coreStderrTailBytes = 4 << 10
	coreStderrPoll      = 100 * time.Millisecond
)

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

func coreStderrPath(dataDir string, agentID int64) string {
	return filepath.Join(dataDir, "core-stderr", fmt.Sprintf("agent_%d.log", agentID))
}

// createCoreStderr gives a new core an empty stderr file and returns the end
// Core writes to and the end the server reads. The old file is unlinked first,
// so a follower still reading a previous core's output keeps its own file.
func createCoreStderr(path string) (*os.File, *os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	// O_APPEND keeps Core writing at the end after the follower empties the file.
	w, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, nil, err
	}
	r, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		w.Close()
		return nil, nil, err
	}
	return w, r, nil
}

// coreStderr copies what a core writes to stderr into the server's log.
type coreStderr struct {
	file     *os.File
	alive    func() bool // set for a core this server did not start
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
	out      io.Writer
	mu       sync.Mutex
	tail     []string
}

func followCoreStderr(file *os.File, alive func() bool) *coreStderr {
	f := &coreStderr{file: file, alive: alive, stop: make(chan struct{}), done: make(chan struct{}), out: os.Stderr}
	go f.run()
	return f
}

// followAdoptedCoreStderr follows the stderr file of a core that survived the
// previous server, from where it is now. Cores started before stderr went to a
// file have none, and nothing is followed.
func followAdoptedCoreStderr(path string, pid int) *coreStderr {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		file.Close()
		return nil
	}
	return followCoreStderr(file, func() bool { return processAlive(pid) })
}

func (f *coreStderr) run() {
	defer close(f.done)
	defer f.file.Close()
	var partial []byte
	ticker := time.NewTicker(coreStderrPoll)
	defer ticker.Stop()
	for {
		if !f.readAvailable(&partial) {
			break
		}
		stopping := false
		select {
		case <-f.stop:
			stopping = true
		case <-ticker.C:
			stopping = f.alive != nil && !f.alive()
		}
		if stopping {
			f.readAvailable(&partial)
			break
		}
		f.limitSize()
	}
	if len(partial) > 0 {
		f.copyLines(append(partial, '\n'))
	}
}

// readAvailable copies everything written so far. It reports false when the
// file can no longer be read.
func (f *coreStderr) readAvailable(partial *[]byte) bool {
	buf := make([]byte, 32<<10)
	for {
		n, err := f.file.Read(buf)
		if n > 0 {
			*partial = f.copyLines(append(*partial, buf[:n]...))
		}
		if errors.Is(err, io.EOF) || (err == nil && n == 0) {
			return true
		}
		if err != nil {
			return false
		}
	}
}

// copyLines writes the complete lines in data to the server log and returns
// what is left of an unfinished line.
func (f *coreStderr) copyLines(data []byte) []byte {
	end := bytes.LastIndexByte(data, '\n') + 1
	if end == 0 {
		if len(data) < 64<<10 {
			return data
		}
		data, end = append(data, '\n'), len(data)+1
	}
	_, _ = f.out.Write(data[:end])
	f.mu.Lock()
	for _, line := range strings.Split(string(data[:end-1]), "\n") {
		line = strings.TrimSpace(ansiEscape.ReplaceAllString(line, ""))
		if line == "" {
			continue
		}
		if len(line) > 1000 {
			line = line[:1000] + "…"
		}
		f.tail = append(f.tail, line)
		if len(f.tail) > coreStderrTailLines {
			f.tail = f.tail[len(f.tail)-coreStderrTailLines:]
		}
	}
	f.mu.Unlock()
	return append([]byte(nil), data[end:]...)
}

// limitSize empties the file once it is all copied and has grown past the cap.
func (f *coreStderr) limitSize() {
	info, err := f.file.Stat()
	if err != nil {
		return
	}
	offset, err := f.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return
	}
	switch {
	case offset > info.Size(): // emptied by someone else
		_, _ = f.file.Seek(0, io.SeekStart)
	case info.Size() > coreStderrMaxBytes && offset == info.Size():
		if f.file.Truncate(0) == nil {
			_, _ = f.file.Seek(0, io.SeekStart)
		}
	}
}

// finish copies what is left once the core has exited and returns its last
// lines.
func (f *coreStderr) finish() string {
	if f == nil {
		return ""
	}
	f.stopOnce.Do(func() { close(f.stop) })
	<-f.done
	f.mu.Lock()
	defer f.mu.Unlock()
	out := strings.Join(f.tail, "\n")
	if len(out) > coreStderrTailBytes {
		out = "…" + out[len(out)-coreStderrTailBytes:]
	}
	return out
}

// coreExit is how a core this server started ended.
type coreExit struct {
	pid      int
	exitCode int
	stderr   string
}

// coreStartExitError reports a core that exited before it was up.
type coreStartExitError struct {
	agentID  int64
	exitCode int
	stderr   string
}

func (e *coreStartExitError) Error() string {
	msg := fmt.Sprintf("agent %d core exited during start (exit code %d)", e.agentID, e.exitCode)
	if lines := strings.Split(e.stderr, "\n"); e.stderr != "" {
		msg += ": " + lines[len(lines)-1]
	}
	return msg
}

// AgentStartError is the last failed start of an agent since this server
// started. A successful start clears it.
type AgentStartError struct {
	Message   string    `json:"error"`
	CoreError string    `json:"core_error,omitempty"`
	ExitCode  *int      `json:"exit_code,omitempty"`
	At        time.Time `json:"at"`
}

func (im *AgentManager) recordCoreExitLocked(agentID int64, exit coreExit) {
	if im.coreExits == nil {
		im.coreExits = map[int64]coreExit{}
	}
	im.coreExits[agentID] = exit
}

// CoreExited reports how the core with this pid ended, once it has.
func (im *AgentManager) CoreExited(agentID int64, pid int) (coreExit, bool) {
	im.mu.RLock()
	defer im.mu.RUnlock()
	exit, ok := im.coreExits[agentID]
	if !ok || pid <= 0 || exit.pid != pid {
		return coreExit{}, false
	}
	return exit, true
}

func (im *AgentManager) noteStartResult(agentID int64, err error) {
	if errors.Is(err, errAgentAlreadyRunning) {
		return
	}
	im.mu.Lock()
	defer im.mu.Unlock()
	if err == nil {
		delete(im.startErrors, agentID)
		return
	}
	record := &AgentStartError{Message: err.Error(), At: time.Now().UTC()}
	var exit *coreStartExitError
	if errors.As(err, &exit) {
		code := exit.exitCode
		record.CoreError, record.ExitCode = exit.stderr, &code
	}
	if im.startErrors == nil {
		im.startErrors = map[int64]*AgentStartError{}
	}
	im.startErrors[agentID] = record
}

// LastStartError is the agent's last failed start, if its latest start failed.
func (im *AgentManager) LastStartError(agentID int64) *AgentStartError {
	im.mu.RLock()
	defer im.mu.RUnlock()
	if record := im.startErrors[agentID]; record != nil {
		copied := *record
		return &copied
	}
	return nil
}

// writeCoreStartExit answers a start that failed because Core exited before it
// was up, with Core's own error. It reports false for any other error.
func writeCoreStartExit(w http.ResponseWriter, err error) bool {
	var exit *coreStartExitError
	if !errors.As(err, &exit) {
		return false
	}
	writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{
		"error":      "core exited during start",
		"core_error": exit.stderr,
		"exit_code":  exit.exitCode,
	})
	return true
}
