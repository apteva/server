interface ContactRow {
    contact_id: string;
    firstname?: string;
    lastname?: string;
    email?: string;
    jobtitle?: string;
    company_name?: string;
    last_engagement_at?: string;
}
interface Props {
    items?: ContactRow[];
    title?: string;
    subtitle?: string;
    max_rows?: number;
    portal_id?: string;
    preview?: boolean;
    projectId?: string;
}
export default function ContactList(props: Props): import("react").JSX.Element;
export {};
//# sourceMappingURL=ContactList.d.ts.map