package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// fakeFetch returns the canned result for each repository; results can be changed between polls.
type fakeFetch struct {
	prs  map[string][]pullRequest
	errs map[string]error
}

func (f *fakeFetch) fetch(_ context.Context, repo string) ([]pullRequest, error) {
	if err := f.errs[repo]; err != nil {
		return nil, err
	}
	return f.prs[repo], nil
}

func noLogin(context.Context) (string, error) { return "", errors.New("no login in tests") }

func day(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }

func ids(prs []pullRequest) string {
	s := ""
	for _, pr := range prs {
		s += fmt.Sprintf("%s#%d ", pr.Repo, pr.Number)
	}
	return s
}

func TestPollOrdering(t *testing.T) {
	f := &fakeFetch{prs: map[string][]pullRequest{
		"a/b": {
			{Repo: "a/b", Number: 1, Draft: true, Created: day(1)},
			{Repo: "a/b", Number: 2, Created: day(5)},
			{Repo: "a/b", Number: 3, Created: day(3)},
		},
		"c/d": {
			{Repo: "c/d", Number: 1, Created: day(3)},
			{Repo: "c/d", Number: 9, Draft: true, Created: day(2)},
			{Repo: "c/d", Number: 4, Created: day(4)},
		},
	}}
	p := newPoller([]string{"c/d", "a/b"}, f.fetch, noLogin)
	p.poll(context.Background())
	// ready by age (tie on day 3 broken by repo), then drafts by age even though they are older
	want := "a/b#3 c/d#1 c/d#4 a/b#2 a/b#1 c/d#9 "
	if got := ids(p.snapshot().PRs); got != want {
		t.Errorf("order = %q, want %q", got, want)
	}
}

func TestPollRemovesMissingPR(t *testing.T) {
	f := &fakeFetch{prs: map[string][]pullRequest{"a/b": {{Repo: "a/b", Number: 1}, {Repo: "a/b", Number: 2}}}}
	p := newPoller([]string{"a/b"}, f.fetch, noLogin)
	p.poll(context.Background())
	f.prs["a/b"] = []pullRequest{{Repo: "a/b", Number: 2}}
	p.poll(context.Background())
	if got := ids(p.snapshot().PRs); got != "a/b#2 " {
		t.Errorf("PRs = %q, want only a/b#2", got)
	}
}

func TestPollRepoErrorKeepsLastGoodList(t *testing.T) {
	f := &fakeFetch{prs: map[string][]pullRequest{"a/b": {{Repo: "a/b", Number: 1}}, "c/d": {{Repo: "c/d", Number: 1}}}}
	p := newPoller([]string{"a/b", "c/d"}, f.fetch, noLogin)
	p.poll(context.Background())
	f.errs = map[string]error{"a/b": errors.New("boom")}
	f.prs["c/d"] = []pullRequest{{Repo: "c/d", Number: 2}}
	p.poll(context.Background())
	s := p.snapshot()
	if got := ids(s.PRs); got != "a/b#1 c/d#2 " {
		t.Errorf("PRs = %q, want a/b#1 kept and c/d updated", got)
	}
	if len(s.Errors) != 1 || s.Errors["a/b"] != "boom" || s.Global != "" {
		t.Errorf("errors = %v, global %q; want only a/b: boom", s.Errors, s.Global)
	}
	f.errs = nil
	p.poll(context.Background())
	if s := p.snapshot(); len(s.Errors) != 0 {
		t.Errorf("errors after recovery = %v, want none", s.Errors)
	}
}

func TestPollGlobalErrorCleared(t *testing.T) {
	f := &fakeFetch{errs: map[string]error{"a/b": globalError(msgGhNotLoggedIn)}}
	p := newPoller([]string{"a/b"}, f.fetch, noLogin)
	p.poll(context.Background())
	s := p.snapshot()
	if s.Global != msgGhNotLoggedIn || len(s.Errors) != 0 || !s.FirstPollDone || !s.LastSuccessfulPoll.IsZero() {
		t.Errorf("after auth failure: %+v", s)
	}
	f.errs = nil
	p.poll(context.Background())
	if s := p.snapshot(); s.Global != "" || s.LastSuccessfulPoll.IsZero() {
		t.Errorf("after clean poll: %+v, want global cleared and a successful poll", s)
	}
}

func TestPollEmptyAndZeroRepos(t *testing.T) {
	for _, repos := range [][]string{{"a/b"}, nil} {
		p := newPoller(repos, (&fakeFetch{prs: map[string][]pullRequest{"a/b": {}}}).fetch, noLogin)
		if s := p.snapshot(); s.FirstPollDone || !s.LastSuccessfulPoll.IsZero() {
			t.Errorf("%v before poll: %+v", repos, s)
		}
		p.poll(context.Background())
		if s := p.snapshot(); len(s.PRs) != 0 || len(s.Errors) != 0 || !s.FirstPollDone || s.LastSuccessfulPoll.IsZero() {
			t.Errorf("%v after poll: %+v, want no PRs, done, successful", repos, s)
		}
	}
}

