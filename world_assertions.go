package main

// world_assertions.go — deterministic assertions for World runs.
//
// The meta-agent judge grades open-ended quality; assertions catch
// regressions deterministically. A run passes only if assertions pass AND
// the judge passes. Three kinds, each reading a different World output:
//
//   edge       — over the WorldEdge's recorded calls. "exactly 1 POST to
//                api.twitter.com/2/tweets", "no call to billing".
//   state      — a SQL scalar against an in-world sidecar's SQLite DB.
//                Only possible because Worlds do REAL DB writes:
//                "crm has 1 contact where email='x'".
//   trajectory — over the ordered list of tool names the agent called.
//                "called create_invoice", "never called refund_customer",
//                "looked_up before charged".
//
// State assertions open the sidecar DB read-only with the same driver the
// store uses (modernc.org/sqlite, registered in store.go).

import (
	"database/sql"
	"fmt"
	"strings"
)

type AssertionKind string

const (
	AssertEdge       AssertionKind = "edge"
	AssertState      AssertionKind = "state"
	AssertTrajectory AssertionKind = "trajectory"
)

// Assertion is one declarative check. Exactly one of Edge/State/Trajectory
// is set, matching Kind.
type Assertion struct {
	Name       string               `json:"name"`
	Kind       AssertionKind        `json:"kind"`
	Edge       *EdgeAssertion       `json:"edge,omitempty"`
	State      *StateAssertion      `json:"state,omitempty"`
	Trajectory *TrajectoryAssertion `json:"trajectory,omitempty"`
}

// EdgeAssertion counts outbound calls matching a filter and compares the
// count. Host is exact; Path is a prefix; Method is case-insensitive. Any
// of them empty means "don't filter on this". Use Max=0 for "no call".
type EdgeAssertion struct {
	Host   string `json:"host,omitempty"`
	Path   string `json:"path,omitempty"`
	Method string `json:"method,omitempty"`
	Count  *int   `json:"count,omitempty"` // exact
	Min    *int   `json:"min,omitempty"`
	Max    *int   `json:"max,omitempty"`
}

// StateAssertion runs a single-scalar SQL query against an in-world
// sidecar's DB and compares the integer result.
type StateAssertion struct {
	App    string `json:"app"`   // sidecar name whose DB to query
	Query  string `json:"query"` // must return one row, one integer column
	Equals *int64 `json:"equals,omitempty"`
	Min    *int64 `json:"min,omitempty"`
	Max    *int64 `json:"max,omitempty"`
}

// TrajectoryAssertion checks the ordered tool-call sequence.
type TrajectoryAssertion struct {
	ToolCalled    string `json:"tool_called,omitempty"`
	ToolNotCalled string `json:"tool_not_called,omitempty"`
	// Before asserts ToolCalled occurs before this tool (requires ToolCalled).
	Before string `json:"before,omitempty"`
}

// AssertionResult is the outcome of one assertion.
type AssertionResult struct {
	Name   string        `json:"name"`
	Kind   AssertionKind `json:"kind"`
	Pass   bool          `json:"pass"`
	Detail string        `json:"detail"`
}

// AssertionInputs is everything the evaluator can read from a finished run.
type AssertionInputs struct {
	EdgeCalls    []InterceptedCall
	ToolSequence []string
	// AppDBPath resolves an in-world sidecar name to its SQLite file path.
	// Returns ok=false when the app isn't in the world.
	AppDBPath func(app string) (path string, ok bool)
}

// EvaluateAssertions runs every assertion and returns one result each.
// allPass is true only if every assertion passed.
func EvaluateAssertions(asserts []Assertion, in AssertionInputs) (results []AssertionResult, allPass bool) {
	allPass = true
	for _, a := range asserts {
		r := AssertionResult{Name: a.Name, Kind: a.Kind}
		switch a.Kind {
		case AssertEdge:
			r.Pass, r.Detail = evalEdge(a.Edge, in.EdgeCalls)
		case AssertState:
			r.Pass, r.Detail = evalState(a.State, in.AppDBPath)
		case AssertTrajectory:
			r.Pass, r.Detail = evalTrajectory(a.Trajectory, in.ToolSequence)
		default:
			r.Pass, r.Detail = false, "unknown assertion kind: "+string(a.Kind)
		}
		if !r.Pass {
			allPass = false
		}
		results = append(results, r)
	}
	return results, allPass
}

