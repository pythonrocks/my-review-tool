package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
)

// commentSetup is diffSetup (a/b#3 done at abc123, fake files and a 25-line file) with a fake submit.
func commentSetup(t *testing.T) (http.Handler, *reviewQueue, *job, *fakeGH, *fakeSubmit) {
	cfg, p, q, j := doneSetup(t)
	if err := os.MkdirAll(q.pending.dir(), 0700); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for n := 1; n <= 25; n++ {
		lines = append(lines, fmt.Sprintf("L%d", n))
	}
	lines[0] = "<script>x</script>"
	f := &fakeGH{list: diffFiles(), text: strings.Join(lines, "\n") + "\n"}
	fs := &fakeSubmit{}
	return fresh(q, newHandler(newConfigHolder(cfg, "myreview.json"), p, q, fs.submit, notApproved, f.filesFn, f.contentFn, noThreads, noResolve)), q, j, f, fs
}

func addComment(h http.Handler, path, line, text string) (int, string) {
	return send(h, http.MethodPost, "/pr/a/b/3/drafts", url.Values{"path": {path}, "line": {line}, "text": {text}})
}

func commentDrafts(q *reviewQueue) []pendingDraft {
	var out []pendingDraft
	for _, d := range q.pending.get("a/b#3").Drafts {
		if commentNo(d.ID) > 0 {
			out = append(out, d)
		}
	}
	return out
}

func TestLineCommentCreateAndUpsert(t *testing.T) {
	h, q, _, _, _ := commentSetup(t)
	code, body := addComment(h, "a.go", "7", "<b>why?</b>")
	if code != http.StatusOK {
		t.Fatalf("add = %d %s", code, body)
	}
	mustContain(t, body, `<div class="inline" id="d-c1"><div class="inline-head"><span class="mark mine">C</span><span class="badge plain">Comment</span><span class="point-title">a.go:7</span><span class="state state-draft" id="state-c1">Draft</span></div>`,
		`<input type="hidden" name="path" value="a.go"><input type="hidden" name="line" value="7">
<textarea id="ta-c1" name="text" rows="6" hx-put="/pr/a/b/3/drafts/c1" hx-trigger="input changed delay:800ms" hx-target="#es-c1" hx-swap="outerHTML" hx-include="closest .editor">
&lt;b&gt;why?&lt;/b&gt;</textarea>`,
		`hx-post="/pr/a/b/3/drafts/c1/include" hx-vals='{"include": "0"}' hx-include="closest .editor"`, `>Remove from review</button>`,
		`<button id="submit-count" hx-swap-oob="true">Submit review (1)</button>`)
	want := pendingDraft{ID: "c1", File: "a.go", Line: 7, Commit: "abc123", Text: "<b>why?</b>", Edited: true, InReview: true}
	if got := commentDrafts(q); !reflect.DeepEqual(got, []pendingDraft{want}) {
		t.Fatalf("drafts %+v, want %+v", got, want)
	}
	// a double submit replaces the text of the same comment
	if code, body := addComment(h, "a.go", "7", "again"); code != http.StatusOK || !strings.Contains(body, `id="d-c1"`) {
		t.Errorf("second add = %d %s", code, body)
	}
	want.Text = "again"
	if got := commentDrafts(q); !reflect.DeepEqual(got, []pendingDraft{want}) {
		t.Errorf("after upsert %+v", got)
	}
	// another line gets the next number, also on the renamed file with an awkward name
	if code, body := addComment(h, "dir/a b#?%.go", "7", "x"); code != http.StatusOK || !strings.Contains(body, `id="d-c2"`) {
		t.Errorf("other line = %d %s", code, body)
	}
}

