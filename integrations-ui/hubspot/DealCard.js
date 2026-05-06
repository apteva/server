// DealCard — chat-attachment / dashboard-tile card for a single
// HubSpot deal.
//
// The agent calls
//   respond(components=[{app:"hubspot", name:"deal-card", props:{...}}])
// after a search_crm_objects (objectType=deals) or get-by-id, and
// passes the deal's HubSpot properties through almost verbatim. Field
// names mirror the HubSpot wire format (dealname, amount, dealstage,
// closedate, …) so the agent rarely needs to rename anything.
import { Card, CardHeader, StatusPill, Avatar, DataList } from "@apteva/ui-kit";
import { dealStageMeta, formatUSD, formatRelativeDate, recordUrl, faviconFor, pillToDot, addDaysISO, hubspotVendor, } from "./lib/hubspot";
const previewSample = {
    deal_id: "9876543210",
    dealname: "Acme Q4 Renewal",
    amount: "48000",
    dealstage: "contractsent",
    pipeline: "default",
    closedate: addDaysISO(7),
    owner_email: "marc-olivier@apteva.local",
    company_name: "Acme Logistics",
    company_domain: "acme-logistics.com",
    portal_id: "0",
};
export default function DealCard(props) {
    const p = props.preview ? { ...previewSample, ...props } : props;
    const stage = dealStageMeta(p.dealstage, p.dealstage_label);
    const url = recordUrl("deal", p.deal_id, p.portal_id);
    return (<Card>
      <CardHeader vendor={hubspotVendor} title={p.company_name || "HubSpot deal"} subtitle={p.dealname || `Deal ${p.deal_id}`} status={{ label: stage.label, variant: pillToDot(stage.variant) }} action={{ label: "View in HubSpot", href: url }}/>
      <div className="px-3 py-3 flex flex-col gap-3">
        <div className="flex items-baseline justify-between gap-2">
          <span className="text-zinc-900 dark:text-zinc-100 text-lg font-semibold tabular-nums">
            {formatUSD(p.amount)}
          </span>
          <StatusPill variant={stage.variant}>{stage.label}</StatusPill>
        </div>

        <DataList items={[
            {
                label: "Close date",
                value: <span className="tabular-nums">{formatRelativeDate(p.closedate)}</span>,
            },
            ...(p.pipeline ? [{ label: "Pipeline", value: p.pipeline }] : []),
            ...(p.company_name ? [{
                    label: "Company",
                    value: (<span className="inline-flex items-center gap-1.5">
                  {p.company_domain && (<img src={faviconFor(p.company_domain)} alt="" width={12} height={12} className="rounded-sm"/>)}
                  <span className="text-zinc-900 dark:text-zinc-100">{p.company_name}</span>
                  {p.company_domain && <span className="text-zinc-500">· {p.company_domain}</span>}
                </span>),
                }] : []),
            ...(p.owner_email ? [{
                    label: "Owner",
                    value: (<span className="inline-flex items-center gap-1.5">
                  <Avatar src="" name={p.owner_email} size={14}/>
                  <span className="text-zinc-900 dark:text-zinc-100">{p.owner_email}</span>
                </span>),
                }] : []),
        ]}/>
      </div>
    </Card>);
}
//# sourceMappingURL=DealCard.js.map