interface DealRow {
    deal_id: string;
    dealname?: string;
    amount?: string;
    dealstage?: string;
    dealstage_label?: string;
    closedate?: string;
    company_name?: string;
}
interface Props {
    items?: DealRow[];
    /** Headline shown in the card header. */
    title?: string;
    /** Subtitle below the title — e.g."Open · sorted by close date". */
    subtitle?: string;
    /** Cap rendered rows; show"+N more" footer when exceeded. */
    max_rows?: number;
    portal_id?: string;
    preview?: boolean;
    projectId?: string;
}
export default function DealList(props: Props): import("react").JSX.Element;
export {};
//# sourceMappingURL=DealList.d.ts.map