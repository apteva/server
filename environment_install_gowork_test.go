package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeGoMod(t *testing.T, dir, module, goVer string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "module " + module + "\n"
	if goVer != "" {
		body += "\ngo " + goVer + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCompareGoDirectiveOrdersPatchNumerically(t *testing.T) {
	// Lexical comparison puts 1.25.12 below 1.25.2, which would generate a
	// workspace directive under a member's own requirement.
	cases := []struct {
		a, b string
		want int
	}{
		{"1.25.12", "1.25.2", 1},
		{"1.25.2", "1.25.12", -1},
		{"1.25.1", "1.25.1", 0},
		{"1.26", "1.25.99", 1},
		{"1.25", "1.25.0", 0},
	}
	for _, c := range cases {
		if got := compareGoDirective(c.a, c.b); got != c.want {
			t.Errorf("compareGoDirective(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestHigherGoDirectiveHandlesMissingValues(t *testing.T) {
	if got := higherGoDirective("", "1.25.1"); got != "1.25.1" {
		t.Errorf(`higherGoDirective("","1.25.1") = %q`, got)
	}
	if got := higherGoDirective("1.25.1", ""); got != "1.25.1" {
		t.Errorf(`higherGoDirective("1.25.1","") = %q`, got)
	}
	if got := higherGoDirective("1.25.0", "1.25.1"); got != "1.25.1" {
		t.Errorf("higherGoDirective picked the lower value: %q", got)
	}
}

func TestGenLocalGoWorkTakesHighestMemberDirective(t *testing.T) {
	root := t.TempDir()
	// app-sdk requires more than the app does — the workspace must satisfy the
	// SDK, or go refuses the generated file outright.
	app := writeGoMod(t, filepath.Join(root, "apps", "mcp", "catalog"), "example.com/catalog", "1.25.0")
	writeGoMod(t, filepath.Join(root, "app-sdk"), "github.com/apteva/app-sdk", "1.25.1")

	path, cleanup, err := genLocalGoWork(app)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "go 1.25.1") {
		t.Errorf("generated go.work does not satisfy app-sdk:\n%s", got)
	}
	if strings.Contains(got, "go 1.25.0\n") {
		t.Errorf("generated go.work used the app's own lower directive:\n%s", got)
	}
}

func TestGoModGoDirectiveMissingFile(t *testing.T) {
	if got := goModGoDirective(filepath.Join(t.TempDir(), "absent")); got != "" {
		t.Errorf("goModGoDirective on a missing go.mod = %q, want empty", got)
	}
}
