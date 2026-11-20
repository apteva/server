package claudepool

import (
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// LoginRunner runs Claude Code's own sign-in command, `claude auth login
// --claudeai`, so a sign-in works whether or not the machine running Apteva
// has a browser.
//
// Claude Code prints a sign-in link and waits for the one-time code that
// Anthropic's page shows after the user approves. The runner reads only that
// link, the code prompt and Claude's own sign-in error from the command's
// output, and ignores everything else. It writes the code the user pastes to
// the command's input once and keeps no copy. Claude Code exchanges the code
// itself, with a verifier only it holds, and stores the resulting login; the
// runner never sees or stores tokens.
type LoginRunner struct {
	mu sync.Mutex
	// attempt identifies the current sign-in; output and exit of a replaced
	// or canceled sign-in are ignored.
	attempt       uint64
	running       bool
	startedAt     time.Time
	url           string
	awaitingCode  bool
	codeSubmitted bool
	lastErr       string
	stdin         io.WriteCloser
	cancel        context.CancelFunc
}

// LoginState reports the sign-in's progress.
type LoginState struct {
	Running   bool       `json:"running"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	// URL is Anthropic's sign-in page for this attempt, once Claude Code
	// has printed it.
	URL string `json:"url,omitempty"`
	// AwaitingCode is set once Claude Code asks for the code.
	AwaitingCode bool `json:"awaiting_code"`
	// CodeSubmitted is set once a code was passed to Claude Code; it is
	// checking it while the sign-in still runs.
	CodeSubmitted bool   `json:"code_submitted"`
	LastError     string `json:"last_error,omitempty"`
}

var (
	// ErrLoginNotRunning means no sign-in is in progress.
	ErrLoginNotRunning = errors.New("no sign-in is in progress")
	// ErrInvalidCode means the pasted code cannot be a sign-in code.
	ErrInvalidCode = errors.New("the code is empty, too long or spans several lines")
	// ErrCodeSubmitted means this sign-in already received a code.
	ErrCodeSubmitted = errors.New("a code was already submitted for this sign-in")
)

const maxLoginCode = 512

var (
	loginURLPattern    = regexp.MustCompile(`visit: (https://\S+)`)
	loginPromptPattern = regexp.MustCompile(`(?i)paste code here`)
	loginErrorPattern  = regexp.MustCompile(`Couldn't log in: ([^\r\n]+)`)
	terminalEscapes    = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07`)
)

// State returns the current sign-in progress.
func (l *LoginRunner) State() LoginState {
	l.mu.Lock()
	defer l.mu.Unlock()
	state := LoginState{Running: l.running, URL: l.url, AwaitingCode: l.awaitingCode, CodeSubmitted: l.codeSubmitted, LastError: l.lastErr}
	if !l.startedAt.IsZero() {
		started := l.startedAt
		state.StartedAt = &started
	}
	return state
}

// Start begins a sign-in, replacing one already in progress. done runs
// after the command exits, unless this sign-in was replaced or canceled.
func (l *LoginRunner) Start(c CLI, timeout time.Duration, done func()) error {
	bin, err := c.Resolve()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	cmd := exec.CommandContext(ctx, bin, "auth", "login", "--claudeai")
	cmd.Env = c.Env()
	cmd.WaitDelay = 5 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return err
	}
	// One pipe for both streams: Claude Code prints the link and the prompt
	// on either, depending on the release.
	output, writer, err := os.Pipe()
	if err != nil {
		cancel()
		return err
	}
	cmd.Stdout, cmd.Stderr = writer, writer

	l.mu.Lock()
	if l.cancel != nil {
		l.cancel() // replace the sign-in in progress
	}
	l.attempt++
	attempt := l.attempt
	l.running, l.startedAt, l.lastErr = true, time.Now(), ""
	l.url, l.awaitingCode, l.codeSubmitted = "", false, false
	l.stdin, l.cancel = stdin, cancel
	err = cmd.Start()
	writer.Close()
	if err != nil {
		output.Close()
		l.mu.Unlock()
		l.finish(attempt, err, loginStartFailed)
		cancel()
		return err
	}
	l.mu.Unlock()

	read := make(chan struct{})
	go func() {
		defer close(read)
		l.readOutput(attempt, output)
	}()
	go func() {
		defer cancel()
		err := cmd.Wait()
		// A process Claude Code started may still hold the output open.
		select {
		case <-read:
		case <-time.After(2 * time.Second):
		}
		output.Close()
		<-read
		how := loginExited
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			how = loginTimedOut
		}
		if l.finish(attempt, err, how) && done != nil {
			done()
		}
	}()
	return nil
}

// readOutput picks the sign-in link, the code prompt and Claude Code's
// sign-in error out of the command's output. The prompt ends without a
// newline, so output is matched as it arrives rather than by line.
func (l *LoginRunner) readOutput(attempt uint64, r io.Reader) {
	var pending []byte
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			if len(pending) > 16<<10 {
				pending = pending[len(pending)-8<<10:]
			}
			text := terminalEscapes.ReplaceAllString(string(pending), "")
			l.mu.Lock()
			if l.attempt == attempt {
				if l.url == "" {
					if m := loginURLPattern.FindStringSubmatch(text); m != nil && validLoginURL(m[1]) {
						l.url = m[1]
					}
				}
				if !l.codeSubmitted && loginPromptPattern.MatchString(text) {
					l.awaitingCode = true
				}
				if m := loginErrorPattern.FindStringSubmatch(text); m != nil {
					message := strings.TrimSpace(m[1])
					if len(message) > 300 {
						message = message[:300]
					}
					l.lastErr = message
				}
			}
			l.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// validLoginURL accepts only Anthropic's own sign-in pages.
func validLoginURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, domain := range []string{"claude.com", "claude.ai"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

// SubmitCode passes the one-time code from Anthropic's page to the sign-in
// command. The code is written once and not kept.
func (l *LoginRunner) SubmitCode(code string) error {
	code = strings.TrimSpace(code)
	if code == "" || len(code) > maxLoginCode || strings.ContainsAny(code, "\r\n\x00") {
		return ErrInvalidCode
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.running || l.stdin == nil {
		return ErrLoginNotRunning
	}
	if l.codeSubmitted {
		return ErrCodeSubmitted
	}
	if _, err := io.WriteString(l.stdin, code+"\n"); err != nil {
		return ErrLoginNotRunning
	}
	l.codeSubmitted, l.awaitingCode, l.lastErr = true, false, ""
	return nil
}

// Cancel stops the sign-in in progress. It reports whether one was running.
func (l *LoginRunner) Cancel() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.running {
		return false
	}
	l.cancel()
	l.attempt++ // its exit is not an error
	l.running, l.awaitingCode, l.codeSubmitted = false, false, false
	l.url, l.lastErr, l.stdin, l.cancel = "", "", nil, nil
	return true
}

type loginEnd int

const (
	loginExited loginEnd = iota
	loginTimedOut
	loginStartFailed
)

// finish records how an attempt ended and reports whether it was still the
// current one.
func (l *LoginRunner) finish(attempt uint64, err error, how loginEnd) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.attempt != attempt {
		return false
	}
	l.running, l.awaitingCode, l.url, l.stdin, l.cancel = false, false, "", nil, nil
	switch {
	case how == loginTimedOut:
		l.lastErr = "Sign-in was not completed in time. Start again."
	case err == nil:
		l.lastErr = ""
	case how == loginStartFailed:
		l.lastErr = "Could not start Claude Code's sign-in: " + err.Error()
	case l.lastErr != "":
		// Claude Code's own message, e.g. a code that was not accepted.
	default:
		l.lastErr = "Sign-in was not completed. Start again."
	}
	return true
}
