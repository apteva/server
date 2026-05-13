package main

// agent_templates.go — pre-canned starter agent configs for the
// "build your first agent" wizard. Three sources share one table:
//
//   builtin  — seeded inline in store.go's migrate(). INSERT OR IGNORE
//              preserves operator edits across upgrades; new platform
//              defaults ship under a fresh id.
//   app      — contributed by an installed app via its manifest.
//              apps_loader upserts on install/upgrade.
//   user     — operator's own templates (save-from-agent or hand-rolled).
//
// The listing endpoint returns the union, filtered by user_template_hidden
// for the caller's hide list. Edits to builtin/app rows are allowed in
// place (operator edits their copy); save-as-new is the convention for
// keeping the canonical shipped version available alongside.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// AgentTemplate is the wire shape returned by the templates endpoints.
// recommended_apps is stored as JSON in the DB and emitted as a flat
// []string here so the dashboard doesn't have to parse it itself.
type AgentTemplate struct {
	ID              string    `json:"id"`
	UserID          int64     `json:"user_id,omitempty"`
	Source          string    `json:"source"`      // "builtin" | "app" | "user"
	SourceRef       string    `json:"source_ref,omitempty"`
	Name            string    `json:"name"`
	// Icon is a short name (e.g. "user", "search", "code") that the
	// dashboard resolves to a stroked SVG component. Keeps the wire
	// payload tiny and the rendering consistent with the rest of
	// the platform's lucide-style icon set — no emojis, no per-app
	// PNG fetches at render time.
	Icon            string    `json:"icon,omitempty"`
	Description     string    `json:"description"`
	Directive       string    `json:"directive"`
	Mode            string    `json:"mode"`
	Unconscious     bool      `json:"unconscious"`
	RecommendedApps []string  `json:"recommended_apps"`
	// Requirements is the structured TODO list the wizard's Setup
	// step renders into install + bind + connect actions, and that
	// the future meta-agent reads as a checklist. Persisted as a
	// JSON array on the row.
	Requirements []Requirement `json:"requirements"`
	// ResolvedLogos is server-derived (not stored). The list endpoint
	// walks Requirements, looks up app marketplace icons + integration
	// catalog logos, and emits a flat array the dashboard renders
	// inside each template card.
	ResolvedLogos []TemplateLogo `json:"resolved_logos,omitempty"`
	// SuggestedEvals is the starter eval set shipped with the
	// template. One entry per template in PR-1 (16 total minus
	// 'empty'). Copied into agent_evals at agent-create time so
	// the wizard's Verify step has something to run immediately.
	// Not persisted on agent_templates itself — lives only on the
	// Go-side builtin slice in this file. PR-2 will support
	// app-contributed suggested evals via the manifest.
	SuggestedEvals []SuggestedEval `json:"suggested_evals,omitempty"`
	SortOrder       int       `json:"sort_order"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Requirement is one entry on a template's setup checklist. Four
// kinds:
//
//   kind=app          — sidecar app the agent uses. Wizard installs
//                       via apps.install. Slug names the app
//                       (storage, media, channel-email, ...).
//   kind=integration  — third-party SaaS credential (slack, github,
//                       stripe). compatible_slugs lists any one of
//                       which can satisfy the role; the wizard
//                       picks the first that has a bound connection
//                       or prompts to create one.
//   kind=channel      — agent-time delivery channel (email, slack,
//                       telegram). Channel binding lives on the
//                       agent, not on the project.
//   kind=skill        — markdown playbook the agent loads. Wizard
//                       pushes via skills.push.
//
// Reason is operator-facing copy ("Used to send drafted replies").
// Required gates Setup advancement when true.
type Requirement struct {
	Kind            string         `json:"kind"`
	Slug            string         `json:"slug,omitempty"`
	Role            string         `json:"role,omitempty"`
	Type            string         `json:"type,omitempty"`   // for kind=channel
	CompatibleSlugs []string       `json:"compatible_slugs,omitempty"`
	Capabilities    []string       `json:"capabilities,omitempty"`
	BindTo          *BindTo        `json:"bind_to,omitempty"`
	Reason          string         `json:"reason,omitempty"`
	Required        bool           `json:"required"`
	Source          string         `json:"source,omitempty"` // builtin | app:<slug> | url for skills
	Config          map[string]any `json:"config,omitempty"` // pre-fill for app install
}

// BindTo names the app + role on the app that this integration
// requirement should bind to once both pieces (app install +
// connection) exist. Apps loader resolves this into an
// app_agent_bindings row.
type BindTo struct {
	App  string `json:"app"`
	Role string `json:"role"`
}

// TemplateLogo is one icon resolved from a Requirement. Emitted in
// AgentTemplate.ResolvedLogos for the wizard's card render.
//
//   kind = "app" | "integration" | "channel"
//   source = "direct" (declared on the template) | "derived" (pulled
//            from a required app's requires.integrations)
//   via = app slug that pulled in a derived entry, empty otherwise
type TemplateLogo struct {
	Kind    string `json:"kind"`
	Slug    string `json:"slug"`
	IconURL string `json:"icon_url,omitempty"`
	Label   string `json:"label"`
	Source  string `json:"source"`
	Via     string `json:"via,omitempty"`
}

// builtinAgentTemplates is the canonical shipped set. Seeded at
// migrate() time via INSERT OR IGNORE — operator edits to existing
// rows are preserved across upgrades. To roll a new platform-wide
// version of a template, give it a fresh id ("personal-assistant-v2")
// so the upgrade-time IGNORE doesn't silently fall back to the old
// directive.
//
// Order in this slice matters only as documentation; SortOrder on
// each row drives the wizard render. Integration-driven templates
// (Slack bot, GitHub helper, Sales prospecting, …) lead so the
// wizard's card grid foregrounds recognisable upstream brands.
var builtinAgentTemplates = []AgentTemplate{
	{
		ID:          "slack-bot",
		Source:      "builtin",
		Name:        "Slack bot",
		Icon:        "message",
		Description: "Watches the workspace, replies to mentions, summarises threads, posts a daily digest.",
		Mode:        "learn",
		Unconscious: true,
		SortOrder:   10,
		Requirements: []Requirement{
			{Kind: "integration", CompatibleSlugs: []string{"slack"}, Capabilities: []string{"chat.send"}, Required: true, Reason: "Read channels and reply to mentions."},
		},
		Directive: `You are a Slack assistant for this workspace. Watch the channels you have access to for mentions and direct messages.

When mentioned:
- Read the recent thread context before replying.
- Reply conversationally, in line with the channel's tone (skim 20 recent messages to calibrate).
- Brief is better than verbose.

Daily routine:
- At 9am, summarise yesterday's high-activity threads (≥10 messages) into a one-paragraph digest. Post to a designated channel — ask the operator which one on first run.

Tone: match the team. No formal preambles, no apologies for being a bot.`,
		SuggestedEvals: []SuggestedEval{
			{
				ID:   "slack-bot:default",
				Name: "Mention triggers thread summary",
				Trigger: EvalTrigger{
					Type: "chat_message",
					Payload: map[string]any{
						"channel": "#engineering",
						"text":    "@apteva can you summarise this thread?",
					},
				},
				Goals: []string{
					"Read recent thread context before replying.",
					"Reply in the same channel with a short summary.",
					"Keep the reply brief — under 200 words.",
					"Match the team's informal tone — no formal preambles or sign-offs.",
				},
				Mocks: []EvalMock{
					{App: "slack", Tool: "read_messages",
						Return: json.RawMessage(`{"messages":[{"user":"alice","text":"the auth migration failed in staging"},{"user":"bob","text":"rolling back now"},{"user":"alice","text":"want me to file an incident?"}]}`)},
					{App: "slack", Tool: "send_message", Return: json.RawMessage(`{"ok":true,"ts":"123"}`)},
				},
			},
		},
	},
	{
		ID:          "github-helper",
		Source:      "builtin",
		Name:        "GitHub helper",
		Icon:        "github",
		Description: "Reads pull requests, drafts reviews, flags stale PRs, summarises diffs.",
		Mode:        "cautious",
		Unconscious: true,
		SortOrder:   15,
		Requirements: []Requirement{
			{Kind: "integration", CompatibleSlugs: []string{"github"}, Capabilities: []string{"repo.read", "pr.comment"}, Required: true, Reason: "Read PRs and post review comments."},
		},
		Directive: `You are a GitHub assistant. Watch open pull requests in the repositories you have access to.

For each new PR:
- Read the diff and the PR description.
- Note anything that looks like regression risk — test changes, dependency bumps, schema migrations, removed code that other code depends on.
- Post a one-paragraph summary as a PR comment: what the change does + your read on risk.

Daily:
- Surface PRs open >5 days with no activity. Post a digest to the designated review channel (ask on first run).

Tone: technical and direct. "Probably fine" is unacceptable — be specific or say you don't know.`,
		SuggestedEvals: []SuggestedEval{
			{
				ID:   "github-helper:default",
				Name: "New PR gets a review comment",
				Trigger: EvalTrigger{
					Type: "webhook",
					Payload: map[string]any{
						"event": "pull_request.opened",
						"pr":    map[string]any{"number": 42, "title": "Bump auth library to v2", "files_changed": 3, "additions": 120, "deletions": 30},
					},
				},
				Goals: []string{
					"Read the PR diff before commenting.",
					"Post a review comment summarising the change in 1-2 sentences.",
					"Flag regression risk if the diff touches authentication or migrations.",
					"Don't approve or request changes — just comment.",
				},
				Mocks: []EvalMock{
					{App: "github", Tool: "get_pull_request", Return: json.RawMessage(`{"number":42,"title":"Bump auth library to v2","body":"upgrades jose from 1.x to 2.x","diff":"--- a/auth.go\n+++ b/auth.go\n@@ ... @@"}`)},
					{App: "github", Tool: "create_review_comment", Return: json.RawMessage(`{"id":99,"ok":true}`)},
				},
			},
		},
	},
	{
		ID:          "sales-prospecting",
		Source:      "builtin",
		Name:        "Sales prospecting",
		Icon:        "target",
		Description: "Cadence outbound emails through Gmail, log every touch into HubSpot, file responses in storage.",
		Mode:        "cautious",
		Unconscious: true,
		SortOrder:   20,
		Requirements: []Requirement{
			{Kind: "integration", CompatibleSlugs: []string{"gmail"}, Capabilities: []string{"email.send", "email.read"}, Required: true, Reason: "Send outbound + read replies."},
			{Kind: "integration", CompatibleSlugs: []string{"hubspot"}, Capabilities: []string{"contact.read", "contact.write", "activity.log"}, Required: true, Reason: "Read contact records + log every touchpoint."},
			{Kind: "app", Slug: "storage", Required: false, Reason: "Archive responses + per-contact notes."},
		},
		Directive: `You are a sales prospecting assistant. Run outbound + follow-up cadences against contacts you load from HubSpot.

Per contact:
- Read the HubSpot profile + any prior interactions before composing.
- Compose one short, specific email. One ask per message. Mention something only this contact would care about.
- Wait 4 business days for reply before scheduled follow-up. Stop after 3 follow-ups with no response.
- Log every send, every reply, every meeting booked back into HubSpot as a note on the contact record.

SAFETY: never send the first message in a cadence without showing it to me first. Follow-ups within an approved cadence are pre-approved.

Tone: warm and human. Avoid every cold-email cliché ("hope this finds you well", "circle back", "synergies"). Read like a peer reaching out.`,
		SuggestedEvals: []SuggestedEval{
			{
				ID:   "sales-prospecting:default",
				Name: "Drafts the first email, doesn't send",
				Trigger: EvalTrigger{
					Type: "chat_message",
					Payload: map[string]any{
						"text": "Start a cadence for Sarah Chen, CTO at Acme — they just announced their Series B.",
					},
				},
				Goals: []string{
					"Read Sarah's HubSpot profile before drafting.",
					"Compose ONE specific email that references the Series B news.",
					"Do NOT send the first email — show me the draft and wait for confirmation.",
					"Log the draft as an activity on the HubSpot contact.",
					"Avoid cold-email clichés (hope this finds you well / circle back / synergies).",
				},
				Mocks: []EvalMock{
					{App: "hubspot", Tool: "get_contact", Return: json.RawMessage(`{"id":"c-1","name":"Sarah Chen","title":"CTO","company":"Acme","email":"sarah@acme.com","activities":[]}`)},
					{App: "hubspot", Tool: "log_activity", Return: json.RawMessage(`{"id":"a-1","ok":true}`)},
					{App: "gmail", Tool: "send_email", Error: "test_mode: first-email send blocked, must be drafted via create_draft"},
					{App: "gmail", Tool: "create_draft", Return: json.RawMessage(`{"draft_id":"d-1","ok":true}`)},
				},
			},
		},
	},
	{
		ID:          "customer-support",
		Source:      "builtin",
		Name:        "Customer support",
		Icon:        "life-buoy",
		Description: "Reads Intercom tickets, checks Stripe subscription state, drafts replies, escalates to Slack.",
		Mode:        "cautious",
		Unconscious: true,
		SortOrder:   25,
		Requirements: []Requirement{
			{Kind: "integration", CompatibleSlugs: []string{"intercom"}, Capabilities: []string{"conversation.read", "conversation.write"}, Required: true, Reason: "Read incoming tickets + post replies."},
			{Kind: "integration", CompatibleSlugs: []string{"stripe"}, Capabilities: []string{"subscription.read", "customer.read"}, Required: true, Reason: "Check subscription state before replying to billing questions."},
			{Kind: "integration", CompatibleSlugs: []string{"slack"}, Capabilities: []string{"chat.send"}, Required: true, Reason: "Escalate technical issues to the on-call channel."},
		},
		Directive: `You are a customer support assistant. Watch Intercom for new conversations.

For each one:
- Look up the customer in Stripe before replying. Note their plan, next invoice date, payment method status.
- Billing questions → draft a reply with the relevant subscription details. Show me the draft, send only after confirm.
- Technical issues → post a summary to the on-call Slack channel + tag @oncall. Reply to the customer with "thanks, our engineering team is on it, ETA Xh".
- Feature requests → tag the conversation feature-request, file a one-liner in our internal tracker, reply with a thank-you.
- Churn signals (angry tone, mentions of cancellation, repeated questions) → tag churn-risk and ping me directly in Slack DM.

Tone: warm, brief, acknowledge the customer's frustration before solving.`,
		SuggestedEvals: []SuggestedEval{
			{
				ID:   "customer-support:default",
				Name: "Billing question pulls Stripe state",
				Trigger: EvalTrigger{
					Type: "webhook",
					Payload: map[string]any{
						"event": "intercom.conversation_created",
						"from":  "user@example.com",
						"body":  "Why did I just get charged $99? I thought I was on the free plan.",
					},
				},
				Goals: []string{
					"Look up the customer in Stripe before replying.",
					"Reference the actual subscription details (plan, last invoice) in the draft.",
					"Show me the draft — do not auto-send.",
					"Acknowledge the customer's confusion in the opening line.",
				},
				Mocks: []EvalMock{
					{App: "stripe", Tool: "get_customer", Return: json.RawMessage(`{"id":"cus_1","email":"user@example.com","subscription":{"plan":"Pro","status":"active","last_invoice":{"amount":99,"date":"2026-05-10"}}}`)},
					{App: "intercom", Tool: "draft_reply", Return: json.RawMessage(`{"draft_id":"r-1","ok":true}`)},
				},
			},
		},
	},
	{
		ID:          "meeting-coordinator",
		Source:      "builtin",
		Name:        "Meeting coordinator",
		Icon:        "calendar",
		Description: "Schedules across Google Calendar, drafts Gmail invites, posts summaries to Slack.",
		Mode:        "learn",
		Unconscious: true,
		SortOrder:   30,
		Requirements: []Requirement{
			{Kind: "integration", CompatibleSlugs: []string{"google-calendar"}, Capabilities: []string{"event.read", "event.write"}, Required: true, Reason: "Read availability + create events."},
			{Kind: "integration", CompatibleSlugs: []string{"gmail"}, Capabilities: []string{"email.send"}, Required: true, Reason: "Draft and send invites."},
			{Kind: "integration", CompatibleSlugs: []string{"slack"}, Capabilities: []string{"chat.send"}, Required: false, Reason: "Post summaries to a planning channel."},
		},
		Directive: `You are a meeting coordinator. Given a "schedule a meeting with X" request:

- Check Google Calendar for mutual availability across the relevant participants.
- Draft a Gmail invite with three time options spanning the next 5 business days. Send only after I confirm the draft.
- Once a time is locked: create the calendar event, attach the agenda if one's been drafted, send a confirmation.
- Optionally post a one-line summary to #planning on Slack ("Booked: chat with Y on Thursday 2pm about Z").

Daily 8am: post tomorrow's full calendar to #planning. Flag conflicts (back-to-back without travel time, double-booked rooms, meetings without agendas).

Tone: short and exact. Times always in the recipient's timezone.`,
		SuggestedEvals: []SuggestedEval{
			{
				ID:   "meeting-coordinator:default",
				Name: "Schedule request proposes 3 times",
				Trigger: EvalTrigger{
					Type: "chat_message",
					Payload: map[string]any{
						"text": "Set up a 30-minute chat with Diana next week to talk about the launch.",
					},
				},
				Goals: []string{
					"Check Google Calendar for mutual availability across the next 5 business days.",
					"Draft a Gmail invite with three time options.",
					"Do not send the invite — show me the draft and wait for confirmation.",
				},
				Mocks: []EvalMock{
					{App: "google-calendar", Tool: "find_availability", Return: json.RawMessage(`{"slots":[{"date":"2026-05-13","time":"14:00"},{"date":"2026-05-14","time":"10:00"},{"date":"2026-05-15","time":"15:30"}]}`)},
					{App: "gmail", Tool: "create_draft", Return: json.RawMessage(`{"draft_id":"d-1","ok":true}`)},
				},
			},
		},
	},
	{
		ID:          "devops-bot",
		Source:      "builtin",
		Name:        "DevOps bot",
		Icon:        "git-branch",
		Description: "Watches GitHub for releases + CI failures, posts to Slack, files Linear issues for security alerts.",
		Mode:        "autonomous",
		Unconscious: true,
		SortOrder:   35,
		Requirements: []Requirement{
			{Kind: "integration", CompatibleSlugs: []string{"github"}, Capabilities: []string{"repo.read", "workflow.read", "release.read"}, Required: true, Reason: "Watch releases + CI runs + security alerts."},
			{Kind: "integration", CompatibleSlugs: []string{"slack"}, Capabilities: []string{"chat.send"}, Required: true, Reason: "Post engineering updates to the team channel."},
			{Kind: "integration", CompatibleSlugs: []string{"linear"}, Capabilities: []string{"issue.create"}, Required: false, Reason: "File Linear issues from Dependabot alerts."},
		},
		Directive: `You are an engineering ops assistant. Watch GitHub for state changes and surface what matters to the team.

Events to act on:
- New release → post to the engineering Slack channel with the release notes summarised in 2-3 bullets.
- Failing CI run on the main branch → @-mention the on-call engineer in Slack with a link to the run.
- Dependabot security alert → file a Linear issue (severity = the alert's severity), tag with security, link from Slack.
- Force-push on main → red-alert in Slack immediately, no waiting.

Weekly Monday 10am: post a digest of release velocity (commits merged, PRs landed, mean time-to-merge, blocked PRs).

Tone: terse engineering register. Links matter more than prose.`,
		SuggestedEvals: []SuggestedEval{
			{
				ID:   "devops-bot:default",
				Name: "Failing CI on main pings on-call",
				Trigger: EvalTrigger{
					Type: "webhook",
					Payload: map[string]any{
						"event": "github.workflow_run.completed",
						"run":   map[string]any{"branch": "main", "status": "completed", "conclusion": "failure", "workflow": "CI", "url": "https://github.com/acme/repo/actions/runs/123"},
					},
				},
				Goals: []string{
					"Post a notification to the engineering Slack channel.",
					"Include the workflow run URL in the message.",
					"@-mention the on-call engineer.",
					"Don't file a Linear issue — this isn't a security alert.",
				},
				Mocks: []EvalMock{
					{App: "slack", Tool: "send_message", Return: json.RawMessage(`{"ok":true}`)},
					{App: "linear", Tool: "create_issue", Error: "test_mode: linear should not be called for CI failures"},
				},
			},
		},
	},
	{
		ID:          "site-monitoring",
		Source:      "builtin",
		Name:        "Site monitoring",
		Icon:        "activity",
		Description: "Watches AWS CloudWatch + S3 metrics, fires PagerDuty for severity, posts to Slack #incidents.",
		Mode:        "autonomous",
		Unconscious: true,
		SortOrder:   40,
		Requirements: []Requirement{
			{Kind: "integration", CompatibleSlugs: []string{"aws-s3"}, Capabilities: []string{"metrics.read", "log.read"}, Required: true, Reason: "Read CloudWatch alarms + S3 metrics."},
			{Kind: "integration", CompatibleSlugs: []string{"pagerduty"}, Capabilities: []string{"incident.create"}, Required: true, Reason: "Page on-call for severity ≥ warning."},
			{Kind: "integration", CompatibleSlugs: []string{"slack"}, Capabilities: []string{"chat.send"}, Required: true, Reason: "Post to #incidents."},
		},
		Directive: `You are an infrastructure monitor. Watch CloudWatch alarms and S3 metrics for the configured services.

For each alarm:
- Check the metric for context: is this a known issue (recurring, has a runbook), a fresh spike, a sustained trend?
- Severity ≥ warning → page via PagerDuty with the metric name, the breached threshold, the duration. Post a parallel notice to #incidents in Slack with the same details + a link to the dashboard.
- Severity info → log to /alerts/<date>.md in storage (one line per event). No paging.
- Auto-recovered alarms → log only, no paging.

Daily 9am: post a 24h health digest to #incidents — total alarms by severity, top 3 noisiest services, anything that paged overnight.

Never escalate the same alarm twice within 30 minutes (suppress via alarm name).`,
		SuggestedEvals: []SuggestedEval{
			{
				ID:   "site-monitoring:default",
				Name: "Critical alarm pages on-call",
				Trigger: EvalTrigger{
					Type: "webhook",
					Payload: map[string]any{
						"event": "cloudwatch.alarm_state_changed",
						"alarm": map[string]any{"name": "api-5xx-rate", "severity": "critical", "metric": "5xx_per_minute", "threshold": 50, "value": 127},
					},
				},
				Goals: []string{
					"Create a PagerDuty incident with the alarm name and breached value.",
					"Post a parallel notice to the #incidents Slack channel.",
					"Don't auto-acknowledge or auto-resolve the PagerDuty incident.",
				},
				Mocks: []EvalMock{
					{App: "pagerduty", Tool: "create_incident", Return: json.RawMessage(`{"id":"PD-1","ok":true}`)},
					{App: "slack", Tool: "send_message", Return: json.RawMessage(`{"ok":true}`)},
				},
			},
		},
	},
	{
		ID:          "content-distribution",
		Source:      "builtin",
		Name:        "Content distribution",
		Icon:        "share-2",
		Description: "Reads Notion drafts, formats for LinkedIn + Mailchimp, schedules publication after operator approval.",
		Mode:        "cautious",
		Unconscious: true,
		SortOrder:   45,
		Requirements: []Requirement{
			{Kind: "integration", CompatibleSlugs: []string{"notion"}, Capabilities: []string{"page.read", "database.read", "page.update"}, Required: true, Reason: "Read drafts + mark them as published."},
			{Kind: "integration", CompatibleSlugs: []string{"linkedin"}, Capabilities: []string{"post.create"}, Required: false, Reason: "Cross-post to LinkedIn."},
			{Kind: "integration", CompatibleSlugs: []string{"mailchimp"}, Capabilities: []string{"campaign.create"}, Required: false, Reason: "Schedule newsletter campaigns."},
		},
		Directive: `You are a content distribution assistant. Watch a Notion database for posts marked status=ready.

For each ready post:
- Format two variants: a LinkedIn version (long-form, ~300 words, one image, hook-driven first line) and a Mailchimp newsletter version (the full post + a CTA at the bottom).
- Show me both formatted versions side-by-side. Wait for "go" before scheduling.
- Schedule LinkedIn for next weekday at 10am (or the next available 10am if today is already past). Schedule Mailchimp for Tuesday or Thursday at 11am (the two windows that historically perform best).
- Once scheduled, mark the Notion entry status=published with timestamps for each surface.

Never publish anything without my explicit go-ahead per surface. "OK" applies to LinkedIn only; ask separately for the newsletter.`,
		SuggestedEvals: []SuggestedEval{
			{
				ID:   "content-distribution:default",
				Name: "Ready post produces both variants, waits for go-ahead",
				Trigger: EvalTrigger{
					Type: "webhook",
					Payload: map[string]any{
						"event": "notion.page_status_changed",
						"page":  map[string]any{"id": "p-1", "title": "Why we rewrote our scheduler", "status": "ready", "body": "<long post body...>"},
					},
				},
				Goals: []string{
					"Format a LinkedIn variant (long-form, ~300 words).",
					"Format a Mailchimp newsletter variant.",
					"Do NOT schedule either yet — show me both variants and wait for explicit per-platform 'go'.",
				},
				Mocks: []EvalMock{
					{App: "notion", Tool: "get_page", Return: json.RawMessage(`{"id":"p-1","title":"Why we rewrote our scheduler","body":"long post body...","status":"ready"}`)},
					{App: "linkedin", Tool: "schedule_post", Error: "test_mode: scheduling blocked, draft first"},
					{App: "mailchimp", Tool: "schedule_campaign", Error: "test_mode: scheduling blocked, draft first"},
				},
			},
		},
	},
	{
		ID:          "personal-assistant",
		Source:      "builtin",
		Name:        "Personal assistant",
		Icon:        "user",
		Description: "Triage email, schedule, remember preferences, draft replies.",
		Mode:        "learn",
		Unconscious: true,
		SortOrder:   50,
		Requirements: []Requirement{
			{Kind: "app", Slug: "storage", Required: true, Reason: "Draft replies + attachments archive."},
			{Kind: "app", Slug: "calendar", Required: true, Reason: "Read + write calendar events."},
			{Kind: "app", Slug: "channel-email", Required: true, Reason: "Send drafted replies."},
			{Kind: "integration", Role: "smtp", CompatibleSlugs: []string{"gmail", "smtp", "sendgrid"}, Capabilities: []string{"email.send"}, BindTo: &BindTo{App: "channel-email", Role: "smtp"}, Required: true, Reason: "Credentials for outgoing mail."},
		},
		Directive: `You are a personal assistant. Keep my inbox triaged, my calendar coherent, my reminders timely.

Daily rhythm:
- Scan new mail at 8am, 1pm, 5pm local time. Tag, archive obvious noise, surface the rest in a one-paragraph summary at the end of each scan.
- Draft replies on anything I've handled a similar message for before. Show me the draft, send only after I confirm.
- Track decisions I make in messages ("Yes, let's do Tuesday") and remember them so I don't have to repeat myself.

Tone: terse, helpful, never apologise for being a bot. Treat the inbox like a queue, not a museum.`,
		SuggestedEvals: []SuggestedEval{
			{
				ID:   "personal-assistant:default",
				Name: "Reschedule request drafts a reply",
				Trigger: EvalTrigger{
					Type: "chat_message",
					Payload: map[string]any{
						"from": "alice@example.com",
						"text": "Email arrived from Alice: 'Can we push our 3pm meeting Tuesday to 4pm instead?'",
					},
				},
				Goals: []string{
					"Check the calendar for Tuesday 4pm availability before replying.",
					"Draft an email response — do NOT send yet.",
					"Show me the draft and wait for explicit confirmation.",
				},
				Mocks: []EvalMock{
					{App: "calendar", Tool: "find_events", Return: json.RawMessage(`{"events":[],"available":true,"day":"Tuesday","time":"16:00"}`)},
					{App: "channel-email", Tool: "create_draft", Return: json.RawMessage(`{"draft_id":"d-1","ok":true}`)},
				},
			},
		},
	},
	{
		ID:          "todo-coach",
		Source:      "builtin",
		Name:        "Todo coach",
		Icon:        "check-square",
		Description: "Tracks your todos, nudges you on what's slipping, sends a daily plan.",
		Mode:        "learn",
		Unconscious: true,
		SortOrder:   60,
		Requirements: []Requirement{
			{Kind: "app", Slug: "todo", Required: true, Reason: "Source of truth for tasks, lists, snoozes."},
			{Kind: "app", Slug: "messaging", Required: true, Reason: "Send the morning plan + reminders."},
		},
		Directive: `You are a todo coach. The todo app is your source of truth — read it before every nudge, write back every change.

Daily rhythm:
- 8am: pull the Today list + anything overdue. Send a one-message plan via messaging: 3 priorities, 5 minutes total to read.
- 6pm: pull what got marked done today. Send a one-line recap + the auto-rollover items for tomorrow.

Conversational quick-add:
- When I message you a sentence ("write the launch email", "groceries on Saturday"), parse it into a todo with a sensible list/tag/due-date and confirm in one short line ("Added to Today, due Friday").

Stalled-task surfacing:
- Anything older than 5 days with no edits → mention it in the morning plan with "still on your list?". Never auto-delete; suggest snooze/archive and wait for me.

Tone: encouraging, never preachy. Don't lecture me about productivity.`,
		SuggestedEvals: []SuggestedEval{
			{
				ID:   "todo-coach:default",
				Name: "Quick-add parses a one-liner",
				Trigger: EvalTrigger{
					Type: "chat_message",
					Payload: map[string]any{
						"text": "write the launch email by Friday p1",
					},
				},
				Goals: []string{
					"Create a todo with title 'write the launch email'.",
					"Set the due date to this Friday.",
					"Set the priority to P1.",
					"Confirm in one short line — don't ask clarifying questions when the input is unambiguous.",
				},
				Mocks: []EvalMock{
					{App: "todo", Tool: "quick_add", Return: json.RawMessage(`{"id":1,"title":"write the launch email","due":"2026-05-15","priority":"p1","ok":true}`)},
				},
			},
		},
	},
	{
		ID:          "health-logger",
		Source:      "builtin",
		Name:        "Health logger",
		Icon:        "heart-pulse",
		Description: "Logs weight, sleep, mood, workouts from conversational one-liners. Weekly recap.",
		Mode:        "cautious",
		Unconscious: true,
		SortOrder:   65,
		Requirements: []Requirement{
			{Kind: "app", Slug: "health", Required: true, Reason: "Time-series store for every logged metric."},
			{Kind: "app", Slug: "messaging", Required: true, Reason: "Send the weekly digest + nudges."},
		},
		Directive: `You are a health logging assistant. Capture conversational one-liners into the health app as structured datapoints; surface trends without judgement.

Logging:
- "weight 78.4" → health_log kind=weight value=78.4 unit=kg.
- "slept 7.5h" → kind=sleep_hours value=7.5.
- "ran 5k 26min" → kind=workout subkind=run distance_km=5 duration_min=26.
- "mood 6/10 tired" → kind=mood value=6 notes="tired".
- Confirm each log in one short line with the parsed values. Ask for clarification only when the unit is genuinely ambiguous (e.g. "120/80" — assume BP).

Weekly recap (Sundays 8pm):
- Pull the last 7 days. Compute deltas vs. the prior week for the pinned kinds. Send a short message: what trended up, what trended down, one observation, no prescription.

Never recommend medical decisions. If I describe symptoms, log them and say "noted, see a doctor if it persists" — don't diagnose.`,
		SuggestedEvals: []SuggestedEval{
			{
				ID:   "health-logger:default",
				Name: "One-liner logs structured datapoint",
				Trigger: EvalTrigger{
					Type: "chat_message",
					Payload: map[string]any{
						"text": "weight 78.4",
					},
				},
				Goals: []string{
					"Log a health datapoint with kind=weight value=78.4.",
					"Use kg as the unit (or note the unit explicitly).",
					"Confirm in one short line — don't ask clarifying questions for unambiguous inputs.",
				},
				Mocks: []EvalMock{
					{App: "health", Tool: "health_log", Return: json.RawMessage(`{"id":42,"kind":"weight","value":78.4,"unit":"kg","ok":true}`)},
				},
			},
		},
	},
	{
		ID:          "crm-assistant",
		Source:      "builtin",
		Name:        "CRM assistant",
		Icon:        "users",
		Description: "Tracks contacts in the CRM app, drafts follow-ups, sends scheduled touches via messaging.",
		Mode:        "cautious",
		Unconscious: true,
		SortOrder:   70,
		Requirements: []Requirement{
			{Kind: "app", Slug: "crm", Required: true, Reason: "Contact store with multi-channel addresses and activity log."},
			{Kind: "app", Slug: "messaging", Required: true, Reason: "Send drafted follow-ups across email/SMS/WhatsApp."},
		},
		Directive: `You are a CRM assistant. Keep the contact records clean, draft follow-ups, send approved touches via messaging. The CRM app's activity log is the source of truth for every interaction — read before sending, write after.

Per follow-up:
- Pull the contact's activity log + custom attributes before composing.
- Draft a short, specific message that references the last interaction. Show me the draft. Send only on confirm.
- After send, append an activity entry with the channel, the gist, and the next suggested follow-up date.

Weekly Monday 9am:
- Surface contacts with no activity in 30+ days, sorted by importance signals (tags, custom attributes you've learned matter). Send a digest message: "5 contacts to re-engage this week, want me to draft?".

Never bulk-send. One draft, one confirmation, one send. Treat the contact list as a relationship graph, not a mailing list.`,
		SuggestedEvals: []SuggestedEval{
			{
				ID:   "crm-assistant:default",
				Name: "Drafts follow-up, doesn't send",
				Trigger: EvalTrigger{
					Type: "chat_message",
					Payload: map[string]any{
						"text": "Follow up with John Park — we last talked about the integration timeline.",
					},
				},
				Goals: []string{
					"Pull John's activity log from the CRM before composing.",
					"Reference the integration-timeline conversation in the draft.",
					"Show me the draft — don't send yet.",
				},
				Mocks: []EvalMock{
					{App: "crm", Tool: "get_contact", Return: json.RawMessage(`{"id":"c-7","name":"John Park","email":"john@partner.com","activities":[{"date":"2026-05-01","note":"talked integration timeline, awaiting their security review"}]}`)},
					{App: "messaging", Tool: "draft_message", Return: json.RawMessage(`{"draft_id":"m-1","ok":true}`)},
				},
			},
		},
	},
	{
		ID:          "image-studio-pal",
		Source:      "builtin",
		Name:        "Image studio",
		Icon:        "image",
		Description: "Generates images on request, files them into storage with descriptive names + tags.",
		Mode:        "cautious",
		Unconscious: false,
		SortOrder:   75,
		Requirements: []Requirement{
			{Kind: "app", Slug: "image-studio", Required: true, Reason: "Generate images via OpenAI/Replicate/Stability."},
			{Kind: "app", Slug: "storage", Required: true, Reason: "Save generations as permanent shareable references."},
		},
		Directive: `You are an image generation assistant. Take rough briefs, produce variants, file them with descriptive names so they're findable later.

Per request:
- Ask one clarifying question if anything is genuinely ambiguous (subject, style, aspect ratio). Otherwise dive straight in.
- Generate 3 variants by default. Vary one dimension across the three (style, framing, palette) so the choice is meaningful.
- Save each generation to /.images/<yyyy-mm>/<short-slug>-<variant>.png in storage. Use the prompt to pick the slug.
- Reply with thumbnails + a one-line description per variant. Wait for me to pick before doing any follow-up edits.

Cost discipline: if I haven't said "more", stop at 3 variants. If I say "tighter on the second one", regenerate that one — don't reroll the whole batch.

Tone: art-director short. No "Here are some images I generated for you!" preamble.`,
		SuggestedEvals: []SuggestedEval{
			{
				ID:   "image-studio-pal:default",
				Name: "Brief produces 3 variants and saves them",
				Trigger: EvalTrigger{
					Type: "chat_message",
					Payload: map[string]any{
						"text": "Generate a hero image for a launch post about our new scheduler — clean, blue-tinted, minimalist.",
					},
				},
				Goals: []string{
					"Generate three variants — not one, not five.",
					"Vary one dimension across the three (style, framing, or palette).",
					"Save each generation to storage under /.images/.",
					"Reply with thumbnails + a one-line description per variant.",
				},
				Mocks: []EvalMock{
					{App: "image-studio", Tool: "generate", Return: json.RawMessage(`{"id":"img-1","url":"https://example.com/i.png"}`)},
					{App: "storage", Tool: "files_upload", Return: json.RawMessage(`{"id":1,"path":"/.images/2026-05/scheduler-v1.png","ok":true}`)},
				},
			},
		},
	},
	{
		ID:          "social-poster",
		Source:      "builtin",
		Name:        "Social poster",
		Icon:        "megaphone",
		Description: "Drafts cross-platform posts, schedules through the Social app, archives drafts in storage.",
		Mode:        "cautious",
		Unconscious: true,
		SortOrder:   80,
		Requirements: []Requirement{
			{Kind: "app", Slug: "social", Required: true, Reason: "Post to X/Instagram/LinkedIn/TikTok/Reddit/Threads (accounts connect inside the app)."},
			{Kind: "app", Slug: "storage", Required: true, Reason: "Archive drafts + reference assets."},
		},
		Directive: `You are a social media drafting assistant. Connected accounts live inside the social app (X, LinkedIn, Instagram, Threads, etc.) — you don't need separate integration setup. Storage holds drafts and references.

Per post request:
- Read 3 recent posts from /writing/voice-samples/ in storage before drafting (or whatever folder I've asked you to mirror). Mirror cadence and tone.
- Produce platform-tailored variants: X (≤280 chars, hook-first), LinkedIn (1-2 short paragraphs, no emoji walls), Instagram caption (1 hook line + body + tags).
- File the draft set as /.social/<yyyy-mm-dd>-<slug>.md in storage with all variants in one doc.
- Show me the doc + a per-platform preview. Wait for explicit per-platform "go" before scheduling. "OK" on X doesn't authorise LinkedIn.

Scheduling: weekday 10am local unless I specify otherwise. Never schedule the same content to two platforms within an hour of each other.

Tone: my voice, not corporate-LinkedIn. Stay editorial.`,
		SuggestedEvals: []SuggestedEval{
			{
				ID:   "social-poster:default",
				Name: "Draft request produces per-platform variants",
				Trigger: EvalTrigger{
					Type: "chat_message",
					Payload: map[string]any{
						"text": "Draft a social post about shipping our scheduler rewrite — punchy on Twitter, story-driven on LinkedIn.",
					},
				},
				Goals: []string{
					"Read voice samples from /writing/voice-samples/ in storage before drafting.",
					"Produce a Twitter variant (≤280 chars, hook-first).",
					"Produce a LinkedIn variant (1-2 short paragraphs).",
					"Save the draft set to storage under /.social/.",
					"Do NOT schedule either — wait for per-platform confirmation.",
				},
				Mocks: []EvalMock{
					{App: "storage", Tool: "files_list", Return: json.RawMessage(`{"files":[{"path":"/writing/voice-samples/sample1.md","content":"short editorial voice example..."}]}`)},
					{App: "storage", Tool: "files_upload", Return: json.RawMessage(`{"id":1,"path":"/.social/2026-05-12-scheduler.md","ok":true}`)},
					{App: "social", Tool: "schedule_post", Error: "test_mode: scheduling blocked, draft first"},
				},
			},
		},
	},
	{
		ID:          "research-bot",
		Source:      "builtin",
		Name:        "Research bot",
		Icon:        "search",
		Description: "Browse, summarise, file findings.",
		Mode:        "cautious",
		Unconscious: true,
		SortOrder:   55,
		Requirements: []Requirement{
			{Kind: "app", Slug: "computer", Required: true, Reason: "Browse the web via headless Chrome."},
			{Kind: "app", Slug: "storage", Required: true, Reason: "File summaries as markdown notes."},
		},
		Directive: `You are a research assistant. Given a question or topic, find and synthesise the best available sources.

Workflow per question:
- Spend the first 5 minutes scoping: what does "good enough" look like for this question, and which sources are likely authoritative?
- Browse and read. Take notes as you go — short bullets with a link, not full quotes.
- Summarise in two layers: a 3-sentence headline answer at the top, then a structured breakdown of evidence with citations underneath.
- File the summary as a markdown note in storage under /research/<date>-<slug>.md so I can retrieve it later.

Tone: skeptical of single-source claims, comfortable saying "I couldn't find a clear answer."`,
		SuggestedEvals: []SuggestedEval{
			{
				ID:   "research-bot:default",
				Name: "Research request files a markdown note",
				Trigger: EvalTrigger{
					Type: "chat_message",
					Payload: map[string]any{
						"text": "Research the current state of Postgres logical replication tooling for multi-region failover.",
					},
				},
				Goals: []string{
					"Use the computer app to browse the web.",
					"Take notes as you go — bullets with links, not full quotes.",
					"Save a markdown summary to storage under /research/.",
					"Reply with a 3-sentence headline answer + a pointer to the saved note.",
				},
				Mocks: []EvalMock{
					{App: "computer", Tool: "browse", Return: json.RawMessage(`{"url":"https://www.postgresql.org/docs/current/logical-replication.html","text":"PostgreSQL logical replication ships changes by replicating decoded WAL..."}`)},
					{App: "storage", Tool: "files_upload", Return: json.RawMessage(`{"id":1,"path":"/research/2026-05-12-postgres-logical-replication.md","ok":true}`)},
				},
			},
		},
	},
	{
		ID:          "empty",
		Source:      "builtin",
		Name:        "Empty",
		Icon:        "box",
		Description: "Start from scratch. I'll write the directive myself.",
		Mode:        "learn",
		Unconscious: false,
		SortOrder:   999,
		Requirements: []Requirement{},
		Directive:   "",
	},
}

