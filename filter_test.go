package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestParseFilterAndQuery(t *testing.T) {
	for _, c := range []struct {
		raw, want string
		active    bool
	}{
		{"", "", false},
		{"other=1&mine=0&mine=x&hideskipped=yes&hideapproved=true&org=&risk=&rec=", "", false},
		{"mine=1", "mine=1", true},
		{"mine=1&mine=0", "mine=1", true}, // first value
		{"org=a&org=b&repo=a/x&author=app/bot", "author=app%2Fbot&org=a&repo=a%2Fx", true},
		{"risk=High&risk=High&risk=bogus&risk=Low&rec=x&rec=human&rec=human", "rec=human&rec=x&risk=High&risk=Low&risk=bogus", true},
		{"hideskipped=1&hideapproved=1", "hideapproved=1&hideskipped=1", true},
		{"org=%3Cscript%3E%26repo%3D%2F%2Fevil", "org=%3Cscript%3E%26repo%3D%2F%2Fevil", true},
	} {
		v, err := url.ParseQuery(c.raw)
		if err != nil {
			t.Fatal(err)
		}
		f := parseFilter(v)
		if got := f.query(); got != c.want || f.active() != c.active {
			t.Errorf("%q: query %q active %v, want %q %v (filter %+v)", c.raw, got, f.active(), c.want, c.active, f)
		}
	}
}

