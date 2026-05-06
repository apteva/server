interface Props {
    /** "owner/name" — required. */
    repo: string;
    /** Pull-request number, required. */
    pr_number: number;
    title?: string;
    state?: "open" | "closed";
    /** GitHub returns merged separately on closed PRs. */
    merged?: boolean;
    /** Draft PRs don't get the "open" treatment. */
    draft?: boolean;
    user_login?: string;
    user_avatar_url?: string;
    /** Comma-separated reviewer logins — terse agent calls. */
    reviewers?: string;
    /** Approval count (0+). */
    approvals?: number;
    /** Changes-requested count (0+). */
    changes_requested?: number;
    /** Comma-separated label names. */
    labels?: string;
    comments?: number;
    additions?: number;
    deletions?: number;
    changed_files?: number;
    head_ref?: string;
    base_ref?: string;
    /** Mergeable state from GitHub. */
    mergeable?: "mergeable" | "conflicting" | "unknown" | "draft";
    created_at?: string;
    preview?: boolean;
    projectId?: string;
}
export default function PullRequestCard(props: Props): import("react").JSX.Element;
export {};
//# sourceMappingURL=PullRequestCard.d.ts.map