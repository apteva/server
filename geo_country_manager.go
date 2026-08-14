package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	geoIPManagedRefreshAge = 24 * time.Hour
	geoIPManagedPoll       = 6 * time.Hour
)

var (
	managedGeoIPTestURL          = "https://raw.githubusercontent.com/maxmind/MaxMind-DB/main/test-data/GeoIP2-Country-Test.mmdb"
	managedGeoIPMaxMindURL       = "https://download.maxmind.com/geoip/databases/GeoLite2-Country/download?suffix=tar.gz"
	managedGeoIPDBIPURLForMonth  = dbIPCountryLiteURL
	validateManagedGeoIPDatabase = func(path string) error {
		lookup, err := newMaxMindCountryLookup(path)
		if err != nil {
			return err
		}
		defer lookup.Close()
		if !strings.Contains(strings.ToLower(lookup.reader.Metadata.DatabaseType), "country") {
			return fmt.Errorf("unexpected country database type %q", lookup.reader.Metadata.DatabaseType)
		}
		return nil
	}
)

type managedGeoIPConfig struct {
	Enabled    bool   `json:"enabled"`
	Source     string `json:"source"`
	AccountID  string `json:"account_id,omitempty"`
	LicenseKey string `json:"license_key,omitempty"`
}

func dbIPCountryLiteURL(now time.Time) string {
	return fmt.Sprintf("https://download.db-ip.com/free/dbip-country-lite-%s.mmdb.gz", now.UTC().Format("2006-01"))
}

// managedGeoCountryLookup owns the operational lifecycle around the hot-path
// MMDB reader. A new installation defaults to the anonymous DB-IP Country
// Lite database; the CLI and Settings can select MaxMind or the public test
// fixture instead. Downloads and refreshes are asynchronous and atomic. A
// failed refresh never replaces the last known-good reader or delays readiness.
type managedGeoCountryLookup struct {
	dataDir    string
	dbPath     string
	configPath string
	client     *http.Client

	current   atomic.Pointer[maxMindCountryLookup]
	updateMu  sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
	refreshCh chan bool
}

func newManagedGeoCountryLookup(dataDir string) countryLookup {
	// An explicit path remains an advanced operator override. It is never
	// downloaded over or coupled to CLI-managed credentials.
	if strings.TrimSpace(os.Getenv(geoCountryDBEnv)) != "" {
		return geoCountryLookupFromEnv()
	}
	dir := filepath.Join(dataDir, "geoip")
	m := &managedGeoCountryLookup{
		dataDir:    dataDir,
		dbPath:     filepath.Join(dir, "GeoIP2-Country.mmdb"),
		configPath: filepath.Join(dir, "config.json"),
		client:     &http.Client{Timeout: 5 * time.Minute},
		done:       make(chan struct{}),
		refreshCh:  make(chan bool, 1),
	}
	config, err := ensureDefaultManagedGeoIPConfig(m.configPath)
	if err != nil {
		log.Printf("[GEOIP] initialize default configuration: %v", err)
	} else if config.Enabled {
		m.loadExisting()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	go m.run(ctx)
	return m
}

func (m *managedGeoCountryLookup) Country(ip netip.Addr) (string, bool) {
	if m == nil {
		return "", false
	}
	lookup := m.current.Load()
	if lookup == nil {
		return "", false
	}
	return lookup.Country(ip)
}

func (m *managedGeoCountryLookup) Close() error {
	if m == nil {
		return nil
	}
	var closeErr error
	m.closeOnce.Do(func() {
		if m.cancel != nil {
			m.cancel()
		}
		if m.done != nil {
			<-m.done
		}
		if lookup := m.current.Swap(nil); lookup != nil {
			closeErr = lookup.Close()
		}
	})
	return closeErr
}

func (m *managedGeoCountryLookup) run(ctx context.Context) {
	defer close(m.done)
	m.refreshIfDue(ctx)
	ticker := time.NewTicker(geoIPManagedPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case force := <-m.refreshCh:
			m.refresh(ctx, force)
		case <-ticker.C:
			m.refreshIfDue(ctx)
		}
	}
}

func (m *managedGeoCountryLookup) loadExisting() {
	if _, err := os.Stat(m.dbPath); err != nil {
		return
	}
	lookup, err := newMaxMindCountryLookup(m.dbPath)
	if err != nil {
		log.Printf("[GEOIP] managed country database ignored: %v", err)
		return
	}
	m.installLookup(lookup)
	log.Printf("[GEOIP] managed country database loaded path=%s type=%s build=%s",
		m.dbPath, lookup.reader.Metadata.DatabaseType, lookup.reader.Metadata.BuildTime().UTC().Format(time.RFC3339))
}

