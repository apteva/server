package main

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Serialize hydration and settings writes so a late catalog response cannot
// overwrite an operator pin saved while discovery was in progress.
var runtimeModelConfigLocks sync.Map

func (s *Server) lockRuntimeModelConfig(id int64) func() {
	key := struct {
		store *Store
		id    int64
	}{s.store, id}
	value, _ := runtimeModelConfigLocks.LoadOrStore(key, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

// RuntimeModelPolicy is an opt-in, integration-owned view of discovery for a
// particular use. Patterns admit verified model families, not every model on
// an endpoint that happens to implement the same generation method.
type RuntimeModelPolicy struct {
	Purpose           string              `json:"purpose"`
	RequiredMethods   []string            `json:"required_methods"`
	AllowedIDPatterns []string            `json:"allowed_id_patterns"`
	TierPreferences   map[string][]string `json:"tier_preferences"`
}

func matchesModelPattern(id string, patterns []string) bool {
	for _, pattern := range patterns {
		if re, err := regexp.Compile(pattern); err == nil && re.MatchString(id) {
			return true
		}
	}
	return false
}

func (p *RuntimeModelPolicy) allowsID(id string) bool {
	return p == nil || (p.Purpose == "agent" && matchesModelPattern(id, p.AllowedIDPatterns))
}

func (p *RuntimeModelPolicy) filter(models []ModelInfo) []ModelInfo {
	if p == nil {
		return models
	}
	result := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		if !p.allowsID(model.ID) {
			continue
		}
		eligible := true
		for _, method := range p.RequiredMethods {
			if !slices.Contains(model.Methods, method) {
				eligible = false
				break
			}
		}
		if eligible {
			model.Purposes = []string{p.Purpose}
			result = append(result, model)
		}
	}
	return result
}

func fetchRuntimeModels(runtime *AppRuntimeConfig, apiKey string, refresh ...bool) ([]ModelInfo, error) {
	models, err := FetchModels(runtime.ProviderKey, apiKey, refresh...)
	if err != nil {
		return nil, err
	}
	return runtime.ModelPolicy.filter(models), nil
}

var modelVersionParts = regexp.MustCompile(`[0-9]+|[^0-9]+`)

// Compare numeric portions naturally (3.10 > 3.9). This is only a tie breaker
// inside the integration's ordered preferences, never a capability heuristic.
func newerModelID(a, b string) bool {
	aa, bb := modelVersionParts.FindAllString(a, -1), modelVersionParts.FindAllString(b, -1)
	for i := 0; i < len(aa) && i < len(bb); i++ {
		if aa[i] == bb[i] {
			continue
		}
		x, xe := strconv.Atoi(aa[i])
		y, ye := strconv.Atoi(bb[i])
		if xe == nil && ye == nil {
			return x > y
		}
		return aa[i] > bb[i]
	}
	return len(aa) > len(bb)
}

func (p *RuntimeModelPolicy) selectTier(models []ModelInfo, tier string) string {
	candidates := append([]ModelInfo(nil), models...)
	patterns := p.TierPreferences[tier]
	rank := func(id string) int {
		for i, pattern := range patterns {
			if matchesModelPattern(id, []string{pattern}) {
				return i
			}
		}
		return len(patterns)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := rank(candidates[i].ID), rank(candidates[j].ID)
		if a != b {
			return a < b
		}
		return newerModelID(candidates[i].ID, candidates[j].ID)
	})
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0].ID
}

var runtimeModelTiers = []string{"large", "medium", "small"}

func stateObject(state map[string]any, key string) map[string]any {
	object, _ := state[key].(map[string]any)
	if object == nil {
		object = map[string]any{}
		state[key] = object
	}
	return object
}