// seedBuiltinTemplates idempotently writes the canonical builtin
// templates. INSERT OR IGNORE on first boot; UPDATE pass for
// fields the platform owns (requirements, sort_order) on every
// subsequent boot so a release that ships new shape gets it
// applied without trampling operator-edited directives or modes.
func seedBuiltinTemplates(db *sql.DB) {
	for _, t := range builtinAgentTemplates {
		reqJSON, _ := json.Marshal(t.Requirements)
		if t.Requirements == nil {
			reqJSON = []byte("[]")
		}
		db.Exec(`
			INSERT OR IGNORE INTO agent_templates
				(id, user_id, source, source_ref, name, icon, description, directive,
				 mode, unconscious, recommended_apps, requirements, sort_order)
			VALUES (?, NULL, 'builtin', '', ?, ?, ?, ?, ?, ?, '[]', ?, ?)`,
			t.ID, t.Name, t.Icon, t.Description, t.Directive, t.Mode,
			boolToInt(t.Unconscious), string(reqJSON), t.SortOrder,
		)
		// Platform-owned fields that follow the shipped value on
		// every boot. Directive/mode/unconscious/description stay
		// editable by the operator after first install.
		db.Exec(
			`UPDATE agent_templates
			    SET requirements = ?, icon = ?, sort_order = ?
			  WHERE id = ? AND source = 'builtin'`,
			string(reqJSON), t.Icon, t.SortOrder, t.ID,
		)
	}
}

