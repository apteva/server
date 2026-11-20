package claudepool

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apteva/server/claudepool/claudetest"
)

func fakeLoginCLI(t *testing.T) (CLI, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDEPOOL_FAKE", "1")
	received := filepath.Join(t.TempDir(), "received")
	t.Setenv("CLAUDEPOOL_FAKE_LOGIN_RECEIVED", received)
	return CLI{Binary: exe, ConfigDir: t.TempDir()}, received
}

func waitLogin(t *testing.T, l *LoginRunner, what string, ok func(LoginState) bool) LoginState {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		state := l.State()
		if ok(state) {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %+v", what, state)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLoginPassesTheCodeToClaude(t *testing.T) {
	cli, received := fakeLoginCLI(t)
	var l LoginRunner
	var done atomic.Int32
	if err := l.Start(cli, time.Minute, func() { done.Add(1) }); err != nil {
		t.Fatal(err)
	}
	state := waitLogin(t, &l, "the code prompt", func(s LoginState) bool { return s.URL != "" && s.AwaitingCode })
	if !strings.HasPrefix(state.URL, "https://claude.com/cai/oauth/authorize?") || !state.Running || state.CodeSubmitted {
		t.Fatalf("state = %+v", state)
	}
	if err := l.SubmitCode("  " + claudetest.FakeLoginCode + "  "); err != nil {
		t.Fatal(err)
	}
	if err := l.SubmitCode(claudetest.FakeLoginCode); !errors.Is(err, ErrCodeSubmitted) && !errors.Is(err, ErrLoginNotRunning) {
		t.Fatalf("second code: %v", err)
	}
	state = waitLogin(t, &l, "the sign-in to finish", func(s LoginState) bool { return !s.Running })
	if state.LastError != "" || state.URL != "" || state.AwaitingCode {
		t.Fatalf("finished state = %+v", state)
	}
	if got, _ := os.ReadFile(received); string(got) != claudetest.FakeLoginCode+"\n" {
		t.Fatalf("claude read %q, want the code once", got)
	}
	if done.Load() != 1 {
		t.Fatalf("done ran %d times", done.Load())
	}
	raw, _ := json.Marshal(l.State())
	if strings.Contains(string(raw), "good-code") {
		t.Fatalf("state keeps the code: %s", raw)
	}
}

func TestLoginReportsARejectedCode(t *testing.T) {
	cli, _ := fakeLoginCLI(t)
	var l LoginRunner
	var done atomic.Int32
	if err := l.Start(cli, time.Minute, func() { done.Add(1) }); err != nil {
		t.Fatal(err)
	}
	waitLogin(t, &l, "the code prompt", func(s LoginState) bool { return s.AwaitingCode })
	if err := l.SubmitCode("wrong-code#fake-state"); err != nil {
		t.Fatal(err)
	}
	if !l.State().CodeSubmitted {
		t.Fatal("code_submitted not set while Claude checks the code")
	}
	state := waitLogin(t, &l, "the sign-in to fail", func(s LoginState) bool { return !s.Running })
	if !strings.Contains(state.LastError, "wasn't accepted") || strings.Contains(state.LastError, "wrong-code") {
		t.Fatalf("last error = %q", state.LastError)
	}
	if done.Load() != 1 {
		t.Fatalf("done ran %d times", done.Load())
	}
}

func TestLoginCancelAndReplace(t *testing.T) {
	cli, received := fakeLoginCLI(t)
	var l LoginRunner
	var done atomic.Int32
	if err := l.Start(cli, time.Minute, func() { done.Add(1) }); err != nil {
		t.Fatal(err)
	}
	waitLogin(t, &l, "the code prompt", func(s LoginState) bool { return s.AwaitingCode })
	if !l.Cancel() {
		t.Fatal("cancel found no sign-in")
	}
	if state := l.State(); state.Running || state.LastError != "" || state.URL != "" {
		t.Fatalf("after cancel: %+v", state)
	}
	if l.Cancel() {
		t.Fatal("second cancel found a sign-in")
	}
	if err := l.SubmitCode(claudetest.FakeLoginCode); !errors.Is(err, ErrLoginNotRunning) {
		t.Fatalf("code after cancel: %v", err)
	}

	// A new sign-in replaces one in progress; the old one's exit is ignored.
	if err := l.Start(cli, time.Minute, func() { done.Add(1) }); err != nil {
		t.Fatal(err)
	}
	waitLogin(t, &l, "the first prompt", func(s LoginState) bool { return s.AwaitingCode })
	first := l.State().StartedAt
	time.Sleep(10 * time.Millisecond)
	if err := l.Start(cli, time.Minute, func() { done.Add(1) }); err != nil {
		t.Fatal(err)
	}
	state := waitLogin(t, &l, "the second prompt", func(s LoginState) bool { return s.AwaitingCode })
	time.Sleep(300 * time.Millisecond) // the replaced process has exited by now
	if state = l.State(); !state.Running || !state.StartedAt.After(*first) || state.LastError != "" {
		t.Fatalf("replaced sign-in disturbed the new one: %+v", state)
	}
	if err := l.SubmitCode(claudetest.FakeLoginCode); err != nil {
		t.Fatal(err)
	}
	waitLogin(t, &l, "the sign-in to finish", func(s LoginState) bool { return !s.Running })
	if state := l.State(); state.LastError != "" {
		t.Fatalf("state = %+v", state)
	}
	if got, _ := os.ReadFile(received); string(got) != claudetest.FakeLoginCode+"\n" {
		t.Fatalf("claude read %q", got)
	}
	if done.Load() != 1 {
		t.Fatalf("done ran %d times; canceled and replaced sign-ins must not count", done.Load())
	}
}

func TestLoginRejectsMalformedCodes(t *testing.T) {
	var l LoginRunner
	for _, code := range []string{"", "   ", "a\nb", "a\rb", strings.Repeat("x", maxLoginCode+1)} {
		if err := l.SubmitCode(code); !errors.Is(err, ErrInvalidCode) {
			t.Fatalf("code %q: %v", code, err)
		}
	}
	if err := l.SubmitCode("fine"); !errors.Is(err, ErrLoginNotRunning) {
		t.Fatalf("no sign-in: %v", err)
	}
}

func TestLoginOnlyShowsAnthropicLinks(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://claude.com/cai/oauth/authorize?code=true": true,
		"https://claude.ai/oauth/authorize?x=1":            true,
		"https://platform.claude.com/oauth":                true,
		"http://claude.com/cai/oauth/authorize":            false,
		"https://claude.com.evil.example/oauth":            false,
		"https://evilclaude.com/oauth":                     false,
		"https://user@claude.com/oauth":                    false,
		"javascript:alert(1)":                              false,
	} {
		if validLoginURL(raw) != ok {
			t.Errorf("validLoginURL(%q) = %v", raw, !ok)
		}
	}
	cli, _ := fakeLoginCLI(t)
	t.Setenv("CLAUDEPOOL_FAKE_LOGIN_URL", "https://evil.example/authorize")
	var l LoginRunner
	if err := l.Start(cli, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	defer l.Cancel()
	if state := waitLogin(t, &l, "the code prompt", func(s LoginState) bool { return s.AwaitingCode }); state.URL != "" {
		t.Fatalf("foreign link shown: %q", state.URL)
	}
}

func TestLoginTimesOut(t *testing.T) {
	cli, _ := fakeLoginCLI(t)
	var l LoginRunner
	if err := l.Start(cli, 500*time.Millisecond, nil); err != nil {
		t.Fatal(err)
	}
	state := waitLogin(t, &l, "the timeout", func(s LoginState) bool { return !s.Running })
	if !strings.Contains(state.LastError, "not completed in time") {
		t.Fatalf("last error = %q", state.LastError)
	}
}
