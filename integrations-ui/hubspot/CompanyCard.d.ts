interface Props {
    company_id: string;
    name?: string;
    domain?: string;
    industry?: string;
    /** HubSpot returns numberofemployees as a string. */
    numberofemployees?: string;
    city?: string;
    country?: string;
    lifecyclestage?: string;
    /** Sum of open-deal amounts associated with this company. Caller
     *  pre-computes (avoids an extra fetch on render). */
    open_deal_total?: string;
    open_deal_count?: number;
    description?: string;
    portal_id?: string;
    preview?: boolean;
    projectId?: string;
}
export default function CompanyCard(props: Props): import("react").JSX.Element;
export {};
//# sourceMappingURL=CompanyCard.d.ts.map