package main

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSubmitWithArgsAndInput(t *testing.T) {
	type call struct {
		input string
		args  []string
	}
	var calls []call
	results := []error{}
	run := func(_ context.Context, input []byte, args ...string) ([]byte, error) {
		calls = append(calls, call{string(input), args})
		if len(results) > 0 {
			err := results[0]
			results = results[1:]
			if err != nil {
				return nil, err
			}
		}
		return []byte("https://github.com/o/r/pull/1#pullrequestreview-9\n"), nil
	}
	submit := submitWith(run)
	drafts := []pendingDraft{{File: "a.go", Line: 42, Text: "fix this"}, {File: "b.go", Line: 0, Text: "whole file"}, {File: "c.go", Line: 3, Text: "c3"}}
	wantArgs := []string{"api", "--method", "POST", "repos/o/r/pulls/1/reviews", "--input", "-", "--jq", ".html_url"}
	for _, event := range []string{"APPROVE", "REQUEST_CHANGES", "COMMENT"} {
		calls = nil
		u, err := submit(context.Background(), "o/r", 1, newReview("sha1", event, "sum", drafts))
		want := `{"commit_id":"sha1","event":"` + event + `","body":"sum\n\n**` + "`b.go`" + `**\n\nwhole file","comments":[` +
			`{"path":"a.go","line":42,"side":"RIGHT","body":"fix this"},{"path":"c.go","line":3,"side":"RIGHT","body":"c3"}]}`
		if err != nil || u != "https://github.com/o/r/pull/1#pullrequestreview-9" || len(calls) != 1 ||
			calls[0].input != want || !reflect.DeepEqual(calls[0].args, wantArgs) {
			t.Fatalf("%s: %q %v, calls %+v\nwant input %s", event, u, err, calls, want)
		}
	}

	// HTTP 422: one retry with every comment in the body
	calls, results = nil, []error{errors.New("gh: Unprocessable Entity (HTTP 422)")}
	u, err := submit(context.Background(), "o/r", 1, newReview("sha1", "COMMENT", "", drafts))
	want := `{"commit_id":"sha1","event":"COMMENT","body":"**` + "`b.go`" + `**\n\nwhole file\n\n` + "`a.go:42`" + `\n\nfix this\n\n` + "`c.go:3`" + `\n\nc3"}`
	if err != nil || u == "" || len(calls) != 2 || calls[1].input != want || !reflect.DeepEqual(calls[1].args, wantArgs) {
		t.Fatalf("422 retry: %q %v, calls %+v\nwant input %s", u, err, calls, want)
	}
	// a second 422 is the error; other errors are not retried
	for _, errs := range [][]error{{errors.New("x (HTTP 422)"), errors.New("y (HTTP 422)")}, {errors.New("boom (HTTP 502)")}} {
		calls, results = nil, errs
		if _, err := submit(context.Background(), "o/r", 1, newReview("sha1", "COMMENT", "", drafts)); err == nil || len(calls) != len(errs) {
			t.Errorf("errors %v: err %v after %d calls", errs, err, len(calls))
		}
	}
}

// fakeSubmit records the reviews it gets; fn, when set, decides the outcome.
type fakeSubmit struct {
	mu    sync.Mutex
	calls []reviewPayload
	fn    func(ctx context.Context) (string, error)
}

const reviewURL = "https://github.com/a/b/pull/3#pullrequestreview-1"

func (f *fakeSubmit) submit(ctx context.Context, repo string, number int, rv reviewPayload) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, rv)
	fn := f.fn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return reviewURL, nil
}

func (f *fakeSubmit) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// submitSetup is pendingSetup with a fake submit; a non-empty login is the poll login and the author of every PR.
func submitSetup(t *testing.T, login string) (http.Handler, *reviewQueue, *job, *fakeSubmit) {
	cfg, p, q, j := doneSetup(t)
	if err := os.MkdirAll(q.pending.dir(), 0700); err != nil {
		t.Fatal(err)
	}
	if login != "" {
		prs := p.snapshot().PRs
		for i := range prs {
			prs[i].Author = strings.ToUpper(login) // case-insensitive
		}
		p = newPoller(cfg.Repositories, (&fakeFetch{prs: map[string][]pullRequest{"a/b": prs}}).fetch, func(context.Context) (string, error) { return login, nil })
		p.poll(context.Background())
	}
	fs := &fakeSubmit{}
	return fresh(q, newHandler(newConfigHolder(cfg, "myreview.json"), p, q, fs.submit, notApproved, noFiles, noContent, noThreads, noResolve)), q, j, fs
}

// reviewForm is the form the dialog would submit for a/b#3 now, with key/value overrides.
func reviewForm(q *reviewQueue, event string, extra ...string) url.Values {
	st, known := q.statuses()["a/b#3"]
	v := url.Values{"event": {event}, "rev": {strconv.Itoa(q.pending.get("a/b#3").Rev)}, "head": {"abc123"},
		"expect": {approveHash(confirmReasons(pullRequest{HeadSHA: "abc123"}, st, known))}}
	for i := 0; i+1 < len(extra); i += 2 {
		v.Set(extra[i], extra[i+1])
	}
	return v
}

