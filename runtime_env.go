package main

// runtime_env.go — the connections half of the providers/connections
// fusion.
//
// Historically two tables held credentials: `providers` (injected into
// apteva-core's environment) and `connections` (called over HTTP by the
// server). They overlapped — OpenAI Codex, Google, and Browserbase each
// existed as both — and the providers table carried env var names as
// literal keys inside its encrypted blob, which is why it could only
// ever describe credentials it had been hand-taught.
//
// The catalog's `runtime` block replaces all of that. A connection whose
// AppTemplate declares `runtime` is a runtime backend; the block says
// which env vars to build and where each value comes from. Everything
// here is about turning such a connection into the two shapes core
// already consumes: an env map, and a ProviderInfo for config.json.
//
// Nothing in apteva-core changes. Both outputs are byte-identical to
// what the providers table produced.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// RuntimeCatalogEntry is one runtime-capable catalog app as reported by
// GET /api/integrations/catalog?runtime_role=llm. It carries exactly what
// a credential form needs — auth types and credential fields — so the
// dashboard renders the same inputs it does for any other integration
// instead of labelling boxes with raw env var names the way the old
// provider-types form did.
type RuntimeCatalogEntry struct {
	Slug             string            `json:"slug"`
	Name             string            `json:"name"`
	Description      string            `json:"description"`
	Logo             *string           `json:"logo"`
	Role             string            `json:"role"`
	ProviderKey      string            `json:"provider_key"`
	AuthTypes        []string          `json:"auth_types"`
	CredentialFields []CredentialField `json:"credential_fields,omitempty"`
	Capabilities     []string          `json:"capabilities,omitempty"`
	// EnvVars — names only, never values. Useful for an operator
	// debugging what a connection is expected to export.
	EnvVars []string `json:"env_vars,omitempty"`
}

// runtimeConnection is the subset of a connections row the runtime
// resolvers need. Deliberately not the full Connection struct: this
// query runs on every agent boot and carries an encrypted blob.
type runtimeConnection struct {
	ID               int64
	AppSlug          string
	Name             string
	AuthType         string
	ProjectID        string
	Status           string
	IsPrimary        bool
	LegacyProviderID int64
	RuntimeConfig    string
	EncryptedCreds   string
}

