// Shared helpers for every HubSpot UI component.
//
//   * pill metadata for deal stages, ticket priorities, lifecycle
//     stages — one source of truth for label + color across cards
//   * formatters: USD, relative date, relative time-since
//   * URL builders for HubSpot canonical record links
//   * favicon helper (Google s2)
//   * pillToDot — bridges StatusPill variant → StatusDot variant for
//     CardHeader's status slot
//   * HubSpot brand mark SVG used in every card header
const DEAL_STAGE = {
    appointmentscheduled: { label: "Appointment scheduled", variant: "info" },
    qualifiedtobuy: { label: "Qualified to buy", variant: "info" },
    presentationscheduled: { label: "Presentation scheduled", variant: "info" },
    decisionmakerboughtin: { label: "Decision-maker bought in ", variant: "info" },
    contractsent: { label: "Contract sent", variant: "warn" },
    closedwon: { label: "Closed (won)", variant: "success" },
    closedlost: { label: "Closed (lost)", variant: "error" },
};
export function dealStageMeta(id, override) {
    if (!id)
        return { label: override ?? "—", variant: "neutral" };
    const known = DEAL_STAGE[id];
    if (known)
        return { label: override ?? known.label, variant: known.variant };
    return { label: override ?? id, variant: "neutral" };
}
const TICKET_PRIORITY = {
    LOW: { label: "Low", variant: "neutral" },
    MEDIUM: { label: "Medium", variant: "info" },
    HIGH: { label: "High", variant: "warn" },
    URGENT: { label: "Urgent", variant: "error" },
};
export function ticketPriorityMeta(id) {
    if (!id)
        return { label: "—", variant: "neutral" };
    return TICKET_PRIORITY[id] ?? { label: id, variant: "neutral" };
}
const TICKET_STAGE = {
    "1": { label: "New", variant: "info" },
    "2": { label: "Waiting on us", variant: "warn" },
    "3": { label: "Waiting on them", variant: "neutral" },
    "4": { label: "Closed", variant: "success" },
};
export function ticketStageMeta(id, override) {
    if (!id)
        return { label: override ?? "—", variant: "neutral" };
    const known = TICKET_STAGE[id];
    if (known)
        return { label: override ?? known.label, variant: known.variant };
    return { label: override ?? id, variant: "neutral" };
}
const LIFECYCLE = {
    subscriber: { label: "Subscriber", variant: "neutral" },
    lead: { label: "Lead", variant: "neutral" },
    marketingqualifiedlead: { label: "MQL", variant: "info" },
    salesqualifiedlead: { label: "SQL", variant: "info" },
    opportunity: { label: "Opportunity", variant: "info" },
    customer: { label: "Customer", variant: "success" },
    evangelist: { label: "Evangelist", variant: "success" },
    other: { label: "Other", variant: "neutral" },
};
export function lifecycleMeta(id) {
    if (!id)
        return { label: "—", variant: "neutral" };
    return LIFECYCLE[id] ?? { label: id, variant: "neutral" };
}
// ─── formatters ───────────────────────────────────────────────────
export function formatUSD(raw) {
    if (raw === undefined || raw === null || raw === "")
        return "—";
    const n = typeof raw === "number" ? raw : Number(raw);
    if (!Number.isFinite(n))
        return String(raw);
    return n.toLocaleString("en-US", {
        style: "currency",
        currency: "USD",
        maximumFractionDigits: n >= 1000 ? 0 : 2,
    });
}
export function formatRelativeDate(iso) {
    if (!iso)
        return "—";
    const d = new Date(iso);
    if (Number.isNaN(d.getTime()))
        return iso;
    const now = new Date();
    const days = Math.round((d.getTime() - now.getTime()) / 86_400_000);
    const dateStr = d.toLocaleDateString("en-US", { month: "short", day: "numeric" });
    if (days === 0)
        return `${dateStr} · today`;
    if (days > 0)
        return `${dateStr} · in ${days} day${days === 1 ? "" : "s"}`;
    const overdue = -days;
    return `${dateStr} · ${overdue} day${overdue === 1 ? "" : "s"} overdue`;
}
export function timeAgo(iso) {
    if (!iso)
        return "—";
    const d = new Date(iso);
    if (Number.isNaN(d.getTime()))
        return iso;
    const sec = Math.round((Date.now() - d.getTime()) / 1000);
    if (sec < 60)
        return "just now";
    const min = Math.round(sec / 60);
    if (min < 60)
        return `${min}m ago`;
    const hr = Math.round(min / 60);
    if (hr < 24)
        return `${hr}h ago`;
    const day = Math.round(hr / 24);
    if (day < 7)
        return `${day}d ago`;
    const wk = Math.round(day / 7);
    if (wk < 5)
        return `${wk}w ago`;
    return d.toLocaleDateString("en-US", { month: "short", day: "numeric" });
}
export function addDaysISO(n) {
    const d = new Date();
    d.setDate(d.getDate() + n);
    return d.toISOString().slice(0, 10);
}
export function minusHoursISO(n) {
    const d = new Date();
    d.setHours(d.getHours() - n);
    return d.toISOString();
}
// ─── URL builders ─────────────────────────────────────────────────
const PORTAL_FALLBACK = "0";
export function recordUrl(type, id, portalId) {
    const portal = portalId || PORTAL_FALLBACK;
    return `https://app.hubspot.com/contacts/${portal}/${type}/${id}`;
}
export function pipelineUrl(portalId, pipeline) {
    const portal = portalId || PORTAL_FALLBACK;
    const path = pipeline ? `?pipeline=${encodeURIComponent(pipeline)}` : "";
    return `https://app.hubspot.com/pipelines/${portal}/deals${path}`;
}
// ─── favicon helper ───────────────────────────────────────────────
export function faviconFor(domain, size = 32) {
    if (!domain)
        return undefined;
    return `https://www.google.com/s2/favicons?domain=${encodeURIComponent(domain)}&sz=${size}`;
}
// ─── pill → dot bridge ────────────────────────────────────────────
//
// CardHeader's status slot wants a StatusDot variant. StatusPill's
// variant set is wider; this is the canonical mapping.
export function pillToDot(v) {
    switch (v) {
        case "success": return "live";
        case "info": return "active";
        case "warn": return "warn";
        case "error": return "error";
        default: return "muted";
    }
}
export const hubspotLogo = (<svg viewBox="0 0 24 24" width="14" height="14" fill="currentColor" aria-hidden>
    <circle cx="18" cy="6" r="2.2"/>
    <circle cx="18" cy="18" r="2.2"/>
    <circle cx="6" cy="12" r="2.2"/>
    <circle cx="14" cy="12" r="3" fill="none" stroke="currentColor" strokeWidth="1.5"/>
    <path d="M8.2 12 L11 12 M16 7.5 L15 10.2 M16 16.5 L15 13.8" stroke="currentColor" strokeWidth="1.2" fill="none"/>
  </svg>);
// HubSpot's official brand orange. Passed through CardHeader's
// `vendor.color` so the brand strip on every HubSpot-emitted card
// reads as obviously HubSpot at a glance, in both light and dark.
export const HUBSPOT_BRAND_COLOR = "#FF7A59";
export const hubspotVendor = {
    name: "HubSpot",
    logo: hubspotLogo,
    color: HUBSPOT_BRAND_COLOR,
};
//# sourceMappingURL=hubspot.js.map