func TestLineCommentRefusals(t *testing.T) {
	h, q, j, _, _ := commentSetup(t)
	for _, c := range []struct {
		path, line, text string
		code             int
		msg              string
	}{
		{"a.go", "7", "", 400, "comment text is empty"},
		{"a.go", "7", " \n", 400, "comment text is empty"},
		{"a.go", "7", strings.Repeat("é", maxCommentLen+1), 400, "comment text is over 65536 characters"},
		{"nope.go", "7", "x", 404, "file not in this pull request"},
		{"b.go", "1", "x", 404, "file has no new-side lines in this pull request"},
		{"a.go", "10", "x", 400, "line 10 is not in the diff of a.go"},
		{"a.go", "07", "x", 400, "line must be a positive number of at most 7 digits"},
		{"a.go", "6", "x", 200, ""},
	} {
		code, body := addComment(h, c.path, c.line, c.text)
		if code != c.code || (c.msg != "" && !strings.Contains(body, `<p class="alert alert-error" role="alert"><span>`+c.msg+`</span></p>`)) {
			t.Errorf("add %s:%s %q = %d %s, want %d %q", c.path, c.line, cutRunes(c.text, 10), code, body, c.code, c.msg)
		}
	}
	// the refused form keeps the text
	_, body := addComment(h, "a.go", "10", "keep <me>")
	mustContain(t, body, `<input type="hidden" name="path" value="a.go"><input type="hidden" name="line" value="10">`, "keep &lt;me&gt;</textarea>")

	j.HeadSHA = "old" // outdated
	if code, _ := addComment(h, "a.go", "7", "x"); code != http.StatusConflict {
		t.Errorf("outdated = %d, want 409", code)
	}
	j.HeadSHA = "abc123"

	have := len(commentDrafts(q))
	q.pending.update("a/b#3", func(p *pendingPR) error {
		for n := have; n < maxLineComments; n++ {
			p.Drafts = append(p.Drafts, pendingDraft{ID: fmt.Sprintf("c%d", n+10), File: "old.go", Line: 1, Commit: "zzz"})
		}
		return nil
	})
	if code, body := addComment(h, "a.go", "21", "x"); code != http.StatusConflict || !strings.Contains(body, "at most 200 line comments") {
		t.Errorf("201st = %d %s", code, body)
	}
	if code, _ := addComment(h, "a.go", "6", "edit"); code != http.StatusOK { // an existing one still updates
		t.Errorf("upsert at the limit = %d", code)
	}
}

func TestLineCommentEditAndDelete(t *testing.T) {
	h, q, _, _, _ := commentSetup(t)
	addComment(h, "a.go", "7", "first")
	if code, body := send(h, http.MethodPut, "/pr/a/b/3/drafts/c1", url.Values{"text": {"changed"}, "path": {"a.go"}, "line": {"7"}}); code != http.StatusOK || !strings.Contains(body, `<span class="editor-status" id="es-c1">Saved</span><span class="state state-draft" id="state-c1" hx-swap-oob="true">Draft</span>`) {
		t.Errorf("edit = %d %s", code, body)
	}
	if code, body := send(h, http.MethodPost, "/pr/a/b/3/drafts/c1/include", url.Values{"include": {"0"}, "path": {"a.go"}, "line": {"7"}}); code != http.StatusOK || !strings.Contains(body, `Submit review (0)`) || !strings.Contains(body, ">Edited<") {
		t.Errorf("remove from review = %d %s", code, body)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localReq(http.MethodPut, "/pr/a/b/3/drafts/c1", url.Values{"text": {" \n "}, "path": {"a.go"}, "line": {"7"}}.Encode())) // blank counts as empty
	if code, body := rec.Code, rec.Body.String(); code != http.StatusOK || body != `<div class="inline"><div class="inline-body"><p class="muted">Your comment on a.go:7 was deleted.</p></div></div><button id="submit-count" hx-swap-oob="true">Submit review (0)</button>`+noError {
		t.Errorf("delete = %d %q", code, body)
	}
	// the whole block goes, so its stale editor cannot save over a later c1
	if rt, rs := rec.Header().Get("HX-Retarget"), rec.Header().Get("HX-Reswap"); rt != "#d-c1, #pt-c1 > details" || rs != "outerHTML" {
		t.Errorf("delete retarget = %q %q", rt, rs)
	}
	if len(commentDrafts(q)) != 0 {
		t.Errorf("not deleted: %+v", commentDrafts(q))
	}
	if code, _ := send(h, http.MethodPut, "/pr/a/b/3/drafts/c1", url.Values{"text": {"back"}, "path": {"a.go"}, "line": {"7"}}); code != http.StatusNotFound {
		t.Errorf("edit deleted = %d, want 404", code)
	}

	// ids: other commits, non-canonical, unknown, and the point endpoints never take c ids
	addComment(h, "a.go", "8", "x") // c1 again: numbering restarts once nothing is left
	q.pending.update("a/b#3", func(p *pendingPR) error {
		p.Drafts = append(p.Drafts, pendingDraft{ID: "c5", File: "a.go", Line: 9, Commit: "old", Text: "t", Edited: true, InReview: true})
		return nil
	})
	for _, req := range [][2]string{{http.MethodPut, "/pr/a/b/3/drafts/c5"}, {http.MethodPut, "/pr/a/b/3/drafts/c01"}, {http.MethodPut, "/pr/a/b/3/drafts/c9"},
		{http.MethodPost, "/pr/a/b/3/drafts/c5/include"}, {http.MethodGet, "/pr/a/b/3/points/c1"}, {http.MethodPost, "/pr/a/b/3/points/c1/checked"},
		{http.MethodGet, "/pr/a/b/3/points/c1/excerpt"}} {
		if code, _ := send(h, req[0], req[1], url.Values{"text": {"y"}, "include": {"1"}, "checked": {"1"}, "path": {"a.go"}, "line": {"9"}}); code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", req[0], req[1], code)
		}
	}
	if code, _ := addComment(h, "a.go", "9", "new"); code != http.StatusOK || commentDrafts(q)[2].ID != "c6" {
		t.Errorf("next id after c5 = %+v", commentDrafts(q))
	}
}

