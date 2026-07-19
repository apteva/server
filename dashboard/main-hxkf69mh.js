function n(e){return e.toLowerCase().trim().replace(/\s+/g,"-")}function s(e){try{let r=JSON.parse(e||"{}");return typeof r.default_provider==="string"?n(r.default_provider):""}catch{return""}}function a(e,r){let i=(r||[]).filter((t)=>!n(t.name).endsWith("-realtime")),o=i.find((t)=>t.default)||i[0];if(o)return n(o.name);return s(e)}
export{a};

//# debugId=A4000E0BD5042CF064756E2164756E21
//# sourceMappingURL=main-hxkf69mh.js.map