func TestFilterMatch(t *testing.T) {
	pr := pullRequest{Repo: "acme/web", Author: "app/dependabot", Requested: true}
	approvedMe := pr
	approvedMe.Reviews = []approval{{Login: "alice", State: "APPROVED"}, {Login: "ME", State: "APPROVED"}}
	approvedMeOld := pr
	approvedMeOld.Reviews = []approval{{Login: "me", Outdated: true, State: "APPROVED"}, {Login: "alice", State: "APPROVED"}}
	changesMe := pr
	changesMe.Reviews = []approval{{Login: "alice", State: "APPROVED"}, {Login: "me", State: "CHANGES_REQUESTED"}}
	notMine := pr
	notMine.Requested = false
	human := jobStatus{Status: statusDone, riskDecision: riskDecision{Risk: "High", Recommendation: recHuman}}
	auto := jobStatus{Status: statusDone, riskDecision: riskDecision{Risk: "Low", Recommendation: recAutomated}}
	outdated := jobStatus{Status: statusDone, riskDecision: riskDecision{Risk: "Medium", Recommendation: recHuman}, Outdated: true}
	risky := jobStatus{riskDecision: riskDecision{Risk: "High", Recommendation: recHuman}} // no done review: risk fields must be ignored
	for _, c := range []struct {
		name  string
		f     dashFilter
		pr    pullRequest
		st    jobStatus
		known bool
		login string
		want  bool
	}{
		{"no filter, unknown PR", dashFilter{}, pr, jobStatus{}, false, "", true},
		{"org", dashFilter{Org: "acme"}, pr, human, true, "", true},
		{"org other", dashFilter{Org: "other"}, pr, human, true, "", false},
		{"org case differs", dashFilter{Org: "Acme"}, pr, human, true, "", false},
		{"org script", dashFilter{Org: "<script>"}, pr, human, true, "", false},
		{"repo", dashFilter{Repo: "acme/web"}, pr, human, true, "", true},
		{"repo other", dashFilter{Repo: "acme/api"}, pr, human, true, "", false},
		{"author", dashFilter{Author: "app/dependabot"}, pr, human, true, "", true},
		{"author other", dashFilter{Author: "dependabot"}, pr, human, true, "", false},
		{"risk", dashFilter{Risk: []string{"High"}}, pr, human, true, "", true},
		{"risk other", dashFilter{Risk: []string{"Low"}}, pr, human, true, "", false},
		{"risk OR", dashFilter{Risk: []string{"High", "Low"}}, pr, auto, true, "", true},
		{"risk bogus", dashFilter{Risk: []string{"bogus"}}, pr, human, true, "", false},
		{"risk mixed bogus", dashFilter{Risk: []string{"High", "bogus"}}, pr, human, true, "", true},
		{"risk outdated done keeps risk", dashFilter{Risk: []string{"Medium"}}, pr, outdated, true, "", true},
		{"risk not done", dashFilter{Risk: []string{"High"}}, pr, risky, true, "", false},
		{"risk queued", dashFilter{Risk: []string{"High"}}, pr, jobStatus{Status: statusQueued, riskDecision: riskDecision{Risk: "High"}}, true, "", false},
		{"risk failed", dashFilter{Risk: []string{"High"}}, pr, jobStatus{Status: statusFailed, riskDecision: riskDecision{Risk: "High"}}, true, "", false},
		{"risk skipped", dashFilter{Risk: []string{"High"}}, pr, jobStatus{Status: statusSkipped}, true, "", false},
		{"risk not queued", dashFilter{Risk: []string{"High"}}, pr, jobStatus{}, false, "", false},
		{"rec human", dashFilter{Rec: []string{"human"}}, pr, human, true, "", true},
		{"rec automated", dashFilter{Rec: []string{"automated"}}, pr, human, true, "", false},
		{"rec OR", dashFilter{Rec: []string{"automated", "human"}}, pr, auto, true, "", true},
		{"rec x", dashFilter{Rec: []string{"x"}}, pr, human, true, "", false},
		{"rec x on empty recommendation", dashFilter{Rec: []string{"x"}}, pr, jobStatus{Status: statusDone}, true, "", false},
		{"rec outdated", dashFilter{Rec: []string{"human"}}, pr, outdated, true, "", true},
		{"rec not done", dashFilter{Rec: []string{"human"}}, pr, risky, true, "", false},
		{"mine", dashFilter{Mine: true}, pr, jobStatus{}, false, "", true},
		{"mine not requested", dashFilter{Mine: true}, notMine, jobStatus{}, false, "", false},
		{"hide skipped", dashFilter{HideSkipped: true}, pr, jobStatus{Status: statusSkipped}, true, "", false},
		{"hide skipped keeps done", dashFilter{HideSkipped: true}, pr, human, true, "", true},
		{"hide approved", dashFilter{HideApproved: true}, approvedMe, human, true, "me", false},
		{"hide approved outdated", dashFilter{HideApproved: true}, approvedMeOld, human, true, "me", true},
		{"hide approved keeps changes requested", dashFilter{HideApproved: true}, changesMe, human, true, "me", true},
		{"hide approved by others", dashFilter{HideApproved: true}, approvedMe, human, true, "bob", true},
		{"hide approved login unknown", dashFilter{HideApproved: true}, approvedMe, human, true, "", true},
		{"AND all pass", dashFilter{Org: "acme", Risk: []string{"Medium", "High"}, Rec: []string{"human"}, Mine: true, HideSkipped: true}, pr, human, true, "", true},
		{"AND one fails", dashFilter{Org: "acme", Risk: []string{"Medium", "High"}, Mine: true}, notMine, human, true, "", false},
		{"contradicting repo and org", dashFilter{Org: "other", Repo: "acme/web"}, pr, human, true, "", false},
	} {
		if got := c.f.match(c.pr, c.st, c.known, c.login); got != c.want {
			t.Errorf("%s: match = %v, want %v", c.name, got, c.want)
		}
	}
}

