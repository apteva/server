interface Props {
    ticket_id: string;
    subject?: string;
    content?: string;
    /** HubSpot internal priority id: LOW | MEDIUM | HIGH | URGENT. */
    hs_ticket_priority?: string;
    /** Pipeline-stage id (per-pipeline;"1" is"New" in the default). */
    hs_pipeline_stage?: string;
    /** Optional human-friendly stage label override. */
    stage_label?: string;
    /** ISO timestamp when the ticket was created. */
    createdate?: string;
    /** ISO timestamp of the last update. */
    hs_lastmodifieddate?: string;
    company_name?: string;
    company_domain?: string;
    /** Number of comments / replies on the ticket. */
    comment_count?: number;
    portal_id?: string;
    preview?: boolean;
    projectId?: string;
}
export default function TicketCard(props: Props): import("react").JSX.Element;
export {};
//# sourceMappingURL=TicketCard.d.ts.map