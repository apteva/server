package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoBuildGOPATHWithoutHome(t *testing.T) {
	for _, tc := range []struct {
		name      string
		inherited string
		overrides []string
		want      string
	}{
		{name: "missing environment"},
		{name: "inherited path", inherited: "/explicit/inherited", want: "/explicit/inherited"},
		{name: "caller override", inherited: "/explicit/inherited", overrides: []string{"GOPATH=/explicit/caller"}, want: "/explicit/caller"},
		{name: "empty caller override", inherited: "/explicit/inherited", overrides: []string{"GOPATH="}},
		{name: "last caller override", overrides: []string{"GOPATH=/first", "GOPATH=/last"}, want: "/last"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			// Exercise the actual goBuild child-process environment, including
			// exec.Cmd's duplicate-variable handling, without a network dependency.
			fakeGo := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$GOPATH\" \"${HOME-unset}\" \"$GOCACHE\" \"$GOMODCACHE\" > \"$3\"\n"
			if err := os.WriteFile(filepath.Join(root, "go"), []byte(fakeGo), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/build\n\ngo 1.22\n"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", root)
			t.Setenv("HOME", "")
			if err := os.Unsetenv("HOME"); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GOPATH", tc.inherited)
			if tc.inherited == "" {
				if err := os.Unsetenv("GOPATH"); err != nil {
					t.Fatal(err)
				}
			}
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			cache := filepath.Join(root, "managed-cache")
			relativeCache, err := filepath.Rel(cwd, cache)
			if err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(root, "app")
			if err := goBuild(root, ".", bin, relativeCache, tc.overrides, nil); err != nil {
				t.Fatal(err)
			}
			out, err := os.ReadFile(bin)
			if err != nil {
				t.Fatal(err)
			}
			want := tc.want
			if want == "" {
				want = filepath.Join(cache, "gopath")
				if err := os.WriteFile(filepath.Join(want, "writable"), []byte("ok"), 0600); err != nil {
					t.Fatalf("fallback GOPATH is not writable: %v", err)
				}
			}
			expected := strings.Join([]string{want, "unset", filepath.Join(cache, "gocache"), filepath.Join(cache, "gomodcache"), ""}, "\n")
			if string(out) != expected {
				t.Fatalf("child environment = %q, want %q", out, expected)
			}
			if os.Getenv("GOPATH") != tc.inherited {
				t.Fatal("build changed the server's GOPATH")
			}
		})
	}
}
