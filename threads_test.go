package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func noThreads(context.Context, string, int) (threadList, error) { return threadList{}, nil }
func noResolve(context.Context, string, bool) (bool, error)      { return false, errors.New("no resolve") }

// fakeThreads serves per-PR thread lists and records resolve calls.
type fakeThreads struct {
	mu         sync.Mutex
	lists      map[string]threadList // by prKey
	err        error
	calls      int      // threads calls
	resolved   []string // "<id> <resolved>" per resolve call
	resolveErr error
}

func (f *fakeThreads) threads(_ context.Context, repo string, number int) (threadList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.lists[prKey(pullRequest{Repo: repo, Number: number})], f.err
}

func (f *fakeThreads) resolve(_ context.Context, id string, resolved bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, id+" "+map[bool]string{true: "1", false: "0"}[resolved])
	return resolved, f.resolveErr
}

func (f *fakeThreads) set(fn func(f *fakeThreads)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// a/b#3's threads: T1 open on a.go:9 (row f5), T2 resolved and locked on b.go:3 (row f3), T3 on b.go:21 (no row).
func abThreads(head string) threadList {
	c := func(body string) threadComment {
		return threadComment{Author: "octo", Body: body, URL: "https://github.com/a/b/pull/3#discussion_r1", Created: time.Now().Add(-2 * time.Hour)}
	}
	return threadList{Head: head, Threads: []reviewThread{
		{ID: "T1", Path: "a.go", Side: "RIGHT", Line: 9, CanReply: true, CanResolve: true,
			Comments: []threadComment{c(`<script>alert(1)</script>`), c(`see <a hx-get="/x">here</a>`)}},
		{ID: "T2", Path: "b.go", Side: "RIGHT", Line: 3, Resolved: true, CanUnresolve: true, Comments: []threadComment{c("done")}},
		{ID: "T3", Path: "b.go", Side: "RIGHT", Line: 21, CanReply: true, Comments: []threadComment{c("elsewhere")}},
	}}
}

// threadsSetup is submitSetup with fake threads: a/b#3 as abThreads at the reviewed commit, a/b#4 with thread OTHER.
func threadsSetup(t *testing.T) (http.Handler, *reviewQueue, *fakeThreads, *fakeSubmit) {
	cfg, p, q, _ := doneSetup(t)
	if err := os.MkdirAll(q.pending.dir(), 0700); err != nil {
		t.Fatal(err)
	}
	ft := &fakeThreads{lists: map[string]threadList{"a/b#3": abThreads("abc123"),
		"a/b#4": {Threads: []reviewThread{{ID: "OTHER", CanReply: true, CanResolve: true}}}}}
	fs := &fakeSubmit{}
	return fresh(q, newHandler(newConfigHolder(cfg, "myreview.json"), p, q, fs.submit, notApproved, noFiles, noContent, ft.threads, ft.resolve)), q, ft, fs
}

func TestRowThreads(t *testing.T) {
	threads := abThreads("h").Threads
	threads = append(threads, reviewThread{ID: "L", Path: "a.go", Side: "LEFT", Line: 9}, reviewThread{ID: "O", Path: "a.go", Side: "RIGHT", Line: 9, Outdated: true})
	items := []checkItem{{ID: "f5", File: "a.go", Line: 9}, {ID: "f3", File: "b.go", Line: 3}, {ID: "f2", File: "b.go", Line: 20}, {ID: "f1", File: "b.go"}}
	m := rowThreads(threads, items)
	ids := func(id string) (out []string) {
		for _, t := range m[id] {
			out = append(out, t.ID)
		}
		return out
	}
	if !reflect.DeepEqual(ids("f5"), []string{"T1"}) || !reflect.DeepEqual(ids("f3"), []string{"T2"}) || len(m["f2"]) != 0 || len(m["f1"]) != 0 {
		t.Errorf("rowThreads = %v", m)
	}
	withThreads(items, items, threads, "a/b", 3, pendingPR{Replies: []pendingReply{{Thread: "T1", Text: "mine"}}})
	if items[0].OpenComments != 2 || len(items[0].Threads) != 1 || items[0].Threads[0].Reply.Text != "mine" {
		t.Errorf("open row: %+v", items[0])
	}
	if items[1].OpenComments != 0 || len(items[1].Threads) != 1 { // resolved only: no marker
		t.Errorf("resolved row: %+v", items[1])
	}
	if items[2].OpenComments != 0 || items[2].Threads != nil {
		t.Errorf("line mismatch: %+v", items[2])
	}
	// a finding and your line comment on one line: the thread goes on the first row only
	both := []checkItem{{ID: "f5", File: "a.go", Line: 9}, {ID: "c1", File: "a.go", Line: 9}}
	if m := rowThreads(threads, both); len(m["f5"]) != 1 || len(m["c1"]) != 0 {
		t.Errorf("thread on two rows: %v", m)
	}
}

func TestThreadsListView(t *testing.T) {
	h, _, ft, _ := threadsSetup(t)
	body := pageOf(t, h)
	mustContain(t, body, `<span class="thread-mark" title="Open conversation on this line">2</span>`,
		`<details class="resolved-thread"><summary>Resolved conversation</summary>`,
		`&lt;script&gt;alert(1)&lt;/script&gt;`, `hx-post="/pr/a/b/3/threads/T1/replies"`, `hx-post="/pr/a/b/3/threads/T2/resolve" hx-vals='{"resolved": "0"}'`)
	if n := strings.Count(body, `class="thread-mark"`); n != 1 {
		t.Errorf("%d thread marks, want 1 (a.go:9)", n)
	}
	if strings.Contains(body, "<script>alert") || strings.Contains(body, `hx-get="/x"`) || strings.Contains(body, "elsewhere") {
		t.Error("thread text unescaped, or a thread without a row listed")
	}
	// the point fragment uses the cache
	code, frag := send(h, http.MethodGet, "/pr/a/b/3/points/f5", nil)
	if code != http.StatusOK || !strings.Contains(frag, `&lt;script&gt;`) || strings.Contains(frag, "<script>alert") || strings.Contains(frag, `hx-get="/x"`) {
		t.Errorf("fragment = %d:\n%s", code, frag)
	}

	// threads of another commit than the reviewed one: no placement, one info line
	ft.set(func(f *fakeThreads) { f.lists["a/b#3"] = abThreads("newer99") })
	body = pageOf(t, h)
	mustContain(t, body, `GitHub discussions are shown when the review is of the current head (<code class="sha">newer99</code>).`)
	if strings.Contains(body, `class="thread"`) || strings.Contains(body, `class="thread-mark"`) {
		t.Error("threads placed on a result of another commit")
	}

	// a failing fetch: the warning, and the rest of the page works
	ft.set(func(f *fakeThreads) { f.err = errors.New("boom") })
	body = pageOf(t, h)
	mustContain(t, body, `<p class="alert alert-warning"><span>Could not load GitHub discussions: boom</span></p>`, `id="pt-f5"`)
}

func TestThreadReplies(t *testing.T) {
	h, q, _, _ := threadsSetup(t)
	reply := func(id, text string) (int, string) {
		return send(h, http.MethodPost, "/pr/a/b/3/threads/"+id+"/replies", url.Values{"text": {text}})
	}
	code, body := reply("T1", "<b>ok</b>")
	if code != http.StatusOK || q.pending.get("a/b#3").Rev != 1 {
		t.Fatalf("add = %d, rev %d:\n%s", code, q.pending.get("a/b#3").Rev, body)
	}
	mustContain(t, body, `<span class="state state-draft">Pending</span></div><div class="comment-body">&lt;b&gt;ok&lt;/b&gt;</div>`,
		`<button id="submit-count" hx-swap-oob="true">Submit review (1)</button>`, noError)
	if got := q.pending.get("a/b#3").Replies; !reflect.DeepEqual(got, []pendingReply{{Thread: "T1", Path: "a.go", Line: 9, Text: "<b>ok</b>"}}) {
		t.Errorf("replies = %+v", got)
	}
	if code, _ := reply("T1", "changed"); code != http.StatusOK || q.pending.get("a/b#3").Rev != 2 || q.pending.get("a/b#3").Replies[0].Text != "changed" {
		t.Errorf("update = %d, %+v", code, q.pending.get("a/b#3"))
	}
	mustContain(t, pageOf(t, h), "Submit review (1)")
	if code, body := reply("T1", "  "); code != http.StatusOK || q.pending.get("a/b#3").Rev != 4 || len(q.pending.get("a/b#3").Replies) != 0 || !strings.Contains(body, "Submit review (0)") {
		t.Errorf("delete = %d, %+v", code, q.pending.get("a/b#3"))
	}
	if code, body := reply("T2", "locked"); code != http.StatusConflict || !strings.Contains(body, "you cannot reply to this conversation") {
		t.Errorf("locked thread = %d %q", code, body)
	}
	if code, body := reply("T1", strings.Repeat("x", maxCommentLen+1)); code != http.StatusBadRequest || !strings.Contains(body, "over 65536") {
		t.Errorf("too long = %d", code)
	}
	if p := q.pending.get("a/b#3"); p.Rev != 4 || len(p.Replies) != 0 {
		t.Errorf("refused replies changed the pending review: %+v", p)
	}
	// a reply to a thread that got locked can still be deleted
	q.pending.update("a/b#3", func(p *pendingPR) error {
		p.Replies = []pendingReply{{Thread: "T2", Path: "b.go", Line: 3, Text: "old"}}
		return nil
	})
	if code, body := reply("T2", ""); code != http.StatusOK || len(q.pending.get("a/b#3").Replies) != 0 {
		t.Errorf("delete on locked thread = %d, %+v:\n%s", code, q.pending.get("a/b#3"), body)
	}
}

func TestThreadFetchFailsNoEntry(t *testing.T) {
	h, q, ft, _ := threadsSetup(t)
	ft.set(func(f *fakeThreads) { f.err = errors.New("boom") }) // no page load: the cache has no entry
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localReq(http.MethodPost, "/pr/a/b/3/threads/T1/replies", "text=typed"))
	code, hdr, body := rec.Code, rec.Header(), rec.Body.String()
	// the block is not swapped (HX-Reswap none), so it keeps its text and buttons; the error goes in the bar
	if code != http.StatusBadGateway || hdr.Get("HX-Reswap") != "none" {
		t.Errorf("fetch failure = %d, HX-Reswap %q", code, hdr.Get("HX-Reswap"))
	}
	mustContain(t, body, `<div id="pending-error" role="alert" hx-swap-oob="true"><p class="alert alert-error"><span>Could not load GitHub discussions: boom</span></p></div>`)
	if p := q.pending.get("a/b#3"); p.Rev != 0 {
		t.Errorf("pending changed: %+v", p)
	}
}

