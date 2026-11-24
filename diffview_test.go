package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func emptyFiles(context.Context, string, int) ([]prFile, error) { return nil, nil }

// fakeGH is a files and content fake that counts calls.
type fakeGH struct {
	mu           sync.Mutex
	list         []prFile
	err, cerr    error
	text         string
	files, reads int
	paths        []string // the paths read
	block        bool     // reads wait for their context to end
}

func (f *fakeGH) filesFn(context.Context, string, int) ([]prFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files++
	return f.list, f.err
}

func (f *fakeGH) contentFn(ctx context.Context, _, s, path string) ([]byte, error) {
	f.mu.Lock()
	f.reads++
	f.paths = append(f.paths, path)
	if f.block {
		f.mu.Unlock()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	defer f.mu.Unlock()
	if s != sha {
		return nil, errors.New("wrong sha")
	}
	return []byte(f.text), f.cerr
}

// a.go: hunk 1 old 5..8 / new 5..9 (6 deleted, 6 and 7 added), hunk 2 old 20..21 / new 21..23.
const aPatch = "@@ -5,4 +5,5 @@\n c5\n-d6\n+a6\n+a7\n c8\n c9\n@@ -20,2 +21,3 @@\n c21\n+<script>alert(1)</script>\n c23"

func diffFiles() []prFile {
	ren := file("dir/a b#?%.go", "renamed", "@@ -7 +7 @@\n-x\n+y")
	ren.PreviousFilename = "old/x.go"
	ren.ContentsURL = "https://api.github.com/repos/o/r/contents/dir/a%20b%23%3F%25.go?ref=" + sha
	removed := file("b.go", "removed", "@@ -1,3 +0,0 @@\n-a\n-b\n-c")
	removed.ContentsURL += "-base" // GitHub points a removed file at the base commit
	return []prFile{file("a.go", "modified", aPatch), ren, removed, file("img.png", "modified", ""),
		file("big.txt", "modified", "@@ -1 +1 @@\n-a\n+"+strings.Repeat("x", maxPatchBytes)), file("new.go", "added", "@@ -0,0 +1,2 @@\n+a\n+b")}
}

// diffSetup is doneSetup (a/b#3 done at abc123) with fake files and content.
func diffSetup(t *testing.T) (http.Handler, *fakeGH, *job) {
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
	return fresh(q, newHandler(newConfigHolder(cfg, "myreview.json"), p, q, nil, notApproved, f.filesFn, f.contentFn, noThreads, noResolve)), f, j
}

func TestDiffView(t *testing.T) {
	h, f, _ := diffSetup(t)
	code, body := send(h, http.MethodGet, "/pr/a/b/3?view=diff", nil)
	if code != http.StatusOK {
		t.Fatalf("diff = %d", code)
	}
	mustContain(t, body,
		// file list with point counts (b.go has 3 items, all outside the diff)
		`<a href="#file-0" title="a.go"><span class="fname">a.go</span><span class="pill pill-quiet">1</span></a>`,
		`<a href="#file-1" title="dir/a b#?%.go"><span class="fname">a b#?%.go</span><span class="pill pill-quiet">2</span></a>`,
		`<a href="#file-2" title="b.go"><span class="fname">b.go</span><span class="pill pill-quiet">3</span></a>`,
		`<a href="#file-3" title="img.png"><span class="fname">img.png</span></a>`,
		`<label class="confirm"><input type="checkbox" id="files-with-points"> Files with points only</label>`,
		// points index
		`<a href="#d-f4"><span class="point-no">1</span> <span class="point-title">Crit title</span><span class="review-mark" data-review-mark="f4"></span></a>`,
		`<a href="#d-f5"><span class="point-no">2</span> <span class="point-title">a9</span><span class="review-mark" data-review-mark="f5"></span></a>`,
		`<a href="#d-f3"><span class="point-no">3</span> <span class="point-title">b3</span><span class="review-mark" data-review-mark="f3"></span></a>`,
		`<a href="#d-f1"><span class="point-no">5</span> <span class="point-title">whole file</span><span class="review-mark" data-review-mark="f1"></span></a>`,
		`<a href="#d-f0"><span class="point-no">6</span> <span class="point-title">sec low</span><span class="review-mark" data-review-mark="f0"></span></a>`,
		// points not on a diff line: the removed b.go's at its head, z.go's (not in the PR) in Other files: line 0 as a
		// block, line 1 on its line with lines 2..4 after it
		`<span class="muted file-status">removed</span></div>
<div class="file-threads">
<span class="label">Not on a diff line</span>
<div class="inline point" id="d-`,
		`<a href="#file-other" title="Files not in the diff"><span class="fname">Other files</span><span class="pill pill-quiet">2</span></a>`,
		`<section class="diff-file" id="file-other">
<div class="diff-file-head"><span class="path">Other files</span></div>
<div class="file-threads">
<code>z.go</code>
<div class="inline point" id="d-p0">`,
		`<div class="code"><div class="code-lines">
<div class="dl-label">Not changed in this PR</div>
<div class="dl unchanged"><span class="dl-num">1</span><span class="dl-num">1</span><span class="dl-mark"><a class="mark" href="#d-f0" title="Point 6: sec low">6<span class="review-mark" data-review-mark="f0"></span></a></span><span class="dl-sign"></span><span class="dl-code">&lt;script&gt;x&lt;/script&gt;</span></div>
<div class="inline point" id="d-f0">`,
		`<div class="dl unchanged"><span class="dl-num">4</span><span class="dl-num">4</span><span class="dl-mark"></span><span class="dl-sign"></span><span class="dl-code">L4</span></div>
</div></div>
</div>
</section>`,
		`<a href="#d-p2"><span class="point-no">8</span> <span class="point-title">First point</span><span class="review-mark" data-review-mark="p2"></span></a>`,
		// headers
		`<span class="path">a.go</span><span class="stat-add">+1</span><span class="stat-del">−0</span><span class="muted file-status">modified</span>`,
		`<span class="path">old/x.go → dir/a b#?%.go</span>`,
		`<span class="muted file-status">removed</span>`,
		`<p class="diff-note muted">Diff not available (binary or too large on GitHub)</p>`,
		`<p class="diff-note muted">Diff not available (over 128 KiB)</p>`,
		// rows: a deletion has no new number, context and add lines link to the comment form
		`<div class="dl del"><span class="dl-num">6</span><span class="dl-num"></span><span class="dl-mark"></span><span class="dl-sign">-</span><span class="dl-code">d6</span></div>`,
		`<div class="dl add"><span class="dl-num"></span><a class="dl-num" href="#" title="Comment on line 7" hx-get="/pr/a/b/3/diff/comment-form?path=a.go&line=7" hx-trigger="click once" hx-target="closest .dl" hx-swap="afterend">7</a><span class="dl-mark"></span><span class="dl-sign">+</span><span class="dl-code">a7</span></div>`,
		// marker on new line 9 (old 8) with its inline block
		`<div class="dl"><span class="dl-num">8</span><a class="dl-num" href="#" title="Comment on line 9" hx-get="/pr/a/b/3/diff/comment-form?path=a.go&line=9" hx-trigger="click once" hx-target="closest .dl" hx-swap="afterend">9</a><span class="dl-mark"><a class="mark" href="#d-f5" title="Point 2: a9">2<span class="review-mark" data-review-mark="f5"></span></a></span><span class="dl-sign"></span><span class="dl-code">c9</span></div>
<div class="inline point" id="d-f5"><input type="checkbox" class="point-check" id="chk-f5"`,
		`<details hx-get="/pr/a/b/3/points/f5?view=diff" hx-trigger="toggle once" hx-target="find .point-body" hx-swap="outerHTML"><summary class="point-toggle"><span class="point-no">2</span>`,
		// two items on the renamed file's line 7
		`<span class="dl-mark"><a class="mark" href="#d-f4" title="Point 1: Crit title">1<span class="review-mark" data-review-mark="f4"></span></a><a class="mark" href="#d-p2" title="Point 8: First point">8<span class="review-mark" data-review-mark="p2"></span></a></span>`,
		`<div class="inline point" id="d-f4">`, `<div class="inline point" id="d-p2">`,
		`hx-get="/pr/a/b/3/diff/comment-form?path=dir%2Fa&#43;b%23%3F%25.go&line=7"`,
		// gaps: leading, middle, trailing; none for the added and removed files
		`<div class="dl-gap"><button class="link-btn" type="button" hx-get="/pr/a/b/3/diff/expand?path=a.go&from=1&to=4" hx-target="closest .dl-gap" hx-swap="outerHTML">↕ Expand</button></div>`,
		`expand?path=a.go&from=10&to=20"`, `expand?path=a.go&from=24&to=0"`,
		`&lt;script&gt;alert(1)&lt;/script&gt;`,
	)
	for _, id := range []string{"f0", "f1", "f2", "f3", "f4", "f5", "p0", "p2"} { // every item (p1 repeats finding f4 and is not listed): one block, linked from the index
		if strings.Count(body, `id="d-`+id+`"`) != 1 || !strings.Contains(body, `<a href="#d-`+id+`"><span class="point-no">`) {
			t.Errorf("%s: not exactly one block, or no index link", id)
		}
	}
	for _, no := range []string{"path=b.go", "path=new.go&amp;from", "<script>alert", "focus", "outside the diff", "is not in the diff", "point-ref", "Only the first 300", `style=`} {
		if strings.Contains(body, no) {
			t.Errorf("diff has %q", no)
		}
	}
	if strings.Index(body, "from=1&to=4") > strings.Index(body, `"dl-code">c5<`) || strings.Index(body, "from=10&") > strings.Index(body, `"dl-code">c21<`) ||
		strings.Index(body, "from=24&") < strings.Index(body, `"dl-code">c23<`) {
		t.Error("gaps out of place")
	}
	send(h, http.MethodGet, "/pr/a/b/3?view=diff", nil)
	if f.files != 1 || f.reads != 1 {
		t.Errorf("%d files and %d content calls, want 1 and 1 (z.go only, cached)", f.files, f.reads)
	}
}

func TestDiffOpenPoint(t *testing.T) {
	h, _, _ := diffSetup(t)
	_, body := send(h, http.MethodGet, "/pr/a/b/3?view=diff&point=f5", nil)
	mustContain(t, body, `<div class="inline point focus" id="d-f5">`, `<details open hx-get="/pr/a/b/3/points/f5?view=diff"`, `<div class="excerpt" hx-get="/pr/a/b/3/points/f5/excerpt"`,
		`<a href="/pr/a/b/3?view=diff&point=f5#d-f5">Open in diff →</a>`)
	if strings.Count(body, "focus") != 1 {
		t.Error("more than one block focused")
	}
	for _, id := range []string{"f0", "f1", "p0"} { // not on a diff line: z.go:1 (not in the PR), b.go (line 0), z.go
		_, body := send(h, http.MethodGet, "/pr/a/b/3?view=diff&point="+id, nil)
		mustContain(t, body, `<div class="inline point focus" id="d-`+id+`">`, `<details open hx-get="/pr/a/b/3/points/`+id+`?view=diff"`,
			`<div class="excerpt" hx-get="/pr/a/b/3/points/`+id+`/excerpt"`)
		if strings.Count(body, "focus") != 1 || strings.Contains(body, "is not in the diff") {
			t.Errorf("point=%s: not exactly one block focused, or the alert shown", id)
		}
	}
	for _, id := range []string{"zzz", "f99", "f05", ""} {
		_, body := send(h, http.MethodGet, "/pr/a/b/3?view=diff&point="+id, nil)
		if strings.Contains(body, "focus") || strings.Contains(body, "is not in the diff") {
			t.Errorf("point=%q not ignored", id)
		}
	}
}

func TestDiffAlerts(t *testing.T) {
	h, f, j := diffSetup(t)
	_, body := send(h, http.MethodGet, "/pr/a/b/1?view=diff", nil)
	mustContain(t, body, `<p class="alert alert-warning"><span>The diff is shown once the review is done.</span></p>`)

	moved := `<p class="alert alert-warning"><span>The diff is shown for the reviewed commit only; the pull request moved to abc123. Re-run or wait for the new review.</span></p>`
	f.list = diffFiles()
	f.list[0].ContentsURL = "https://x/?ref=other" // the PR moved between the poll and the fetch
	_, body = send(h, http.MethodGet, "/pr/a/b/3?view=diff", nil)
	mustContain(t, body, moved)

	f.list, f.err = nil, errors.New("gh: rate limited")
	_, body = send(h, http.MethodGet, "/pr/a/b/3?view=diff", nil)
	mustContain(t, body, `<p class="alert alert-warning"><span>Could not load the diff: gh: rate limited</span></p>`)
	f.list, f.err = diffFiles(), nil
	_, body = send(h, http.MethodGet, "/pr/a/b/3?view=diff", nil)
	if !strings.Contains(body, `<div class="diff-layout">`) || f.files != 3 {
		t.Errorf("reload after error: %d files calls, diff shown %v", f.files, strings.Contains(body, "diff-layout"))
	}

	j.HeadSHA = "def4567" // the result is for an older commit
	_, body = send(h, http.MethodGet, "/pr/a/b/3?view=diff", nil)
	mustContain(t, body, moved)
	if f.files != 3 {
		t.Errorf("outdated result fetched files")
	}
}

func TestDiffNoFilesFromListOrPolling(t *testing.T) {
	h, f, _ := diffSetup(t)
	for _, path := range []string{"/pr/a/b/3", "/pr/a/b/3?view=list", "/pr/a/b/3/points/f0", "/"} {
		send(h, http.MethodGet, path, nil)
	}
	r := localReq(http.MethodGet, "/", "")
	r.Header.Set("HX-Request", "true")
	r.Header.Set("HX-Trigger", "pr-poll")
	do(h, r)
	if f.files != 0 || f.reads != 0 {
		t.Errorf("%d files and %d content calls, want none", f.files, f.reads)
	}
}

func TestDiffExpand(t *testing.T) {
	h, f, _ := diffSetup(t)
	row := func(old, n int, text string) string {
		return fmt.Sprintf(`<div class="dl"><span class="dl-num">%d</span><span class="dl-num">%d</span><span class="dl-mark"></span><span class="dl-sign"></span><span class="dl-code">%s</span></div>`+"\n", old, n, text)
	}
	rows := func(from, to, offset int) string {
		var b strings.Builder
		for n := from; n <= to; n++ {
			text := fmt.Sprintf("L%d", n)
			if n == 1 {
				text = "&lt;script&gt;x&lt;/script&gt;"
			}
			b.WriteString(row(n+offset, n, text))
		}
		return b.String()
	}
	for q, want := range map[string]string{
		"path=a.go&from=1&to=4":                  rows(1, 4, 0),
		"path=a.go&from=10&to=20":                rows(10, 20, -1),
		"path=a.go&from=24&to=0":                 rows(24, 25, -2), // clamped to the 25-line file
		"path=dir%2Fa+b%23%3F%25.go&from=8&to=0": rows(8, 25, 0),
	} {
		code, body := send(h, http.MethodGet, "/pr/a/b/3/diff/expand?"+q, nil)
		if code != http.StatusOK || body != want {
			t.Errorf("expand %s = %d\n%s\nwant\n%s", q, code, body, want)
		}
	}
	for q, want := range map[string]int{
		"path=nope.go&from=1&to=4": http.StatusNotFound,
		"path=b.go&from=1&to=0":    http.StatusNotFound, // removed
		"path=img.png&from=1&to=0": http.StatusNotFound, // no patch
		"path=a.go&from=1&to=5":    http.StatusBadRequest,
		"path=a.go&from=01&to=4":   http.StatusBadRequest,
		"path=a.go&from=1&to=":     http.StatusBadRequest,
		"path=new.go&from=3&to=0":  http.StatusBadRequest, // added file: no trailing gap
	} {
		if code, _ := send(h, http.MethodGet, "/pr/a/b/3/diff/expand?"+q, nil); code != want {
			t.Errorf("expand %s = %d, want %d", q, code, want)
		}
	}
	f.cerr = errNotAvailable
	if code, body := send(h, http.MethodGet, "/pr/a/b/3/diff/expand?path=a.go&from=1&to=4", nil); code != http.StatusOK || body != `<div class="dl-gap">Could not expand: not available</div>`+"\n" {
		t.Errorf("content failure = %d %q", code, body)
	}
	if code, _ := send(h, http.MethodGet, "/pr/a/b/1/diff/expand?path=a.go&from=1&to=4", nil); code != http.StatusConflict {
		t.Errorf("not done = %d, want 409", code)
	}
}

func TestDiffTruncated(t *testing.T) {
	h, f, _ := diffSetup(t)
	f.list = nil
	for i := range filesPerPage * maxFilePages {
		f.list = append(f.list, file(fmt.Sprintf("f%d.go", i), "modified", "@@ -1 +1 @@\n-a\n+b"))
	}
	_, body := send(h, http.MethodGet, "/pr/a/b/3?view=diff", nil)
	mustContain(t, body, `<p class="alert alert-info"><span>Only the first 300 files are shown. Open on GitHub for the rest.</span></p>`, `id="file-299"`)
}

// A hunk with no old lines (pure add) or no new lines (pure delete) starts after its start line, so the offset uses its first lines.
func TestExpandRowsZeroCountHunks(t *testing.T) {
	text := []byte("1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n")
	for _, c := range []struct {
		patch          string
		from, to, old0 int
	}{
		{"@@ -4,0 +5,2 @@\n+a\n+b", 1, 4, 1}, // new 1..4 = old 1..4
		{"@@ -4,0 +5,2 @@\n+a\n+b", 7, 0, 5}, // after it: new 7 = old 5
		{"@@ -5,2 +4,0 @@\n-a\n-b", 1, 4, 1}, // new 1..4 = old 1..4
		{"@@ -5,2 +4,0 @@\n-a\n-b", 5, 0, 7}, // after it: new 5 = old 7
	} {
		h, err := parsePatch(c.patch)
		if err != nil {
			t.Fatal(err)
		}
		rows := expandRows(diffFile{Hunks: h}, c.from, c.to, textLines(text))
		if len(rows) == 0 || rows[0].Old != c.old0 || rows[0].New != c.from {
			t.Errorf("%q gap %d..%d: first row %+v, want old %d", c.patch, c.from, c.to, rows, c.old0)
		}
	}
}

// A point in the review or posted shows 💬 in its gutter marker, summary row and points-index entry; "Add to review" updates them all.
func TestDiffReviewMark(t *testing.T) {
	h, q, _, _, _ := commentSetup(t)
	const oob = `hx-swap-oob="outerHTML:[data-review-mark=`
	mark := func(id, label string) string {
		return `data-review-mark="` + id + `"><span role="img" aria-label="` + label + `" title="` + label + `">💬</span></span>`
	}
	include(t, h, "f5", "f1", "f0", "f4")
	_, body := send(h, http.MethodPost, "/pr/a/b/3/drafts/f4/include", url.Values{"include": {"0"}}) // B: ignored
	if strings.Count(body, oob) != 1 || !strings.Contains(body, `<span class="review-mark" data-review-mark="f4" hx-swap-oob="outerHTML:[data-review-mark='f4']"></span>`) {
		t.Errorf("include=0 response: %s", body)
	}
	_, body = send(h, http.MethodPost, "/pr/a/b/3/drafts/f5/include", url.Values{"include": {"1"}})
	if strings.Count(body, oob) != 1 || !strings.Contains(body, `<span class="review-mark" data-review-mark="f5" hx-swap-oob="outerHTML:[data-review-mark='f5']"><span role="img" aria-label="Comment in review"`) {
		t.Errorf("include=1 response: %s", body)
	}
	send(h, http.MethodPut, "/pr/a/b/3/drafts/p2", url.Values{"text": {"changed"}}) // C: edited, not included
	if _, err := q.pending.update("a/b#3", func(p *pendingPR) error {
		for i := range p.Drafts {
			if p.Drafts[i].ID == "f0" {
				p.Drafts[i].PostedURL = "https://github.com/a/b/pull/3#c"
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	_, diff := send(h, http.MethodGet, "/pr/a/b/3?view=diff", nil)
	for id, want := range map[string]int{"f5": 3, "f1": 2} { // f5: gutter, summary, index; f1 is outside the diff: summary, index
		if n := strings.Count(diff, mark(id, "Comment in review")); n != want {
			t.Errorf("%s: %d in-review marks, want %d", id, n, want)
		}
	}
	if n := strings.Count(diff, mark("f0", "Comment posted")); n != 3 { // f0 is on z.go:1 in Other files
		t.Errorf("f0: %d posted marks, want 2", n)
	}
	for _, id := range []string{"f4", "p2"} {
		if n := strings.Count(diff, `data-review-mark="`+id+`"></span>`); n != 3 {
			t.Errorf("%s: %d empty marks, want 3", id, n)
		}
	}
	mustContain(t, diff, `title="Point 2: a9">2<span class="review-mark" `+mark("f5", "Comment in review")+`</a>`,
		`<a href="#d-f1"><span class="point-no">5</span> <span class="point-title">whole file</span><span class="review-mark" `+mark("f1", "Comment in review"))
	mustContain(t, diff, `id="state-p2" hx-swap-oob="true">Edited<`, `id="state-f4" hx-swap-oob="true">Suggestion<`)
	if strings.Count(diff, "💬") != 8 {
		t.Errorf("%d marks in the diff, want 8", strings.Count(diff, "💬"))
	}

	// line comments get no mark
	addComment(h, "a.go", "7", "on 7")
	_, body = send(h, http.MethodPost, "/pr/a/b/3/drafts/c1/include", url.Values{"include": {"0"}, "path": {"a.go"}, "line": {"7"}})
	if !strings.Contains(body, "Submit review") || strings.Contains(body, "review-mark") {
		t.Errorf("comment include response: %s", body)
	}
}

// Every item gets exactly one place: its diff line, the head of its file, or Other files.
func TestNewDiffViewPlacement(t *testing.T) {
	d, err := buildDiff(diffFiles(), sha)
	if err != nil {
		t.Fatal(err)
	}
	d.Truncated = true // so far.go stands for a file past the truncation
	items := []checkItem{
		{ID: "on", File: "a.go", Line: 9},       // on a diff line
		{ID: "gap", File: "a.go", Line: 15},     // (1) an unchanged region
		{ID: "notpr", File: "z.go", Line: 3},    // (2) a file not in the PR
		{ID: "zero", File: "a.go", Line: 0},     // (3) line 0
		{ID: "beyond", File: "a.go", Line: 999}, // (4) beyond the file
		{ID: "note", File: "img.png", Line: 1},  // (6) a file with a Note
		{ID: "far", File: "far.go", Line: 1},    // (6) missing because the diff is truncated
		{ID: "removed", File: "b.go", Line: 2},  // a removed file
		{ID: "notpr2", File: "y.go", Line: 1},
		{ID: "notpr3", File: "z.go", Line: 0},
	}
	v := newDiffView(d, items, nil, nil, nil)
	got := map[string]string{}
	place := func(id, where string) {
		if got[id] != "" {
			t.Errorf("%s placed twice: %s and %s", id, got[id], where)
		}
		got[id] = where
	}
	for _, f := range v.Files {
		for _, it := range f.Outside {
			place(it.ID, "head of "+f.Filename)
		}
		for _, r := range f.Rows {
			for _, it := range r.Items {
				place(it.ID, fmt.Sprintf("%s:%d", f.Filename, r.Line.New))
			}
		}
	}
	var other []string
	for _, o := range v.Other {
		for _, it := range o.Outside {
			place(it.ID, "other")
			other = append(other, it.ID)
		}
	}
	want := map[string]string{"on": "a.go:9", "gap": "head of a.go", "notpr": "other", "zero": "head of a.go", "beyond": "head of a.go",
		"note": "head of img.png", "far": "other", "removed": "head of b.go", "notpr2": "other", "notpr3": "other"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("placement = %v\nwant %v", got, want)
	}
	if fmt.Sprint(other) != "[notpr notpr3 far notpr2]" { // grouped by path, paths in first-item order
		t.Errorf("Other = %v", other)
	}
	if len(v.Points) != len(items) {
		t.Errorf("%d index entries, want %d", len(v.Points), len(items))
	}
}

func TestLineRanges(t *testing.T) {
	for _, c := range []struct {
		ns     []int
		lo, hi int
		want   string
	}{
		{[]int{10}, 1, 100, "[[7 13]]"},
		{[]int{2, 99}, 1, 100, "[[1 5] [96 100]]"},  // clamped to the file
		{[]int{10, 15}, 1, 100, "[[7 18]]"},         // 5 apart: overlapping, merged
		{[]int{17, 10}, 1, 100, "[[7 20]]"},         // touching (13, 14), merged; any order
		{[]int{10, 18}, 1, 100, "[[7 13] [15 21]]"}, // a line apart
		{[]int{10, 10}, 1, 100, "[[7 13]]"},         // one line twice
		{[]int{11, 15, 30}, 10, 20, "[[10 18]]"},    // clamped to a gap, lines outside it ignored
		{nil, 1, 100, "[]"},
	} {
		if got := fmt.Sprint(lineRanges(c.ns, c.lo, c.hi)); got != c.want {
			t.Errorf("lineRanges(%v, %d, %d) = %s, want %s", c.ns, c.lo, c.hi, got, c.want)
		}
	}
}

// Items on unchanged lines of a file with a text are shown on those lines, with lineContext lines around them.
func TestNewDiffViewUnchanged(t *testing.T) {
	d, err := buildDiff(diffFiles(), sha) // a.go: hunks new 5..9 and 21..23, gaps 1..4, 10..20 and 24..
	if err != nil {
		t.Fatal(err)
	}
	d.Truncated = true
	text := func(n int) []byte {
		var b strings.Builder
		for i := 1; i <= n; i++ {
			fmt.Fprintf(&b, "L%d\n", i)
		}
		return []byte(b.String())
	}
	texts := map[string][]byte{"a.go": text(30), "z.go": text(10), "far.go": text(2), "img.png": nil, "b.go": text(5), "new.go": text(2)}
	items := []checkItem{
		{ID: "on", File: "a.go", Line: 9},
		{ID: "lead", File: "a.go", Line: 2},             // leading gap: 1..4, clamped at the file start and the hunk
		{ID: "mid", File: "a.go", Line: 15},             // middle gap: 12..18, pieces 10..11 and 19..20 left
		{ID: "mid2", File: "a.go", Line: 15},            // same line: one row, two items
		{ID: "trail", File: "a.go", Line: 26},           // trailing gap: 24..29, piece 30.. left
		{ID: "beyond", File: "a.go", Line: 31},          // beyond the file: head
		{ID: "zero", File: "a.go", Line: 0},             // head
		{ID: "notext", File: "dir/a b#?%.go", Line: 20}, // no text: head
		{ID: "note", File: "img.png", Line: 1},          // a file with a Note: head
		{ID: "removed", File: "b.go", Line: 2},          // removed: head
		{ID: "added", File: "new.go", Line: 1},          // on an added file's diff line
		{ID: "z3", File: "z.go", Line: 3},               // not in the PR: 1..6 and 5..10 merged
		{ID: "z8", File: "z.go", Line: 8},
		{ID: "z0", File: "z.go", Line: 0},     // block in Other
		{ID: "y", File: "y.go", Line: 1},      // no text: block in Other
		{ID: "far", File: "far.go", Line: 1},  // past the truncation: 1..2
		{ID: "far9", File: "far.go", Line: 9}, // beyond its file: block in Other
	}
	v := newDiffView(d, items, nil, nil, texts)
	rows := func(rs []diffRow) string {
		var out []string
		for _, r := range rs {
			s := ""
			switch {
			case r.From != 0:
				s = fmt.Sprintf("gap %d-%d", r.From, r.To)
			case r.Unchanged:
				s = fmt.Sprintf("u%d/%d", r.Line.New, r.Line.Old)
				if r.Label {
					s = "label " + s
				}
			default:
				s = fmt.Sprintf("%s%d", r.Line.Kind[:1], r.Line.New)
			}
			for _, it := range r.Items {
				s += " " + it.ID
			}
			out = append(out, s)
		}
		return strings.Join(out, ", ")
	}
	want := "label u1/1, u2/2 lead, u3/3, u4/4, c5, d0, a6, a7, c8, c9 on, gap 10-11, label u12/11, u13/12, u14/13, u15/14 mid mid2, " +
		"u16/15, u17/16, u18/17, gap 19-20, c21, a22, c23, label u24/22, u25/23, u26/24 trail, u27/25, u28/26, u29/27, gap 30-0"
	if got := rows(v.Files[0].Rows); got != want {
		t.Errorf("a.go rows:\n%s\nwant\n%s", got, want)
	}
	if got := rows(v.Other[0].Rows); v.Other[0].Path != "z.go" || got != "label u1/1, u2/2, u3/3 z3, u4/4, u5/5, u6/6, u7/7, u8/8 z8, u9/9, u10/10" {
		t.Errorf("z.go rows: %s", got)
	}
	if got := rows(v.Other[2].Rows); v.Other[2].Path != "far.go" || got != "label u1/1 far, u2/2" {
		t.Errorf("far.go rows: %s", got)
	}
	// every item in exactly one place
	got := map[string]string{}
	place := func(id, where string) {
		if got[id] != "" {
			t.Errorf("%s placed twice: %s and %s", id, got[id], where)
		}
		got[id] = where
	}
	for _, f := range v.Files {
		for _, it := range f.Outside {
			place(it.ID, "head of "+f.Filename)
		}
		for _, r := range f.Rows {
			for _, it := range r.Items {
				place(it.ID, fmt.Sprintf("%s:%d", f.Filename, r.Line.New))
			}
		}
	}
	for _, o := range v.Other {
		for _, it := range o.Outside {
			place(it.ID, "other "+o.Path)
		}
		for _, r := range o.Rows {
			for _, it := range r.Items {
				place(it.ID, fmt.Sprintf("other %s:%d", o.Path, r.Line.New))
			}
		}
	}
	wantPlace := map[string]string{"on": "a.go:9", "lead": "a.go:2", "mid": "a.go:15", "mid2": "a.go:15", "trail": "a.go:26", "beyond": "head of a.go",
		"zero": "head of a.go", "notext": "head of dir/a b#?%.go", "note": "head of img.png", "removed": "head of b.go", "added": "new.go:1",
		"z3": "other z.go:3", "z8": "other z.go:8", "z0": "other z.go", "y": "other y.go", "far": "other far.go:1", "far9": "other far.go"}
	if fmt.Sprint(got) != fmt.Sprint(wantPlace) {
		t.Errorf("placement = %v\nwant %v", got, wantPlace)
	}
	if v.OtherN != 6 || v.Files[0].Points != 7 || len(v.Other) != 3 || v.Other[1].Path != "y.go" {
		t.Errorf("OtherN %d, a.go Points %d, Other %d files", v.OtherN, v.Files[0].Points, len(v.Other))
	}

	// a trailing range that reaches the end of the file leaves no piece; without texts all fall back
	texts["a.go"] = text(28)
	if got := rows(newDiffView(d, items[4:5], nil, nil, texts).Files[0].Rows); !strings.HasSuffix(got, "u27/25, u28/26") {
		t.Errorf("trailing range to EOF: %s", got)
	}
	v = newDiffView(d, items, nil, nil, nil)
	if len(v.Files[0].Outside) != 6 || strings.Contains(rows(v.Files[0].Rows), "u") || len(v.Other[0].Rows) != 0 || len(v.Other[0].Outside) != 3 {
		t.Errorf("no texts: a.go outside %d, rows %s", len(v.Files[0].Outside), rows(v.Files[0].Rows))
	}
}

// GitHub threads never attach to unchanged lines: they stay at the head of the file.
func TestNewDiffViewUnchangedThreads(t *testing.T) {
	d, err := buildDiff(diffFiles(), sha)
	if err != nil {
		t.Fatal(err)
	}
	tv := []threadView{{T: reviewThread{ID: "t1", Path: "a.go", Line: 15, Side: "RIGHT"}}}
	v := newDiffView(d, []checkItem{{ID: "mid", File: "a.go", Line: 15}}, nil, tv, map[string][]byte{"a.go": []byte(strings.Repeat("x\n", 30))})
	if len(v.Files[0].Threads) != 1 {
		t.Errorf("thread on an unchanged line not at the head: %d", len(v.Files[0].Threads))
	}
}

// A point on an unchanged line of a.go (gap 10..20) shows lines 12..18 in place; the pieces left expand.
func TestDiffUnchanged(t *testing.T) {
	h, f, j := diffSetup(t)
	j.Findings = append(j.Findings, taggedFinding{Step: "code review", finding: finding{Severity: "Low", File: "a.go", Line: 15, Description: "a15"}},
		taggedFinding{Step: "code review", finding: finding{Severity: "Low", File: "../x.go", Line: 3, Description: "bad path"}})
	code, body := send(h, http.MethodGet, "/pr/a/b/3?view=diff", nil)
	if code != http.StatusOK {
		t.Fatalf("diff = %d", code)
	}
	m := regexp.MustCompile(`<a href="#d-(\w+)"><span class="point-no">\d+</span> <span class="point-title">a15</span>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("a15 not in the points index")
	}
	id := m[1]
	mustContain(t, body, `expand?path=a.go&from=10&to=11" hx-target="closest .dl-gap" hx-swap="outerHTML">↕ Expand</button></div>
<div class="dl-label">Not changed in this PR</div>
<div class="dl unchanged"><span class="dl-num">11</span><span class="dl-num">12</span><span class="dl-mark"></span><span class="dl-sign"></span><span class="dl-code">L12</span></div>`,
		`<div class="dl unchanged"><span class="dl-num">14</span><span class="dl-num">15</span><span class="dl-mark"><a class="mark" href="#d-`+id+`"`,
		`<span class="dl-code">L15</span></div>
<div class="inline point" id="d-`+id+`">`,
		`<span class="dl-code">L18</span></div>
<div class="dl-gap"><button class="link-btn" type="button" hx-get="/pr/a/b/3/diff/expand?path=a.go&from=19&to=20"`)
	if strings.Count(body, `id="d-`+id+`"`) != 1 || strings.Contains(body, "from=10&to=20") {
		t.Errorf("point %q: not one block, or the whole gap still offered", id)
	}
	if fmt.Sprint(f.paths) != "[a.go z.go]" && fmt.Sprint(f.paths) != "[z.go a.go]" { // not the invalid ../x.go
		t.Errorf("content read %v", f.paths)
	}
	send(h, http.MethodGet, "/pr/a/b/3?view=diff", nil)
	if f.reads != 2 {
		t.Errorf("%d content calls after a second load, want 2 (cached)", f.reads)
	}
	// the pieces left expand with the gap's old numbers, without the shown lines
	for q, want := range map[string]string{"from=10&to=11": "10 9 L10|11 10 L11|", "from=19&to=20": "19 18 L19|20 19 L20|", "from=25&to=0": "25 23 L25|"} {
		_, got := send(h, http.MethodGet, "/pr/a/b/3/diff/expand?path=a.go&"+q, nil)
		var rows strings.Builder
		for _, l := range strings.Split(strings.TrimSpace(got), "\n") {
			var n, o int
			var text string
			fmt.Sscanf(l, `<div class="dl"><span class="dl-num">%d</span><span class="dl-num">%d</span><span class="dl-mark"></span><span class="dl-sign"></span><span class="dl-code">%s`, &o, &n, &text)
			fmt.Fprintf(&rows, "%d %d %s|", n, o, strings.TrimSuffix(text, "</span></div>"))
		}
		if rows.String() != want {
			t.Errorf("expand %s = %s, want %s", q, rows.String(), want)
		}
	}
}

// When content fails, times out or is not available, points fall back to their blocks and the page still loads.
func TestDiffUnchangedFallback(t *testing.T) {
	defer func(d time.Duration) { contentTimeout = d }(contentTimeout)
	contentTimeout = 50 * time.Millisecond
	for _, c := range []struct {
		name   string
		cerr   error
		block  bool
		cached bool
	}{{"failing", errors.New("gh: boom"), false, false}, {"not available", errNotAvailable, false, true}, {"blocking", nil, true, false}} {
		h, f, j := diffSetup(t)
		j.Findings = append(j.Findings, taggedFinding{Step: "code review", finding: finding{Severity: "Low", File: "a.go", Line: 15, Description: "a15"}})
		f.cerr, f.block = c.cerr, c.block
		reads := func() int {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.reads
		}
		start := time.Now()
		code, body := send(h, http.MethodGet, "/pr/a/b/3?view=diff", nil)
		if code != http.StatusOK || time.Since(start) > 2*time.Second {
			t.Errorf("%s: %d after %v", c.name, code, time.Since(start))
		}
		mustContain(t, body, `<span class="path">a.go</span><span class="stat-add">+1</span><span class="stat-del">−0</span><span class="muted file-status">modified</span></div>
<div class="file-threads">
<span class="label">Not on a diff line</span>`, "expand?path=a.go&from=10&to=20", `<code>z.go</code>
<div class="inline point" id="d-f0">`)
		if strings.Contains(body, "unchanged") {
			t.Errorf("%s: unchanged rows shown", c.name)
		}
		first := reads()
		send(h, http.MethodGet, "/pr/a/b/3?view=diff", nil)
		if again := reads() - first; again == 0 != c.cached {
			t.Errorf("%s: %d content calls on the second load, cached %v", c.name, again, c.cached)
		}
	}
}

// countingWriter counts body Writes, the chunks WebKit would get from Wails on macOS.
type countingWriter struct {
	*httptest.ResponseRecorder
	writes int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.writes++
	return c.ResponseRecorder.Write(p)
}

func TestResponseIsOneWrite(t *testing.T) {
	h, f, _ := diffSetup(t)
	var patch strings.Builder
	patch.WriteString("@@ -1,300 +1,300 @@\n")
	for n := 1; n <= 300; n++ {
		fmt.Fprintf(&patch, "-old%d\n+new%d\n", n, n)
	}
	f.list = []prFile{file("one.go", "modified", patch.String()), file("two.go", "modified", patch.String()), file("three.go", "modified", patch.String())}
	for _, path := range []string{"/pr/a/b/3?view=diff", "/pr/a/b/3"} {
		w := &countingWriter{ResponseRecorder: httptest.NewRecorder()}
		h.ServeHTTP(w, localReq(http.MethodGet, path, ""))
		if w.Code != http.StatusOK || w.writes != 1 {
			t.Fatalf("%s = %d with %d writes, want 200 with 1", path, w.Code, w.writes)
		}
		if strings.Contains(path, "diff") {
			mustContain(t, w.Body.String(), "three.go", "new300")
		}
	}
}
