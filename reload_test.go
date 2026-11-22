package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
)

// setPending replaces the pending review of a/b#3.
func setPending(t *testing.T, q *reviewQueue, p pendingPR) pendingPR {
	t.Helper()
	p, err := q.pending.update("a/b#3", func(old *pendingPR) error { *old = p; return nil })
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// noGitHub is a handler of a/b#3 whose GitHub funcs fail the test when called.
func noGitHub(t *testing.T, cfg config, p *poller, q *reviewQueue) http.Handler {
	called := func(name string) error { t.Errorf("%s called", name); return errors.New(name) }
	return newHandler(newConfigHolder(cfg, "myreview.json"), p, q,
		func(context.Context, string, int, reviewPayload) (string, error) { return "", called("submit") },
		func(context.Context, string, int, string, string) (bool, error) { return false, called("approved") },
		func(context.Context, string, int) ([]prFile, error) { return nil, called("files") },
		func(context.Context, string, string, string) ([]byte, error) { return nil, called("content") },
		func(context.Context, string, int) (threadList, error) { return threadList{}, called("threads") },
		func(context.Context, string, bool) (bool, error) { return false, called("resolve") })
}

func TestStatusBanner(t *testing.T) {
	cfg, p, q, j := doneSetup(t)
	h := noGitHub(t, cfg, p, q)
	status := func(h http.Handler, results, head string) (int, string) {
		return send(h, http.MethodGet, "/pr/a/b/3/status?"+url.Values{"results": {results}, "head": {head}, "view": {"diff"}}.Encode(), nil)
	}
	old := resultID(q.statuses()["a/b#3"])
	if code, body := status(h, old, "abc123"); code != http.StatusNoContent || body != "" {
		t.Errorf("same results = %d %q, want 204", code, body)
	}
	before := setPending(t, q, pendingPR{Result: old, Rows: 8, Drafts: []pendingDraft{
		{ID: "f0", Edited: true}, {ID: "p0"}, {ID: "c1"}, {ID: "c2", Edited: true, PostedURL: "u"}, {ID: "f1", Edited: true, PostedURL: "u"}}})
	j.Findings[0].Description = "sec low, again"
	cur := resultID(q.statuses()["a/b#3"])
	code, body := status(h, old, "abc123")
	if code != http.StatusOK || body != `<div class="alert alert-info"><span>New review results for commit abc123. Your 2 edited drafts will be kept; 7 untouched suggestions will be replaced.</span>`+
		` <form method="post" action="/pr/a/b/3/reload" hx-post="/pr/a/b/3/reload" hx-swap="none" class="page-action"><input type="hidden" name="view" value="diff"><button id="reload-review" class="btn-sm">Reload review</button></form></div>` {
		t.Errorf("new results = %d %s", code, body)
	}
	if got := q.pending.get("a/b#3"); !reflect.DeepEqual(got, before) {
		t.Errorf("status changed the pending review: %+v", got)
	}
	setPending(t, q, pendingPR{Result: old, Rows: 2, Drafts: []pendingDraft{{ID: "p1", Edited: true}}})
	mustContain(t, func() string { _, b := status(h, old, "abc123"); return b }(), "Your 1 edited draft will be kept; 1 untouched suggestion will be replaced.")
	setPending(t, q, pendingPR{Result: cur})
	if _, body := status(h, old, "abc123"); !strings.Contains(body, "<span>New review results for commit abc123 were loaded in another tab.</span> <form") || strings.Contains(body, "kept") {
		t.Errorf("another tab = %s", body)
	}
	if code, _ := status(h, cur, "abc123"); code != http.StatusNoContent {
		t.Errorf("page on the new results = %d, want 204", code)
	}
	j.Status, j.FailedStep, j.Reason = statusFailed, "code review", "exit <1>"
	if _, body := status(h, cur, "abc123"); !strings.Contains(body, "<span>The review of commit abc123 failed at code review: exit &lt;1&gt;.</span> <form") {
		t.Errorf("failed = %s", body)
	}
	failed := resultID(q.statuses()["a/b#3"])
	if _, body := status(h, failed, "0000000"); body != `<div class="alert alert-info"><span>PR updated to abc123; review queued.</span></div>` {
		t.Errorf("new head = %s", body)
	}
	j.Status = statusOnDemand
	if _, body := status(h, failed, "0000000"); body != `<div class="alert alert-info"><span>PR updated to abc123; click Review to review it.</span></div>` {
		t.Errorf("new head on demand = %s", body)
	}
	j.Status, j.SkipRule = statusSkipped, 2
	if _, body := status(h, failed, "0000000"); body != `<div class="alert alert-info"><span>PR updated to abc123; not reviewed (skipped by rule skip[2]).</span></div>` {
		t.Errorf("new head skipped = %s", body)
	}
	j.Status = statusRunning
	if code, _ := status(h, failed, "abc123"); code != http.StatusNoContent {
		t.Errorf("running = %d, want 204", code)
	}
	prs := p.snapshot().PRs
	prs[2].Draft = true
	dp := newPoller(cfg.Repositories, (&fakeFetch{prs: map[string][]pullRequest{"a/b": prs}}).fetch, noLogin)
	dp.poll(context.Background())
	if _, body := status(noGitHub(t, cfg, dp, q), "", "0000000"); !strings.Contains(body, "<span>PR updated to abc123; Re-run to review it.</span></div>") {
		t.Errorf("new head on a draft = %s", body)
	}
	if code, _ := send(h, http.MethodGet, "/pr/a/b/9/status?results=&head=", nil); code != http.StatusNotFound {
		t.Errorf("unknown PR = %d, want 404", code)
	}
}

// reloadSetup is a/b#3 with an edited draft on a finding that the new result describes differently, an untouched
// one on another changed finding and an edited one on an unchanged finding, then that new result.
func reloadSetup(t *testing.T) (http.Handler, *reviewQueue, *job) {
	h, q, j := pendingSetup(t)
	setPending(t, q, pendingPR{Result: resultID(q.statuses()["a/b#3"]), Drafts: []pendingDraft{
		{ID: "f3", File: "b.go", Line: 3, Commit: "abc123", SourceText: "b3", Text: "my b3", Edited: true, InReview: true},
		{ID: "f2", File: "b.go", Line: 20, Commit: "abc123", SourceText: "b20", Text: "untouched"},
		{ID: "f5", File: "a.go", Line: 9, Commit: "abc123", SourceText: "a9", Text: "my a9", Edited: true},
	}})
	j.Findings[2].Description, j.Findings[3].Description = "b20, again", "b3, again"
	return h, q, j
}

// wantReconciled checks the pending review of reloadSetup after reconcile.
func wantReconciled(t *testing.T, q *reviewQueue) {
	t.Helper()
	p := q.pending.get("a/b#3")
	want := []pendingDraft{
		{ID: "c1", File: "b.go", Line: 3, Commit: "abc123", SourceText: "b3", Text: "my b3", Edited: true, InReview: true, Unflagged: true},
		{ID: "f5", File: "a.go", Line: 9, Commit: "abc123", SourceText: "a9", Text: "my a9", Edited: true},
	}
	if !reflect.DeepEqual(p.Drafts, want) || p.Result != resultID(q.statuses()["a/b#3"]) || p.Rows != 8 {
		t.Errorf("pending = %+v, want drafts %+v", p, want)
	}
}

func TestReload(t *testing.T) {
	h, q, _ := reloadSetup(t)
	rev := q.pending.get("a/b#3").Rev
	code, _, loc := do(h, localReq(http.MethodPost, "/pr/a/b/3/reload", "view=diff&results=whatever"))
	if code != http.StatusSeeOther || loc != "/pr/a/b/3?view=diff" {
		t.Errorf("reload = %d %q", code, loc)
	}
	wantReconciled(t, q)
	if got := q.pending.get("a/b#3").Rev; got != rev+1 {
		t.Errorf("rev = %d, want %d", got, rev+1)
	}
	if code, _, loc := do(h, localReq(http.MethodPost, "/pr/a/b/3/reload", "view=bogus")); code != http.StatusSeeOther || loc != "/pr/a/b/3?view=list" {
		t.Errorf("invalid view = %d %q", code, loc)
	}
	if got := q.pending.get("a/b#3").Rev; got != rev+1 {
		t.Errorf("a reload with nothing new wrote: rev = %d", got)
	}
	r := localReq(http.MethodPost, "/pr/a/b/3/reload", "view=list")
	r.Header.Set("HX-Request", "true") // the app's WebKit does not follow a 303
	rec := httptest.NewRecorder()
	if h.ServeHTTP(rec, r); rec.Code != http.StatusOK || rec.Header().Get("HX-Redirect") != "/pr/a/b/3?view=list" || rec.Header().Get("Location") != "" {
		t.Errorf("htmx reload = %d, HX-Redirect %q, Location %q", rec.Code, rec.Header().Get("HX-Redirect"), rec.Header().Get("Location"))
	}
	if code, _, _ := do(h, localReq(http.MethodPost, "/pr/a/b/9/reload", "")); code != http.StatusNotFound {
		t.Errorf("unknown PR = %d, want 404", code)
	}
}

func TestReconcileOnGet(t *testing.T) {
	h, q, _ := reloadSetup(t)
	pg := pageOf(t, h)
	wantReconciled(t, q)
	mustContain(t, pg, `<body data-results="`+resultID(q.statuses()["a/b#3"])+`">`, "<p class=\"muted\">No longer flagged by the review.</p>\n", "\nmy b3</textarea>", `<a href="?view=diff#d-c1">`)
	if strings.Contains(pg, "untouched") || strings.Contains(pg, "http-equiv") {
		t.Error("the untouched draft or a meta refresh is shown")
	}
	// a failed write answers 500
	h, q, _ = reloadSetup(t)
	if err := os.RemoveAll(q.pending.dir()); err != nil {
		t.Fatal(err)
	}
	if code, _ := send(h, http.MethodGet, "/pr/a/b/3", nil); code != http.StatusInternalServerError {
		t.Errorf("failed write = %d, want 500", code)
	}
	if code, _, _ := do(h, localReq(http.MethodPost, "/pr/a/b/3/reload", "")); code != http.StatusInternalServerError {
		t.Errorf("failed reload write = %d, want 500", code)
	}
}

// Queued, running and failed results never reconcile: that would drop the untouched suggestions and marks.
func TestNoReconcileUnlessDone(t *testing.T) {
	for _, status := range []string{statusQueued, statusRunning, statusFailed} {
		h, q, j := reloadSetup(t)
		j.Status = status
		before := newPendingStore(q.store).get("a/b#3")
		pageOf(t, h)
		do(h, localReq(http.MethodPost, "/pr/a/b/3/reload", ""))
		got := newPendingStore(q.store).get("a/b#3")
		got.Rev, got.OpenedAt, got.OpenedResult = before.Rev, before.OpenedAt, before.OpenedResult // the GET starts the review clock
		if !reflect.DeepEqual(got, before) {
			t.Errorf("%s: pending file changed to %+v", status, got)
		}
	}
}

func TestStaleWrites(t *testing.T) {
	h, q, _, _, _ := commentSetup(t)
	addComment(h, "a.go", "7", "mine")
	before := q.pending.get("a/b#3")
	for _, c := range []struct{ method, path, form string }{
		{http.MethodPut, "/pr/a/b/3/drafts/f0", "text=x&results=old"},
		{http.MethodPost, "/pr/a/b/3/points/f0/checked", "checked=1&results=old"},
		{http.MethodPost, "/pr/a/b/3/drafts/f0/include", "include=1&results=old"},
		{http.MethodPut, "/pr/a/b/3/drafts/c1", "text=x&path=a.go&line=7&results=old"},
		{http.MethodPost, "/pr/a/b/3/drafts", "path=a.go&line=8&text=x&results=old"},
		{http.MethodPost, "/pr/a/b/3/review", reviewForm(q, "COMMENT", "body", "sum", "results", "old").Encode()},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, localReq(c.method, c.path, c.form))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), staleMsg) {
			t.Errorf("%s %s = %d %s", c.method, c.path, rec.Code, rec.Body)
		}
		if strings.Contains(c.path, "/points/") || strings.Contains(c.path, "/drafts/") {
			if rec.Header().Get("HX-Reswap") != "none" {
				t.Errorf("%s %s swaps", c.method, c.path)
			}
		}
	}
	if _, body := send(h, http.MethodPut, "/pr/a/b/3/drafts/f0", url.Values{"text": {"x"}, "results": {"old"}}); !strings.Contains(body, `<span class="editor-status" id="es-f0" hx-swap-oob="true">Not saved</span>`) {
		t.Errorf("stale save = %s", body)
	}
	if got := q.pending.get("a/b#3"); !reflect.DeepEqual(got, before) {
		t.Errorf("a stale write changed the pending review: %+v", got)
	}
	// GET fragments are checked only when they send results
	for _, path := range []string{"/pr/a/b/3/points/f0", "/pr/a/b/3/points/f0/excerpt", "/pr/a/b/3/review/summary", "/pr/a/b/3/diff/comment-form?path=a.go&line=8"} {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		if code, _ := send(h, http.MethodGet, path, nil); code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, code)
		}
		if code, body := send(h, http.MethodGet, path+sep+"results=old", nil); code != http.StatusConflict || !strings.Contains(body, staleMsg) {
			t.Errorf("stale GET %s = %d %s", path, code, body)
		}
		if code, _ := send(h, http.MethodGet, path+sep+"results="+resultID(q.statuses()["a/b#3"]), nil); code != http.StatusOK {
			t.Errorf("current GET %s = %d, want 200", path, code)
		}
	}
	if code, _ := send(h, http.MethodGet, "/pr/a/b/3/diff/expand?path=a.go&from=1&to=2&results=old", nil); code != http.StatusConflict {
		t.Errorf("stale expand = %d, want 409", code)
	}
}

