var B={large:"",medium:"",small:""};function C(x){return x.toLowerCase().trim().replace(/\s+/g,"-")}function E(x){let m=new Set,q=[];for(let z of x){if(m.has(z.provider_key))continue;m.add(z.provider_key),q.push(z)}return q}function F(x){let m=new Map;for(let q of x){let z=q.project_id||"global",v=`${q.provider_key}:${z}`,A=m.get(v)||[];A.push(q),m.set(v,A)}return[...m.entries()]}function G(x,m,q=""){let z=new Set(m.filter((v)=>v.project_id===q).map((v)=>v.app_slug));return x.filter((v)=>!z.has(v.slug))}
export{B as f,C as g,E as h,F as i,G as j};

//# debugId=B8E44B963A30E04964756E2164756E21
//# sourceMappingURL=main-t8fxqw05.js.map