func TestThreadCheck(t *testing.T) {
	h, q, ft, _ := threadsSetup(t)
	for _, id := range []string{"OTHER", "nope", strings.Repeat("T", maxThreadID+1)} {
		if code, _ := send(h, http.MethodPost, "/pr/a/b/3/threads/"+id+"/replies", url.Values{"text": {"hi"}}); code != http.StatusNotFound {
			t.Errorf("reply to %.10s = %d, want 404", id, code)
		}
		if code, _ := send(h, http.MethodPost, "/pr/a/b/3/threads/"+id+"/resolve", url.Values{"resolved": {"1"}}); code != http.StatusNotFound {
			t.Errorf("resolve %.10s = %d, want 404", id, code)
		}
	}
	if p := q.pending.get("a/b#3"); p.Rev != 0 || ft.resolved != nil {
		t.Errorf("refused ids wrote: pending %+v, resolve calls %v", p, ft.resolved)
	}
}

func TestThreadResolve(t *testing.T) {
	h, _, ft, _ := threadsSetup(t)
	if code, _ := send(h, http.MethodPost, "/pr/a/b/3/threads/T1/resolve", url.Values{"resolved": {"yes"}}); code != http.StatusBadRequest {
		t.Errorf("resolved=yes = %d, want 400", code)
	}
	code, body := send(h, http.MethodPost, "/pr/a/b/3/threads/T1/resolve", url.Values{"resolved": {"1"}})
	if code != http.StatusOK || !reflect.DeepEqual(ft.resolved, []string{"T1 1"}) {
		t.Fatalf("resolve = %d, calls %v", code, ft.resolved)
	}
	mustContain(t, body, `<span class="state state-posted">Resolved</span>`, `hx-vals='{"resolved": "0"}'`, ">Unresolve</button>")
	// the cache has GitHub's answer: the row has no open marker now
	if _, frag := send(h, http.MethodGet, "/pr/a/b/3/points/f5", nil); !strings.Contains(frag, `<details class="resolved-thread">`) {
		t.Errorf("cached thread not resolved:\n%s", frag)
	}
	ft.set(func(f *fakeThreads) { f.resolveErr = errors.New("gh: forbidden") })
	code, body = send(h, http.MethodPost, "/pr/a/b/3/threads/T1/resolve", url.Values{"resolved": {"0"}})
	if code != http.StatusBadGateway || !strings.Contains(body, `<p class="alert alert-error" role="alert"><span>gh: forbidden</span></p>`) {
		t.Errorf("gh error = %d:\n%s", code, body)
	}
}