// filterSetup returns a polled poller (login "Me") and a queue for five PRs:
// acme/web#1 alice done High human (requested), acme/web#2 Bob outdated done Medium automated (approved by me),
// Acme2/api#3 app/dependabot skipped, other/x#4 alice queued (approved by me on an older commit),
// other/x#5 carol done Low automated (requested).
func filterSetup(t *testing.T) (config, *poller, *reviewQueue) {
	cfg := config{Repositories: []string{"acme/web", "Acme2/api", "other/x"}, Concurrency: 2}
	prs := []pullRequest{
		{Repo: "acme/web", Number: 1, Author: "alice", Created: day(1), Requested: true},
		{Repo: "acme/web", Number: 2, Author: "Bob", Created: day(2), Reviews: []approval{{Login: "me", State: "APPROVED"}}},
		{Repo: "Acme2/api", Number: 3, Author: "app/dependabot", Created: day(3)},
		{Repo: "other/x", Number: 4, Author: "alice", Created: day(4), Reviews: []approval{{Login: "me", Outdated: true, State: "APPROVED"}}},
		{Repo: "other/x", Number: 5, Author: "carol", Created: day(5), Requested: true},
	}
	byRepo := map[string][]pullRequest{}
	for _, pr := range prs {
		byRepo[pr.Repo] = append(byRepo[pr.Repo], pr)
	}
	p := newPoller(cfg.Repositories, (&fakeFetch{prs: byRepo}).fetch, func(context.Context) (string, error) { return "Me", nil })
	p.poll(context.Background())
	q := newReviewQueue(0, workflow{}, resultStore{dir: t.TempDir()})
	for i, s := range []jobStatus{
		{Status: statusDone, riskDecision: riskDecision{Risk: "High", Recommendation: recHuman}},
		{Status: statusDone, riskDecision: riskDecision{Risk: "Medium", Recommendation: recAutomated}, Outdated: true},
		{Status: statusSkipped, Reason: "author rule"},
		{Status: statusQueued},
		{Status: statusDone, riskDecision: riskDecision{Risk: "Low", Recommendation: recAutomated}},
	} {
		q.jobs[prKey(prs[i])] = &job{pr: prs[i], jobStatus: s}
	}
	return cfg, p, q
}

var rowLink = regexp.MustCompile(`<a class="btn btn-sm btn-quiet" id="details-[^"]+" href="/pr/([^"]+)/(\d+)">`)

// rowIDs returns the dashboard rows of a page as repo#number in page order.
func rowIDs(body string) []string {
	var ids []string
	for _, m := range rowLink.FindAllStringSubmatch(body, -1) {
		ids = append(ids, m[1]+"#"+m[2])
	}
	return ids
}

