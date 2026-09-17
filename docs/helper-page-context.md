# Chat assistant page context and project app tools

The dashboard passes an optional versioned `pageContext` prop to the
Conversations widget. Conversations sends it as `page_context` with each user
message, validates/allowlists it, and persists it in message metadata. Inbound
agent events label that snapshot as untrusted descriptive data—not instructions
or authorization. Changing pages never changes an earlier message's snapshot.

Supported pages: dashboard, installed app, agent detail, apps catalog, settings.
Only project/page identifiers, display names, active panel/thread/tab are shared.
There is no screenshot, DOM scraping, arbitrary query string, form value, or
credential collection. `viewed_agent_id` never changes the conversation recipient.
The composer chip can remove sharing for the current page selection; changing
selection restores the chip. Settings → Chat assistant can disable sharing
altogether; its default is enabled. Public conversations reject page context.

## App tools

- `app_tool_search(query, app?)` searches running project-owned installations.
  Results include descriptions, complete input schemas, annotations, installation
  IDs, and signed short-lived references. At most 12 catalogs are probed and five
  tools/32 KiB are returned. Schemas are never silently truncated.
- `app_tool_call(reference, arguments)` revalidates the operator conversation,
  project membership, app/tool availability, and schema. References bind agent,
  thread, project, install, tool, and schema for 15 minutes.
- Calls use the existing app proxy and retain the trusted caller agent, so SDK
  permission/resource gates still apply. `app_only` and disabled tools are not
  exposed. Page context cannot supply the trusted identity or project.
- A running Conversations operator thread is required; public rooms, another
  user's conversation, and cross-project references fail closed. Currently this
  broker belongs to the platform Helper gateway and does not expose globally
  installed platform apps or make arbitrary MCP/network connections.
- Calls are audited with app, installation, project, tool and status, but not
  arguments. No permanent Helper MCP attachment is made. Tool descriptions warn
  that consequential work requires approval; the broker does not infer consent
  from a page snapshot.

## Verification

Run `go test -run '^TestHelperAppTools|^TestPlatformMCP' -count=1` from server.
Conversations has `TestPageContext*` and `TestOperatorContext*`, plus the
`pageContext.test.tsx` composer test. Dashboard has context derivation, default
preference, target separation and disabled-sharing tests.

`scripts/test-helper-app-tools-live.ts` is explicitly opt-in and uses the real
local Helper through Conversations. See its header for required environment
variables. It checks Tickets supplied only in page context, the real
search→call trace and successful Tickets audit, compares the result with the
Tickets API, and separately checks viewed-agent/recipient separation. It keeps
test conversation history, stops its own test threads, and never changes
providers, tickets, grants, or permanent MCP configuration. A provider quota or
balance failure is a blocked LLM test, never a pass.
