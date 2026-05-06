interface Props {
    /** Engagement id. */
    email_id: string;
    direction?: "INCOMING_EMAIL" | "EMAIL" | "FORWARDED_EMAIL";
    from_name?: string;
    from_email?: string;
    to_email?: string;
    subject?: string;
    /** Plain-text body — first 240 chars rendered as snippet. */
    body?: string;
    /** ISO timestamp the email was received / sent. */
    sent_at?: string;
    /** Length of the thread this message belongs to (1 if standalone). */
    thread_length?: number;
    /** Associated company / contact name for the chip strip. */
    company_name?: string;
    company_domain?: string;
    contact_name?: string;
    portal_id?: string;
    preview?: boolean;
    projectId?: string;
}
export default function EmailCard(props: Props): import("react").JSX.Element;
export {};
//# sourceMappingURL=EmailCard.d.ts.map