func TestIndexFilters(t *testing.T) {
	cfg, p, q := filterSetup(t)
	page := func(path string) (int, string) {
		rec := serve(cfg, "myreview.json", p, q, localReq(http.MethodGet, path, ""))
		return rec.Code, rec.Body.String()
	}
	_, all := page("/")
	order := rowIDs(all)
	if len(order) != 5 {
		t.Fatalf("want 5 rows unfiltered, got %v", order)
	}
	for _, c := range []struct {
		path string
		want []string // in any order; the page must keep the unfiltered order
	}{
		{"/?org=acme", []string{"acme/web#1", "acme/web#2"}},
		{"/?org=Acme2", []string{"Acme2/api#3"}},
		{"/?repo=other/x", []string{"other/x#5", "other/x#4"}},
		{"/?author=alice", []string{"acme/web#1", "other/x#4"}},
		{"/?author=app/dependabot", []string{"Acme2/api#3"}},
		{"/?risk=High", []string{"acme/web#1"}},
		{"/?risk=Medium&risk=Low", []string{"other/x#5", "acme/web#2"}},
		{"/?rec=human", []string{"acme/web#1"}},
		{"/?rec=automated", []string{"other/x#5", "acme/web#2"}},
		{"/?rec=automated&rec=human", []string{"acme/web#1", "acme/web#2", "other/x#5"}},
		{"/?mine=1", []string{"acme/web#1", "other/x#5"}},
		{"/?hideskipped=1", []string{"acme/web#1", "acme/web#2", "other/x#4", "other/x#5"}},
		{"/?hideapproved=1", []string{"acme/web#1", "Acme2/api#3", "other/x#4", "other/x#5"}},
		{"/?org=acme&risk=Medium&risk=High&hideskipped=1", []string{"acme/web#1", "acme/web#2"}},
		{"/?org=other&risk=Medium&risk=High&hideskipped=1", nil},
		{"/?risk=bogus", nil},
		{"/?rec=x", nil},
		{"/?org=%3Cscript%3Ealert(1)%3C%2Fscript%3E", nil},
		{"/?repo=gone/repo", nil},
		{"/?mine=x", order},
		{"/?mine=0", order},
	} {
		code, body := page(c.path)
		var want []string
		for _, id := range order {
			if slices.Contains(c.want, id) {
				want = append(want, id)
			}
		}
		if got := rowIDs(body); code != http.StatusOK || !slices.Equal(got, want) {
			t.Errorf("%s: %d %v, want 200 %v", c.path, code, got, want)
		}
		if c.want != nil && len(c.want) != len(want) {
			t.Fatalf("%s: test expects rows not on the page: %v", c.path, c.want)
		}
		active := !slices.Equal(c.want, order)
		line := fmt.Sprintf(`<p class="filter-summary"><span>Showing %d of 5 pull requests, %d hidden by filters.</span> <a class="filter-link" id="chip-`, len(want), 5-len(want))
		if strings.Contains(body, line) != active || strings.Contains(body, ` <a class="filter-link" id="clear-filters" href="/">Clear filters</a></p>`) != active {
			t.Errorf("%s: active-filters line shown = %v, want %v (%q):\n%s", c.path, strings.Contains(body, line), active, line, body)
		}
		if len(want) == 0 && !strings.Contains(body, `<p class="muted">No open pull requests match the filters.</p>`) {
			t.Errorf("%s: want the no-match message:\n%s", c.path, body)
		}
	}
}

