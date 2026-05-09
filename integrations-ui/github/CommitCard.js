// CommitCard — single GitHub commit.
//
// The agent's"I just pushed X" response, or any inline reference to
// a commit by sha. Two variants:
//
// - Default: full card with subject, body excerpt, file stats, and
// a list of touched files.
// - Compact: one-line affordance for inline mentions in chat —
// [octocat] abc1234 refactor webhook handler
//
// The compact variant skips CardHeader entirely and renders inside
// a <Card compact>.
import { Avatar, Card, CardHeader, DataList, StatusPill } from "@apteva/ui-kit";
import { avatarDataUrl, commitUrl, githubVendor, shortSha, timeAgo, } from "./lib/github";
const previewSample = {
    repo: "acme/api",
    sha: "abc1234d5e6f7890abcdef",
    subject: "refactor webhook handler to share retry logic",
    body: "The retry policy was duplicated across the four webhook receivers." + "Extract into shared/retry.ts and apply consistently across stripe," + "hubspot, github, linear handlers.",
    author: { login: "maya", avatar_url: avatarDataUrl("a78bfa") },
    committed_at: new Date(Date.now() - 12 * 60 * 1000).toISOString(),
    additions: 47,
    deletions: 31,
    changed_files: 5,
    files: ["api/webhooks/stripe.ts", "api/webhooks/hubspot.ts", "api/webhooks/github.ts", "api/webhooks/linear.ts", "shared/retry.ts",
    ],
    verified: true,
};
export default function CommitCard(props) {
    const p = props.preview
        ? previewSample
        : (() => {
            const message = props.message || "";
            const [subject, ...rest] = message.split("\n");
            return {
                repo: props.repo,
                sha: props.sha,
                subject: subject || `Commit ${shortSha(props.sha)}`,
                body: rest.join("\n").trim(),
                author: {
                    login: props.author_login || "",
                    avatar_url: props.author_avatar_url || "",
                },
                committed_at: props.committed_at || new Date().toISOString(),
                additions: props.additions ?? 0,
                deletions: props.deletions ?? 0,
                changed_files: props.changed_files ?? 0,
                files: (props.files || "").split(",").map((s) => s.trim()).filter(Boolean),
                verified: !!props.verified,
            };
        })();
    const url = commitUrl(p.repo, p.sha);
    const sha = shortSha(p.sha);
    if (props.compact) {
        return (<Card compact href={url}>
 <span className="text-[11px] uppercase tracking-wider font-semibold px-1.5 py-0.5 rounded-md flex-shrink-0" style={{ color: "#8957e5", backgroundColor: "#8957e514" }}>
 GITHUB
 </span>
 <span className="font-mono text-xs text-text-dim flex-shrink-0">{sha}</span>
 <span className="text-sm text-text truncate">{p.subject}</span>
 <span className="ml-auto text-xs text-text-dim flex-shrink-0">{timeAgo(p.committed_at)}</span>
 </Card>);
    }
    return (<Card>
 <CardHeader vendor={githubVendor} title={p.repo} subtitle={<span>
 <span className="font-mono">{sha}</span> · {p.subject}
 </span>} status={p.verified ? { label: "verified", variant: "live" } : undefined} action={{ label: "View commit", href: url }}/>

 <div className="px-4 py-3 flex flex-col gap-3">
 {/* author + time + diff stats */}
 <div className="flex items-center gap-2 text-xs">
 <Avatar src={p.author.avatar_url} name={p.author.login} size={18}/>
 <span className="text-text font-medium">{p.author.login}</span>
 <span className="text-text-dim">committed {timeAgo(p.committed_at)}</span>
 <span className="ml-auto inline-flex items-center gap-2 tabular-nums">
 <span className="text-green-600 dark:text-success">+{p.additions}</span>
 <span className="text-red-600 dark:text-error">−{p.deletions}</span>
 <span className="text-text-dim">in {p.changed_files} file{p.changed_files === 1 ? "" : "s"}</span>
 </span>
 </div>

 {p.body && (<p className="text-sm text-text whitespace-pre-wrap break-words leading-relaxed line-clamp-3">
 {p.body}
 </p>)}

 {p.files.length > 0 && (<DataList items={[
                {
                    label: "Files",
                    value: (<ul className="flex flex-col gap-0.5 font-mono text-xs">
 {p.files.slice(0, 5).map((f) => (<li key={f} className="text-text truncate">{f}</li>))}
 {p.files.length > 5 && (<li className="text-text-dim">+{p.files.length - 5} more</li>)}
 </ul>),
                },
            ]}/>)}

 {p.verified && (<div className="flex items-center gap-1.5 text-[11px] text-text-dim">
 <StatusPill variant="success">verified signature</StatusPill>
 </div>)}
 </div>
 </Card>);
}
//# sourceMappingURL=CommitCard.js.map