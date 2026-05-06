// PageCard — single Notion page. Title + icon up top, optional
// breadcrumb above the title, optional excerpt below, properties as
// a compact DataList when the page is a database row.
//
// Notion's "page" is the universal unit — a free-form doc, a
// database row, a sub-page. Same card renders all three; the
// presence of `properties` distinguishes a row from a free doc.
import { Avatar, Card, CardHeader, DataList } from "@apteva/ui-kit";
import { notionVendor, pageUrl, PageIcon, timeAgo, } from "./lib/notion";
const previewSample = {
    page_id: "abc-123-def",
    title: "Q4 engineering roadmap",
    icon: "📋",
    parent_path: "Apteva › Engineering",
    workspace: "apteva",
    archived: false,
    last_edited_at: new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString(),
    last_edited_by: "marc-olivier",
    last_edited_by_avatar: "",
    excerpt: "We need to deliver three things in Q4: reliability work on the " +
        "retry path, a 30% reduction in agent latency, and the v2 API " +
        "gateway behind a feature flag.",
    properties: "Status=In progress, Owner=marc-olivier, Due=May 11, Sprint=Q4-S2",
};
export default function PageCard(props) {
    const p = props.preview
        ? previewSample
        : {
            page_id: props.page_id,
            title: props.title || "Untitled",
            icon: props.icon ?? "",
            parent_path: props.parent_path || "",
            workspace: props.workspace || "",
            archived: !!props.archived,
            last_edited_at: props.last_edited_at || new Date().toISOString(),
            last_edited_by: props.last_edited_by || "",
            last_edited_by_avatar: props.last_edited_by_avatar || "",
            excerpt: props.excerpt || "",
            properties: props.properties || "",
        };
    const url = props.url || pageUrl(p.page_id, p.workspace);
    const props_kv = parseProperties(p.properties);
    return (<Card>
      <CardHeader vendor={notionVendor} title={<span className="inline-flex items-center gap-2">
            <PageIcon icon={p.icon} fallback={p.title} size={16}/>
            <span className="truncate">{p.title}</span>
          </span>} subtitle={p.parent_path || undefined} status={p.archived ? { label: "archived", variant: "muted" } : undefined} action={{ label: "Open in Notion", href: url }}/>

      <div className="px-4 py-3 flex flex-col gap-3">
        {/* last-edited byline — always shown */}
        <div className="flex items-center gap-2 text-xs">
          {p.last_edited_by && (<Avatar src={p.last_edited_by_avatar} name={p.last_edited_by} size={18}/>)}
          {p.last_edited_by && (<span className="text-zinc-900 dark:text-zinc-100 font-medium">{p.last_edited_by}</span>)}
          <span className="text-zinc-500">edited {timeAgo(p.last_edited_at)}</span>
        </div>

        {/* properties — only when it's a database row */}
        {props_kv.length > 0 && (<DataList items={props_kv}/>)}

        {/* excerpt — first block of page content if available */}
        {p.excerpt && (<p className="text-sm text-zinc-700 dark:text-zinc-300 whitespace-pre-wrap break-words leading-relaxed line-clamp-4">
            {p.excerpt}
          </p>)}
      </div>
    </Card>);
}
// "Status=In progress, Owner=ari" → DataList items
function parseProperties(raw) {
    if (!raw)
        return [];
    return raw
        .split(",")
        .map((entry) => entry.trim())
        .filter(Boolean)
        .map((entry) => {
        const eq = entry.indexOf("=");
        if (eq === -1)
            return { label: entry, value: "" };
        return { label: entry.slice(0, eq).trim(), value: entry.slice(eq + 1).trim() };
    });
}
//# sourceMappingURL=PageCard.js.map