func TestSubmitHandlerReplies(t *testing.T) {
	h, q, ft, fs := threadsSetup(t)
	for _, id := range []string{"T1", "T3"} {
		if code, body := send(h, http.MethodPost, "/pr/a/b/3/threads/"+id+"/replies", url.Values{"text": {"re " + id}}); code != http.StatusOK {
			t.Fatalf("reply %s = %d %s", id, code, body)
		}
	}
	// T3 is deleted on GitHub
	ft.set(func(f *fakeThreads) { tl := abThreads("abc123"); tl.Threads = tl.Threads[:2]; f.lists["a/b#3"] = tl })
	pageOf(t, h) // refetches
	_, dlg := send(h, http.MethodGet, "/pr/a/b/3/review/summary?event=COMMENT", nil)
	mustContain(t, dlg, `<li><details><summary><span class="mono" title="Reply · a.go:9">Reply · a.go:9</span><span class="snip">re T1</span></summary><div class="text">re T1</div></details></li>`,
		`<li><details><summary><span class="mono" title="Reply · b.go:21">Reply · b.go:21</span><span class="snip">re T3</span></summary><div class="text">re T3</div></details><span class="discard">thread no longer available, will be discarded</span></li>`,
		">Post 1 comment</button>")

	// a failing fetch at submit posts nothing
	ft.set(func(f *fakeThreads) { f.err = errors.New("boom"); f.calls = 0 })
	if code, msg, _ := postReview(h, reviewForm(q, "COMMENT")); code != http.StatusBadGateway || !strings.Contains(msg, "boom") || fs.count() != 0 || ft.calls != 1 {
		t.Fatalf("fetch failure = %d %q, %d submits, %d fetches (want 1)", code, msg, fs.count(), ft.calls)
	}
	ft.set(func(f *fakeThreads) { f.err = nil })
	code, msg, redirect := postReview(h, reviewForm(q, "COMMENT"))
	if code != http.StatusOK || redirect != "/pr/a/b/3?submitted=COMMENT&n=1" {
		t.Fatalf("submit = %d %q %q", code, msg, redirect)
	}
	if got := fs.calls[0].Replies; !reflect.DeepEqual(got, []threadReply{{"T1", "re T1"}}) {
		t.Errorf("sent replies %+v", got)
	}
	if p := q.pending.get("a/b#3"); len(p.Replies) != 0 {
		t.Errorf("replies left after submit: %+v", p.Replies)
	}
}