// postReview posts v to a/b#3; the body is the error of a re-rendered dialog.
func postReview(h http.Handler, v url.Values) (int, string, string) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localReq(http.MethodPost, "/pr/a/b/3/review", v.Encode()))
	return rec.Code, errorOf(rec.Body.String()), rec.Header().Get("HX-Redirect")
}

func include(t *testing.T, h http.Handler, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if code, body := send(h, http.MethodPost, "/pr/a/b/3/drafts/"+id+"/include", url.Values{"include": {"1"}}); code != http.StatusOK {
			t.Fatalf("include %s = %d %q", id, code, body)
		}
	}
}

func TestReviewSubmit(t *testing.T) {
	h, q, _, fs := submitSetup(t, "")
	include(t, h, "f1", "f4") // b.go line 0, then the Critical finding (row 1)
	before := q.pending.get("a/b#3")
	form := reviewForm(q, "COMMENT", "body", "sum")
	code, body, redirect := postReview(h, form)
	if code != http.StatusOK || redirect != "/pr/a/b/3?submitted=COMMENT&n=2" {
		t.Fatalf("submit = %d %q %q", code, body, redirect)
	}
	want := reviewPayload{CommitID: "abc123", Event: "COMMENT", Body: "sum\n\n**`b.go`**\n\nwhole file",
		Comments: []reviewComment{{Path: "dir/a b#?%.go", Line: 7, Side: "RIGHT", Body: "Please fix crit."}}}
	if !reflect.DeepEqual(fs.calls, []reviewPayload{want}) {
		t.Errorf("submitted %+v, want %+v", fs.calls, want)
	}
	after := q.pending.get("a/b#3")
	if after.Rev != before.Rev+1 {
		t.Errorf("rev %d, want %d", after.Rev, before.Rev+1)
	}
	for _, d := range after.Drafts {
		if d.PostedURL != reviewURL || d.InReview {
			t.Errorf("draft %+v not marked posted", d)
		}
	}
	mustContain(t, pageOf(t, h), `href="`+reviewURL+`"`)
	// the same dialog cannot post again
	if code, body, _ := postReview(h, form); code != http.StatusConflict || body != "your drafts changed since the dialog opened; open it again" || fs.count() != 1 {
		t.Errorf("resubmit = %d %q, %d calls", code, body, fs.count())
	}
	// posted drafts are not submitted again
	if code, _, redirect := postReview(h, reviewForm(q, "APPROVE")); code != http.StatusOK || redirect != "/pr/a/b/3?submitted=APPROVE&n=0" ||
		!reflect.DeepEqual(fs.calls[1], reviewPayload{CommitID: "abc123", Event: "APPROVE"}) {
		t.Errorf("approve = %d %q, %+v", code, redirect, fs.calls)
	}
}

func TestReviewCommit(t *testing.T) {
	for _, c := range []struct {
		name   string
		status string
		want   string
	}{{"done, outdated", statusDone, "rev1"}, {"queued", statusQueued, "abc123"}} {
		h, q, j, fs := submitSetup(t, "")
		j.HeadSHA, j.Status = "rev1", c.status
		if code, body, _ := postReview(h, reviewForm(q, "COMMENT", "body", "s")); code != http.StatusOK || len(fs.calls) != 1 || fs.calls[0].CommitID != c.want {
			t.Errorf("%s: %d %q, calls %+v, want commit %s", c.name, code, body, fs.calls, c.want)
		}
	}
}

