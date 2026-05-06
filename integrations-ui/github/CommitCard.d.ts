interface Props {
    repo: string;
    /** Full or short sha — display will short. */
    sha: string;
    /** Full commit message (first line is subject, rest body). */
    message?: string;
    author_login?: string;
    author_avatar_url?: string;
    committed_at?: string;
    additions?: number;
    deletions?: number;
    changed_files?: number;
    /** Comma-separated paths — agent passes the touched files. */
    files?: string;
    /** GitHub signature verification status. */
    verified?: boolean;
    /** Render as a one-liner for inline mentions in chat. */
    compact?: boolean;
    preview?: boolean;
    projectId?: string;
}
export default function CommitCard(props: Props): import("react").JSX.Element;
export {};
//# sourceMappingURL=CommitCard.d.ts.map