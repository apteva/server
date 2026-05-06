interface Stage {
    /** HubSpot internal stage id (e.g. "contractsent"). */
    id: string;
    /** Optional human label override for custom pipelines. */
    label?: string;
    count: number;
    /** Sum of deal amounts in the stage. HubSpot wire string or number. */
    total?: string | number;
}
interface Props {
    /** Pipeline display name (e.g. "Sales pipeline"). */
    pipeline_label?: string;
    /** Pipeline id — used for the canonical link. */
    pipeline?: string;
    stages?: Stage[];
    portal_id?: string;
    preview?: boolean;
    projectId?: string;
}
export default function PipelineStrip(props: Props): import("react").JSX.Element;
export {};
//# sourceMappingURL=PipelineStrip.d.ts.map