func TestIndexFilterForm(t *testing.T) {
	cfg, p, q := filterSetup(t)
	body := serve(cfg, "myreview.json", p, q, localReq(http.MethodGet, "/", "")).Body.String()
	mustContain(t, body,
		`<label><span class="visually-hidden">Org</span><select name="org"><option value="">All orgs</option><option value="acme">acme</option><option value="Acme2">Acme2</option><option value="other">other</option></select></label>`,
		`<label><span class="visually-hidden">Repository</span><select name="repo"><option value="">All repos</option><option value="acme/web">acme/web</option><option value="Acme2/api">Acme2/api</option><option value="other/x">other/x</option></select></label>`,
		`<label><span class="visually-hidden">Author</span><select name="author"><option value="">All authors</option><option value="alice">alice</option><option value="app/dependabot">app/dependabot</option><option value="Bob">Bob</option><option value="carol">carol</option></select></label>`,
		`<fieldset class="seg"><legend class="visually-hidden">Risk</legend><input class="visually-hidden" type="checkbox" id="f-risk-low" name="risk" value="Low"><label for="f-risk-low"><span class="meter risk-low"></span>Low</label>`,
		`<input class="visually-hidden" type="checkbox" id="f-risk-high" name="risk" value="High"><label for="f-risk-high"><span class="meter risk-high"></span>High</label></fieldset>`,
		`<fieldset class="seg"><legend class="visually-hidden">Recommendation</legend><input class="visually-hidden" type="checkbox" id="f-rec-human" name="rec" value="human"><label for="f-rec-human" title="Human review required"><span class="rec rec-human"></span><span class="visually-hidden">Human review required</span></label>`,
		`<input class="visually-hidden" type="checkbox" id="f-rec-auto" name="rec" value="automated"><label for="f-rec-auto" title="Automated review is sufficient"><span class="rec rec-auto"></span><span class="visually-hidden">Automated review is sufficient</span></label></fieldset>`,
		`<input class="visually-hidden" type="checkbox" id="f-hideskipped" name="hideskipped" value="1"><label class="toggle" for="f-hideskipped">Hide skipped</label>`,
		`<input class="visually-hidden" type="checkbox" id="f-hideapproved" name="hideapproved" value="1"><label class="toggle" for="f-hideapproved" title="Hide approved by me">Hide approved</label>`,
		`<form class="filters" method="get" action="/" hx-get="/" hx-trigger="change, submit" hx-target="#pr-rows" hx-sync="#pr-list:replace">`,
		`<button class="btn-sm apply">Apply</button>`)
	if strings.Contains(body, " selected") || strings.Contains(body, " checked") {
		t.Errorf("unfiltered page selects or checks a filter:\n%s", body)
	}

	// The choices come from every PR, not the filtered rows; the current filters are selected and checked.
	body = serve(cfg, "myreview.json", p, q, localReq(http.MethodGet,
		"/?org=acme&author=alice&risk=High&risk=Low&rec=automated&mine=1&hideskipped=1&hideapproved=1", "")).Body.String()
	mustContain(t, body,
		`<option value="acme" selected>acme</option><option value="Acme2">Acme2</option><option value="other">other</option>`,
		`<option value="alice" selected>alice</option><option value="app/dependabot">app/dependabot</option>`,
		`name="risk" value="Low" checked>`, `name="risk" value="Medium">`, `name="risk" value="High" checked>`,
		`name="rec" value="human">`, `name="rec" value="automated" checked>`,
		`name="mine" value="1" checked>`, `name="hideskipped" value="1" checked>`, `name="hideapproved" value="1" checked>`)

	// An unknown value is an extra selected option, escaped.
	body = serve(cfg, "myreview.json", p, q, localReq(http.MethodGet, "/?org=%3Cscript%3Ealert(1)%3C%2Fscript%3E&repo=gone/repo&risk=%3Cb%3E", "")).Body.String()
	mustContain(t, body,
		`<option value="">All orgs</option><option value="&lt;script&gt;alert(1)&lt;/script&gt;" selected>&lt;script&gt;alert(1)&lt;/script&gt;</option><option value="acme">acme</option>`,
		`<option value="Acme2/api">Acme2/api</option><option value="gone/repo" selected>gone/repo</option><option value="other/x">other/x</option>`)
	if strings.Contains(body, "<script>") || strings.Contains(body, "<b>") {
		t.Errorf("query value echoed unescaped:\n%s", body)
	}
}

func TestIndexFilterEmptyStates(t *testing.T) {
	cfg, p, q := filterSetup(t)
	for path, want := range map[string]string{
		"/?mine=1&risk=Medium": "No open pull requests match the filters.",
		"/?org=nobody":         "No open pull requests match the filters.",
	} {
		body := serve(cfg, "myreview.json", p, q, localReq(http.MethodGet, path, "")).Body.String()
		if !strings.Contains(body, `<p class="muted">`+want+`</p>`) || strings.Contains(body, "<table") {
			t.Errorf("%s: want %q and no table:\n%s", path, want, body)
		}
	}
	// loading and no-PR states take precedence over the no-match message
	if body := get(t, cfg, "myreview.json", "/?org=x").Body.String(); !strings.Contains(body, "Loading pull requests…") || strings.Contains(body, "match the filters") {
		t.Errorf("before first poll want loading only:\n%s", body)
	}
	empty := newPoller(cfg.Repositories, (&fakeFetch{}).fetch, noLogin)
	empty.poll(context.Background())
	if body := getWith(t, cfg, "myreview.json", empty, "/?org=x").Body.String(); !strings.Contains(body, `<p class="muted">No open pull requests.</p>`) || strings.Contains(body, "match the filters") {
		t.Errorf("with no PRs want the no-PR message only:\n%s", body)
	}
}

