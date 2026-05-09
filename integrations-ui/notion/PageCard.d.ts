interface Props {
    page_id: string;
    title?: string;
    /** Emoji or https URL — see PageIcon. */
    icon?: string;
    /** Breadcrumb path (workspace › database › parent). One string. */
    parent_path?: string;
    /** Optional cover image URL (currently unused — kept for API parity). */
    cover?: string;
    /** Workspace slug, used to build the canonical URL. */
    workspace?: string;
    url?: string;
    archived?: boolean;
    last_edited_at?: string;
    last_edited_by?: string;
    last_edited_by_avatar?: string;
    /** Plain-text excerpt of the first content block (≤ 240 chars). */
    excerpt?: string;
    /** Comma-separated"label=value" pairs for database-row properties.
    * Example:"Status=In progress, Owner=ari, Due=May 11". */
    properties?: string;
    preview?: boolean;
    projectId?: string;
}
export default function PageCard(props: Props): import("react").JSX.Element;
export {};
//# sourceMappingURL=PageCard.d.ts.map