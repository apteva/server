interface Props {
    database_id: string;
    title?: string;
    /** Emoji or https URL. */
    icon?: string;
    description?: string;
    parent_path?: string;
    workspace?: string;
    url?: string;
    /** Total rows in the database. */
    item_count?: number;
    /** Comma-separated "Name:type" pairs. */
    schema?: string;
    /** Comma-separated view names. */
    views?: string;
    last_edited_at?: string;
    last_edited_by?: string;
    last_edited_by_avatar?: string;
    preview?: boolean;
    projectId?: string;
}
export default function DatabaseCard(props: Props): import("react").JSX.Element;
export {};
//# sourceMappingURL=DatabaseCard.d.ts.map