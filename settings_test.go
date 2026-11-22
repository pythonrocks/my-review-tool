package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRepoView records the repos it is asked about and accepts all but "no/such".
type fakeRepoView struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeRepoView) view(_ context.Context, repo string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, repo)
	if repo == "no/such" {
		return "", errRepoNotFound
	}
	return repo, nil
}

// holderFor loads the config at path into a holder whose gh checks go to a fakeRepoView.
func holderFor(t *testing.T, path string) (*configHolder, *fakeRepoView) {
	t.Helper()
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h, f := newConfigHolder(cfg, path), &fakeRepoView{}
	h.repoView = f.view
	return h, f
}

// unchanged is the input the Settings page sends for cfg when the user changes nothing.
func unchanged(cfg config) settingsInput {
	return settingsInput{Repos: cfg.Repositories, CodeReview: cfg.Steps.CodeReview.Prompt,
		SecurityReview: cfg.Steps.SecurityReview.Prompt, RiskAssessment: cfg.Steps.RiskAssessment.Prompt}
}

func decodeJSON(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%v in %s", err, b)
	}
	return v
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// assertNoTemps fails if a temp file of save is left in dir or dir/prompts.
func assertNoTemps(t *testing.T, dir string) {
	t.Helper()
	for _, d := range []string{dir, filepath.Join(dir, "prompts")} {
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".tmp") {
				t.Errorf("temp file %s left", filepath.Join(d, e.Name()))
			}
		}
	}
}

const handEdited = `{
    "repositories": ["a/b", "c/d"],
    "concurrency": 4,
    "quiet_period": "2m",
    "auto_review_max_age": "24h",
    "skip": [{"label": "^wip$"}, {"author": "bot"}],
    "my_note": {"keep": [1, 2]},
    "steps": {
        "code_review": {"agent": "codex", "prompt_file": "mine/cr.md", "x_extra": true},
        "security_review": {"agent": "claude", "prompt_file": ""},
        "risk_assessment": {"agent": "claude", "prompt_file": ""},
        "future_step": {"agent": "claude"}
    }
}`

func TestSaveRoundTripsHandEditedConfig(t *testing.T) {
	path := writeConfig(t, handEdited)
	dir := filepath.Dir(path)
	os.MkdirAll(filepath.Join(dir, "mine"), 0700)
	os.WriteFile(filepath.Join(dir, "mine", "cr.md"), []byte("my review prompt\r\nline 2\n"), 0600)
	h, f := holderFor(t, path)
	cfg, err := h.save(context.Background(), unchanged(h.get()))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := decodeJSON(t, []byte(readFile(t, path))), decodeJSON(t, []byte(handEdited)); !reflect.DeepEqual(got, want) {
		t.Errorf("saved %v\nwant %v", got, want)
	}
	if len(f.calls) != 0 {
		t.Errorf("configured repos were checked with gh: %v", f.calls)
	}
	if cfg.Concurrency != 4 || cfg.Steps.CodeReview.Prompt != "my review prompt\r\nline 2\n" || len(cfg.Skip) != 2 || cfg.Skip[0].re == nil {
		t.Errorf("returned config = %+v", cfg)
	}
	if _, err := os.Stat(filepath.Join(dir, "prompts")); err == nil {
		t.Error("prompts dir created with no changed prompt")
	}
	if m := mode(t, path); m != 0600 {
		t.Errorf("config mode = %v, want 0600", m)
	}
	assertNoTemps(t, dir)
}