func TestSubmitOnlyVanishedReply(t *testing.T) {
	h, q, ft, fs := threadsSetup(t)
	send(h, http.MethodPost, "/pr/a/b/3/threads/T3/replies", url.Values{"text": {"gone soon"}})
	ft.set(func(f *fakeThreads) { tl := abThreads("abc123"); tl.Threads = tl.Threads[:2]; f.lists["a/b#3"] = tl })
	code, msg, _ := postReview(h, reviewForm(q, "COMMENT"))
	if code != http.StatusConflict || !strings.Contains(msg, "Add a comment or a summary") || fs.count() != 0 {
		t.Fatalf("submit = %d %q, %d submits", code, msg, fs.count())
	}
	if p := q.pending.get("a/b#3"); len(p.Replies) != 0 {
		t.Errorf("vanished reply kept: %+v", p.Replies)
	}
}

func TestSubmitRepliesTimeoutKept(t *testing.T) {
	h, q, _, fs := threadsSetup(t)
	reviewTimeout = 20 * time.Millisecond
	t.Cleanup(func() { reviewTimeout = ghTimeout })
	fs.fn = func(ctx context.Context) (string, error) { <-ctx.Done(); return "", ctx.Err() }
	send(h, http.MethodPost, "/pr/a/b/3/threads/T1/replies", url.Values{"text": {"hi"}})
	rev := q.pending.get("a/b#3").Rev
	if code, msg, _ := postReview(h, reviewForm(q, "COMMENT")); code != http.StatusBadGateway || !strings.Contains(msg, "may have been posted") {
		t.Fatalf("timeout = %d %q", code, msg)
	}
	if p := q.pending.get("a/b#3"); len(p.Replies) != 1 || p.Rev != rev+1 {
		t.Errorf("after timeout: %+v, want the reply kept and rev %d", p, rev+1)
	}
}