func (m *managedGeoCountryLookup) installLookup(lookup *maxMindCountryLookup) {
	old := m.current.Swap(lookup)
	if old != nil {
		_ = old.Close()
	}
}

func (m *managedGeoCountryLookup) refreshIfDue(ctx context.Context) {
	m.refresh(ctx, false)
}

func (m *managedGeoCountryLookup) requestRefresh(force bool) {
	if m == nil {
		return
	}
	select {
	case m.refreshCh <- force:
	default:
	}
}

func (m *managedGeoCountryLookup) refresh(ctx context.Context, force bool) {
	m.updateMu.Lock()
	defer m.updateMu.Unlock()
	config, err := readManagedGeoIPConfig(m.configPath)
	if err != nil || !config.Enabled {
		if lookup := m.current.Swap(nil); lookup != nil {
			_ = lookup.Close()
		}
		return
	}
	if info, statErr := os.Stat(m.dbPath); !force && statErr == nil && !managedGeoIPRefreshDue(config, info, time.Now()) {
		if m.current.Load() == nil {
			m.loadExisting()
		}
		if m.current.Load() != nil {
			return
		}
	}
	if err := downloadManagedGeoIP(ctx, m.client, config, m.dbPath); err != nil {
		if !errors.Is(err, context.Canceled) {
			log.Printf("[GEOIP] background update failed: %v", err)
		}
		return
	}
	lookup, err := newMaxMindCountryLookup(m.dbPath)
	if err != nil {
		log.Printf("[GEOIP] downloaded database rejected: %v", err)
		return
	}
	m.installLookup(lookup)
	log.Printf("[GEOIP] country database updated source=%s type=%s build=%s",
		config.Source, lookup.reader.Metadata.DatabaseType, lookup.reader.Metadata.BuildTime().UTC().Format(time.RFC3339))
}

func managedGeoIPRefreshDue(config managedGeoIPConfig, info os.FileInfo, now time.Time) bool {
	if info == nil {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(config.Source), "dbip") {
		modified := info.ModTime().UTC()
		now = now.UTC()
		return modified.Year() != now.Year() || modified.Month() != now.Month()
	}
	return now.Sub(info.ModTime()) >= geoIPManagedRefreshAge
}

