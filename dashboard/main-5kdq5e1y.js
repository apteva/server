var m=/^#{1,6}\s+.+$/m;function g(e){return m.test(e)}function p(e){return`# Role
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
- Add stable lessons here when evaluations or operators identify recurring behavior.`}function y(e,t){let s=e.trim();if(g(s))return e;if(!s)return p(t);return`# Role
You are ${(t||"").trim()||"this agent"}.

# Goals
- ${s.replace(/\s+/g," ")}

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
- Add stable lessons here when evaluations or operators identify recurring behavior.`}function w(e,t){let s=t.map((i)=>i.trim()).filter(Boolean);if(s.length===0)return e;if(!g(e))return s.reduce((i,n)=>i.trim()?`${i.trimEnd()}

${n}`:n,e);return v(e,"Learning",s.map(f).join(`
`))}function f(e){let t=e.replace(/\s+/g," ").trim();if(t.startsWith("- ")||t.startsWith("* "))return t;return`- ${t}`}function v(e,t,s){let i=e.replace(/\r\n/g,`
`),n=i.split(`
`),h=t.toLowerCase(),a=-1;for(let r=0;r<n.length;r+=1){let c=u(n[r]);if(c&&c.toLowerCase()===h){a=r;break}}if(a<0){let r=i.trimEnd();return`${r?`${r}

`:""}# ${t}
${s}`}let l=n.length;for(let r=a+1;r<n.length;r+=1)if(u(n[r])){l=r;break}let o=l;while(o>a+1&&n[o-1].trim()==="")o-=1;let d=[...n.slice(a+1,o).some((r)=>r.trim()!=="")?[""]:[],...s.split(`
`),...l<n.length?[""]:[]];return[...n.slice(0,o),...d,...n.slice(l)].join(`
`).trimEnd()}function u(e){let t=e.match(/^#{1,6}\s+(.+?)\s*$/);return t?t[1].replace(/#+$/,"").trim():null}
export{y as f,w as g};

//# debugId=F4C404DAC495964664756E2164756E21
//# sourceMappingURL=main-5kdq5e1y.js.map