func TestSavePromptRules(t *testing.T) {
	path := writeConfig(t, `{"steps": {"security_review": {"prompt_file": "sec.md"}, "risk_assessment": {"prompt_file": "risk.md"}}}`)
	dir := filepath.Dir(path)
	os.WriteFile(filepath.Join(dir, "sec.md"), []byte("custom security"), 0600)
	os.WriteFile(filepath.Join(dir, "risk.md"), []byte("custom risk"), 0600)
	h, _ := holderFor(t, path)
	in := unchanged(h.get())
	in.CodeReview = "edited\r\ncode review"        // edited: written to prompts/code-review.md
	in.RiskAssessment = "\n" + riskPrompt + "\n\n" // the built-in: prompt_file emptied
	cfg, err := h.save(context.Background(), in)   // security review unchanged: its path is kept
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Steps map[string]struct {
			PromptFile string `json:"prompt_file"`
		}
	}
	json.Unmarshal([]byte(readFile(t, path)), &saved)
	for step, want := range map[string]string{"code_review": "prompts/code-review.md", "security_review": "sec.md", "risk_assessment": ""} {
		if got := saved.Steps[step].PromptFile; got != want {
			t.Errorf("%s prompt_file = %q, want %q", step, got, want)
		}
	}
	prompt := filepath.Join(dir, "prompts", "code-review.md")
	if got := readFile(t, prompt); got != "edited\ncode review" {
		t.Errorf("prompt file = %q, want CRLF turned into LF", got)
	}
	if m := mode(t, prompt); m != 0600 {
		t.Errorf("prompt mode = %v, want 0600", m)
	}
	if m := mode(t, filepath.Dir(prompt)); m != 0700 {
		t.Errorf("prompts dir mode = %v, want 0700", m)
	}
	if cfg.Steps.CodeReview.Prompt != "edited\ncode review" || cfg.Steps.SecurityReview.Prompt != "custom security" || cfg.Steps.RiskAssessment.Prompt != riskPrompt {
		t.Errorf("returned prompts = %q, %q, %q", cfg.Steps.CodeReview.Prompt, cfg.Steps.SecurityReview.Prompt, cfg.Steps.RiskAssessment.Prompt)
	}
	assertNoTemps(t, dir)
}

func TestSaveRefusesBadInputAndWritesNothing(t *testing.T) {
	const body = `{"repositories": ["a/b"]}`
	tests := []struct {
		name string
		edit func(*settingsInput)
		want string
	}{
		{"blank prompt", func(in *settingsInput) { in.SecurityReview = " \r\n\t" }, "blank"},
		{"long prompt", func(in *settingsInput) { in.CodeReview = strings.Repeat("x", maxPromptBytes+1) }, "at most 65536"},
		{"NUL", func(in *settingsInput) { in.CodeReview = "a\x00b" }, "NUL"},
		{"bad UTF-8", func(in *settingsInput) { in.CodeReview = "a\xffb" }, "UTF-8"},
		{"unknown agent", func(in *settingsInput) { in.Agent = "gpt" }, "agent"},
		{"duplicate repo", func(in *settingsInput) { in.Repos = []string{"a/b", "A/B"} }, "duplicate"},
		{"bad repo", func(in *settingsInput) { in.Repos = []string{"-R/x"} }, "not owner/name"},
		{"repo not found", func(in *settingsInput) { in.Repos = []string{"no/such"} }, "not found"},
		{"too many repos", func(in *settingsInput) {
			in.Repos = nil
			for i := range maxRepos + 1 {
				in.Repos = append(in.Repos, "o/r"+strings.Repeat("x", i))
			}
		}, "at most 100"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, body)
			h, _ := holderFor(t, path)
			in := unchanged(h.get())
			in.CodeReview = "changed, so a prompt file would be written"
			tc.edit(&in)
			if _, err := h.save(context.Background(), in); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if got := readFile(t, path); got != body {
				t.Errorf("config changed to %s", got)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(path), "prompts")); err == nil {
				t.Error("prompts dir written")
			}
			assertNoTemps(t, filepath.Dir(path))
		})
	}
}

func TestSaveAllows100Repos(t *testing.T) {
	h, _ := holderFor(t, writeConfig(t, `{}`))
	in := unchanged(h.get())
	for i := range maxRepos {
		in.Repos = append(in.Repos, "o/r"+strings.Repeat("x", i))
	}
	if _, err := h.save(context.Background(), in); err != nil {
		t.Fatal(err)
	}
}

// A config that parseConfig refuses after a prompt change leaves the existing prompt file and the config as they were.
func TestSaveConfigValidationFailureKeepsPromptFile(t *testing.T) {
	path := writeConfig(t, `{"steps": {"code_review": {"prompt_file": "prompts/code-review.md"}}}`)
	dir := filepath.Dir(path)
	prompt := filepath.Join(dir, "prompts", "code-review.md")
	os.MkdirAll(filepath.Dir(prompt), 0700)
	os.WriteFile(prompt, []byte("saved custom prompt"), 0600)
	h, _ := holderFor(t, path)
	// The file is hand-edited into an invalid config after it was loaded.
	broken := `{"concurrency": 0, "steps": {"code_review": {"prompt_file": "prompts/code-review.md"}}}`
	os.WriteFile(path, []byte(broken), 0600)
	in := unchanged(h.get())
	in.CodeReview = "a new prompt"
	if _, err := h.save(context.Background(), in); err == nil || !strings.Contains(err.Error(), "concurrency must be positive") {
		t.Fatalf("err = %v, want the config validation error", err)
	}
	if got := readFile(t, prompt); got != "saved custom prompt" {
		t.Errorf("prompt file = %q, want it untouched", got)
	}
	if got := readFile(t, path); got != broken {
		t.Errorf("config = %s, want it untouched", got)
	}
	assertNoTemps(t, dir)
}

