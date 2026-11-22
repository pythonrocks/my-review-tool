package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	maxListRepos    = 1000 // 10 pages of 100
	maxSuggestions  = 20
	repoListTimeout = 60 * time.Second
)

var repoListArgs = []string{"api", "user/repos?affiliation=owner,collaborator,organization_member&per_page=100", "--paginate", "--jq", ".[].full_name"}

// ghLines runs gh with args and calls line for each line of stdout until it returns false; then gh is cancelled.
// Tests replace it.
var ghLines = func(ctx context.Context, line func(string) bool, args ...string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return classifyGhError(err, 0, "")
	}
	sc := bufio.NewScanner(out)
	stopped := false
	for sc.Scan() {
		if !line(sc.Text()) {
			stopped = true
			cancel()
			break
		}
	}
	err = cmd.Wait()
	switch {
	case stopped:
		return nil
	case err != nil:
		code := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		}
		if ctx.Err() == context.DeadlineExceeded {
			err = errors.New("gh timed out")
		}
		return classifyGhError(err, code, stderr.String())
	}
	return sc.Err()
}

// repoList is the user's repositories from gh, fetched once per app session in the background.
type repoList struct {
	mu      sync.Mutex
	loading bool
	done    bool // repos is complete (or capped)
	err     error
	repos   []string
	capped  bool
}

// start begins the fetch unless it is running or has finished; retry restarts a failed one.
func (l *repoList) start(retry bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.loading || l.done || (l.err != nil && !retry) {
		return
	}
	l.loading, l.err = true, nil
	go l.fetch()
}

func (l *repoList) fetch() {
	ctx, cancel := context.WithTimeout(context.Background(), repoListTimeout)
	defer cancel()
	var repos []string
	capped := false
	err := ghLines(ctx, func(s string) bool {
		if s = strings.TrimSpace(s); !validGitHubRepo(s) {
			return true
		}
		if len(repos) == maxListRepos {
			capped = true
			return false
		}
		repos = append(repos, s)
		return true
	}, repoListArgs...)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loading = false
	if err != nil {
		l.err = err
		return
	}
	l.repos, l.capped, l.done = repos, capped, true
}

// repoListView is a snapshot of the list for the suggestions fragment.
type repoListView struct {
	Q       string
	Loading bool
	Err     string
	// NotLoggedIn replaces the shared message, which promises a poll that never reloads this list.
	NotLoggedIn bool
	Matches     []string
	Capped      bool
}

// match returns up to maxSuggestions repos containing q, case-insensitively.
func (l *repoList) match(q string) repoListView {
	l.mu.Lock()
	defer l.mu.Unlock()
	v := repoListView{Q: q, Loading: l.loading, Capped: l.capped}
	if l.err != nil {
		v.Err = l.err.Error()
		v.NotLoggedIn = errors.Is(l.err, globalError(msgGhNotLoggedIn))
	}
	q = strings.ToLower(q)
	for _, r := range l.repos {
		if strings.Contains(strings.ToLower(r), q) {
			v.Matches = append(v.Matches, r)
			if len(v.Matches) == maxSuggestions {
				break
			}
		}
	}
	return v
}

// find returns the list's casing of repo if the finished list has it.
func (l *repoList) find(repo string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.repos {
		if strings.EqualFold(r, repo) {
			return r, true
		}
	}
	return "", false
}
