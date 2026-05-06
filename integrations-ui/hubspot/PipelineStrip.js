// PipelineStrip — at-a-glance funnel state. Compact horizontal strip
// of deal-pipeline stages, each tile rendering count + total $ via
// the shared KPI primitive. Designed for the dashboard tile slot and
// the demo runner's kiosk-mode header.
//
// Caller pre-aggregates the data — we don't fetch here. A typical
// agent computation:
//
//   stages = [
//     { id: "qualifiedtobuy",        count: 4, total: "210000" },
//     { id: "presentationscheduled", count: 3, total: "165000" },
//     { id: "decisionmakerboughtin", count: 2, total: "180000" },
//     { id: "contractsent",          count: 2, total: "98000" },
//   ]
import { Card, CardHeader, KPI } from "@apteva/ui-kit";
import { dealStageMeta, formatUSD, pipelineUrl, hubspotVendor } from "./lib/hubspot";
const previewStages = [
    { id: "qualifiedtobuy", count: 4, total: "210000" },
    { id: "presentationscheduled", count: 3, total: "165000" },
    { id: "decisionmakerboughtin", count: 2, total: "180000" },
    { id: "contractsent", count: 2, total: "98000" },
    { id: "closedwon", count: 5, total: "412000" },
];
export default function PipelineStrip(props) {
    const p = props.preview ? { pipeline_label: "Sales pipeline", pipeline: "default", stages: previewStages, portal_id: "0", ...props } : props;
    const stages = p.stages ?? [];
    const url = pipelineUrl(p.portal_id, p.pipeline);
    // Aggregate total across non-closed stages — the headline number.
    const openTotal = stages
        .filter((s) => s.id !== "closedwon" && s.id !== "closedlost")
        .reduce((acc, s) => acc + (Number(s.total ?? 0) || 0), 0);
    const openCount = stages
        .filter((s) => s.id !== "closedwon" && s.id !== "closedlost")
        .reduce((acc, s) => acc + (s.count || 0), 0);
    return (<Card fullWidth>
      <CardHeader vendor={hubspotVendor} title={p.pipeline_label || "Pipeline"} subtitle={openCount > 0 ? `${openCount} open · ${formatUSD(openTotal)} weighted` : "No open deals"} action={{ label: "Open pipeline", href: url }}/>
      <div className="px-3 py-3 overflow-x-auto">
        <div className="flex items-stretch gap-4 min-w-max">
          {stages.map((s, i) => {
            const meta = dealStageMeta(s.id, s.label);
            return (<div key={s.id} className="flex items-stretch gap-4">
                <KPI label={meta.label} value={<span>{s.count}</span>} caption={s.total !== undefined ? formatUSD(s.total) : undefined} tone={meta.variant === "success" ? "positive"
                    : meta.variant === "error" ? "negative"
                        : meta.variant === "warn" ? "accent"
                            : "neutral"}/>
                {i < stages.length - 1 && (<span className="self-center text-zinc-500">›</span>)}
              </div>);
        })}
          {stages.length === 0 && (<span className="text-xs text-zinc-500">No stages provided.</span>)}
        </div>
      </div>
    </Card>);
}
//# sourceMappingURL=PipelineStrip.js.map