func TestSaveWithoutConfigStartsFromExample(t *testing.T) {
	path := filepath.Join(t.TempDir(), "myreview.json")
	h, f := newConfigHolder(defaultConfig(), path), &fakeRepoView{}
	h.repoView = f.view
	in := unchanged(h.get())
	in.Repos, in.Agent = []string{"o/r"}, "codex"
	cfg, err := h.save(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Steps.RiskAssessment.Agent != "codex" || cfg.QuietPeriod.Duration != 5*time.Minute || !slices.Equal(cfg.Repositories, []string{"o/r"}) {
		t.Errorf("cfg = %+v", cfg)
	}
	if m := mode(t, path); m != 0600 {
		t.Errorf("config mode = %v, want 0600", m)
	}
	if !slices.Equal(f.calls, []string{"o/r"}) {
		t.Errorf("gh checks = %v, want o/r once", f.calls)
	}
	// o/r is validated now, so saving it again does not ask gh, even before it is applied.
	if _, err := h.save(context.Background(), in); err != nil || len(f.calls) != 1 {
		t.Errorf("second save: err %v, gh checks %v", err, f.calls)
	}
}

func TestValidGitHubRepo(t *testing.T) {
	long := strings.Repeat("a", 101)
	for _, s := range []string{"-R/x", "a/-b", "a/..", "a/.", "a b/c", "a", "a/b/c", long + "/b", "a/" + long, "/b", "a/", "", "a/b c", "a_b/c"} {
		if validGitHubRepo(s) {
			t.Errorf("validGitHubRepo(%q) = true", s)
		}
	}
	for _, s := range []string{"a/b", "Owner-1/repo.name_x-y", "a/.github", "a/_b", strings.Repeat("a", 100) + "/" + strings.Repeat("b", 100)} {
		if !validGitHubRepo(s) {
			t.Errorf("validGitHubRepo(%q) = false", s)
		}
	}
}

func TestRepoView(t *testing.T) {
	var calls [][]string
	run := func(out string, err error) func(context.Context, ...string) ([]byte, error) {
		return func(_ context.Context, args ...string) ([]byte, error) {
			calls = append(calls, args)
			return []byte(out), err
		}
	}
	view := repoViewWith(run(`{"nameWithOwner":"Owner/Repo"}`, nil))
	for _, s := range []string{"-R/x", "a/-b", "a/..", "a/.", "a b/c", "a", "a/b/c", strings.Repeat("a", 101) + "/b", "/b", "a/"} {
		if _, err := view(context.Background(), s); !errors.Is(err, errNotOwnerName) {
			t.Errorf("%q: err = %v", s, err)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("gh ran for invalid input: %v", calls)
	}
	if got, err := view(context.Background(), "owner/repo"); err != nil || got != "Owner/Repo" {
		t.Errorf("got %q, %v", got, err)
	}
	if want := []string{"repo", "view", "--json", "nameWithOwner", "--", "owner/repo"}; !slices.Equal(calls[0], want) {
		t.Errorf("args = %q, want %q", calls[0], want)
	}
	for _, msg := range []string{"GraphQL: Could not resolve to a Repository with the name 'o/r'. (repository)", "HTTP 404: Not Found"} {
		if _, err := repoViewWith(run("", errors.New(msg)))(context.Background(), "o/r"); !errors.Is(err, errRepoNotFound) {
			t.Errorf("%q: err = %v", msg, err)
		}
	}
	other := "HTTP 403: API rate limit exceeded " + strings.Repeat("x", 300)
	if _, err := repoViewWith(run("", errors.New(other)))(context.Background(), "o/r"); err == nil || errors.Is(err, errRepoNotFound) ||
		!strings.HasPrefix(err.Error(), "HTTP 403: API rate limit exceeded") || len([]rune(err.Error())) > 200 {
		t.Errorf("other gh failure: err = %v", err)
	}
	if _, err := repoViewWith(run("", globalError(msgGhNotLoggedIn)))(context.Background(), "o/r"); err == nil || err.Error() != msgGhNotLoggedIn {
		t.Errorf("logged out: err = %v", err)
	}
	blocking := repoViewWith(func(ctx context.Context, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, errors.New("gh timed out")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := blocking(ctx, "o/r"); !errors.Is(err, errRepoTimeout) {
		t.Errorf("blocked view: err = %v", err)
	}
}

// blockingFetch serves fixed PRs per repo; a repo in block waits for its channel to close first.
type blockingFetch struct {
	entered chan string
	block   map[string]chan struct{}
	err     map[string]error
}

func (b *blockingFetch) fetch(ctx context.Context, repo string) ([]pullRequest, error) {
	b.entered <- repo
	if c := b.block[repo]; c != nil {
		<-c
	}
	if err := b.err[repo]; err != nil {
		return nil, err
	}
	return []pullRequest{{Repo: repo, Number: 1}}, nil
}

func nextFetch(t *testing.T, entered chan string) string {
	t.Helper()
	select {
	case r := <-entered:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no fetch started")
		return ""
	}
}

func TestSetReposWakesAndForgets(t *testing.T) {
	b := &blockingFetch{entered: make(chan string, 10), err: map[string]error{"c/d": errors.New("boom")}}
	p := newPoller([]string{"a/b", "c/d"}, b.fetch, noLogin)
	polls := make(chan pollSnapshot, 10)
	p.onPoll = func(s pollSnapshot) { polls <- s }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.run(ctx)
	nextFetch(t, b.entered)
	nextFetch(t, b.entered)
	if s := <-polls; ids(s.PRs) != "a/b#1 " || s.Errors["c/d"] != "boom" || !slices.Equal(s.Fetched, []string{"a/b"}) {
		t.Fatalf("first poll = %+v", s)
	}
	p.setRepos([]string{"e/f"})
	if s := p.snapshot(); len(s.PRs) != 0 || len(s.Errors) != 0 || len(s.Fetched) != 0 {
		t.Errorf("after setRepos = %+v, want the removed repos forgotten", s)
	}
	if r := nextFetch(t, b.entered); r != "e/f" { // well before the 60 s interval
		t.Errorf("woken poll fetched %s", r)
	}
	if s := <-polls; ids(s.PRs) != "e/f#1 " {
		t.Errorf("woken poll = %q", ids(s.PRs))
	}
}

func TestSetReposDuringFetch(t *testing.T) {
	for _, fail := range []bool{false, true} {
		release := make(chan struct{})
		b := &blockingFetch{entered: make(chan string, 10), block: map[string]chan struct{}{"a/b": release}, err: map[string]error{}}
		if fail {
			b.err["a/b"] = errors.New("boom")
		}
		p := newPoller([]string{"a/b"}, b.fetch, noLogin)
		q := newReviewQueue(0, workflow{}, resultStore{dir: t.TempDir()})
		var synced []pullRequest
		p.onPoll = func(s pollSnapshot) { q.sync(s.PRs); synced = append(synced, s.PRs...) }
		done := make(chan struct{})
		go func() {
			p.poll(context.Background())
			p.onPoll(p.snapshot())
			close(done)
		}()
		nextFetch(t, b.entered)
		p.setRepos([]string{"c/d"})
		close(release)
		<-done
		p.mu.Lock()
		_, hasPRs := p.prs["a/b"]
		_, hasErr := p.errs["a/b"]
		p.mu.Unlock()
		if hasPRs || hasErr || len(synced) != 0 || len(q.statuses()) != 0 {
			t.Errorf("fail=%v: removed repo kept: prs %v, errs %v, synced %v", fail, hasPRs, hasErr, ids(synced))
		}
	}
}

func TestSetWorkflowDuringRun(t *testing.T) {
	q, f := queueFakes(t, 1)
	g := newFakes()
	q.sync([]pullRequest{pr(1)})
	c1 := f.next(t)
	newWF := q.wf
	newWF.codeReview = g.step("new code review")
	q.setWorkflow(newWF)
	q.sync([]pullRequest{pr(1), pr(2)})
	c1.ret <- nil // the running job ends with the old workflow
	waitStatus(t, q, 1, statusDone)
	c2 := g.next(t)
	if c2.pr != key(2) || c2.step != "new code review" {
		t.Errorf("next job = %+v, want pr 2 on the new workflow", c2)
	}
	c2.ret <- nil
	waitStatus(t, q, 2, statusDone)
	select {
	case c := <-f.calls:
		t.Errorf("old workflow ran %s again", c.pr)
	default:
	}
}

// A full Save and apply while a poll waits on gh and a review waits on its agent.
func TestSaveAndApplyWhilePollAndReviewRun(t *testing.T) {
	path := writeConfig(t, `{"repositories": ["o/r"]}`)
	h, _ := holderFor(t, path)
	release := make(chan struct{})
	b := &blockingFetch{entered: make(chan string, 10), block: map[string]chan struct{}{"o/r": release}}
	p := newPoller(h.get().Repositories, b.fetch, noLogin)
	q, f := queueFakes(t, 1)
	base := q.wf
	agents := make(chan string, 10)
	h.onApply = func(c config) {
		p.setRepos(c.Repositories)
		wf := base // the test's fakes, with an agent step that reports which prompt it got
		wf.codeReview = func(context.Context, pullRequest, workspace) (stepResult, error) {
			agents <- c.Steps.CodeReview.Prompt
			return stepResult{}, nil
		}
		q.setWorkflow(wf)
	}
	p.onPoll = func(s pollSnapshot) {
		q.sync(append(s.PRs, pr(1))) // keeps the running review listed
		q.prune(s.Fetched, s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.run(ctx)
	nextFetch(t, b.entered) // the poll is in flight
	q.sync([]pullRequest{pr(1)})
	c1 := f.next(t) // the review is in flight

	in := unchanged(h.get())
	in.Repos = []string{"x/y"}
	in.CodeReview = "new prompt"
	if _, err := h.saveAndApply(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if got := h.get().Repositories; !slices.Equal(got, []string{"x/y"}) {
		t.Errorf("holder repos = %v", got)
	}

	close(release)
	if r := nextFetch(t, b.entered); r != "x/y" {
		t.Errorf("woken poll fetched %s, want x/y", r)
	}
	c1.ret <- nil
	waitStatus(t, q, 1, statusDone)
	waitFor(t, "x/y reviewed with the new prompt", func() bool {
		select {
		case got := <-agents:
			if got != "new prompt" {
				t.Errorf("new review got prompt %q", got)
			}
			return true
		default:
			return false
		}
	})
	for _, pr := range p.snapshot().PRs {
		if pr.Repo == "o/r" {
			t.Errorf("removed repo still listed: %+v", pr)
		}
	}
	if !strings.Contains(readFile(t, path), `"x/y"`) {
		t.Error("config not saved")
	}
}

func TestSaveAndApplyConcurrentSavesMatchFile(t *testing.T) {
	path := writeConfig(t, `{"repositories": ["o/r"]}`)
	h, _ := holderFor(t, path)
	p := newPoller(h.get().Repositories, (&blockingFetch{entered: make(chan string, 100)}).fetch, noLogin)
	q, _ := queueFakes(t, 1)
	var applied string // the code review prompt of the last workflow handed to the queue
	h.onApply = func(c config) {
		p.setRepos(c.Repositories)
		time.Sleep(time.Millisecond) // widen the window between save and apply
		q.setWorkflow(q.wf)
		applied = c.Steps.CodeReview.Prompt
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := unchanged(h.get())
			in.Repos = []string{fmt.Sprintf("o/r%d", i)}
			in.CodeReview = fmt.Sprintf("prompt %d", i)
			if _, err := h.saveAndApply(context.Background(), in); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	onDisk, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.get(); !slices.Equal(got.Repositories, onDisk.Repositories) || got.Steps.CodeReview.Prompt != onDisk.Steps.CodeReview.Prompt {
		t.Errorf("holder %v %q, file %v %q", got.Repositories, got.Steps.CodeReview.Prompt, onDisk.Repositories, onDisk.Steps.CodeReview.Prompt)
	}
	p.mu.Lock()
	polled := slices.Clone(p.repos)
	p.mu.Unlock()
	if !slices.Equal(polled, onDisk.Repositories) {
		t.Errorf("poller repos %v, file %v", polled, onDisk.Repositories)
	}
	if applied != onDisk.Steps.CodeReview.Prompt {
		t.Errorf("queue workflow from prompt %q, file %q", applied, onDisk.Steps.CodeReview.Prompt)
	}
}

func TestSaveUpdatesSymlinkedConfigAtTarget(t *testing.T) {
	dotfiles, home := t.TempDir(), t.TempDir()
	target := filepath.Join(dotfiles, "myreview.json")
	if err := os.WriteFile(target, []byte(`{"repositories": ["o/r"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "myreview.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	h, _ := holderFor(t, link)
	in := unchanged(h.get())
	in.Repos = []string{"x/y"}
	if _, err := h.saveAndApply(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link replaced: %v, %v", fi, err)
	}
	if !strings.Contains(readFile(t, target), `"x/y"`) {
		t.Error("target not updated")
	}
	if mode(t, target) != 0600 {
		t.Errorf("target mode %v", mode(t, target))
	}
	assertNoTemps(t, home)
	assertNoTemps(t, dotfiles)
}