// An editor left open after its comment was deleted (another tab) sends the old path and line, so it cannot
// change a later comment that reuses the id.
func TestLineCommentStaleEditor(t *testing.T) {
	h, q, _, _, _ := commentSetup(t)
	addComment(h, "a.go", "7", "on 7")
	if code, _ := send(h, http.MethodPut, "/pr/a/b/3/drafts/c1", url.Values{"text": {""}, "path": {"a.go"}, "line": {"7"}}); code != http.StatusOK {
		t.Fatalf("delete = %d", code)
	}
	addComment(h, "a.go", "8", "on 8") // c1 again
	for _, req := range []struct {
		method, path string
		v            url.Values
	}{
		{http.MethodPut, "/pr/a/b/3/drafts/c1", url.Values{"text": {"stale"}, "path": {"a.go"}, "line": {"7"}}},
		{http.MethodPut, "/pr/a/b/3/drafts/c1", url.Values{"text": {""}, "path": {"a.go"}, "line": {"7"}}}, // a stale delete
		{http.MethodPost, "/pr/a/b/3/drafts/c1/include", url.Values{"include": {"0"}, "path": {"a.go"}, "line": {"7"}}},
		{http.MethodPut, "/pr/a/b/3/drafts/c1", url.Values{"text": {"stale"}, "path": {"b.go"}, "line": {"8"}}},
		{http.MethodPut, "/pr/a/b/3/drafts/c1", url.Values{"text": {"stale"}}},
		{http.MethodPut, "/pr/a/b/3/drafts/c1", url.Values{"text": {"stale"}, "path": {"a.go"}}},
		{http.MethodPost, "/pr/a/b/3/drafts/c1/include", url.Values{"include": {"0"}}},
	} {
		if code, _ := send(h, req.method, req.path, req.v); code != http.StatusNotFound {
			t.Errorf("%s %s %v = %d, want 404", req.method, req.path, req.v, code)
		}
	}
	want := []pendingDraft{{ID: "c1", File: "a.go", Line: 8, Commit: "abc123", Text: "on 8", Edited: true, InReview: true}}
	if got := commentDrafts(q); !reflect.DeepEqual(got, want) {
		t.Fatalf("drafts %+v, want %+v", got, want)
	}
	// the current editor still saves and includes
	if code, _ := send(h, http.MethodPut, "/pr/a/b/3/drafts/c1", url.Values{"text": {"new 8"}, "path": {"a.go"}, "line": {"8"}}); code != http.StatusOK {
		t.Errorf("matching edit = %d", code)
	}
	if code, _ := send(h, http.MethodPost, "/pr/a/b/3/drafts/c1/include", url.Values{"include": {"0"}, "path": {"a.go"}, "line": {"8"}}); code != http.StatusOK {
		t.Errorf("matching include = %d", code)
	}
	if got := commentDrafts(q); len(got) != 1 || got[0].Text != "new 8" || got[0].InReview {
		t.Errorf("after matching edit %+v", got)
	}
}

