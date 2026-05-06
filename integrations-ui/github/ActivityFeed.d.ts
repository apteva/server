type Kind = "push" | "pull_opened" | "pull_merged" | "pull_closed" | "issue_opened" | "issue_closed" | "release" | "run_completed" | "comment" | "branch_create";
interface Event {
    id: string;
    kind: Kind;
    /** ISO timestamp. */
    timestamp: string;
    title: string;
    subtitle?: string;
    /** Optional URL for the event (commit, PR, run, etc.). */
    href?: string;
}
interface Props {
    /** "owner/name" — required for the header link + title. */
    repo: string;
    /** Either an array (preferred) or JSON-encoded list of events. */
    events?: Event[] | string;
    /** Cap rendered events; show "+N more" footer when exceeded. Default 12. */
    max?: number;
    preview?: boolean;
    projectId?: string;
}
export default function ActivityFeed(props: Props): import("react").JSX.Element;
export {};
//# sourceMappingURL=ActivityFeed.d.ts.map