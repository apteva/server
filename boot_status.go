package main

// boot_status.go — feeds the CLI's auto-rollback safety net.
//
// The CLI keeps two files in $APTEVA_HOME:
//
//   boot-attempts       — int. Incremented on every server start.
//                         Reset to 0 once we've been healthy for ~30s.
//   last-good-version   — semver string. Set when /health is first
//                         200 after boot. Becomes the rollback target
//                         if a future binary fails to come up.
//
// On boot, BumpBootAttempts increments the counter. A goroutine
// started by ScheduleHealthyMark waits until the in-process /health
// path returns successfully for `healthyDuration`, then writes
// last-good-version and resets the counter.
//
// The CLI's rollbackIfFailed (apteva/layout.go) reads boot-attempts
// before launching the server — if it's ≥ 3 AND the active version
// differs from last-good, the CLI flips bin/current back. Without
// this, a v0.13.0 that segfaults on every start would just thrash
// systemd's Restart=on-failure forever.

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	healthyDuration = 30 * time.Second
	healthCheckURL  = "http://127.0.0.1:%s/health"
)

// aptevaHome is APTEVA_HOME with the same fallback chain the
// preflight runner uses. Returns "" if nothing usable. Callers
// should no-op gracefully on "" — boot status is best-effort.
func aptevaHome() string {
	h := os.Getenv("APTEVA_HOME")
	if h != "" {
		return h
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".apteva")
	}
	return ""
}

func bootAttemptsFile() string {
	if h := aptevaHome(); h != "" {
		return filepath.Join(h, "boot-attempts")
	}
	return ""
}

func lastGoodFile() string {
	if h := aptevaHome(); h != "" {
		return filepath.Join(h, "last-good-version")
	}
	return ""
}

// BumpBootAttempts increments the on-disk counter. Called from main
// before the listener binds — putting it earlier means a crash
// during init still counts toward the auto-rollback threshold.
// Returns the new count for log emission.
func BumpBootAttempts() int {
	p := bootAttemptsFile()
	if p == "" {
		return 0
	}
	b, _ := os.ReadFile(p)
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	n++
	_ = os.WriteFile(p, []byte(strconv.Itoa(n)), 0o644)
	return n
}

// ScheduleHealthyMark spins a goroutine that polls /health on the
// local listener and, once it's been 200 for `healthyDuration` of
// continuous uptime, writes last-good-version and zeros boot-
// attempts. We poll instead of marking immediately because a real-
// world bad release can boot, accept a request or two, then segv —
// we want last-good to stick only AFTER the binary's demonstrably
// stable.
func ScheduleHealthyMark(port string, version string) {
	go func() {
		client := &http.Client{Timeout: 2 * time.Second}
		var firstOK time.Time
		for {
			time.Sleep(2 * time.Second)
			req, err := http.NewRequest("GET", "http://127.0.0.1:"+port+"/health", nil)
			if err != nil {
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				firstOK = time.Time{}
				continue
			}
			_ = resp.Body.Close()
			if resp.StatusCode != 200 {
				firstOK = time.Time{}
				continue
			}
			if firstOK.IsZero() {
				firstOK = time.Now()
				continue
			}
			if time.Since(firstOK) < healthyDuration {
				continue
			}
			// Healthy long enough — promote.
			if p := lastGoodFile(); p != "" {
				_ = os.WriteFile(p, []byte(version), 0o644)
			}
			if p := bootAttemptsFile(); p != "" {
				_ = os.WriteFile(p, []byte("0"), 0o644)
			}
			return
		}
	}()
}
