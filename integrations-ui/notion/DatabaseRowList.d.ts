interface RowItem {
    page_id: string;
    title: string;
    icon?: string;
    /** Status select-option label (any select prop the agent picks). */
    status?: string;
    /** Status color hint — Notion's option colors. */
    status_color?: "default" | "gray" | "brown" | "orange" | "yellow" | "green" | "blue" | "purple" | "pink" | "red";
    /** Person property — first assignee's display name. */
    owner?: string;
    owner_avatar?: string;
    /** Date property — pre-formatted like"May 11" or ISO. */
    due?: string;
    /** Plain-text excerpt of the row's first content block. */
    excerpt?: string;
    /** Last-edit timestamp for sorting / display. */
    last_edited_at?: string;
}
interface Props {
    database_id: string;
    database_title?: string;
    database_icon?: string;
    workspace?: string;
    url?: string;
    /** Optional view / filter label —"In progress","This sprint", … */
    view_label?: string;
    /** Either an array (preferred) or JSON-encoded string. */
    rows?: RowItem[] | string;
    /** Cap rendered rows; rest collapses into"+N more". Default 10. */
    max?: number;
    preview?: boolean;
    projectId?: string;
}
export default function DatabaseRowList(props: Props): import("react").JSX.Element;
export {};
//# sourceMappingURL=DatabaseRowList.d.ts.map