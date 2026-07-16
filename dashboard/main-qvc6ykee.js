import{$ as s}from"./main-45wrcmr0.js";import*as n from"react";var m=s((p)=>{function i(e,t){return e===t&&(e!==0||1/e===1/t)||e!==e&&t!==t}var f=typeof Object.is==="function"?Object.is:i,d=n.useState,l=n.useEffect,S=n.useLayoutEffect,h=n.useDebugValue;function y(e,t){var r=t(),c=d({inst:{value:r,getSnapshot:t}}),u=c[0].inst,o=c[1];return S(function(){u.value=r,u.getSnapshot=t,a(u)&&o({inst:u})},[e,r,t]),l(function(){return a(u)&&o({inst:u}),e(function(){a(u)&&o({inst:u})})},[e]),h(r),r}function a(e){var t=e.getSnapshot;e=e.value;try{var r=t();return!f(e,r)}catch(c){return!0}}function E(e,t){return t()}var v=typeof window>"u"||typeof window.document>"u"||typeof window.document.createElement>"u"?E:y;p.useSyncExternalStore=n.useSyncExternalStore!==void 0?n.useSyncExternalStore:v});
export{m as B};

//# debugId=FFFAF22C5221880064756E2164756E21
//# sourceMappingURL=main-qvc6ykee.js.map
