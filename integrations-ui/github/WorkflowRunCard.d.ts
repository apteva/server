interface Job {
    name: string;
    status?: string;
    conclusion?: string;
    duration_ms?: number;
}
interface Props {
    repo: string;
    run_id: number;
    workflow_name?: string;
    status?: "queued" | "in_progress" | "completed";
    conclusion?: "success" | "failure" | "cancelled" | "skipped" | "neutral" | "timed_out";
    head_branch?: string;
    head_sha?: string;
    /** What triggered the run — "push", "pull_request", "schedule", etc. */
    event?: string;
    run_number?: number;
    actor_login?: string;
    actor_avatar_url?: string;
    started_at?: string;
    /** Total wall-clock duration in ms (server computes it). */
    duration_ms?: number;
    /** Either an array (preferred) or JSON-encoded string of jobs. */
    jobs?: Job[] | string;
    preview?: boolean;
    projectId?: string;
}
export default function WorkflowRunCard(props: Props): import("react").JSX.Element;
export {};
//# sourceMappingURL=WorkflowRunCard.d.ts.map