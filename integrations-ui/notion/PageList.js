// PageList — search results / recent pages, simpler than
// DatabaseRowList because pages aren't typed. Each row is just
// title + parent breadcrumb + last-edited byline.
import { Card, CardHeader, Row } from "@apteva/ui-kit";
import { notionVendor, pageUrl, PageIcon, timeAgo } from "./lib/notion";
const previewPages = [
    { page_id: "abc1", title: "Q4 engineering roadmap", icon: "📋", parent_path: "Apteva › Engineering", last_edited_at: new Date(Date.now() - 12 * 60 * 1000).toISOString(), last_edited_by: "marc-olivier" },
    { page_id: "abc2", title: "Bug triage — Sprint 23", icon: "🐛", parent_path: "Apteva › Engineering", last_edited_at: new Date(Date.now() - 1 * 60 * 60 * 1000).toISOString(), last_edited_by: "ari" },
    { page_id: "abc3", title: "Quarterly metrics dashboard", icon: "📊", parent_path: "Apteva › Analytics", last_edited_at: new Date(Date.now() - 3 * 60 * 60 * 1000).toISOString(), last_edited_by: "lin" },
    { page_id: "abc4", title: "Feature kickoff — idempotency", icon: "✨", parent_path: "Apteva › Engineering › Specs", last_edited_at: new Date(Date.now() - 24 * 60 * 60 * 1000).toISOString(), last_edited_by: "maya" },
    { page_id: "abc5", title: "Engineering offsite agenda", icon: "📝", parent_path: "Apteva › Team", last_edited_at: new Date(Date.now() - 48 * 60 * 60 * 1000).toISOString(), last_edited_by: "ari" },
];
export default function PageList(props) {
    const pages = props.preview
        ? previewPages
        : (parseList(props.pages) ?? []);
    const max = props.max ?? 10;
    const visible = pages.slice(0, max);
    const overflow = pages.length - visible.length;
    const label = props.label ?? (props.preview ? "Recent pages" : "Pages");
    const subtitle = pages.length === 0
        ? "No pages"
        : `${pages.length} page${pages.length === 1 ? "" : "s"}`;
    return (<Card fullWidth>
      <CardHeader vendor={notionVendor} title={label} subtitle={subtitle} action={props.url ? { label: "Open in Notion", href: props.url } : undefined}/>

      <div className="flex flex-col">
        {visible.map((page, i) => (<Row key={page.page_id} flush={i === 0} href={pageUrl(page.page_id, props.workspace)} leading={<PageIcon icon={page.icon} fallback={page.title} size={18}/>} title={page.title} subtitle={page.parent_path} trailing={<span className="inline-flex items-center gap-1.5 text-zinc-500">
                {page.last_edited_by && <span>{page.last_edited_by}</span>}
                {page.last_edited_at && (<span className="tabular-nums">· {timeAgo(page.last_edited_at)}</span>)}
              </span>}/>))}
        {overflow > 0 && (<div className="px-4 py-2 text-xs text-zinc-500 border-t border-zinc-200 dark:border-zinc-800">
            +{overflow} more
          </div>)}
      </div>
    </Card>);
}
function parseList(raw) {
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
//# sourceMappingURL=PageList.js.map