// ListRuntimeConnections returns a user's connections ordered the way
// the runtime pool resolves them:
//
//  1. project-scoped rows before global ones  (unchanged from the
//     providers table — a global credential must never silently
//     displace the selected project's own)
//  2. is_primary DESC                          (new: the operator's
//     pick among several keys for the same app)
//  3. id ASC                                   (unchanged fallback)
//
// Only 'active' rows are returned. A connection stuck in 'pending' (an
// OAuth handshake the user abandoned) has no usable credential, and
// injecting a half-finished blob into a core would surface as a confusing
// 401 at first inference rather than as "not connected".
func (s *Store) ListRuntimeConnections(userID int64, projectID ...string) ([]runtimeConnection, error) {
	const cols = `id, app_slug, name, COALESCE(auth_type,''), COALESCE(project_id,''), COALESCE(status,'active'),
	              COALESCE(is_primary,0), COALESCE(legacy_provider_id,0),
	              COALESCE(runtime_config,'{}'), encrypted_credentials`

	var (
		rows interface {
			Next() bool
			Scan(...any) error
			Close() error
			Err() error
		}
		err error
	)
	if len(projectID) > 0 && projectID[0] != "" {
		rows, err = s.db.Query(
			`SELECT `+cols+` FROM connections
			 WHERE user_id = ? AND (project_id = ? OR project_id = '') AND COALESCE(status,'active') = 'active'
			 ORDER BY CASE WHEN project_id = ? THEN 0 ELSE 1 END, is_primary DESC, id ASC`,
			userID, projectID[0], projectID[0],
		)
	} else {
		rows, err = s.db.Query(
			`SELECT `+cols+` FROM connections
			 WHERE user_id = ? AND COALESCE(status,'active') = 'active'
			 ORDER BY is_primary DESC, id ASC`,
			userID,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []runtimeConnection
	for rows.Next() {
		var c runtimeConnection
		var isPrimary int
		if err := rows.Scan(&c.ID, &c.AppSlug, &c.Name, &c.AuthType, &c.ProjectID, &c.Status,
			&isPrimary, &c.LegacyProviderID, &c.RuntimeConfig, &c.EncryptedCreds); err != nil {
			continue
		}
		c.IsPrimary = isPrimary != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

// runtimeTemplateSources holds the three namespaces a runtime.env
// template can read from.
type runtimeTemplateSources struct {
	credentials map[string]any
	config      map[string]any
	connection  map[string]any
}

// buildRuntimeSources decrypts a connection's credentials and decodes its
// runtime_config into the namespaces templates resolve against.
//
// `connection.provider_ref` is synthesized rather than stored: it is the
// legacy providers.id when this row was migrated from one, else the
// connection id. Cores spawned before the fusion build their token
// refresh URL from whatever we put in OPENAI_CODEX_PROVIDER_ID, so a
// migrated Codex row has to keep handing out its old id — see
// resolveRuntimeTokenConnection.
func buildRuntimeSources(conn runtimeConnection, secret []byte) (runtimeTemplateSources, error) {
	src := runtimeTemplateSources{
		credentials: map[string]any{},
		config:      map[string]any{},
		connection: map[string]any{
			"id":                 conn.ID,
			"legacy_provider_id": conn.LegacyProviderID,
			"app_slug":           conn.AppSlug,
			"project_id":         conn.ProjectID,
		},
	}

	providerRef := conn.ID
	if conn.LegacyProviderID != 0 {
		providerRef = conn.LegacyProviderID
	}
	src.connection["provider_ref"] = providerRef

	if strings.TrimSpace(conn.EncryptedCreds) != "" {
		plaintext, err := Decrypt(secret, conn.EncryptedCreds)
		if err != nil {
			return src, fmt.Errorf("decrypt connection %d: %w", conn.ID, err)
		}
		// Connections store map[string]string; decoding into map[string]any
		// accepts that and also tolerates nested objects, so a future OAuth
		// shape with {"credentials":{"access_token":…}} resolves through
		// dotted paths without a format migration.
		if err := json.Unmarshal([]byte(plaintext), &src.credentials); err != nil {
			return src, fmt.Errorf("decode connection %d credentials: %w", conn.ID, err)
		}
	}

	if cfg := strings.TrimSpace(conn.RuntimeConfig); cfg != "" && cfg != "{}" {
		// A malformed runtime_config is not fatal: it holds preferences,
		// not credentials, so the connection still works with catalog
		// defaults. Losing a pinned model beats failing to boot the agent.
		_ = json.Unmarshal([]byte(cfg), &src.config)
	}

	return src, nil
}

// resolveRuntimeTemplate substitutes {{namespace.path}} references.
// Returns "" when any reference in the template fails to resolve, which
// the caller treats as "omit this env var" rather than "inject empty".
//
// An env var present but empty reads to core as "configured, broken";
// absent reads as "not configured", which is what an unresolvable
// template actually means.
func resolveRuntimeTemplate(tmpl string, src runtimeTemplateSources) string {
	out := tmpl
	for {
		start := strings.Index(out, "{{")
		if start < 0 {
			break
		}
		end := strings.Index(out[start:], "}}")
		if end < 0 {
			break
		}
		end += start
		ref := strings.TrimSpace(out[start+2 : end])
		value, ok := lookupRuntimeRef(ref, src)
		if !ok || value == "" {
			return ""
		}
		out = out[:start] + value + out[end+2:]
	}
	return out
}

// lookupRuntimeRef resolves one "namespace.path.to.value" reference.
func lookupRuntimeRef(ref string, src runtimeTemplateSources) (string, bool) {
	parts := strings.Split(ref, ".")
	if len(parts) < 2 {
		return "", false
	}
	var cursor any
	switch parts[0] {
	case "credentials":
		cursor = src.credentials
	case "config":
		cursor = src.config
	case "connection":
		cursor = src.connection
	default:
		return "", false
	}
	for _, key := range parts[1:] {
		m, ok := cursor.(map[string]any)
		if !ok {
			return "", false
		}
		cursor, ok = m[key]
		if !ok {
			return "", false
		}
	}
	return stringifyRuntimeValue(cursor), true
}

// stringifyRuntimeValue renders a resolved leaf as an env var value.
// Numbers are formatted without a decimal tail so OLLAMA_EMBED_DIM comes
// out as "768", not "768.000000" — encoding/json decodes every JSON
// number as float64.
func stringifyRuntimeValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", t), "0"), ".")
	case int64:
		return fmt.Sprintf("%d", t)
	case int:
		return fmt.Sprintf("%d", t)
	case json.Number:
		return t.String()
	default:
		return ""
	}
}

// renderRuntimeEnv builds the env vars one runtime connection contributes.
// Keys whose template does not fully resolve are omitted.
func renderRuntimeEnv(runtime *AppRuntimeConfig, src runtimeTemplateSources) map[string]string {
	env := map[string]string{}
	if runtime == nil {
		return env
	}
	for name, tmpl := range runtime.Env {
		if !isEnvVar(name) {
			// The catalog is data; a typo'd key would otherwise become a
			// silently-ignored env var that looks configured.
			continue
		}
		if value := resolveRuntimeTemplate(tmpl, src); value != "" {
			env[name] = value
		}
	}
	return env
}

// runtimeAppFor returns the catalog entry for a connection when that
// entry declares a runtime block, else nil. Connections whose app has no
// runtime block are plain integrations: their credentials are called over
// HTTP by the server and must never reach a core's environment.
func (s *Server) runtimeAppFor(conn runtimeConnection) *AppTemplate {
	if s.catalog == nil {
		return nil
	}
	app := s.catalog.Get(conn.AppSlug)
	if app == nil || app.Runtime == nil {
		return nil
	}
	if strings.TrimSpace(app.Runtime.Role) == "" || strings.TrimSpace(app.Runtime.ProviderKey) == "" {
		return nil
	}
	return app
}

// runtimeEnvFromConnections builds the env map contributed by every
// runtime-backed connection visible to this user/project.
//
// `existing` is what the providers table already produced. A connection
// may only overwrite one of those vars when it was migrated FROM a
// provider row (legacy_provider_id != 0) — i.e. when it *is* that row
// under a new name. An independently-created connection that happens to
// supply the same var leaves the provider's value alone.
//
// Without that rule, adding a Gemini connection would silently swap the
// GOOGLE_API_KEY every agent in the project boots with, which is a
// credential change nobody asked for and nothing surfaces.
//
// Within the connections themselves, later rows never overwrite earlier
// ones: ListRuntimeConnections already ordered by (project-before-global,
// is_primary, id), so the first row to claim a var is the one the pool
// picks too. Otherwise a global credential could stomp the project's own
// key in the env while config.json still named the project's provider.
func (s *Server) runtimeEnvFromConnections(userID int64, existing map[string]string, projectID ...string) (map[string]string, error) {
	conns, err := s.store.ListRuntimeConnections(userID, projectID...)
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, conn := range conns {
		app := s.runtimeAppFor(conn)
		if app == nil {
			continue
		}
		src, err := buildRuntimeSources(conn, s.secret)
		if err != nil {
			// One unreadable connection must not stop an agent booting on
			// the others. The provider path behaved the same way.
			continue
		}
		migrated := conn.LegacyProviderID != 0
		for name, value := range renderRuntimeEnv(app.Runtime, src) {
			if _, taken := env[name]; taken {
				continue
			}
			if _, fromProvider := existing[name]; fromProvider && !migrated {
				continue
			}
			env[name] = value
		}
	}
	return env, nil
}

// providerSuppliedLLMKeys reports which LLM provider keys the legacy
// providers table still serves for this user/project. Connections that
// were not migrated from a provider row must not claim these — see
// runtimeEnvFromConnections for why.
func (s *Server) providerSuppliedLLMKeys(userID int64, projectID ...string) map[string]bool {
	keys := map[string]bool{}
	providers, err := s.store.ListProviders(userID, projectID...)
	if err != nil {
		return keys
	}
	for _, p := range providers {
		providerKey := strings.ToLower(p.Type)
		if providerKey == "llm" {
			providerKey = providerKeyFromName(p.Name)
		}
		if isLLMKey(providerKey) {
			keys[providerKey] = true
		}
	}
	return keys
}

// UpdateConnectionRuntimeConfig replaces a connection's runtime_config
// blob. Used when a Codex connection's model catalog is hydrated at boot
// so the fetch does not repeat on every agent start.
func (s *Store) UpdateConnectionRuntimeConfig(connID int64, runtimeConfig string) error {
	_, err := s.db.Exec("UPDATE connections SET runtime_config = ? WHERE id = ?", runtimeConfig, connID)
	return err
}

// runtimePoolFromConnections builds the LLM pool entries contributed by
// connections, in the same shape and order GetProviderPool produced from
// the providers table.
//
// Returns the ordinary entries, the OpenAI Codex entries (which sort
// last, matching the providers path — Codex is a subscription fallback
// rather than a default), and the set of provider keys claimed, so the
// providers half of the dual-read can skip anything already supplied.
// `shadowed` lists provider keys the legacy providers table still
// serves. A connection may only take one of those over when it carries
// legacy_provider_id — otherwise the provider row keeps the key and the
// operator's agents keep the credential they had.
type providerPoolRank struct {
	anchored bool
	scope    int
	id       int64
}

func providerRankForConnection(conn runtimeConnection, projectID ...string) providerPoolRank {
	rank := providerPoolRank{id: conn.ID}
	if conn.LegacyProviderID != 0 {
		rank.anchored = true
		rank.id = conn.LegacyProviderID
	}
	if len(projectID) > 0 && projectID[0] != "" && conn.ProjectID != projectID[0] {
		rank.scope = 1
	}
	return rank
}

func (s *Server) runtimePoolFromConnections(userID int64, shadowed map[string]bool, projectID ...string) ([]ProviderInfo, []ProviderInfo, map[string]bool, map[string]providerPoolRank) {
	claimed := map[string]bool{}
	ranks := map[string]providerPoolRank{}
	conns, err := s.store.ListRuntimeConnections(userID, projectID...)
	if err != nil || len(conns) == 0 {
		return nil, nil, claimed, ranks
	}

	// Pool POSITION is load-bearing: providerIsDefault marks index 0 as
	// the default when an agent has no pin, GetProviderInfo returns
	// pool[0], and the dashboard falls back to its first row. Iteration
	// order alone would rank apps by connection id — and a migrated
	// provider gets a fresh high id, so the operator's long-standing
	// default would silently lose position 0 to whichever integration
	// happened to be connected first. Order the groups instead.
	order := runtimeGroupOrder(s, conns)

	var pool, codexPool []ProviderInfo
	for _, conn := range conns {
		app := s.runtimeAppFor(conn)
		if app == nil || !strings.EqualFold(app.Runtime.Role, "llm") {
			continue
		}
		providerKey := app.Runtime.ProviderKey
		// Platform-managed credentials are deliberately inert until an admin
		// selects one in the access policy. They still appear in the runtime
		// connection list so Settings can select them, but they must never become
		// a core provider that receives the platform credential directly.
		if providerKey == "managed" {
			continue
		}
		// The allow-list stays the gate even though the key now comes from
		// the catalog: config.json provider names are a contract with
		// apteva-core, and catalog JSON is data that ships separately from
		// the binary that has to understand it.
		if !isLLMKey(providerKey) || claimed[providerKey] {
			continue
		}
		if shadowed[providerKey] && conn.LegacyProviderID == 0 {
			continue
		}
		src, err := buildRuntimeSources(conn, s.secret)
		if err != nil {
			// During dual-read, a corrupted migrated copy must not hide the
			// still-readable provider row. Once that row is deliberately gone,
			// retain the old present-but-unconfigured behavior.
			if shadowed[providerKey] && conn.LegacyProviderID != 0 {
				continue
			}
			claimed[providerKey] = true
			ranks[providerKey] = providerRankForConnection(conn, projectID...)
			// Credential unreadable — still claim the key so the pool
			// reports the provider as present-but-unconfigured, exactly
			// as the providers path did on a decrypt failure.
			pool = append(pool, ProviderInfo{Type: providerKey})
			continue
		}
		claimed[providerKey] = true
		ranks[providerKey] = providerRankForConnection(conn, projectID...)

		state := src.config
		if state == nil {
			state = map[string]any{}
		}
		s.hydrateRuntimeModels(conn, app, src, state)

		info := ProviderInfo{
			Type:              providerKey,
			ModelLarge:        normalizeStaleModel(providerKey, stringValue(state["model_large"])),
			ModelMedium:       normalizeStaleModel(providerKey, stringValue(state["model_medium"])),
			ModelSmall:        normalizeStaleModel(providerKey, stringValue(state["model_small"])),
			BuiltinTools:      runtimeBuiltinTools(state),
			ModelCapabilities: runtimeModelCapabilities(state),
		}
		if providerKey == "openai-codex" {
			codexPool = append(codexPool, info)
			continue
		}
		pool = append(pool, info)
	}
	sort.SliceStable(pool, func(i, j int) bool {
		return order[pool[i].Type] < order[pool[j].Type]
	})
	return pool, codexPool, claimed, ranks
}

// runtimeGroupOrder ranks provider-key groups for pool position:
//
//  1. groups migrated from a provider row, project scope before global and
//     then in legacy provider id order —
//     the providers table ranked by its own id ASC, so this keeps every
//     migrated provider in exactly the pool slot it held before the
//     fusion. The default for unpinned agents does not move.
//  2. never-migrated groups, in first-connection order — the natural
//     analog of "the order the operator added providers".
//
// (Codex is excluded from ordering concerns by its caller — it is
// appended after the pool regardless, as the subscription fallback.)
//
// Position is per provider key, not per row: is_primary still chooses
// WHICH credential serves a group; this decides only where the group
// stands relative to other groups.
func runtimeGroupOrder(s *Server, conns []runtimeConnection) map[string]int64 {
	type groupInfo struct {
		legacyID int64
		firstID  int64
		scope    int64
	}
	projectScopedView := false
	for _, conn := range conns {
		if conn.ProjectID != "" {
			projectScopedView = true
			break
		}
	}
	groups := map[string]*groupInfo{}
	for _, conn := range conns {
		app := s.runtimeAppFor(conn)
		if app == nil {
			continue
		}
		key := app.Runtime.ProviderKey
		g, ok := groups[key]
		if !ok {
			g = &groupInfo{firstID: conn.ID, legacyID: conn.LegacyProviderID}
			if projectScopedView && conn.ProjectID == "" {
				g.scope = 1
			}
			groups[key] = g
		}
	}

	// Rank: effective (first) connection per provider key, project before
	// global within each tier; legacy ids first, then never-migrated groups
	// offset far above any plausible provider id.
	const (
		scopeOffset       = int64(1) << 31
		nonMigratedOffset = int64(1) << 33
	)
	order := map[string]int64{}
	for key, g := range groups {
		if g.legacyID != 0 {
			order[key] = g.scope*scopeOffset + g.legacyID
		} else {
			order[key] = nonMigratedOffset + g.scope*scopeOffset + g.firstID
		}
	}
	return order
}

// hydrateRuntimeModels fills a connection's model tiers from the
// provider's LIVE model list when runtime_config doesn't pin them, then
// persists the result so the fetch happens once rather than on every
// agent boot.
//
// Deliberately not a static default in the catalog: model ids churn
// faster than the catalog ships, and a stale hardcoded id fails at first
// inference with an unhelpful upstream error. Asking the provider what
// it currently serves is the only answer that stays correct.
//
// Every failure path is non-fatal. Saved models — or apteva-core's own
// factory defaults — are a better outcome than refusing to boot the
// agent because a model list endpoint was slow.
func (s *Server) hydrateRuntimeModels(conn runtimeConnection, app *AppTemplate, src runtimeTemplateSources, state map[string]any) {
	if state == nil || app == nil || app.Runtime == nil {
		return
	}
	if !runtimeModelsMissing(state) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var models []ModelInfo
	if app.Runtime.ProviderKey == openAICodexAuthProvider {
		// Codex has its own catalog endpoint: subscription-gated, and it
		// reports per-model capabilities the generic /models cannot.
		accessToken, _ := lookupRuntimeRef("credentials.access_token", src)
		accountID, _ := lookupRuntimeRef("credentials.account_id", src)
		if strings.TrimSpace(accessToken) == "" {
			return
		}
		fetched, err := fetchCodexModelCatalog(ctx, accessToken, accountID, false)
		if err != nil {
			log.Printf("[RUNTIME-MODELS] connection=%d catalog unavailable; retaining saved models: %v", conn.ID, err)
			return
		}
		applyCodexCatalogToState(state, fetched)
		s.persistRuntimeModels(conn, state)
		return
	}

	apiKey := runtimeAPIKeyFor(app.Runtime, src)
	if apiKey == "" {
		return
	}
	models, err := FetchModels(app.Runtime.ProviderKey, apiKey, runtimeBaseURLFor(src))
	if err != nil || len(models) == 0 {
		if err != nil {
			log.Printf("[RUNTIME-MODELS] connection=%d model list unavailable: %v", conn.ID, err)
		}
		return
	}
	// One list, applied to every unset tier. Tier-specific selection is
	// the operator's call in the Providers tab; this only stops the pool
	// from handing core an empty model id.
	top := models[0].ID
	for _, key := range []string{"model_large", "model_medium", "model_small"} {
		if strings.TrimSpace(stringValue(state[key])) == "" {
			state[key] = top
		}
	}
	s.persistRuntimeModels(conn, state)
}

// runtimeModelsMissing reports whether any tier is still unset.
func runtimeModelsMissing(state map[string]any) bool {
	for _, key := range []string{"model_large", "model_medium", "model_small"} {
		if strings.TrimSpace(stringValue(state[key])) == "" {
			return true
		}
	}
	return false
}

// runtimeBaseURLFor returns the operator-configured API root override
// for a runtime connection, or "" when the connection uses the
// provider's default endpoint. It lives in runtime_config (non-secret
// knobs) under base_url — the same key the provider migration writes
// when it lifts OPENAI_BASE_URL out of a legacy provider blob, and the
// same key the openai-api catalog entry renders back into the agent's
// environment via {{config.base_url}}.
func runtimeBaseURLFor(src runtimeTemplateSources) string {
	base, _ := src.config["base_url"].(string)
	return strings.TrimSpace(base)
}

// runtimeAPIKeyFor renders the runtime env and returns the first *_KEY
// value. The catalog declares which var carries the key, so this needs
// no per-provider knowledge — unlike the providers path, which scanned
// the blob for a name ending in _API_KEY.
func runtimeAPIKeyFor(runtime *AppRuntimeConfig, src runtimeTemplateSources) string {
	env := renderRuntimeEnv(runtime, src)
	for _, name := range sortedEnvNames(runtime.Env) {
		if strings.HasSuffix(name, "_KEY") && env[name] != "" {
			return env[name]
		}
	}
	return ""
}

func (s *Server) persistRuntimeModels(conn runtimeConnection, state map[string]any) {
	encoded, err := json.Marshal(state)
	if err != nil {
		return
	}
	if err := s.store.UpdateConnectionRuntimeConfig(conn.ID, string(encoded)); err != nil {
		log.Printf("[RUNTIME-MODELS] connection=%d could not persist models: %v", conn.ID, err)
	}
}

func runtimeBuiltinTools(state map[string]any) []string {
	var tools []string
	switch raw := state["builtin_tools"].(type) {
	case string:
		if raw != "" {
			_ = json.Unmarshal([]byte(raw), &tools)
		}
	case []any:
		for _, item := range raw {
			if value, ok := item.(string); ok {
				tools = append(tools, value)
			}
		}
	}
	return tools
}

func runtimeModelCapabilities(state map[string]any) map[string]ProviderModelCapabilities {
	caps := map[string]ProviderModelCapabilities{}
	raw, ok := state["model_capabilities"]
	if !ok {
		return caps
	}
	if encoded, err := json.Marshal(raw); err == nil {
		_ = json.Unmarshal(encoded, &caps)
	}
	return caps
}

// runtimeLLMCatalog returns catalog entries declaring a runtime block for
// the given role, sorted by provider key. Backs
// GET /api/integrations/catalog?runtime_role=llm, which onboarding uses
// instead of pulling all 600+ entries and filtering client-side.
func (s *Server) runtimeCatalogForRole(role string) []RuntimeCatalogEntry {
	if s.catalog == nil {
		return nil
	}
	role = strings.TrimSpace(strings.ToLower(role))
	var out []RuntimeCatalogEntry
	for _, summary := range s.catalog.List() {
		app := s.catalog.Get(summary.Slug)
		if app == nil || app.Runtime == nil {
			continue
		}
		if role != "" && !strings.EqualFold(app.Runtime.Role, role) {
			continue
		}
		out = append(out, RuntimeCatalogEntry{
			Slug:             app.Slug,
			Name:             app.Name,
			Description:      app.Description,
			Logo:             app.Logo,
			Role:             app.Runtime.Role,
			ProviderKey:      app.Runtime.ProviderKey,
			AuthTypes:        app.Auth.Types,
			CredentialFields: app.Auth.CredentialFields,
			Capabilities:     app.Runtime.Capabilities,
			EnvVars:          sortedEnvNames(app.Runtime.Env),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProviderKey < out[j].ProviderKey })
	return out
}

func sortedEnvNames(env map[string]string) []string {
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
