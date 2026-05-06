interface Props {
    deal_id: string;
    dealname?: string;
    amount?: string;
    /** Internal stage id (e.g. "contractsent"). */
    dealstage?: string;
    /** Optional human label override for custom pipelines. */
    dealstage_label?: string;
    pipeline?: string;
    /** ISO date for the expected close date. */
    closedate?: string;
    owner_email?: string;
    company_name?: string;
    company_domain?: string;
    /** HubSpot portal id — required to build the canonical record URL.
     *  Absent → the link still resolves via HubSpot's portal redirect. */
    portal_id?: string;
    preview?: boolean;
    projectId?: string;
}
export default function DealCard(props: Props): import("react").JSX.Element;
export {};
//# sourceMappingURL=DealCard.d.ts.map