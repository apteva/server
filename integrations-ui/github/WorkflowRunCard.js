// WorkflowRunCard — single GitHub Actions run.
//
// Used after the agent pushes / re-runs / opens a PR — the operator
// pastes the URL and the card shows what's happening on CI without
// needing to load github.com. Auto-refreshes (via the host's
// IntegrationCard refresh_seconds prop) while status=in_progress.
//
// Job list is rendered as inline rows of [StatusDot · name · duration]
// — same density as the deal-stages KPI strip but vertical.
import { Avatar, Card, CardHeader, DataList, StatusDot, StatusPill } from "@apteva/ui-kit";
import { avatarDataUrl, formatDuration, githubVendor, jobDot, runState, shortSha, timeAgo, workflowRunUrl, } from "./lib/github";
const previewSample = {
    repo: "acme/api",
    run_id: 18723492,
    workflow_name: "CI",
    status: "in_progress",
    conclusion: "neutral",
    head_branch: "feature/idempotency-keys",
    head_sha: "abc1234d5e6f7890abcdef",
    event: "push",
    run_number: 2841,
    actor_login: "maya",
    actor_avatar_url: avatarDataUrl("a78bfa"),
    started_at: new Date(Date.now() - 2 * 60 * 1000).toISOString(),
    duration_ms: 2 * 60 * 1000,
    jobs: [
        { name: "lint", status: "completed", conclusion: "success", duration_ms: 12_000 },
        { name: "unit", status: "completed", conclusion: "success", duration_ms: 63_000 },
        { name: "integration", status: "in_progress", conclusion: undefined, duration_ms: 120_000 },
        { name: "e2e", status: "queued", conclusion: undefined },
        { name: "build", status: "queued", conclusion: undefined },
        { name: "deploy", status: "queued", conclusion: undefined },
    ],
};
export default function WorkflowRunCard(props) {
    const p = props.preview
        ? previewSample
        : {
            repo: props.repo,
            run_id: props.run_id,
            workflow_name: props.workflow_name || "Workflow",
            status: props.status ?? "completed",
            conclusion: props.conclusion ?? "neutral",
            head_branch: props.head_branch || "main",
            head_sha: props.head_sha || "",
            event: props.event || "push",
            run_number: props.run_number ?? 0,
            actor_login: props.actor_login || "",
            actor_avatar_url: props.actor_avatar_url || "",
            started_at: props.started_at || new Date().toISOString(),
            duration_ms: props.duration_ms ?? 0,
            jobs: parseJobs(props.jobs) ?? [],
        };
    const overall = runState({ status: p.status, conclusion: p.conclusion });
    const url = workflowRunUrl(p.repo, p.run_id);
    // Rolling job tally — for the headline KPI under the title.
    const total = p.jobs.length;
    const done = p.jobs.filter((j) => j.status === "completed").length;
    const tally = total > 0 ? `${done}/${total} jobs done` : null;
    return (<Card>
      <CardHeader vendor={githubVendor} title={`${p.repo} · ${p.workflow_name}`} subtitle={<span>
            Run #{p.run_number} · {p.event} · <span className="font-mono">{p.head_branch}</span>
          </span>} status={{ label: overall.label, variant: overall.dot }} action={{ label: "View run", href: url }}/>

      <div className="px-4 py-3 flex flex-col gap-3">
        {/* actor + relative time + duration */}
        <div className="flex items-center gap-2 text-xs">
          <Avatar src={p.actor_avatar_url} name={p.actor_login} size={18}/>
          <span className="text-zinc-900 dark:text-zinc-100 font-medium">{p.actor_login}</span>
          <span className="text-zinc-500">triggered {timeAgo(p.started_at)}</span>
          <span className="ml-auto text-zinc-500 tabular-nums">
            {formatDuration(p.duration_ms)}
          </span>
        </div>

        <DataList items={[
            {
                label: "Status",
                value: (<span className="inline-flex items-center gap-2">
                  <StatusPill variant={overall.variant}>{overall.label}</StatusPill>
                  {tally && <span className="text-xs text-zinc-500">{tally}</span>}
                </span>),
            },
            ...(p.head_sha
                ? [{
                        label: "Commit",
                        value: (<span className="font-mono text-xs">
                      <span className="text-zinc-900 dark:text-zinc-100">{shortSha(p.head_sha)}</span>
                      <span className="text-zinc-500"> · {p.head_branch}</span>
                    </span>),
                    }]
                : []),
        ]}/>

        {/* Job list — small dot + name + duration. Most CI runs have
            6–10 jobs so this stays compact. */}
        {p.jobs.length > 0 && (<ul className="flex flex-col divide-y divide-zinc-100 dark:divide-zinc-800 border-t border-zinc-100 dark:border-zinc-800 -mx-4">
            {p.jobs.map((j) => (<li key={j.name} className="flex items-center gap-3 px-4 py-1.5 text-xs">
                <StatusDot variant={jobDot(j.conclusion, j.status)}>
                  {j.status === "in_progress" ? "running" : j.status === "queued" ? "queued" : (j.conclusion || "—")}
                </StatusDot>
                <span className="text-zinc-900 dark:text-zinc-100 font-medium font-mono">{j.name}</span>
                <span className="ml-auto text-zinc-500 tabular-nums">
                  {j.status === "queued" ? "—" : formatDuration(j.duration_ms)}
                </span>
              </li>))}
          </ul>)}
      </div>
    </Card>);
}
function parseJobs(raw) {
    if (!raw)
        return null;
    if (Array.isArray(raw))
        return raw;
    try {
        const parsed = JSON.parse(raw);
        return Array.isArray(parsed) ? parsed : null;
    }
    catch {
        return null;
    }
}
//# sourceMappingURL=WorkflowRunCard.js.map