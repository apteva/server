import type { StatusDotVariant, StatusPillVariant } from "@apteva/ui-kit";
export type PillMeta = {
    label: string;
    variant: StatusPillVariant;
};
export declare function dealStageMeta(id: string | undefined, override?: string): PillMeta;
export declare function ticketPriorityMeta(id: string | undefined): PillMeta;
export declare function ticketStageMeta(id: string | undefined, override?: string): PillMeta;
export declare function lifecycleMeta(id: string | undefined): PillMeta;
export declare function formatUSD(raw: string | number | undefined | null): string;
export declare function formatRelativeDate(iso: string | undefined): string;
export declare function timeAgo(iso: string | undefined): string;
export declare function addDaysISO(n: number): string;
export declare function minusHoursISO(n: number): string;
export declare function recordUrl(type: "deal" | "company" | "contact" | "ticket" | "engagement", id: string, portalId?: string): string;
export declare function pipelineUrl(portalId?: string, pipeline?: string): string;
export declare function faviconFor(domain: string | undefined, size?: number): string | undefined;
export declare function pillToDot(v: StatusPillVariant): StatusDotVariant;
import type { ReactNode } from "react";
import type { CardVendor } from "@apteva/ui-kit";
export declare const hubspotLogo: ReactNode;
export declare const HUBSPOT_BRAND_COLOR = "#FF7A59";
export declare const hubspotVendor: CardVendor;
//# sourceMappingURL=hubspot.d.ts.map