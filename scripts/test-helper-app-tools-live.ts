/** Opt-in integration test against a LOCAL running server + real Helper LLM.
 * Creates two retained, read-only test conversations; never changes tickets,
 * app attachments, grants, or agent configuration. Requires a running Tickets
 * installation in an explicitly selected project. May incur provider usage.
 *
 * RUN_HELPER_LLM_TEST=1 TEST_PROJECT_ID=... TEST_TICKETS_INSTALL_ID=... 
 * TEST_CONVERSATIONS_INSTALL_ID=... bun run scripts/test-helper-app-tools-live.ts
 * Optional APTEVA_HOME, APTEVA_TEST_KEY_FILE, TEST_HELPER_ID, TEST_PORT.
 */
import { Database } from "bun:sqlite";
import { homedir } from "node:os";
import { join } from "node:path";
import assert from "node:assert/strict";

assert.equal(process.env.RUN_HELPER_LLM_TEST, "1", "Explicit opt-in required");
const project = process.env.TEST_PROJECT_ID;
const ticketsInstall = Number(process.env.TEST_TICKETS_INSTALL_ID);
const conversationsInstall = Number(process.env.TEST_CONVERSATIONS_INSTALL_ID);
const helper = Number(process.env.TEST_HELPER_ID || 1);
assert(project && ticketsInstall > 0 && conversationsInstall > 0);
const dataHome = process.env.APTEVA_HOME || join(homedir(), ".apteva");
const key = (await Bun.file(process.env.APTEVA_TEST_KEY_FILE || join(dataHome, "agent-bench-api-key")).text()).trim();
const base = `http://127.0.0.1:${Number(process.env.TEST_PORT || 5280)}/api`;
const db = new Database(join(dataHome, "apteva.db"), { readonly: true });
async function api(path: string, body?: unknown, method = body === undefined ? "GET" : "POST"): Promise<any> {
  const response = await fetch(base + path, {
    method,
    headers: { Authorization: `Bearer ${key}`, "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(20_000),
  });
  assert(response.ok, `${path.split("?")[0]} returned HTTP ${response.status}`);
  return response.json();
}
const scope = `project_id=${encodeURIComponent(project)}&install_id=${conversationsInstall}`;
const convAPI = (path: string, body?: unknown) => api(`/apps/conversations/${path}${path.includes("?") ? "&" : "?"}${scope}`, body);
const testThreads: string[] = [];
try {
const authoritative = await api(`/apps/tickets/tickets?project_id=${encodeURIComponent(project)}&install_id=${ticketsInstall}&limit=5`);
assert(Array.isArray(authoritative.tickets), "Expected Tickets list response");
const helperMCP = () => JSON.parse((db.query("SELECT config FROM agents WHERE id=?").get(helper) as {config:string}).config).mcp_servers;
const toolsBefore = helperMCP();
async function chat(title: string, content: string, pageContext: unknown) {
  const created = await convAPI("chats", { agent_id: helper, title });
  const id = created.id || created.chat?.id || created.conversation?.id;
  assert(typeof id === "string", "Conversation creation did not return an ID");
  testThreads.push(`chat-${id}`);
  console.log(JSON.stringify({ stage: "created", chat_id: id }));
  const sent = await convAPI("messages", { chat_id: id, client_message_id: crypto.randomUUID(), content, page_context: pageContext });
  assert.deepEqual(sent.metadata?.page_context, pageContext, "Message snapshot was not retained exactly");
  const deadline = Date.now() + 240_000;
  while (Date.now() < deadline) {
    const failure = db.query("SELECT data FROM telemetry WHERE agent_id=? AND thread_id=? AND type='llm.error' ORDER BY time DESC LIMIT 1").get(helper, `chat-${id}`) as {data:string} | null;
    if (failure && /usage_limit_reached|Insufficient .*balance/.test(failure.data)) {
      throw new Error(`LLM test blocked by configured provider quota/balance in ${id}; no provider settings were changed`);
    }
    const result = await convAPI(`messages?chat_id=${encodeURIComponent(id)}`);
    const messages = Array.isArray(result) ? result : result.messages || result.items;
    assert(Array.isArray(messages), "Expected message list");
    const responses = messages.filter((m: any) => m.role === "agent" && m.id > sent.id && (m.phase || m.metadata?.phase || "final") === "final" && !m.component_kind);
    if (responses.length) return { id, sent, response: responses.map((m: any) => m.content).join("\n") };
    await Bun.sleep(1500);
  }
  throw new Error(`Timed out waiting for a final LLM response in ${id}`);
}
const appContext = { version: 1, page: "app", project_id: project, app: "tickets", installation_id: ticketsInstall, panel: "Tickets" };
const appTest = await chat("Page context + app tools — read-only LLM test",
  "Which app am I viewing? List up to five tickets from this app in the current project. Read-only: do not create, update, attach, install, or connect anything. If there are no tickets, say so clearly. Use the available app discovery and call tools to verify the live result.", appContext);
// Telemetry is authoritative: a plausible answer alone is not a passing test.
await Bun.sleep(2000);
const events = db.query("SELECT type,data FROM telemetry WHERE agent_id=? AND thread_id=? ORDER BY time").all(helper, `chat-${appTest.id}`) as {type:string;data:string}[];
const calls = events.filter(e => e.type === "tool.call").map(e => JSON.parse(e.data).name as string);
const search = calls.findIndex(name => /app_tool_search$/.test(name));
const call = calls.findIndex(name => /app_tool_call$/.test(name));
assert(search >= 0 && call > search, `Missing search → call flow (observed: ${calls.join(", ")})`);
const audits = events.filter(e => e.type === "app_tool.call").map(e => JSON.parse(e.data));
assert(audits.some(e => e.app === "tickets" && e.install_id === ticketsInstall && e.tool === "tickets_list" && e.status === "success"), "No successful audited tickets_list call");
assert(audits.every(e => e.app === "tickets" && e.tool === "tickets_list"), "Unexpected app work in read-only test");
assert(/tickets/i.test(appTest.response), "Response did not identify current app");
if (authoritative.total === 0) assert(/no tickets|zero tickets|0 tickets|empty|no .*tickets/i.test(appTest.response), "Response did not report the authoritative empty result");
else for (const ticket of authoritative.tickets) assert(appTest.response.includes(String(ticket.title)), "Response omitted a returned ticket");
console.log(JSON.stringify({ test: "context-to-app-tools", passed: true, chat_id: appTest.id, tool_flow: calls, tickets_total: authoritative.total, response: appTest.response }));

const viewed = db.query("SELECT id,name FROM agents WHERE project_id=? AND id<>? ORDER BY id LIMIT 1").get(project, helper) as { id:number;name:string } | null;
assert(viewed, "Need a distinct project agent for the viewed-vs-recipient test");
const agentTest = await chat("Viewed agent vs Helper — read-only LLM test",
  "Without making changes or calling tools, name the agent I am viewing and its numeric ID from the page context. Also say whether you, Helper, are a different agent. Do not switch your identity to the viewed agent.",
  { version: 1, page: "agent", project_id: project, viewed_agent_id: viewed.id, viewed_agent_name: viewed.name, thread_id: "main", tab: "overview" });
assert(agentTest.response.includes(String(viewed.id)) && agentTest.response.includes(viewed.name), "Viewed agent snapshot missing from response");
assert(/helper/i.test(agentTest.response) && /different|separate|distinct|not the same/i.test(agentTest.response), "Recipient conflated with viewed agent");
assert.deepEqual(helperMCP(), toolsBefore, "Helper permanent MCP configuration changed");
console.log(JSON.stringify({ test: "viewed-agent-separate", passed: true, chat_id: agentTest.id, response: agentTest.response }));
} finally {
  // Stop only this run's test threads, including quota retry loops. Durable
  // Conversations messages and server telemetry remain available for review.
  for (const thread of testThreads) {
    try { await api(`/agents/${helper}/threads/${encodeURIComponent(thread)}`, undefined, "DELETE"); }
    catch { console.warn(`Could not stop test thread ${thread}`); }
  }
  db.close();
}