func TestReviewRefusals(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(j *job)
		form  func(q *reviewQueue) url.Values
		code  int
		body  string
	}{
		{"bad event", nil, func(q *reviewQueue) url.Values { return reviewForm(q, "MERGE", "body", "s") }, 400, "event must be APPROVE, REQUEST_CHANGES or COMMENT"},
		{"no event", nil, func(q *reviewQueue) url.Values { return reviewForm(q, "", "body", "s") }, 400, "event must be APPROVE, REQUEST_CHANGES or COMMENT"},
		{"long body", nil, func(q *reviewQueue) url.Values {
			return reviewForm(q, "COMMENT", "body", strings.Repeat("é", maxCommentLen+1))
		}, 400, "summary is over 65536 characters"},
		{"head moved", nil, func(q *reviewQueue) url.Values { return reviewForm(q, "COMMENT", "body", "s", "head", "old") }, 409,
			"the pull request has new commits since you loaded the page (now at abc123). Check the page and submit again."},
		{"stale rev", nil, func(q *reviewQueue) url.Values { return reviewForm(q, "COMMENT", "body", "s", "rev", "7") }, 409, "your drafts changed since the dialog opened; open it again"},
		{"no rev", nil, func(q *reviewQueue) url.Values { return reviewForm(q, "COMMENT", "body", "s", "rev", "") }, 409, "your drafts changed since the dialog opened; open it again"},
		{"approve, stale expect", nil, func(q *reviewQueue) url.Values { return reviewForm(q, "APPROVE", "expect", "x") }, 409,
			"the review changed since you loaded the page. Check the page and approve again."},
		{"approve outdated, no confirm", func(j *job) { j.HeadSHA = "rev1" }, func(q *reviewQueue) url.Values { return reviewForm(q, "APPROVE") }, 409, "confirm the approval first."},
		{"approve failed, no confirm", func(j *job) { j.Status = statusFailed }, func(q *reviewQueue) url.Values { return reviewForm(q, "APPROVE") }, 409, "confirm the approval first."},
		{"request changes, nothing", nil, func(q *reviewQueue) url.Values { return reviewForm(q, "REQUEST_CHANGES", "body", " \n") }, 409, "Add a comment or a summary to request changes"},
		{"comment, nothing", nil, func(q *reviewQueue) url.Values { return reviewForm(q, "COMMENT") }, 409, "Add a comment or a summary to comment"},
	} {
		h, q, j, fs := submitSetup(t, "")
		got := captureReviews(q)
		if !strings.HasSuffix(c.name, "nothing") {
			include(t, h, "f1") // a stored draft, so refusals can be shown to keep it
		}
		if c.setup != nil {
			c.setup(j)
		}
		before := q.pending.get("a/b#3")
		if code, body, _ := postReview(h, c.form(q)); code != c.code || body != c.body {
			t.Errorf("%s = %d %q, want %d %q", c.name, code, body, c.code, c.body)
		}
		if fs.count() != 0 || !reflect.DeepEqual(q.pending.get("a/b#3"), before) || len(*got) != 0 {
			t.Errorf("%s: %d calls, %d records, pending changed", c.name, fs.count(), len(*got))
		}
	}
	h, _, _, _ := submitSetup(t, "")
	if code, _, _ := do(h, localReq(http.MethodPost, "/pr/a/b/9/review", "event=COMMENT")); code != http.StatusNotFound {
		t.Errorf("unknown PR = %d, want 404", code)
	}
}

func TestReviewApproveRules(t *testing.T) {
	// human review required on a current result: the human-review lines need no confirmation
	h, q, _, fs := submitSetup(t, "")
	if code, body, _ := postReview(h, reviewForm(q, "APPROVE")); code != http.StatusOK || fs.count() != 1 {
		t.Errorf("approve human review = %d %q", code, body)
	}
	// outdated: confirmed, the review goes to the reviewed commit
	h, q, j, fs := submitSetup(t, "")
	j.HeadSHA = "rev1"
	if code, body, _ := postReview(h, reviewForm(q, "APPROVE", "confirm", "1")); code != http.StatusOK || fs.count() != 1 || fs.calls[0].CommitID != "rev1" {
		t.Errorf("approve outdated confirmed = %d %q %+v", code, body, fs.calls)
	}
}

func TestReviewOwnPR(t *testing.T) {
	h, q, _, fs := submitSetup(t, "me")
	for _, event := range []string{"APPROVE", "REQUEST_CHANGES"} {
		if code, body, _ := postReview(h, reviewForm(q, event, "body", "s")); code != http.StatusConflict ||
			body != "GitHub does not allow approving or requesting changes on your own pull request" {
			t.Errorf("own %s = %d %q", event, code, body)
		}
	}
	if fs.count() != 0 {
		t.Fatalf("%d calls", fs.count())
	}
	if code, body, _ := postReview(h, reviewForm(q, "COMMENT", "body", "s")); code != http.StatusOK || fs.count() != 1 {
		t.Errorf("own COMMENT = %d %q", code, body)
	}
}

func TestReviewFailureAndRetry(t *testing.T) {
	h, q, _, fs := submitSetup(t, "")
	got := captureReviews(q)
	include(t, h, "f4")
	fs.fn = func(context.Context) (string, error) { return "", errors.New("gh: Bad credentials (HTTP 401)") }
	before := q.pending.get("a/b#3")
	form := reviewForm(q, "REQUEST_CHANGES")
	if code, body, _ := postReview(h, form); code != http.StatusBadGateway || body != "gh: Bad credentials (HTTP 401)" {
		t.Fatalf("failed submit = %d %q", code, body)
	}
	if !reflect.DeepEqual(q.pending.get("a/b#3"), before) || len(*got) != 0 {
		t.Fatalf("pending changed or %d records after a failed submit", len(*got))
	}
	fs.fn = nil
	if code, body, _ := postReview(h, form); code != http.StatusOK || fs.count() != 2 || len(*got) != 1 || (*got)[0].Uncertain {
		t.Fatalf("retry = %d %q, %d calls, records %+v", code, body, fs.count(), *got)
	}
	if code, _, _ := postReview(h, form); code != http.StatusConflict || fs.count() != 2 {
		t.Errorf("third = %d, %d calls; want 409 and no call", code, fs.count())
	}
}

