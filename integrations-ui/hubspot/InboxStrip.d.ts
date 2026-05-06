interface InboxItem {
    email_id: string;
    from_name?: string;
    from_email?: string;
    subject?: string;
    /** First line of the body — caller pre-trims. */
    snippet?: string;
    sent_at?: string;
    unread?: boolean;
    thread_length?: number;
}
interface Props {
    items?: InboxItem[];
    title?: string;
    subtitle?: string;
    max_rows?: number;
    portal_id?: string;
    preview?: boolean;
    projectId?: string;
}
export default function InboxStrip(props: Props): import("react").JSX.Element;
export {};
//# sourceMappingURL=InboxStrip.d.ts.map