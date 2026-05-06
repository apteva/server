// ActivityFeed (GitHub flavor) — chronological event stream for a
// repo. Mirrors the HubSpot ActivityFeed shape exactly (Card +
// CardHeader + Timeline) so a demo profile can swap CRM activity ↔
// engineering activity by changing the slot's component.
//
// Event-kind mapping table (kept in sync with what the agent
// fetches from gh's events API and normalises before passing in):
//
//   push           Pushed N commits to <branch>
//   pull_opened    Opened PR #<n> "<title>"
//   pull_merged    Merged PR #<n>
//   pull_closed    Closed PR #<n>
//   issue_opened   Opened issue #<n> "<title>"
//   issue_closed   Closed issue #<n>
//   release        Released <tag>
//   run_completed  CI #<n> <success|failed> on <branch>
//   comment        Commented on #<n>
//   branch_create  Created branch <name>
//
// The agent server-side compacts rapid-fire pushes from the same
// actor into one entry ("Pushed 3 commits to X") so the feed stays
// scannable. The component itself is dumb — every event is one row.
import { Check, CircleDot, GitBranchPlus, GitCommitHorizontal, GitMerge, GitPullRequest, GitPullRequestClosed, MessageSquare, Tag, } from "lucide-react";
import { Card, CardHeader, Timeline } from "@apteva/ui-kit";
import { githubVendor, minusHoursISO, repoUrl } from "./lib/github";
const KIND_TONE = {
    push: "neutral",
    pull_opened: "info",
    pull_merged: "success",
    pull_closed: "error",
    issue_opened: "info",
    issue_closed: "success",
    release: "success",
    run_completed: "neutral",
    comment: "neutral",
    branch_create: "neutral",
};
const KIND_ICON = {
    push: GitCommitHorizontal,
    pull_opened: GitPullRequest,
    pull_merged: GitMerge,
    pull_closed: GitPullRequestClosed,
    issue_opened: CircleDot,
    issue_closed: Check,
    release: Tag,
    run_completed: Check,
    comment: MessageSquare,
    branch_create: GitBranchPlus,
};
const previewEvents = [
    { id: "1", kind: "push", timestamp: minusHoursISO(0.2), title: "Pushed 3 commits to feature/idempotency-keys", subtitle: "maya · refactor webhook handler · …" },
    { id: "2", kind: "run_completed", timestamp: minusHoursISO(0.13), title: "CI #2841 passed", subtitle: "feature/idempotency-keys · 2m 14s" },
    { id: "3", kind: "pull_opened", timestamp: minusHoursISO(0.5), title: "Opened PR #847", subtitle: "Add idempotency keys to webhook receiver · maya" },
    { id: "4", kind: "issue_closed", timestamp: minusHoursISO(2), title: "Closed issue #1219", subtitle: "Webhook receipts duplicate on retry · ari" },
    { id: "5", kind: "comment", timestamp: minusHoursISO(3), title: "Commented on PR #843", subtitle: "lin · \"can we split this into 2 PRs?\"" },
    { id: "6", kind: "release", timestamp: minusHoursISO(28), title: "Released v1.4.2", subtitle: "3 contributors · 12 commits since v1.4.1" },
    { id: "7", kind: "run_completed", timestamp: minusHoursISO(31), title: "CI #2837 failed", subtitle: "main · 4m 02s · integration job timed out" },
    { id: "8", kind: "pull_merged", timestamp: minusHoursISO(33), title: "Merged PR #843", subtitle: "ari → main · Switch retry queue to bullmq" },
    { id: "9", kind: "issue_opened", timestamp: minusHoursISO(50), title: "Opened issue #1234", subtitle: "Retry tier escalation skips dunning step · maya" },
    { id: "10", kind: "branch_create", timestamp: minusHoursISO(54), title: "Created branch feature/idempotency-keys", subtitle: "from main · maya" },
];
export default function ActivityFeed(props) {
    const repo = props.repo || "acme/api"; // preview-mode fallback
    const events = props.preview
        ? previewEvents
        : (parseEvents(props.events) ?? []);
    const max = props.max ?? 12;
    // Hand events to the ui-kit Timeline. Build the icon node lazily —
    // each kind gets its own sized lucide component so the column of
    // icons reads as a coherent legend.
    const timelineEvents = events.map((e) => {
        const Icon = KIND_ICON[e.kind] ?? GitCommitHorizontal;
        return {
            id: e.id,
            timestamp: e.timestamp,
            tone: KIND_TONE[e.kind] ?? "neutral",
            icon: <Icon className="w-3.5 h-3.5"/>,
            title: e.title,
            subtitle: e.subtitle,
            href: e.href,
        };
    });
    // Headline count — count what we show, not what would render
    // after the +N truncation; consistent with HubSpot's pattern.
    const subtitle = events.length === 0
        ? "Quiet"
        : `${events.length} event${events.length === 1 ? "" : "s"}`;
    return (<Card fullWidth>
      <CardHeader vendor={githubVendor} title={`${repo} · activity`} subtitle={subtitle} action={{ label: "View on GitHub", href: repoUrl(repo) }}/>
      <Timeline events={timelineEvents} max={max} emptyLabel="No activity yet."/>
    </Card>);
}
function parseEvents(raw) {
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
//# sourceMappingURL=ActivityFeed.js.map