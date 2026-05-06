// DatabaseCard — overview of a Notion database. Useful when the
// agent is referencing or about to query a database the operator
// hasn't seen before.
//
// Surfaces:
//   - icon + title + breadcrumb
//   - optional description block
//   - schema as small typed pills (Status: select · Owner: person · …)
//   - item count + last-edited byline
import { Avatar, Card, CardHeader, DataList } from "@apteva/ui-kit";
import { databaseUrl, notionVendor, PageIcon, parseSchema, propTone, timeAgo, } from "./lib/notion";
const previewSample = {
    database_id: "f1e2d3c4-b5a6-7890-1234-567890abcdef",
    title: "Q4 Sprint Tasks",
    icon: "🗂",
    description: "Tasks tracked for the Q4 engineering sprint.",
    parent_path: "Apteva › Engineering",
    workspace: "apteva",
    item_count: 38,
    schema: "Title:title, Status:status, Owner:person, Due:date, Sprint:select, Priority:select",
    views: "Board · Calendar · Table · By owner",
    last_edited_at: new Date(Date.now() - 12 * 60 * 1000).toISOString(),
    last_edited_by: "marc-olivier",
    last_edited_by_avatar: "",
};
export default function DatabaseCard(props) {
    const p = props.preview
        ? previewSample
        : {
            database_id: props.database_id,
            title: props.title || "Untitled database",
            icon: props.icon ?? "",
            description: props.description || "",
            parent_path: props.parent_path || "",
            workspace: props.workspace || "",
            item_count: props.item_count ?? 0,
            schema: props.schema || "",
            views: props.views || "",
            last_edited_at: props.last_edited_at || new Date().toISOString(),
            last_edited_by: props.last_edited_by || "",
            last_edited_by_avatar: props.last_edited_by_avatar || "",
        };
    const url = props.url || databaseUrl(p.database_id, p.workspace);
    const schemaProps = parseSchema(p.schema);
    return (<Card>
      <CardHeader vendor={notionVendor} title={<span className="inline-flex items-center gap-2">
            <PageIcon icon={p.icon} fallback={p.title} size={16}/>
            <span className="truncate">{p.title}</span>
          </span>} subtitle={p.parent_path || undefined} action={{ label: "Open database", href: url }}/>

      <div className="px-4 py-3 flex flex-col gap-3">
        {p.description && (<p className="text-sm text-zinc-700 dark:text-zinc-300 leading-relaxed line-clamp-2">
            {p.description}
          </p>)}

        <DataList items={[
            {
                label: "Items",
                value: (<span className="tabular-nums">{p.item_count.toLocaleString()}</span>),
            },
            ...(schemaProps.length > 0
                ? [{
                        label: "Schema",
                        value: (<span className="inline-flex flex-wrap gap-1">
                      {schemaProps.map((s) => (<span key={s.name} className={`text-[11px] font-medium px-1.5 py-0.5 rounded-md ${propTone(s.type)}`}>
                          {s.name}
                          <span className="opacity-60 ml-1">{s.type}</span>
                        </span>))}
                    </span>),
                    }]
                : []),
            ...(p.views
                ? [{ label: "Views", value: <span className="text-sm text-zinc-700 dark:text-zinc-300">{p.views}</span> }]
                : []),
        ]}/>

        {/* last-edited byline */}
        <div className="flex items-center gap-2 text-xs pt-1">
          {p.last_edited_by && (<Avatar src={p.last_edited_by_avatar} name={p.last_edited_by} size={18}/>)}
          {p.last_edited_by && (<span className="text-zinc-900 dark:text-zinc-100 font-medium">{p.last_edited_by}</span>)}
          <span className="text-zinc-500">edited {timeAgo(p.last_edited_at)}</span>
        </div>
      </div>
    </Card>);
}
//# sourceMappingURL=DatabaseCard.js.map