// An Outdated line comment shows only in the list, with Discard; a new comment on its old line is a new comment.
func TestOutdatedComment(t *testing.T) {
	h, q, _, _, _ := commentSetup(t)
	addComment(h, "a.go", "7", "mine")
	if _, err := q.pending.update("a/b#3", func(p *pendingPR) error { p.Drafts[0].OutdatedFrom = "0123456789"; return nil }); err != nil {
		t.Fatal(err)
	}
	pg := pageOf(t, h)
	mustContain(t, pg, `<span class="point-loc" title="a.go:7 (on 0123456)"><span>a.go</span><span>:7</span><span> (on 0123456)</span></span>`,
		`id="state-c1" hx-swap-oob="true">Outdated</span>`,
		`<button class="btn-sm btn-quiet" type="button" hx-put="/pr/a/b/3/drafts/c1" hx-vals='{"text": ""}' hx-include="closest .editor" hx-target="#es-c1" hx-swap="outerHTML">Discard</button>`)
	if strings.Contains(pg, "#d-c1") {
		t.Error("an Outdated comment links to the diff")
	}
	_, diff := send(h, http.MethodGet, "/pr/a/b/3?view=diff", nil)
	if strings.Contains(diff, `id="d-c1"`) || strings.Contains(diff, `href="#d-c1"`) {
		t.Error("an Outdated comment is placed in the diff")
	}
	if _, body := send(h, http.MethodGet, "/pr/a/b/3/diff/comment-form?path=a.go&line=7", nil); !strings.HasPrefix(body, "<form") {
		t.Errorf("comment-form on the Outdated comment's line = %s", body)
	}
	if code, _ := addComment(h, "a.go", "7", "new"); code != http.StatusOK {
		t.Fatalf("add = %d", code)
	}
	ds := commentDrafts(q)
	if len(ds) != 2 || ds[0].Text != "mine" || ds[0].OutdatedFrom == "" || ds[1].ID != "c2" || ds[1].Text != "new" {
		t.Errorf("drafts = %+v, want the Outdated c1 kept and a new c2", ds)
	}
	// editable and discardable
	if code, _ := send(h, http.MethodPut, "/pr/a/b/3/drafts/c1", url.Values{"text": {"edited"}, "path": {"a.go"}, "line": {"7"}}); code != http.StatusOK || commentDrafts(q)[0].Text != "edited" {
		t.Errorf("edit = %d", code)
	}
	if code, _ := send(h, http.MethodPut, "/pr/a/b/3/drafts/c1", url.Values{"text": {""}, "path": {"a.go"}, "line": {"7"}}); code != http.StatusOK || len(commentDrafts(q)) != 1 {
		t.Errorf("discard = %d, drafts %+v", code, commentDrafts(q))
	}
}

// An unflagged draft keeps its source text and can still be edited and included.
func TestUnflaggedComment(t *testing.T) {
	h, q, _ := reloadSetup(t)
	pageOf(t, h)
	form := url.Values{"path": {"b.go"}, "line": {"3"}}
	form.Set("include", "0")
	if code, body := send(h, http.MethodPost, "/pr/a/b/3/drafts/c1/include", form); code != http.StatusOK || !strings.Contains(body, "Submit review (0)") {
		t.Errorf("include = %d %s", code, body)
	}
	form.Set("text", "changed")
	if code, _ := send(h, http.MethodPut, "/pr/a/b/3/drafts/c1", form); code != http.StatusOK || q.pending.get("a/b#3").Drafts[0].Text != "changed" {
		t.Errorf("edit = %d", code)
	}
}
