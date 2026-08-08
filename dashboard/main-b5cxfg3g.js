import{useEffect as D,useState as F}from"react";function H(g){let[B,C]=F(()=>typeof window<"u"?window.matchMedia(g).matches:!1);return D(()=>{let z=window.matchMedia(g),A=()=>C(z.matches);return A(),z.addEventListener("change",A),()=>z.removeEventListener("change",A)},[g]),B}
export{H as w};

//# debugId=53261FC8BC177F9864756E2164756E21
//# sourceMappingURL=main-b5cxfg3g.js.map
