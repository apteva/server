package claudepool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRealCLISleepingAgent checks parking a sleeping agent on the
// installed, signed-in claude binary against holding its call as before.
// Each run is one short conversation: the agent notes a code word and
// sleeps with pace; it is held or parked; then it wakes on an event that
// asks for the code word. It logs what Claude Code saved for the pace call,
// which is what the model sees on resume. It uses the subscription, so it
// only runs when asked:
//
//	CLAUDEPOOL_REAL_CLI=1 go test -run TestRealCLISleepingAgent -v -timeout 20m ./claudepool
//
// CLAUDEPOOL_REAL_MODEL picks the model (default sonnet).
func TestRealCLISleepingAgent(t *testing.T) {
	if os.Getenv("CLAUDEPOOL_REAL_CLI") != "1" {
		t.Skip("set CLAUDEPOOL_REAL_CLI=1 to run against the installed claude binary")
	}
	model := os.Getenv("CLAUDEPOOL_REAL_MODEL")
	if model == "" {
		model = "sonnet"
	}
	type outcome struct {
		name      string
		park      time.Duration
		parked    bool
		firstWake time.Duration
		wakeTotal time.Duration
		wake      Usage
		reply     string
		recalled  bool
		flags     []string
	}
	var results []outcome

	modes := []struct {
		name string
		hold bool
	}{
		{"held", true},
		{"parked", false},
	}
	for i, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			oldReap, oldObserver := reapInterval, parkObserver
			defer func() { reapInterval, parkObserver = oldReap, oldObserver }()
			reapInterval = 200 * time.Millisecond
			parked := make(chan time.Duration, 1)
			parkObserver = func(_ string, took time.Duration) { parked <- took }

			tp := newRealPoolWith(t, 10*time.Minute, func(cfg *Config) {
				cfg.ParkTools = []string{"pace"}
				cfg.Limits = Limits{IdleTimeout: 3 * time.Second, HoldSleeping: m.hold}
			})
			word := []string{"HERON", "LANTERN"}[i] + "-" + fmt.Sprint(time.Now().Unix()%1000)
			req := Request{
				Owner: "real-sleep-" + m.name, Model: model,
				System: "You are an always-on assistant agent. You act through your tools and short replies. " +
					"When you have nothing to do, call the pace tool with sleep set to how long to wait; the next event wakes you, " +
					"and it arrives as the result of that call or as a new message.",
				Tools: []Tool{paceTool, lookupTool},
				Messages: []Message{user("[event] Setup: the code word for today is " + word + ". Remember it. " +
					"There is nothing else to do now, so sleep for 10 minutes with pace.")},
			}
			r, err := tp.run(t, req)
			if err != nil {
				t.Fatal(err)
			}
			if r.finish != "tool_calls" || len(r.calls) != 1 || r.calls[0].Name != "pace" {
				t.Fatalf("the agent did not sleep: %+v", r)
			}
			t.Logf("slept: %s %s", r.calls[0].Name, r.calls[0].Arguments)

			o := outcome{name: m.name}
			if m.hold {
				time.Sleep(15 * time.Second)
				if st := tp.Status(); len(st.Processes) != 1 || st.Processes[0].State != "waiting_tool" {
					t.Fatalf("held call was not kept: %+v", st.Processes)
				}
			} else {
				select {
				case o.park = <-parked:
					o.parked = true
				case <-time.After(time.Minute):
					t.Fatal("the sleeping agent was not parked")
				}
				time.Sleep(5 * time.Second)
			}

			req.Messages = append(req.Messages,
				Message{Role: "assistant", ToolCalls: r.calls},
				Message{Role: "tool", ToolCallID: r.calls[0].ID, Text: "set sleep=10m"},
				user("[event] A colleague asks: what is today's code word? Answer them in one short sentence."))
			start := time.Now()
			var first time.Duration
			var once sync.Once
			var woke reply
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			err = tp.Run(ctx, req, func(ev Event) error {
				once.Do(func() { first = time.Since(start) })
				switch ev.Kind {
				case EventText:
					woke.text += ev.Text
				case EventToolStart:
					woke.calls = append(woke.calls, ToolCall{ID: ev.ToolID, Name: ev.ToolName})
				case EventDone:
					woke.finish, woke.usage = ev.FinishReason, ev.Usage
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			o.firstWake, o.wakeTotal, o.wake, o.reply = first, time.Since(start), woke.usage, strings.TrimSpace(woke.text)
			o.recalled = strings.Contains(woke.text, word)
			for _, flag := range []string{"interrupt", "cancel", "stopped", "error", "didn't finish", "did not finish", "waiting"} {
				if strings.Contains(strings.ToLower(woke.text), flag) {
					o.flags = append(o.flags, flag)
				}
			}
			if len(woke.calls) > 0 {
				o.flags = append(o.flags, fmt.Sprintf("also called %d tool(s)", len(woke.calls)))
			}
			t.Logf("wake reply (%s): %q", woke.finish, o.reply)
			logSavedTranscript(t, tp, r.calls[0].ID)
			results = append(results, o)
		})
	}

	t.Logf("%-7s | %-8s | %-8s | %-30s | %s", "run", "park", "1st wake", "wake tokens in/read/write/out", "recalled, flags")
	for _, o := range results {
		took := "-"
		if o.parked {
			took = o.park.Round(time.Millisecond).String()
		}
		t.Logf("%-7s | %-8s | %-8s | %-30s | %v %v", o.name, took, o.firstWake.Round(100*time.Millisecond),
			fmt.Sprintf("%d/%d/%d/%d", o.wake.InputTokens, o.wake.CacheReadTokens, o.wake.CacheWriteTokens, o.wake.OutputTokens), o.recalled, o.flags)
	}
	for _, o := range results {
		if !o.recalled {
			t.Errorf("%s: the woken agent did not recall the code word: %q", o.name, o.reply)
		}
	}
}

// logSavedTranscript logs what Claude Code saved from the sleep call on:
// what the model sees when the session is resumed.
func logSavedTranscript(t *testing.T, tp *testPool, callID string) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(tp.projectDir(), "*.jsonl"))
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		on := false
		for scanner.Scan() {
			line := scanner.Text()
			if !on && strings.Contains(line, callID) {
				on = true
			}
			if !on {
				continue
			}
			var rec struct {
				Type    string `json:"type"`
				Message struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal([]byte(line), &rec) != nil || (rec.Type != "user" && rec.Type != "assistant") {
				continue
			}
			t.Logf("saved %s: %s", rec.Type, clip(compactContent(rec.Message.Content), 400))
			if rec.Type == "user" && strings.Contains(line, "set sleep=10m") {
				break
			}
		}
		_ = f.Close()
	}
}

var signatureField = regexp.MustCompile(`"signature":"[^"]*"`)

func compactContent(raw json.RawMessage) string {
	return signatureField.ReplaceAllString(string(raw), `"signature":"…"`)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
