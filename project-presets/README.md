# Workspace presets

Bundled presets live in the four JSON files in this directory. They are embedded
in the server binary; rebuild the server after editing them. Existing
`dashboard` and `dashboard_layout` definitions continue to work.

## Agent teams

Use a team where work has distinct ownership or benefits from independent review.
Lead generation and e-commerce have three roles. Professional services, local
services, webinars, sales, support, research, video-to-blog, and creator work have
two. The engineering team retains its four roles. The software product team adds
five roles for product, technical review, development, QA, and release. General personal assistance,
household planning, wellbeing, executive assistance, software development,
infrastructure, QA, and data analysis retain one agent.

Team members attach the existing `a2a` app and use bounded requests and asynchronous
replies. Their directives identify the owner of shared records and deliverables,
preserve operator approval requirements, and restrict preset collaboration to
the current project's local teammates. These are model instructions, not new
permission enforcement. App attachments are scoped by role; shared apps can
still expose write tools, so directives also specify who should perform writes.

New specialists are instructed to wait for assignments rather than invent
recurring work. They use `unconscious: false` to disable background memory
consolidation and retain working evidence in apps. This does not disable their
Core process or guarantee no model calls while idle. Existing primary agents
retain their memory setting. Keep stable agent keys and first-agent names when
revising recipes; apply reuses existing agents without overwriting their custom
directives or attachments, so revised team recipes apply fully to new workspaces.

Every added role needs an `agent_overview` layout. Put its plain-language
responsibility in the first directive sentence so the preset preview explains
the role without exposing the full coordination instructions.

## Layouts

`layouts.home` takes precedence over the legacy Home fields.
`layouts.agent_overview` maps a preset agent key to its overview widgets.
Each widget has a stable `id`, registered `component`, `size` (`half` or `full`),
and optional `settings`. Use `agent_key` to bind a Home app widget to an agent
from the same preset. The app must be attached to that agent, and its component
must support the destination slot. Agent overview widgets already receive their
page's agent context.

```json
{
  "layouts": {
    "home": [
      {"id": "inbox", "component": "conversations:inbox-overview", "size": "half"},
      {"id": "tasks", "component": "tasks:task-overview", "size": "half",
       "settings": {"show_active": true, "recent_limit": 4}}
    ],
    "agent_overview": {
      "assistant": [
        {"id": "activity", "component": "native:agent-activity", "size": "half"},
        {"id": "chat", "component": "conversations:agent-conversations", "size": "half",
         "settings": {"display_mode": "browser", "composer_layout": "compact"}}
      ]
    }
  }
}
```

Activity is required on agent overviews and remains movable/resizable. Layouts
seed all three interface modes; users can subsequently customize each one.
On mobile the shared widget canvas stacks widgets in their configured order.
An explicit Home layout is also used at the root in Personal view; agent/chat
deep links and the Conversations page keep their focused conversation surface.
Only components supported by an installed app are rendered. Unavailable or
ineligible components retain their place with an explanation, including when
an installed version does not yet support the requested slot.

Apply resolves portable agent keys after creating/reusing agents. Surface writes
use the existing user layout revision checks. Stable preset widget IDs make
retries additive and preserve existing settings, order, and sizes. To intentionally
add another instance of a component, give it a distinct ID and configuration.
Applying to an existing workspace adds widgets; it does not replace that layout.

## App setup guidance

The optional `connections` list describes account/data setup the operator should
review. Each entry has `app`, `title`, `description`, and optional `required`.
`required` describes a workflow dependency; it does not prevent finishing
onboarding. Actual required bindings and authorization remain enforced by apps.

The review shows these instructions before installation. After apply, Configure
opens the existing app settings panel, allowing setup now or later. Widgets from
that app also retain the guidance in a Connection setup disclosure. These are
instructions, not a claim that a connection is authenticated or correctly bound.
Do not put secrets, connection IDs, or installation IDs in presets.

## Capture

Captured templates include Home and the current interface mode's saved agent
overviews, widget sizes/order/settings, role icons, and portable agent bindings.
If no explicit overview exists, the saved project-wide agent detail layout is
used when present. Resource IDs, credentials, and conversation state are omitted
from captured presentation settings. The destination app may need configuration.

