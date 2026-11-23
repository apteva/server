package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// agents_models and the provider/model fields of agents_update. Both read the
// same server routes the dashboard's Models settings use, so the names and
// model IDs a model sees are exactly the ones the config route accepts.

const gatewayModelsPerProvider = 100

// Keys that select an agent's provider or model. agents_update takes them as
// top-level fields; inside config they would bypass the provider checks.
var gatewayConfigModelKeys = []string{"default_provider", "providers", "model_override", "provider", "model"}

type gatewayRuntimeConnection struct {
	ID          int64          `json:"id"`
	AppName     string         `json:"app_name"`
	ProviderKey string         `json:"provider_key"`
	Role        string         `json:"role"`
	RuntimeConf map[string]any `json:"runtime_config"`
}

type gatewayModelChoice struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type gatewayProviderModels struct {
	Provider     string
	Name         string
	NewAgents    bool
	DefaultModel string
	ConnectionID int64
	Models       []gatewayModelChoice
	ModelsError  string
}

// gatewayProjectProviders lists the LLM providers agents in projectID can
// use, in the server's pool order. When only is set, just that provider is
// returned; an unknown name is an error listing the available ones. Model
// lists are fetched only when withModels is true.
func gatewayProjectProviders(serverAPI gatewayAPIClient, projectID, only string, withModels bool) ([]gatewayProviderModels, error) {
	query := ""
	if projectID != "" {
		query = "?project_id=" + url.QueryEscape(projectID)
	}
	var settings struct {
		EffectiveProvider  string   `json:"effective_provider"`
		AvailableProviders []string `json:"available_providers"`
	}
	if err := serverAPI.do(http.MethodGet, "/settings/new-agent-provider"+query, nil, &settings); err != nil {
		return nil, err
	}
	var conns []gatewayRuntimeConnection
	if err := serverAPI.do(http.MethodGet, "/connections/runtime"+query, nil, &conns); err != nil {
		return nil, err
	}
	only = providerKeyFromName(only)
	newAgentDefault := providerKeyFromName(settings.EffectiveProvider)
	available := make([]string, 0, len(settings.AvailableProviders))
	providers := []gatewayProviderModels{}
	for _, raw := range settings.AvailableProviders {
		key := providerKeyFromName(raw)
		available = append(available, key)
		if only != "" && key != only {
			continue
		}
		entry := gatewayProviderModels{Provider: key, NewAgents: key == newAgentDefault}
		// The runtime list is in pool order and the pool uses the first
		// connection per provider, so the first match is the one agents run on.
		for _, conn := range conns {
			if providerKeyFromName(conn.ProviderKey) == key && strings.EqualFold(conn.Role, "llm") {
				entry.Name = conn.AppName
				entry.ConnectionID = conn.ID
				entry.DefaultModel, _ = conn.RuntimeConf["model_large"].(string)
				break
			}
		}
		if withModels {
			entry.Models, entry.ModelsError = gatewayProviderModelList(serverAPI, entry.ConnectionID)
		}
		providers = append(providers, entry)
	}
	if only != "" && len(providers) == 0 {
		if len(available) == 0 {
			return nil, fmt.Errorf("provider %q is not available: no LLM provider is connected for this project", only)
		}
		return nil, fmt.Errorf("provider %q is not available for this project; available providers: %s", only, strings.Join(available, ", "))
	}
	return providers, nil
}

func gatewayProviderModelList(serverAPI gatewayAPIClient, connectionID int64) ([]gatewayModelChoice, string) {
	if connectionID <= 0 {
		return nil, "no connection found for this provider"
	}
	var listed []gatewayModelChoice
	if err := serverAPI.do(http.MethodGet, fmt.Sprintf("/connections/%d/models", connectionID), nil, &listed); err != nil {
		reason := err.Error()
		var apiErr *gatewayAPIError
		if errors.As(err, &apiErr) {
			reason = fmt.Sprintf("status %d: %s", apiErr.StatusCode, apiErr.Body)
		}
		if len(reason) > 300 {
			reason = reason[:300] + "..."
		}
		return nil, "model list unavailable (" + reason + ")"
	}
	models := make([]gatewayModelChoice, 0, len(listed))
	for _, model := range listed {
		model.ID = strings.TrimSpace(model.ID)
		if model.ID != "" {
			models = append(models, model)
		}
	}
	return models, ""
}

