package main

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

const pollInterval = 60 * time.Second

type fetchFunc func(ctx context.Context, repo string) ([]pullRequest, error)

type loginFunc func(ctx context.Context) (string, error)

type poller struct {
	repos []string // guarded by mu
	fetch fetchFunc
	login loginFunc
	// onPoll, if set before run, gets the snapshot after each poll. A failed fetch keeps the last good list, so only a successful fetch drops a PR.
	onPoll func(pollSnapshot)
	wake   chan struct{} // setRepos asks run for a poll now; buffered 1

	mu        sync.Mutex
	prs       map[string][]pullRequest // last good list per repository
	errs      map[string]string        // last error per repository
	global    string                   // gh install or auth problem
	firstDone bool
	lastOK    time.Time // end of the last poll with no errors; zero before
	user      string    // active gh login; empty until fetched once
}

type pollSnapshot struct {
	PRs                []pullRequest
	Errors             map[string]string
	Global             string
	FirstPollDone      bool
	LastSuccessfulPoll time.Time
	Login              string
	Fetched            []string // configured repos with a fetched PR list; a repo just added is not in it until fetched
}

func newPoller(repos []string, fetch fetchFunc, login loginFunc) *poller {
	return &poller{repos: repos, wake: make(chan struct{}, 1), fetch: fetch, login: login, prs: map[string][]pullRequest{}, errs: map[string]string{}}
}

// poll fetches every repository once, one after another.
func (p *poller) poll(ctx context.Context) {
	p.mu.Lock()
	known := p.user != ""
	repos := slices.Clone(p.repos)
	p.mu.Unlock()
	if !known {
		// A failure is ignored: the login is only informational, the next poll retries.
		if login, err := p.login(ctx); err == nil && login != "" {
			p.mu.Lock()
			p.user = login
			p.mu.Unlock()
		}
	}
	global, ok := "", true
	for _, repo := range repos {
		prs, err := p.fetch(ctx, repo)
		p.mu.Lock()
		if !slices.Contains(p.repos, repo) { // removed by setRepos during the fetch
			p.mu.Unlock()
			continue
		}
		var g globalError
		switch {
		case errors.As(err, &g):
			ok, global = false, string(g)
		case err != nil:
			ok = false
			p.errs[repo] = err.Error()
		default:
			p.prs[repo] = prs
			delete(p.errs, repo)
		}
		p.mu.Unlock()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.global = global
	p.firstDone = true
	if ok {
		p.lastOK = time.Now().UTC()
	}
}

// run polls right away, then on every tick until ctx is done. Polls never overlap.
func (p *poller) run(ctx context.Context) {
	tick := func() {
		p.poll(ctx)
		if p.onPoll != nil {
			p.onPoll(p.snapshot())
		}
	}
	tick()
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		case <-p.wake:
			tick()
		}
	}
}

// setRepos replaces the repositories, forgets the lists and errors of removed ones and asks run for a poll now.
func (p *poller) setRepos(repos []string) {
	p.mu.Lock()
	p.repos = slices.Clone(repos)
	maps.DeleteFunc(p.errs, func(r string, _ string) bool { return !slices.Contains(repos, r) })
	maps.DeleteFunc(p.prs, func(r string, _ []pullRequest) bool { return !slices.Contains(repos, r) })
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default: // a poll is already pending
	}
}

// snapshot returns sorted copies of the state: ready PRs before drafts, oldest first, then by repository and number.
func (p *poller) snapshot() pollSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	var all []pullRequest
	for _, prs := range p.prs {
		all = append(all, prs...)
	}
	slices.SortFunc(all, func(a, b pullRequest) int {
		if a.Draft != b.Draft {
			if a.Draft {
				return 1
			}
			return -1
		}
		return cmp.Or(a.Created.Compare(b.Created), cmp.Compare(a.Repo, b.Repo), cmp.Compare(a.Number, b.Number))
	})
	return pollSnapshot{PRs: all, Errors: maps.Clone(p.errs), Global: p.global, FirstPollDone: p.firstDone, LastSuccessfulPoll: p.lastOK, Login: p.user,
		Fetched: slices.Sorted(maps.Keys(p.prs))}
}

// markReviewed records login's review of commit on repo#number with state (APPROVED, CHANGES_REQUESTED or
// COMMENTED) until the next poll replaces it; a COMMENTED keeps a standing approval or change request.
// It assigns new slices because snapshot shares the old Reviews ones.
func (p *poller) markReviewed(repo string, number int, login, commit, state string) {
	if login == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	prs := p.prs[repo]
	i := slices.IndexFunc(prs, func(pr pullRequest) bool { return pr.Number == number })
	if i < 0 {
		return
	}
	if state == "COMMENTED" && slices.ContainsFunc(prs[i].Reviews, func(a approval) bool {
		return strings.EqualFold(a.Login, login) && a.State != "COMMENTED"
	}) {
		return
	}
	out := slices.DeleteFunc(slices.Clone(prs[i].Reviews), func(a approval) bool { return strings.EqualFold(a.Login, login) })
	out = append(out, approval{Login: login, Outdated: commit != prs[i].HeadSHA, State: state})
	slices.SortFunc(out, func(a, b approval) int { return strings.Compare(strings.ToLower(a.Login), strings.ToLower(b.Login)) })
	prs = slices.Clone(prs) // the list may be the fetch result, so it is not changed in place either
	prs[i].Reviews = out
	p.prs[repo] = prs
}
