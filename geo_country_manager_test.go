package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDownloadManagedGeoIPDBIPDatabaseWithoutCredentials(t *testing.T) {
	payload := bytes.Repeat([]byte("b"), 4096)
	var gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write(payload)
		_ = gz.Close()
	}))
	defer server.Close()
	oldURL := managedGeoIPDBIPURLForMonth
	oldValidate := validateManagedGeoIPDatabase
	managedGeoIPDBIPURLForMonth = func(time.Time) string { return server.URL }
	validateManagedGeoIPDatabase = func(string) error { return nil }
	defer func() {
		managedGeoIPDBIPURLForMonth = oldURL
		validateManagedGeoIPDatabase = oldValidate
	}()

	destination := filepath.Join(t.TempDir(), "geoip", "country.mmdb")
	if err := downloadManagedGeoIP(context.Background(), server.Client(), managedGeoIPConfig{Source: "dbip"}, destination); err != nil {
		t.Fatal(err)
	}
	if gotAuthorization != "" {
		t.Fatalf("DB-IP download sent authorization header %q", gotAuthorization)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("decompressed DB-IP database differs from response")
	}
}

func TestDownloadManagedGeoIPTestDatabaseAtomically(t *testing.T) {
	payload := bytes.Repeat([]byte("m"), 2048)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	oldURL := managedGeoIPTestURL
	oldValidate := validateManagedGeoIPDatabase
	managedGeoIPTestURL = server.URL
	validateManagedGeoIPDatabase = func(string) error { return nil }
	defer func() { managedGeoIPTestURL = oldURL; validateManagedGeoIPDatabase = oldValidate }()

	destination := filepath.Join(t.TempDir(), "geoip", "country.mmdb")
	if err := downloadManagedGeoIP(context.Background(), server.Client(), managedGeoIPConfig{Source: "test"}, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("downloaded database differs from response")
	}
}

func TestDownloadManagedGeoIPMaxMindArchiveUsesBasicAuth(t *testing.T) {
	payload := bytes.Repeat([]byte("d"), 4096)
	var gotUser, gotPassword string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPassword, _ = r.BasicAuth()
		gz := gzip.NewWriter(w)
		tw := tar.NewWriter(gz)
		_ = tw.WriteHeader(&tar.Header{Name: "GeoLite2-Country_1/GeoLite2-Country.mmdb", Mode: 0600, Size: int64(len(payload))})
		_, _ = tw.Write(payload)
		_ = tw.Close()
		_ = gz.Close()
	}))
	defer server.Close()
	oldURL := managedGeoIPMaxMindURL
	oldValidate := validateManagedGeoIPDatabase
	managedGeoIPMaxMindURL = server.URL
	validateManagedGeoIPDatabase = func(string) error { return nil }
	defer func() { managedGeoIPMaxMindURL = oldURL; validateManagedGeoIPDatabase = oldValidate }()

	destination := filepath.Join(t.TempDir(), "country.mmdb")
	config := managedGeoIPConfig{Source: "maxmind", AccountID: "123", LicenseKey: "secret"}
	if err := downloadManagedGeoIP(context.Background(), server.Client(), config, destination); err != nil {
		t.Fatal(err)
	}
	if gotUser != "123" || gotPassword != "secret" {
		t.Fatalf("basic auth=%q %q", gotUser, gotPassword)
	}
	got, _ := os.ReadFile(destination)
	if !bytes.Equal(got, payload) {
		t.Fatal("archive database differs from response")
	}
}

func TestDownloadManagedGeoIPPreservesExistingDatabaseOnFailure(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "country.mmdb")
	original := bytes.Repeat([]byte("o"), 2048)
	if err := os.WriteFile(destination, original, 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer server.Close()
	oldURL := managedGeoIPTestURL
	managedGeoIPTestURL = server.URL
	defer func() { managedGeoIPTestURL = oldURL }()
	if err := downloadManagedGeoIP(context.Background(), server.Client(), managedGeoIPConfig{Source: "test"}, destination); err == nil {
		t.Fatal("expected download failure")
	}
	got, _ := os.ReadFile(destination)
	if !bytes.Equal(got, original) {
		t.Fatal("failed refresh replaced existing database")
	}
}

