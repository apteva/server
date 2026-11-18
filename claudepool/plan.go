package claudepool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// askTimeout bounds one question to Claude Code: start, answer, exit.
var askTimeout = 30 * time.Second

// PlanUsage is the plan usage meter as Claude Code reports it: what share
// of each limit window is used. Claude Code fetches it with its own login;
// none of that login passes through Apteva.
type PlanUsage struct {
	SubscriptionType string
	// Available is false when plan limits do not apply to the login.
	Available bool
	Windows   []UsageWindow
	CheckedAt time.Time
}

// UsageWindow is one limit window.
type UsageWindow struct {
	// ID is five_hour, seven_day, seven_day_opus, seven_day_sonnet,
	// seven_day_oauth_apps, or model:<name> for a weekly window that
	// covers one model.
	ID string
	// Label names the model of a model:<name> window, as Claude sends it.
	Label string
	// Utilization is the percentage used, 0-100.
	Utilization *float64
	ResetsAt    *time.Time
}

// ModelOption is one model Claude Code offers this login.
type ModelOption struct {
	// Value is what --model and set_model accept.
	Value         string
	DisplayName   string
	Description   string
	ResolvedModel string
}

// PlanUsage returns the plan usage, asking Claude Code when the cached
// copy is older than maxAge. When asking fails, the last answer (if any)
// comes back with the error.
func (p *Pool) PlanUsage(ctx context.Context, maxAge time.Duration) (PlanUsage, error) {
	p.probeMu.Lock()
	defer p.probeMu.Unlock()
	if p.usage != nil && time.Since(p.usage.CheckedAt) < maxAge {
		return *p.usage, nil
	}
	raw, err := p.ask(ctx, map[string]any{"subtype": "get_usage", "skip_behaviors": true})
	if err == nil {
		var usage PlanUsage
		if usage, err = parsePlanUsage(raw); err == nil {
			usage.CheckedAt = time.Now()
			p.usage = &usage
			return usage, nil
		}
	}
	if p.usage != nil {
		return *p.usage, err
	}
	return PlanUsage{}, err
}

// Models returns the models Claude Code offers, asking it when the cached
// list is older than maxAge.
func (p *Pool) Models(ctx context.Context, maxAge time.Duration) ([]ModelOption, error) {
	p.probeMu.Lock()
	defer p.probeMu.Unlock()
	if p.models != nil && time.Since(p.modelsAt) < maxAge {
		return append([]ModelOption(nil), p.models...), nil
	}
	raw, err := p.ask(ctx, map[string]any{"subtype": "list_models"})
	if err != nil {
		return nil, err
	}
	var body struct {
		Models []struct {
			Value         string `json:"value"`
			DisplayName   string `json:"displayName"`
			Description   string `json:"description"`
			ResolvedModel string `json:"resolvedModel"`
			Disabled      bool   `json:"disabled"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("read Claude Code models: %w", err)
	}
	models := []ModelOption{}
	for _, m := range body.Models {
		if m.Value == "" || m.Disabled {
			continue
		}
		models = append(models, ModelOption{Value: m.Value, DisplayName: m.DisplayName, Description: m.Description, ResolvedModel: m.ResolvedModel})
	}
	p.models, p.modelsAt = models, time.Now()
	return append([]ModelOption(nil), models...), nil
}

// ask starts a short-lived claude process, sends it one control request
// and returns the answer. The process runs no model turn and saves no
// session.
func (p *Pool) ask(ctx context.Context, request map[string]any) (json.RawMessage, error) {
	p.mu.Lock()
	cli, closed := p.cfg.CLI, p.closed
	p.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	bin, err := cli.Resolve()
	if err != nil {
		return nil, err
	}
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--tools", "",
		"--strict-mcp-config",
		"--setting-sources", "project",
		"--no-session-persistence",
	}
	proc, err := startProcess(bin, args, cli.Env("CLAUDE_CODE_DISABLE_CLAUDE_MDS=1"), p.cfg.WorkDir)
	if err != nil {
		return nil, err
	}
	defer func() {
		proc.close()
		go func() {
			for range proc.msgs {
			}
		}()
	}()
	id, err := proc.control(request)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	subtype, _ := request["subtype"].(string)
	for {
		select {
		case msg, ok := <-proc.msgs:
			if !ok {
				return nil, fmt.Errorf("Claude Code %s: %s", subtype, proc.failure())
			}
			if msg.Type != "control_response" || msg.Response == nil || msg.Response.RequestID != id {
				continue
			}
			if msg.Response.Subtype == "error" {
				return nil, fmt.Errorf("Claude Code %s: %s", subtype, msg.Response.Error)
			}
			return msg.Response.Body, nil
		case <-ctx.Done():
			return nil, fmt.Errorf("Claude Code %s: %w", subtype, ctx.Err())
		}
	}
}

func parsePlanUsage(raw json.RawMessage) (PlanUsage, error) {
	type window struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    *string  `json:"resets_at"`
	}
	var body struct {
		SubscriptionType    *string `json:"subscription_type"`
		RateLimitsAvailable bool    `json:"rate_limits_available"`
		RateLimits          *struct {
			FiveHour          *window `json:"five_hour"`
			SevenDay          *window `json:"seven_day"`
			SevenDayOpus      *window `json:"seven_day_opus"`
			SevenDaySonnet    *window `json:"seven_day_sonnet"`
			SevenDayOAuthApps *window `json:"seven_day_oauth_apps"`
			ModelScoped       []struct {
				DisplayName string `json:"display_name"`
				window
			} `json:"model_scoped"`
		} `json:"rate_limits"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return PlanUsage{}, fmt.Errorf("read Claude Code usage: %w", err)
	}
	usage := PlanUsage{Available: body.RateLimitsAvailable}
	if body.SubscriptionType != nil {
		usage.SubscriptionType = *body.SubscriptionType
	}
	add := func(id, label string, w *window) {
		if w == nil || (w.Utilization == nil && w.ResetsAt == nil) {
			return
		}
		out := UsageWindow{ID: id, Label: label, Utilization: w.Utilization}
		if w.ResetsAt != nil {
			if t, err := time.Parse(time.RFC3339Nano, *w.ResetsAt); err == nil {
				out.ResetsAt = &t
			}
		}
		usage.Windows = append(usage.Windows, out)
	}
	if limits := body.RateLimits; limits != nil {
		add("five_hour", "", limits.FiveHour)
		add("seven_day", "", limits.SevenDay)
		add("seven_day_opus", "Opus", limits.SevenDayOpus)
		add("seven_day_sonnet", "Sonnet", limits.SevenDaySonnet)
		add("seven_day_oauth_apps", "Apps", limits.SevenDayOAuthApps)
		for _, m := range limits.ModelScoped {
			if m.DisplayName == "" {
				continue
			}
			w := m.window
			add("model:"+strings.ToLower(m.DisplayName), m.DisplayName, &w)
		}
	}
	return usage, nil
}