// Reconcile only automatic or unclassified legacy selections. Legacy provenance
// cannot be recovered: preserve valid choices and archive rejected old values.
// Explicit invalid pins remain visible as errors and block this provider.
func reconcileRuntimeModels(policy *RuntimeModelPolicy, state map[string]any, models []ModelInfo, catalogErr error) {
	origins := stateObject(state, "model_selection_sources")
	errors := map[string]any{}
	available := map[string]bool{}
	for _, model := range models {
		available[model.ID] = true
	}
	for _, tier := range runtimeModelTiers {
		key := "model_" + tier
		id := strings.TrimSpace(stringValue(state[key]))
		source := stringValue(origins[key])
		valid := id != "" && policy.allowsID(id) && (catalogErr != nil || available[id])
		if valid {
			if source == "" {
				origins[key] = "legacy"
			}
			continue
		}
		if source != "explicit" && catalogErr == nil {
			if replacement := policy.selectTier(models, tier); replacement != "" {
				if id != "" {
					stateObject(state, "model_selection_previous")[key] = id
				}
				state[key] = replacement
				origins[key] = "automatic"
				continue
			}
		}
		errors[key] = fmt.Sprintf("%s tier: model %q is not available for agent reasoning; select a compatible model or clear this tier to use automatic selection", tier, id)
	}
	if len(errors) == 0 {
		delete(state, "model_selection_errors")
	} else {
		state["model_selection_errors"] = errors
	}
}

func (s *Server) hydratePolicyModels(conn runtimeConnection, app *AppTemplate, src runtimeTemplateSources, state map[string]any) map[string]bool {
	unlock := s.lockRuntimeModelConfig(conn.ID)
	defer unlock()
	var saved string
	if err := s.store.db.QueryRow("SELECT COALESCE(runtime_config,'{}') FROM connections WHERE id = ?", conn.ID).Scan(&saved); err == nil {
		var latest map[string]any
		if json.Unmarshal([]byte(saved), &latest) == nil && latest != nil {
			clear(state)
			for key, value := range latest {
				state[key] = value
			}
			src.config = state
		}
	}
	before, _ := json.Marshal(state)
	models, err := fetchRuntimeModels(app.Runtime, runtimeAPIKeyFor(app.Runtime, src))
	if err != nil {
		log.Printf("[RUNTIME-MODELS] connection=%d catalog unavailable: %v", conn.ID, err)
	}
	reconcileRuntimeModels(app.Runtime.ModelPolicy, state, models, err)
	after, _ := json.Marshal(state)
	if string(before) != string(after) {
		s.persistRuntimeModels(conn, state)
	}
	if err != nil {
		return nil
	}
	available := map[string]bool{}
	for _, model := range models {
		available[model.ID] = true
	}
	return available
}

func (s *Server) modelPolicyForProvider(providerKey string) *RuntimeModelPolicy {
	if s.catalog == nil {
		return nil
	}
	for _, summary := range s.catalog.List() {
		app := s.catalog.Get(summary.Slug)
		if app != nil && app.Runtime != nil && app.Runtime.ProviderKey == providerKey && app.Runtime.ModelPolicy != nil {
			return app.Runtime.ModelPolicy
		}
	}
	return nil
}

func validateProviderModel(pool []ProviderInfo, provider, model string) error {
	if strings.TrimSpace(model) == "" {
		return nil
	}
	for _, info := range pool {
		if providerKeyFromName(info.Type) == providerKeyFromName(provider) && !info.allowsModel(model) {
			return fmt.Errorf("model %q is not supported for agent reasoning by %s", model, provider)
		}
	}
	return nil
}

// Final boundary: never let an invalid/empty tier fall through to a core
// factory default. This also covers legacy providers without a connection.
func eligibleProviderPool(pool []ProviderInfo) []ProviderInfo {
	result := make([]ProviderInfo, 0, len(pool))
	for _, info := range pool {
		if info.ModelPolicy != nil {
			if info.ModelSelectionError || info.ModelLarge == "" || info.ModelMedium == "" || info.ModelSmall == "" ||
				!info.allowsModel(info.ModelLarge) || !info.allowsModel(info.ModelMedium) || !info.allowsModel(info.ModelSmall) {
				continue
			}
			caps := map[string]ProviderModelCapabilities{}
			for id, cap := range info.ModelCapabilities {
				if info.allowsModel(id) {
					caps[id] = cap
				}
			}
			info.ModelCapabilities = caps
		}
		result = append(result, info)
	}
	return result
}

