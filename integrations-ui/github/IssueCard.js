// IssueCard — chat-attachment card for a single GitHub issue.
//
// The agent calls
//   respond(components=[{app:"github", name:"issue-card", props:{...}}])
// and this card mounts under the agent's message bubble.
//
// v1 renders from inline props — the agent passes what it just got
// back from the github get_issue tool. A future revision can add a
// component-side fetch path so the card live-updates without the
// agent having to re-render. Until that exists, props ARE the
// source of truth.
//
// Visuals are entirely composed from @apteva/ui-kit primitives so the
// card looks like every other chat-attachment in the platform.
import { Card, CardHeader, StatusPill, Avatar, DataList } from "@apteva/ui-kit";
const previewSample = {
    number: 1234,
    title: "Retry tier escalation skips dunning step on past-due renewals",
    state: "open",
    state_reason: null,
    user: {
        login: "maya",
        avatar_url: "data:image/svg+xml;utf8," +
            encodeURIComponent("<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'><circle cx='8' cy='8' r='8' fill='%23a78bfa'/></svg>"),
    },
    assignees: [
        {
            login: "ari",
            avatar_url: "data:image/svg+xml;utf8," +
                encodeURIComponent("<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'><circle cx='8' cy='8' r='8' fill='%2334d399'/></svg>"),
        },
    ],
    labels: [
        { name: "billing", color: "1f6feb" },
        { name: "bug", color: "d73a4a" },
    ],
    comments: 7,
    html_url: "https://github.com/acme/api/issues/1234",
};
const githubLogo = (<svg viewBox="0 0 24 24" width="14" height="14" fill="currentColor" aria-hidden>
    <path d="M12 .297c-6.63 0-12 5.373-12 12 0 5.303 3.438 9.8 8.205 11.385.6.113.82-.258.82-.577 0-.285-.01-1.04-.015-2.04-3.338.724-4.042-1.61-4.042-1.61-.546-1.387-1.333-1.756-1.333-1.756-1.089-.745.084-.729.084-.729 1.205.084 1.838 1.236 1.838 1.236 1.07 1.835 2.807 1.305 3.492.998.108-.776.42-1.305.762-1.604-2.665-.305-5.466-1.334-5.466-5.93 0-1.31.467-2.38 1.235-3.22-.135-.303-.54-1.524.105-3.176 0 0 1.005-.322 3.3 1.23.96-.267 1.98-.4 3-.405 1.02.005 2.04.138 3 .405 2.28-1.552 3.285-1.23 3.285-1.23.645 1.652.24 2.873.12 3.176.765.84 1.23 1.91 1.23 3.22 0 4.61-2.805 5.625-5.475 5.92.435.375.81 1.102.81 2.222 0 1.605-.015 2.898-.015 3.293 0 .315.21.69.825.57C20.565 22.092 24 17.592 24 12.297c0-6.627-5.373-12-12-12"/>
  </svg>);
// Stable color palette for labels we don't get a hex from. Hashes
// the label name into one of a small fixed set so the same label
// always renders the same color across cards.
const labelPalette = ["1f6feb", "8957e5", "d73a4a", "fbca04", "0e8a16", "5319e7", "b60205", "0075ca"];
function defaultLabelColor(name) {
    let h = 0;
    for (let i = 0; i < name.length; i++)
        h = (h * 31 + name.charCodeAt(i)) >>> 0;
    return labelPalette[h % labelPalette.length];
}
function pillForState(issue) {
    if (issue.state === "closed") {
        if (issue.state_reason === "not_planned")
            return { label: "closed (not planned)", variant: "neutral" };
        return { label: "closed", variant: "success" };
    }
    return { label: "open", variant: "info" };
}
export default function IssueCard(props) {
    const issue = props.preview
        ? previewSample
        : {
            number: props.issue_number,
            title: props.title || `Issue #${props.issue_number}`,
            state: props.state || "open",
            state_reason: props.state_reason || null,
            user: {
                login: props.user_login || "",
                avatar_url: props.user_avatar_url || "",
            },
            assignees: (props.assignees || "")
                .split(",")
                .map((s) => s.trim())
                .filter(Boolean)
                .map((login) => ({ login, avatar_url: "" })),
            labels: (props.labels || "")
                .split(",")
                .map((s) => s.trim())
                .filter(Boolean)
                .map((name) => ({ name, color: defaultLabelColor(name) })),
            comments: typeof props.comments === "number" ? props.comments : 0,
            html_url: `https://github.com/${props.repo}/issues/${props.issue_number}`,
        };
    const pill = pillForState(issue);
    const url = issue.html_url;
    const repo = props.repo;
    return (<Card>
      <CardHeader logo={githubLogo} title={repo} subtitle={`#${issue.number} · ${issue.title}`} status={{ label: pill.label, variant: pill.variant === "info" ? "active" : pill.variant === "success" ? "live" : "muted" }} action={{ label: "View on GitHub", href: url }}/>
      <div className="px-3 py-3 flex flex-col gap-3">
        <div className="flex items-center gap-2 text-xs text-text-muted">
          <Avatar src={issue.user.avatar_url} name={issue.user.login} size={16}/>
          <span className="text-text">{issue.user.login}</span>
          <span className="text-text-dim">opened this issue</span>
          <span className="ml-auto inline-flex items-center gap-1">
            <span className="text-text-dim">💬</span>
            <span className="text-text-muted">{issue.comments}</span>
          </span>
        </div>
        <DataList items={[
            { label: "Status", value: <StatusPill variant={pill.variant}>{pill.label}</StatusPill> },
            ...(issue.labels && issue.labels.length > 0
                ? [
                    {
                        label: "Labels",
                        value: (<span className="flex flex-wrap gap-1">
                        {issue.labels.map((l) => (<span key={l.name} className="px-1.5 py-0.5 rounded text-[10px] font-medium" style={{ backgroundColor: `#${l.color}33`, color: `#${l.color}` }}>
                            {l.name}
                          </span>))}
                      </span>),
                    },
                ]
                : []),
            ...(issue.assignees && issue.assignees.length > 0
                ? [
                    {
                        label: "Assignees",
                        value: (<span className="inline-flex items-center gap-1">
                        {issue.assignees.map((a) => (<span key={a.login} className="inline-flex items-center gap-1 text-xs text-text">
                            <Avatar src={a.avatar_url} name={a.login} size={12}/>
                            {a.login}
                          </span>))}
                      </span>),
                    },
                ]
                : []),
        ]}/>
      </div>
    </Card>);
}
//# sourceMappingURL=IssueCard.js.map