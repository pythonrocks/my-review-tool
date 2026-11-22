// External links open in the system browser through this delegated click handler and Browser.OpenURL;
// the app window never leaves its own origin. /wails/runtime.js is served by the Wails app.
document.addEventListener("click", async (e) => {
  const a = e.target instanceof Element && e.target.closest("a[href]");
  if (!a || a.origin === location.origin) return;
  e.preventDefault();
  if (a.protocol === "https:" || a.protocol === "http:") {
    (await import("/wails/runtime.js")).Browser.OpenURL(a.href);
  }
});
