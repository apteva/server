package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sdk "github.com/apteva/app-sdk"
	"gopkg.in/yaml.v3"
)

// registerAppSkills walks one install's manifest, resolves each
// declared skill's body (inline or via body_file), parses any SKILL.md
// frontmatter that body_file points at, and writes one skills row
// per entry. Replaces existing rows for the install — safe to run on
// install + every upgrade.
//
// projectID is the install's scope. Empty string for global-scope
// installs (rare; storage etc. are project-scoped).
//
// The `fetchBodyFile` closure resolves a body_file path against the
// app's source — github raw URL for git-installed apps, manual
// upload payload for manual installs. Caller provides it because
// install vs upgrade fetch from different surfaces.
func (s *Server) registerAppSkills(
	installID int64,
	appName string,
	projectID string,
	skills []sdk.Skill,
	fetchBodyFile func(path string) (string, error),
) error {
	tx, err := s.store.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	// Wipe the install's existing skills so an upgrade that drops a
	// skill cleans up the row. Cheap — installs ship a handful, not
	// hundreds.
	if _, err := tx.Exec(`DELETE FROM skills WHERE install_id = ?`, installID); err != nil {
		return fmt.Errorf("clear: %w", err)
	}

	for i, skill := range skills {
		resolved, err := resolveSkill(skill, fetchBodyFile)
		if err != nil {
			return fmt.Errorf("skill[%d] %s: %w", i, skill.Name, err)
		}
		// Slug = "<app>:<name>" so user-authored skills never collide
		// with app-shipped ones (those use "user:<name>").
		slug := appName + ":" + resolved.Name
		metaJSON, _ := json.Marshal(coalesceMetadata(resolved.Metadata))
		if _, err := tx.Exec(`
			INSERT INTO skills (slug, name, description, body, source, install_id, project_id, command, metadata_json)
			VALUES (?, ?, ?, ?, 'app', ?, ?, ?, ?)`,
			slug, resolved.Name, resolved.Description, resolved.Body,
			installID, projectID, resolved.Command, string(metaJSON),
		); err != nil {
			return fmt.Errorf("insert %s: %w", slug, err)
		}
	}
	return tx.Commit()
}

// resolveSkill collapses a manifest-declared skill (which may carry
// body XOR body_file) into a fully-resolved skill with body in
// memory. When body_file is set, fetchBodyFile reads the file +
// the result is parsed as canonical SKILL.md — frontmatter values
// override / fill in the manifest's fields, body becomes the body.
//
// This is the key bit that lets apteva interop with Anthropic's
// open SKILL.md format: an app can write the entire skill as a
// SKILL.md file (one document, one source of truth) and
// `body_file:` points at it. The yaml entries in apteva.yaml
// become a sanity-check / preview the dashboard can show before
// actually fetching.
func resolveSkill(s sdk.Skill, fetchBodyFile func(string) (string, error)) (sdk.Skill, error) {
	if s.Name == "" {
		return s, errors.New("name required")
	}
	if (s.Body == "" && s.BodyFile == "") || (s.Body != "" && s.BodyFile != "") {
		return s, errors.New("exactly one of body or body_file must be set")
	}
	if s.BodyFile != "" {
		raw, err := fetchBodyFile(s.BodyFile)
		if err != nil {
			return s, fmt.Errorf("fetch %s: %w", s.BodyFile, err)
		}
		front, body, err := parseSkillMD(raw)
		if err != nil {
			return s, fmt.Errorf("parse SKILL.md: %w", err)
		}
		s.Body = body
		// Frontmatter wins — the file is the canonical source. Manifest
		// entries are a preview.
		if front.Name != "" {
			s.Name = front.Name
		}
		if front.Description != "" {
			s.Description = front.Description
		}
		if front.Command != "" {
			s.Command = front.Command
		}
		if len(front.Metadata) > 0 {
			if s.Metadata == nil {
				s.Metadata = map[string]any{}
			}
			for k, v := range front.Metadata {
				if _, present := s.Metadata[k]; !present {
					s.Metadata[k] = v
				}
			}
		}
	}
	if s.Description == "" {
		return s, errors.New("description required (in manifest or SKILL.md frontmatter)")
	}
	return s, nil
}

// skillFrontmatter is the YAML at the top of a SKILL.md file.
// Matches Anthropic's open spec: name + description required,
// the rest are optional and apteva-specific extensions live in
// `metadata`.
type skillFrontmatter struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Command     string         `yaml:"command"`
	Metadata    map[string]any `yaml:"metadata"`
}

// parseSkillMD splits a canonical SKILL.md document into
// frontmatter (parsed YAML) and body (the markdown after the
// closing ---). Tolerates files with no frontmatter — those
// have an empty struct + the whole file as body, useful when
// an app wants to ship a body-only skill and declare metadata
// entirely in apteva.yaml.
func parseSkillMD(s string) (skillFrontmatter, string, error) {
	var fm skillFrontmatter
	trimmed := strings.TrimLeft(s, "\r\n\t ")
	if !strings.HasPrefix(trimmed, "---") {
		return fm, s, nil
	}
	// Find the closing --- on its own line. We slice the document
	// at that boundary; everything before is yaml, after is body.
	rest := strings.TrimPrefix(trimmed, "---")
	rest = strings.TrimLeft(rest, "\r\n")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return fm, s, errors.New("frontmatter has no closing ---")
	}
	yamlBlock := rest[:end]
	body := strings.TrimLeft(rest[end+len("\n---"):], "\r\n")
	if err := yaml.Unmarshal([]byte(yamlBlock), &fm); err != nil {
		return fm, s, fmt.Errorf("parse yaml: %w", err)
	}
	return fm, body, nil
}