func TestIndexFragment(t *testing.T) {
	cfg, p, q := filterSetup(t)
	req := func(path string, h ...string) *httptest.ResponseRecorder {
		r := localReq(http.MethodGet, path, "")
		for i := 0; i < len(h); i += 2 {
			r.Header.Set(h[i], h[i+1])
		}
		return serve(cfg, "myreview.json", p, q, r)
	}
	for _, c := range []struct {
		path, push string
		want       []string
	}{
		{"/?risk=High&org=acme&repo=&risk=High", "/?org=acme&risk=High", []string{"acme/web#1"}},
		{"/?org=", "/", nil},
		{"/?risk=bogus", "/?risk=bogus", []string{}},
	} {
		frag := req(c.path, "HX-Request", "true")
		body := frag.Body.String()
		if frag.Code != http.StatusOK || frag.Header().Get("HX-Push-Url") != c.push || frag.Header().Get("Vary") != "HX-Request" {
			t.Errorf("%s fragment = %d, HX-Push-Url %q, Vary %q; want 200, %q, HX-Request", c.path, frag.Code, frag.Header().Get("HX-Push-Url"), frag.Header().Get("Vary"), c.push)
		}
		for _, no := range []string{"<html", "<head", `class="filters"`} {
			if strings.Contains(body, no) {
				t.Errorf("%s fragment contains %q:\n%s", c.path, no, body)
			}
		}
		if c.want != nil && !slices.Equal(rowIDs(body), c.want) {
			t.Errorf("%s fragment rows %v, want %v", c.path, rowIDs(body), c.want)
		}
		if c.want != nil {
			mustContain(t, body, fmt.Sprintf("<span>Showing %d of 5 pull requests, %d hidden by filters.</span>", len(c.want), 5-len(c.want)))
		}
		if len(c.want) == 0 && c.want != nil {
			mustContain(t, body, `<p class="muted">No open pull requests match the filters.</p>`)
		}
		// (c) the full page's #pr-rows is exactly the fragment
		full := req(c.path)
		if full.Header().Get("Vary") != "HX-Request" || full.Header().Get("HX-Push-Url") != "" {
			t.Errorf("%s full page: Vary %q, HX-Push-Url %q", c.path, full.Header().Get("Vary"), full.Header().Get("HX-Push-Url"))
		}
		list, oob, ok := strings.Cut(body, `<div id="poll-status" hx-swap-oob="true">`)
		if !ok || strings.Count(body, "hx-swap-oob") != 1 || !strings.HasSuffix(oob, "</section>\n</div>\n") {
			t.Errorf("%s fragment: want the list then one OOB poll-status:\n%s", c.path, body)
		}
		if !strings.Contains(full.Body.String(), `<div id="poll-status">`+oob) || strings.Contains(full.Body.String(), "hx-swap-oob") {
			t.Errorf("%s: full page poll-status differs from the fragment's OOB copy, or has hx-swap-oob:\n%s", c.path, full.Body.String())
		}
		body = list
		if !strings.Contains(full.Body.String(), `<div id="pr-rows">`+body+`</div>`) {
			t.Errorf("%s: full page #pr-rows differs from the fragment:\n%s\n---\n%s", c.path, full.Body.String(), body)
		}
	}
	// (d) a history restore gets the full page
	if body := req("/?org=acme", "HX-Request", "true", "HX-History-Restore-Request", "true").Body.String(); !strings.Contains(body, "<html") || !strings.Contains(body, `class="filters"`) {
		t.Errorf("history restore want the full page:\n%s", body)
	}
}