func TestReviewTimeout(t *testing.T) {
	defer func(d time.Duration) { reviewTimeout = d }(reviewTimeout)
	reviewTimeout = 10 * time.Millisecond
	h, q, _, fs := submitSetup(t, "")
	got := captureReviews(q)
	include(t, h, "f4")
	fs.fn = func(ctx context.Context) (string, error) { <-ctx.Done(); return "", errors.New("gh timed out") }
	before := q.pending.get("a/b#3")
	form := reviewForm(q, "COMMENT")
	if code, body, _ := postReview(h, form); code != http.StatusBadGateway || body != "gh timed out; the review may have been posted, check GitHub" {
		t.Fatalf("timeout = %d %q", code, body)
	}
	after := q.pending.get("a/b#3")
	if after.Rev != before.Rev+1 || !reflect.DeepEqual(after.Drafts, before.Drafts) {
		t.Errorf("after timeout %+v, want rev+1 and drafts unchanged", after)
	}
	if code, _, _ := postReview(h, form); code != http.StatusConflict || fs.count() != 1 {
		t.Errorf("blind resubmit = %d, %d calls", code, fs.count())
	}
	if len(*got) != 1 || !(*got)[0].Uncertain || (*got)[0].Steps[0] != (stepCount{Step: "code review", Suggested: 1, Accepted: 1}) {
		t.Errorf("records %+v, want one uncertain with the draft", *got)
	}
}

func TestReviewInFlight(t *testing.T) {
	h, q, _, fs := submitSetup(t, "")
	started, release := make(chan struct{}), make(chan struct{})
	fs.fn = func(context.Context) (string, error) { close(started); <-release; return reviewURL, nil }
	form := reviewForm(q, "COMMENT", "body", "s")
	done := make(chan int)
	go func() { code, _, _ := postReview(h, form); done <- code }()
	<-started
	if code, body, _ := postReview(h, form); code != http.StatusConflict || body != "a review is already being submitted" {
		t.Errorf("second = %d %q", code, body)
	}
	close(release)
	if code := <-done; code != http.StatusOK || fs.count() != 1 {
		t.Errorf("first = %d, %d calls", code, fs.count())
	}
}

func summary(t *testing.T, h http.Handler, query string) string {
	t.Helper()
	code, body := send(h, http.MethodGet, "/pr/a/b/3/review/summary"+query, nil)
	if code != http.StatusOK {
		t.Fatalf("summary%s = %d %q", query, code, body)
	}
	return body
}

func TestReviewDialog(t *testing.T) {
	h, q, _, _ := submitSetup(t, "")
	// nothing included: empty list, required summary, the message, all 8 points unchecked
	for event, label := range map[string]string{"": "Comment", "COMMENT": "Comment", "REQUEST_CHANGES": "Request changes", "APPROVE": "Approve"} {
		dlg := summary(t, h, "?event="+event)
		mustContain(t, dlg, "<span class=\"label\">Comments to post (0)</span>\n<p class=\"muted\">No comments in this review.</p>",
			"<p class=\"alert alert-warning\"><span>8 of 8 points are not checked yet. You can still submit.</span></p>",
			`<input type="hidden" name="rev" value="0"><input type="hidden" name="head" value="abc123"><input type="hidden" name="expect" value="`+approveHash(nil)+`">`,
			"<button form=\"review-form\" id=\"review-submit\">"+label+"</button>", `<form method="dialog"><button class="btn-quiet">Back to review</button></form>`,
			`value="`+cmp.Or(event, "COMMENT")+`" hx-get="/pr/a/b/3/review/summary" checked autofocus>`, `<fieldset class="choice-list" hx-include="closest form" hx-target="#dialog-slot">`,
			`<form id="review-form" hx-post="/pr/a/b/3/review" hx-target="#dialog-slot" hx-disabled-elt="#review-submit, #review-form fieldset" hx-indicator="#review-submit" hx-disinherit="hx-disabled-elt hx-indicator">`)
		blocked := event != "APPROVE"
		if strings.Contains(dlg, `<textarea id="summary" name="body" rows="3" required>`) != blocked ||
			strings.Contains(dlg, "Add a comment or a summary to") != blocked {
			t.Errorf("%q: required/message shown != %v:\n%s", event, blocked, dlg)
		}
	}
	mustContain(t, summary(t, h, "?event=REQUEST_CHANGES"), "Add a comment or a summary to request changes.</span>")
	mustContain(t, summary(t, h, "?event=COMMENT&body=keep+%3Cme%3E"), "<textarea id=\"summary\" name=\"body\" rows=\"3\" required>\nkeep &lt;me&gt;</textarea>")
	// two comments: the first line cut to 80 characters, a file-level one without a line; one point checked
	long := strings.Repeat("é", 81)
	send(h, http.MethodPut, "/pr/a/b/3/drafts/f4", url.Values{"text": {"\n " + long + "\nsecond"}})
	include(t, h, "f4", "f1")
	send(h, http.MethodPost, "/pr/a/b/3/points/f4/checked", url.Values{"checked": {"1"}})
	dlg := summary(t, h, "?event=COMMENT")
	mustContain(t, dlg, "<span class=\"label\">Comments to post (2)</span>\n<ul class=\"post-list\">\n"+
		"<li><details><summary><span class=\"mono\" title=\"dir/a b#?%.go:7\">dir/a b#?%.go:7</span><span class=\"snip\">"+long[:160]+"…</span></summary>"+
		"<div class=\"text\">"+long+"\nsecond</div></details></li>\n"+
		"<li><details><summary><span class=\"mono\" title=\"b.go\">b.go</span><span class=\"snip\">whole file</span></summary><div class=\"text\">whole file</div></details></li>\n</ul>",
		"7 of 8 points are not checked yet.", `<input type="hidden" name="rev" value="`+strconv.Itoa(q.pending.get("a/b#3").Rev)+`">`, "<button form=\"review-form\" id=\"review-submit\">Post 2 comments</button>")
	if strings.Contains(dlg, " required>") || strings.Contains(dlg, "Add a comment or a summary") {
		t.Error("summary required with comments")
	}
	mustContain(t, summary(t, h, "?event=APPROVE"), "Approve and post 2 comments</button>")
	mustContain(t, summary(t, h, "?event=REQUEST_CHANGES"), "Request changes with 2 comments</button>")
	for event, want := range map[string]string{"COMMENT": "Post 1 comment", "APPROVE": "Approve and post 1 comment", "REQUEST_CHANGES": "Request changes with 1 comment"} {
		if got := submitLabel(event, 1); got != want {
			t.Errorf("submitLabel(%s, 1) = %q, want %q", event, got, want)
		}
	}
	for _, path := range []string{"/pr/a/b/3/review/summary?event=MERGE", "/pr/a/b/3/review/summary?event=approve"} {
		if code, _ := send(h, http.MethodGet, path, nil); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", path, code)
		}
	}
	// HTML in a comment is escaped in the opened row
	send(h, http.MethodPut, "/pr/a/b/3/drafts/f1", url.Values{"text": {"<b>bold</b>"}})
	mustContain(t, summary(t, h, "?event=COMMENT"), `<div class="text">&lt;b&gt;bold&lt;/b&gt;</div>`)
	for _, path := range []string{"/pr/a/b/9/review/summary", "/pr/a/c/3/review/summary", "/pr/a/b/x/review/summary"} {
		if code, _ := send(h, http.MethodGet, path, nil); code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, code)
		}
	}
}

