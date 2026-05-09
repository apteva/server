// ContactCard — single HubSpot contact. Avatar (Gravatar from email
// or monogram fallback) + identity, lifecycle pill, last-engagement
// hint. The card the email-monitor demo wants when the agent is
// reasoning about a person rather than a deal.
import { Card, CardHeader, StatusPill, Avatar, DataList } from "@apteva/ui-kit";
import { lifecycleMeta, recordUrl, pillToDot, timeAgo, minusHoursISO, hubspotVendor } from "./lib/hubspot";
const previewSample = {
    contact_id: "501",
    firstname: "Sarah",
    lastname: "Chen",
    email: "sarah.chen@acme-logistics.com",
    phone: "+1 312 555 0142",
    jobtitle: "VP Operations",
    lifecyclestage: "customer",
    company_name: "Acme Logistics",
    company_domain: "acme-logistics.com",
    last_engagement_at: minusHoursISO(72),
    last_engagement_kind: "email",
    portal_id: "0",
};
function gravatarFor(email) {
    if (!email)
        return undefined;
    // Use Gravatar identicon fallback — no MD5 needed if we accept the
    // Google s2 favicon trick won't work for emails. Skip rather than
    // ship a hash impl in v1; Avatar component will monogram.
    return undefined;
}
export default function ContactCard(props) {
    const p = props.preview ? { ...previewSample, ...props } : props;
    const lifecycle = lifecycleMeta(p.lifecyclestage);
    const url = recordUrl("contact", p.contact_id, p.portal_id);
    const fullName = [p.firstname, p.lastname].filter(Boolean).join(" ") || p.email || `Contact ${p.contact_id}`;
    return (<Card>
 <CardHeader vendor={hubspotVendor} title={fullName} subtitle={p.jobtitle || p.email} status={{ label: lifecycle.label, variant: pillToDot(lifecycle.variant) }} action={{ label: "View in HubSpot", href: url }}/>
 <div className="px-3 py-3 flex flex-col gap-3">
 <div className="flex items-center gap-3">
 <Avatar src={gravatarFor(p.email)} name={fullName} size={32}/>
 <div className="min-w-0 flex-1">
 <div className="text-text font-medium truncate">{fullName}</div>
 {p.jobtitle && p.company_name && (<div className="text-text-dim text-xs truncate">
 {p.jobtitle} · {p.company_name}
 </div>)}
 </div>
 </div>

 <DataList items={[
            ...(p.email ? [{ label: "Email", value: <a href={`mailto:${p.email}`} className="text-accent hover:underline">{p.email}</a> }] : []),
            ...(p.phone ? [{ label: "Phone", value: <span className="tabular-nums">{p.phone}</span> }] : []),
            { label: "Lifecycle", value: <StatusPill variant={lifecycle.variant}>{lifecycle.label}</StatusPill> },
            ...(p.last_engagement_at ? [{
                    label: "Last contact",
                    value: (<span>
 <span className="text-text">{p.last_engagement_kind || "engagement"}</span>
 <span className="text-text-dim"> · {timeAgo(p.last_engagement_at)}</span>
 </span>),
                }] : []),
        ]}/>
 </div>
 </Card>);
}
//# sourceMappingURL=ContactCard.js.map