// (f) Each active value is a removable chip linking to the canonical query without just that value.
func TestFilterChips(t *testing.T) {
	chips := func(raw string) []filterChip {
		v, err := url.ParseQuery(raw)
		if err != nil {
			t.Fatal(err)
		}
		return parseFilter(v).chips()
	}
	got := chips("risk=Low&risk=High&org=a")
	want := []filterChip{{"chip-org-a", "Org: a", "/?risk=High&risk=Low"}, {"chip-risk-High", "Risk: High", "/?org=a&risk=Low"}, {"chip-risk-Low", "Risk: Low", "/?org=a&risk=High"}}
	if !slices.Equal(got, want) {
		t.Errorf("multi-value chips = %v, want %v", got, want)
	}
	got = chips("hideapproved=1&rec=automated&rec=human&author=app/bot&repo=a/x&mine=1&hideskipped=1&org=a")
	var labels []string
	for _, c := range got {
		labels = append(labels, c.Label)
	}
	if wantL := []string{"Org: a", "Repository: a/x", "Author: app/bot", "Recommendation: Automated review is sufficient", "Recommendation: Human review required",
		"Requested", "Hide skipped", "Hide approved"}; !slices.Equal(labels, wantL) {
		t.Errorf("chip order = %q, want %q", labels, wantL)
	}
	if got := chips("mine=1"); !slices.Equal(got, []filterChip{{"chip-mine-1", "Requested", "/"}}) {
		t.Errorf("only value chip = %v, want href /", got)
	}
	if got := chips("risk=bogus&rec=x"); !slices.Equal(got, []filterChip{{"chip-risk-bogus", "Risk: bogus", "/?rec=x"}, {"chip-rec-x", "Recommendation: x", "/?risk=bogus"}}) {
		t.Errorf("unknown value chips = %v", got)
	}
	if got := chips(""); got != nil {
		t.Errorf("no filter chips = %v", got)
	}

	// On the page: links in the counts line, escaped, before Clear filters.
	cfg, p, q := filterSetup(t)
	body := serve(cfg, "myreview.json", p, q, localReq(http.MethodGet, "/?risk=Low&risk=High&org=a&risk=%3Cb%3E", "")).Body.String()
	mustContain(t, body,
		`<a class="filter-link" id="chip-risk-&lt;b&gt;" href="/?org=a&amp;risk=High&amp;risk=Low" aria-label="Remove filter Risk: &lt;b&gt;">Risk: &lt;b&gt; <span aria-hidden="true">×</span></a>`,
		` <a class="filter-link" id="chip-risk-Low" href="/?org=a&amp;risk=%3Cb%3E&amp;risk=High" aria-label="Remove filter Risk: Low">Risk: Low <span aria-hidden="true">×</span></a> <a class="filter-link" id="clear-filters" href="/">Clear filters</a></p>`)
	if strings.Contains(body, "<b>") {
		t.Errorf("chip label echoed unescaped:\n%s", body)
	}
}