// Validate raw per-agent provider edits through the same integration policy.
func validateProviderModelMaps(pool []ProviderInfo, providers []map[string]any) error {
	for _, provider := range providers {
		name, _ := provider["name"].(string)
		for _, info := range pool {
			if info.Type != providerKeyFromName(name) || info.ModelPolicy == nil {
				continue
			}
			raw, err := json.Marshal(provider["models"])
			if err != nil {
				return fmt.Errorf("invalid models for %s", name)
			}
			var models map[string]string
			if json.Unmarshal(raw, &models) != nil {
				return fmt.Errorf("invalid models for %s", name)
			}
			for _, tier := range runtimeModelTiers {
				if models[tier] == "" || !info.allowsModel(models[tier]) {
					return fmt.Errorf("model %q is not supported for agent reasoning by %s", models[tier], name)
				}
			}
			// Caller-supplied metadata must not reintroduce a catalog of media models.
			if rawCaps, ok := provider["model_capabilities"]; ok {
				encoded, _ := json.Marshal(rawCaps)
				var caps map[string]ProviderModelCapabilities
				if json.Unmarshal(encoded, &caps) != nil {
					return fmt.Errorf("invalid model capabilities for %s", name)
				}
				for id := range caps {
					if !info.allowsModel(id) {
						delete(caps, id)
					}
				}
				provider["model_capabilities"] = caps
			}
		}
	}
	return nil
}

func (s *Server) validateRuntimeModelPatch(conn *Connection, encrypted string, app *AppTemplate, current, patch map[string]any) error {
	for key := range patch {
		if strings.HasPrefix(key, "model_selection_") {
			return fmt.Errorf("%s is managed by the server", key)
		}
	}
	var models []ModelInfo
	needsCatalog := false
	for _, tier := range runtimeModelTiers {
		if raw, ok := patch["model_"+tier]; ok && raw != nil {
			id, ok := raw.(string)
			if !ok || !app.Runtime.ModelPolicy.allowsID(strings.TrimSpace(id)) {
				return fmt.Errorf("model %q is not supported for agent reasoning", raw)
			}
			needsCatalog = true
		}
	}
	if needsCatalog {
		src, err := buildRuntimeSources(runtimeConnection{ID: conn.ID, AppSlug: conn.AppSlug, EncryptedCreds: encrypted}, s.secret)
		if err != nil {
			return fmt.Errorf("could not read connection credentials")
		}
		src.config = current
		models, err = fetchRuntimeModels(app.Runtime, runtimeAPIKeyFor(app.Runtime, src))
		if err != nil {
			return fmt.Errorf("could not validate model availability; retry when the provider catalog is available")
		}
	}
	for _, tier := range runtimeModelTiers {
		key := "model_" + tier
		value, changed := patch[key]
		if !changed {
			continue
		}
		if value != nil {
			id := strings.TrimSpace(value.(string))
			if !slices.ContainsFunc(models, func(m ModelInfo) bool { return m.ID == id }) {
				return fmt.Errorf("model %q is not available for agent reasoning on this connection", id)
			}
			patch[key] = id
		}
	}
	// Change provenance only after every requested tier has passed validation.
	for _, tier := range runtimeModelTiers {
		key := "model_" + tier
		value, changed := patch[key]
		if !changed {
			continue
		}
		origins := stateObject(current, "model_selection_sources")
		if value == nil {
			delete(origins, key)
		} else {
			origins[key] = "explicit"
		}
		if errors, ok := current["model_selection_errors"].(map[string]any); ok {
			delete(errors, key)
			if len(errors) == 0 {
				delete(current, "model_selection_errors")
			}
		}
	}
	return nil
}

func (info ProviderInfo) allowsModel(model string) bool {
	return info.ModelPolicy.allowsID(model) && (info.ModelPolicy == nil || info.AvailableModels == nil || info.AvailableModels[model])
}
