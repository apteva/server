# App discovery and agent attachments

An installed app exports its display name, description and MCP tool catalog.
`apps_list` provides compact discovery and pagination; `apps_get` provides the
app's details. Both explain that installation and agent attachment are separate.
Descriptions are untrusted app data, not instructions or authorization.

Agents directly calling app tools need those apps attached to themselves.
Include coordination apps (such as Processes or A2A) as well as the domain apps
used by their work. Apps can also ensure attachments when dispatching execution;
that does not mean an idle executor already has them. Helper's scoped operator
`app_tool_search` / `app_tool_call` broker requires no permanent Helper attachment.

## Updating an existing agent through Apteva Server MCP

```json
{
  "id": 42,
  "bound_app_install_ids": [10, 11, 12],
  "mcp_action": "add"
}
```

Use installation IDs from `apps_list`. MCP registry IDs from `list_mcp_servers`
are a separate namespace and belong in `mcp_server_ids`. Both selectors can be
combined in one update. Resolution and MCP mutation run under the existing
per-agent config lock, with the normal project authorization, persistence,
live reconciliation, app binding synchronization and skill assignment.

- App-only updates default to `add`, preserving unrelated capabilities.
- Legacy MCP-ID updates continue to default to `set` for compatibility.
- `remove` detaches the selected capabilities.
- `set` explicitly replaces the full non-system MCP selection.
- Global installations are allowed; installations from other projects are not.
- Apps without a registered MCP surface return an explicit error.
- Attachment does not start a stopped agent or dispatch any work.

The underlying shared HTTP endpoint accepts the same selectors:

```http
POST /api/agents/42/mcp-servers
Content-Type: application/json
```

```json
{"action":"add","bound_app_install_ids":[10,11,12]}
```

Do not put attachment selectors inside `config`. Both the MCP update tool and
the HTTP config handler reject misplaced fields rather than silently saving
settings that cannot attach tools.

## Verification

Attachment updates verify that the selected capabilities are present (or removed)
in the canonical configuration before reporting success. Attachment receipts and
`agents_get` expose `capabilities` with actual MCP IDs,
names, sources, connection IDs, and attached app installation identities/status.
The snapshot reads live Core configuration for running agents and saved config
for stopped agents. Legacy app selections saved in generic database config are
not evidence of attachment. URLs, commands, headers and credentials are omitted.
App status describes the installation; attachment alone does not prove an app
is running or that a particular tool is usable.

Helper tool search indexes app display names and descriptions as well as tool
metadata. Permission checks, allowed-tool filtering and thread-bound references
still apply. An app-description match cannot expose private tools.