Custom presets still use the existing schema-v2 preset envelope and database;
bundled schema-v1 files accept these optional additive fields.

## Individual agent templates

The New Agent picker derives its built-in roles from these same bundled presets.
Each preset agent contributes its name, icon, behavior, instructions, and apps;
the preset supplies its category and source label. The role's opening sentence
becomes the card description. Edit the preset agent to update both entry points.

Stable template IDs use `preset:<preset-id>:<agent-key>`. Startup refreshes these
platform-owned templates. Legacy built-ins are omitted from the picker; existing
agents and app/user templates are preserved. Selecting a role creates only that
agent, using the regular app setup flow, without applying workspace layouts or
creating teammates. Single-agent instructions resolve workspace placeholders and
clarify that other roles are only available if they actually exist.

## Optional app setup steps

Presets can include `setup`, an ordered array of calls to existing public app
MCP tools. For portable presets this lives in `definition.setup`; bundled
presets use `setup` directly. This is additive to agents, apps, connections, and
layouts. Existing presets do not acquire any sample content automatically.
No SDK change or app-specific import endpoint is required. An app must already
expose a suitable synchronous tool for the resource being created.

Each step has:

- `key`: stable, unique identifier (same rules as agent keys).
- `app`: target app slug. Included in dependency installation and preview.
- `tool`: exact, unprefixed public tool name declared in the app manifest.
- `title`: optional short label shown before applying the preset.
- `description`: optional explanation of the content being created.
- `min_app_version`: optional minimum semantic version of the installed app.
- `requires_operator`: reject delegated app/agent calls when the tool needs an operator.
- `input`: JSON object conforming to that tool's input schema.

Within inputs, an object containing only `{"$ref":"..."}` resolves a typed value:

- `project.id`: the project receiving this preset.
- `preset.id` and `step.key`: stable setup identifiers.
- `step.idempotency_key`: `preset:<preset-id>:<step-key>`; the app must scope it to the project.
- `agents.<agent-key>`: the actual created or reused agent ID.
- `steps.<step-key>`: an earlier step's complete result.
- `steps.<step-key>.<path>`: a result property, with dot-separated object keys
  or zero-based array indexes. Missing properties fail; no forward references.

References are not string templates: a numeric ID stays numeric. Tool results
prefer MCP `structuredContent`; otherwise a single JSON text content block is
parsed. Other content retains the MCP result object. Agent directive template
expansion remains separate. Root input fields beginning with `_` are reserved
for trusted server context. Do not embed credentials in a preset.

### Applying and recovery

Apply installs available dependencies, creates/reuses agents, runs setup calls
in order, then applies layouts. Agent lifecycle follows the existing preset
behavior; setup steps should provision app records, not start live jobs.
Calls use the ordinary operator app proxy, with project authorization and a
trusted app principal. App-only tools and declared asynchronous tools are not
supported here. This does not bypass connection requirements or external-action
permissions.

Progress and results are persisted in `preset_setup_steps`, scoped to project,
preset ID, and step key. Completed steps are skipped on subsequent applies.
Resolved inputs, tool, app, and installation ID are fingerprinted; changing an
attempted step blocks automatic replay. Renaming a step or copying the preset
creates a new execution identity and may create duplicate app data. Reconcile
existing records first. Capturing a workspace does not export app records or
invent setup calls. Preset export includes definitions, not execution results.

There is no exactly-once guarantee across app/server failures. A timeout, tool
error, interrupted response, or server restart during a call leaves an uncertain
outcome. Apply stops at that step; later steps stay pending. The UI lets the
operator acknowledge checking the app before retrying that specific step. API
clients do the same by passing `retry_setup_steps: ["<step-key>"]` to apply.
Normal apply does not retry uncertain calls. An unchanged completed step never
runs again, even when explicitly listed for retry. Changed inputs/installations
cannot be retried through this override.

Preview returns `setup_progress`. Apply returns `setup` progress and
`status: "needs_attention"` when setup is incomplete; clients must not equate
HTTP 200 with completed setup. Status values are `pending`, `running`,
`completed`, `blocked`, and `uncertain`. Results remain internal for references.
Concurrent applies to a project are rejected. One server process should own a
database, as with the existing local instance model.

