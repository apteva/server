package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/apteva/app-sdk"
)

const configEchoFixtureMain = `package main

import (
	"net/http"
	"os"
)

func main() {
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	http.HandleFunc("/config", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(os.Getenv("APTEVA_APP_CONFIG"))) })
	_ = http.ListenAndServe("127.0.0.1:"+os.Getenv("APTEVA_APP_PORT"), nil)
}
`

const configEchoSchema = `config_schema:
  - { name: softphone_webrtc_enabled, type: boolean, default: "false" }
  - { name: encoder_rate, type: select, options: ["48000", "24000"], default: "48000" }
  - { name: greeting, type: text }
`

func configEchoServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	dir := writeEnvironmentRunnableFixture(t, root, "telephony", "1.0.0", configEchoSchema)
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(configEchoFixtureMain), 0644); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t)
	s.localApps = NewLocalSupervisor(t.TempDir())
	s.installedApps = NewInstalledAppsRegistry()
	s.environments = NewEnvironmentManager(environmentDataRoot(t.TempDir()))
	s.environments.server = s
	s.environments.ResolveEnvironmentSource = func(_, _, _ string) (string, error) { return dir, nil }
	t.Cleanup(func() { s.localApps.StopAll(time.Second) })
	return s, dir
}

func configEchoSpec(id, dir string, cfg map[string]map[string]string) EnvironmentSpec {
	return EnvironmentSpec{
		ID: id, ProjectID: "source-project", GatewayURL: "http://127.0.0.1:5280", CreatorUserID: 1,
		AppSrcDirs: map[string]string{"telephony": dir}, NetworkMode: EdgeBlock, IntegrationMode: IntegrationModeMock,
		HealthBudget: 10 * time.Second, AppConfig: cfg,
	}
}

func TestRuntimeAppConfigReachesCopyOnFirstStart(t *testing.T) {
	s, dir := configEchoServer(t)
	env, err := s.environments.Create(configEchoSpec("rt_config", dir, map[string]map[string]string{
		"telephony": {"softphone_webrtc_enabled": "true", "encoder_rate": "24000"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer s.environments.Destroy(env.ID)
	inst, ok := env.Install("telephony")
	if !ok {
		t.Fatal("telephony copy missing")
	}
	resp, err := http.Get(inst.SidecarURL + "/config")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("APTEVA_APP_CONFIG=%q: %v", body, err)
	}
	if got["softphone_webrtc_enabled"] != "true" || got["encoder_rate"] != "24000" || len(got) != 2 {
		t.Fatalf("sidecar config=%v", got)
	}
	stored, err := decryptInstallConfig(s, inst.InstallID)
	if err != nil || stored["softphone_webrtc_enabled"] != "true" {
		t.Fatalf("stored=%v err=%v", stored, err)
	}
	// The source project's settings are untouched: copies live in the runtime project.
	var sourceRows int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs WHERE project_id='source-project'`).Scan(&sourceRows); err != nil || sourceRows != 0 {
		t.Fatalf("source rows=%d err=%v", sourceRows, err)
	}
}

func TestRuntimeWithoutAppConfigStartsWithEmptyConfig(t *testing.T) {
	s, dir := configEchoServer(t)
	env, err := s.environments.Create(configEchoSpec("rt_noconfig", dir, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer s.environments.Destroy(env.ID)
	inst, _ := env.Install("telephony")
	resp, err := http.Get(inst.SidecarURL + "/config")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.TrimSpace(string(body)) != "{}" {
		t.Fatalf("APTEVA_APP_CONFIG=%q", body)
	}
}

func TestRuntimeAppConfigRejectsBadValuesAndLeavesNothing(t *testing.T) {
	s, dir := configEchoServer(t)
	cases := map[string]map[string]map[string]string{
		"unknown config key":        {"telephony": {"nope": "1"}},
		`must be "true" or "false"`: {"telephony": {"softphone_webrtc_enabled": "yes"}},
		"must be one of":            {"telephony": {"encoder_rate": "8000"}},
		"not in the runtime: crm":   {"telephony": {"greeting": "hi"}, "crm": {"x": "y"}},
	}
	i := 0
	for want, cfg := range cases {
		i++
		_, err := s.environments.Create(configEchoSpec("rt_bad"+string(rune('a'+i)), dir, cfg))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err=%v", want, err)
		}
	}
	var rows int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM app_installs`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("install rows left behind=%d err=%v", rows, err)
	}
}

func TestRuntimeAppConfigShapeLimits(t *testing.T) {
	if err := validateRuntimeAppConfigShape(map[string]map[string]string{"": {"a": "b"}}); err == nil {
		t.Fatal("accepted empty app name")
	}
	if err := validateRuntimeAppConfigShape(map[string]map[string]string{"a": {"": "b"}}); err == nil {
		t.Fatal("accepted empty key")
	}
	if err := validateRuntimeAppConfigShape(map[string]map[string]string{"a": {"k": strings.Repeat("x", maxRuntimeAppConfigValueLen+1)}}); err == nil {
		t.Fatal("accepted oversized value")
	}
	m := &sdk.Manifest{ConfigSchema: []sdk.ConfigField{{Name: "flag", Type: "toggle"}, {Name: "mode", Type: "select"}}}
	if err := validateRuntimeAppConfig(m, map[string]string{"flag": "false", "mode": "anything"}); err != nil {
		t.Fatalf("toggle/select without options: %v", err)
	}
}