func TestReviewDialogOwnPR(t *testing.T) {
	for login, own := range map[string]bool{"me": true, "": false} {
		h, _, _, _ := submitSetup(t, login)
		pg := pageOf(t, h)
		dlg := summary(t, h, "?event=APPROVE")
		for _, s := range []string{pg, dlg} {
			if strings.Contains(s, `value="APPROVE"`) == own || strings.Contains(s, `value="REQUEST_CHANGES"`) == own {
				t.Errorf("login %q: Approve/Request changes shown on own PR %v:\n%s", login, own, s)
			}
		}
		mustContain(t, pg, `value="COMMENT" id="decision-comment" checked>`)
		if own {
			mustContain(t, dlg, `<input type="radio" name="event" value="COMMENT" hx-get="/pr/a/b/3/review/summary" checked autofocus>`, "<button form=\"review-form\" id=\"review-submit\">Comment</button>")
		}
	}
}

func TestReviewErrorKeepsSummary(t *testing.T) {
	h, q, _, fs := submitSetup(t, "")
	fs.fn = func(context.Context) (string, error) { return "", errors.New("gh: Bad credentials (HTTP 401)") }
	form := reviewForm(q, "REQUEST_CHANGES", "body", "my <summary>", "expect", "e1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localReq(http.MethodPost, "/pr/a/b/3/review", form.Encode()))
	mustContain(t, rec.Body.String(), "<dialog class=\"dialog-backdrop\" open aria-labelledby=\"submit-title\">",
		`<p class="alert alert-error" role="alert"><span>gh: Bad credentials (HTTP 401)</span></p>`,
		"<textarea id=\"summary\" name=\"body\" rows=\"3\" required>\nmy &lt;summary&gt;</textarea>",
		`<input type="radio" name="event" value="REQUEST_CHANGES" hx-get="/pr/a/b/3/review/summary" checked autofocus>`,
		`<input type="hidden" name="rev" value="0"><input type="hidden" name="head" value="abc123"><input type="hidden" name="expect" value="e1">`)
	if rec.Code != http.StatusBadGateway || rec.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Errorf("code %d, type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	// the dialog of a refused submit keeps the form's rev, so a stale dialog stays stale
	include(t, h, "f4")
	if code, body := send(h, http.MethodPost, "/pr/a/b/3/review", form); code != http.StatusConflict || !strings.Contains(body, `name="rev" value="0"`) {
		t.Errorf("stale = %d %q", code, body)
	}
}

func TestSubmittedLine(t *testing.T) {
	h, _, _, _ := submitSetup(t, "")
	for query, want := range map[string]string{
		"?submitted=APPROVE&n=0":         "Review submitted: Approved, 0 comments.",
		"?submitted=REQUEST_CHANGES&n=1": "Review submitted: Changes requested, 1 comment.",
		"?submitted=COMMENT&n=3":         "Review submitted: Commented, 3 comments.",
		"?submitted=MERGE&n=1":           "",
		"?submitted=COMMENT&n=x":         "",
		"?submitted=COMMENT&n=-1":        "",
		"?submitted=COMMENT":             "",
		"?n=2":                           "",
	} {
		_, body := send(h, http.MethodGet, "/pr/a/b/3"+query, nil)
		if got := strings.Contains(body, "Review submitted"); got != (want != "") || want != "" && !strings.Contains(body, `<p class="alert alert-success" role="status"><span>`+want+`</span></p>`) {
			t.Errorf("%s: want %q", query, want)
		}
	}
}

func TestSubmitWithReplies(t *testing.T) {
	type call struct {
		input string
		args  []string
	}
	var calls []call
	fail := map[int]error{} // by call index
	run := func(_ context.Context, input []byte, args ...string) ([]byte, error) {
		calls = append(calls, call{string(input), args})
		if err := fail[len(calls)-1]; err != nil {
			return nil, err
		}
		switch {
		case args[0] == "api" && args[1] == "--method":
			return []byte("77\nPRR_n77\n"), nil
		case slices.Contains(args, "--jq"):
			return []byte("https://github.com/o/r/pull/1#pullrequestreview-77\n"), nil
		}
		return []byte(`{}`), nil
	}
	submit := submitWith(run)
	rv := newReview("sha1", "REQUEST_CHANGES", "sum", []pendingDraft{{File: "a.go", Line: 4, Text: "c"}})
	rv.Replies = []threadReply{{"T_1", "agree"}, {"T_2", "-f x=1 $body"}}
	wantCalls := []call{
		{`{"commit_id":"sha1","comments":[{"path":"a.go","line":4,"side":"RIGHT","body":"c"}]}`,
			[]string{"api", "--method", "POST", "repos/o/r/pulls/1/reviews", "--input", "-", "--jq", ".id, .node_id"}},
		{"", []string{"api", "graphql", "-f", "query=" + addReplyQuery, "-f", "review=PRR_n77", "-f", "thread=T_1", "-f", "body=agree"}},
		{"", []string{"api", "graphql", "-f", "query=" + addReplyQuery, "-f", "review=PRR_n77", "-f", "thread=T_2", "-f", "body=-f x=1 $body"}},
		{"", []string{"api", "graphql", "-f", "query=" + submitQuery, "-f", "review=PRR_n77", "-f", "event=REQUEST_CHANGES", "-f", "body=sum",
			"--jq", ".data.submitPullRequestReview.pullRequestReview.url"}},
	}
	u, err := submit(context.Background(), "o/r", 1, rv)
	if err != nil || u != "https://github.com/o/r/pull/1#pullrequestreview-77" || !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("%q %v, calls %+v", u, err, calls)
	}

	// 422 on create: retried with the comment in the body, which goes with the submit
	calls, fail = nil, map[int]error{0: errors.New("x (HTTP 422)")}
	if _, err := submit(context.Background(), "o/r", 1, rv); err != nil || len(calls) != 5 ||
		calls[1].input != `{"commit_id":"sha1"}` || calls[4].args[9] != "body=sum\n\n`a.go:4`\n\nc" {
		t.Fatalf("422: %v, calls %+v", err, calls)
	}

	// a failing reply or submit deletes the pending review and posts nothing
	del := call{"", []string{"api", "-X", "DELETE", "repos/o/r/pulls/1/reviews/77"}}
	for _, i := range []int{1, 2, 3} {
		boom := errors.New("GraphQL: boom")
		calls, fail = nil, map[int]error{i: boom}
		if _, err := submit(context.Background(), "o/r", 1, rv); err != boom || len(calls) != i+2 || !reflect.DeepEqual(calls[i+1], del) {
			t.Errorf("fail at %d: %v, calls %+v", i, err, calls)
		}
	}
	// a failing create has nothing to delete
	calls, fail = nil, map[int]error{0: errors.New("boom (HTTP 502)")}
	if _, err := submit(context.Background(), "o/r", 1, rv); err == nil || len(calls) != 1 {
		t.Errorf("create fails: %v, calls %+v", err, calls)
	}
}

// TestReviewApproveMarksDashboard: a successful APPROVE or REQUEST_CHANGES shows my marker on the dashboard without a poll;
// a failed or timed-out one does not.
func TestReviewApproveMarksDashboard(t *testing.T) {
	defer func(d time.Duration) { reviewTimeout = d }(reviewTimeout)
	reviewTimeout = 50 * time.Millisecond
	mine, older := approvedInd("Approved by me", "✅", false), approvedInd("Approved by me on an older commit", "✅", true)
	changes := approvedInd("Changes requested by me", "⛔", false)
	for _, c := range []struct {
		name, event string
		status      string // job status; done at rev1 reviews an older commit
		fn          func(ctx context.Context) (string, error)
		want, not   string
	}{
		{"current", "APPROVE", statusQueued, nil, mine, older},
		{"older commit", "APPROVE", statusDone, nil, older, mine},
		{"request changes", "REQUEST_CHANGES", statusQueued, nil, changes, "Approved by me"},
		{"failed", "APPROVE", statusQueued, func(context.Context) (string, error) { return "", errors.New("boom") }, "", "Approved by me"},
		{"timed out", "APPROVE", statusQueued, func(ctx context.Context) (string, error) { <-ctx.Done(); return "", errors.New("gh timed out") }, "", "Approved by me"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, p, q, j := doneSetup(t)
			prs := p.snapshot().PRs
			prs[2].Author, prs[2].Reviews = "bob", []approval{{Login: "alice", State: "APPROVED"}, {Login: "zed", Outdated: true, State: "APPROVED"}}
			f := &fakeFetch{prs: map[string][]pullRequest{"a/b": prs}}
			p = newPoller(cfg.Repositories, f.fetch, func(context.Context) (string, error) { return "me", nil })
			p.poll(context.Background())
			fs := &fakeSubmit{fn: c.fn}
			h := fresh(q, newHandler(newConfigHolder(cfg, "myreview.json"), p, q, fs.submit, notApproved, noFiles, noContent, noThreads, noResolve))
			j.HeadSHA, j.Status = "rev1", c.status
			code, body, _ := postReview(h, reviewForm(q, c.event, "confirm", "1", "body", "needs work"))
			if (code == http.StatusOK) != (c.fn == nil) || fs.count() != 1 {
				t.Fatalf("submit = %d %q, %d calls", code, body, fs.count())
			}
			get := func(path string, hx bool) string {
				r := localReq(http.MethodGet, path, "")
				if hx {
					r.Header.Set("HX-Request", "true")
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, r)
				return rec.Body.String()
			}
			for _, hx := range []bool{false, true} { // the page and the htmx list fragment
				page := get("/", hx)
				if c.want != "" {
					mustContain(t, page, c.want+approvedInd("Approved by alice; on an older commit by zed", "👥", false))
				}
				if strings.Contains(page, c.not) {
					t.Errorf("hx %v: page contains %q", hx, c.not)
				}
			}
			if c.fn != nil {
				return
			}
			if hidden := !strings.Contains(get("/?hideapproved=1", true), `href="/pr/a/b/3"`); hidden != (c.want == mine) {
				t.Errorf("hide approved: row hidden %v", hidden)
			}
			p.poll(context.Background()) // the next poll replaces the mark with what GitHub returns
			if strings.Contains(get("/", false), c.want) {
				t.Error("mark survived a poll")
			}
		})
	}
}