// ListAgentTemplates returns every template visible to the user:
// builtin + app + the user's own. Per-user-hidden entries are filtered
// out. Sorted by (sort_order, name) so the wizard renders in a
// deterministic order.
func (s *Store) ListAgentTemplates(userID int64) ([]AgentTemplate, error) {
	rows, err := s.db.Query(`
		SELECT id, COALESCE(user_id, 0), source, source_ref, name, icon,
		       description, directive, mode, unconscious, recommended_apps,
		       requirements, sort_order, created_at, updated_at
		  FROM agent_templates t
		 WHERE (t.user_id IS NULL OR t.user_id = ?)
		   AND t.id NOT IN (
		       SELECT template_id FROM user_template_hidden WHERE user_id = ?
		   )
		 ORDER BY sort_order ASC, name ASC`,
		userID, userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentTemplate
	for rows.Next() {
		t, err := scanAgentTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetAgentTemplate returns one template by id, scoped to what the
// caller is allowed to see (builtin/app are global, user-owned must
// match userID).
func (s *Store) GetAgentTemplate(userID int64, id string) (*AgentTemplate, error) {
	row := s.db.QueryRow(`
		SELECT id, COALESCE(user_id, 0), source, source_ref, name, icon,
		       description, directive, mode, unconscious, recommended_apps,
		       requirements, sort_order, created_at, updated_at
		  FROM agent_templates
		 WHERE id = ?
		   AND (user_id IS NULL OR user_id = ?)`,
		id, userID,
	)
	t, err := scanAgentTemplate(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, err
		}
		return nil, err
	}
	return &t, nil
}

// CreateAgentTemplate inserts a new user-owned template. id is
// generated from the name (lowercase, hyphenated) prefixed with the
// user id so cross-user collisions can't happen.
func (s *Store) CreateAgentTemplate(userID int64, t AgentTemplate) (*AgentTemplate, error) {
	if t.ID == "" {
		t.ID = userTemplateID(userID, t.Name)
	}
	appsJSON, _ := json.Marshal(t.RecommendedApps)
	reqJSON, _ := json.Marshal(t.Requirements)
	if t.Requirements == nil {
		reqJSON = []byte("[]")
	}
	_, err := s.db.Exec(`
		INSERT INTO agent_templates
			(id, user_id, source, source_ref, name, icon, description,
			 directive, mode, unconscious, recommended_apps, requirements,
			 sort_order)
		VALUES (?, ?, 'user', '', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, userID, t.Name, t.Icon, t.Description, t.Directive,
		t.Mode, boolToInt(t.Unconscious), string(appsJSON), string(reqJSON),
		t.SortOrder,
	)
	if err != nil {
		return nil, err
	}
	return s.GetAgentTemplate(userID, t.ID)
}

// UpdateAgentTemplate edits a row the caller is allowed to write
// (their own user-owned rows, or the global builtin/app rows). Edits
// to builtin/app rows are allowed by design — operators have
// permission to tune the shipped templates to their preferences and
// the seed step's INSERT OR IGNORE keeps their edits across upgrades.
func (s *Store) UpdateAgentTemplate(userID int64, id string, t AgentTemplate) error {
	appsJSON, _ := json.Marshal(t.RecommendedApps)
	reqJSON, _ := json.Marshal(t.Requirements)
	if t.Requirements == nil {
		reqJSON = []byte("[]")
	}
	_, err := s.db.Exec(`
		UPDATE agent_templates
		   SET name = ?, icon = ?, description = ?, directive = ?,
		       mode = ?, unconscious = ?, recommended_apps = ?,
		       requirements = ?, sort_order = ?,
		       updated_at = CURRENT_TIMESTAMP
		 WHERE id = ?
		   AND (user_id IS NULL OR user_id = ?)`,
		t.Name, t.Icon, t.Description, t.Directive, t.Mode,
		boolToInt(t.Unconscious), string(appsJSON), string(reqJSON),
		t.SortOrder, id, userID,
	)
	return err
}

// DeleteAgentTemplate removes a user-owned row. For builtin/app rows,
// the caller should use HideAgentTemplate instead — those rows stay
// in the table so other users / fresh logins still see them.
func (s *Store) DeleteAgentTemplate(userID int64, id string) error {
	_, err := s.db.Exec(
		`DELETE FROM agent_templates WHERE id = ? AND user_id = ?`,
		id, userID,
	)
	return err
}

// HideAgentTemplate adds the template to the user's hide list. No-op
// if it's already there.
func (s *Store) HideAgentTemplate(userID int64, id string) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO user_template_hidden (user_id, template_id) VALUES (?, ?)`,
		userID, id,
	)
	return err
}

// UnhideAgentTemplate removes the template from the user's hide list.
func (s *Store) UnhideAgentTemplate(userID int64, id string) error {
	_, err := s.db.Exec(
		`DELETE FROM user_template_hidden WHERE user_id = ? AND template_id = ?`,
		userID, id,
	)
	return err
}

// userTemplateID builds a stable id for user-owned templates. Format:
// usr-<userID>:<slug>. Conflict probability stays at zero for the same
// user (the dashboard nudges them to pick a different name on dupe)
// and is namespaced away from builtin/app id collisions by design.
func userTemplateID(userID int64, name string) string {
	slug := strings.ToLower(strings.TrimSpace(name))
	slug = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r == ' ' || r == '_' || r == '-':
			return '-'
		}
		return -1
	}, slug)
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "template"
	}
	return "usr-" + i64s(userID) + ":" + slug
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func i64s(v int64) string {
	// avoid pulling in strconv here just for one call
	return itoa64(v)
}

