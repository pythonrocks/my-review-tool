package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func get(t *testing.T, cfg config, configPath, path string) *httptest.ResponseRecorder {
	t.Helper()
	return getWith(t, cfg, configPath, newPoller(cfg.Repositories, nil, noLogin), path)
}

func getWith(t *testing.T, cfg config, configPath string, p *poller, path string) *httptest.ResponseRecorder {
	t.Helper()
	return serve(cfg, configPath, p, newReviewQueue(0, workflow{}, resultStore{dir: t.TempDir()}), localReq(http.MethodGet, path, ""))
}

func serve(cfg config, configPath string, p *poller, q *reviewQueue, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	os.MkdirAll(q.pending.dir(), 0700) // openStore makes it; a page load writes the pending file
	newHandler(newConfigHolder(cfg, configPath), p, q, nil, notApproved, noFiles, noContent, noThreads, noResolve).ServeHTTP(rec, r)
	return rec
}

// localReq builds a request as the app window sends it to the asset handler (Host localhost, no TCP listener).
func localReq(method, target, form string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(form))
	r.Host = "localhost"
	if form != "" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return r
}

func TestHealthz(t *testing.T) {
	rec := get(t, config{Repositories: []string{"a/b", "c/d"}, Concurrency: 2}, "myreview.json", "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q, want application/json", ct)
	}
	var body health
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "ok" || body.Version != version || body.Repositories != 2 {
		t.Errorf("body = %+v, want status ok, version %s and 2 repositories", body, version)
	}
	if !strings.Contains(rec.Body.String(), `"pull_requests":0,"last_successful_poll":null`) {
		t.Errorf("body = %s, want 0 pull requests and null last_successful_poll before the first poll", rec.Body)
	}
}

func TestHealthzAfterPoll(t *testing.T) {
	cfg := config{Repositories: []string{"a/b", "c/d"}, Concurrency: 2}
	fail := true
	p := newPoller(cfg.Repositories, func(_ context.Context, repo string) ([]pullRequest, error) {
		if repo == "c/d" && fail {
			return nil, errors.New("boom")
		}
		return []pullRequest{{Repo: repo, Number: 1}, {Repo: repo, Number: 2}}, nil
	}, noLogin)
	p.poll(context.Background())
	rec := getWith(t, cfg, "myreview.json", p, "/healthz")
	if !strings.Contains(rec.Body.String(), `"pull_requests":2,"last_successful_poll":null`) {
		t.Errorf("after failing poll body = %s, want 2 pull requests and null last_successful_poll", rec.Body)
	}
	fail = false
	p.poll(context.Background())
	var body struct {
		PullRequests       int     `json:"pull_requests"`
		LastSuccessfulPoll *string `json:"last_successful_poll"`
	}
	if err := json.Unmarshal(getWith(t, cfg, "myreview.json", p, "/healthz").Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.PullRequests != 4 || body.LastSuccessfulPoll == nil {
		t.Fatalf("body = %+v, want 4 pull requests and a last_successful_poll", body)
	}
	if ts, err := time.Parse(time.RFC3339, *body.LastSuccessfulPoll); err != nil || ts.Location() != time.UTC {
		t.Errorf("last_successful_poll = %q, want RFC 3339 UTC (err %v)", *body.LastSuccessfulPoll, err)
	}
}

func TestIndex(t *testing.T) {
	rec := get(t, config{Repositories: []string{"a/b", "c/d"}, Concurrency: 2}, "myreview.json", "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content type = %q, want text/html", ct)
	}
	for _, want := range []string{
		"<title>myReview</title>", `id="pr-list"`, "<h2>Watched repositories</h2>",
		`<li><a id="repo-a/b" href="https://github.com/a/b">a/b</a></li>`,
		`<li><a id="repo-c/d" href="https://github.com/c/d">c/d</a></li>`,
		`<link rel="stylesheet" href="/myreview.css">`, `<header class="app-header">`, `<section class="watched">`, `<ul class="repos">`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("body missing %q", want)
		}
	}
	if strings.Contains(rec.Body.String(), "Add repositories in Settings") {
		t.Error("body has the empty-list message")
	}
}

func TestIndexEmpty(t *testing.T) {
	rec := get(t, config{Concurrency: 2}, "myreview.json", "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	for _, want := range []string{"<h2>Watched repositories</h2>", `<p><a href="/settings">Add repositories in Settings</a></p>`, `<a class="settings-link" href="/settings">Settings</a>`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("body missing %q", want)
		}
	}
	if strings.Contains(rec.Body.String(), "<ul") {
		t.Error("body has a list")
	}
}

func TestNotFound(t *testing.T) {
	for _, path := range []string{"/nope", "/healthz/"} {
		if rec := get(t, config{Concurrency: 2}, "myreview.json", path); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
	}
}

