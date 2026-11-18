package claudepool

import (
	"bufio"
	"encoding/json"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// streamMsg is one line of `claude -p --output-format stream-json`.
type streamMsg struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype,omitempty"`
	Event   *streamEvent    `json:"event,omitempty"`
	Result  string          `json:"result,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
	Usage   json.RawMessage `json:"usage,omitempty"`
	// rate_limit_event
	RateLimitInfo *rateLimitInfo `json:"rate_limit_info,omitempty"`
	// system/init
	MCPServers []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers,omitempty"`
	Model string `json:"model,omitempty"`
	// Response answers a control request.
	Response *struct {
		Subtype   string          `json:"subtype"`
		RequestID string          `json:"request_id"`
		Error     string          `json:"error,omitempty"`
		Body      json.RawMessage `json:"response,omitempty"`
	} `json:"response,omitempty"`
}

type streamEvent struct {
	Type         string        `json:"type"`
	Index        int           `json:"index"`
	Message      *eventMessage `json:"message,omitempty"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id,omitempty"`
		Name string `json:"name,omitempty"`
	} `json:"content_block,omitempty"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text,omitempty"`
		Thinking    string `json:"thinking,omitempty"`
		PartialJSON string `json:"partial_json,omitempty"`
		StopReason  string `json:"stop_reason,omitempty"`
	} `json:"delta,omitempty"`
	Usage *apiUsage `json:"usage,omitempty"`
}

type eventMessage struct {
	Model string    `json:"model,omitempty"`
	Usage *apiUsage `json:"usage,omitempty"`
}

type apiUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

type rateLimitInfo struct {
	Status        string   `json:"status"`
	ResetsAt      int64    `json:"resetsAt"`
	RateLimitType string   `json:"rateLimitType"`
	Utilization   *float64 `json:"utilization"`
}

// process is one running `claude -p` child.
type process struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	msgs      chan streamMsg
	done      chan struct{}
	pid       int
	startedAt time.Time

	mu       sync.Mutex
	exitErr  error
	stderr   limitedBuffer
	controls int
}

func startProcess(bin string, args, env []string, dir string) (*process, error) {
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Dir = dir
	// Own process group so closing the process also ends anything it
	// spawned.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	p := &process{
		cmd:       cmd,
		stdin:     stdin,
		msgs:      make(chan streamMsg, 4096),
		done:      make(chan struct{}),
		startedAt: time.Now(),
	}
	cmd.Stderr = &p.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p.pid = cmd.Process.Pid
	go p.read(stdout)
	return p, nil
}

func (p *process) read(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	for scanner.Scan() {
		var msg streamMsg
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil || msg.Type == "" {
			continue
		}
		p.msgs <- msg
	}
	err := p.cmd.Wait()
	p.mu.Lock()
	p.exitErr = err
	p.mu.Unlock()
	close(p.msgs)
	close(p.done)
}

// send writes one stream-json user message with the given content blocks.
// control sends a control request and returns its id.
func (p *process) control(request map[string]any) (string, error) {
	p.mu.Lock()
	p.controls++
	id := "apteva-" + strconv.Itoa(p.controls)
	p.mu.Unlock()
	line, err := json.Marshal(map[string]any{"type": "control_request", "request_id": id, "request": request})
	if err != nil {
		return "", err
	}
	_, err = p.stdin.Write(append(line, '\n'))
	return id, err
}

func (p *process) send(content []map[string]any) error {
	line, err := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": content},
	})
	if err != nil {
		return err
	}
	_, err = p.stdin.Write(append(line, '\n'))
	return err
}

// close ends the process: closing stdin lets Claude Code exit on its own
// and save the session; the process group is killed if it lingers.
func (p *process) close() {
	_ = p.stdin.Close()
	go func() {
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		}
	}()
}

// kill stops the process at once, before it can act on anything else.
func (p *process) kill() {
	_ = p.stdin.Close()
	_ = syscall.Kill(-p.pid, syscall.SIGKILL)
}

func (p *process) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *process) failure() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	msg := ""
	if p.exitErr != nil {
		msg = p.exitErr.Error()
	}
	if tail := p.stderr.String(); tail != "" {
		if msg != "" {
			msg += ": "
		}
		msg += tail
	}
	if msg == "" {
		msg = "exited"
	}
	return msg
}

// limitedBuffer keeps the last 4 KiB written to it.
type limitedBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > 4096 {
		b.buf = b.buf[len(b.buf)-4096:]
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func msString(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10)
}
