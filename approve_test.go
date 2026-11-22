package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func notApproved(context.Context, string, int, string, string) (bool, error) { return false, nil }

func do(h http.Handler, r *http.Request) (int, string, string) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code, rec.Body.String(), rec.Header().Get("Location")
}

func page(t *testing.T, h http.Handler) string {
	t.Helper()
	code, body, _ := do(h, localReq(http.MethodGet, "/pr/o/r/1", ""))
	if code != http.StatusOK {
		t.Fatalf("details = %d", code)
	}
	return body
}

// approveSetup is o/r#1 at head with the postQueue result (reviewed at h1, one High and one Low finding),
// st applied to the job, approved as the already-approved check, and a fake submit.
func approveSetup(t *testing.T, head string, st func(*job), approved func(context.Context, string, int, string, string) (bool, error)) (http.Handler, *reviewQueue, *fakeSubmit) {
	q, _, _ := postQueue(t, t.TempDir())
	if err := os.MkdirAll(q.pending.dir(), 0700); err != nil {
		t.Fatal(err)
	}
	pr := hpr(1, head)
	p := newPoller([]string{"o/r"}, (&fakeFetch{prs: map[string][]pullRequest{"o/r": {pr}}}).fetch, noLogin)
	p.poll(context.Background())
	q.sync([]pullRequest{pr})
	if st != nil {
		st(q.jobs[key(1)])
		if q.jobs[key(1)].Status == "" { // unknown: the queue forgets the PR
			delete(q.jobs, key(1))
		}
	}
	fs := &fakeSubmit{}
	return fresh(q, newHandler(newConfigHolder(config{Repositories: []string{"o/r"}}, "myreview.json"), p, q, fs.submit, approved, noFiles, noContent, noThreads, noResolve)), q, fs
}

func lowRisk(j *job) {
	j.Recommendation, j.Risk, j.Rationale = recAutomated, "Low", nil
}

// approveForm is the APPROVE form the current o/r#1 page would submit, with extra key/value overrides.
func approveForm(q *reviewQueue, head string, extra ...string) string {
	st, known := q.statuses()[key(1)]
	v := url.Values{"event": {"APPROVE"}, "rev": {strconv.Itoa(q.pending.get(key(1)).Rev)}, "head": {head},
		"expect": {approveHash(confirmReasons(hpr(1, head), st, known))}}
	for i := 0; i+1 < len(extra); i += 2 {
		v.Set(extra[i], extra[i+1])
	}
	return v.Encode()
}

// errorOf is the error a re-rendered dialog shows, or the whole body when it is not a dialog.
func errorOf(body string) string {
	const open = `<p class="alert alert-error" role="alert"><span>`
	if i := strings.Index(body, open); i >= 0 && strings.HasPrefix(body, "<dialog") {
		msg, _, _ := strings.Cut(body[i+len(open):], "</span>")
		return msg
	}
	return strings.TrimSpace(body)
}

