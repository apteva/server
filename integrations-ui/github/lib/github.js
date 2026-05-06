// Shared helpers for every GitHub UI component.
//
//   * githubVendor — CardHeader brand pill (logo + name + color)
//   * URL builders — repo, issue, PR, commit, workflow run, branch
//   * formatters — relative time, short sha, file-stat string
//   * status mappers — PR state → StatusPill variant, run conclusion
//     → StatusDot variant, etc.
//
// Mirrors the lib/hubspot.tsx pattern so any vendor lib has the
// same shape (logo + vendor + URL helpers + status mappers + time
// helpers).
// ─── Brand mark ───────────────────────────────────────────────────
//
// Octocat outline. currentColor so CardHeader's vendor pill can
// recolor it via inline `style.color`.
export const githubLogo = (<svg viewBox="0 0 24 24" width="14" height="14" fill="currentColor" aria-hidden>
    <path d="M12 .297c-6.63 0-12 5.373-12 12 0 5.303 3.438 9.8 8.205 11.385.6.113.82-.258.82-.577 0-.285-.01-1.04-.015-2.04-3.338.724-4.042-1.61-4.042-1.61-.546-1.387-1.333-1.756-1.333-1.756-1.089-.745.084-.729.084-.729 1.205.084 1.838 1.236 1.838 1.236 1.07 1.835 2.807 1.305 3.492.998.108-.776.42-1.305.762-1.604-2.665-.305-5.466-1.334-5.466-5.93 0-1.31.467-2.38 1.235-3.22-.135-.303-.54-1.524.105-3.176 0 0 1.005-.322 3.3 1.23.96-.267 1.98-.4 3-.405 1.02.005 2.04.138 3 .405 2.28-1.552 3.285-1.23 3.285-1.23.645 1.652.24 2.873.12 3.176.765.84 1.23 1.91 1.23 3.22 0 4.61-2.805 5.625-5.475 5.92.435.375.81 1.102.81 2.222 0 1.605-.015 2.898-.015 3.293 0 .315.21.69.825.57C20.565 22.092 24 17.592 24 12.297c0-6.627-5.373-12-12-12"/>
  </svg>);
// GitHub Mona Purple — secondary brand color, used in the vendor
// pill and any GitHub-specific accents. Distinct from HubSpot orange
// so two cards from different vendors read as different upstreams at
// a glance.
export const GITHUB_BRAND_COLOR = "#8957e5";
export const githubVendor = {
    name: "GitHub",
    logo: githubLogo,
    color: GITHUB_BRAND_COLOR,
};
// ─── URL builders ─────────────────────────────────────────────────
export function repoUrl(repo) {
    return `https://github.com/${repo}`;
}
export function issueUrl(repo, number) {
    return `https://github.com/${repo}/issues/${number}`;
}
export function pullRequestUrl(repo, number) {
    return `https://github.com/${repo}/pull/${number}`;
}
export function commitUrl(repo, sha) {
    return `https://github.com/${repo}/commit/${sha}`;
}
export function workflowRunUrl(repo, run_id) {
    return `https://github.com/${repo}/actions/runs/${run_id}`;
}
export function branchUrl(repo, branch) {
    return `https://github.com/${repo}/tree/${encodeURIComponent(branch)}`;
}
export function userUrl(login) {
    return `https://github.com/${login}`;
}
// ─── Formatters ───────────────────────────────────────────────────
/** "3d ago" / "12m ago" / "just now" — coarse human-readable delta. */
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
/** Coerce ISO durations to mm:ss / hh:mm:ss. Used for run/job times. */
export function formatDuration(ms) {
    if (!ms || ms < 0)
        return "—";
    const s = Math.round(ms / 1000);
    if (s < 60)
        return `${s}s`;
    const m = Math.floor(s / 60);
    const rs = s % 60;
    if (m < 60)
        return rs ? `${m}m ${rs}s` : `${m}m`;
    const h = Math.floor(m / 60);
    return `${h}h ${m % 60}m`;
}
/** "abc1234" — 7-char short sha. Tolerates already-short input. */
export function shortSha(sha) {
    if (!sha)
        return "";
    return sha.length > 7 ? sha.slice(0, 7) : sha;
}
/** Test fixture helper — same shape as lib/hubspot's minusHoursISO. */
export function minusHoursISO(h) {
    return new Date(Date.now() - h * 60 * 60 * 1000).toISOString();
}
// ─── Status mappers ───────────────────────────────────────────────
/** PR state → StatusPill variant + label. */
export function pullRequestState(opts) {
    if (opts.merged)
        return { label: "merged", variant: "success", dot: "live" };
    if (opts.state === "closed")
        return { label: "closed", variant: "error", dot: "error" };
    if (opts.draft)
        return { label: "draft", variant: "neutral", dot: "muted" };
    return { label: "open", variant: "info", dot: "active" };
}
/** Workflow-run status + conclusion → label/variant pair. */
export function runState(opts) {
    if (opts.status === "in_progress")
        return { label: "running", variant: "info", dot: "active" };
    if (opts.status === "queued")
        return { label: "queued", variant: "neutral", dot: "muted" };
    switch (opts.conclusion) {
        case "success": return { label: "success", variant: "success", dot: "live" };
        case "failure": return { label: "failed", variant: "error", dot: "error" };
        case "cancelled": return { label: "cancelled", variant: "neutral", dot: "muted" };
        case "skipped": return { label: "skipped", variant: "neutral", dot: "muted" };
        case "timed_out": return { label: "timed out", variant: "warn", dot: "warn" };
        case "neutral": return { label: "neutral", variant: "neutral", dot: "muted" };
        default: return { label: "—", variant: "neutral", dot: "muted" };
    }
}
/** Job conclusion → small dot variant for inline lists. */
export function jobDot(conclusion, status) {
    if (status === "in_progress")
        return "active";
    if (status === "queued")
        return "muted";
    switch (conclusion) {
        case "success": return "live";
        case "failure": return "error";
        case "cancelled": return "muted";
        case "skipped": return "muted";
        case "timed_out": return "warn";
        default: return "muted";
    }
}
// ─── Mini avatar fallback (data URL) ──────────────────────────────
//
// IssueCard's preview avatars used inline data:image/svg+xml URLs so
// the explorer doesn't depend on network requests. Same helper
// reused across cards' preview samples.
export function avatarDataUrl(hexFill) {
    return ("data:image/svg+xml;utf8," +
        encodeURIComponent(`<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'><circle cx='8' cy='8' r='8' fill='%23${hexFill}'/></svg>`));
}
//# sourceMappingURL=github.js.map