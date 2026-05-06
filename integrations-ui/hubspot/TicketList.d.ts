interface TicketRow {
    ticket_id: string;
    subject?: string;
    hs_ticket_priority?: string;
    hs_pipeline_stage?: string;
    stage_label?: string;
    createdate?: string;
    company_name?: string;
}
interface Props {
    items?: TicketRow[];
    title?: string;
    subtitle?: string;
    max_rows?: number;
    portal_id?: string;
    preview?: boolean;
    projectId?: string;
}
export default function TicketList(props: Props): import("react").JSX.Element;
export {};
//# sourceMappingURL=TicketList.d.ts.map