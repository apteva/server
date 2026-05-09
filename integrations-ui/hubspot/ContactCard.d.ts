interface Props {
    contact_id: string;
    firstname?: string;
    lastname?: string;
    email?: string;
    phone?: string;
    jobtitle?: string;
    lifecyclestage?: string;
    /** Pre-resolved by the agent (HubSpot returns associated company id). */
    company_name?: string;
    company_domain?: string;
    /** ISO timestamp of the most recent engagement (email open, call,
    * note, etc.) — surfaced as"emailed 3d ago" /"no contact in 14d". */
    last_engagement_at?: string;
    last_engagement_kind?: string;
    portal_id?: string;
    preview?: boolean;
    projectId?: string;
}
export default function ContactCard(props: Props): import("react").JSX.Element;
export {};
//# sourceMappingURL=ContactCard.d.ts.map