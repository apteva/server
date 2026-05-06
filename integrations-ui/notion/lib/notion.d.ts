import type { ReactNode } from "react";
import type { CardVendor } from "@apteva/ui-kit";
export declare const notionLogo: ReactNode;
export declare const NOTION_BRAND_COLOR = "#191919";
export declare const notionVendor: CardVendor;
export declare function pageUrl(id: string, workspace?: string): string;
export declare function databaseUrl(id: string, workspace?: string): string;
export declare function searchUrl(query: string): string;
/** "3d ago" / "12m ago" / "just now". */
export declare function timeAgo(iso?: string): string;
/** Test fixture helper — same shape as the other libs. */
export declare function minusHoursISO(h: number): string;
interface PageIconProps {
    /** Emoji string OR https URL. Anything else is treated as nothing. */
    icon?: string | null;
    /** Optional: render this letter when there's no icon (page title's
     *  initial usually). Falls back to a generic doc glyph if absent. */
    fallback?: string;
    size?: number;
    className?: string;
}
export declare function PageIcon({ icon, fallback, size, className }: PageIconProps): import("react").JSX.Element;
export type NotionPropType = "title" | "rich_text" | "select" | "multi_select" | "person" | "date" | "checkbox" | "number" | "url" | "email" | "phone" | "files" | "formula" | "relation" | "rollup" | "status" | "created_time" | "last_edited_time" | "created_by" | "last_edited_by";
export interface NotionPropDef {
    name: string;
    type: NotionPropType;
}
export declare function parseSchema(raw?: string): NotionPropDef[];
/** Type → tone class for the schema pills. */
export declare function propTone(type: NotionPropType): string;
export {};
//# sourceMappingURL=notion.d.ts.map