// captureReviews makes q record reviews into the returned slice.
func captureReviews(q *reviewQueue) *[]reviewRecord {
	var got []reviewRecord
	q.recordReview = func(r reviewRecord) { got = append(got, r) }
	return &got
}

// TestReviewRecorded: a submit with one finding draft as is, one point draft edited, two line comments and
// one reply records those counts, the rows, the run and per-step counts whose sums are the totals.
func TestReviewRecorded(t *testing.T) {
	cfg, p, q, j := doneSetup(t)
	if err := os.MkdirAll(q.pending.dir(), 0700); err != nil {
		t.Fatal(err)
	}
	j.Metrics = &reviewMetrics{RunID: "run1"}
	threads := func(context.Context, string, int) (threadList, error) {
		return threadList{Threads: []reviewThread{{ID: "T1", CanReply: true}}}, nil
	}
	fs := &fakeSubmit{}
	h := fresh(q, newHandler(newConfigHolder(cfg, "myreview.json"), p, q, fs.submit, notApproved, noFiles, noContent, threads, noResolve))
	got := captureReviews(q)
	opened := time.Now().UTC().Add(-time.Hour)
	if _, err := q.pending.update("a/b#3", func(pp *pendingPR) error {
		pp.OpenedAt = opened
		pp.Checked = []pendingCheck{{ID: "f0", File: "z.go", Line: 1, Text: "sec low"}, {ID: "p2", File: "dir/a b#?%.go", Line: 7, Text: "first"}}
		pp.Drafts = []pendingDraft{
			{ID: "f4", File: "dir/a b#?%.go", Line: 7, Commit: "abc123", SourceText: "crit\nsecond line", Text: "Please fix crit.", InReview: true},
			{ID: "p2", File: "dir/a b#?%.go", Line: 7, Commit: "abc123", SourceText: "first", Text: "Why first, really?", Edited: true, InReview: true},
			{ID: "c1", File: "a.go", Line: 1, Commit: "abc123", Text: "mine", InReview: true},
			{ID: "c2", File: "a.go", Line: 2, Commit: "abc123", Text: "mine too", InReview: true},
			{ID: "c3", File: "a.go", Line: 3, Commit: "abc123", Text: "not included"},
		}
		pp.Replies = []pendingReply{{Thread: "T1", Text: "ok"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := postReview(h, reviewForm(q, "REQUEST_CHANGES")); code != http.StatusOK {
		t.Fatalf("submit = %d %q", code, body)
	}
	if len(*got) != 1 {
		t.Fatalf("%d records", len(*got))
	}
	r := (*got)[0]
	if r.Repo != "a/b" || r.Number != 3 || r.HeadSHA != "abc123" || r.RunID != "run1" || r.Event != "REQUEST_CHANGES" || !r.OpenedAt.Equal(opened) ||
		r.Total != 8 || r.Checked != 2 || r.Own != 2 || r.Replies != 1 || r.Risk != "High" || r.Recommendation != recHuman || r.Uncertain ||
		time.Since(r.SubmittedAt) > time.Minute {
		t.Fatalf("record %+v", r)
	}
	steps := map[string]stepCount{}
	var sugg, acc, mod int
	for _, c := range r.Steps {
		steps[c.Step] = c
		sugg, acc, mod = sugg+c.Suggested, acc+c.Accepted, mod+c.Modified
	}
	want := map[string]stepCount{
		"code review":     {Step: "code review", Suggested: 1, Accepted: 1},
		"security review": {Step: "security review"},
		"risk assessment": {Step: "risk assessment", Suggested: 1, Modified: 1},
	}
	if !reflect.DeepEqual(steps, want) || len(r.Steps) != 3 {
		t.Fatalf("steps %+v", r.Steps)
	}
	if sugg != 2 || acc != 1 || mod != 1 {
		t.Fatalf("totals %d/%d/%d", sugg, acc, mod)
	}

	// the same record in a real metrics.db: the totals equal the step sums, run_id joins ai_runs
	dir := t.TempDir()
	m := openTestDB(t, dir)
	run := sampleRun("run1", time.Now())
	m.record(run)
	m.recordReview(r)
	m.close(5 * time.Second)
	db := readDB(t, dir)
	var bad, joined int
	db.QueryRow(`SELECT COUNT(*) FROM review_submissions r WHERE ai_suggested != (SELECT SUM(suggested) FROM review_step_counts WHERE submission_id = r.id)
		OR accepted_as_is != (SELECT SUM(accepted_as_is) FROM review_step_counts WHERE submission_id = r.id)
		OR modified != (SELECT SUM(modified) FROM review_step_counts WHERE submission_id = r.id)`).Scan(&bad)
	db.QueryRow(`SELECT COUNT(*) FROM review_submissions JOIN ai_runs USING (run_id)`).Scan(&joined)
	if bad != 0 || joined != 1 || count(t, db, "review_step_counts") != 3 {
		t.Fatalf("mismatched %d, joined %d, steps %d", bad, joined, count(t, db, "review_step_counts"))
	}
}

// TestReviewRecordedNoResult: without a done result a review has zero rows, no run or risk, and no steps.
func TestReviewRecordedNoResult(t *testing.T) {
	h, q, j, _ := submitSetup(t, "")
	j.Status = statusFailed
	got := captureReviews(q)
	if code, body, _ := postReview(h, reviewForm(q, "APPROVE", "confirm", "1")); code != http.StatusOK || len(*got) != 1 {
		t.Fatalf("submit = %d %q, %d records", code, body, len(*got))
	}
	if r := (*got)[0]; r.RunID != "" || r.Risk != "" || r.Total != 0 || r.Steps != nil || r.Event != "APPROVE" {
		t.Fatalf("record %+v", r)
	}
}

// TestReviewRecordedSaveFailed: posted, but the pending file could not be saved: 500 and one certain record.
func TestReviewRecordedSaveFailed(t *testing.T) {
	h, q, _, fs := submitSetup(t, "")
	got := captureReviews(q)
	dir := q.pending.dir()
	t.Cleanup(func() { os.Chmod(dir, 0700) })
	fs.fn = func(context.Context) (string, error) { os.Chmod(dir, 0500); return reviewURL, nil }
	code, body, _ := postReview(h, reviewForm(q, "COMMENT", "body", "s"))
	if code != http.StatusInternalServerError || !strings.HasPrefix(body, "the review was posted") {
		t.Fatalf("submit = %d %q", code, body)
	}
	if len(*got) != 1 || (*got)[0].Uncertain {
		t.Fatalf("records %+v", *got)
	}
}

// TestReviewRecordedDisabled: a nil or closed metrics.db changes nothing about the answer.
func TestReviewRecordedDisabled(t *testing.T) {
	m := openTestDB(t, t.TempDir())
	m.close(time.Second)
	for _, rec := range []func(reviewRecord){(*metricsDB)(nil).recordReview, m.recordReview} {
		h, q, _, fs := submitSetup(t, "")
		q.recordReview = rec
		if code, body, redirect := postReview(h, reviewForm(q, "COMMENT", "body", "s")); code != http.StatusOK || redirect == "" || fs.count() != 1 {
			t.Fatalf("submit = %d %q %q", code, body, redirect)
		}
	}
}
