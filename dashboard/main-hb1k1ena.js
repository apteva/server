import{Ga as c}from"./main-kac5t8vh.js";import{useEffect as u,useState as b}from"react";var m={enabled:!1,available:!1,voice:"marin",mcp:[],provider:"openai-realtime"};function _(i,n=!1){let[s,l]=b(m);return u(()=>{if(!i||!n){l(m);return}let t=!1,o=()=>{c.config(i).then((a)=>{if(t)return;let r=(a.providers||[]).find((e)=>e.name==="openai-realtime"||e.name.includes("realtime")),p=new Set((a.mcp_servers||[]).map((e)=>e.name));l({enabled:!!a.realtime_enabled,available:!!r,voice:a.realtime_voice||r?.realtime_voice||"marin",mcp:(a.realtime_voice_mcp||[]).filter((e)=>p.has(e)),provider:r?.name||"openai-realtime"})}).catch(()=>{if(!t)l(m)})};o();let v=window.setInterval(o,15000);return()=>{t=!0,window.clearInterval(v)}},[i,n]),s}
export{_ as t};

//# debugId=76CCEF6119DD748C64756E2164756E21
//# sourceMappingURL=main-hb1k1ena.js.map