func TestFormatAge(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0m"}, {59*time.Minute + 59*time.Second, "59m"}, {time.Hour, "1h"},
		{23*time.Hour + 59*time.Minute, "23h"}, {24 * time.Hour, "1d"}, {50 * time.Hour, "2d"},
	} {
		if got := formatAge(tc.d); got != tc.want {
			t.Errorf("formatAge(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func bodyOf(t *testing.T, cfg config, p *poller) string {
	t.Helper()
	return getWith(t, cfg, "myreview.json", p, "/").Body.String()
}

func TestIndexLoadingAndRefresh(t *testing.T) {
	body := get(t, config{Repositories: []string{"a/b"}, Concurrency: 2}, "myreview.json", "/").Body.String()
	for _, want := range []string{`<meta http-equiv="refresh" content="60">`, `<p class="alert alert-info"><span>Loading pull requests…</span></p>`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if strings.Contains(body, "<table") {
		t.Error("body has a table before the first poll")
	}
}

func TestIndexTable(t *testing.T) {
	cfg := config{Repositories: []string{"a/b", "c/d"}, Concurrency: 2}
	now := time.Now()
	f := &fakeFetch{prs: map[string][]pullRequest{
		"a/b": {{Repo: "a/b", Number: 7, Title: "Old draft", URL: "https://github.com/a/b/pull/7", Author: "alice", Draft: true, Created: now.Add(-50 * time.Hour)}},
		"c/d": {{Repo: "c/d", Number: 3, Title: "Ready <script>alert(1)</script>", URL: "https://github.com/c/d/pull/3", Author: "ghost", Created: now.Add(-3*time.Hour - time.Minute)}},
	}}
	p := newPoller(cfg.Repositories, f.fetch, noLogin)
	p.poll(context.Background())
	body := bodyOf(t, cfg, p)
	header := "<tr><th title=\"Review requested\"><span aria-hidden=\"true\">⭐</span><span class=\"visually-hidden\">Review requested</span></th><th>Repository</th><th>Pull request</th><th>Author</th><th>Age</th><th>Size</th><th>Draft</th><th>Status</th><th>Review</th><th>Risk</th><th>Recommendation</th><th></th></tr>"
	ready := `<tr><td></td><td class="repo"><span class="org">c</span><br>d</td><td class="pr"><a id="pr-c/d-3" href="https://github.com/c/d/pull/3"><span class="num">#3</span>Ready &lt;script&gt;alert(1)&lt;/script&gt;</a></td><td>ghost</td><td class="age">3h</td><td></td><td></td><td></td><td><span class="badge status-not-queued">not queued</span></td><td></td><td></td><td class="actions"><a class="btn btn-sm btn-quiet" id="details-c/d-3" href="/pr/c/d/3">Details</a> <form method="post" action="/rerun" hx-post="/rerun" hx-target="closest tr" hx-swap="outerHTML" hx-sync="#pr-list:queue all"><input type="hidden" name="repo" value="c/d"><input type="hidden" name="number" value="3"><input type="hidden" name="filter" value=""><button class="btn-sm btn-quiet" id="rerun-c/d-3">Re-run</button></form></td></tr>`
	draft := `<tr><td></td><td class="repo"><span class="org">a</span><br>b</td><td class="pr"><a id="pr-a/b-7" href="https://github.com/a/b/pull/7"><span class="num">#7</span>Old draft</a></td><td>alice</td><td class="age">2d</td><td></td><td><span class="badge plain">draft</span></td><td></td><td><span class="badge status-not-queued">not queued</span></td><td></td><td></td><td class="actions"><a class="btn btn-sm btn-quiet" id="details-a/b-7" href="/pr/a/b/7">Details</a> <form method="post" action="/rerun" hx-post="/rerun" hx-target="closest tr" hx-swap="outerHTML" hx-sync="#pr-list:queue all"><input type="hidden" name="repo" value="a/b"><input type="hidden" name="number" value="7"><input type="hidden" name="filter" value=""><button class="btn-sm btn-quiet" id="rerun-a/b-7">Re-run</button></form></td></tr>`
	mustContain(t, body, `<div class="table-scroll">`+"\n"+`<table class="pr-table">`)
	h, r, d := strings.Index(body, header), strings.Index(body, ready), strings.Index(body, draft)
	if h < 0 || r < 0 || d < 0 || !(h < r && r < d) {
		t.Errorf("want header, ready row, draft row in order (%d %d %d); body:\n%s", h, r, d, body)
	}
	if strings.Contains(body, "<script>") || strings.Contains(body, "Loading") || strings.Contains(body, "No open pull requests.") {
		t.Errorf("body has raw script, loading or empty text:\n%s", body)
	}
}

func TestIndexEmptyResultAndRepoError(t *testing.T) {
	cfg := config{Repositories: []string{"a/b", "c/d"}, Concurrency: 2}
	f := &fakeFetch{errs: map[string]error{"c/d": errors.New("GraphQL: <b>not found</b>")}}
	p := newPoller(cfg.Repositories, f.fetch, noLogin)
	p.poll(context.Background())
	body := bodyOf(t, cfg, p)
	for _, want := range []string{
		`<p class="muted">No open pull requests.</p>`,
		`<li><a id="repo-a/b" href="https://github.com/a/b">a/b</a></li>`,
		`<li><a id="repo-c/d" href="https://github.com/c/d">c/d</a> <span class="repo-error">— error: GraphQL: &lt;b&gt;not found&lt;/b&gt;</span></li>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<table") {
		t.Error("body has a table with no PRs")
	}
}

func TestIndexGlobalHidesRepoErrors(t *testing.T) {
	cfg := config{Repositories: []string{"a/b"}, Concurrency: 2}
	f := &fakeFetch{errs: map[string]error{"a/b": errors.New("boom")}}
	p := newPoller(cfg.Repositories, f.fetch, noLogin)
	p.poll(context.Background())
	f.errs["a/b"] = globalError(msgGhNotLoggedIn)
	p.poll(context.Background())
	body := bodyOf(t, cfg, p)
	msg := `<p class="alert alert-error"><span>` + "GitHub CLI is not logged in. Run &#34;gh auth login&#34; (or set GH_TOKEN); the next poll picks it up.</span></p>"
	if g, l := strings.Index(body, msg), strings.Index(body, "<h2>Watched repositories</h2>"); g < 0 || g > l {
		t.Errorf("want global message above the list:\n%s", body)
	}
	if strings.Contains(body, "boom") {
		t.Errorf("stale repository error shown with the global message:\n%s", body)
	}
}

// minePoller returns a polled poller with two requested PRs (a/b#1, c/d#3) and one not requested (a/b#2).
func minePoller(cfg config, requested bool) *poller {
	f := &fakeFetch{prs: map[string][]pullRequest{
		"a/b": {{Repo: "a/b", Number: 1, Requested: requested}, {Repo: "a/b", Number: 2}},
		"c/d": {{Repo: "c/d", Number: 3, Requested: requested}},
	}}
	p := newPoller(cfg.Repositories, f.fetch, noLogin)
	p.poll(context.Background())
	return p
}

func TestIndexFilterOff(t *testing.T) {
	cfg := config{Repositories: []string{"a/b", "c/d"}, Concurrency: 2}
	p := minePoller(cfg, true)
	for _, path := range []string{"/", "/?mine=0", "/?mine=x"} {
		body := getWith(t, cfg, "myreview.json", p, path).Body.String()
		if strings.Count(body, `<td class="repo">`) != 3 || strings.Count(body, "<tr><td>"+star+`</td><td class="repo">`) != 2 {
			t.Errorf("%s: want 3 rows, 2 with you:\n%s", path, body)
		}
		if !strings.Contains(body, `<div class="toolbar">`+"\n"+`<form class="filters" method="get" action="/" hx-get="/" hx-trigger="change, submit" hx-target="#pr-rows" hx-sync="#pr-list:replace">`) ||
			!strings.Contains(body, `<input class="visually-hidden" type="checkbox" id="f-mine" name="mine" value="1"><label class="toggle" for="f-mine" title="Only PRs requesting my review">Requested <span aria-hidden="true">⭐</span></label>`) ||
			strings.Contains(body, "hidden by filters") || strings.Contains(body, "Clear filters") {
			t.Errorf("%s: want the filter form with mine unchecked and no active-filters line:\n%s", path, body)
		}
	}
}

func TestIndexFilterOn(t *testing.T) {
	cfg := config{Repositories: []string{"a/b", "c/d"}, Concurrency: 2}
	rec := getWith(t, cfg, "myreview.json", minePoller(cfg, true), "/?mine=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no redirect)", rec.Code)
	}
	body := rec.Body.String()
	if strings.Count(body, `<td class="repo">`) != 2 || strings.Count(body, "<tr><td>"+star+`</td><td class="repo">`) != 2 || strings.Contains(body, "#2<") {
		t.Errorf("want only the 2 requested rows:\n%s", body)
	}
	if a, c := strings.Index(body, "#1<"), strings.Index(body, "#3<"); a < 0 || c < 0 || a > c {
		t.Errorf("want snapshot order a/b#1 then c/d#3:\n%s", body)
	}
	for _, want := range []string{
		`<input class="visually-hidden" type="checkbox" id="f-mine" name="mine" value="1" checked><label class="toggle" for="f-mine" title="Only PRs requesting my review">Requested <span aria-hidden="true">⭐</span></label>`,
		`<div id="pr-rows"><p class="filter-summary"><span>Showing 2 of 3 pull requests, 1 hidden by filters.</span> <a class="filter-link" id="chip-mine-1" href="/" aria-label="Remove filter Requested">Requested <span aria-hidden="true">×</span></a> <a class="filter-link" id="clear-filters" href="/">Clear filters</a></p>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `href="/?mine=1"`) {
		t.Error("filter-on page links to the filter again")
	}
}

func TestIndexRerunKeepsFilter(t *testing.T) {
	cfg := config{Repositories: []string{"a/b", "c/d"}, Concurrency: 2}
	body := getWith(t, cfg, "myreview.json", minePoller(cfg, true), "/?org=a&mine=1&foo=bar").Body.String()
	if want := `<input type="hidden" name="filter" value="mine=1&amp;org=a">`; !strings.Contains(body, want) {
		t.Errorf("body missing %q:\n%s", want, body)
	}
}

func TestIndexFilterNoMatch(t *testing.T) {
	cfg := config{Repositories: []string{"a/b", "c/d"}, Concurrency: 2}
	body := getWith(t, cfg, "myreview.json", minePoller(cfg, false), "/?mine=1").Body.String()
	if !strings.Contains(body, `<p class="muted">No open pull requests request your review.</p>`) || strings.Contains(body, "<table") {
		t.Errorf("want the no-match message and no table:\n%s", body)
	}
	// loading and no-PR states take precedence over the no-match message
	if body := get(t, cfg, "myreview.json", "/?mine=1").Body.String(); !strings.Contains(body, "Loading pull requests…") || strings.Contains(body, "request your review.</p>") {
		t.Errorf("before first poll want loading only:\n%s", body)
	}
	p := newPoller(cfg.Repositories, (&fakeFetch{}).fetch, noLogin)
	p.poll(context.Background())
	if body := getWith(t, cfg, "myreview.json", p, "/?mine=1").Body.String(); !strings.Contains(body, `<p class="muted">No open pull requests.</p>`) || strings.Contains(body, "request your review.</p>") {
		t.Errorf("with no PRs want the no-PR message only:\n%s", body)
	}
}

func TestIndexLogin(t *testing.T) {
	cfg := config{Repositories: []string{"a/b"}, Concurrency: 2}
	p := newPoller(cfg.Repositories, (&fakeFetch{}).fetch, func(context.Context) (string, error) { return "octo<cat>", nil })
	p.poll(context.Background())
	if body := bodyOf(t, cfg, p); !strings.Contains(body, `<p class="signed-in">Signed in to GitHub as <strong>octo&lt;cat&gt;</strong></p>`) {
		t.Errorf("want the login line:\n%s", body)
	}
	if body := bodyOf(t, cfg, minePoller(cfg, true)); strings.Contains(body, "Signed in") {
		t.Errorf("login line shown without a login:\n%s", body)
	}
}

// reviewSetup returns a polled poller with a/b#1..#5 (all requesting review) and a queue that knows #1..#4
// with the given statuses. Limit 0 keeps re-run jobs queued, so nothing runs.
func reviewSetup(t *testing.T) (config, *poller, *reviewQueue) {
	cfg := config{Repositories: []string{"a/b"}, Concurrency: 2}
	var prs []pullRequest
	for n := 1; n <= 5; n++ {
		prs = append(prs, pullRequest{Repo: "a/b", Number: n, Created: day(n), Requested: true})
	}
	p := newPoller(cfg.Repositories, (&fakeFetch{prs: map[string][]pullRequest{"a/b": prs}}).fetch, noLogin)
	p.poll(context.Background())
	q := newReviewQueue(0, workflow{}, resultStore{dir: t.TempDir()})
	if err := os.MkdirAll(q.pending.dir(), 0700); err != nil { // openStore makes it; a page load writes the pending file
		t.Fatal(err)
	}
	for i, s := range []jobStatus{{Status: statusQueued}, {Status: statusRunning}, {Status: statusDone}, {Status: statusFailed, FailedStep: "code review", Reason: "exit <1>"}} {
		q.jobs[prKey(prs[i])] = &job{pr: prs[i], jobStatus: s}
	}
	return cfg, p, q
}

// star is the Review requested indicator.
const star = `<span class="ind" role="img" aria-label="Your review is requested" title="Your review is requested">⭐</span>`

// ind is a dashboard indicator wrapper with its label.
func ind(label, visual string) string {
	return fmt.Sprintf(`<span class="ind" role="img" aria-label="%s" title="%s">%s</span>`, label, label, visual)
}

func TestIndexReviewColumn(t *testing.T) {
	cfg, p, q := reviewSetup(t)
	check := func(n int, want, risk, rec string) {
		t.Helper()
		body := serve(cfg, "myreview.json", p, q, localReq(http.MethodGet, "/?mine=1", "")).Body.String()
		// the ⭐ cell comes first; Draft and Approved are empty
		first := fmt.Sprintf(`<tr><td>%s</td><td class="repo"><span class="org">a</span><br>b</td><td class="pr"><a id="pr-a/b-%d"`, star, n)
		if !strings.Contains(body, first) || strings.Contains(body, "dot status-done") {
			t.Errorf("#%d: want %q first and no done dot:\n%s", n, first, body)
		}
		cell := fmt.Sprintf(`</td><td></td><td></td><td>%s</td><td>%s</td><td>%s</td><td class="actions"><a class="btn btn-sm btn-quiet" id="details-a/b-%d" href="/pr/a/b/%d">Details</a> <form method="post" action="/rerun" hx-post="/rerun" hx-target="closest tr" hx-swap="outerHTML" hx-sync="#pr-list:queue all"><input type="hidden" name="repo" value="a/b"><input type="hidden" name="number" value="%d"><input type="hidden" name="filter" value="mine=1"><button class="btn-sm btn-quiet" id="rerun-a/b-%d">Re-run</button></form></td>`, want, risk, rec, n, n, n, n)
		if !strings.Contains(body, cell) {
			t.Errorf("#%d: body missing %q:\n%s", n, cell, body)
		}
	}
	human := ind("Human review required", `<span class="rec rec-human"></span>`)
	// Risk fields on every status prove that only a done review shows them.
	for n := 1; n <= 4; n++ {
		q.jobs[prKey(pullRequest{Repo: "a/b", Number: n})].riskDecision = riskDecision{Recommendation: recHuman, Risk: "Medium"}
	}
	for i, want := range []string{`<span class="badge status-queued">queued</span>`, `<span class="badge status-running">running</span>`,
		ind("Review done: 0 critical, 0 high, 0 medium, 0 low", `<span class="dot zero">0</span><span class="dot zero">0</span><span class="dot zero">0</span><span class="dot zero">0</span>`), `<span class="badge status-failed">failed: code review: exit &lt;1&gt;</span>`,
		`<span class="badge status-not-queued">not queued</span>`} {
		risk, rec := "", ""
		if i == 2 {
			risk, rec = ind("Risk: Medium", `<span class="meter risk-medium"></span>`), human
		}
		check(i+1, want, risk, rec)
	}
	j := q.jobs[prKey(pullRequest{Repo: "a/b", Number: 3})]
	for _, tc := range []struct {
		sevs       []string
		risk, rec  string
		want, rInd string
		recInd     string
	}{
		{[]string{"Medium", "Critical", "Medium"}, "High", recAutomated,
			ind("Review done: 1 critical, 0 high, 2 medium, 0 low", `<span class="dot sev-critical">1</span><span class="dot zero">0</span><span class="dot sev-medium">2</span><span class="dot zero">0</span>`),
			ind("Risk: High", `<span class="meter risk-high"></span>`), ind("Automated review is sufficient", `<span class="rec rec-auto"></span>`)},
		{[]string{"High", "High", "Critical", "Low", "Medium"}, "Medium", recHuman,
			ind("Review done: 1 critical, 2 high, 1 medium, 1 low", `<span class="dot sev-critical">1</span><span class="dot sev-high">2</span><span class="dot sev-medium">1</span><span class="dot sev-low">1</span>`),
			ind("Risk: Medium", `<span class="meter risk-medium"></span>`), human},
		{[]string{"Low"}, "Low", recAutomated,
			ind("Review done: 0 critical, 0 high, 0 medium, 1 low", `<span class="dot zero">0</span><span class="dot zero">0</span><span class="dot zero">0</span><span class="dot sev-low">1</span>`),
			ind("Risk: Low", `<span class="meter risk-low"></span>`), ind("Automated review is sufficient", `<span class="rec rec-auto"></span>`)},
		// an older result without risk fields leaves both cells empty
		{nil, "", "", ind("Review done: 0 critical, 0 high, 0 medium, 0 low", `<span class="dot zero">0</span><span class="dot zero">0</span><span class="dot zero">0</span><span class="dot zero">0</span>`), "", ""},
	} {
		j.Findings = nil
		for _, sev := range tc.sevs {
			j.Findings = append(j.Findings, taggedFinding{Step: "code review", finding: finding{Severity: sev}})
		}
		j.Risk, j.Recommendation = tc.risk, tc.rec
		check(3, tc.want, tc.rInd, tc.recInd)
	}
}

func TestRerun(t *testing.T) {
	for _, tc := range []struct {
		name, form string
		code       int
		location   string
	}{
		{"filter kept", "repo=a/b&number=3&filter=mine%3D1", http.StatusSeeOther, "/?mine=1"},
		{"no headers (curl)", "repo=a/b&number=3&filter=", http.StatusSeeOther, "/"},
		{"filter re-encoded", "repo=a/b&number=3&filter=" + url.QueryEscape("rec=human&risk=high&org=a&risk=low&risk=high&hideskipped=1&rec=human"), http.StatusSeeOther, "/?hideskipped=1&org=a&rec=human&risk=high&risk=low"},
		{"old mine field ignored", "repo=a/b&number=3&mine=1", http.StatusSeeOther, "/"},
		{"filter no active value", "repo=a/b&number=3&filter=" + url.QueryEscape("mine=0&org=&foo=bar"), http.StatusSeeOther, "/"},
		{"filter bad escape", "repo=a/b&number=3&filter=" + url.QueryEscape("org=a&repo=%zz"), http.StatusSeeOther, "/"},
		{"filter scheme-relative repo", "repo=a/b&number=3&filter=" + url.QueryEscape("org=x&repo=//evil.com"), http.StatusSeeOther, "/?org=x&repo=%2F%2Fevil.com"},
		{"filter CRLF", "repo=a/b&number=3&filter=" + url.QueryEscape("org=a%0d%0aSet-Cookie:%20x=1"), http.StatusSeeOther, "/?org=a%0D%0ASet-Cookie%3A+x%3D1"},
		{"filter extra params", "repo=a/b&number=3&filter=" + url.QueryEscape("foo=bar&org=a&next=http://evil.com"), http.StatusSeeOther, "/?org=a"},
		{"filter absolute URL", "repo=a/b&number=3&filter=" + url.QueryEscape("https://evil.com/?org=a"), http.StatusSeeOther, "/"},
		{"filter path", "repo=a/b&number=3&filter=" + url.QueryEscape("/../evil?mine=1"), http.StatusSeeOther, "/"},
		{"unknown PR", "repo=a/b&number=5", http.StatusNotFound, ""},
		{"from details", "repo=a/b&number=3&from=details&filter=mine%3D1", http.StatusSeeOther, "/pr/a/b/3"},
		{"from details, unknown PR", "repo=a/b&number=9&from=details", http.StatusNotFound, ""},
		{"bad number", "repo=a/b&number=x", http.StatusBadRequest, ""},
		{"missing repo", "number=3", http.StatusBadRequest, ""},
	} {
		cfg, p, q := reviewSetup(t)
		r := localReq(http.MethodPost, "/rerun", tc.form)
		rec := serve(cfg, "myreview.json", p, q, r)
		if rec.Code != tc.code || rec.Header().Get("Location") != tc.location {
			t.Errorf("%s: got %d %q, want %d %q", tc.name, rec.Code, rec.Header().Get("Location"), tc.code, tc.location)
		}
		want := statusDone
		if tc.code == http.StatusSeeOther {
			want = statusQueued
		}
		if got := q.statuses()[prKey(pullRequest{Repo: "a/b", Number: 3})].Status; got != want {
			t.Errorf("%s: a/b#3 status = %q, want %q", tc.name, got, want)
		}
	}
}

// A state-changing request with a foreign Origin is refused before any handler runs; the app's own origin,
// no Origin (htmx in the app) and reads from anywhere still work.
func TestForeignOrigin(t *testing.T) {
	for _, tc := range []struct {
		method, path, form, origin string
		code                       int
	}{
		{http.MethodPost, "/rerun", "repo=a/b&number=3", "https://evil.example", http.StatusForbidden},
		{http.MethodPost, "/rerun", "repo=a/b&number=3", "null", http.StatusForbidden},
		{http.MethodPost, "/rerun", "repo=a/b&number=3", "wails://localhost.evil", http.StatusForbidden},
		{http.MethodPut, "/pr/a/b/3/drafts/f0", "text=x", "https://evil.example", http.StatusForbidden},
		{http.MethodDelete, "/pr/a/b/3/drafts/c1", "", "https://evil.example", http.StatusForbidden},
		{http.MethodPost, "/rerun", "repo=a/b&number=3", appOrigin, http.StatusSeeOther},
		{http.MethodPost, "/rerun", "repo=a/b&number=3", "", http.StatusSeeOther},
		{http.MethodGet, "/", "", "https://evil.example", http.StatusOK},
	} {
		cfg, p, q := reviewSetup(t)
		r := localReq(tc.method, tc.path, tc.form)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		rec := serve(cfg, "myreview.json", p, q, r)
		if rec.Code != tc.code {
			t.Errorf("%s %s Origin %q = %d, want %d", tc.method, tc.path, tc.origin, rec.Code, tc.code)
		}
		want := statusDone
		if tc.code == http.StatusSeeOther {
			want = statusQueued
		}
		if got := q.statuses()[prKey(pullRequest{Repo: "a/b", Number: 3})].Status; got != want {
			t.Errorf("%s %s Origin %q: a/b#3 status = %q, want %q", tc.method, tc.path, tc.origin, got, want)
		}
	}
}

func TestRerunHTMX(t *testing.T) {
	errRow := func(msg string) string {
		return `<tr class="row-error"><td colspan="12"><p class="alert alert-error" role="alert">` + msg + `</p></td></tr>`
	}
	for _, tc := range []struct {
		name, form string
		code       int
		want       string // exact body, or a fragment of it for 200
	}{
		{"done PR", "repo=a/b&number=3&filter=" + url.QueryEscape("risk=High&mine=1&next=//evil.com&org=<x>"), http.StatusOK, ""},
		{"unknown PR", "repo=a/b&number=9", http.StatusNotFound, errRow("pull request not on the dashboard")},
		{"bad number", "repo=a/b&number=x", http.StatusBadRequest, errRow("repo and a numeric number are required")},
	} {
		cfg, p, q := reviewSetup(t)
		r := localReq(http.MethodPost, "/rerun", tc.form)
		r.Header.Set("HX-Request", "true")
		rec := serve(cfg, "myreview.json", p, q, r)
		body := rec.Body.String()
		if rec.Code != tc.code {
			t.Errorf("%s: code %d, want %d: %s", tc.name, rec.Code, tc.code, body)
			continue
		}
		st := q.statuses()["a/b#3"].Status
		switch tc.code {
		case http.StatusOK:
			mustContain(t, body, "<tr><td>"+star+`</td><td class="repo">`, `<span class="badge status-queued">queued</span>`, `id="rerun-a/b-3"`,
				`<input type="hidden" name="filter" value="mine=1&amp;org=%3Cx%3E&amp;risk=High">`,
				`<span id="pr-poll" hx-swap-oob="true" hidden hx-get="/?mine=1&amp;org=%3Cx%3E&amp;risk=High" hx-trigger="every 10s, myreview:refresh"`)
			if strings.Count(body, "<tr") != 1 || strings.Contains(body, "evil") || strings.Contains(body, "<x>") {
				t.Errorf("%s: want one row and no raw filter:\n%s", tc.name, body)
			}
		default:
			if body != tc.want || st != statusDone {
				t.Errorf("%s: got %q (a/b#3 %s), want %q and nothing queued", tc.name, body, st, tc.want)
			}
		}
	}
	// the details form posts with htmx: the app's WebKit does not follow a 303, so it gets HX-Redirect
	cfg, p, q := reviewSetup(t)
	r := localReq(http.MethodPost, "/rerun", "repo=a/b&number=3&from=details")
	r.Header.Set("HX-Request", "true")
	if rec := serve(cfg, "myreview.json", p, q, r); rec.Code != http.StatusOK || rec.Header().Get("HX-Redirect") != "/pr/a/b/3" || rec.Header().Get("Location") != "" || q.statuses()["a/b#3"].Status != statusQueued {
		t.Errorf("from details: %d, HX-Redirect %q, Location %q, a/b#3 %s", rec.Code, rec.Header().Get("HX-Redirect"), rec.Header().Get("Location"), q.statuses()["a/b#3"].Status)
	}
	cfg, p, q = skipSetup(t)
	r = localReq(http.MethodPost, "/rerun", "repo=a/b&number=5")
	r.Header.Set("HX-Request", "true")
	rec := serve(cfg, "myreview.json", p, q, r)
	if want := errRow("pull request is skipped by rule skip[1]"); rec.Code != http.StatusConflict || rec.Body.String() != want || len(q.fifo) != 0 {
		t.Errorf("skipped: got %d %q, fifo %v; want 409 %q", rec.Code, rec.Body.String(), q.fifo, want)
	}
}

func TestRerunGetNotAllowed(t *testing.T) {
	cfg, p, q := reviewSetup(t)
	if rec := serve(cfg, "myreview.json", p, q, localReq(http.MethodGet, "/rerun", "")); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /rerun = %d, want 405", rec.Code)
	}
}

func details(t *testing.T, cfg config, p *poller, q *reviewQueue, path string) (int, string) {
	t.Helper()
	rec := serve(cfg, "myreview.json", p, q, localReq(http.MethodGet, path, ""))
	return rec.Code, rec.Body.String()
}

func mustContain(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

// doneSetup is reviewSetup with a/b#3 done: findings of both steps, a point generated from a finding, human review, High risk.
func doneSetup(t *testing.T) (config, *poller, *reviewQueue, *job) {
	cfg, _, q := reviewSetup(t)
	var prs []pullRequest
	for n := 1; n <= 5; n++ {
		prs = append(prs, pullRequest{Repo: "a/b", Number: n, Created: day(n), Requested: true})
	}
	prs[2].HeadSHA = "abc123" // the result is current
	p := newPoller(cfg.Repositories, (&fakeFetch{prs: map[string][]pullRequest{"a/b": prs}}).fetch, noLogin)
	p.poll(context.Background())
	j := q.jobs[prKey(pullRequest{Repo: "a/b", Number: 3})]
	j.HeadSHA = "abc123"
	j.Summary = "Adds X.\nBecause Y."
	j.Findings = []taggedFinding{
		{Step: "security review", finding: finding{Severity: "Low", File: "z.go", Line: 1, Description: "sec low"}},
		{Step: "code review", finding: finding{Severity: "Low", File: "b.go", Line: 0, Description: "whole file"}},
		{Step: "code review", finding: finding{Severity: "High", File: "b.go", Line: 20, Description: "b20"}},
		{Step: "code review", finding: finding{Severity: "High", File: "b.go", Line: 3, Description: "b3"}},
		{Step: "code review", finding: finding{Severity: "Critical", File: "dir/a b#?%.go", Line: 7, Description: "crit\nsecond line", Title: "Crit title", SuggestedComment: "Please fix crit.", Tags: []string{"security", "bug"}}},
		{Step: "code review", finding: finding{Severity: "High", File: "a.go", Line: 9, Description: "a9"}},
	}
	j.riskDecision = riskDecision{Recommendation: recHuman, Risk: "High",
		Rationale: []string{"4 findings at or above the High threshold.", "Risk High: a Critical finding.", "Touches sensitive areas: auth.", "third reason"},
		Points:    []pointOfInterest{{File: "z.go", Line: 0, Reason: "second"}, {File: "dir/a b#?%.go", Line: 7, Reason: "Critical code review finding: crit\nsecond line"}, {File: "dir/a b#?%.go", Line: 7, Reason: "first", Title: "First point", Comment: "Why first?"}}}
	j.Threshold, j.ThresholdSource, j.SensitiveAreas, j.AreasSource = "High", "default", defaultSensitiveAreas, "default"
	return cfg, p, q, j
}

func TestDetailsDone(t *testing.T) {
	cfg, p, q, j := doneSetup(t)
	code, body := details(t, cfg, p, q, "/pr/a/b/3")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if strings.Contains(body, "http-equiv=\"refresh\"") {
		t.Error("done page refreshes")
	}
	mustContain(t, body, `<link rel="stylesheet" href="/myreview.css">`, "<div class=\"page\">\n<a class=\"back-link\" href=\"/\">Back to the dashboard</a>",
		`<script src="/static/htmx.min.js" defer></script>`, `<meta name="htmx-config" content='{"includeIndicatorStyles":false,"allowEval":false,"responseHandling":[{"code":"204","swap":false},{"code":"[23]..","swap":true},{"code":"^(400|409|500|502)$","swap":true,"error":true},{"code":"[45]..","swap":false,"error":true}]}'>`,
		`<h1 class="pr-title"><span class="repo">a/b</span> <span class="num">#3</span> </h1>`, "<li>Draft <strong>no</strong></li>",
		"<li>Reviewed <code class=\"sha\">abc123</code></li>\n<li><span class=\"badge status-done\">done</span></li>",
		`<a class="gh-link" href="">Open on GitHub ↗</a>`,
		`<input type="hidden" name="from" value="details"><button class="btn-sm btn-quiet">Re-run review</button></form>`,
		"<div class=\"decision-bar\">\n<div class=\"verdict\">\n<span class=\"rec rec-human\">Human review required</span>\n<span class=\"badge risk risk-high\">High risk</span>\n"+
			"<span class=\"verdict-counts\">1 critical, 3 high, 2 low findings · 2 points of interest · 1 security finding</span>\n"+
			"<span class=\"progress\" id=\"progress\" hx-swap-oob=\"true\">0 of 8 checked</span>\n"+
			"<div class=\"verdict-reasons\"><span class=\"text\">4 findings at or above the High threshold.</span><span class=\"text\">Touches sensitive areas: auth.</span></div>\n</div>",
		"<nav class=\"view-tabs\">\n<a href=\"?view=list\" aria-current=\"page\">What to check <span class=\"pill pill-quiet\">8</span></a>\n<a href=\"?view=diff\">Diff</a>\n</nav>",
		"<section class=\"context clamped\">\n<h2>Context</h2>\n<pre class=\"text\">Adds X.\nBecause Y.</pre>\n</section>",
		"<p class=\"policy muted\">Severity threshold High (default) · Sensitive areas: authentication, authorization, data model (default)</p>",
		`<li class="point" id="pt-f4"><input type="checkbox" class="point-check" id="chk-f4" aria-label="Mark point 1 as checked" hx-post="/pr/a/b/3/points/f4/checked" hx-vals='{"checked": "1"}' hx-swap="outerHTML"><details hx-get="/pr/a/b/3/points/f4" hx-trigger="toggle once" hx-target="find .point-body" hx-swap="outerHTML"><summary class="point-toggle"><span class="point-no">1</span><span class="point-kind"><span class="badge sev-critical">Critical</span><span class="badge plain tag">security</span><span class="badge plain tag">bug</span></span><span class="point-title" title="Crit title">Crit title</span><span class="point-loc" title="dir/a b#?%.go:7"><span>dir/a b#?%.go</span><span>:7</span></span><span class="point-meta"><span class="review-mark" data-review-mark="f4"></span><span class="state state-suggestion" id="state-f4" hx-swap-oob="true">Suggestion</span></span><span class="chev"></span></summary>`,
		"<div class=\"point-body\">\n<div class=\"point-tools\"><span class=\"muted\">Code review</span><span class=\"tip\"><button type=\"button\" class=\"tip-btn\" aria-label=\"Issue description\" aria-describedby=\"tip-i-f4\">ⓘ</button><span role=\"tooltip\" tabindex=\"0\" id=\"tip-i-f4\">crit\nsecond line</span></span><span class=\"tip\"><button type=\"button\" class=\"tip-btn\" data-restore=\"ta-f4\" aria-label=\"Restore the AI reviewer's comment\" aria-describedby=\"tip-r-f4\">↺</button><span role=\"tooltip\" tabindex=\"0\" id=\"tip-r-f4\">Please fix crit.</span></span></div>\n<div class=\"point-links\"><a href=\"/pr/a/b/3?view=diff&point=f4#d-f4\">Open in diff →</a><a class=\"loc\" href=\"https://github.com/a/b/blob/abc123/dir/a%20b%23%3F%25.go#L7\">dir/a b#?%.go:7</a><span class=\"sep\"></span><a class=\"point-nav\" href=\"#pt-f5\">Next →</a></div>\n<div class=\"editor\">\n<label class=\"label\" for=\"ta-f4\">Your comment on dir/a b#?%.go:7</label>\n<textarea id=\"ta-f4\" name=\"text\" rows=\"6\" hx-put=\"/pr/a/b/3/drafts/f4\" hx-trigger=\"input changed delay:800ms\" hx-target=\"#es-f4\" hx-swap=\"outerHTML\">\nPlease fix crit.</textarea>\n<div class=\"editor-actions\" id=\"ea-f4\"><button class=\"btn-sm\" type=\"button\" hx-post=\"/pr/a/b/3/drafts/f4/include\" hx-vals='{\"include\": \"1\"}' hx-target=\"#ea-f4\" hx-swap=\"outerHTML\">Add to review</button>\n<span class=\"editor-status\" id=\"es-f4\"></span></div>\n</div>\n</div></details></li>",
		"<form class=\"review-controls\" hx-get=\"/pr/a/b/3/review/summary\" hx-target=\"#dialog-slot\">\n<fieldset class=\"seg\"><legend class=\"visually-hidden\">Review decision</legend>\n"+
			"<input type=\"radio\" class=\"visually-hidden\" name=\"event\" value=\"APPROVE\" id=\"decision-approve\"><label for=\"decision-approve\">Approve</label>\n"+
			"<input type=\"radio\" class=\"visually-hidden\" name=\"event\" value=\"REQUEST_CHANGES\" id=\"decision-request\"><label for=\"decision-request\">Request changes</label>\n"+
			"<input type=\"radio\" class=\"visually-hidden\" name=\"event\" value=\"COMMENT\" id=\"decision-comment\" checked><label for=\"decision-comment\">Comment</label>\n</fieldset>\n"+
			"<button id=\"submit-count\" hx-swap-oob=\"true\">Submit review (0)</button>\n</form>\n<div id=\"pending-error\" role=\"alert\"></div>\n</div>",
		"<div id=\"dialog-slot\"></div>\n</div>\n</body>",
		`<span class="point-loc" title="b.go"><span>b.go</span></span><span class="point-meta">`, `<a class="loc" href="https://github.com/a/b/blob/abc123/b.go">b.go</a>`,
		"<div class=\"point-tools\"><span class=\"muted\">Security review</span><span class=\"tip\"><button type=\"button\" class=\"tip-btn\" aria-label=\"Issue description\" aria-describedby=\"tip-i-f0\">ⓘ</button><span role=\"tooltip\" tabindex=\"0\" id=\"tip-i-f0\">sec low</span></span><span class=\"tip\"><button type=\"button\" class=\"tip-btn\" data-restore=\"ta-f0\" aria-label=\"Restore the AI reviewer's comment\" aria-describedby=\"tip-r-f0\">↺</button><span role=\"tooltip\" tabindex=\"0\" id=\"tip-r-f0\">sec low</span></span></div>",
		`<span class="point-no">7</span><span class="point-kind"><span class="badge plain kind-point">Point</span></span><span class="point-title" title="second">second</span><span class="point-loc" title="z.go"><span>z.go</span></span>`,
		"<div class=\"point-body\">\n<div class=\"point-tools\"><span class=\"tip\"><button type=\"button\" class=\"tip-btn\" aria-label=\"Issue description\" aria-describedby=\"tip-i-p0\">ⓘ</button><span role=\"tooltip\" tabindex=\"0\" id=\"tip-i-p0\">second</span></span><span class=\"tip\"><button type=\"button\" class=\"tip-btn\" data-restore=\"ta-p0\" aria-label=\"Restore the AI reviewer's comment\" aria-describedby=\"tip-r-p0\">↺</button><span role=\"tooltip\" tabindex=\"0\" id=\"tip-r-p0\">second</span></span></div>\n<div class=\"point-links\"><a href=\"/pr/a/b/3?view=diff&point=p0#d-p0\">Open in diff →</a><a class=\"loc\" href=\"https://github.com/a/b/blob/abc123/z.go\">z.go</a><span class=\"sep\"></span><a class=\"point-nav\" href=\"#pt-f0\">← Previous</a><a class=\"point-nav\" href=\"#pt-p2\">Next →</a></div>\n<div class=\"editor\">", "Add to review</button>\n<span class=\"editor-status\" id=\"es-p0\"></span></div>\n</div>\n</div></details></li>")
	for _, no := range []string{"pt-p1", "Code review report", "Security report", "Risk recommendation", "Risk level", "Risk High: a Critical finding.", "third reason", "Points of interest", `class="count"`, "Approving needs confirmation", "Post as comment", "Approve anyway"} {
		if strings.Contains(body, no) {
			t.Errorf("body has %q:\n%s", no, body)
		}
	}
	// findings of both steps Critical to Low, then the points in decision order without the finding-derived one, numbered 1..N
	ids := []string{"f4", "f5", "f3", "f2", "f1", "f0", "p0", "p2"}
	if n := strings.Count(body, `<li class="point"`); n != len(ids) {
		t.Errorf("%d rows, want %d:\n%s", n, len(ids), body)
	}
	for i, id := range ids {
		row := body[strings.Index(body, `id="pt-`+id+`"`)+1:]
		row, _, _ = strings.Cut(row, `<li class="point"`)
		if !strings.Contains(row, fmt.Sprintf(`<span class="point-no">%d</span>`, i+1)) {
			t.Errorf("row %d is not %s:\n%s", i+1, id, body)
		}
	}
	j.Recommendation, j.Risk, j.Rationale = recAutomated, "Low", []string{"No finding at or above the High threshold and no sensitive area touched.", "Risk Low: automated review is sufficient."}
	_, auto := details(t, cfg, p, q, "/pr/a/b/3")
	mustContain(t, auto, "<div class=\"decision-bar compact\">\n<div class=\"verdict\">\n<span class=\"rec rec-auto\">Automated review is sufficient</span>\n<span class=\"badge risk risk-low\">Low risk</span>\n<span class=\"verdict-counts\">")
	if strings.Contains(auto, "verdict-reasons") || strings.Contains(auto, "Approve anyway") {
		t.Errorf("compact bar has reasons or a confirmation:\n%s", auto)
	}
}

func TestDetailsVerdictOnce(t *testing.T) {
	cfg, p, q := reviewSetup(t)
	j := q.jobs[prKey(pullRequest{Repo: "a/b", Number: 3})]
	j.Findings = []taggedFinding{{Step: "code review", finding: finding{Severity: "Medium", File: "a.go", Line: 2, Description: "d"}}}
	j.Threshold = "Medium"
	j.riskDecision = decideRisk(j.Findings, nil, nil, "Medium", nil)
	if j.Risk != "Medium" || j.Recommendation != recHuman {
		t.Fatalf("decision = %+v", j.riskDecision)
	}
	_, body := details(t, cfg, p, q, "/pr/a/b/3")
	for s, n := range map[string]int{"Human review required": 1, "risk-medium": 1, "Risk recommendation": 0, "Risk level": 0, "Risk Medium: human review required.": 0, "Automated review": 0} {
		if got := strings.Count(body, s); got != n {
			t.Errorf("%q %d times, want %d:\n%s", s, got, n, body)
		}
	}
	mustContain(t, body, "<div class=\"decision-bar\">", "<span class=\"verdict-counts\">1 medium finding · 0 points of interest · no security findings</span>",
		`<div class="verdict-reasons"><span class="text">1 finding at or above the Medium threshold.</span></div>`, "Submit review (0)")
}

func TestDetailsContextToggle(t *testing.T) {
	cfg, p, q := reviewSetup(t)
	j := q.jobs[prKey(pullRequest{Repo: "a/b", Number: 3})]
	const toggle = `<input type="checkbox" id="context-more" class="visually-hidden"><label for="context-more" class="link-btn"><span class="more">Show more</span><span class="less">Show less</span></label>`
	for summary, want := range map[string]bool{"a\nb": false, "a\nb\n": false, "a\nb\nc": true, strings.Repeat("x", 240): false, strings.Repeat("é", 241): true} {
		j.Summary = summary
		if _, body := details(t, cfg, p, q, "/pr/a/b/3"); strings.Contains(body, toggle) != want {
			t.Errorf("summary %q: toggle = %v, want %v", summary, !want, want)
		}
	}
}

func TestDetailsEmptyReports(t *testing.T) {
	cfg, p, q := reviewSetup(t)
	_, body := details(t, cfg, p, q, "/pr/a/b/3")
	mustContain(t, body, "<h2>Context</h2>\n<p>No overview.</p>\n</section>", "<p>Nothing to check: no findings and no points of interest.</p>",
		"<span class=\"verdict-counts\">No findings · 0 points of interest · no security findings</span>", `What to check <span class="pill pill-quiet">0</span>`)
	for _, no := range []string{"Security report", "Code review report", `class="points"`, "Show more"} {
		if strings.Contains(body, no) {
			t.Errorf("body has %q:\n%s", no, body)
		}
	}
}

func TestDetailsView(t *testing.T) {
	cfg, p, q, _ := doneSetup(t)
	for _, c := range []struct {
		query string
		diff  bool
	}{{"", false}, {"?view=", false}, {"?view=list", false}, {"?view=bogus", false}, {"?view=list&view=diff", false}, {"?view=diff", true}, {"?view=diff&view=list", true}} {
		code, body, _ := do(newHandler(newConfigHolder(cfg, "myreview.json"), p, q, nil, notApproved, emptyFiles, offlineContent, noThreads, noResolve), localReq(http.MethodGet, "/pr/a/b/3"+c.query, ""))
		list, diff := `<a href="?view=list" aria-current="page">`, `<a href="?view=diff" aria-current="page">`
		if c.diff {
			list, diff = diff, list
		}
		mustContain(t, body, "<div class=\"decision-bar\">", list)
		if code != http.StatusOK || strings.Contains(body, diff) || strings.Contains(body, `class="points"`) != !c.diff ||
			strings.Contains(body, `<div class="diff-layout">`) != c.diff || strings.Contains(body, "Context</h2>") == c.diff {
			t.Errorf("GET %q = %d, want 200 and the %v view:\n%s", c.query, code, map[bool]string{false: "list", true: "diff"}[c.diff], body)
		}
	}
}

func TestPointEndpoint(t *testing.T) {
	cfg, p, q, _ := doneSetup(t)
	for path, want := range map[string]string{
		"/pr/a/b/3/points/f0": "<div class=\"point-body\">\n<div class=\"point-tools\"><span class=\"muted\">Security review</span><span class=\"tip\"><button type=\"button\" class=\"tip-btn\" aria-label=\"Issue description\" aria-describedby=\"tip-i-f0\">ⓘ</button><span role=\"tooltip\" tabindex=\"0\" id=\"tip-i-f0\">sec low</span></span><span class=\"tip\"><button type=\"button\" class=\"tip-btn\" data-restore=\"ta-f0\" aria-label=\"Restore the AI reviewer's comment\" aria-describedby=\"tip-r-f0\">↺</button><span role=\"tooltip\" tabindex=\"0\" id=\"tip-r-f0\">sec low</span></span></div>\n<div class=\"excerpt\" hx-get=\"/pr/a/b/3/points/f0/excerpt\" hx-trigger=\"load\" hx-target=\"this\" hx-swap=\"outerHTML\"></div>\n<div class=\"point-links\"><a href=\"/pr/a/b/3?view=diff&point=f0#d-f0\">Open in diff →</a><a class=\"loc\" href=\"https://github.com/a/b/blob/abc123/z.go#L1\">z.go:1</a><span class=\"sep\"></span><a class=\"point-nav\" href=\"#pt-f1\">← Previous</a><a class=\"point-nav\" href=\"#pt-p0\">Next →</a></div>\n<div class=\"editor\">",
		"/pr/a/b/3/points/f4": "<span role=\"tooltip\" tabindex=\"0\" id=\"tip-i-f4\">crit\nsecond line</span>",
		"/pr/a/b/3/points/p2": "<div class=\"point-body\">\n<div class=\"point-tools\"><span class=\"tip\"><button type=\"button\" class=\"tip-btn\" aria-label=\"Issue description\" aria-describedby=\"tip-i-p2\">ⓘ</button><span role=\"tooltip\" tabindex=\"0\" id=\"tip-i-p2\">first</span></span><span class=\"tip\"><button type=\"button\" class=\"tip-btn\" data-restore=\"ta-p2\" aria-label=\"Restore the AI reviewer's comment\" aria-describedby=\"tip-r-p2\">↺</button><span role=\"tooltip\" tabindex=\"0\" id=\"tip-r-p2\">Why first?</span></span></div>\n<div class=\"excerpt\" hx-get=\"/pr/a/b/3/points/p2/excerpt\" hx-trigger=\"load\" hx-target=\"this\" hx-swap=\"outerHTML\"></div>\n<div class=\"point-links\"><a href=\"/pr/a/b/3?view=diff&point=p2#d-p2\">Open in diff →</a><a class=\"loc\" href=\"https://github.com/a/b/blob/abc123/dir/a%20b%23%3F%25.go#L7\">dir/a b#?%.go:7</a><span class=\"sep\"></span><a class=\"point-nav\" href=\"#pt-p0\">← Previous</a></div>\n<div class=\"editor\">\n<label class=\"label\" for=\"ta-p2\">Your comment on dir/a b#?%.go:7</label>",
	} {
		rec := serve(cfg, "myreview.json", p, q, localReq(http.MethodGet, path, ""))
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/html; charset=utf-8" || rec.Header().Get("Content-Security-Policy") != csp ||
			!strings.HasPrefix(rec.Body.String(), `<div class="point-body">`) || !strings.Contains(rec.Body.String(), want) || strings.Contains(rec.Body.String(), "<html") {
			t.Errorf("GET %s = %d %q, want 200 text/html with CSP and %q:\n%s", path, rec.Code, rec.Header().Get("Content-Type"), want, rec.Body.String())
		}
		// one editor, pre-filled; the description and the suggested comment only in tooltips
		if body := rec.Body.String(); strings.Count(body, "<textarea") != 1 || strings.Contains(body, "Suggested comment") || strings.Contains(body, `class="suggested"`) || strings.Contains(body, "point-text") {
			t.Errorf("GET %s: not exactly one editor, or the old blocks:\n%s", path, body)
		}
	}
	for _, path := range []string{"/pr/a/b/3/points/x1", "/pr/a/b/3/points/f999", "/pr/a/b/3/points/p3", "/pr/a/b/3/points/p-1", "/pr/a/b/3/points/f",
		"/pr/a/b/3/points/f01", "/pr/a/b/3/points/f+1", "/pr/a/b/1/points/f0", "/pr/a/b/4/points/f0", "/pr/a/b/9/points/f0", "/pr/a/b/x/points/f0"} {
		if rec := serve(cfg, "myreview.json", p, q, localReq(http.MethodGet, path, "")); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

func TestDetailsStates(t *testing.T) {
	cfg, p, q := reviewSetup(t)
	for n, want := range map[int]string{1: `<p class="alert alert-info"><span>Review queued.</span></p>`, 2: `<p class="alert alert-info"><span>Review running.</span></p>`, 4: `<p class="alert alert-error"><span>Review failed at code review: exit &lt;1&gt;.</span></p>`, 5: `<p class="alert alert-info"><span>Review not queued yet.</span></p>`} {
		code, body := details(t, cfg, p, q, fmt.Sprintf("/pr/a/b/%d", n))
		if code != http.StatusOK || !strings.Contains(body, "<div class=\"decision-bar\">\n<div class=\"verdict\">\n"+want+"\n</div>") {
			t.Errorf("#%d: got %d, want 200 and %q in the bar:\n%s", n, code, want, body)
		}
		badge := map[int]string{1: `<span class="badge status-queued">queued</span>`, 2: `<span class="badge status-running">running</span>`,
			4: `<span class="badge status-failed">failed: code review: exit &lt;1&gt;</span>`, 5: `<span class="badge status-not-queued">not queued</span>`}[n]
		mustContain(t, body, "<li>"+badge+"</li>", "</nav>\n<div id=\"dialog-slot\"></div>\n</div>\n</body>")
		if strings.Contains(body, `http-equiv="refresh"`) || !strings.Contains(body, `<div id="update-slot" role="status" hx-get="/pr/a/b/`+strconv.Itoa(n)+`/status?results=`+resultID(q.statuses()[fmt.Sprintf("a/b#%d", n)])+`&amp;head=&amp;view=list" hx-trigger="every 15s" hx-swap="innerHTML"></div>`) {
			t.Errorf("#%d: a meta refresh or no update poller", n)
		}
		if strings.Contains(body, "Context") || strings.Contains(body, "Nothing to check") || strings.Contains(body, "Risk") || strings.Contains(body, "Reviewed") {
			t.Errorf("#%d: results shown before done", n)
		}
	}
}

func TestDetailsEscaping(t *testing.T) {
	cfg := config{Repositories: []string{"a/b"}, Concurrency: 2}
	x := `<script>"'&`
	pr := pullRequest{Repo: "a/b", Number: 1, Title: "t" + x, Author: "u" + x, URL: "https://github.com/a/b/pull/1", Draft: true, Created: time.Now().Add(-2 * time.Hour)}
	p := newPoller(cfg.Repositories, (&fakeFetch{prs: map[string][]pullRequest{"a/b": {pr}}}).fetch, noLogin)
	p.poll(context.Background())
	q := newReviewQueue(0, workflow{}, resultStore{dir: t.TempDir()})
	q.jobs[prKey(pr)] = &job{pr: pr, jobStatus: jobStatus{Status: statusDone, HeadSHA: "s", Summary: "s" + x,
		Findings: []taggedFinding{{Step: "code review", finding: finding{Severity: "High", File: "javascript:alert(1)" + x, Line: 2, Description: "d" + x,
			Title: "ft" + x, SuggestedComment: "fc" + x}}},
		riskDecision: riskDecision{Rationale: []string{"why" + x}, Points: []pointOfInterest{{File: "p" + x, Line: 3, Reason: "reason" + x, Title: "pt" + x, Comment: "pc" + x}}}}}
	_, body := details(t, cfg, p, q, "/pr/a/b/1")
	esc := `&lt;script&gt;&#34;&#39;&amp;`
	mustContain(t, body, "t"+esc, "u"+esc, "s"+esc, "d"+esc, "why"+esc, "reason"+esc, `href="https://github.com/a/b/blob/s/p%3Cscript%3E%22%27&amp;#L3"`, "Draft <strong>yes</strong>", "Age <strong>2h</strong>",
		`href="https://github.com/a/b/blob/s/javascript:alert%281%29%3Cscript%3E%22%27&amp;#L2"`, `title="p`+esc+`:3"`,
		`<span class="point-title" title="ft`+esc+`">ft`+esc+`</span>`, `<span role="tooltip" tabindex="0" id="tip-r-f0">fc`+esc+`</span>`, "hx-swap=\"outerHTML\">\nfc"+esc+"</textarea>",
		`<span class="point-title" title="pt`+esc+`">pt`+esc+`</span>`, `<span role="tooltip" tabindex="0" id="tip-r-p0">pc`+esc+`</span>`)
	if strings.Contains(body, "<script>") {
		t.Errorf("raw script in body:\n%s", body)
	}
	q.jobs[prKey(pr)].jobStatus = jobStatus{Status: statusFailed, FailedStep: "security review", Reason: "r" + x}
	_, body = details(t, cfg, p, q, "/pr/a/b/1")
	mustContain(t, body, "Review failed at security review: r"+esc+".")
	q.jobs[prKey(pr)].jobStatus = jobStatus{Status: statusDone, Threshold: "t" + x, ThresholdSource: "s" + x, SensitiveAreas: []string{"a" + x, "b"}, AreasSource: "o" + x}
	_, body = details(t, cfg, p, q, "/pr/a/b/1")
	mustContain(t, body, "Severity threshold t"+esc+" (s"+esc+") · Sensitive areas: a"+esc+", b (o"+esc+")</p>")
	if strings.Contains(body, "<script>") {
		t.Errorf("raw script in body:\n%s", body)
	}
}

func TestDetailsPolicy(t *testing.T) {
	ptr := func(s string) *string { return &s }
	for _, tc := range []struct {
		name string
		pol  effectivePolicy
		want string
	}{
		{"no policy", resolvePolicy(nil, config{}), "Severity threshold High (default) · Sensitive areas: authentication, authorization, data model (default)"},
		{"full policy", resolvePolicy(&policy{SeverityThreshold: ptr("Medium"), SensitiveAreas: &[]sensitiveArea{{Name: "payments", Paths: []string{"billing/**"}}, {Name: "data model", Description: "d"}}}, config{}),
			"Severity threshold Medium (repository policy) · Sensitive areas: payments, data model (repository policy)"},
		{"partial policy", resolvePolicy(&policy{SeverityThreshold: ptr("Low")}, config{}),
			"Severity threshold Low (repository policy) · Sensitive areas: authentication, authorization, data model (default)"},
		{"empty areas", resolvePolicy(&policy{SensitiveAreas: &[]sensitiveArea{}}, config{}),
			"Severity threshold High (default) · Sensitive areas: none (repository policy)"},
	} {
		cfg, p, q := reviewSetup(t)
		q.jobs[prKey(pullRequest{Repo: "a/b", Number: 3})].jobStatus = jobStatus{Status: statusDone,
			Threshold: tc.pol.Threshold, ThresholdSource: tc.pol.ThresholdSource, SensitiveAreas: areaNames(tc.pol.Areas), AreasSource: tc.pol.AreasSource}
		_, body := details(t, cfg, p, q, "/pr/a/b/3")
		if !strings.Contains(body, "</section>\n<p class=\"policy muted\">"+tc.want+"</p>") {
			t.Errorf("%s: body missing %q:\n%s", tc.name, tc.want, body)
		}
	}
}

func TestDetailsFailedAtPolicy(t *testing.T) {
	cfg, p, q := reviewSetup(t)
	q.jobs[prKey(pullRequest{Repo: "a/b", Number: 3})].jobStatus = jobStatus{Status: statusFailed, FailedStep: "policy",
		Reason: `invalid .myreview.json on the default branch: severity_threshold "high" is not Low, Medium, High or Critical`}
	_, body := details(t, cfg, p, q, "/pr/a/b/3")
	mustContain(t, body, `<p class="alert alert-error"><span>Review failed at policy: invalid .myreview.json on the default branch: severity_threshold &#34;high&#34; is not Low, Medium, High or Critical.</span></p>`)
	if strings.Contains(body, "Severity threshold") {
		t.Errorf("failed run shows the threshold:\n%s", body)
	}
}

func TestDetailsNotFound(t *testing.T) {
	cfg, p, q := reviewSetup(t)
	for _, path := range []string{"/pr/a/b/9", "/pr/a/c/1", "/pr/a/b/x", "/pr/a/b/1.0", "/pr/a/b"} {
		if code, _ := details(t, cfg, p, q, path); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
	}
	before := newPoller(cfg.Repositories, nil, noLogin)
	if code, _ := details(t, cfg, before, q, "/pr/a/b/1"); code != http.StatusNotFound {
		t.Errorf("before the first poll = %d, want 404", code)
	}
}

func TestIndexOutdatedPrefix(t *testing.T) {
	cfg, p, q := reviewSetup(t)
	for n := 3; n <= 4; n++ {
		j := q.jobs[prKey(pullRequest{Repo: "a/b", Number: n})]
		j.Outdated = true
		j.riskDecision = riskDecision{Recommendation: recHuman, Risk: "Medium"}
	}
	body := serve(cfg, "myreview.json", p, q, localReq(http.MethodGet, "/", "")).Body.String()
	mustContain(t, body, "<td>"+ind("Outdated: review done: 0 critical, 0 high, 0 medium, 0 low", `<span class="badge plain">outdated</span><span class="dot zero">0</span><span class="dot zero">0</span><span class="dot zero">0</span><span class="dot zero">0</span>`)+"</td><td>"+ind("Risk: Medium", `<span class="meter risk-medium"></span>`)+"</td><td>"+ind("Human review required", `<span class="rec rec-human"></span>`)+"</td>",
		`<td><span class="badge status-failed">outdated: failed: code review: exit &lt;1&gt;</span></td><td></td><td></td>`)
}

func TestDetailsOutdated(t *testing.T) {
	cfg := config{Repositories: []string{"a/b"}, Concurrency: 2}
	prs := []pullRequest{{Repo: "a/b", Number: 1, HeadSHA: "1111111222"}, {Repo: "a/b", Number: 2, HeadSHA: "3333333444", Draft: true}}
	p := newPoller(cfg.Repositories, (&fakeFetch{prs: map[string][]pullRequest{"a/b": prs}}).fetch, noLogin)
	p.poll(context.Background())
	q := newReviewQueue(0, workflow{}, resultStore{dir: t.TempDir()})
	q.quietPeriod = 5 * time.Minute
	for _, pr := range prs {
		// Low risk, but the outdated result needs confirmation, so the bar stays full (not compact)
		q.jobs[prKey(pr)] = &job{pr: pr, jobStatus: jobStatus{Status: statusDone, HeadSHA: "abcdef0123", Outdated: true,
			riskDecision: riskDecision{Recommendation: recAutomated, Risk: "Low"}}}
	}
	_, body := details(t, cfg, p, q, "/pr/a/b/1")
	if strings.Contains(body, "http-equiv=\"refresh\"") {
		t.Error("outdated ready page refreshes; a reload would erase a comment being edited")
	}
	mustContain(t, body, `<p class="alert alert-warning"><span>These results are for commit <code class="sha">abcdef0</code>; the pull request is now at <code class="sha">1111111</code>. A new review is queued 5m0s after the last push.</span></p>`+"\n<div class=\"decision-bar\">")
	_, body = details(t, cfg, p, q, "/pr/a/b/2")
	mustContain(t, body, `<p class="alert alert-warning"><span>These results are for commit <code class="sha">abcdef0</code>; the pull request is now at <code class="sha">3333333</code>. Draft: click Re-run to review the new commits.</span></p>`+"\n<div class=\"decision-bar\">")
	if strings.Contains(body, "http-equiv=\"refresh\"") {
		t.Error("outdated draft page refreshes")
	}
	q.jobs[prKey(prs[0])].Outdated = false
	if _, body = details(t, cfg, p, q, "/pr/a/b/1"); strings.Contains(body, "These results") || strings.Contains(body, "http-equiv=\"refresh\"") {
		t.Errorf("current result shows the outdated line or refreshes:\n%s", body)
	}
}

func TestIndexOrder(t *testing.T) {
	cfg := config{Repositories: []string{"a/b", "c/d"}, Concurrency: 2}
	human := func(risk string) jobStatus {
		return jobStatus{Status: statusDone, riskDecision: riskDecision{Recommendation: recHuman, Risk: risk}}
	}
	auto := jobStatus{Status: statusDone, riskDecision: riskDecision{Recommendation: recAutomated, Risk: "Low"}}
	failed := jobStatus{Status: statusFailed, FailedStep: "prepare", Reason: "x"}
	outdated := jobStatus{Status: statusDone, Outdated: true, riskDecision: riskDecision{Recommendation: recHuman, Risk: "High"}}
	none := jobStatus{} // not known to the queue
	skipped := jobStatus{Status: statusSkipped, Reason: "title matches rule skip[0] x"}
	onDemand := jobStatus{Status: statusOnDemand, Reason: "created more than 168h0m0s ago"}
	// listed in the expected order; "a/b" and "c/d" PRs with the same age test the repo/number tie-break
	rows := []struct {
		repo  string
		n     int
		draft bool
		age   int // day of creation: lower is older
		st    jobStatus
		req   bool // review requested: these come first
	}{
		{"c/d", 16, false, 5, human("High"), true},
		{"a/b", 17, false, 6, human("High"), true}, // younger after older inside the requested part
		{"a/b", 18, false, 1, failed, true},        // a later group, still above every non-requested PR
		{"a/b", 19, true, 1, human("High"), true},  // a requested draft, still above every non-requested PR
		{"a/b", 13, false, 5, human("High"), false},
		{"c/d", 1, false, 5, human("High"), false},
		{"a/b", 3, false, 9, human("High"), false},
		{"a/b", 1, false, 1, human("Medium"), false}, // older, but Medium after High
		{"a/b", 2, false, 2, failed, false},
		{"a/b", 4, false, 1, auto, false},
		{"a/b", 5, false, 1, jobStatus{Status: statusQueued}, false},
		{"a/b", 6, false, 2, outdated, false}, // outdated ready waits for a review
		{"a/b", 7, false, 3, jobStatus{Status: statusRunning}, false},
		{"a/b", 8, false, 3, none, false},
		{"c/d", 2, false, 3, none, false},
		{"a/b", 20, false, 1, onDemand, false},    // on demand just above skipped
		{"a/b", 15, false, 1, skipped, false},     // skipped last in its section
		{"a/b", 9, true, 4, human("High"), false}, // drafts last, by the same groups
		{"a/b", 10, true, 1, failed, false},
		{"a/b", 11, true, 1, none, false},
		{"a/b", 12, true, 1, outdated, false},
		{"a/b", 21, true, 1, onDemand, false}, // after an outdated draft
		{"a/b", 14, true, 1, skipped, false},
	}
	byRepo := map[string][]pullRequest{}
	q := newReviewQueue(0, workflow{}, resultStore{dir: t.TempDir()})
	var want []string
	for i := len(rows) - 1; i >= 0; i-- { // fetch in reverse so the result is not the input order
		r := rows[i]
		pr := pullRequest{Repo: r.repo, Number: r.n, Draft: r.draft, Created: day(r.age), Requested: r.req}
		byRepo[r.repo] = append(byRepo[r.repo], pr)
		if r.st.Status != "" {
			q.jobs[prKey(pr)] = &job{pr: pr, jobStatus: r.st}
		}
	}
	var wantMine []string
	for _, r := range rows {
		w := fmt.Sprintf(`href="/pr/%s/%d"`, r.repo, r.n)
		want = append(want, w)
		if r.req {
			wantMine = append(wantMine, w)
		}
	}
	p := newPoller(cfg.Repositories, (&fakeFetch{prs: byRepo}).fetch, noLogin)
	p.poll(context.Background())
	for path, want := range map[string][]string{"/": want, "/?mine=1": wantMine} {
		body := serve(cfg, "myreview.json", p, q, localReq(http.MethodGet, path, "")).Body.String()
		last := -1
		for _, w := range want {
			i := strings.Index(body, w)
			if i <= last {
				t.Errorf("%s: %s at %d, want after %d", path, w, i, last)
			}
			last = i
		}
	}
}

// skipSetup is reviewSetup with a/b#5 skipped by a label rule whose label needs escaping.
func skipSetup(t *testing.T) (config, *poller, *reviewQueue) {
	cfg, p, q := reviewSetup(t)
	pr := pullRequest{Repo: "a/b", Number: 5}
	q.jobs[prKey(pr)] = &job{pr: pr, jobStatus: jobStatus{Status: statusSkipped, SkipRule: 1, Reason: `label "<dep>" matches rule skip[1] ^<dep>$`}}
	return cfg, p, q
}

func TestIndexSkipped(t *testing.T) {
	cfg, p, q := skipSetup(t)
	body := serve(cfg, "myreview.json", p, q, localReq(http.MethodGet, "/", "")).Body.String()
	mustContain(t, body, `<td><span class="badge plain" title="skipped: label &#34;&lt;dep&gt;&#34; matches rule skip[1] ^&lt;dep&gt;$">skipped</span></td><td></td><td></td><td class="actions"><a class="btn btn-sm btn-quiet" id="details-a/b-5" href="/pr/a/b/5">Details</a> </td></tr>`)
	if strings.Contains(body, `name="number" value="5"`) {
		t.Errorf("Re-run button shown for a skipped PR:\n%s", body)
	}
}

func TestRerunSkipped(t *testing.T) {
	cfg, p, q := skipSetup(t)
	rec := serve(cfg, "myreview.json", p, q, localReq(http.MethodPost, "/rerun", "repo=a/b&number=5"))
	if rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != "pull request is skipped by rule skip[1]" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if st := q.statuses()["a/b#5"].Status; st != statusSkipped || len(q.fifo) != 0 {
		t.Fatalf("status %q, fifo %v", st, q.fifo)
	}
}

func TestDetailsSkipped(t *testing.T) {
	cfg, p, q := skipSetup(t)
	_, body := details(t, cfg, p, q, "/pr/a/b/5")
	mustContain(t, body, `<li><span class="badge plain">skipped: label &#34;&lt;dep&gt;&#34; matches rule skip[1] ^&lt;dep&gt;$</span></li>`,
		`<p class="alert alert-info"><span>skipped: label &#34;&lt;dep&gt;&#34; matches rule skip[1] ^&lt;dep&gt;$</span></p>`)
	for _, no := range []string{`action="/rerun"`, `http-equiv="refresh"`} {
		if strings.Contains(body, no) {
			t.Errorf("skipped details page has %s:\n%s", no, body)
		}
	}
}

// onDemandSetup is reviewSetup with a/b#5 on demand.
func onDemandSetup(t *testing.T) (config, *poller, *reviewQueue) {
	cfg, p, q := reviewSetup(t)
	pr := pullRequest{Repo: "a/b", Number: 5}
	q.jobs[prKey(pr)] = &job{pr: pr, jobStatus: jobStatus{Status: statusOnDemand, Reason: "created more than 168h0m0s ago"}}
	return cfg, p, q
}

func TestIndexOnDemand(t *testing.T) {
	cfg, p, q := onDemandSetup(t)
	body := serve(cfg, "myreview.json", p, q, localReq(http.MethodGet, "/", "")).Body.String()
	mustContain(t, body, `<span class="badge plain" title="on demand: created more than 168h0m0s ago">on demand</span>`,
		`<button class="btn-sm btn-quiet" id="rerun-a/b-5">Review</button>`)
}

func TestRerunOnDemand(t *testing.T) {
	cfg, p, q := onDemandSetup(t)
	r := localReq(http.MethodPost, "/rerun", "repo=a/b&number=5")
	r.Header.Set("HX-Request", "true")
	rec := serve(cfg, "myreview.json", p, q, r)
	if rec.Code != http.StatusOK || q.statuses()["a/b#5"].Status != statusQueued {
		t.Fatalf("got %d, a/b#5 %s:\n%s", rec.Code, q.statuses()["a/b#5"].Status, rec.Body.String())
	}
	mustContain(t, rec.Body.String(), `<span class="badge status-queued">queued</span>`, `id="rerun-a/b-5">Re-run</button>`)
}

func TestDetailsOnDemand(t *testing.T) {
	cfg, p, q := onDemandSetup(t)
	_, body := details(t, cfg, p, q, "/pr/a/b/5")
	mustContain(t, body, `<p class="alert alert-info">Not reviewed automatically: created more than 168h0m0s ago. Click Review to start it.</p>`,
		`<input type="hidden" name="from" value="details"><button class="btn-sm btn-quiet">Review</button>`)
}

func TestStylesheet(t *testing.T) {
	want, err := os.ReadFile("static/myreview.css")
	if err != nil {
		t.Fatal(err)
	}
	rec := get(t, config{Concurrency: 2}, "myreview.json", "/myreview.css")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/css; charset=utf-8" || !bytes.Equal(rec.Body.Bytes(), want) {
		t.Errorf("GET /myreview.css = %d %q, %d bytes; want 200 text/css with the %d bytes of static/myreview.css",
			rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len(), len(want))
	}
}

// Titles line up down the "What to check" list only if the severity/tags column
// has the same fixed width on every row; each row is its own grid.
func TestPointKindColumnFixed(t *testing.T) {
	css, err := os.ReadFile("static/myreview.css")
	if err != nil {
		t.Fatal(err)
	}
	rules := regexp.MustCompile(`\.point-toggle \{[^}]*grid-template-columns: ([^;]+);`).FindAllSubmatch(css, -1)
	if len(rules) != 2 {
		t.Fatalf("found %d .point-toggle grid rules, want 2 (wide and narrow)", len(rules))
	}
	for _, r := range rules {
		if tracks := strings.Fields(string(r[1])); len(tracks) < 2 || !regexp.MustCompile(`^\d+px$`).MatchString(tracks[1]) {
			t.Errorf(".point-toggle columns %q: the severity/tags track must be a fixed px width", r[1])
		}
	}
}

func TestHTMX(t *testing.T) {
	want, err := os.ReadFile("static/htmx.min.js")
	if err != nil {
		t.Fatal(err)
	}
	rec := get(t, config{Concurrency: 2}, "myreview.json", "/static/htmx.min.js")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/javascript; charset=utf-8" || len(want) == 0 ||
		!bytes.Equal(rec.Body.Bytes(), want) || !bytes.Equal(htmxJS, want) {
		t.Errorf("GET /static/htmx.min.js = %d %q, %d bytes; want 200 text/javascript with the %d bytes of static/htmx.min.js",
			rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len(), len(want))
	}
}

func TestDashboardJS(t *testing.T) {
	want, err := os.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatal(err)
	}
	rec := get(t, config{Concurrency: 2}, "myreview.json", "/static/dashboard.js")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/javascript; charset=utf-8" || len(want) == 0 || !bytes.Equal(rec.Body.Bytes(), want) {
		t.Errorf("GET /static/dashboard.js = %d %q, %q; want 200 text/javascript with static/dashboard.js", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	// htmx first, then dashboard.js, which checks window.htmx
	mustContain(t, get(t, config{Concurrency: 2}, "myreview.json", "/").Body.String(),
		`<meta name="htmx-config" content='{"includeIndicatorStyles":false,"allowEval":false,"historyCacheSize":0,"refreshOnHistoryMiss":true}'>
<script src="/static/htmx.min.js" defer></script>
<script src="/static/dashboard.js" defer></script>
<script src="/static/links.js" defer></script>
<script type="module" src="/wails/runtime.js"></script>`)
	want, err = os.ReadFile("static/links.js")
	if err != nil {
		t.Fatal(err)
	}
	if rec := get(t, config{Concurrency: 2}, "myreview.json", "/static/links.js"); rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/javascript; charset=utf-8" || !bytes.Equal(rec.Body.Bytes(), want) {
		t.Errorf("GET /static/links.js = %d %q; want 200 text/javascript with static/links.js", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestCSP(t *testing.T) {
	const want = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
	cfg, p, q := reviewSetup(t)
	j := q.jobs[prKey(pullRequest{Repo: "a/b", Number: 3})]
	j.Summary = "Adds X."
	j.Findings = []taggedFinding{{Step: "code review", finding: finding{Severity: "High", File: "a.go", Line: 1, Description: "d"}}}
	j.riskDecision = riskDecision{Recommendation: recHuman, Risk: "High", Rationale: []string{"Risk High."}}
	h := newHandler(newConfigHolder(cfg, "myreview.json"), p, q, nil, notApproved, emptyFiles, offlineContent, noThreads, noResolve)
	// every response, error and redirect included, carries the CSP and no-store (WebKit caches scheme-handler responses)
	for _, r := range []*http.Request{
		localReq(http.MethodGet, "/", ""), localReq(http.MethodGet, "/pr/a/b/3", ""), localReq(http.MethodGet, "/pr/a/b/3?view=diff", ""),
		localReq(http.MethodGet, "/pr/a/b/3/points/f0", ""), localReq(http.MethodGet, "/static/links.js", ""), localReq(http.MethodGet, "/healthz", ""),
		localReq(http.MethodGet, "/nope", ""), localReq(http.MethodPost, "/rerun", "repo=a/b&number=3"), localReq(http.MethodPost, "/rerun", "number=x"),
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if got, cc := rec.Header().Get("Content-Security-Policy"), rec.Header().Get("Cache-Control"); got != want || cc != "no-store" {
			t.Errorf("%s %s = %d, CSP %q, Cache-Control %q; want %q and no-store", r.Method, r.URL, rec.Code, got, cc, want)
		}
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			continue
		}
		// the pages load only the vendored htmx, their own script, links.js and the Wails runtime
		body := rec.Body.String()
		for _, tag := range []string{`<script src="/static/htmx.min.js" defer></script>`, `<script src="/static/details.js" defer></script>`, `<script src="/static/dashboard.js" defer></script>`,
			`<script src="/static/links.js" defer></script>`, `<script type="module" src="/wails/runtime.js"></script>`} {
			body = strings.Replace(body, tag, "", 1)
		}
		if strings.Contains(body, "<style") || strings.Contains(body, "style=") || strings.Contains(body, "<script") {
			t.Errorf("GET %s has inline styles or scripts:\n%s", r.URL, body)
		}
	}
}

func TestClassFuncs(t *testing.T) {
	for _, tc := range []struct {
		st   jobStatus
		want string
	}{
		{jobStatus{}, "status-not-queued"},
		{jobStatus{Status: statusQueued}, "status-queued"},
		{jobStatus{Status: statusRunning}, "status-running"},
		{jobStatus{Status: statusDone}, "status-done"},
		{jobStatus{Status: statusFailed}, "status-failed"},
		{jobStatus{Status: statusSkipped}, "plain"},
		{jobStatus{Status: statusDone, Outdated: true}, "status-done"},
		{jobStatus{Status: statusFailed, Outdated: true}, "status-failed"},
	} {
		if got := statusClass(tc.st); got != tc.want {
			t.Errorf("statusClass(%q outdated=%v) = %q, want %q", tc.st.Status, tc.st.Outdated, got, tc.want)
		}
	}
	for rec, want := range map[string]string{recHuman: "rec-human", recAutomated: "rec-auto"} {
		if got := recClass(rec); got != want {
			t.Errorf("recClass(%q) = %q, want %q", rec, got, want)
		}
	}
}

func TestCheckItemTitleAndComment(t *testing.T) {
	st := jobStatus{Findings: []taggedFinding{
		{Step: "code review", finding: finding{Severity: "High", File: "a.go", Line: 1, Description: "\n  Nil  map\nwrite", Title: "", SuggestedComment: ""}},
		{Step: "code review", finding: finding{Severity: "Low", File: "b.go", Line: 2, Description: "d", Title: "T", SuggestedComment: "C"}},
	}, riskDecision: riskDecision{Points: []pointOfInterest{
		{File: "c.go", Reason: "why\nthis"},
		{File: "d.go", Reason: "r", Title: "PT", Comment: "PC"},
	}}}
	r := newReport("a/b", 1, st)
	want := map[string][2]string{"f0": {"Nil map", "\n  Nil  map\nwrite"}, "f1": {"T", "C"}, "p0": {"why", "why\nthis"}, "p1": {"PT", "PC"}}
	items := checkItems(r, st, pendingPR{})
	if len(items) != 4 {
		t.Fatalf("checkItems = %+v", items)
	}
	for _, it := range items {
		if got := [2]string{it.Title, it.Comment}; got != want[it.ID] {
			t.Errorf("checkItems %s = %q, want %q", it.ID, got, want[it.ID])
		}
	}
	for id, w := range want {
		if it, ok := lookupItem(r, st, pendingPR{}, id); !ok || [2]string{it.Title, it.Comment} != w {
			t.Errorf("lookupItem %s = %q, %q, %v; want %q", id, it.Title, it.Comment, ok, w)
		}
	}
}

// The details back link returns to the dashboard with the filters of the latest GET / of the same handler (URE-108).
func TestDetailsBackLink(t *testing.T) {
	cfg, p, q, _ := doneSetup(t)
	os.MkdirAll(q.pending.dir(), 0700)
	files := func(context.Context, string, int) ([]prFile, error) { return nil, errors.New("offline") }
	h := newHandler(newConfigHolder(cfg, "myreview.json"), p, q, nil, notApproved, files, noContent, noThreads, noResolve)
	do := func(r *http.Request) string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Body.String()
	}
	back := func(path, want string) {
		t.Helper()
		body := do(localReq(http.MethodGet, path, ""))
		mustContain(t, body, `<a class="back-link" href="`+want+`">Back to the dashboard</a>`)
		if strings.Contains(body, "evil.com") {
			t.Errorf("%s: body contains evil.com", path)
		}
	}
	back("/pr/a/b/3", "/") // no GET / since startup
	do(localReq(http.MethodGet, "/?risk=high&mine=1", ""))
	for _, path := range []string{"/pr/a/b/3", "/pr/a/b/3?view=diff", "/pr/a/b/3?submitted=APPROVE&n=0"} {
		back(path, "/?mine=1&amp;risk=high")
	}
	htmx := localReq(http.MethodGet, "/?org=x", "")
	htmx.Header.Set("HX-Request", "true")
	do(htmx)
	back("/pr/a/b/3", "/?org=x")
	rerun := localReq(http.MethodPost, "/rerun", "repo=a/b&number=3&filter="+url.QueryEscape("risk=high"))
	rerun.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, rerun)
	if rec.Code != http.StatusOK {
		t.Fatalf("/rerun: code %d, want 200 (must reach the filter code): %s", rec.Code, rec.Body)
	}
	back("/pr/a/b/3", "/?org=x") // /rerun is not a dashboard navigation
	do(localReq(http.MethodGet, "/", ""))
	back("/pr/a/b/3", "/")
	do(localReq(http.MethodGet, "/?next=//evil.com&org=x", ""))
	back("/pr/a/b/3", "/?org=x")
}

func TestFormatMetrics(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{formatCost(0.42), "$0.42"}, {formatCost(0.004999), "<$0.01"}, {formatCost(0.005), "$0.01"}, {formatCost(0), "$0.00"}, {formatCost(12.345), "$12.35"},
		{formatMs(0), "0 ms"}, {formatMs(850), "850 ms"}, {formatMs(12_400), "12s"}, {formatMs(65_000), "1m 05s"}, {formatMs(192_000), "3m 12s"}, {formatMs(3_720_000), "1h 02m"},
		{formatTokens(950), "950"}, {formatTokens(1000), "1k"}, {formatTokens(23_700), "23.7k"}, {formatTokens(999_960), "1M"}, {formatTokens(1_234_567), "1.2M"},
		{formatBytes(900), "900 B"}, {formatBytes(38 * 1024), "38 KiB"}, {formatBytes(3 << 20), "3.0 MiB"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

func TestDetailsMetrics(t *testing.T) {
	usd := func(f float64) *float64 { return &f }
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	claude := stepMetrics{Step: "code review", Agent: "claude", WallMs: 61_000, Status: statusDone, Items: 3, Usage: &agentUsage{
		Models: []modelUsage{{Model: "claude-opus-5-5"}, {Model: "claude-haiku-5-5"}}, APIMs: 58_000, Turns: 4,
		tokens: tokens{InputTokens: 1200, OutputTokens: 23_700, CacheReadTokens: 1_000_000, CacheWriteTokens: 950}, CostUSD: usd(0.40)}}
	claude2 := stepMetrics{Step: "risk assessment", Agent: "claude", WallMs: 850, Status: statusDone, Items: 1, Usage: &agentUsage{
		Models: []modelUsage{{Model: "claude-opus-5-5"}}, Turns: 1, tokens: tokens{InputTokens: 100}, CostUSD: usd(0.02)}}
	codex := stepMetrics{Step: "security review", Agent: "codex", WallMs: 30_000, Status: statusDone, Usage: &agentUsage{
		Models: []modelUsage{{Model: "codex default"}}, Turns: 2, tokens: tokens{InputTokens: 5000, OutputTokens: 100}}}
	killed := stepMetrics{Step: "security review", Agent: "claude", WallMs: 600_000, Status: statusFailed, ErrorKind: errTimeout, Error: "timed out after 10m"}
	run := func(steps ...stepMetrics) *reviewMetrics {
		return &reviewMetrics{QueuedAt: t0, StartedAt: t0.Add(12 * time.Second), FinishedAt: t0.Add(12*time.Second + 192*time.Second),
			Attempt: 2, DiffFiles: 14, DiffBytes: 38 * 1024, Steps: steps}
	}
	for _, tc := range []struct {
		name   string
		st     jobStatus
		want   []string
		absent []string
	}{
		{"claude two models", jobStatus{Status: statusDone, Metrics: run(claude, claude2)},
			[]string{"AI review: $0.42 · 3m 12s · claude-opus-5-5, claude-haiku-5-5 · 1M tokens",
				`<td>code review</td><td>claude</td><td>claude-opus-5-5, claude-haiku-5-5</td><td class="n">1m 01s</td><td class="n">58s</td><td class="n">4</td><td class="n">1.2k</td><td class="n">23.7k</td><td class="n">1M</td><td class="n">950</td><td class="n">$0.40</td><td class="result">3 findings</td>`,
				`<td class="n">850 ms</td>`, `<td class="result">1 finding</td>`, "Queued 12s · Attempt 2 · Diff 14 files, 38 KiB", "<details class=\"post\"><summary>Run details</summary>"},
			[]string{"no price", "unknown"}},
		{"mixed", jobStatus{Status: statusDone, Metrics: run(claude, codex)},
			[]string{"AI review: $0.40 &#43; codex (no price) · 3m 12s · claude-opus-5-5, claude-haiku-5-5, codex default · ", `<td>codex default</td><td class="n">30s</td><td class="n">—</td><td class="n">2</td>`, `<td class="n">no price</td>`}, nil},
		{"only codex", jobStatus{Status: statusDone, Metrics: run(codex)},
			[]string{"AI review: no price · 3m 12s · codex default · 5.1k tokens"}, []string{"AI review: $", ">$"}},
		{"killed step", jobStatus{Status: statusDone, Metrics: run(claude, killed)},
			[]string{"AI review: $0.40 &#43; unknown · ", `<td>security review</td><td>claude</td><td>—</td><td class="n">10m 00s</td><td class="n">—</td><td class="n">—</td><td class="n">—</td><td class="n">—</td><td class="n">—</td><td class="n">—</td><td class="n">unknown</td><td class="result">timed out after 10m</td>`}, nil},
		{"failed", jobStatus{Status: statusFailed, FailedStep: "security review", Reason: "timed out after 10m", Metrics: run(killed)},
			[]string{"Review failed at security review", "AI review: unknown · 3m 12s · 0 tokens"}, []string{"$0.00"}},
		{"no metrics", jobStatus{Status: statusDone}, nil, []string{"AI review", "Run details"}},
		{"failed no metrics", jobStatus{Status: statusFailed, FailedStep: "prepare", Reason: "x"}, nil, []string{"AI review", "Run details"}},
	} {
		cfg, p, q := reviewSetup(t)
		q.jobs[prKey(pullRequest{Repo: "a/b", Number: 3})].jobStatus = tc.st
		_, body := details(t, cfg, p, q, "/pr/a/b/3")
		for _, w := range tc.want {
			if !strings.Contains(body, w) {
				t.Errorf("%s: body missing %q:\n%s", tc.name, w, body)
			}
		}
		for _, a := range tc.absent {
			if strings.Contains(body, a) {
				t.Errorf("%s: body has %q", tc.name, a)
			}
		}
	}
}

func TestPRSize(t *testing.T) {
	cfg := config{Repositories: []string{"a/b"}, Concurrency: 2}
	f := &fakeFetch{prs: map[string][]pullRequest{"a/b": {
		{Repo: "a/b", Number: 1, HeadSHA: "h1", Additions: 1234, Deletions: 56, ChangedFiles: 7, HasSize: true},
		{Repo: "a/b", Number: 2, HeadSHA: "h2"}, // no size data
		{Repo: "a/b", Number: 3, HeadSHA: "h3", ChangedFiles: 1, HasSize: true},
	}}}
	p := newPoller(cfg.Repositories, f.fetch, noLogin)
	p.poll(context.Background())
	q := newReviewQueue(0, workflow{}, resultStore{dir: t.TempDir()})
	const tip = `title="1234 lines added, 56 removed, 7 files changed"`
	body := bodyOf(t, cfg, p)
	mustContain(t, body, `<td class="age">`, `<td><span class="pr-size" `+tip+`><span class="stat-add">+1.2k</span> <span class="stat-del">−56</span></span></td>`,
		`<span class="stat-add">+0</span> <span class="stat-del">−0</span>`)
	if n := strings.Count(body, `class="pr-size"`); n != 2 {
		t.Errorf("want 2 size cells (#1, #3), got %d", n)
	}
	_, d := details(t, cfg, p, q, "/pr/a/b/1")
	mustContain(t, d, `<li `+tip+`>Size <strong><span class="stat-add">+1.2k</span> <span class="stat-del">−56</span></strong> in 7 files</li>`)
	_, d = details(t, cfg, p, q, "/pr/a/b/3")
	mustContain(t, d, `in 1 file</li>`)
	if _, d = details(t, cfg, p, q, "/pr/a/b/2"); strings.Contains(d, "Size <strong>") {
		t.Errorf("PR without size data shows a Size entry:\n%s", d)
	}

	// A push: the next poll returns a new head and new numbers. A new slice, so only the second poll can deliver them.
	f.prs["a/b"] = []pullRequest{{Repo: "a/b", Number: 1, HeadSHA: "h1b", Additions: 950, Deletions: 23_700, ChangedFiles: 8, HasSize: true}}
	p.poll(context.Background())
	mustContain(t, bodyOf(t, cfg, p), `<span class="stat-add">+950</span> <span class="stat-del">−23.7k</span>`)
	_, d = details(t, cfg, p, q, "/pr/a/b/1")
	mustContain(t, d, `title="950 lines added, 23700 removed, 8 files changed">Size <strong><span class="stat-add">+950</span> <span class="stat-del">−23.7k</span></strong> in 8 files</li>`)
	if strings.Contains(d, "+1.2k") {
		t.Error("details page still shows the old numbers")
	}
}