func TestPollLastSuccessfulOnlyWhenErrorFree(t *testing.T) {
	f := &fakeFetch{errs: map[string]error{"c/d": errors.New("boom")}}
	p := newPoller([]string{"a/b", "c/d"}, f.fetch, noLogin)
	p.poll(context.Background())
	if s := p.snapshot(); !s.FirstPollDone || !s.LastSuccessfulPoll.IsZero() {
		t.Fatalf("after failing poll: %+v, want done and no successful poll", s)
	}
	f.errs = nil
	p.poll(context.Background())
	first := p.snapshot().LastSuccessfulPoll
	if first.IsZero() || first.Location() != time.UTC {
		t.Fatalf("last successful poll = %v, want set in UTC", first)
	}
	f.errs = map[string]error{"c/d": errors.New("boom")}
	p.poll(context.Background())
	if got := p.snapshot().LastSuccessfulPoll; !got.Equal(first) {
		t.Errorf("failing poll moved last successful poll from %v to %v", first, got)
	}
}

func TestPollLoginFetchedUntilFirstSuccess(t *testing.T) {
	calls, fail := 0, true
	login := func(context.Context) (string, error) {
		calls++
		if fail {
			return "", errors.New("gh not logged in")
		}
		return "alice", nil
	}
	f := &fakeFetch{prs: map[string][]pullRequest{"a/b": {{Repo: "a/b", Number: 1}}}}
	p := newPoller([]string{"a/b"}, f.fetch, login)
	p.poll(context.Background())
	s := p.snapshot()
	if s.Login != "" || ids(s.PRs) != "a/b#1 " || len(s.Errors) != 0 || s.Global != "" || s.LastSuccessfulPoll.IsZero() {
		t.Fatalf("after login failure: %+v, want empty login and an unaffected successful poll", s)
	}
	fail = false
	p.poll(context.Background())
	p.poll(context.Background())
	if got := p.snapshot().Login; got != "alice" || calls != 2 {
		t.Errorf("login = %q after %d calls, want alice after 2 (not fetched again once known)", got, calls)
	}
}

func TestPollSnapshotCarriesRequested(t *testing.T) {
	f := &fakeFetch{prs: map[string][]pullRequest{"a/b": {{Repo: "a/b", Number: 1, Requested: true}, {Repo: "a/b", Number: 2, Created: day(1)}}}}
	p := newPoller([]string{"a/b"}, f.fetch, noLogin)
	p.poll(context.Background())
	for _, pr := range p.snapshot().PRs {
		if pr.Requested != (pr.Number == 1) {
			t.Errorf("#%d Requested = %v", pr.Number, pr.Requested)
		}
	}
}

func TestRunCallsOnPollKeepingFailedRepo(t *testing.T) {
	f := &fakeFetch{prs: map[string][]pullRequest{"a/b": {{Repo: "a/b", Number: 1}}, "c/d": {{Repo: "c/d", Number: 1}}}}
	p := newPoller([]string{"a/b", "c/d"}, f.fetch, noLogin)
	p.poll(context.Background())
	f.errs = map[string]error{"a/b": errors.New("boom")}
	f.prs["c/d"] = []pullRequest{{Repo: "c/d", Number: 2}}
	got := make(chan []pullRequest, 1)
	p.onPoll = func(s pollSnapshot) { got <- s.PRs }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.run(ctx)
	select {
	case prs := <-got:
		if s := ids(prs); s != "a/b#1 c/d#2 " {
			t.Errorf("onPoll got %q, want a/b#1 kept and c/d updated", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("onPoll not called after the first poll")
	}
}

func TestMarkApproved(t *testing.T) {
	pr := pullRequest{Repo: "a/b", Number: 3, HeadSHA: "h2", Approvals: []approval{{Login: "alice"}, {Login: "ME", Outdated: true}, {Login: "zed"}}}
	f := &fakeFetch{prs: map[string][]pullRequest{"a/b": {pr, {Repo: "a/b", Number: 4}}}}
	p := newPoller([]string{"a/b"}, f.fetch, noLogin)
	p.poll(context.Background())
	approvals := func() string { return fmt.Sprint(p.snapshot().PRs[0].Approvals) }
	before := p.snapshot().PRs[0].Approvals

	p.markApproved("a/b", 3, "me", "h2") // replaces ME, case-insensitive, kept sorted
	if got := approvals(); got != "[{alice false} {me false} {zed false}]" {
		t.Errorf("replace: %s", got)
	}
	if fmt.Sprint(before) != "[{alice false} {ME true} {zed false}]" || fmt.Sprint(f.prs["a/b"][0].Approvals) != fmt.Sprint(before) {
		t.Errorf("an earlier snapshot or the fetch result changed: %v, %v", before, f.prs["a/b"][0].Approvals)
	}
	p.markApproved("a/b", 3, "Me", "h1") // older commit
	if got := approvals(); got != "[{alice false} {Me true} {zed false}]" {
		t.Errorf("outdated: %s", got)
	}
	p.markApproved("a/b", 4, "me", "") // add; the commit is the (empty) head
	if got := fmt.Sprint(p.snapshot().PRs[1].Approvals); got != "[{me false}]" {
		t.Errorf("add: %s", got)
	}
	p.markApproved("a/b", 3, "", "h2")
	p.markApproved("a/b", 9, "me", "h2")
	p.markApproved("x/y", 3, "me", "h2")
	if got := approvals(); got != "[{alice false} {Me true} {zed false}]" || len(p.snapshot().PRs) != 2 {
		t.Errorf("empty login or unknown PR changed state: %s", got)
	}
	p.poll(context.Background()) // the next poll is the source of truth
	if got := approvals(); got != fmt.Sprint(before) {
		t.Errorf("after poll: %s", got)
	}
}
