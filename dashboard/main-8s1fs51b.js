var o={autonomous:"Instructs the agent to act independently within its scope and permissions, respecting explicit approval requirements.",cautious:"Instructs the agent to ask and wait before state-changing actions. Read-only work can proceed within scope.",learn:"Instructs the agent to ask before unfamiliar tool and scope combinations, including reads, and reuse approvals available in context. No dedicated safety-profile storage."},c="These choices add behavior instructions; they do not enforce approval gates.",l=25;function u(e){if(e===0)return"Reactive";if(e<=25)return"Conservative";if(e<=50)return"Balanced";if(e<=75)return"Proactive";return"Highly proactive"}function d(e){if(e===0)return"Responds to events and completes assigned work, including recurring responsibilities. Initiates no unsolicited work.";if(e<=25)return"Makes tiny adjacent follow-ups when the need and exact action are already known. Does not start unsolicited investigations.";if(e<=50)return"Investigates observed problems with clear expected benefit and completes one bounded improvement. Skips weak leads and speculative experiments.";if(e<=75)return"Validates a promising but uncertain lead and may run a small reversible experiment. Needs an existing lead; does not start discovery without one.";return"Reviews unexamined areas, compares opportunities, and coordinates worthwhile initiatives within its goals. Still sleeps when nothing useful remains; does not invent busywork."}var r=/^#{1,6}\s+.+$/m;function s(e){return r.test(e)}function i(e){return`# Role
You are ${(e||"").trim()||"this agent"}.

# Goals
- 

# Operating Rules
- Prefer direct, useful action over commentary.
- Ask before irreversible or high-blast-radius actions.

# Inputs and Events
- Treat user messages, app events, and channel messages as work requests.

# Tools and Integrations
- Use available tools when they materially improve the result.
- Never expose credentials or secrets in messages, directives, or logs.

# Schedule
- Work reactively unless a subscription, schedule, or user request says otherwise.

# Escalation and Safety
- Pause and ask when the next action is ambiguous, destructive, or externally visible.

# Tone
- Be concise, specific, and clear.

# Learning
- Add stable lessons here when evaluations or operators identify recurring behavior.`}function g(e,t){let n=e.trim();if(s(n))return e;if(!n)return i(t);return`# Role
You are ${(t||"").trim()||"this agent"}.

# Goals
- ${n.replace(/\s+/g," ")}

# Operating Rules
- Prefer direct, useful action over commentary.
- Ask before irreversible or high-blast-radius actions.

# Inputs and Events
- Treat user messages, app events, and channel messages as work requests.

# Tools and Integrations
- Use available tools when they materially improve the result.
- Never expose credentials or secrets in messages, directives, or logs.

# Schedule
- Work reactively unless a subscription, schedule, or user request says otherwise.

# Escalation and Safety
- Pause and ask when the next action is ambiguous, destructive, or externally visible.

# Tone
- Be concise, specific, and clear.

# Learning
- Add stable lessons here when evaluations or operators identify recurring behavior.`}
export{o as B,c as C,l as D,u as E,d as F,g as G};

//# debugId=3090EC69D3A787D164756E2164756E21
//# sourceMappingURL=main-8s1fs51b.js.map
