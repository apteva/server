interface Props {
    /** "owner/name" — required. */
    repo: string;
    /** GitHub issue number (#NN), required. */
    issue_number: number;
    /** Issue title — agent passes from get_issue response. */
    title?: string;
    /** Issue state — "open" or "closed". */
    state?: "open" | "closed";
    state_reason?: "completed" | "not_planned" | "reopened" | null;
    /** Author login (the user who opened the issue). */
    user_login?: string;
    /** Author avatar URL. */
    user_avatar_url?: string;
    /** Comma-separated label names. Optional convenience for terse
     *  agent calls; richer apps can pass `labels` directly. */
    labels?: string;
    /** Comma-separated assignee logins. */
    assignees?: string;
    /** Comments count. */
    comments?: number;
    /** Soft preview convention — render synthetic data when no real
     *  data is available so the dashboard's app detail panel can show
     *  what the card looks like even before the user creates a
     *  connection. */
    preview?: boolean;
    /** Injected by the host (unused in v1, here for the future fetch
     *  path). */
    projectId?: string;
}
export default function IssueCard(props: Props): any;
export {};
//# sourceMappingURL=IssueCard.d.ts.map