// scanAgentTemplate handles both *sql.Row and *sql.Rows via the
// shared rowScanner interface from skills_handlers.go (same package).
func scanAgentTemplate(r rowScanner) (AgentTemplate, error) {
	var (
		t         AgentTemplate
		uid       int64
		unc       int
		appsJSON  string
		reqJSON   string
		createdAt string
		updatedAt string
	)
	if err := r.Scan(
		&t.ID, &uid, &t.Source, &t.SourceRef, &t.Name, &t.Icon,
		&t.Description, &t.Directive, &t.Mode, &unc, &appsJSON,
		&reqJSON, &t.SortOrder, &createdAt, &updatedAt,
	); err != nil {
		return t, err
	}
	if uid != 0 {
		t.UserID = uid
	}
	t.Unconscious = unc == 1
	if appsJSON != "" {
		_ = json.Unmarshal([]byte(appsJSON), &t.RecommendedApps)
	}
	if t.RecommendedApps == nil {
		t.RecommendedApps = []string{}
	}
	if reqJSON != "" {
		_ = json.Unmarshal([]byte(reqJSON), &t.Requirements)
	}
	if t.Requirements == nil {
		t.Requirements = []Requirement{}
	}
	t.CreatedAt, _ = parseTime(createdAt)
	t.UpdatedAt, _ = parseTime(updatedAt)
	return t, nil
}

