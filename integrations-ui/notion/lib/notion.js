// Shared helpers for every Notion UI component.
//
//   * notionVendor — CardHeader brand pill (logo + name + color)
//   * URL builders — page, database, search, comment
//   * formatters — relative time, page-icon resolver, property-value
//     pretty-printer for typed Notion properties
//
// Mirrors the lib/hubspot.tsx / lib/github.tsx pattern: every vendor
// lib has the same shape (logo + vendor + URLs + helpers).
// ─── Brand mark ───────────────────────────────────────────────────
//
// Notion's mark is famously a stylized "N". currentColor so
// CardHeader's vendor pill recolors it via inline `style.color`.
export const notionLogo = (<svg viewBox="0 0 32 32" width="14" height="14" fill="currentColor" aria-hidden>
    <path d="M5 4.7l3.4 2.3a1 1 0 00.8.2l16-1.6c.5-.1 1 .2 1 .7v18.4c0 .5-.4.9-.9.9L8 26.2c-.5 0-1-.3-1.2-.7l-1.7-3.4V4.7zm4 4.5v15l16-.9V8.3l-16 .9zm3.4 2.3v9.3l1.7.1V13l4.7 7.7 1.6.1v-9.4l-1.7-.1v6.6l-4.6-7.4-1.7.1z"/>
  </svg>);
// Notion's brand is monochrome — they use near-black `#191919` for
// the wordmark on light surfaces. Works fine in both themes since
// our vendor pill applies it as ink color on a low-alpha bg.
export const NOTION_BRAND_COLOR = "#191919";
export const notionVendor = {
    name: "Notion",
    logo: notionLogo,
    color: NOTION_BRAND_COLOR,
};
// ─── URL builders ─────────────────────────────────────────────────
//
// Notion pages are addressable by id with dashes stripped. A page or
// database canonical URL is `https://www.notion.so/<id-no-dashes>`.
// When a workspace slug is known (e.g. "apteva"), the URL becomes
// `https://www.notion.so/apteva/<id>` — same target, prettier link.
function bareId(id) {
    if (!id)
        return "";
    return id.replace(/-/g, "");
}
export function pageUrl(id, workspace) {
    const slug = workspace ? `${encodeURIComponent(workspace)}/` : "";
    return `https://www.notion.so/${slug}${bareId(id)}`;
}
export function databaseUrl(id, workspace) {
    return pageUrl(id, workspace);
}
export function searchUrl(query) {
    return `https://www.notion.so/search?query=${encodeURIComponent(query)}`;
}
// ─── Formatters ───────────────────────────────────────────────────
/** "3d ago" / "12m ago" / "just now". */
export function timeAgo(iso) {
    if (!iso)
        return "";
    const t = new Date(iso).getTime();
    if (!Number.isFinite(t))
        return "";
    const s = Math.max(0, (Date.now() - t) / 1000);
    if (s < 60)
        return "just now";
    const m = s / 60;
    if (m < 60)
        return `${Math.round(m)}m ago`;
    const h = m / 60;
    if (h < 24)
        return `${Math.round(h)}h ago`;
    const d = h / 24;
    if (d < 30)
        return `${Math.round(d)}d ago`;
    const mo = d / 30;
    if (mo < 12)
        return `${Math.round(mo)}mo ago`;
    return `${Math.round(mo / 12)}y ago`;
}
/** Test fixture helper — same shape as the other libs. */
export function minusHoursISO(h) {
    return new Date(Date.now() - h * 60 * 60 * 1000).toISOString();
}
export function PageIcon({ icon, fallback, size = 18, className = "" }) {
    const style = { width: size, height: size, fontSize: Math.round(size * 0.66) };
    const isUrl = !!icon && /^https?:\/\//i.test(icon);
    const isEmoji = !!icon && !isUrl && Array.from(icon).length <= 3;
    if (isUrl) {
        return (<img src={icon} alt="" className={`rounded-sm flex-shrink-0 object-cover ${className}`} style={style}/>);
    }
    if (isEmoji) {
        return (<span aria-hidden className={`flex-shrink-0 inline-flex items-center justify-center leading-none ${className}`} style={style}>
        {icon}
      </span>);
    }
    // Generic doc fallback — minimal page glyph, monochrome.
    return (<span aria-hidden className={`flex-shrink-0 inline-flex items-center justify-center rounded-sm bg-zinc-100 dark:bg-zinc-800 text-zinc-500 font-medium ${className}`} style={style}>
      {fallback ? fallback.charAt(0).toUpperCase() : "·"}
    </span>);
}
export function parseSchema(raw) {
    if (!raw)
        return [];
    return raw.split(",").map((s) => {
        const [name, type] = s.split(":").map((x) => x.trim());
        return { name: name || "—", type: (type || "rich_text") };
    });
}
/** Type → tone class for the schema pills. */
export function propTone(type) {
    switch (type) {
        case "select":
        case "status": return "bg-blue-500/10 text-blue-700 dark:bg-blue-500/15 dark:text-blue-400";
        case "multi_select": return "bg-purple-500/10 text-purple-700 dark:bg-purple-500/15 dark:text-purple-400";
        case "person":
        case "created_by":
        case "last_edited_by": return "bg-emerald-500/10 text-emerald-700 dark:bg-emerald-500/15 dark:text-emerald-400";
        case "date":
        case "created_time":
        case "last_edited_time": return "bg-amber-500/10 text-amber-700 dark:bg-amber-500/15 dark:text-amber-400";
        case "checkbox": return "bg-zinc-200 text-zinc-700 dark:bg-zinc-700 dark:text-zinc-300";
        case "number": return "bg-cyan-500/10 text-cyan-700 dark:bg-cyan-500/15 dark:text-cyan-400";
        default: return "bg-zinc-100 text-zinc-700 dark:bg-zinc-800 dark:text-zinc-300";
    }
}
//# sourceMappingURL=notion.js.map