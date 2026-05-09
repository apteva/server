// TicketList — dense rows of tickets. Same shape as TicketCard.
// Footer summarises open count and HIGH/URGENT count.
import { Card, CardHeader, StatusPill, Row } from "@apteva/ui-kit";
import { ticketPriorityMeta, ticketStageMeta, recordUrl, timeAgo, minusHoursISO, hubspotVendor } from "./lib/hubspot";
const previewItems = [
    { ticket_id: "1", subject: "Slow API response times — affecting overnight runs", hs_ticket_priority: "HIGH", hs_pipeline_stage: "2", createdate: minusHoursISO(240), company_name: "Acme Logistics" },
    { ticket_id: "2", subject: "Quote clarification on enterprise license terms", hs_ticket_priority: "MEDIUM", hs_pipeline_stage: "1", createdate: minusHoursISO(36), company_name: "Initech Corp" },
    { ticket_id: "3", subject: "SSO integration broken after Okta migration", hs_ticket_priority: "URGENT", hs_pipeline_stage: "2", createdate: minusHoursISO(6), company_name: "Hooli" },
    { ticket_id: "4", subject: "Bulk export hits row limit on weekly run", hs_ticket_priority: "LOW", hs_pipeline_stage: "3", createdate: minusHoursISO(120), company_name: "Soylent Foods" },
];
export default function TicketList(props) {
    const items = props.preview ? (props.items ?? previewItems) : (props.items ?? []);
    const max = props.max_rows ?? 6;
    const visible = items.slice(0, max);
    const overflow = items.length - visible.length;
    const urgent = items.filter((t) => t.hs_ticket_priority === "HIGH" || t.hs_ticket_priority === "URGENT").length;
    return (<Card fullWidth>
 <CardHeader vendor={hubspotVendor} title={props.title || "Tickets"} subtitle={props.subtitle || (items.length > 0 ? `${items.length} open${urgent > 0 ? ` · ${urgent} HIGH+` : ""}` : "Nothing open")}/>
 {visible.length === 0 && (<div className="px-3 py-3 text-xs text-text-dim">No tickets match.</div>)}
 {visible.map((t, i) => {
            const priority = ticketPriorityMeta(t.hs_ticket_priority);
            const stage = ticketStageMeta(t.hs_pipeline_stage, t.stage_label);
            return (<Row key={t.ticket_id} flush={i === 0} href={recordUrl("ticket", t.ticket_id, props.portal_id)} leading={<StatusPill variant={priority.variant}>{priority.label}</StatusPill>} title={t.subject || `Ticket ${t.ticket_id}`} subtitle={t.company_name} trailing={<span className="inline-flex items-center gap-2">
 <StatusPill variant={stage.variant}>{stage.label}</StatusPill>
 {t.createdate && (<span className="text-text-dim hidden sm:inline">{timeAgo(t.createdate)}</span>)}
 </span>}/>);
        })}
 {overflow > 0 && (<div className="px-3 py-1.5 text-[11px] text-text-dim border-t border-border">
 +{overflow} more
 </div>)}
 </Card>);
}
//# sourceMappingURL=TicketList.js.map