// resolveTemplateLogos walks a template's Requirements and resolves
// each one to a TemplateLogo the wizard's card can render. The
// resolver is server-side so the dashboard never has to fetch the
// catalog/marketplace itself to render a card.
//
// Direct logos come from the Requirement itself: kind=integration uses
// the integrations catalog (s.catalog.Get(slug).Logo + .Name);
// kind=app uses the curated marketplace registry's Icon for the slug.
//
// Derived logos: for each kind=app requirement, peek at its manifest
// (via the cache) and pull its requires.integrations through the same
// catalog lookup. The card can then show "this template uses storage,
// which itself needs SMTP" without the template author having to
// enumerate transitive deps. Marked source="derived" + via=<app slug>
// so the dashboard can render them distinctly.
//
// Channel kind maps to the integration that backs it (email→smtp,
// slack→slack, telegram→telegram) so the wizard renders a familiar
// brand logo rather than a generic "channel" pictogram.
func (s *Server) resolveTemplateLogos(t *AgentTemplate) {
	seen := map[string]bool{} // dedupe by kind+slug
	out := []TemplateLogo{}
	add := func(l TemplateLogo) {
		key := l.Kind + ":" + l.Slug
		if seen[key] || l.Slug == "" {
			return
		}
		seen[key] = true
		out = append(out, l)
	}
	channelToIntegration := map[string]string{
		"email":    "smtp",
		"slack":    "slack",
		"telegram": "telegram",
	}
	var registry *CuratedRegistry
	registryFor := func() *CuratedRegistry {
		if registry != nil {
			return registry
		}
		r, err := s.fetchAndCacheRegistry()
		if err != nil {
			return nil
		}
		registry = r
		return r
	}
	resolveIntegration := func(slug, source, via string) {
		if slug == "" {
			return
		}
		entry := s.catalog.Get(slug)
		if entry == nil {
			return
		}
		var icon string
		if entry.Logo != nil {
			icon = *entry.Logo
		}
		add(TemplateLogo{
			Kind:    "integration",
			Slug:    slug,
			IconURL: icon,
			Label:   entry.Name,
			Source:  source,
			Via:     via,
		})
	}
	resolveApp := func(slug, source string) {
		if slug == "" {
			return
		}
		reg := registryFor()
		if reg == nil {
			add(TemplateLogo{Kind: "app", Slug: slug, Label: slug, Source: source})
			return
		}
		want := normalizeAppName(slug)
		for _, e := range reg.Apps {
			if normalizeAppName(e.Name) == want {
				label := e.DisplayName
				if label == "" {
					label = e.Name
				}
				add(TemplateLogo{Kind: "app", Slug: slug, IconURL: e.Icon, Label: label, Source: source})
				return
			}
		}
		add(TemplateLogo{Kind: "app", Slug: slug, Label: slug, Source: source})
	}
	// manifestURLFor looks up the registry entry for an app slug and
	// returns its manifest_url, or "" if the registry doesn't know
	// the app (offline / typo'd slug).
	manifestURLFor := func(slug string) string {
		reg := registryFor()
		if reg == nil {
			return ""
		}
		want := normalizeAppName(slug)
		for _, e := range reg.Apps {
			if normalizeAppName(e.Name) == want {
				return e.ManifestURL
			}
		}
		return ""
	}
	for _, req := range t.Requirements {
		switch req.Kind {
		case "integration":
			slug := req.Slug
			if slug == "" && len(req.CompatibleSlugs) > 0 {
				slug = req.CompatibleSlugs[0]
			}
			resolveIntegration(slug, "direct", "")
		case "app":
			resolveApp(req.Slug, "direct")
		case "channel":
			if mapped, ok := channelToIntegration[req.Type]; ok {
				resolveIntegration(mapped, "direct", "")
			}
		}
	}

	// Derived pass: for each required app, peek at its manifest and
	// surface the integration slugs the app itself needs (e.g. the
	// messaging app needs aws-ses, twilio). Marked source="derived"
	// + via=<app slug> so the dashboard renders them at lower opacity
	// after a divider. Failures (network, parse, unknown slug) are
	// silent — the card still has its direct logos.
	for _, req := range t.Requirements {
		if req.Kind != "app" || !req.Required || req.Slug == "" {
			continue
		}
		url := manifestURLFor(req.Slug)
		if url == "" {
			continue
		}
		manifest, err := s.fetchAndCacheManifest(url)
		if err != nil || manifest == nil {
			continue
		}
		for _, dep := range manifest.Requires.Integrations {
			// kind on IntegrationDep defaults to "integration".
			kind := dep.Kind
			if kind == "" {
				kind = "integration"
			}
			if kind != "integration" || !dep.Required {
				continue
			}
			depSlug := ""
			if len(dep.CompatibleSlugs) > 0 {
				depSlug = dep.CompatibleSlugs[0]
			}
			resolveIntegration(depSlug, "derived", req.Slug)
		}
	}
	t.ResolvedLogos = out
}

