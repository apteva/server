// TicketCard — single HubSpot ticket. Subject + content body up top,
// priority pill (LOW/MED/HIGH/URGENT — color-coded), pipeline-stage
// pill, age, associated company. The card the demo uses for Acme's
// HIGH-priority API-latency ticket.
import { MessageCircle } from "lucide-react";
import { Card, CardHeader, StatusPill, DataList } from "@apteva/ui-kit";
import { ticketPriorityMeta, ticketStageMeta, recordUrl, faviconFor, pillToDot, timeAgo, minusHoursISO, hubspotVendor, } from "./lib/hubspot";
const previewSample = {
    ticket_id: "20100",
    subject: "Slow API response times — affecting overnight runs",
    content: "Acme reports API p95 latency above 4s for 10 days. Two follow-ups, no resolution.",
    hs_ticket_priority: "HIGH",
    hs_pipeline_stage: "2",
    createdate: minusHoursISO(240), // ~10 days
    hs_lastmodifieddate: minusHoursISO(36),
    company_name: "Acme Logistics",
    company_domain: "acme-logistics.com",
    comment_count: 4,
    portal_id: "0",
};
export default function TicketCard(props) {
    const p = props.preview ? { ...previewSample, ...props } : props;
    const priority = ticketPriorityMeta(p.hs_ticket_priority);
    const stage = ticketStageMeta(p.hs_pipeline_stage, p.stage_label);
    const url = recordUrl("ticket", p.ticket_id, p.portal_id);
    return (<Card>
 <CardHeader vendor={hubspotVendor} title={p.subject || `Ticket ${p.ticket_id}`} subtitle={p.company_name} status={{ label: priority.label, variant: pillToDot(priority.variant) }} action={{ label: "View in HubSpot", href: url }}/>
 <div className="px-3 py-3 flex flex-col gap-3">
 <div className="flex items-center gap-2 flex-wrap">
 <StatusPill variant={priority.variant}>{priority.label} priority</StatusPill>
 <StatusPill variant={stage.variant}>{stage.label}</StatusPill>
 {p.comment_count !== undefined && p.comment_count > 0 && (<span className="text-[11px] text-text-dim inline-flex items-center gap-1"><MessageCircle className="w-3 h-3"/>{p.comment_count}</span>)}
 </div>

 {p.content && (<div className="text-xs text-text-muted line-clamp-3 border-l-2 border-border pl-2">
 {p.content}
 </div>)}

 <DataList items={[
            ...(p.company_name ? [{
                    label: "Company",
                    value: (<span className="inline-flex items-center gap-1.5">
 {p.company_domain && (<img src={faviconFor(p.company_domain)} alt="" width={12} height={12} className="rounded-sm"/>)}
 <span className="text-text">{p.company_name}</span>
 </span>),
                }] : []),
            ...(p.createdate ? [{ label: "Opened", value: timeAgo(p.createdate) }] : []),
            ...(p.hs_lastmodifieddate ? [{ label: "Updated", value: timeAgo(p.hs_lastmodifieddate) }] : []),
        ]}/>
 </div>
 </Card>);
}
//# sourceMappingURL=TicketCard.js.map