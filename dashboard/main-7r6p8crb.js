import{useEffect as D,useState as F}from"react";function H(g){let[B,C]=F(()=>typeof window<"u"?window.matchMedia(g).matches:!1);return D(()=>{let z=window.matchMedia(g),A=()=>C(z.matches);return A(),z.addEventListener("change",A),()=>z.removeEventListener("change",A)},[g]),B}
export{H as v};

//# debugId=6E4458EA8CD7450964756E2164756E21
//# sourceMappingURL=main-7r6p8crb.js.map
