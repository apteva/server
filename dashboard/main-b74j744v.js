function s(e){return`context-agent-chat:helper-conversation:${e}`}function a(e,n){return e.filter((t)=>t.kind==="direct"&&t.agent_ids.length===1&&t.agent_ids[0]===n).slice().sort((t,r)=>Date.parse(r.updated_at)-Date.parse(t.updated_at))}function u(e,n){return e.filter((t)=>t.instance_id!==n&&!t.agent_ids.includes(n))}function l(e,n,t){let r=a(e,n);if(t){let i=r.find((o)=>o.id===t);if(i)return i}return r[0]||null}
export{s as n,a as o,u as p,l as q};

//# debugId=F65793D6B9DF52E164756E2164756E21
//# sourceMappingURL=main-b74j744v.js.map
