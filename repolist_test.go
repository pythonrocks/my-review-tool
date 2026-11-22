package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// settingsHandler is one handler (one repo list cache) over h.
func settingsHandler(t *testing.T, h *configHolder) http.Handler {
	q := newReviewQueue(0, workflow{}, resultStore{dir: t.TempDir()})
	return newHandler(h, newPoller(nil, nil, noLogin), q, nil, notApproved, noFiles, noContent, noThreads, noResolve)
}

func serveH(hd http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	hd.ServeHTTP(rec, r)
	return rec
}

func addRepo(hd http.Handler, ctx context.Context, origin, q string, chips ...string) *httptest.ResponseRecorder {
	r := localReq(http.MethodPost, "/settings/repo", url.Values{"q": {q}, "repo": chips}.Encode()).WithContext(ctx)
	r.Header.Set("HX-Request", "true")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return serveH(hd, r)
}

// fakeGhLines replaces ghLines: each call waits for release (if set), then sends lines and returns err.
type fakeGhLines struct {
	mu      sync.Mutex
	calls   int
	sent    int // lines the last call delivered
	release chan struct{}
	lines   []string
	err     error
}

func (f *fakeGhLines) run(ctx context.Context, line func(string) bool, args ...string) error {
	f.mu.Lock()
	f.calls++
	rel, lines, err := f.release, f.lines, f.err
	f.mu.Unlock()
	if !slices.Equal(args, repoListArgs) {
		return fmt.Errorf("args %q", args)
	}
	if rel != nil {
		<-rel
	}
	n := 0
	for _, l := range lines {
		n++
		if !line(l) {
			break
		}
	}
	f.mu.Lock()
	f.sent = n
	f.mu.Unlock()
	return err
}

func (f *fakeGhLines) count() (calls, sent int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.sent
}

func useGhLines(t *testing.T, f *fakeGhLines) {
	old := ghLines
	ghLines = f.run
	t.Cleanup(func() { ghLines = old })
}

