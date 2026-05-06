type Kind = "email" | "call" | "note" | "task" | "meeting" | "record_change";
interface Event {
    id: string;
    kind: Kind;
    /** ISO timestamp. */
    timestamp: string;
    title: string;
    subtitle?: string;
    /** Engagement id — used to build the canonical link. */
    engagement_id?: string;
}
interface Props {
    events?: Event[];
    /** Cap rendered events; show "+N more" when exceeded. Default 12. */
    max?: number;
    portal_id?: string;
    preview?: boolean;
    projectId?: string;
}
export default function ActivityFeed(props: Props): import("react").JSX.Element;
export {};
//# sourceMappingURL=ActivityFeed.d.ts.map