// Settings page: prompt tabs, repository add and chip removal, and the agent choice. Without JS the prompts are stacked.
function selectTab(tab, focus) {
  tab.parentNode.querySelectorAll('[role="tab"]').forEach(function (t) {
    var on = t === tab;
    t.setAttribute("aria-selected", on ? "true" : "false");
    t.tabIndex = on ? 0 : -1;
    document.getElementById(t.getAttribute("aria-controls")).hidden = !on;
  });
  var field = document.querySelector('#settings-form input[name="tab"]');
  if (field) field.value = tab.id.slice(4); // "tab-<step>"; the server renders it selected after a 422
  if (focus) tab.focus();
}
// Runs on load and after every swap (a 422 replaces the form); shows the tab the server rendered selected.
function setupTabs() {
  var list = document.querySelector(".prompt-tabs");
  if (!list) return;
  list.hidden = false;
  selectTab(list.querySelector('[aria-selected="true"]'), false);
}
document.addEventListener("DOMContentLoaded", setupTabs);
document.addEventListener("htmx:afterSettle", setupTabs);
document.addEventListener("click", function (e) {
  if (!(e.target instanceof Element)) return;
  var tab = e.target.closest('.prompt-tabs [role="tab"]');
  if (tab) selectTab(tab, true);
  var rm = e.target.closest(".chip-remove");
  if (rm) {
    var chip = rm.closest(".chip"), next = chip.nextElementSibling || chip.previousElementSibling;
    chip.remove();
    (next ? next.querySelector(".chip-remove") : document.getElementById("repos-h")).focus();
  }
});
// Arrow keys, Home and End move between tabs.
document.addEventListener("keydown", function (e) {
  var tab = e.target instanceof Element && e.target.closest('.prompt-tabs [role="tab"]');
  if (!tab) return;
  var tabs = Array.from(tab.parentNode.querySelectorAll('[role="tab"]')), i = tabs.indexOf(tab), n = tabs.length;
  var j = { ArrowRight: i + 1, ArrowLeft: i - 1 + n, Home: 0, End: n - 1 }[e.key];
  if (j === undefined) return;
  e.preventDefault();
  selectTab(tabs[j % n], true);
});
// Once the user picks an agent, the first-launch oob swap of #agent must not overwrite the choice.
document.addEventListener("change", function (e) {
  if (e.target instanceof Element && e.target.name === "agent") e.target.closest("#agent").dataset.touched = "1";
});
document.addEventListener("htmx:oobBeforeSwap", function (e) {
  if (e.detail.target.id === "agent" && e.detail.target.dataset.touched) e.detail.shouldSwap = false;
});
// Enter in the repository search adds what is typed instead of submitting the form.
document.addEventListener("keydown", function (e) {
  if (e.key === "Enter" && e.target instanceof Element && e.target.id === "repo-q") {
    e.preventDefault();
    document.getElementById("repo-add-btn").click();
  }
});
// After a repository is added: clear the search and its suggestions, and return focus to the search.
document.addEventListener("htmx:afterRequest", function (e) {
  if (!e.detail.successful || e.detail.requestConfig.path !== "/settings/repo") return;
  var q = document.getElementById("repo-q");
  q.value = "";
  document.getElementById("repo-suggest").replaceChildren();
  q.focus();
});
