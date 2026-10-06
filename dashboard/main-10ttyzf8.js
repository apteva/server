import{useEffect as m,useMemo as g,useRef as p}from"react";var r="",l=null,n=0;function a(e){if(e===!1||e==null)return"";return String(e).replace(/\s+/g," ").trim()}function o(e){if(Array.isArray(e)){let t=e.map(a).filter(Boolean);if(t.length===0)return"";if(t.length===1)return t[0];return`${t[0]}: ${t.slice(1).join(" - ")}`}return a(e)}function i(){let e=r?`${r} - Apteva`:"Apteva",t=n>0?`(${n>99?"99+":n}) `:"";document.title=`${t}${e}`}function f(e,t){l=e,r=o(t),i()}function c(e){if(l!==e)return;l=null,r="",i()}function T(e){n=Math.max(0,Math.floor(e||0)),i()}function A(e){let t=p(null);if(!t.current)t.current=Symbol("page-title");let u=g(()=>o(e),[e]);m(()=>{let s=t.current;return f(s,u),()=>c(s)},[u])}
export{T as Eb,A as Fb};

//# debugId=F0D012E92F9CBA3D64756E2164756E21
//# sourceMappingURL=main-10ttyzf8.js.map
