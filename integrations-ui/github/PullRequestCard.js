// PullRequestCard — chat-attachment card for a single GitHub PR.
//
// The agent passes what it just got back from the github get_pull
// tool. Same prop-shape convention as IssueCard: each field optional,
// each carrying just enough so the title + status + a few stats
// render without needing a follow-up fetch.
//
// Composes ui-kit primitives only — Card, CardHeader, StatusPill,
// Avatar, AvatarStack, DataList, KPI. No GitHub-specific chrome.
import { GitMerge, GitPullRequest } from "lucide-react";
import { Avatar, AvatarStack, Card, CardHeader, DataList, StatusPill } from "@apteva/ui-kit";
import { avatarDataUrl, githubVendor, pullRequestState, pullRequestUrl, shortSha, timeAgo, } from "./lib/github";
const previewSample = {
    repo: "acme/api",
    pr_number: 847,
    title: "Add idempotency keys to webhook receiver",
    state: "open",
    merged: false,
    draft: false,
    user: { login: "maya", avatar_url: avatarDataUrl("a78bfa") },
    reviewers: [
        { login: "ari", avatar_url: avatarDataUrl("34d399") },
        { login: "lin", avatar_url: avatarDataUrl("60a5fa") },
        { login: "soren", avatar_url: avatarDataUrl("f59e0b") },
    ],
    approvals: 1,
    changes_requested: 0,
    labels: [
        { name: "enhancement", color: "0e8a16" },
        { name: "api", color: "1f6feb" },
        { name: "needs-tests", color: "fbca04" },
    ],
    comments: 4,
    additions: 124,
    deletions: 18,
    changed_files: 7,
    head_ref: "feature/idempotency-keys",
    base_ref: "main",
    mergeable: "mergeable",
    created_at: new Date(Date.now() - 3 * 24 * 60 * 60 * 1000).toISOString(),
};
export default function PullRequestCard(props) {
    const p = props.preview
        ? previewSample
        : {
            ...previewSample,
            repo: props.repo,
            pr_number: props.pr_number,
            title: props.title || `PR #${props.pr_number}`,
            state: props.state ?? "open",
            merged: props.merged ?? false,
            draft: props.draft ?? false,
            user: { login: props.user_login || "", avatar_url: props.user_avatar_url || "" },
            reviewers: (props.reviewers || "")
                .split(",").map((s) => s.trim()).filter(Boolean)
                .map((login) => ({ login, avatar_url: "" })),
            approvals: props.approvals ?? 0,
            changes_requested: props.changes_requested ?? 0,
            labels: (props.labels || "")
                .split(",").map((s) => s.trim()).filter(Boolean)
                .map((name) => ({ name, color: "1f6feb" })),
            comments: props.comments ?? 0,
            additions: props.additions ?? 0,
            deletions: props.deletions ?? 0,
            changed_files: props.changed_files ?? 0,
            head_ref: props.head_ref || "",
            base_ref: props.base_ref || "main",
            mergeable: props.mergeable ?? "unknown",
            created_at: props.created_at || new Date().toISOString(),
        };
    const status = pullRequestState({ state: p.state, merged: p.merged, draft: p.draft });
    const reviewSummary = reviewLine(p.approvals, p.changes_requested, p.reviewers.length);
    const url = pullRequestUrl(p.repo, p.pr_number);
    const StateIcon = p.merged ? GitMerge : GitPullRequest;
    return (<Card>
      <CardHeader vendor={githubVendor} title={p.repo} subtitle={<span className="inline-flex items-center gap-1.5">
            <StateIcon className="w-3.5 h-3.5 inline-block"/>
            <span>#{p.pr_number} · {p.title}</span>
          </span>} status={{ label: status.label, variant: status.dot }} action={{ label: "View on GitHub", href: url }}/>

      <div className="px-4 py-3 flex flex-col gap-3">
        {/* author + creation time + diff stats */}
        <div className="flex items-center gap-2 text-xs">
          <Avatar src={p.user.avatar_url} name={p.user.login} size={18}/>
          <span className="text-zinc-900 dark:text-zinc-100 font-medium">{p.user.login}</span>
          <span className="text-zinc-500">opened {timeAgo(p.created_at)}</span>
          <span className="ml-auto inline-flex items-center gap-2 tabular-nums">
            <span className="text-green-600 dark:text-green-500">+{p.additions}</span>
            <span className="text-red-600 dark:text-red-500">−{p.deletions}</span>
            <span className="text-zinc-500">in {p.changed_files} file{p.changed_files === 1 ? "" : "s"}</span>
          </span>
        </div>

        <DataList items={[
            {
                label: "Status",
                value: (<span className="inline-flex items-center gap-2">
                  <StatusPill variant={status.variant}>{status.label}</StatusPill>
                  {p.mergeable === "conflicting" && (<StatusPill variant="warn">conflicts</StatusPill>)}
                </span>),
            },
            {
                label: "Branch",
                value: (<span className="font-mono text-xs">
                  <span className="text-zinc-900 dark:text-zinc-100">{p.head_ref || "—"}</span>
                  <span className="text-zinc-500 mx-1">→</span>
                  <span className="text-zinc-500">{p.base_ref}</span>
                </span>),
            },
            ...(p.reviewers.length > 0
                ? [{
                        label: "Reviewers",
                        value: (<span className="inline-flex items-center gap-2">
                      <AvatarStack users={p.reviewers} size={18} max={4}/>
                      {reviewSummary && <span className="text-xs text-zinc-500">{reviewSummary}</span>}
                    </span>),
                    }]
                : []),
            ...(p.labels.length > 0
                ? [{
                        label: "Labels",
                        value: (<span className="inline-flex flex-wrap gap-1">
                      {p.labels.map((l) => (<span key={l.name} className="text-[11px] font-medium px-1.5 py-0.5 rounded-md" style={{
                                    color: `#${l.color}`,
                                    backgroundColor: `#${l.color}1F`,
                                }}>
                          {l.name}
                        </span>))}
                    </span>),
                    }]
                : []),
            {
                label: "Activity",
                value: (<span className="text-xs text-zinc-500 tabular-nums">
                  {p.comments} comment{p.comments === 1 ? "" : "s"}
                </span>),
            },
        ]}/>
      </div>
    </Card>);
}
function reviewLine(approvals, changes, total) {
    if (total === 0)
        return "";
    const parts = [];
    if (approvals > 0)
        parts.push(`${approvals} approval${approvals === 1 ? "" : "s"}`);
    if (changes > 0)
        parts.push(`${changes} changes requested`);
    if (parts.length === 0)
        parts.push("no review yet");
    return `· ${parts.join(", ")}`;
}
// Make sure shortSha stays imported when not used yet — guards
// against linter pruning when other cards in the same lib reach
// for it. (Compile-time only.)
void shortSha;
//# sourceMappingURL=PullRequestCard.js.map