func TestManagedGeoIPDisabledDoesNotLoadRetainedDatabase(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "geoip")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "GeoIP2-Country.mmdb"), bytes.Repeat([]byte("x"), 2048), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"enabled":false,"source":"test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	lookup := newManagedGeoCountryLookup(root)
	manager, ok := lookup.(*managedGeoCountryLookup)
	if !ok {
		t.Fatalf("lookup type %T", lookup)
	}
	defer manager.Close()
	if manager.current.Load() != nil {
		t.Fatal("disabled configuration loaded retained database")
	}
}

func TestEnsureDefaultManagedGeoIPConfigEnablesDBIPOnlyWhenAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "geoip", "config.json")
	config, err := ensureDefaultManagedGeoIPConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || config.Source != "dbip" {
		t.Fatalf("default config=%+v", config)
	}
	if err := writeManagedGeoIPConfig(path, managedGeoIPConfig{Enabled: false, Source: "dbip"}); err != nil {
		t.Fatal(err)
	}
	config, err = ensureDefaultManagedGeoIPConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Enabled || config.Source != "dbip" {
		t.Fatalf("explicitly disabled config was overwritten: %+v", config)
	}
}

func TestManagedGeoIPDBIPRefreshesAtMonthBoundary(t *testing.T) {
	info := fakeGeoIPFileInfo{modified: time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)}
	config := managedGeoIPConfig{Enabled: true, Source: "dbip"}
	if managedGeoIPRefreshDue(config, info, time.Date(2026, time.August, 31, 23, 0, 0, 0, time.UTC)) {
		t.Fatal("DB-IP refresh was due again within the same monthly edition")
	}
	if !managedGeoIPRefreshDue(config, info, time.Date(2026, time.September, 1, 1, 0, 0, 0, time.UTC)) {
		t.Fatal("DB-IP refresh was not due at the next month boundary")
	}
}

type fakeGeoIPFileInfo struct {
	modified time.Time
}

func (f fakeGeoIPFileInfo) Name() string       { return "country.mmdb" }
func (f fakeGeoIPFileInfo) Size() int64        { return 4096 }
func (f fakeGeoIPFileInfo) Mode() os.FileMode  { return 0600 }
func (f fakeGeoIPFileInfo) ModTime() time.Time { return f.modified }
func (f fakeGeoIPFileInfo) IsDir() bool        { return false }
func (f fakeGeoIPFileInfo) Sys() any           { return nil }

func TestUpdateManagedGeoIPConfigPersistsPrivateConfiguration(t *testing.T) {
	root := t.TempDir()
	s := &Server{dataDir: root}
	if err := s.updateManagedGeoIPConfig(true, "maxmind", "123", "secret"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "geoip", "config.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("config mode=%o", info.Mode().Perm())
	}
	config, err := readManagedGeoIPConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || config.AccountID != "123" || config.LicenseKey != "secret" {
		t.Fatalf("config=%+v", config)
	}
	state := s.geoIPSettingsState()
	if state["has_license_key"] != true {
		t.Fatalf("state=%v", state)
	}
	if _, exposed := state["license_key"]; exposed {
		t.Fatal("settings state exposed license key")
	}
}

func TestUpdateManagedGeoIPConfigRequiresProductionCredentials(t *testing.T) {
	s := &Server{dataDir: t.TempDir()}
	if err := s.updateManagedGeoIPConfig(true, "maxmind", "123", ""); err == nil {
		t.Fatal("expected missing license key error")
	}
	if err := s.updateManagedGeoIPConfig(true, "test", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.updateManagedGeoIPConfig(true, "dbip", "", ""); err != nil {
		t.Fatal(err)
	}
}