func TestThreadsArgs(t *testing.T) {
	var got []string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		got = args
		return []byte(`{"data":{"repository":{"pullRequest":{"headRefOid":"h1","reviewThreads":{"nodes":[]}}}}}`), nil
	}
	tl, err := threadsWith(run)(context.Background(), "own/repo", 12)
	want := []string{"api", "graphql", "--paginate", "-f", "query=" + threadsQuery, "-f", "owner=own", "-f", "name=repo", "-F", "number=12"}
	if err != nil || tl.Head != "h1" || !reflect.DeepEqual(got, want) {
		t.Errorf("threadsWith = %+v %v, args %q", tl, err, got)
	}
}

func TestThreadDiscardLockedReply(t *testing.T) {
	h, q, _, _ := threadsSetup(t)
	discard := `hx-post="/pr/a/b/3/threads/T2/replies" hx-vals='{"text": ""}' hx-target="closest .thread" hx-swap="outerHTML">Discard reply</button>`
	if body := pageOf(t, h); strings.Contains(body, "Discard reply") {
		t.Error("Discard reply without a pending reply")
	}
	q.pending.update("a/b#3", func(p *pendingPR) error {
		p.Replies = []pendingReply{{Thread: "T2", Path: "b.go", Line: 3, Text: "old"}, {Thread: "T1", Path: "a.go", Line: 9, Text: "mine"}}
		return nil
	})
	body := pageOf(t, h)
	mustContain(t, body, discard)
	if n := strings.Count(body, "Discard reply"); n != 1 {
		t.Errorf("%d Discard reply buttons, want 1 (locked T2 only)", n)
	}
}

// diffThreads: on a.go (see aPatch) RIGHT 9 open, LEFT 6 on the deleted line, LEFT 8 on context new 9, RIGHT 7 resolved,
// outdated, file-level and unmatched ones at the file head; one on a file not in the diff.
func diffThreads(head string) threadList {
	c := threadComment{Author: "octo", Body: "x", Created: time.Now()}
	two := []threadComment{c, c}
	return threadList{Head: head, Threads: []reviewThread{
		{ID: "R9", Path: "a.go", Side: "RIGHT", Line: 9, CanReply: true, CanResolve: true,
			Comments: []threadComment{{Author: "octo", Body: `<script>alert(2)</script>`}, {Author: "octo", Body: `see <a hx-get="/x">here</a>`}}},
		{ID: "L6", Path: "a.go", Side: "LEFT", Line: 6, Comments: two},
		{ID: "L8", Path: "a.go", Side: "LEFT", Line: 8, Comments: two},
		{ID: "R7", Path: "a.go", Side: "RIGHT", Line: 7, Resolved: true, CanUnresolve: true, Comments: two, More: 1},
		{ID: "OUT", Path: "a.go", Side: "RIGHT", Outdated: true, Comments: two},
		{ID: "FILE", Path: "a.go", Side: "RIGHT", Comments: two},
		{ID: "U15", Path: "a.go", Side: "RIGHT", Line: 15, Comments: two},
		{ID: "GONE", Path: "nope.go", Side: "RIGHT", Line: 1, Comments: two},
	}}
}

