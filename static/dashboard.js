if (window.htmx) document.documentElement.classList.add("htmx-on");
// Pause the list poller while the tab is hidden; refresh at once when it is shown again.
document.addEventListener("htmx:beforeRequest", function (e) {
  if (document.hidden && e.detail.elt.id === "pr-poll") e.preventDefault();
});
document.addEventListener("visibilitychange", function () {
  var poll = document.getElementById("pr-poll");
  if (!document.hidden && poll && window.htmx) htmx.trigger(poll, "myreview:refresh");
});
// A failed Re-run answers 4xx with an error row; swap it in place of the row. Other errors (poll, filters) are never swapped.
document.addEventListener("htmx:beforeSwap", function (e) {
  var s = e.detail.xhr.status;
  if (s >= 400 && s < 500 && e.detail.requestConfig.verb === "post" && e.detail.pathInfo.requestPath === "/rerun") e.detail.shouldSwap = true;
});
// Re-runs queue on #pr-list so none aborts another; a poll in flight is aborted first so the Re-run goes out at once and the stale poll is never swapped.
document.addEventListener("htmx:confirm", function (e) {
  var poll = document.getElementById("pr-poll");
  if (e.detail.verb === "post" && e.detail.path === "/rerun" && poll && poll.classList.contains("htmx-request")) htmx.trigger(document.getElementById("pr-list"), "htmx:abort");
});
