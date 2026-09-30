# Workspace presets

Bundled presets live in the four JSON files in this directory. They are embedded
in the server binary; rebuild the server after editing them. Existing
`dashboard` and `dashboard_layout` definitions continue to work.

## Agent teams

Use a team where work has distinct ownership or benefits from independent review.
Lead generation and e-commerce have three roles. Professional services, local
services, webinars, sales, support, research, video-to-blog, and creator work have
two. The engineering team retains its four roles. General personal assistance,
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
