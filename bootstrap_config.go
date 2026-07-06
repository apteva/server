package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

type aptevaConfig struct {
	Server    aptevaServerConfig    `yaml:"server"`
	Bootstrap aptevaBootstrapConfig `yaml:"bootstrap"`
}

type aptevaServerConfig struct {
	PublicURL    string `yaml:"public_url"`
	Registration string `yaml:"registration"`
}

type aptevaBootstrapConfig struct {
	Enabled       bool                       `yaml:"enabled"`
	MarkOnboarded bool                       `yaml:"mark_onboarded"`
	Admin         aptevaBootstrapAdminConfig `yaml:"admin"`
	Project       aptevaBootstrapProject     `yaml:"project"`
}

type aptevaBootstrapAdminConfig struct {
	Email        string `yaml:"email"`
	Password     string `yaml:"password"`
	PasswordFile string `yaml:"password_file"`
	PasswordHash string `yaml:"password_hash"`
}

type aptevaBootstrapProject struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Color       string `yaml:"color"`
}

func loadAptevaConfig(dataDir string) (*aptevaConfig, string, error) {
	path := strings.TrimSpace(os.Getenv("APTEVA_CONFIG"))
	if path == "" && strings.TrimSpace(dataDir) != "" {
		candidate := filepath.Join(dataDir, "apteva.yaml")
		if _, err := os.Stat(candidate); err == nil {
			path = candidate
		} else if err != nil && !os.IsNotExist(err) {
			return nil, "", err
		}
	}
	if path == "" {
		return &aptevaConfig{}, "", nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, path, err
	}
	var cfg aptevaConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, path, err
	}
	normalizeAptevaConfig(&cfg)
	if err := validateAptevaConfig(&cfg); err != nil {
		return nil, path, err
	}
	return &cfg, path, nil
}

func normalizeAptevaConfig(cfg *aptevaConfig) {
	if cfg == nil {
		return
	}
	cfg.Server.PublicURL = strings.TrimSpace(cfg.Server.PublicURL)
	cfg.Server.Registration = strings.ToLower(strings.TrimSpace(cfg.Server.Registration))
	cfg.Bootstrap.Admin.Email = strings.TrimSpace(cfg.Bootstrap.Admin.Email)
	cfg.Bootstrap.Admin.Password = strings.TrimSpace(cfg.Bootstrap.Admin.Password)
	cfg.Bootstrap.Admin.PasswordFile = strings.TrimSpace(cfg.Bootstrap.Admin.PasswordFile)
	cfg.Bootstrap.Admin.PasswordHash = strings.TrimSpace(cfg.Bootstrap.Admin.PasswordHash)
	cfg.Bootstrap.Project.Name = strings.TrimSpace(cfg.Bootstrap.Project.Name)
	cfg.Bootstrap.Project.Description = strings.TrimSpace(cfg.Bootstrap.Project.Description)
	cfg.Bootstrap.Project.Color = strings.TrimSpace(cfg.Bootstrap.Project.Color)
}

func validateAptevaConfig(cfg *aptevaConfig) error {
	if cfg == nil {
		return nil
	}
	switch cfg.Server.Registration {
	case "", "open", "locked", "setup":
	default:
		return fmt.Errorf("server.registration must be open, locked, or setup")
	}
	if cfg.Server.PublicURL != "" && !strings.HasPrefix(cfg.Server.PublicURL, "http://") && !strings.HasPrefix(cfg.Server.PublicURL, "https://") {
		return fmt.Errorf("server.public_url must start with http:// or https://")
	}
	return nil
}

func applyAptevaBootstrap(store *Store, cfg *aptevaConfig) (*User, error) {
	if store == nil || cfg == nil || !cfg.Bootstrap.Enabled {
		return nil, nil
	}
	if store.HasUsers() {
		return nil, nil
	}
	admin := cfg.Bootstrap.Admin
	if admin.Email == "" {
		return nil, errors.New("bootstrap.admin.email is required")
	}
	hash := admin.PasswordHash
	if hash == "" {
		password, err := bootstrapPassword(admin)
		if err != nil {
			return nil, err
		}
		if len(password) < 8 {
			return nil, errors.New("bootstrap admin password must be at least 8 characters")
		}
		b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return nil, err
		}
		hash = string(b)
	}
	user, err := store.CreateUser(admin.Email, hash)
	if err != nil {
		return nil, err
	}
	if err := store.SetPlatformRole(user.ID, PlatformAdmin); err != nil {
		return nil, err
	}
	project := cfg.Bootstrap.Project
	if project.Name == "" {
		project.Name = "Default"
	}
	if project.Description == "" {
		project.Description = "Default project"
	}
	p, err := store.CreateProject(user.ID, project.Name, project.Description, project.Color)
	if err != nil {
		return nil, err
	}
	if p != nil {
		_ = store.AddProjectMember(p.ID, user.ID, ProjectOwner, user.ID)
	}
	if cfg.Bootstrap.MarkOnboarded {
		if err := store.MarkUserOnboarded(user.ID); err != nil {
			return nil, err
		}
	}
	_ = store.SetSetting("bootstrap_fingerprint", bootstrapFingerprint(cfg))
	return user, nil
}

func bootstrapPassword(admin aptevaBootstrapAdminConfig) (string, error) {
	if admin.Password != "" && admin.PasswordFile != "" {
		return "", errors.New("set only one of bootstrap.admin.password or bootstrap.admin.password_file")
	}
	if admin.PasswordFile != "" {
		raw, err := os.ReadFile(admin.PasswordFile)
		if err != nil {
			return "", fmt.Errorf("read bootstrap.admin.password_file: %w", err)
		}
		return strings.TrimRight(string(raw), "\r\n"), nil
	}
	if admin.Password == "" {
		return "", errors.New("bootstrap.admin.password, password_file, or password_hash is required")
	}
	return admin.Password, nil
}

func bootstrapFingerprint(cfg *aptevaConfig) string {
	if cfg == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		cfg.Bootstrap.Admin.Email,
		cfg.Bootstrap.Project.Name,
		cfg.Bootstrap.Project.Description,
		cfg.Bootstrap.Project.Color,
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}