func TestCommentForm(t *testing.T) {
	h, _, _, _, _ := commentSetup(t)
	code, body := send(h, http.MethodGet, "/pr/a/b/3/diff/comment-form?path=dir%2Fa+b%23%3F%25.go&line=7", nil)
	if code != http.StatusOK || !strings.HasPrefix(body, `<form class="inline focus" hx-post="/pr/a/b/3/drafts" hx-swap="outerHTML">`) {
		t.Fatalf("form = %d %s", code, body)
	}
	mustContain(t, body, `<input type="hidden" name="path" value="dir/a b#?%.go"><input type="hidden" name="line" value="7">`, `<button class="btn-sm">Add comment</button>`)
	addComment(h, "dir/a b#?%.go", "7", "mine")
	if _, body := send(h, http.MethodGet, "/pr/a/b/3/diff/comment-form?path=dir%2Fa+b%23%3F%25.go&line=7", nil); !strings.HasPrefix(body, `<div class="inline" id="d-c1">`) || !strings.Contains(body, "\nmine</textarea>") {
		t.Errorf("existing = %s", body)
	}
	for q, want := range map[string]int{"path=nope.go&line=7": 404, "path=b.go&line=1": 404, "path=a.go&line=10": 400, "path=a.go&line=x": 400} {
		if code, _ := send(h, http.MethodGet, "/pr/a/b/3/diff/comment-form?"+q, nil); code != want {
			t.Errorf("comment-form %s = %d, want %d", q, code, want)
		}
	}
	if code, _ := send(h, http.MethodGet, "/pr/a/b/1/diff/comment-form?path=a.go&line=7", nil); code != http.StatusConflict {
		t.Errorf("not done = %d, want 409", code)
	}
}

// A comment made in the diff shows there and in the list as a Draft outside the checks, and Submit posts it.
func TestLineCommentListDiffSubmit(t *testing.T) {
	h, q, _, _, fs := commentSetup(t)
	addComment(h, "a.go", "21", "on 21")
	addComment(h, "a.go", "7", "on 7")
	include(t, h, "f5") // a point draft in review on a.go:9 gets the comment mark too

	_, diff := send(h, http.MethodGet, "/pr/a/b/3?view=diff", nil)
	mustContain(t, diff,
		`<a class="dl-num" href="#d-c2" title="Your comment on line 7">7</a><span class="dl-mark"><a class="mark mine" href="#d-c2" title="Your comment">C</a></span>`,
		`<span class="dl-mark"><a class="mark" href="#d-f5" title="Point 2: a9">2<span class="review-mark" data-review-mark="f5"><span role="img" aria-label="Comment in review" title="Comment in review">💬</span></span></a><a class="mark mine" href="#d-f5" title="Your comment">C</a></span>`,
		`<span class="dl-code">a7</span></div>
<div class="inline" id="d-c2">`, `<div class="inline" id="d-c1">`)

	list := pageOf(t, h)
	mustContain(t, list, `<li class="group-label">Your comments</li>
<li class="point" id="pt-c1"><details><summary class="point-toggle"><span class="point-no"></span><span><span class="badge plain">Comment</span></span><span class="point-title"></span><span class="point-loc" title="a.go:21"><span>a.go</span><span>:21</span></span><span class="point-meta"><span class="state state-draft" id="state-c1" hx-swap-oob="true">Draft</span></span>`,
		`<a href="?view=diff#d-c1">Open in diff →</a>`, `<li class="point" id="pt-c2">`,
		`0 of 8 checked`, `What to check <span class="pill pill-quiet">8</span>`, `Submit review (3)`)
	if strings.Index(list, `id="pt-c1"`) > strings.Index(list, `id="pt-c2"`) || strings.Index(list, `id="pt-p2"`) > strings.Index(list, "Your comments") {
		t.Error("comments out of order")
	}
	if strings.Contains(list, `id="chk-c1"`) {
		t.Error("comment has a checkbox")
	}

	code, _, redirect := postReview(h, reviewForm(q, "COMMENT"))
	if code != http.StatusOK || redirect != "/pr/a/b/3?submitted=COMMENT&n=3" {
		t.Fatalf("submit = %d %q", code, redirect)
	}
	want := []reviewComment{{Path: "a.go", Line: 9, Side: "RIGHT", Body: "a9"}, {Path: "a.go", Line: 21, Side: "RIGHT", Body: "on 21"}, {Path: "a.go", Line: 7, Side: "RIGHT", Body: "on 7"}}
	if !reflect.DeepEqual(fs.calls[0].Comments, want) {
		t.Errorf("posted %+v, want %+v", fs.calls[0].Comments, want)
	}
	for _, d := range commentDrafts(q) {
		if d.PostedURL != reviewURL || d.InReview {
			t.Errorf("%+v not marked posted", d)
		}
	}
	mustContain(t, pageOf(t, h), `>Posted</span>`, `Submit review (0)`)
}