Limits: 50 steps, 64 KiB raw input per step, 64 nesting levels, 256 KiB resolved
input, 1 MiB tool response, and two minutes per call. Calls are synchronous JSON
MCP requests; this initial system does not wait for asynchronous app jobs or
consume SSE tool responses. These constraints make this a small preset setup
mechanism, not a second runtime workflow system.

## Draft tasks

Customer support provisions its Notes triage/escalation guide and a real
“Investigate a ticket” draft through Tasks' existing `create` tool. Tasks 3.7.0
or newer is required. The preset declares `min_app_version` so older installations
produce an actionable upgrade message before the call.

The draft contains instructions, expected outcome, required ticket reference,
optional investigation context, and the actual Support Agent ID as
`suggested_agent_id`. It has no assigned agent, execution thread, or schedule.
Tasks owns configuration, input validation, agent selection, and starting work.
The preset's existing Tasks overview widget displays these records.

The setup call requires an authenticated operator. The server signs a
`preset_operator` subject via the ordinary app proxy after project authorization;
it does not impersonate an agent. Delegated app users and agents cannot acquire
this identity through preset input or caller headers.

`idempotency_key: {"$ref":"step.idempotency_key"}` resolves to a stable preset/step
key. Tasks scopes that key to the project and returns the existing record on
retry, preserving user edits and its current state. The server also retains its
normal setup progress and explicit uncertain-call retry behavior.

The preset preview lists this app content after agents and apps. There is no
separate server assignment store, launch endpoint, or Helper draft card. Existing
saved starter widgets resolve to the Tasks overview (or disappear if that widget
is already present). Existing database snapshots are retained but no longer read.
Reapply Customer support to an existing workspace to provision its actual draft;
startup does not create tasks automatically.

## Software product team

`development-product-team` adds a separate five-agent preset without changing
Engineering team. Its normal `setup` calls create a real **Release to production**
draft in Processes, then a paused, unscheduled assignment pinned to that version.
Portable references bind the five created agents and a human approval role.
Nine connected steps cover scope, candidate preparation, review, staging, QA,
human approval, production promotion, verification, and reporting.

Processes must support the signed `preset_operator` caller for `create` and
`assignment_create` (minimum version 0.16.2). The compatibility patch is prepared
separately from the preset; that app version must be released/installed before
provisioning succeeds. Older apps produce the existing upgrade-required message.
Only draft creation and paused unscheduled assignment creation are delegated;
activation, edits, schedules, and execution are not authorized by preset setup.

Required assignment parameters intentionally have no fabricated defaults.
The operator supplies repository/candidate, acceptance criteria, distinct staging
and production targets and databases, and recovery references, then explicitly
activates the procedure and assignment before starting a run. Database manages
local named SQLite/Pebble databases; the preset does not create infrastructure,
databases, credentials, or new permission boundaries.

Processes owns procedure runs, workers, handoffs, and evidence. Tasks remains for
ad hoc work. Home includes the existing Processes overview widget; the saved draft
and paused assignment are managed in the Processes app. There is no custom server
workflow engine or special Conversations implementation.

Processes creation tools do not currently offer an idempotency argument. The
server's durable completed-step receipts prevent ordinary reapply from creating
duplicates; uncertain outcomes still require inspecting Processes before an
explicit retry, as described above.

## Starter evaluations

Seven bundled presets provision one Evals suite with three cases through the
same `setup` mechanism: Customer support, Sales team, Lead-generation business,
Quality assurance, Software project, Engineering team, and Software product
team. Evals is discovered from the setup calls and installed through the normal
dependency resolver, including its Environments and LLM dependencies. It is a
project service; evaluated agents do not acquire Evals management tools.

The suite step calls `eval_suite_create` and binds the preset's primary agent
with `agents.<key>`. The next three steps call `eval_case_create`, referring to
`steps.evals-suite-v1.id` for the suite returned by Evals. Inputs require Evals
0.9.5 or newer and an authenticated operator. No new preset fields, import API,
worker, scheduling system, or Server knowledge of grading is involved.

Sales and lead generation cover saved CRM state, contact deduplication, and
unsent outreach. Support covers grounded policy answers, escalation when policy
is missing, and preservation of ticket context. Engineering and QA cover
bounded patch proposals, failed or incomplete verification, harness failures,
workspace ownership, independent review, exact candidate evidence, process
dispatch ownership, and release approval gates.