func TestCompactApprove(t *testing.T) {
	h, q, fs := approveSetup(t, "h1", lowRisk, notApproved)
	pg := page(t, h)
	mustContain(t, pg, "<div class=\"decision-bar compact\">",
		`<button type="button" class="link-btn" hx-get="/pr/o/r/1/review/summary?event=COMMENT" hx-target="#dialog-slot">Comment or request changes…</button>`,
		`<form hx-post="/pr/o/r/1/review" hx-target="#dialog-slot" hx-disabled-elt="find button" hx-indicator="find button"><input type="hidden" name="event" value="APPROVE"><input type="hidden" name="rev" value="1"><input type="hidden" name="head" value="h1"><input type="hidden" name="expect" value="`+approveHash(nil)+`"><button>Approve</button></form>`)
	for _, no := range []string{"Approve anyway", "You approved", `class="seg"`, "Submit review ("} {
		if strings.Contains(pg, no) {
			t.Errorf("compact page has %q", no)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localReq(http.MethodPost, "/pr/o/r/1/review", approveForm(q, "h1")))
	if rec.Code != http.StatusOK || rec.Header().Get("HX-Redirect") != "/pr/o/r/1?submitted=APPROVE&n=0" ||
		!reflect.DeepEqual(fs.calls, []reviewPayload{{CommitID: "h1", Event: "APPROVE"}}) {
		t.Fatalf("one-click approve = %d %q, calls %+v", rec.Code, rec.Header().Get("HX-Redirect"), fs.calls)
	}
	// a draft in the review: the full bar
	if code, body, _ := do(h, localReq(http.MethodPost, "/pr/o/r/1/drafts/f0/include", "include=1")); code != http.StatusOK {
		t.Fatalf("include = %d %q", code, body)
	}
	pg = page(t, h)
	mustContain(t, pg, "<div class=\"decision-bar\">", "Submit review (1)</button>")
	if strings.Contains(pg, "Comment or request changes") {
		t.Error("compact link with a draft in the review")
	}
}

func TestDialogApproveConfirmation(t *testing.T) {
	human := func(j *job) {
		j.Recommendation, j.Risk, j.Threshold, j.Rationale = recHuman, "High", "High", []string{"1 finding at or above the threshold (High).", "Touches <auth>."}
	}
	failed := func(j *job) { j.Status, j.FailedStep, j.Reason = statusFailed, "code review", "exit 1" }
	outdated := "<p class=\"alert alert-warning\"><span>These results are outdated: the comments and the decision go to the reviewed commit <code class=\"sha\">h1</code>, not the current head.</span></p>"
	for _, c := range []struct {
		name, head string
		st         func(*job)
		want       []string // the confirmation lines; none: nothing to confirm
		outdated   bool
		commit     string // the reviewed commit when done, else the head
	}{
		// the human-review lines are shown in the bar only
		{"human", "h1", human, nil, false, "h1"},
		{"human outdated", "h2abcdef99", human, []string{"The results are outdated: they are for commit h1, the pull request is now at h2abcde."}, true, "h1"},
		{"outdated", "h2abcdef99", lowRisk, []string{"The results are outdated: they are for commit h1, the pull request is now at h2abcde."}, true, "h1"},
		{"unknown", "h1", func(j *job) { j.Status = "" }, []string{"No review results yet."}, false, "h1"},
		{"queued", "h1", func(j *job) { j.Status = statusQueued }, []string{"No review results yet: the review is queued."}, false, "h1"},
		{"on demand", "h1", func(j *job) {
			j.jobStatus = jobStatus{Status: statusOnDemand, Reason: "created more than 168h0m0s ago"}
		}, []string{"No review results yet: the review is on demand."}, false, "h1"},
		{"failed", "h1", failed, []string{"The review failed at code review: exit 1."}, false, "h1"},
		{"skipped", "h1", func(j *job) {
			j.jobStatus = jobStatus{Status: statusSkipped, SkipRule: 2, Reason: "title matches rule skip[2] ^Bump "}
		}, []string{"Review skipped: title matches rule skip[2] ^Bump ."}, false, "h1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, q, fs := approveSetup(t, c.head, c.st, notApproved)
			st, known := q.statuses()[key(1)]
			expect := `<input type="hidden" name="expect" value="` + approveHash(confirmReasons(hpr(1, c.head), st, known)) + `">`
			pg := page(t, h)
			// not done: the same controls without progress, so Approve works as in v1
			mustContain(t, pg, `<input type="radio" class="visually-hidden" name="event" value="APPROVE" id="decision-approve">`, "Submit review (0)</button>")
			code, dlg, _ := do(h, localReq(http.MethodGet, "/pr/o/r/1/review/summary?event=APPROVE", ""))
			if code != http.StatusOK {
				t.Fatalf("summary = %d", code)
			}
			mustContain(t, dlg, expect, `<input type="radio" name="event" value="APPROVE" hx-get="/pr/o/r/1/review/summary" checked autofocus>`, ">Approve</button>")
			var lines []string
			for _, l := range c.want {
				lines = append(lines, "<li class=\"text\">"+l+"</li>")
			}
			mustContain(t, dlg, lines...)
			const confirm = `<label class="confirm"><input type="checkbox" name="confirm" value="1" required> Approve anyway</label>`
			if strings.Contains(dlg, confirm) != (c.want != nil) || strings.Contains(dlg, outdated) != c.outdated {
				t.Errorf("confirm %v, outdated warning %v:\n%s", c.want != nil, c.outdated, dlg)
			}
			for _, hidden := range []string{"Human review required (risk", "Touches &lt;auth&gt;.</li>", "finding at a.go:3"} {
				if strings.Contains(dlg, hidden) {
					t.Errorf("dialog lists %q", hidden)
				}
			}
			if _, cm, _ := do(h, localReq(http.MethodGet, "/pr/o/r/1/review/summary?event=COMMENT", "")); strings.Contains(cm, confirm) {
				t.Error("COMMENT dialog asks to confirm an approval")
			}
			if c.want == nil {
				return
			}
			code, body, _ := do(h, localReq(http.MethodPost, "/pr/o/r/1/review", approveForm(q, c.head)))
			if code != http.StatusConflict || errorOf(body) != "confirm the approval first." || fs.count() != 0 {
				t.Fatalf("unconfirmed = %d %q, %d calls", code, errorOf(body), fs.count())
			}
			mustContain(t, body, confirm, expect)
			if code, body, _ = do(h, localReq(http.MethodPost, "/pr/o/r/1/review", approveForm(q, c.head, "confirm", "1"))); code != http.StatusOK || fs.count() != 1 {
				t.Fatalf("confirmed = %d %q, %d calls", code, body, fs.count())
			}
			if fs.calls[0].CommitID != c.commit {
				t.Errorf("commit %s, want %s", fs.calls[0].CommitID, c.commit)
			}
		})
	}
}

func TestApprovedLine(t *testing.T) {
	var got []string
	yes := func(_ context.Context, repo string, number int, login, sha string) (bool, error) {
		got = []string{repo, login, sha}
		return true, nil
	}
	h, _, _ := approveSetup(t, "h1abcdef99", lowRisk, yes)
	mustContain(t, page(t, h), "<p class=\"alert alert-success\"><span>You approved commit <code class=\"sha\">h1abcde</code>.</span></p>", "Submit review (0)")
	if got[0] != "o/r" || got[2] != "h1abcdef99" {
		t.Errorf("approved called with %v", got)
	}
	fail := func(context.Context, string, int, string, string) (bool, error) {
		return false, errors.New("gh login not known yet")
	}
	h, _, _ = approveSetup(t, "h1", lowRisk, fail)
	pg := page(t, h)
	mustContain(t, pg, "<p class=\"alert alert-warning\"><span>Could not check your approvals on GitHub: gh login not known yet</span></p>", "<button>Approve</button>")
	if strings.Contains(pg, "You approved") {
		t.Error("approved line shown on error")
	}
}

func TestRemovedRoutes(t *testing.T) {
	h, _, fs := approveSetup(t, "h1", lowRisk, notApproved)
	for _, path := range []string{"/comment", "/approve"} {
		for _, m := range []string{http.MethodPost, http.MethodGet} {
			if code, _, _ := do(h, localReq(m, path, "repo=o/r&number=1&head=h1")); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d, want 404 or 405", m, path, code)
			}
		}
	}
	if fs.count() != 0 {
		t.Errorf("%d calls", fs.count())
	}
}
