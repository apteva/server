package main

import "testing"

// Only the stale-cache symptom should trigger a cache purge + retry; a genuine
// missing dependency must surface on the first attempt.
func TestBuildCacheSuspectMatchesOnlyTheStaleSymptom(t *testing.T) {
	stale := `pdf.go:34:2: no required module provides package github.com/go-pdf/fpdf; to add it:
	go get github.com/go-pdf/fpdf`
	if !buildCacheSuspect(stale) {
		t.Error("the stale-cache symptom must trigger a retry")
	}

	for _, other := range []string{
		"pdf.go:34:2: undefined: fpdf.New",
		"go: module github.com/foo/bar@v1.2.3: reading ...: 404 Not Found",
		"# github.com/apteva/apps/mcp/billing\n./main.go:10:2: syntax error",
		"",
	} {
		if buildCacheSuspect(other) {
			t.Errorf("must not retry for unrelated failure: %q", other)
		}
	}
}

func TestFirstLineTrimsToTheLeadingDiagnostic(t *testing.T) {
	if got := firstLine("a\nb\nc"); got != "a" {
		t.Errorf("firstLine = %q, want %q", got, "a")
	}
	if got := firstLine("only"); got != "only" {
		t.Errorf("firstLine = %q, want %q", got, "only")
	}
	if got := firstLine(""); got != "" {
		t.Errorf("firstLine = %q, want empty", got)
	}
}

func TestAbsCacheOfReturnsAnAbsolutePath(t *testing.T) {
	if got := absCacheOf("relative/dir"); got == "relative/dir" {
		t.Error("absCacheOf must absolutise a relative cache dir")
	}
	if got := absCacheOf("/already/abs"); got != "/already/abs" {
		t.Errorf("absCacheOf(%q) = %q", "/already/abs", got)
	}
}
