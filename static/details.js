// Details page keys: j/k next/previous point, c comment on the current point. Previous / Next links open their
// target, and "Files with points only" hides the other files of the diff.
(function () {
  var current = null; // the point (list row or diff block) last moved to or clicked
  var focusAfterLoad = null; // the id of a textarea to focus once its point body has loaded

  function points() {
    return Array.prototype.slice.call(document.querySelectorAll("li.point, .inline[id^='d-']"));
  }
  function open(el) {
    var d = el.querySelector("details");
    if (d) d.open = true;
    current = el;
  }
  function textarea(el) {
    return el.querySelector("textarea");
  }

  document.addEventListener("keydown", function (e) {
    if (e.ctrlKey || e.metaKey || e.altKey || e.target.closest("input, textarea, select, [contenteditable]")) return;
    if (e.key !== "j" && e.key !== "k" && e.key !== "c") return;
    var ps = points();
    if (!ps.length) return;
    var i = ps.indexOf(current);
    if (i < 0 && current && current.isConnected) {
      // current is a comment form or a deleted comment: start from the point before it
      for (i = ps.length - 1; i >= 0 && !(ps[i].compareDocumentPosition(current) & Node.DOCUMENT_POSITION_FOLLOWING); i--);
    }
    if (e.key === "c") {
      var el = i >= 0 ? current : ps[0];
      var d = el.querySelector("details");
      // a closed row with a lazy body is replaced when it loads, so focus the loaded textarea instead
      var lazy = d && !d.open && d.hasAttribute("hx-get") && !d.hasAttribute("data-loaded");
      open(el);
      el.scrollIntoView({ block: "nearest" });
      var ta = textarea(el);
      if (lazy && ta) focusAfterLoad = ta.id;
      else if (ta) ta.focus();
    } else {
      i = e.key === "j" ? Math.min(i + 1, ps.length - 1) : Math.max(i - 1, 0);
      open(ps[i]);
      ps[i].scrollIntoView({ block: "start" });
    }
    e.preventDefault();
  });

  // every request says which results the page shows, so the server refuses a write meant for older results (409)
  document.addEventListener("htmx:configRequest", function (e) {
    var f = e.detail.formData;
    if (!f.has("results") && !/[?&]results=/.test(e.detail.path)) f.set("results", document.body.dataset.results || "");
  });

  // Re-run and Reload review answer with HX-Redirect; any error shows in the page's alert row instead.
  document.addEventListener("htmx:responseError", function (e) {
    var slot = document.getElementById("pending-error");
    if (!slot || !e.detail.elt.classList.contains("page-action")) return;
    var p = document.createElement("p"), span = document.createElement("span");
    p.className = "alert alert-error";
    span.textContent = (e.detail.xhr.responseText.trim() || "Error " + e.detail.xhr.status);
    p.appendChild(span);
    slot.replaceChildren(p);
  });

  document.addEventListener("htmx:load", function (e) {
    // a swap replaced current (a comment form by its comment, a comment by "deleted"): follow it
    if (current && !current.isConnected && e.target.classList && e.target.classList.contains("inline")) current = e.target;
  });

  document.addEventListener("htmx:afterSwap", function (e) {
    if (e.target.tagName === "DETAILS") e.target.setAttribute("data-loaded", "");
    var ta = focusAfterLoad && document.getElementById(focusAfterLoad);
    if (ta && e.target.contains(ta)) {
      focusAfterLoad = null;
      ta.focus();
    }
  });

  document.addEventListener("click", function (e) {
    var p = e.target.closest("li.point, .inline");
    if (p) current = p;
    var nav = e.target.closest("a.point-nav");
    var target = nav && document.getElementById(nav.getAttribute("href").slice(1));
    if (target) open(target); // the browser follows the fragment and scrolls to it
  });

  var only = document.getElementById("files-with-points");
  function filter() {
    document.querySelectorAll(".file-list a[href^='#file-']").forEach(function (a) {
      var hide = only.checked && !a.querySelector(".pill");
      var file = document.getElementById(a.getAttribute("href").slice(1));
      a.style.display = hide ? "none" : "";
      if (file) file.style.display = hide ? "none" : "";
    });
  }
  if (only) {
    only.addEventListener("change", filter);
    filter(); // a checkbox the browser restored on Back
  }
})();
