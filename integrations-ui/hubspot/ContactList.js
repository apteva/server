// ContactList — dense rows of contacts. Avatar leading, name + title,
// company chip + last engagement on the right.
import { Card, CardHeader, Avatar, Row } from "@apteva/ui-kit";
import { recordUrl, timeAgo, minusHoursISO, hubspotVendor } from "./lib/hubspot";
const previewItems = [
    { contact_id: "1", firstname: "Sarah", lastname: "Chen", email: "sarah.chen@acme-logistics.com", jobtitle: "VP Operations", company_name: "Acme Logistics", last_engagement_at: minusHoursISO(2) },
    { contact_id: "2", firstname: "David", lastname: "Park", email: "david.park@globex-innovations.com", jobtitle: "CTO", company_name: "Globex Innovations", last_engagement_at: minusHoursISO(336) },
    { contact_id: "3", firstname: "Lisa", lastname: "Rodriguez", email: "lisa.rodriguez@initech-corp.com", jobtitle: "CFO", company_name: "Initech Corp", last_engagement_at: minusHoursISO(36) },
    { contact_id: "4", firstname: "Marcus", lastname: "Hayes", email: "marcus.hayes@acme-logistics.com", jobtitle: "IT Director", company_name: "Acme Logistics", last_engagement_at: minusHoursISO(168) },
];
function nameOf(c) {
    return [c.firstname, c.lastname].filter(Boolean).join(" ") || c.email || `Contact ${c.contact_id}`;
}
export default function ContactList(props) {
    const items = props.preview ? (props.items ?? previewItems) : (props.items ?? []);
    const max = props.max_rows ?? 6;
    const visible = items.slice(0, max);
    const overflow = items.length - visible.length;
    return (<Card fullWidth>
      <CardHeader vendor={hubspotVendor} title={props.title || "Contacts"} subtitle={props.subtitle || (items.length > 0 ? `${items.length} contact${items.length === 1 ? "" : "s"}` : "No contacts")}/>
      {visible.length === 0 && (<div className="px-3 py-3 text-xs text-zinc-500">No contacts match.</div>)}
      {visible.map((c, i) => {
            const name = nameOf(c);
            return (<Row key={c.contact_id} flush={i === 0} href={recordUrl("contact", c.contact_id, props.portal_id)} leading={<Avatar src="" name={name} size={20}/>} title={name} subtitle={[c.jobtitle, c.company_name].filter(Boolean).join(" · ")} trailing={c.last_engagement_at && (<span className="text-zinc-500 tabular-nums">{timeAgo(c.last_engagement_at)}</span>)}/>);
        })}
      {overflow > 0 && (<div className="px-3 py-1.5 text-[11px] text-zinc-500 border-t border-zinc-200 dark:border-zinc-800">
          +{overflow} more
        </div>)}
    </Card>);
}
//# sourceMappingURL=ContactList.js.map