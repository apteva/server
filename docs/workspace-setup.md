# Workspace setup

Onboarding starts at `/onboarding` with a searchable AI provider picker, including
Codex. An existing working provider skips connection. Failed verification stays
on the provider step with repair/retry. There is no early Personal/Business or
Developer fork, and connection never creates a starter agent or completes onboarding.

After verification, onboarding proceeds directly to the setup choice without
installing apps. Registration, interface preferences, and onboarding completion
also do not install Conversations. Manual from-scratch setup remains app-free;
a chosen preset installs only its declared dependencies.

Selecting **Configure with AI** explicitly calls
`POST /api/auth/onboarding/prepare` with `{"mode":"ai"}` if Conversations is not
already running. This prepares the shared Conversations dependency using the
existing installer and shows its progress in the AI step before opening Helper.
It is authenticated, retryable, and respects the operator's app allowlist.
Missing mode and `"manual"` are harmless no-ops for older clients; unknown modes
are rejected. Failed AI preparation stays retryable and does not activate Helper.

`/onboarding/setup` offers **Configure with AI** or **Explore presets** inside a
standalone onboarding shell. The preset explorer starts with all categories;
Personal, Business, Work, and Development are filters. Preset preview, editing,
and the from-scratch wizard remain inside onboarding. AI opens Helper's saved
conversation and offers **Review setup** to inspect agents, apps, and the selected
interface. `/setup` redirects to the same flow for later access.

Presets optionally carry `interface_level`: `personal` (Focused), `business`
(Workspace), or `developer` (Advanced). This is explicit metadata, independent of
category. Older presets without it remain valid. It survives generic preset
storage/export and is captured from the creator's interface when saving a workspace
as a preset. All built-in presets declare a recommendation.

Preview and apply accept an optional `interface_level` override and return the
resolved recommendation. Apply saves it in the onboarding caller's setup draft,
preserving an explicit draft selection; it does not change account preferences.
The manual preview and AI review let the user edit their interface selection.
`POST /api/auth/onboarding/complete` accepts that selection and atomically commits
it with the completion stamp for that user only. Replayed completion and later
preset installs preserve an onboarded user's existing preference. Other members'
preferences, capabilities, and authorization are unchanged.

Manual configuration uses the existing preview/apply endpoints. Requests can
include `agent_overrides`, an array of `{key, name, directive, mode}`. The server
validates overrides before installing apps. Preset app assignments remain
authoritative. Stable preset/agent creation keys preserve identity across retries
and later renames. Apply responses retain warnings and running/stopped status;
the UI does not label a partial setup as ready.

`GET` and `PUT /api/projects/:id/setup/session` persist a caller-owned setup draft
under the project's normal editor authorization. The draft includes category,
preset ID, goal, manual edits, interface selection, and current mode. Browsing/saving drafts creates no
agents or apps and does not activate Helper.

**Set up with AI** activates/reuses Apteva Helper and mounts the existing
Conversations contribution directly, without a Builder dependency. It creates
an operator conversation keyed by user and project and sends the selected preset
and goal with an idempotent message ID. Reloads resume that same conversation.
Changing the goal adds a new context message to it. Helper activation retains its
existing provider and access requirements; failures remain visible and retryable.

Helper's gateway exposes `setup_presets_list`, `setup_preview`, and `setup_apply`.
These call the same authenticated catalog and setup endpoints used by the UI.
The project-conversation gateway permits these tools and replaces any supplied
project ID with the trusted conversation project. UI eligibility accepts only
the caller's own active Helper. Helper can attach the shared global Conversations
MCP owned by the installer; this exception does not expose other users' integrations
or project-private apps. For older Conversations versions, the app agent
directory also includes that Helper only for an explicitly attached global app;
it remains absent from the ordinary dashboard agent roster.
The server-owned Helper directive asks it to inspect existing resources, clarify
goals, preview the proposed setup, and obtain agreement before applying changes.
This is a conversational instruction, not a new authorization gate. Existing
server authorization remains in force. Retained Helper processes refresh their
gateway once on next use to pick up these tools. No core directive was changed.

Validation covers onboarding provider flows (including Codex), manual previews
and edits, partial failures, draft reloads, Helper conversation/message identity,
project access checks, invalid overrides, and retry after an agent rename.

Shared Conversations installs must retain the signed-in user on platform
callbacks. The current Conversations HTTP accessor uses SDK
`mountedCtx.WithUserSession(r)`. The server validates its install token and the
forwarded browser session independently, preserving install permission checks
while resolving agents and threads under the requesting user. Expired/revoked
sessions fail instead of reverting to installer ownership. This requires the
matching SDK/Conversations update; it is currently built locally against SDK
v0.77.0 plus the unreleased method. Existing conversation data is unchanged.

Conversations' durable delivery worker uses the app service credential after the
HTTP request ends. Server agent callbacks accept that credential only for agents
owned by the installer or explicitly enabled in this install's agent bindings.
This binding exception is unavailable to browser-scoped callbacks, does not
expand agent directory listings, and still enforces app permissions and install
project boundaries. Revoking the attachment removes the service grant. Global
Helper threads additionally require their owner to have access to the selected
project. No browser credential is stored in the delivery queue.
The same shared-app boundary applies when an agent replies: the proxy resolves a
foreign-owned agent's thread project only from a scope created by that exact
install with a still-enabled binding. Arbitrary project headers, another app's
thread scope, and revoked bindings cannot select it.