// suggestDone gets /settings/repos?q= until the list is no longer loading.
func suggestDone(t *testing.T, hd http.Handler, q string) string {
	t.Helper()
	for i := 0; i < 200; i++ {
		body := serveH(hd, localReq(http.MethodGet, "/settings/repos?q="+url.QueryEscape(q), "")).Body.String()
		if !strings.Contains(body, "Loading your repositories") {
			return body
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("still loading")
	return ""
}

func TestSettingsRepoAdd(t *testing.T) {
	var mu sync.Mutex
	var calls [][]string
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		mu.Lock()
		calls = append(calls, args)
		mu.Unlock()
		switch args[len(args)-1] {
		case "o/found", "o/new":
			return []byte(`{"nameWithOwner":"` + strings.ToUpper(args[len(args)-1][:1]) + args[len(args)-1][1:] + `"}`), nil
		case "o/old-name":
			return []byte(`{"nameWithOwner":"o/b"}`), nil
		case "o/slow":
			<-ctx.Done()
			return nil, errors.New("gh timed out")
		}
		return nil, errors.New("HTTP 404: Not Found")
	}
	ncalls := func() int { mu.Lock(); defer mu.Unlock(); return len(calls) }
	path := writeConfig(t, `{"repositories": ["a/b"]}`)
	h, _ := holderFor(t, path)
	h.repoView = repoViewWith(run)
	useGhLines(t, &fakeGhLines{err: errors.New("no list")}) // the cache is never filled here
	hd := settingsHandler(t, h)
	bg := context.Background()

	refused := func(rec *httptest.ResponseRecorder, want string) {
		t.Helper()
		if rec.Code != http.StatusUnprocessableEntity || rec.Header().Get("HX-Retarget") != "#repo-error" || rec.Header().Get("HX-Reswap") != "innerHTML" || rec.Body.String() != want {
			t.Errorf("status %d, retarget %q, body %q, want 422 %q", rec.Code, rec.Header().Get("HX-Retarget"), rec.Body, want)
		}
	}
	for _, s := range []string{"-R/x", "a/-b", "a/..", "a b/c", "", "a", "--help"} {
		refused(addRepo(hd, bg, "", s, "a/b"), "not owner/name")
	}
	if n := ncalls(); n != 0 {
		t.Fatalf("gh ran for invalid input: %v", calls)
	}

	rec := addRepo(hd, bg, appOrigin, " o/found ", "a/b")
	if rec.Code != http.StatusOK {
		t.Fatalf("found: status %d: %s", rec.Code, rec.Body)
	}
	mustContain(t, rec.Body.String(), `<input type="hidden" name="repo" value="O/found">`, `<p id="repo-error" class="repo-error" role="alert" hx-swap-oob="true"></p>`)
	if want := []string{"repo", "view", "--json", "nameWithOwner", "--", "o/found"}; !slices.Equal(calls[0], want) {
		t.Errorf("args %q, want %q", calls[0], want)
	}
	if !h.isValidated("o/found") {
		t.Error("added repo not in the session-validated set")
	}

	refused(addRepo(hd, bg, "", "o/missing", "a/b"), "not found or no access")
	ctx, cancel := context.WithTimeout(bg, 20*time.Millisecond)
	defer cancel()
	refused(addRepo(hd, ctx, "", "o/slow", "a/b"), "timed out")

	n := ncalls()
	refused(addRepo(hd, bg, "", "A/B", "a/b", "o/found"), "A/B is already added")
	chips := make([]string, maxRepos)
	for i := range chips {
		chips[i] = fmt.Sprintf("o/r%d", i)
	}
	refused(addRepo(hd, bg, "", "o/new", chips...), "at most 100 repositories")
	if ncalls() != n {
		t.Errorf("gh ran for a duplicate or the 101st repo: %v", calls[n:])
	}
	if rec := addRepo(hd, bg, "", "o/new", chips[:99]...); rec.Code != http.StatusOK {
		t.Errorf("100th repo refused: %s", rec.Body)
	}
	refused(addRepo(hd, bg, "", "o/old-name", "a/b", "O/B"), "o/b is already added") // a rename resolving to a chip

	if rec := addRepo(hd, bg, "https://evil.example", "o/found", "a/b"); rec.Code != http.StatusForbidden {
		t.Errorf("foreign Origin: status %d", rec.Code)
	}

	// The added repo saves without another gh repo view.
	n = ncalls()
	r := localReq(http.MethodPost, "/settings", settingsForm(h.get(), "", "a/b", "O/found").Encode())
	r.Header.Set("HX-Request", "true")
	if rec := serveH(hd, r); rec.Header().Get("HX-Redirect") != "/" {
		t.Fatalf("save: status %d: %s", rec.Code, rec.Body)
	}
	if ncalls() != n {
		t.Errorf("save ran gh repo view again: %v", calls[n:])
	}
	if got := h.get().Repositories; !slices.Equal(got, []string{"a/b", "O/found"}) {
		t.Errorf("saved %v", got)
	}
}

func TestSettingsRepoAddFromCache(t *testing.T) {
	h, f := holderFor(t, writeConfig(t, `{"repositories": []}`))
	useGhLines(t, &fakeGhLines{lines: []string{"Octo/Cached"}})
	hd := settingsHandler(t, h)
	suggestDone(t, hd, "x")
	rec := addRepo(hd, context.Background(), "", "octo/cached")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `value="Octo/Cached"`) {
		t.Errorf("status %d: %s", rec.Code, rec.Body)
	}
	if len(f.calls) != 0 || !h.isValidated("Octo/Cached") {
		t.Errorf("gh repo view calls %v, validated %v", f.calls, h.isValidated("Octo/Cached"))
	}
}

func TestSettingsRepoSuggest(t *testing.T) {
	h := newConfigHolder(defaultConfig(), "myreview.json")
	lines := []string{"Other/ABC", "-bad/x", ""}
	for i := 0; i < 30; i++ {
		lines = append(lines, fmt.Sprintf("own/repo-%d", i))
	}
	f := &fakeGhLines{release: make(chan struct{}), lines: lines}
	useGhLines(t, f)
	hd := settingsHandler(t, h)

	if body := serveH(hd, localReq(http.MethodGet, "/settings/repos?q=", "")).Body.String(); body != "" {
		t.Errorf("empty q: %q", body)
	}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mustContain(t, serveH(hd, localReq(http.MethodGet, "/settings/repos?q=a", "")).Body.String(),
				"Loading your repositories…", `hx-trigger="load delay:1s" hx-include="#repo-q"`)
		}()
	}
	wg.Wait()
	close(f.release)
	body := suggestDone(t, hd, "REPO")
	if n := strings.Count(body, `hx-post="/settings/repo"`); n != maxSuggestions {
		t.Errorf("%d suggestions, want %d", n, maxSuggestions)
	}
	mustContain(t, body, `hx-vals='{"q":"own/repo-0"}'`, `hx-params="q,repo"`, ">own/repo-0</button>")
	mustContain(t, suggestDone(t, hd, "abc"), ">Other/ABC</button>")
	mustContain(t, suggestDone(t, hd, "zzz"), "None of your repositories match")
	if strings.Contains(suggestDone(t, hd, "bad"), "-bad") {
		t.Error("an invalid name from gh is suggested")
	}
	if c, _ := f.count(); c != 1 {
		t.Errorf("%d fetches, want 1", c)
	}
	if strings.Contains(body, "first 1,000") {
		t.Error("cap note without the cap")
	}
}

