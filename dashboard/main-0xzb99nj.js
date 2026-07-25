var x=new Set(["token","accesstoken","access_token","refresh_token","refreshtoken","expires_in","expiresin","token_type","tokentype","scope"]);function z(v){if(!v?.auth?.types?.includes("oauth2")||!v.auth.oauth2)return!1;let j=v.auth.credential_fields||[];if(j.length===0)return!0;if(j.every((q)=>q.source==="user"||q.source==="oauth"))return!0;return j.every((q)=>x.has(String(q.name||"").toLowerCase()))}function B(v){let j=v?.auth?.types||[];if(j.includes("oauth_device_code"))return"oauth_device_code";if(j.includes("oauth2")&&z(v))return"oauth2";return j.find((q)=>q!=="oauth2")||j[0]||""}function C(v,j){let q=v?.auth?.credential_fields||[];if(j==="oauth2")return q.filter((w)=>w.source==="user"&&!w.hidden);return q.filter((w)=>w.source!=="oauth"&&!w.hidden)}
export{B as b,C as c};

//# debugId=DFB1A69DD6EE5EA164756E2164756E21
//# sourceMappingURL=main-0xzb99nj.js.map
