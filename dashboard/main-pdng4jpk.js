import{useEffect as D,useState as F}from"react";function H(g){let[B,C]=F(()=>typeof window<"u"?window.matchMedia(g).matches:!1);return D(()=>{let z=window.matchMedia(g),A=()=>C(z.matches);return A(),z.addEventListener("change",A),()=>z.removeEventListener("change",A)},[g]),B}
export{H as q};

//# debugId=CD80A8E6C47DC4C164756E2164756E21
//# sourceMappingURL=main-pdng4jpk.js.map
