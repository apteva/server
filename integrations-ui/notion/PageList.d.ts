interface PageItem {
    page_id: string;
    title: string;
    icon?: string;
    /** Breadcrumb up to the page —"Apteva › Engineering". */
    parent_path?: string;
    last_edited_at?: string;
    last_edited_by?: string;
}
interface Props {
    /** Optional title for the strip —"Recent pages","Search results". */
    label?: string;
    workspace?: string;
    /** Where the"Open in Notion" link goes (search-results URL,
    * workspace home, etc.). When omitted, no header action. */
    url?: string;
    pages?: PageItem[] | string;
    max?: number;
    preview?: boolean;
    projectId?: string;
}
export default function PageList(props: Props): import("react").JSX.Element;
export {};
//# sourceMappingURL=PageList.d.ts.map