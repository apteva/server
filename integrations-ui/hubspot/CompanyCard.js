// CompanyCard — single HubSpot company. Domain favicon as the leading
// visual, lifecycle stage as the right-side pill, key facts in the
// data list. Designed to slot into chat after a get-by-id and into
// the dashboard's"key accounts" tile.
import { Card, CardHeader, StatusPill, DataList } from "@apteva/ui-kit";
import { lifecycleMeta, recordUrl, faviconFor, pillToDot, formatUSD, hubspotVendor } from "./lib/hubspot";
const previewSample = {
    company_id: "1234567890",
    name: "Acme Logistics",
    domain: "acme-logistics.com",
    industry: "Transportation & Logistics",
    numberofemployees: "250",
    city: "Chicago",
    country: "United States",
    lifecyclestage: "customer",
    open_deal_total: "48000",
    open_deal_count: 1,
    description: "Mid-market freight + last-mile logistics provider.",
    portal_id: "0",
};
function prettyIndustry(raw) {
    if (!raw)
        return undefined;
    // HubSpot industries are SHOUTY_SNAKE_CASE — humanize for display.
    return raw
        .split("_")
        .map((s) => s.length > 0 ? s[0].toUpperCase() + s.slice(1).toLowerCase() : s)
        .join(" ")
        .replace(/_/g, "");
}
export default function CompanyCard(props) {
    const p = props.preview ? { ...previewSample, ...props } : props;
    const lifecycle = lifecycleMeta(p.lifecyclestage);
    const url = recordUrl("company", p.company_id, p.portal_id);
    const favicon = faviconFor(p.domain, 32);
    return (<Card>
 <CardHeader vendor={hubspotVendor} title={p.name || `Company ${p.company_id}`} subtitle={p.domain} status={{ label: lifecycle.label, variant: pillToDot(lifecycle.variant) }} action={{ label: "View in HubSpot", href: url }}/>
 <div className="px-3 py-3 flex flex-col gap-3">
 <div className="flex items-center gap-3">
 {favicon && (<img src={favicon} alt="" width={32} height={32} className="rounded-md bg-bg-hover flex-shrink-0"/>)}
 <div className="min-w-0 flex-1">
 <div className="text-text font-medium truncate">{p.name}</div>
 {p.description && (<div className="text-text-dim text-xs line-clamp-2">{p.description}</div>)}
 </div>
 </div>

 <DataList items={[
            { label: "Lifecycle", value: <StatusPill variant={lifecycle.variant}>{lifecycle.label}</StatusPill> },
            ...(p.industry ? [{ label: "Industry", value: prettyIndustry(p.industry) }] : []),
            ...(p.numberofemployees ? [{ label: "Employees", value: <span className="tabular-nums">{Number(p.numberofemployees).toLocaleString("en-US")}</span> }] : []),
            ...(p.city || p.country ? [{ label: "Location", value: [p.city, p.country].filter(Boolean).join(",") }] : []),
            ...(p.open_deal_count !== undefined && p.open_deal_count > 0 ? [{
                    label: "Open deals",
                    value: (<span className="inline-flex items-center gap-1.5 tabular-nums">
 <span className="text-text font-medium">{p.open_deal_count}</span>
 {p.open_deal_total !== undefined && (<span className="text-text-dim">· {formatUSD(p.open_deal_total)}</span>)}
 </span>),
                }] : []),
        ]}/>
 </div>
 </Card>);
}
//# sourceMappingURL=CompanyCard.js.map