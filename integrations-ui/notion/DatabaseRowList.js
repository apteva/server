// DatabaseRowList — rows from a Notion database query.
//
// The most operationally useful Notion card. Agents that fetch
// "all open tasks" or "this week's design reviews" hand the
// resulting page list here; each row gets a dense one-line render:
//
//   [icon] Title                              status · owner · due
//
// Status / owner / due are pulled from common Notion property
// shapes; the agent passes them as flat fields per row so the
// component doesn't have to know about Notion's typed-property API.
import { Avatar, Card, CardHeader, Row, StatusPill } from "@apteva/ui-kit";
import { databaseUrl, notionVendor, PageIcon, timeAgo, } from "./lib/notion";
const previewRows = [
    { page_id: "p1", title: "Fix retry loop on 5xx", icon: "🐛", status: "In progress", status_color: "blue", owner: "ari", due: "May 11", last_edited_at: new Date(Date.now() - 30 * 60 * 1000).toISOString() },
    { page_id: "p2", title: "Add idempotency keys", icon: "✨", status: "In progress", status_color: "blue", owner: "maya", due: "May 13", last_edited_at: new Date(Date.now() - 1 * 60 * 60 * 1000).toISOString() },
    { page_id: "p3", title: "Quarterly metrics dashboard", icon: "📊", status: "Todo", status_color: "gray", owner: "lin", due: "May 18", last_edited_at: new Date(Date.now() - 4 * 60 * 60 * 1000).toISOString() },
    { page_id: "p4", title: "Migrate retry queue to bullmq", icon: "⚙️", status: "In review", status_color: "yellow", owner: "ari", due: "May 14", last_edited_at: new Date(Date.now() - 5 * 60 * 60 * 1000).toISOString() },
    { page_id: "p5", title: "Engineering offsite agenda", icon: "📝", status: "Todo", status_color: "gray", owner: "maya", due: "May 20", last_edited_at: new Date(Date.now() - 6 * 60 * 60 * 1000).toISOString() },
];
export default function DatabaseRowList(props) {
    const rows = props.preview
        ? previewRows
        : (parseRows(props.rows) ?? []);
    const max = props.max ?? 10;
    const visible = rows.slice(0, max);
    const overflow = rows.length - visible.length;
    const dbTitle = props.database_title ?? (props.preview ? "Q4 Sprint Tasks" : "Database");
    const dbIcon = props.database_icon ?? (props.preview ? "🗂" : undefined);
    const dbId = props.database_id || (props.preview ? "f1e2d3c4-b5a6-7890-1234-567890abcdef" : "");
    const url = props.url || (dbId ? databaseUrl(dbId, props.workspace) : "");
    const subtitle = rows.length === 0
        ? props.view_label || "No rows"
        : `${rows.length} item${rows.length === 1 ? "" : "s"}` +
            (props.view_label ? ` in view: ${props.view_label}` : "");
    return (<Card fullWidth>
      <CardHeader vendor={notionVendor} title={<span className="inline-flex items-center gap-2">
            <PageIcon icon={dbIcon} fallback={dbTitle} size={16}/>
            <span className="truncate">{dbTitle}</span>
          </span>} subtitle={subtitle} action={{ label: "Open database", href: url }}/>

      <div className="flex flex-col">
        {visible.map((r, i) => (<Row key={r.page_id} flush={i === 0} leading={<PageIcon icon={r.icon} fallback={r.title} size={18}/>} title={r.title} subtitle={r.excerpt} trailing={<span className="inline-flex items-center gap-2">
                {r.status && (<StatusPill variant={statusPillVariant(r.status_color)}>
                    {r.status}
                  </StatusPill>)}
                {r.owner && (<span className="inline-flex items-center gap-1 text-zinc-500">
                    <Avatar src={r.owner_avatar} name={r.owner} size={16}/>
                    <span className="hidden sm:inline">{r.owner}</span>
                  </span>)}
                {r.due && (<span className="text-zinc-500 tabular-nums">{r.due}</span>)}
                {!r.due && r.last_edited_at && (<span className="text-zinc-500 tabular-nums">{timeAgo(r.last_edited_at)}</span>)}
              </span>}/>))}
        {overflow > 0 && (<div className="px-4 py-2 text-xs text-zinc-500 border-t border-zinc-200 dark:border-zinc-800">
            +{overflow} more
          </div>)}
      </div>
    </Card>);
}
// Map Notion's option color → ui-kit StatusPill variant. Notion has
// 9 colors; the ui-kit pill has 5 variants. We collapse the
// long-tail to "neutral" so agents that pass weird colors don't
// crash; status/blue/green variants get their natural mapping.
function statusPillVariant(color) {
    switch (color) {
        case "green": return "success";
        case "blue": return "info";
        case "yellow":
        case "orange": return "warn";
        case "red": return "error";
        default: return "neutral";
    }
}
function parseRows(raw) {
    if (!raw)
        return null;
    if (Array.isArray(raw))
        return raw;
    try {
        const parsed = JSON.parse(raw);
        return Array.isArray(parsed) ? parsed : null;
    }
    catch {
        return null;
    }
}
//# sourceMappingURL=DatabaseRowList.js.map