func gatewayAgentModels(args map[string]any, projectID string, serverAPI gatewayAPIClient) (any, error) {
	pid := gatewayProjectIDArg(args, projectID)
	only, _ := args["provider"].(string)
	query, _ := args["query"].(string)
	query = strings.ToLower(strings.TrimSpace(query))
	providers, err := gatewayProjectProviders(serverAPI, pid, only, true)
	if err != nil {
		return nil, err
	}
	rows := make([]map[string]any, 0, len(providers))
	for _, provider := range providers {
		row := map[string]any{"provider": provider.Provider}
		if provider.Name != "" {
			row["name"] = provider.Name
		}
		if provider.NewAgents {
			row["default_for_new_agents"] = true
		}
		if provider.DefaultModel != "" {
			row["default_model"] = provider.DefaultModel
		}
		if provider.ModelsError != "" {
			row["models_error"] = provider.ModelsError
		}
		models := []gatewayModelChoice{}
		for _, model := range provider.Models {
			if query == "" || strings.Contains(strings.ToLower(model.ID), query) || strings.Contains(strings.ToLower(model.Name), query) {
				models = append(models, model)
			}
		}
		row["models_total"] = len(models)
		if len(models) > gatewayModelsPerProvider {
			models = models[:gatewayModelsPerProvider]
			row["models_truncated"] = true
		}
		if provider.ModelsError == "" {
			row["models"] = models
		}
		rows = append(rows, row)
	}
	out := map[string]any{
		"providers": rows,
		"usage":     "Set an agent with agents_update provider=<provider> and model=<model id>. Omit model to keep the agent's model, or pass an empty model to use the provider's default_model.",
	}
	if pid != "" {
		out["project_id"] = pid
	}
	return out, nil
}

// gatewayModelSelection is a checked provider/model change for agents_update.
type gatewayModelSelection struct {
	Provider     string
	Model        string
	ModelSet     bool
	ModelChecked bool
	Warning      string
	AgentStopped bool
}

// gatewayAgentModelSelection reads agents_update's provider and model fields
// and checks them against the agent's project before anything is written.
// It returns nil when neither field was sent.
func gatewayAgentModelSelection(id int64, args map[string]any, projectID string, serverAPI gatewayAPIClient, store *Store) (*gatewayModelSelection, error) {
	rawProvider, hasProvider := args["provider"]
	rawModel, hasModel := args["model"]
	if !hasProvider && !hasModel {
		return nil, nil
	}
	selection := &gatewayModelSelection{}
	if hasProvider {
		provider, ok := rawProvider.(string)
		if !ok || strings.TrimSpace(provider) == "" {
			return nil, fmt.Errorf("provider must be a provider name from agents_models")
		}
		selection.Provider = providerKeyFromName(provider)
	}
	if hasModel {
		model, ok := rawModel.(string)
		if !ok {
			return nil, fmt.Errorf("model must be a model id string from agents_models, or empty to use the provider's default model")
		}
		selection.Model = strings.TrimSpace(model)
		selection.ModelSet = true
	}

	pid := projectID
	sameOwner := true
	if store != nil {
		agent, err := store.GetAgentByID(id)
		if err != nil || agent == nil {
			return nil, fmt.Errorf("agent %d not found", id)
		}
		pid = agent.ProjectID
		// The config route checks against the agent owner's providers. Only
		// pre-check when this caller sees the same pool.
		sameOwner = agent.UserID == serverAPI.userID
		selection.AgentStopped = agent.Status != "running"
		if selection.Provider == "" {
			selection.Provider = configuredAgentDefaultProvider(agent.Config)
		}
	}
	if selection.Provider == "" {
		return nil, fmt.Errorf("provider is required with model: the agent has no saved provider; call agents_models and pass both provider and model")
	}
	if !sameOwner {
		selection.Warning = "provider and model were checked by the server's rules only, because the agent belongs to another user"
		return selection, nil
	}

	providers, err := gatewayProjectProviders(serverAPI, pid, selection.Provider, selection.Model != "")
	if err != nil {
		return nil, err
	}
	if selection.Model == "" {
		return selection, nil
	}
	entry := providers[0]
	if entry.ModelsError != "" {
		selection.Warning = fmt.Sprintf("model %q was not checked against the provider's model list: %s", selection.Model, entry.ModelsError)
		return selection, nil
	}
	for _, model := range entry.Models {
		if model.ID == selection.Model {
			selection.ModelChecked = true
			return selection, nil
		}
	}
	return nil, gatewayUnknownModelError(selection.Provider, selection.Model, entry.Models)
}

func gatewayUnknownModelError(provider, model string, models []gatewayModelChoice) error {
	lower := strings.ToLower(model)
	suggestions := []string{}
	for _, candidate := range models {
		if strings.ToLower(candidate.ID) == lower || strings.ToLower(candidate.Name) == lower {
			suggestions = append(suggestions, candidate.ID)
		}
	}
	if len(suggestions) == 0 {
		for _, candidate := range models {
			if strings.Contains(strings.ToLower(candidate.ID), lower) || strings.Contains(strings.ToLower(candidate.Name), lower) {
				suggestions = append(suggestions, candidate.ID)
			}
		}
	}
	sort.Strings(suggestions)
	if len(suggestions) > 5 {
		suggestions = suggestions[:5]
	}
	msg := fmt.Sprintf("model %q is not offered by provider %q.", model, provider)
	if len(suggestions) > 0 {
		msg += " Did you mean " + strings.Join(suggestions, ", ") + "?"
	}
	return fmt.Errorf("%s Call agents_models with provider=%q for its model ids", msg, provider)
}

// gatewayRejectConfigModelKeys keeps provider and model selection out of the
// agents_update config patch.
func gatewayRejectConfigModelKeys(config map[string]any) error {
	for _, key := range gatewayConfigModelKeys {
		if _, exists := config[key]; exists {
			return fmt.Errorf("config.%s is not accepted: set the agent's provider and model with the top-level provider and model fields (agents_models lists both)", key)
		}
	}
	return nil
}