func TestSettingsRepoSuggestOtherFailure(t *testing.T) {
	useGhLines(t, &fakeGhLines{err: errors.New("HTTP 502: bad gateway")})
	body := suggestDone(t, settingsHandler(t, newConfigHolder(defaultConfig(), "myreview.json")), "a")
	mustContain(t, body, "Could not load your repositories: HTTP 502: bad gateway <button", ">Retry</button>")
}

func TestSettingsRepoSuggestFailureAndRetry(t *testing.T) {
	f := &fakeGhLines{err: globalError(msgGhNotLoggedIn)}
	useGhLines(t, f)
	hd := settingsHandler(t, newConfigHolder(defaultConfig(), "myreview.json"))
	body := suggestDone(t, hd, "a")
	mustContain(t, body, "Could not load your repositories: GitHub CLI is not logged in. Run <code>gh auth login</code> in Terminal, then Retry. <button", `hx-get="/settings/repos?retry=1"`, ">Retry</button>")
	suggestDone(t, hd, "a")
	if c, _ := f.count(); c != 1 {
		t.Fatalf("a failed fetch restarted without Retry: %d calls", c)
	}
	f.mu.Lock()
	f.err, f.lines = nil, []string{"a/b"}
	f.mu.Unlock()
	serveH(hd, localReq(http.MethodGet, "/settings/repos?q=a&retry=1", ""))
	mustContain(t, suggestDone(t, hd, "a"), ">a/b</button>")
	if c, _ := f.count(); c != 2 {
		t.Errorf("%d calls, want 2", c)
	}
}

func TestSettingsRepoSuggestCap(t *testing.T) {
	var lines []string
	for i := 0; i < 1500; i++ {
		lines = append(lines, fmt.Sprintf("o/r%04d", i))
	}
	f := &fakeGhLines{lines: lines}
	useGhLines(t, f)
	hd := settingsHandler(t, newConfigHolder(defaultConfig(), "myreview.json"))
	mustContain(t, suggestDone(t, hd, "r0999"), ">o/r0999</button>", "Showing the first 1,000; type owner/name to add others.")
	if body := suggestDone(t, hd, "r1000"); strings.Contains(body, "o/r1000") {
		t.Error("repo past the cap listed")
	}
	if _, sent := f.count(); sent != maxListRepos+1 {
		t.Errorf("read %d lines, want the fetch stopped at %d", sent, maxListRepos+1)
	}
}

// TestGhLines runs a fake gh: an endless one is stopped, and a failing one gives a short classified error.
func TestGhLines(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	gh := filepath.Join(dir, "gh")
	os.WriteFile(gh, []byte("#!/bin/sh\nexec yes o/r\n"), 0700)
	n := 0
	start := time.Now()
	err := ghLines(context.Background(), func(string) bool { n++; return n < 1001 }, "api")
	if err != nil || n != 1001 || time.Since(start) > 5*time.Second {
		t.Errorf("endless gh: err %v, %d lines, %v", err, n, time.Since(start))
	}
	os.WriteFile(gh, []byte("#!/bin/sh\necho o/r\necho 'HTTP 401: Bad credentials' >&2\nexit 1\n"), 0700)
	n = 0
	err = ghLines(context.Background(), func(string) bool { n++; return true }, "api")
	if err == nil || err.Error() != "HTTP 401: Bad credentials" || n != 1 {
		t.Errorf("failing gh: err %v, %d lines", err, n)
	}
}
