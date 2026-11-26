package claudepool

import (
	"slices"
	"time"
)

// Sleeping agents.
//
// An agent that calls a park tool (core's pace) waits for an outside event,
// often for minutes or hours. Holding that call keeps a claude process and
// a pool slot for the whole wait. Instead, once such a session has waited
// for Limits.IdleTimeout, or as soon as another conversation needs its
// slot, the pool closes the process with the call still open and keeps the
// session. Claude Code records the call as interrupted. When the results
// arrive the session is resumed (--resume) and they reach Claude in a new
// message, shaped like the result of a held call. Waits shorter than
// IdleTimeout are held as before.
//
// Closing the process costs no model request. Answering the call to let
// Claude end its turn cost two requests per park in a comparison on the
// real CLI, and interrupting the turn records the call as refused by the
// user; the resumed replies were alike in all three.

// parkObserver, when set, is told how long each park took. Tests use it.
var parkObserver func(session string, took time.Duration)

// parkPause, when set, runs while a park holds the session's turn lock,
// before the process is closed. Tests use it.
var parkPause func()

func (p *Pool) isParkTool(name string) bool {
	return slices.Contains(p.cfg.ParkTools, name)
}

// sleepingLocked reports whether sess waits only on park tools, with its
// process running and no request in the way. The pool lock must be held.
func (p *Pool) sleepingLocked(sess *session) bool {
	if sess.proc == nil || !sess.busy || sess.turnHeld || sess.parking || sess.state != "waiting_tool" || len(sess.awaiting) == 0 {
		return false
	}
	for _, call := range sess.awaiting {
		if !p.isParkTool(call.Name) {
			return false
		}
	}
	return true
}

// longestSleepingLocked returns the sleeping session that has waited
// longest, other than except.
func (p *Pool) longestSleepingLocked(except *session) *session {
	var found *session
	for _, sess := range p.sessions {
		if sess == except || !p.sleepingLocked(sess) {
			continue
		}
		if found == nil || sess.stateSince.Before(found.stateSince) {
			found = sess
		}
	}
	return found
}

// parkSleeping closes a sleeping session's process, keeping the session to
// resume when the results arrive. It holds the session's turn lock while it
// works, so a request that brings the results waits for it. It reports
// whether the session was parked.
func (p *Pool) parkSleeping(sess *session, reason string) bool {
	p.mu.Lock()
	if p.sessions[sess.id] != sess || !p.sleepingLocked(sess) {
		p.mu.Unlock()
		return false
	}
	select {
	case sess.turn <- struct{}{}:
	default:
		// A request is about to deliver the results.
		p.mu.Unlock()
		return false
	}
	sess.turnHeld = true
	sess.parking = true
	proc, calls, detail := sess.proc, sess.awaiting, sess.detail
	waited := time.Since(sess.stateSince).Round(time.Second)
	sess.setState("parking", detail)
	p.mu.Unlock()

	if parkPause != nil {
		parkPause()
	}
	start := time.Now()
	proc.kill()
	select {
	case <-proc.done:
	case <-time.After(10 * time.Second):
		p.cfg.Logf("[CLAUDE-POOL] session %s: process %d still running 10s after it was killed", short(sess.id), proc.pid)
	}
	took := time.Since(start)
	p.cfg.Logf("[CLAUDE-POOL] session %s: sleeping on %s for %s (%s); closed its process", short(sess.id), detail, waited, reason)

	p.mu.Lock()
	if sess.proc == proc {
		sess.proc = nil
	}
	sess.parking = false
	sess.busy = false
	sess.awaiting = nil
	if !sess.dirty {
		sess.parked = calls
		sess.setState("sleeping", detail)
	} else {
		sess.setState("idle", "")
	}
	p.notifyLocked()
	p.mu.Unlock()
	sess.failHeld("The process was closed while the agent sleeps.")
	if parkObserver != nil {
		parkObserver(sess.id, took)
	}
	p.releaseTurn(sess)
	return true
}

// wakeContent is what a session resumed after sleeping receives: the
// results of the calls it slept on, then the messages that came with them,
// as a held call would have received them.
func wakeContent(pl plan) pieces {
	var ps pieces
	for i, msg := range pl.wakeResults {
		if i > 0 {
			ps.addText("\n\n")
		}
		ps.addText(renderToolResult(msg))
		ps.addImages(msg.Images)
	}
	ps.addNewMessages(pl.user)
	return ps
}
