import type { ReactNode } from "react";
import type { CardVendor, StatusDotVariant, StatusPillVariant } from "@apteva/ui-kit";
export declare const githubLogo: ReactNode;
export declare const GITHUB_BRAND_COLOR = "#8957e5";
export declare const githubVendor: CardVendor;
export declare function repoUrl(repo: string): string;
export declare function issueUrl(repo: string, number: number | string): string;
export declare function pullRequestUrl(repo: string, number: number | string): string;
export declare function commitUrl(repo: string, sha: string): string;
export declare function workflowRunUrl(repo: string, run_id: number | string): string;
export declare function branchUrl(repo: string, branch: string): string;
export declare function userUrl(login: string): string;
/** "3d ago" / "12m ago" / "just now" — coarse human-readable delta. */
export declare function timeAgo(iso?: string): string;
/** Coerce ISO durations to mm:ss / hh:mm:ss. Used for run/job times. */
export declare function formatDuration(ms?: number): string;
/** "abc1234" — 7-char short sha. Tolerates already-short input. */
export declare function shortSha(sha?: string): string;
/** Test fixture helper — same shape as lib/hubspot's minusHoursISO. */
export declare function minusHoursISO(h: number): string;
/** PR state → StatusPill variant + label. */
export declare function pullRequestState(opts: {
    state?: "open" | "closed";
    merged?: boolean;
    draft?: boolean;
}): {
    label: string;
    variant: StatusPillVariant;
    dot: StatusDotVariant;
};
/** Workflow-run status + conclusion → label/variant pair. */
export declare function runState(opts: {
    status?: string;
    conclusion?: string;
}): {
    label: string;
    variant: StatusPillVariant;
    dot: StatusDotVariant;
};
/** Job conclusion → small dot variant for inline lists. */
export declare function jobDot(conclusion?: string, status?: string): StatusDotVariant;
export declare function avatarDataUrl(hexFill: string): string;
//# sourceMappingURL=github.d.ts.map