func TestNewDiffViewThreads(t *testing.T) {
	d, err := buildDiff(diffFiles(), sha)
	if err != nil {
		t.Fatal(err)
	}
	var tv []threadView
	for _, th := range diffThreads(sha).Threads {
		tv = append(tv, newThreadView("a/b", 3, th, pendingPR{}))
	}
	v := newDiffView(d, nil, nil, tv, nil)
	ids := func(ts []threadView) (out []string) {
		for _, t := range ts {
			out = append(out, t.T.ID)
		}
		return out
	}
	on := map[string][]string{}
	resolved := map[string]int{}
	for _, f := range v.Files {
		for _, r := range f.Rows {
			if len(r.Threads) > 0 {
				k := fmt.Sprintf("%s %s %d/%d", f.Filename, r.Line.Kind, r.Line.Old, r.Line.New)
				on[k] = ids(r.Threads)
				resolved[k] = r.ResolvedComments()
			}
		}
		if f.Filename == "a.go" && !reflect.DeepEqual(ids(f.Threads), []string{"OUT", "FILE", "U15"}) {
			t.Errorf("a.go head = %v", ids(f.Threads))
		} else if f.Filename != "a.go" && f.Threads != nil {
			t.Errorf("%s head = %v", f.Filename, ids(f.Threads))
		}
	}
	want := map[string][]string{"a.go context 8/9": {"R9", "L8"}, "a.go del 6/0": {"L6"}, "a.go add 0/7": {"R7"}}
	if !reflect.DeepEqual(on, want) {
		t.Errorf("rows = %v, want %v", on, want)
	}
	if resolved["a.go add 0/7"] != 3 || resolved["a.go context 8/9"] != 0 {
		t.Errorf("resolved comments = %v", resolved)
	}
}

func TestDiffViewThreads(t *testing.T) {
	cfg, p, q, _ := doneSetup(t)
	if err := os.MkdirAll(q.pending.dir(), 0700); err != nil {
		t.Fatal(err)
	}
	f := &fakeGH{list: diffFiles()}
	ft := &fakeThreads{lists: map[string]threadList{"a/b#3": diffThreads(sha)}}
	h := fresh(q, newHandler(newConfigHolder(cfg, "myreview.json"), p, q, nil, notApproved, f.filesFn, f.contentFn, ft.threads, ft.resolve))
	code, body := send(h, http.MethodGet, "/pr/a/b/3?view=diff", nil)
	if code != http.StatusOK {
		t.Fatalf("diff = %d", code)
	}
	mustContain(t, body,
		`<div class="inline"><div class="inline-head"><span class="thread-mark">Conversation</span></div><div class="inline-body"><div class="thread">`,
		`&lt;script&gt;alert(2)&lt;/script&gt;`, `see &lt;a hx-get=&#34;/x&#34;&gt;here&lt;/a&gt;`,
		`hx-post="/pr/a/b/3/threads/R9/resolve"`,
		// the comment form goes after the line's resolved slot, which sits right after the line: the icon stays on its line
		`hx-target="#rc-0-4" hx-swap="afterend">7</a><span class="dl-mark"></span><span class="dl-sign">+</span><span class="dl-code">a7</span></div>
<details class="resolved-conv" id="rc-0-4" hx-vals="{&#34;row&#34;:&#34;0-4&#34;,&#34;row_threads&#34;:&#34;R7&#34;}"><summary aria-label="Resolved conversation (3 comments)" title="Resolved conversation"><span class="resolved-btn" aria-hidden="true">✓</span></summary>`,
		`hx-target="#rc-0-6" hx-swap="afterend">9</a>`, `<span class="dl-code">c9</span></div>
<div id="rc-0-6" hidden></div>`, `hx-target="closest .dl" hx-swap="afterend">5</a>`,
		`hx-post="/pr/a/b/3/threads/R7/resolve" hx-vals='{"resolved": "0"}'`,
		`<div class="file-threads">`, `<span class="badge plain">Outdated</span>`, `<span class="badge plain">File</span>`, `<span class="badge plain">Line 15</span>`)
	if strings.Contains(body, "<script>alert") || strings.Contains(body, `hx-get="/x"`) || strings.Contains(body, "GONE") {
		t.Error("thread text unescaped, or a thread of a file not in the diff shown")
	}
	// R9's open thread comes after the point on that line
	if strings.Index(body, `id="d-f5"`) > strings.Index(body, `threads/R9/resolve`) {
		t.Error("open thread before the point")
	}
	// threads of another head are not placed
	ft.set(func(f *fakeThreads) { f.lists["a/b#3"] = diffThreads("other") })
	_, body = send(h, http.MethodGet, "/pr/a/b/3?view=diff", nil)
	if strings.Contains(body, `class="thread"`) || strings.Contains(body, "resolved-conv") || strings.Contains(body, `<span class="thread-mark">Conversation</span>`) {
		t.Error("threads placed for another head")
	}
}