// The list ends with a poller for its own canonical query; polls push no history entry.
func TestIndexPoller(t *testing.T) {
	cfg, p, q := filterSetup(t)
	req := func(path string, h ...string) *httptest.ResponseRecorder {
		r := localReq(http.MethodGet, path, "")
		for i := 0; i < len(h); i += 2 {
			r.Header.Set(h[i], h[i+1])
		}
		return serve(cfg, "myreview.json", p, q, r)
	}
	poller := func(query string, every int) string {
		return fmt.Sprintf(`<span id="pr-poll" hidden hx-get="/?%s" hx-trigger="every %ds, myreview:refresh" hx-target="#pr-rows" hx-swap="innerHTML" hx-sync="#pr-list:drop"></span>`, query, every)
	}
	for _, c := range []struct {
		path, query string
		every       int
		rows        []string
	}{
		{"/?risk=High&org=acme&org=x", "org=acme&amp;risk=High", 60, []string{"acme/web#1"}}, // other/x#4 queued but filtered out
		{"/?org=other", "org=other", 10, []string{"other/x#5", "other/x#4"}},                 // other/x#4 queued and visible
		{"/", "", 10, nil},
		{"/?hideskipped=1&rec=automated", "hideskipped=1&amp;rec=automated", 60, []string{"other/x#5", "acme/web#2"}},
	} {
		filter := req(c.path, "HX-Request", "true")
		poll := req(c.path, "HX-Request", "true", "HX-Trigger", "pr-poll")
		if filter.Header().Get("HX-Push-Url") == "" || poll.Header().Get("HX-Push-Url") != "" {
			t.Errorf("%s: HX-Push-Url filter %q, poll %q; want set, unset", c.path, filter.Header().Get("HX-Push-Url"), poll.Header().Get("HX-Push-Url"))
		}
		for _, body := range []string{filter.Body.String(), poll.Body.String(), req(c.path).Body.String()} {
			if strings.Count(body, `id="pr-poll"`) != 1 {
				t.Errorf("%s: want one poller:\n%s", c.path, body)
			}
			mustContain(t, body, poller(c.query, c.every))
			// unique ids let htmx restore focus after a swap; a fragment's OOB copy is the only poll-status in it
			seen := map[string]bool{}
			for _, m := range regexp.MustCompile(` id="([^"]*)"`).FindAllStringSubmatch(body, -1) {
				if seen[m[1]] {
					t.Errorf("%s: duplicate id %q", c.path, m[1])
				}
				seen[m[1]] = true
			}
			if c.rows != nil && !slices.Equal(rowIDs(body), c.rows) {
				t.Errorf("%s rows %v, want %v", c.path, rowIDs(body), c.rows)
			}
		}
	}
	mustContain(t, req("/?risk=High&org=acme", "HX-Request", "true").Body.String(), `<a id="pr-acme/web-1" href=`, ` id="chip-org-acme" `, ` id="chip-risk-High" `,
		`<a class="filter-link" id="clear-filters" href="/">`, `<div id="poll-status" hx-swap-oob="true">`, `<a id="repo-acme/web" href=`)
	// running counts like queued; before the first poll the list is loading
	q.jobs[prKey(pullRequest{Repo: "acme/web", Number: 1})].Status = statusRunning
	mustContain(t, req("/?org=acme", "HX-Request", "true").Body.String(), poller("org=acme", 10))
	mustContain(t, get(t, cfg, "myreview.json", "/?org=x").Body.String(), "Loading pull requests…", poller("org=x", 10))
}

// Fragments carry the page status out of band, so errors refresh with the rows; the full page has no meta refresh outside noscript.
func TestIndexPollStatus(t *testing.T) {
	cfg := config{Repositories: []string{"a/b"}, Concurrency: 2}
	f := &fakeFetch{errs: map[string]error{"a/b": errors.New("<boom>")}}
	p := newPoller(cfg.Repositories, f.fetch, noLogin)
	p.poll(context.Background())
	poll := func() string {
		r := localReq(http.MethodGet, "/", "")
		r.Header.Set("HX-Request", "true")
		r.Header.Set("HX-Trigger", "pr-poll")
		return serve(cfg, "myreview.json", p, newReviewQueue(0, workflow{}, resultStore{dir: t.TempDir()}), r).Body.String()
	}
	mustContain(t, poll(), `<div id="poll-status" hx-swap-oob="true"><section class="watched">`,
		`<li><a id="repo-a/b" href="https://github.com/a/b">a/b</a> <span class="repo-error">— error: &lt;boom&gt;</span></li>`)
	f.errs["a/b"] = globalError(msgGhNotLoggedIn)
	p.poll(context.Background())
	body := poll()
	mustContain(t, body, `<div id="poll-status" hx-swap-oob="true"><p class="alert alert-error"><span>GitHub CLI is not logged in.`)
	if strings.Contains(body, "boom") {
		t.Errorf("stale repository error in the poll response:\n%s", body)
	}

	full := getWith(t, cfg, "myreview.json", p, "/").Body.String()
	mustContain(t, full, "<head>\n<meta charset=\"utf-8\">\n<noscript><meta http-equiv=\"refresh\" content=\"60\"></noscript>\n",
		`<div id="poll-status"><p class="alert alert-error"><span>GitHub CLI is not logged in.`)
	if strings.Count(full, "http-equiv") != 1 || strings.Contains(full, "hx-swap-oob") || strings.Count(full, `id="poll-status"`) != 1 {
		t.Errorf("full page: want one meta refresh (in noscript), one poll-status, no OOB:\n%s", full)
	}
}