func TestExcerpt(t *testing.T) {
	text := []byte("1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n")
	for _, c := range []struct{ line, first, last int }{{1, 1, 4}, {2, 1, 5}, {5, 2, 8}, {9, 6, 10}, {10, 7, 10}, {11, 0, 0}, {0, 0, 0}} {
		got := excerpt(text, c.line)
		if c.first == 0 {
			if got != nil {
				t.Errorf("line %d: %+v, want nil", c.line, got)
			}
			continue
		}
		if len(got) != c.last-c.first+1 || got[0].New != c.first || got[len(got)-1].New != c.last || got[c.line-c.first].Kind != "focus" {
			t.Errorf("line %d: %+v, want %d..%d", c.line, got, c.first, c.last)
		}
	}
	if excerpt(nil, 1) != nil {
		t.Error("empty file has an excerpt")
	}
}

func TestExcerptEndpoint(t *testing.T) {
	h, _, j, f, _ := commentSetup(t)
	const none = "<p class=\"muted\">Code excerpt not available</p>\n"
	row := func(n int, text string, focus bool) string {
		cls := "dl"
		if focus {
			cls += " focus"
		}
		return fmt.Sprintf(`<div class="%s"><span class="dl-num">%d</span><span class="dl-code">%s</span></div>`+"\n", cls, n, text)
	}
	// f0 is z.go:1, outside the diff
	code, body := send(h, http.MethodGet, "/pr/a/b/3/points/f0/excerpt", nil)
	want := `<div class="code"><div class="code-lines">` + "\n" + row(1, "&lt;script&gt;x&lt;/script&gt;", true) + row(2, "L2", false) + row(3, "L3", false) + row(4, "L4", false) + "</div></div>\n"
	if code != http.StatusOK || body != want {
		t.Errorf("excerpt = %d\n%s\nwant\n%s", code, body, want)
	}
	j.Outdated = true // works for outdated results; the file is read at the reviewed commit
	j.Findings[0].Line = 25
	if _, body := send(h, http.MethodGet, "/pr/a/b/3/points/f0/excerpt", nil); !strings.HasSuffix(body, row(25, "L25", true)+"</div></div>\n") || !strings.Contains(body, row(22, "L22", false)) {
		t.Errorf("last line = %s", body)
	}
	for name, change := range map[string]func(){
		"past EOF":  func() { j.Findings[0].Line = 26 },
		"line 0":    func() { j.Findings[0].Line = 0 },
		"bad path":  func() { j.Findings[0].File = "../etc/passwd" },
		"content":   func() { f.cerr = errNotAvailable },
		"read fail": func() { f.text = ""; f.cerr = fmt.Errorf("gh: 404") },
	} {
		j.Findings[0].File, j.Findings[0].Line, f.cerr = "z.go", 1, nil
		change()
		if code, body := send(h, http.MethodGet, "/pr/a/b/3/points/f0/excerpt", nil); code != http.StatusOK || body != none {
			t.Errorf("%s = %d %q", name, code, body)
		}
	}
	if f.files != 0 {
		t.Errorf("excerpt fetched the PR files %d times", f.files)
	}
	if code, _ := send(h, http.MethodGet, "/pr/a/b/3/points/f99/excerpt", nil); code != http.StatusNotFound {
		t.Errorf("unknown point = %d", code)
	}
}

func TestPreviousNextEnds(t *testing.T) {
	h, _, _, _, _ := commentSetup(t)
	_, first := send(h, http.MethodGet, "/pr/a/b/3/points/f4?view=diff", nil)
	_, last := send(h, http.MethodGet, "/pr/a/b/3/points/p2", nil)
	mustContain(t, first, `<a class="point-nav" href="#d-f5">Next →</a>`)
	mustContain(t, last, `<a class="point-nav" href="#pt-p0">← Previous</a>`)
	if strings.Contains(first, "Previous") || strings.Contains(last, "Next") {
		t.Errorf("neighbours at the ends:\n%s\n%s", first, last)
	}
}