// In the diff view a resolve re-renders the row's thread slots out of band: the thread moves behind the line's
// icon or back under the line, and the icon's count follows.
func TestDiffResolveMovesThread(t *testing.T) {
	cfg, p, q, _ := doneSetup(t)
	if err := os.MkdirAll(q.pending.dir(), 0700); err != nil {
		t.Fatal(err)
	}
	f := &fakeGH{list: diffFiles()}
	ft := &fakeThreads{lists: map[string]threadList{"a/b#3": diffThreads(sha)}}
	h := fresh(q, newHandler(newConfigHolder(cfg, "myreview.json"), p, q, nil, notApproved, f.filesFn, f.contentFn, ft.threads, ft.resolve))
	if code, _ := send(h, http.MethodGet, "/pr/a/b/3?view=diff", nil); code != http.StatusOK {
		t.Fatalf("diff = %d", code)
	}
	resolve := func(id, v, row, ids string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, localReq(http.MethodPost, "/pr/a/b/3/threads/"+id+"/resolve",
			url.Values{"resolved": {v}, "row": {row}, "row_threads": {ids}}.Encode()))
		if rec.Code != http.StatusOK || rec.Header().Get("HX-Reswap") != "none" {
			t.Fatalf("resolve %s = %d, HX-Reswap %q", id, rec.Code, rec.Header().Get("HX-Reswap"))
		}
		return rec.Body.String()
	}
	// resolve: R9 goes behind a new icon, L8 stays open under the line
	body := resolve("R9", "1", "0-6", "R9 L8")
	mustContain(t, body, `<details class="resolved-conv" id="rc-0-6" hx-vals="{&#34;row&#34;:&#34;0-6&#34;,&#34;row_threads&#34;:&#34;R9 L8&#34;}" hx-swap-oob="true"><summary aria-label="Resolved conversation (2 comments)"`,
		`<div class="row-threads" id="rt-0-6" hx-vals="{&#34;row&#34;:&#34;0-6&#34;,&#34;row_threads&#34;:&#34;R9 L8&#34;}" hx-swap-oob="true">`)
	rt := body[strings.Index(body, `id="rt-0-6"`):]
	if !strings.Contains(body[:strings.Index(body, `id="rt-0-6"`)], "threads/R9/resolve") || strings.Contains(rt, "threads/R9/") || !strings.Contains(rt, `class="thread"`) {
		t.Errorf("R9 not behind the icon or L8 not open:\n%s", body)
	}
	// unresolve the last resolved thread: the icon goes, R7 is open under the line
	body = resolve("R7", "0", "0-4", "R7")
	mustContain(t, body, `<div id="rc-0-4" hidden hx-swap-oob="true"></div>`, `id="rt-0-4"`, `hx-post="/pr/a/b/3/threads/R7/resolve" hx-vals='{"resolved": "1"}'`)
	if strings.Contains(body, "resolved-conv") {
		t.Errorf("icon left after its last thread was unresolved:\n%s", body)
	}
}
