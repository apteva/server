package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
	"github.com/apteva/app-sdk/contract"
)

func TestArtifactManifestWireNumbers(t *testing.T) {
	// Source YAML yields ints; a persisted release crosses encoding/json.
	raw := artifactFixtureManifest + "provides:\n  mcp_tools:\n    - name: hello\n      meta:\n        sample_count: 3\n"
	m, err := sdk.ParseManifest([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(m)
	var stored sdk.Manifest
	if err = json.Unmarshal(wire, &stored); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	appRoot := artifactAppRoot(m, filepath.Join(root, "bin"))
	if err = os.MkdirAll(appRoot, 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(appRoot, "apteva.yaml"), []byte(raw), 0600)
	os.WriteFile(filepath.Join(root, "bin"), []byte("verified separately"), 0700)
	if err = validateAppArtifact(root, &stored); err != nil {
		t.Fatal(err)
	}
	stored.Provides.MCPTools[0].Meta["sample_count"] = 4
	if err = validateAppArtifact(root, &stored); err == nil {
		t.Fatal("accepted changed configuration declaration")
	}
}

// Explicitly opt in with a locally built Rust binary. The test never touches a
// running Server, installed app, real project, gateway, or production account.
func TestRustArtifactContract(t *testing.T) {
	executable := os.Getenv("APTEVA_TEST_RUST_APP")
	if executable == "" {
		t.Skip("set APTEVA_TEST_RUST_APP to the hello example binary")
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../app-sdk/rust/examples/hello/apteva.yaml")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := sdk.ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for name, data := range map[string][]byte{"bin": binary, "src/apteva.yaml": raw} {
		if err = tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err = tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err = tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = gz.Close(); err != nil {
		t.Fatal(err)
	}
	localArtifact(t, manifest, archive.Bytes())
	// Also reproduce the stored manifest boundary used on recovery.
	serialized, _ := json.Marshal(manifest)
	if err = json.Unmarshal(serialized, &manifest); err != nil {
		t.Fatal(err)
	}
	sup := NewLocalSupervisor(t.TempDir())
	defer sup.StopAll(time.Second)
	t.Setenv("PATH", t.TempDir())
	baseEnv := map[string]string{"APTEVA_APP_TOKEN": "rust-test-101", "APTEVA_INSTALL_ID": "101", "APTEVA_PROJECT_ID": "test", "APTEVA_APP_CONFIG": `{"greeting":"Hello"}`}
	port, bin, err := sup.BuildFromSource(101, manifest, baseEnv, nil)
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	fixtures := contract.BasicFixtures()
	extraRaw, _ := os.ReadFile("../app-sdk/contract/testdata/hello.json")
	var extra []contract.Fixture
	if err = json.Unmarshal(extraRaw, &extra); err != nil {
		t.Fatal(err)
	}
	fixtures = append(fixtures, extra...)
	check := func() {
		t.Helper()
		if err := contract.Check(context.Background(), url, baseEnv["APTEVA_APP_TOKEN"], fixtures); err != nil {
			t.Fatal(err)
		}
	}
	check()
	// Ordinary bound-agent tools work, while delegated principals fail closed.
	for _, tc := range []struct {
		header, value string
		status        int
	}{
		{"X-Apteva-Caller-Agent", "7", http.StatusOK},
		{"X-Apteva-Subject-Type", "user", http.StatusForbidden},
		{"X-Apteva-Trusted-Principal", "unverified", http.StatusForbidden},
	} {
		req, _ := http.NewRequest(http.MethodPost, url+"/mcp", bytes.NewReader(extra[1].Body))
		req.Header.Set("Authorization", "Bearer "+baseEnv["APTEVA_APP_TOKEN"])
		req.Header.Set(tc.header, tc.value)
		client := &http.Client{Timeout: time.Second}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("%s: HTTP %d", tc.header, resp.StatusCode)
		}
	}
	// A second installation shares verified package bytes, never config or tokens.
	otherEnv := map[string]string{"APTEVA_APP_TOKEN": "rust-test-102", "APTEVA_INSTALL_ID": "102", "APTEVA_PROJECT_ID": "other-project", "APTEVA_APP_CONFIG": `{"greeting":"Bonjour"}`}
	otherPort, otherBin, err := sup.BuildFromSource(102, manifest, otherEnv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if otherBin != bin || otherPort == port {
		t.Fatal("package reuse or port isolation failed")
	}
	otherURL := fmt.Sprintf("http://127.0.0.1:%d", otherPort)
	crossed := []contract.Fixture{{Name: "cross-install token", Method: "GET", Path: "/manifest", Auth: "valid", Status: 401}}
	if err = contract.Check(context.Background(), otherURL, baseEnv["APTEVA_APP_TOKEN"], crossed); err != nil {
		t.Fatal(err)
	}
	otherFixtures := append([]contract.Fixture(nil), extra...)
	otherFixtures[1].Want = json.RawMessage(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(string(otherFixtures[1].Want), "Hello, Ada!", "Bonjour, Ada!"), "101", "102"), `test`, `other-project`))
	if err = contract.Check(context.Background(), otherURL, otherEnv["APTEVA_APP_TOKEN"], otherFixtures); err != nil {
		t.Fatal(err)
	}
	// Update/replacement and failed verification retain the previous process.
	oldPID := sup.PID(101)
	broken := *manifest
	broken.Runtime.Artifacts = map[string]sdk.BundleSpec{localPlatform(): {URL: manifest.Runtime.Artifacts[localPlatform()].URL, SHA256: strings.Repeat("0", 64)}}
	if _, _, err = sup.BuildFromSource(101, &broken, baseEnv, nil); err == nil {
		t.Fatal("bad checksum accepted")
	}
	if sup.PID(101) != oldPID {
		t.Fatal("failed package verification replaced healthy process")
	}
	check()
	newPort, _, err := sup.BuildFromSource(101, manifest, baseEnv, nil)
	if err != nil {
		t.Fatal(err)
	}
	sup.RetireOld(101, time.Second)
	if sup.PID(101) == oldPID {
		t.Fatal("replacement did not start")
	}
	port = newPort
	url = fmt.Sprintf("http://127.0.0.1:%d", port)
	check()
	// No persistent app files are removed by stop/restart.
	dataDir := filepath.Join(sup.cacheDir, manifest.Name, "data", "101")
	if err = os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(dataDir, "keep.txt")
	os.WriteFile(sentinel, []byte("keep"), 0600)
	started := time.Now()
	if err = sup.Stop(101); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 4*time.Second {
		t.Fatal("SIGTERM was not handled gracefully")
	}
	client := &http.Client{Timeout: time.Second}
	if r, e := client.Get(url + "/health"); e == nil {
		r.Body.Close()
		t.Fatal("stopped listener still accepts requests")
	}
	if err = sup.Restart(101, manifest, port, bin, baseEnv); err != nil {
		t.Fatal(err)
	}
	check()
	got, err := os.ReadFile(sentinel)
	if err != nil || string(got) != "keep" {
		t.Fatal("persistent data changed")
	}
}