func writeManagedGeoIPConfig(path string, config managedGeoIPConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func ensureDefaultManagedGeoIPConfig(path string) (managedGeoIPConfig, error) {
	config, err := readManagedGeoIPConfig(path)
	if err == nil {
		return config, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return managedGeoIPConfig{}, err
	}
	config = managedGeoIPConfig{Enabled: true, Source: "dbip"}
	if err := writeManagedGeoIPConfig(path, config); err != nil {
		return managedGeoIPConfig{}, err
	}
	return config, nil
}

func (s *Server) geoIPSettingsState() map[string]any {
	if override := strings.TrimSpace(os.Getenv(geoCountryDBEnv)); override != "" {
		info, err := os.Stat(override)
		state := map[string]any{
			"enabled":          s != nil && s.geoCountry != nil,
			"active":           s != nil && s.geoCountry != nil,
			"source":           "environment",
			"managed":          false,
			"database_path":    override,
			"database_present": err == nil,
			"has_license_key":  false,
		}
		if err == nil {
			state["updated_at"] = info.ModTime().UTC().Format(time.RFC3339)
		}
		return state
	}
	path := filepath.Join(s.dataDir, "geoip", "config.json")
	config, _ := readManagedGeoIPConfig(path)
	dbPath := filepath.Join(s.dataDir, "geoip", "GeoIP2-Country.mmdb")
	info, err := os.Stat(dbPath)
	active := false
	if manager, ok := s.geoCountry.(*managedGeoCountryLookup); ok {
		active = manager.current.Load() != nil
	}
	state := map[string]any{
		"enabled":          config.Enabled,
		"active":           active,
		"source":           config.Source,
		"managed":          true,
		"account_id":       config.AccountID,
		"has_license_key":  config.LicenseKey != "",
		"database_path":    dbPath,
		"database_present": err == nil,
	}
	if config.Source == "dbip" {
		state["attribution"] = map[string]any{
			"name":    "DB-IP",
			"url":     "https://db-ip.com",
			"license": "CC BY 4.0",
		}
	}
	if err == nil {
		state["updated_at"] = info.ModTime().UTC().Format(time.RFC3339)
		state["database_size"] = info.Size()
	}
	return state
}

func (s *Server) updateManagedGeoIPConfig(enabled bool, source, accountID, licenseKey string) error {
	if strings.TrimSpace(os.Getenv(geoCountryDBEnv)) != "" {
		return errors.New("GeoIP is controlled by APTEVA_GEOIP_COUNTRY_DB")
	}
	path := filepath.Join(s.dataDir, "geoip", "config.json")
	config, _ := readManagedGeoIPConfig(path)
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "" {
		source = config.Source
	}
	if source == "" {
		source = "dbip"
	}
	if source != "dbip" && source != "maxmind" && source != "test" {
		return errors.New("geoip_source must be dbip, maxmind, or test")
	}
	config.Enabled = enabled
	config.Source = source
	if strings.TrimSpace(accountID) != "" {
		config.AccountID = strings.TrimSpace(accountID)
	}
	if strings.TrimSpace(licenseKey) != "" {
		config.LicenseKey = strings.TrimSpace(licenseKey)
	}
	if enabled && source == "maxmind" && (config.AccountID == "" || config.LicenseKey == "") {
		return errors.New("MaxMind account ID and license key are required")
	}
	if err := writeManagedGeoIPConfig(path, config); err != nil {
		return err
	}
	if manager, ok := s.geoCountry.(*managedGeoCountryLookup); ok {
		manager.requestRefresh(true)
	}
	return nil
}

func readManagedGeoIPConfig(path string) (managedGeoIPConfig, error) {
	var config managedGeoIPConfig
	raw, err := os.ReadFile(path)
	if err != nil {
		return config, err
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return config, err
	}
	return config, nil
}

func downloadManagedGeoIP(ctx context.Context, client *http.Client, config managedGeoIPConfig, destination string) error {
	if client == nil {
		client = http.DefaultClient
	}
	var url string
	switch config.Source {
	case "dbip":
		url = managedGeoIPDBIPURLForMonth(time.Now())
	case "maxmind":
		if strings.TrimSpace(config.AccountID) == "" || strings.TrimSpace(config.LicenseKey) == "" {
			return errors.New("MaxMind credentials are incomplete")
		}
		url = managedGeoIPMaxMindURL
	case "test":
		url = managedGeoIPTestURL
	default:
		return fmt.Errorf("unsupported GeoIP source %q", config.Source)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if config.Source == "maxmind" {
		req.SetBasicAuth(config.AccountID, config.LicenseKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".country-*.mmdb")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	var copyErr error
	switch config.Source {
	case "maxmind":
		copyErr = copyManagedCountryMMDBFromTarGZ(tmp, resp.Body)
	case "dbip":
		copyErr = copyManagedCountryMMDBFromGZIP(tmp, resp.Body)
	default:
		_, copyErr = io.Copy(tmp, io.LimitReader(resp.Body, 128<<20))
	}
	if copyErr == nil {
		copyErr = tmp.Sync()
	}
	if closeErr := tmp.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return copyErr
	}
	info, err := os.Stat(tmpName)
	if err != nil {
		return err
	}
	if info.Size() < 1024 {
		return errors.New("downloaded country database is unexpectedly small")
	}
	if err := validateManagedGeoIPDatabase(tmpName); err != nil {
		return fmt.Errorf("validate country database: %w", err)
	}
	return os.Rename(tmpName, destination)
}

func copyManagedCountryMMDBFromGZIP(dst io.Writer, src io.Reader) error {
	gz, err := gzip.NewReader(src)
	if err != nil {
		return fmt.Errorf("open DB-IP archive: %w", err)
	}
	defer gz.Close()
	limited := &io.LimitedReader{R: gz, N: (128 << 20) + 1}
	written, err := io.Copy(dst, limited)
	if err != nil {
		return fmt.Errorf("read DB-IP archive: %w", err)
	}
	if written > 128<<20 {
		return errors.New("DB-IP country database exceeds the size limit")
	}
	return nil
}

func copyManagedCountryMMDBFromTarGZ(dst io.Writer, src io.Reader) error {
	gz, err := gzip.NewReader(src)
	if err != nil {
		return fmt.Errorf("open MaxMind archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read MaxMind archive: %w", err)
		}
		if header.Typeflag == tar.TypeReg && strings.HasSuffix(header.Name, ".mmdb") {
			if header.Size <= 0 || header.Size > 128<<20 {
				return errors.New("invalid country database size")
			}
			_, err = io.CopyN(dst, tr, header.Size)
			return err
		}
	}
	return errors.New("MaxMind archive did not contain a country database")
}
