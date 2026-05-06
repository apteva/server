// InboxStrip — compact unread-feed of recent emails. Same shape as
// EmailCard items[] but flattened into one-line rows with a snippet.
// Used in the dashboard tile and the demo runner's kiosk header.
import { Card, CardHeader, Avatar, Row, StatusPill } from "@apteva/ui-kit";
import { recordUrl, timeAgo, minusHoursISO, hubspotVendor } from "./lib/hubspot";
const previewItems = [
    { email_id: "1", from_name: "Sarah Chen", from_email: "sarah.chen@acme-logistics.com", subject: "Third time I'm asking — board review tomorrow", snippet: "We're 36 hours away from our board review.", sent_at: minusHoursISO(2), unread: true, thread_length: 3 },
    { email_id: "2", from_name: "David Park", from_email: "david.park@globex-innovations.com", subject: "Globex pilot — push to next quarter", snippet: "We've had to deprioritize new initiatives…", sent_at: minusHoursISO(28), unread: true },
    { email_id: "3", from_name: "Lisa Rodriguez", from_email: "lisa.rodriguez@initech-corp.com", subject: "Re: Pricing — final ask before committee", snippet: "Three different per-seat numbers from your team.", sent_at: minusHoursISO(6), unread: true, thread_length: 5 },
    { email_id: "4", from_name: "HubSpot", from_email: "no-reply@hubspot.com", subject: "Weekly digest: 3 new opportunities", snippet: "Your pipeline summary for this week.", sent_at: minusHoursISO(80) },
];
export default function InboxStrip(props) {
    const items = props.preview ? (props.items ?? previewItems) : (props.items ?? []);
    const max = props.max_rows ?? 6;
    const visible = items.slice(0, max);
    const overflow = items.length - visible.length;
    const unreadCount = items.filter((i) => i.unread).length;
    return (<Card fullWidth>
      <CardHeader vendor={hubspotVendor} title={props.title || "Inbox"} subtitle={props.subtitle || (unreadCount > 0 ? `${unreadCount} unread` : `${items.length} message${items.length === 1 ? "" : "s"}`)}/>
      {visible.length === 0 && (<div className="px-3 py-3 text-xs text-zinc-500">Inbox is quiet.</div>)}
      {visible.map((it, i) => (<Row key={it.email_id} flush={i === 0} href={recordUrl("engagement", it.email_id, props.portal_id)} leading={<span className="relative">
              <Avatar src="" name={it.from_name || it.from_email || "?"} size={20}/>
              {it.unread && (<span className="absolute -top-0.5 -right-0.5 w-2 h-2 rounded-full bg-blue-500 ring-1 ring-white dark:ring-zinc-900" aria-label="unread"/>)}
            </span>} title={<span className={it.unread ? "font-semibold" : undefined}>
              {it.from_name || it.from_email}
              {it.subject && <span className="text-zinc-500 font-normal"> · {it.subject}</span>}
            </span>} subtitle={it.snippet} trailing={<span className="inline-flex items-center gap-1.5">
              {it.thread_length && it.thread_length > 1 && (<StatusPill variant="info">{it.thread_length}</StatusPill>)}
              <span className="text-zinc-500 tabular-nums">{timeAgo(it.sent_at)}</span>
            </span>}/>))}
      {overflow > 0 && (<div className="px-3 py-1.5 text-[11px] text-zinc-500 border-t border-zinc-200 dark:border-zinc-800">
          +{overflow} more
        </div>)}
    </Card>);
}
//# sourceMappingURL=InboxStrip.js.map