Each case supplies an inline Environments specification with stable app names,
separate app data, blocked external networking, mock integrations, and bounded
run duration/turns. Seeds create only synthetic records inside that run. Model
calls and app tools still execute: isolation is not a replacement for tool
permissions. These starter engineering cases assess supplied code/log fixtures
and plans using Tasks only; they do not attach Code, Workspaces, or Deploy, touch
a host checkout, execute a patch, dispatch a real team, or prove an end-to-end
release. Add disposable repository fixtures before extending that coverage.

Cases use `agentic-quality-v2`: saved outcomes, evidence, and safety are critical;
final acknowledgement wording is flexible. Record IDs may remain in the durable
task result. The normal Evals interface owns execution, grading, traces, history,
and optional continuous checks. After setup, choose an available judge model
from Evals' catalog and run the suite against its preselected agent. There is no
hardcoded provider/model, automatic initial experiment, or recurring schedule.

Stable suite/case IDs are namespaced by preset and recipe version. Completed
setup receipts preserve user edits, run history, and schedules on reapply. Do
not revise an attempted step in place to overwrite customized cases. As with
other setup tools, uncertain calls require inspecting the app before retrying;
the app's create tools are upserts, not a new exactly-once guarantee. Existing
projects receive nothing at startup; explicitly applying/reapplying a preset
provisions its new steps. User-authored presets remain unchanged.

Local tests cover recipe references/isolation, app version requirements, normal
proxy calls, and durable reapply behavior. An optional contract test provisions
all seven suites and 21 cases against a real Evals executable in a temporary DB:

```sh
GOWORK=off \
APTEVA_PRESET_EVALS_BINARY=/absolute/path/to/evals \
APTEVA_PRESET_EVALS_MIGRATIONS=/absolute/path/to/evals/migrations \
go test -run TestProjectPresetEvals -count=1 .
```

Run that command from Server. The executable and migrations must be from the
same supporting release. The test gateway supplies assertion metadata only;
no model, real agent, external service, or existing installation is used.

### One starter for Personal and Work

The bundled Personal and Work templates each create one starter agent. Personal
assistant and Work assistant are recommended; the other templates remain available
under More templates and search. Stable preset IDs and primary agent keys are
retained, including `work-executive` / `chief-of-staff`. No startup migration
changes existing projects or agents. Reapplying a preset preserves existing agents
and reports missing capability attachments instead of silently granting access.

Presets may declare built-in MCP capabilities separately from installed apps:

```json
{
  "capabilities": [{
    "name": "apteva-server",
    "allowed_tools": ["agents_list", "agents_get", "agents_create", "agents_start"]
  }]
}
```

`allowed_tools` is required, validated, saved on the new agent, and enforced by
the existing management gateway for discovery and calls. An empty array denies
all tools; omission of the entire capability keeps existing behavior. Currently
Apteva Server is the supported built-in capability. This is a declarative grant,
not an installable app, a separate management service, or an OS sandbox. Normal
owner/project access checks still apply. Restricted callers cannot change a
management-enabled agent or grant management access to another agent. Existing
agents without a tool policy retain their existing grants.

The confirmation screen shows the management grant before creating anything.
Starter directives treat a user's explicit request to create a bounded specialist
as authorization, ask for missing or consequential permissions, use exact app
attachments and stable creation keys, and do not copy management access or
credentials to specialists. Accounts are connected when needed. Conversations
and A2A remain ordinary apps and use their existing APIs. No special transport is
introduced. The generic single-agent setup result opens `/conversations?agent=ID`.
An empty workspace may initialize its assistant selection once; existing saved
preferences, including a disabled assistant, are preserved. Sales and Support
retain their manual, isolated starter Evals against the unchanged primary keys.

Validation: `TestStarter*` covers all nine templates, preview cancellation,
normal provisioning and app attachments, idempotent specialist creation, tool
filtering, escalation and cross-project rejection, and saved preferences. The
opt-in `TestStarterFlowLive` uses actual local Core and cached app executables
with entirely temporary databases; it tests creation through a starter
conversation, a direct specialist conversation, and an A2A exchange.
