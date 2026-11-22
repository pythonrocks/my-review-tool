package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDraftState(t *testing.T) {
	for _, c := range []struct {
		d    pendingDraft
		want string
	}{
		{pendingDraft{}, "Suggestion"},
		{pendingDraft{Edited: true}, "Edited"},
		{pendingDraft{InReview: true}, "Draft"},
		{pendingDraft{InReview: true, Edited: true}, "Draft edited"},
		{pendingDraft{PostedURL: "u"}, "Posted"},
		{pendingDraft{PostedURL: "u", InReview: true, Edited: true}, "Posted"},
	} {
		if got := draftState(c.d); got != c.want {
			t.Errorf("draftState(%+v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestPendingPersistence(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(s.dir, "pending")); err != nil || fi.Mode().Perm() != 0700 {
		t.Fatalf("pending dir: %v %v", fi, err)
	}
	ps := newPendingStore(s)
	want := pendingPR{Rev: 1, Checked: []pendingCheck{{"f0", "a.go", 3, "d"}},
		Drafts: []pendingDraft{{ID: "p1", File: "b.go", Line: 2, Commit: "h1", SourceText: "r", Text: "my", Edited: true, InReview: true}}}
	if _, err := ps.update("o/r#1", func(p *pendingPR) error {
		p.Checked, p.Drafts = want.Checked, want.Drafts
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := newPendingStore(s).get("o/r#1"); !reflect.DeepEqual(got, want) {
		t.Errorf("reloaded %+v, want %+v", got, want)
	}
	if err := os.WriteFile(ps.file("o/r#2"), []byte("{bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := newPendingStore(s).get("o/r#2"); !reflect.DeepEqual(got, pendingPR{}) {
		t.Errorf("bad JSON loaded as %+v, want empty", got)
	}
}

func TestPendingPruneAndRerun(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	q := newReviewQueue(0, workflow{}, s)
	for _, k := range []string{"a/b#1", "a/b#2", "c/d#1"} {
		if _, err := q.pending.update(k, func(p *pendingPR) error { p.Checked = []pendingCheck{{ID: "f0"}}; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(k string) bool { _, err := os.Stat(q.pending.file(k)); return err == nil }
	q.store.remove("a/b#1") // what enqueue does on Re-run and new commits
	q.sync(nil)             // the forget path
	if !exists("a/b#1") {
		t.Fatal("pending file removed with the result")
	}
	q.prune([]string{"a/b", "c/d"}, pollSnapshot{PRs: []pullRequest{{Repo: "a/b", Number: 2}}, Errors: map[string]string{"c/d": "boom"}})
	if exists("a/b#1") || !exists("a/b#2") || !exists("c/d#1") {
		t.Errorf("after prune: a/b#1 %v, a/b#2 %v, c/d#1 %v; want only a/b#1 gone", exists("a/b#1"), exists("a/b#2"), exists("c/d#1"))
	}
	if p := q.pending.get("a/b#1"); p.Rev != 0 || len(p.Checked) != 0 {
		t.Errorf("pruned PR still cached: %+v", p)
	}
}

// pendingSetup is doneSetup with the pending dir created; a/b#3 is done at abc123.
func pendingSetup(t *testing.T) (http.Handler, *reviewQueue, *job) {
	cfg, p, q, j := doneSetup(t)
	if err := os.MkdirAll(q.pending.dir(), 0700); err != nil {
		t.Fatal(err)
	}
	return fresh(q, newHandler(newConfigHolder(cfg, "myreview.json"), p, q, nil, notApproved, noFiles, noContent, noThreads, noResolve)), q, j
}

// fresh sends the current result's id as results with every write that has none, as details.js does on a
// page showing that result, so tests of other rules pass the stale-page guard. It also marks the pending review
// of every done result as reconciled with it, so a page load does not reconcile (tested on its own).
func fresh(q *reviewQueue, h http.Handler) http.Handler {
	for key, st := range q.statuses() {
		if st.Status == statusDone {
			q.pending.mu.Lock()
			p := q.pending.load(key)
			p.Result = resultID(st)
			q.pending.prs[key] = p
			q.pending.mu.Unlock()
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if _, sent := r.Form["results"]; !sent && r.Method != http.MethodGet {
			if parts := strings.Split(r.URL.Path, "/"); len(parts) > 4 && parts[1] == "pr" {
				id := resultID(q.statuses()[parts[2]+"/"+parts[3]+"#"+parts[4]])
				r.Form.Set("results", id)
				r.PostForm.Set("results", id)
			}
		}
		h.ServeHTTP(w, r)
	})
}

func send(h http.Handler, method, path string, form url.Values) (int, string) {
	code, body, _ := do(h, localReq(method, path, form.Encode()))
	return code, body
}

func pageOf(t *testing.T, h http.Handler) string {
	t.Helper()
	code, body := send(h, http.MethodGet, "/pr/a/b/3", nil)
	if code != http.StatusOK {
		t.Fatalf("details = %d", code)
	}
	return body
}

// noError is the empty error region every pending change answers with on success.
const noError = `<div id="pending-error" role="alert" hx-swap-oob="true"></div>`

func TestCheckedEndpoint(t *testing.T) {
	h, q, _ := pendingSetup(t)
	code, body := send(h, http.MethodPost, "/pr/a/b/3/points/f4/checked", url.Values{"checked": {"1"}})
	if code != http.StatusOK || body != `<input type="checkbox" class="point-check" id="chk-f4" aria-label="Mark point 1 as checked" checked hx-post="/pr/a/b/3/points/f4/checked" hx-vals='{"checked": "0"}' hx-swap="outerHTML">`+
		`<span class="progress" id="progress" hx-swap-oob="true">1 of 8 checked</span>`+noError {
		t.Fatalf("check = %d %q", code, body)
	}
	send(h, http.MethodPost, "/pr/a/b/3/points/p0/checked", url.Values{"checked": {"1"}})
	send(h, http.MethodPost, "/pr/a/b/3/points/p0/checked", url.Values{"checked": {"1"}}) // twice: still one mark
	mustContain(t, pageOf(t, h), `id="chk-f4" aria-label="Mark point 1 as checked" checked`, `id="chk-p0" aria-label="Mark point 7 as checked" checked`, ">2 of 8 checked<")
	if code, body := send(h, http.MethodPost, "/pr/a/b/3/points/f4/checked", url.Values{"checked": {"0"}}); code != http.StatusOK || !strings.Contains(body, ">1 of 8 checked<") {
		t.Errorf("uncheck = %d %q", code, body)
	}
	if p := q.pending.get("a/b#3"); !reflect.DeepEqual(p.Checked, []pendingCheck{{"p0", "z.go", 0, "second"}}) || p.Rev != 5 { // 3 changes and the clock set by the first page GET
		t.Errorf("stored %+v", p)
	}
	for _, v := range []url.Values{{}, {"checked": {"yes"}}, {"checked": {"true"}}} {
		if code, _ := send(h, http.MethodPost, "/pr/a/b/3/points/f4/checked", v); code != http.StatusBadRequest {
			t.Errorf("checked %v = %d, want 400", v, code)
		}
	}
}

func TestDraftEndpoints(t *testing.T) {
	h, q, _ := pendingSetup(t)
	draft := func(id string) pendingDraft {
		t.Helper()
		for _, d := range q.pending.get("a/b#3").Drafts {
			if d.ID == id {
				return d
			}
		}
		t.Fatalf("no draft %s", id)
		return pendingDraft{}
	}
	// the suggested text unchanged: a draft, not edited
	code, body := send(h, http.MethodPut, "/pr/a/b/3/drafts/p2", url.Values{"text": {"Why first?"}})
	if code != http.StatusOK || body != `<span class="editor-status" id="es-p2">Saved</span><span class="state state-suggestion" id="state-p2" hx-swap-oob="true">Suggestion</span>`+noError {
		t.Fatalf("PUT = %d %q", code, body)
	}
	if d := draft("p2"); d != (pendingDraft{ID: "p2", File: "dir/a b#?%.go", Line: 7, Commit: "abc123", SourceText: "first", Text: "Why first?"}) {
		t.Errorf("draft %+v", d)
	}
	// a changed text sets edited for good
	if code, body := send(h, http.MethodPut, "/pr/a/b/3/drafts/f4", url.Values{"text": {"mine <b>"}}); code != http.StatusOK || !strings.Contains(body, ">Edited</span>") {
		t.Fatalf("PUT = %d %q", code, body)
	}
	send(h, http.MethodPut, "/pr/a/b/3/drafts/f4", url.Values{"text": {"Please fix crit."}})
	if d := draft("f4"); !d.Edited || d.Text != "Please fix crit." || d.SourceText != "crit\nsecond line" {
		t.Errorf("draft %+v", d)
	}
	// include and remove
	code, body = send(h, http.MethodPost, "/pr/a/b/3/drafts/f4/include", url.Values{"include": {"1"}})
	mustContain(t, body, `<div class="editor-actions" id="ea-f4"><button class="btn-sm btn-quiet" type="button" hx-post="/pr/a/b/3/drafts/f4/include" hx-vals='{"include": "0"}'`, ">Remove from review</button>",
		`<span class="state state-draft" id="state-f4" hx-swap-oob="true">Draft edited</span>`, `<button id="submit-count" hx-swap-oob="true">Submit review (1)</button>`)
	if code != http.StatusOK {
		t.Fatalf("include = %d", code)
	}
	// including a row without a draft makes one with the suggested comment
	_, body = send(h, http.MethodPost, "/pr/a/b/3/drafts/f5/include", url.Values{"include": {"1"}})
	mustContain(t, body, ">Draft</span>", "Submit review (2)")
	if d := draft("f5"); !d.InReview || d.Edited || d.Text != "a9" {
		t.Errorf("draft %+v", d)
	}
	pg := pageOf(t, h)
	mustContain(t, pg, "Submit review (2)", `id="state-f4" hx-swap-oob="true">Draft edited</span>`, "<textarea id=\"ta-f4\" name=\"text\" rows=\"6\" hx-put=\"/pr/a/b/3/drafts/f4\" hx-trigger=\"input changed delay:800ms\" hx-target=\"#es-f4\" hx-swap=\"outerHTML\">\nPlease fix crit.</textarea>")
	_, body = send(h, http.MethodPost, "/pr/a/b/3/drafts/f4/include", url.Values{"include": {"0"}})
	mustContain(t, body, ">Add to review</button>", ">Edited</span>", "Submit review (1)")

	// refusals leave the stored state alone
	rev := q.pending.get("a/b#3").Rev
	send(h, http.MethodPut, "/pr/a/b/3/drafts/f3", url.Values{"text": {" \n"}}) // empty text is fine while typing
	for _, c := range []struct {
		method, path string
		form         url.Values
		code         int
		msg          string
	}{
		{http.MethodPost, "/pr/a/b/3/drafts/f3/include", url.Values{"include": {"1"}}, 400, "comment text is empty"},
		{http.MethodPost, "/pr/a/b/3/drafts/f3/include", url.Values{"include": {"on"}}, 400, "include must be 1 or 0"},
		{http.MethodPost, "/pr/a/b/3/drafts/f3/include", url.Values{}, 400, "include must be 1 or 0"},
		{http.MethodPut, "/pr/a/b/3/drafts/f3", url.Values{"text": {strings.Repeat("é", maxCommentLen+1)}}, 400, "comment text is over 65536 characters"},
	} {
		if code, body := send(h, c.method, c.path, c.form); code != c.code || !strings.Contains(body, `<div id="pending-error" role="alert" hx-swap-oob="true"><p class="alert alert-error"><span>`+c.msg+`</span></p></div>`) {
			t.Errorf("%s %s %v = %d %q, want %d %q", c.method, c.path, c.form, code, body, c.code, c.msg)
		}
	}
	if code, _ := send(h, http.MethodPut, "/pr/a/b/3/drafts/f3", url.Values{"text": {strings.Repeat("é", maxCommentLen)}}); code != http.StatusOK {
		t.Errorf("PUT at the limit = %d", code)
	}
	if got := q.pending.get("a/b#3").Rev; got != rev+2 {
		t.Errorf("rev %d, want %d", got, rev+2)
	}
}

func TestDraftPosted(t *testing.T) {
	h, q, j := pendingSetup(t)
	j.Findings[4].PostedURL = "https://github.com/a/b/pull/3#discussion_r1" // f4, posted by v1
	if _, err := q.pending.update("a/b#3", func(p *pendingPR) error {
		p.Drafts = []pendingDraft{{ID: "p2", File: "dir/a b#?%.go", Line: 7, Commit: "abc123", SourceText: "first", Text: "t", PostedURL: "https://github.com/a/b/pull/3#pullrequestreview-1"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := q.pending.get("a/b#3")
	for _, id := range []string{"f4", "p2"} {
		if code, _ := send(h, http.MethodPut, "/pr/a/b/3/drafts/"+id, url.Values{"text": {"x"}}); code != http.StatusConflict {
			t.Errorf("PUT %s = %d, want 409", id, code)
		}
		if code, _ := send(h, http.MethodPost, "/pr/a/b/3/drafts/"+id+"/include", url.Values{"include": {"1"}}); code != http.StatusConflict {
			t.Errorf("include %s = %d, want 409", id, code)
		}
	}
	if after := q.pending.get("a/b#3"); !reflect.DeepEqual(after, before) {
		t.Errorf("stored %+v, want %+v", after, before)
	}
	mustContain(t, pageOf(t, h), `<span class="state state-posted" id="state-f4" hx-swap-oob="true">Posted</span>`, `<textarea id="ta-f4" name="text" rows="6" readonly>`,
		`<div class="editor-actions" id="ea-f4"><a class="posted" href="https://github.com/a/b/pull/3#discussion_r1">Posted</a></div>`,
		`<a class="posted" href="https://github.com/a/b/pull/3#pullrequestreview-1">Posted</a>`, "Submit review (0)")
}

func TestPendingNotFound(t *testing.T) {
	h, _, _ := pendingSetup(t)
	// not on the dashboard, queued, failed, not known, bad number, other repo; then bad ids on the done PR
	var rows []string
	for _, base := range []string{"/pr/a/b/9", "/pr/a/b/1", "/pr/a/b/4", "/pr/a/b/5", "/pr/a/b/x", "/pr/a/c/3"} {
		rows = append(rows, base+"/f0")
	}
	for _, id := range []string{"x1", "f999", "p3", "p-1", "f", "f01"} {
		rows = append(rows, "/pr/a/b/3/"+id)
	}
	for _, row := range rows {
		i := strings.LastIndex(row, "/")
		base, id := row[:i], row[i+1:]
		for _, c := range []struct{ method, path, form string }{
			{http.MethodPost, base + "/points/" + id + "/checked", "checked=1"},
			{http.MethodPut, base + "/drafts/" + id, "text=x"},
			{http.MethodPost, base + "/drafts/" + id + "/include", "include=1"},
		} {
			if code, _, _ := do(h, localReq(c.method, c.path, c.form)); code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", c.method, c.path, code)
			}
		}
	}
}

func TestPendingMatching(t *testing.T) {
	h, q, _ := pendingSetup(t)
	stale := pendingPR{Rev: 7, Result: resultID(q.statuses()["a/b#3"]), // already reconciled: only matching applies
		Checked: []pendingCheck{{"f4", "dir/a b#?%.go", 7, "old text"}, {"f5", "a.go", 8, "a9"}},
		Drafts: []pendingDraft{
			{ID: "f4", File: "dir/a b#?%.go", Line: 7, Commit: "old", SourceText: "crit\nsecond line", Text: "stale", InReview: true, Edited: true},
			{ID: "f5", File: "a.go", Line: 9, Commit: "abc123", SourceText: "other", Text: "stale", InReview: true},
		}}
	if _, err := q.pending.update("a/b#3", func(p *pendingPR) error { *p = stale; return nil }); err != nil {
		t.Fatal(err)
	}
	stale.Rev++
	pg := pageOf(t, h)
	mustContain(t, pg, ">0 of 8 checked<", "Submit review (0)", `id="state-f4" hx-swap-oob="true">Suggestion</span>`, "\nPlease fix crit.</textarea>")
	if strings.Contains(pg, "stale") {
		t.Error("a non-matching draft is shown")
	}
	// a change elsewhere keeps the non-matching records as they are
	send(h, http.MethodPost, "/pr/a/b/3/points/f0/checked", url.Values{"checked": {"1"}})
	send(h, http.MethodPost, "/pr/a/b/3/drafts/f4/include", url.Values{"include": {"1"}})
	got := newPendingStore(q.store).get("a/b#3")
	if !reflect.DeepEqual(got.Checked[:2], stale.Checked) || !reflect.DeepEqual(got.Drafts[:2], stale.Drafts) || len(got.Checked) != 3 || len(got.Drafts) != 3 {
		t.Errorf("stored %+v, want the stale records kept", got)
	}
	if d := got.Drafts[2]; d.Commit != "abc123" || d.Text != "Please fix crit." || !d.InReview {
		t.Errorf("new draft %+v", d)
	}
}

func TestPendingWriteError(t *testing.T) {
	h, q, _ := pendingSetup(t)
	send(h, http.MethodPost, "/pr/a/b/3/points/f4/checked", url.Values{"checked": {"1"}})
	before := q.pending.get("a/b#3")
	if err := os.RemoveAll(q.pending.dir()); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		method, path string
		form         url.Values
	}{
		{http.MethodPost, "/pr/a/b/3/points/f4/checked", url.Values{"checked": {"0"}}},
		{http.MethodPost, "/pr/a/b/3/points/f5/checked", url.Values{"checked": {"1"}}},
		{http.MethodPut, "/pr/a/b/3/drafts/f4", url.Values{"text": {"x"}}},
		{http.MethodPost, "/pr/a/b/3/drafts/f4/include", url.Values{"include": {"1"}}},
	} {
		if code, _ := send(h, c.method, c.path, c.form); code != http.StatusInternalServerError {
			t.Errorf("%s %s = %d, want 500", c.method, c.path, code)
		}
	}
	if after := q.pending.get("a/b#3"); !reflect.DeepEqual(after, before) {
		t.Errorf("state after failed writes %+v, want %+v", after, before)
	}
	// the answer carries the stored state, so the page does not show the failed change
	_, body := send(h, http.MethodPost, "/pr/a/b/3/points/f4/checked", url.Values{"checked": {"0"}})
	mustContain(t, body, `id="chk-f4" aria-label="Mark point 1 as checked" checked hx-post="/pr/a/b/3/points/f4/checked" hx-vals='{"checked": "0"}'`,
		">1 of 8 checked</span>", `<div id="pending-error" role="alert" hx-swap-oob="true"><p class="alert alert-error"><span>saving pending review: `)
	_, body = send(h, http.MethodPut, "/pr/a/b/3/drafts/f4", url.Values{"text": {"x"}})
	mustContain(t, body, `<span class="editor-status" id="es-f4">Not saved</span>`, ">Suggestion</span>")
	mustContain(t, pageOf(t, h), ">1 of 8 checked<", "Submit review (0)")
}

func TestPendingReplies(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	ps := newPendingStore(s)
	r := pendingReply{Thread: "T_1", Path: "a.go", Line: 3, Text: "agree"}
	first, err := ps.update("o/r#1", func(p *pendingPR) error { p.Replies = append(p.Replies, r); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ps.update("o/r#1", func(p *pendingPR) error { p.Replies[0].Text = "changed"; return nil }); err != nil {
		t.Fatal(err)
	}
	if first.Replies[0] != r {
		t.Errorf("update changed an earlier state's replies: %+v", first.Replies)
	}
	got := newPendingStore(s).get("o/r#1")
	if !reflect.DeepEqual(got.Replies, []pendingReply{{"T_1", "a.go", 3, "changed"}}) {
		t.Errorf("reloaded replies %+v", got.Replies)
	}
	st := jobStatus{Status: statusDone, HeadSHA: "h2"}
	if p := reconcile(got, st, nil, func(pendingDraft) (int, bool) { return 0, false }); !reflect.DeepEqual(p.Replies, got.Replies) || p.Result == got.Result {
		t.Errorf("reconcile: replies %+v, result %q", p.Replies, p.Result)
	}
}

// The review speed clock: the full details GET starts it once per result; partials and the post-submit redirect never.
func TestReviewClock(t *testing.T) {
	h, q, j := pendingSetup(t)
	for _, path := range []string{"/pr/a/b/3?submitted=COMMENT&n=0", "/pr/a/b/3/status?results=x&head=abc123",
		"/pr/a/b/3/points/f4", "/pr/a/b/3/review/summary?event=COMMENT"} {
		send(h, http.MethodGet, path, nil)
		if p := q.pending.get("a/b#3"); !p.OpenedAt.IsZero() || p.OpenedResult != "" || p.Rev != 0 {
			t.Errorf("GET %s set the clock: %+v", path, p)
		}
	}
	start := time.Now()
	pageOf(t, h)
	first := q.pending.get("a/b#3")
	if first.OpenedAt.Before(start) || first.OpenedAt.Location() != time.UTC || first.OpenedResult != resultID(q.statuses()["a/b#3"]) || first.Rev != 1 {
		t.Fatalf("first GET: %+v", first)
	}
	pageOf(t, h)
	if p := q.pending.get("a/b#3"); p.Rev != first.Rev || !p.OpenedAt.Equal(first.OpenedAt) {
		t.Errorf("second GET wrote: %+v", p)
	}
	if p := newPendingStore(q.store).get("a/b#3"); !p.OpenedAt.Equal(first.OpenedAt) || p.OpenedResult != first.OpenedResult {
		t.Errorf("after reopening the store: %+v", p)
	}
	j.Findings[0].Description = "a new result"
	pageOf(t, h)
	if p := q.pending.get("a/b#3"); !p.OpenedAt.After(first.OpenedAt) || p.OpenedResult != resultID(q.statuses()["a/b#3"]) || p.OpenedResult == first.OpenedResult {
		t.Errorf("GET after a new result: %+v, first %+v", p, first)
	}
}

func TestReviewClockOldFile(t *testing.T) {
	_, q, _ := pendingSetup(t)
	if err := os.WriteFile(q.pending.file("a/b#3"), []byte(`{"rev":3,"checked":[],"drafts":[],"result":"r"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if p := newPendingStore(q.store).get("a/b#3"); p.Rev != 3 || p.Result != "r" || !p.OpenedAt.IsZero() || p.OpenedResult != "" {
		t.Errorf("old file = %+v", p)
	}
}

func TestReviewClockSubmit(t *testing.T) {
	defer func(d time.Duration) { reviewTimeout = d }(reviewTimeout)
	reviewTimeout = 10 * time.Millisecond
	h, q, _, fs := submitSetup(t, "")
	include(t, h, "f4")
	pageOf(t, h)
	opened := q.pending.get("a/b#3")
	fs.fn = func(ctx context.Context) (string, error) { <-ctx.Done(); return "", errors.New("gh timed out") }
	if code, _, _ := postReview(h, reviewForm(q, "COMMENT")); code != http.StatusBadGateway {
		t.Fatalf("timeout = %d", code)
	}
	if p := q.pending.get("a/b#3"); !p.OpenedAt.Equal(opened.OpenedAt) || p.OpenedResult != opened.OpenedResult {
		t.Errorf("timeout cleared the clock: %+v", p)
	}
	fs.fn = nil
	if code, body, _ := postReview(h, reviewForm(q, "COMMENT")); code != http.StatusOK {
		t.Fatalf("submit = %d %q", code, body)
	}
	if p := q.pending.get("a/b#3"); !p.OpenedAt.IsZero() || p.OpenedResult != "" {
		t.Errorf("submit kept the clock: %+v", p)
	}
}