// ─── HTTP handlers ─────────────────────────────────────────────────

// GET /agent-templates
func (s *Server) handleListAgentTemplates(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	list, err := s.store.ListAgentTemplates(userID)
	if err != nil {
		http.Error(w, "list templates: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []AgentTemplate{}
	}
	for i := range list {
		s.resolveTemplateLogos(&list[i])
		mergeSuggestedEvals(&list[i])
	}
	writeJSON(w, list)
}

// mergeSuggestedEvals attaches the in-memory builtin's
// SuggestedEvals onto the wire-bound AgentTemplate. The DB row
// doesn't carry them — they live exclusively on the Go-side
// builtinAgentTemplates slice — so the wizard wouldn't see them
// without this merge. PR-2 promotes them to a JSON column on the
// table when app-contributed templates start shipping their own
// evals via manifest.
func mergeSuggestedEvals(t *AgentTemplate) {
	if t.Source != "builtin" {
		return
	}
	for i := range builtinAgentTemplates {
		if builtinAgentTemplates[i].ID == t.ID {
			t.SuggestedEvals = builtinAgentTemplates[i].SuggestedEvals
			return
		}
	}
}

// POST /agent-templates — body shape mirrors AgentTemplate (no id
// expected; the store assigns one from user_id + name).
func (s *Server) handleCreateAgentTemplate(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	var body AgentTemplate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Directive) == "" {
		http.Error(w, "directive required", http.StatusBadRequest)
		return
	}
	if body.Mode == "" {
		body.Mode = "learn"
	}
	t, err := s.store.CreateAgentTemplate(userID, body)
	if err != nil {
		http.Error(w, "create template: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if t != nil {
		s.resolveTemplateLogos(t); mergeSuggestedEvals(t)
	}
	writeJSON(w, t)
}

// GET /agent-templates/:id   — handleAgentTemplateByID dispatches by method.
func (s *Server) handleAgentTemplateByID(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	path := strings.TrimPrefix(r.URL.Path, "/agent-templates/")
	// Allow /agent-templates/:id/hide for the per-user hide endpoint.
	if strings.HasSuffix(path, "/hide") && r.Method == http.MethodPost {
		id := strings.TrimSuffix(path, "/hide")
		if err := s.store.HideAgentTemplate(userID, id); err != nil {
			http.Error(w, "hide: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "hidden"})
		return
	}
	if strings.HasSuffix(path, "/unhide") && r.Method == http.MethodPost {
		id := strings.TrimSuffix(path, "/unhide")
		if err := s.store.UnhideAgentTemplate(userID, id); err != nil {
			http.Error(w, "unhide: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "unhidden"})
		return
	}
	id := path
	switch r.Method {
	case http.MethodGet:
		t, err := s.store.GetAgentTemplate(userID, id)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		s.resolveTemplateLogos(t); mergeSuggestedEvals(t)
		writeJSON(w, t)
	case http.MethodPut:
		var body AgentTemplate
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if err := s.store.UpdateAgentTemplate(userID, id, body); err != nil {
			http.Error(w, "update: "+err.Error(), http.StatusInternalServerError)
			return
		}
		t, _ := s.store.GetAgentTemplate(userID, id)
		if t != nil {
			s.resolveTemplateLogos(t); mergeSuggestedEvals(t)
		}
		writeJSON(w, t)
	case http.MethodDelete:
		if err := s.store.DeleteAgentTemplate(userID, id); err != nil {
			http.Error(w, "delete: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "deleted"})
	default:
		http.Error(w, "GET, PUT, or DELETE", http.StatusMethodNotAllowed)
	}
}
