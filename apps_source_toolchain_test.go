package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveGoBinaryUsesSelectedToolchain(t *testing.T) {
	bootstrapDir := t.TempDir()
	selectedRoot := t.TempDir()
	selected := filepath.Join(selectedRoot, "bin", "go")
	if err := os.MkdirAll(filepath.Dir(selected), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(selected, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	bootstrap := filepath.Join(bootstrapDir, "go")
	script := "#!/bin/sh\nif [ \"$1\" = env ] && [ \"$2\" = GOROOT ]; then\n  printf '%s\\n' '" + selectedRoot + "'\n  exit 0\nfi\nexit 1\n"
	if err := os.WriteFile(bootstrap, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bootstrapDir)

	got, err := resolveGoBinary(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != selected {
		t.Fatalf("resolveGoBinary() = %q, want selected toolchain %q", got, selected)
	}
}

func TestResolveGoBinaryFallsBackToBootstrapWrapper(t *testing.T) {
	bootstrapDir := t.TempDir()
	bootstrap := filepath.Join(bootstrapDir, "go")
	if err := os.WriteFile(bootstrap, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bootstrapDir)

	got, err := resolveGoBinary(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != bootstrap {
		t.Fatalf("resolveGoBinary() = %q, want bootstrap wrapper %q", got, bootstrap)
	}
}