func evalEdge(ea *EdgeAssertion, calls []InterceptedCall) (bool, string) {
	if ea == nil {
		return false, "edge assertion missing body"
	}
	n := 0
	for _, c := range calls {
		if ea.Host != "" && c.Host != ea.Host {
			continue
		}
		if ea.Path != "" && !strings.HasPrefix(c.Path, ea.Path) {
			continue
		}
		if ea.Method != "" && !strings.EqualFold(c.Method, ea.Method) {
			continue
		}
		n++
	}
	desc := fmt.Sprintf("%d call(s) matched %s %s%s", n, orAny(ea.Method), orAny(ea.Host), ea.Path)
	if ea.Count != nil && n != *ea.Count {
		return false, fmt.Sprintf("expected exactly %d, got %d — %s", *ea.Count, n, desc)
	}
	if ea.Min != nil && n < *ea.Min {
		return false, fmt.Sprintf("expected >= %d, got %d — %s", *ea.Min, n, desc)
	}
	if ea.Max != nil && n > *ea.Max {
		return false, fmt.Sprintf("expected <= %d, got %d — %s", *ea.Max, n, desc)
	}
	return true, desc
}

func evalState(sa *StateAssertion, resolve func(string) (string, bool)) (bool, string) {
	if sa == nil {
		return false, "state assertion missing body"
	}
	if resolve == nil {
		return false, "no app DB resolver available"
	}
	path, ok := resolve(sa.App)
	if !ok {
		return false, fmt.Sprintf("app %q not in world", sa.App)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return false, "open db: " + err.Error()
	}
	defer db.Close()
	var got int64
	if err := db.QueryRow(sa.Query).Scan(&got); err != nil {
		return false, "query: " + err.Error()
	}
	desc := fmt.Sprintf("%s.db query returned %d", sa.App, got)
	if sa.Equals != nil && got != *sa.Equals {
		return false, fmt.Sprintf("expected %d, got %d — %s", *sa.Equals, got, desc)
	}
	if sa.Min != nil && got < *sa.Min {
		return false, fmt.Sprintf("expected >= %d, got %d — %s", *sa.Min, got, desc)
	}
	if sa.Max != nil && got > *sa.Max {
		return false, fmt.Sprintf("expected <= %d, got %d — %s", *sa.Max, got, desc)
	}
	return true, desc
}

func evalTrajectory(ta *TrajectoryAssertion, seq []string) (bool, string) {
	if ta == nil {
		return false, "trajectory assertion missing body"
	}
	idx := func(tool string) int {
		for i, t := range seq {
			if t == tool {
				return i
			}
		}
		return -1
	}
	if ta.ToolNotCalled != "" {
		if idx(ta.ToolNotCalled) >= 0 {
			return false, fmt.Sprintf("tool %q was called but should not have been", ta.ToolNotCalled)
		}
	}
	if ta.ToolCalled != "" {
		i := idx(ta.ToolCalled)
		if i < 0 {
			return false, fmt.Sprintf("tool %q was never called", ta.ToolCalled)
		}
		if ta.Before != "" {
			j := idx(ta.Before)
			if j < 0 {
				return false, fmt.Sprintf("ordering: %q never called (cannot order %q before it)", ta.Before, ta.ToolCalled)
			}
			if i >= j {
				return false, fmt.Sprintf("ordering: %q (idx %d) was not before %q (idx %d)", ta.ToolCalled, i, ta.Before, j)
			}
		}
	}
	return true, "trajectory ok"
}

func orAny(s string) string {
	if s == "" {
		return "*"
	}
	return s
}
