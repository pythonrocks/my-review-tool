package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

const ghTimeout = 30 * time.Second

const (
	msgGhNotFound    = `GitHub CLI (gh) not found. Install it from https://cli.github.com and run "gh auth login".`
	msgGhNotLoggedIn = `GitHub CLI is not logged in. Run "gh auth login" (or set GH_TOKEN); the next poll picks it up.`
)

type pullRequest struct {
	Repo, Title, URL, Author, HeadSHA  string
	Labels                             []string // label names
	Number                             int
	Draft, Requested                   bool
	Created                            time.Time
	Additions, Deletions, ChangedFiles int
	HasSize                            bool       // Additions and Deletions came from GitHub; false means show no size
	Reviews                            []approval // one per user, sorted by login, case-insensitive
}

// approval is a user's review status of a pull request: State is APPROVED, CHANGES_REQUESTED or COMMENTED,
// and Outdated means the deciding review is of an older commit than the head.
type approval struct {
	Login    string
	Outdated bool
	State    string
}

// review is one submitted pull request review as gh reports it.
type review struct{ Login, State, Commit string }

// globalError is a problem with gh itself (missing, not logged in) rather than with one repository.
type globalError string

func (e globalError) Error() string { return string(e) }

// parsePRs decodes `gh pr list --json number,title,url,author,createdAt,isDraft,headRefOid,labels,reviews,additions,deletions,changedFiles` output.
// gh reports bot authors with an "app/" prefix, e.g. "app/dependabot", not "dependabot[bot]".
func parsePRs(repo string, data []byte) ([]pullRequest, error) {
	var raw []struct {
		Number                             int
		Title                              string
		URL                                string
		Author                             struct{ Login string }
		CreatedAt                          time.Time
		IsDraft                            bool
		HeadRefOid                         string
		Labels                             []struct{ Name string }
		Additions, Deletions, ChangedFiles *int
		Reviews                            []struct {
			Author struct{ Login string }
			State  string
			Commit struct{ Oid string }
		}
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse gh output: %w", err)
	}
	prs := make([]pullRequest, 0, len(raw))
	for _, r := range raw {
		author := r.Author.Login
		if author == "" {
			author = "ghost" // deleted user
		}
		var labels []string
		for _, l := range r.Labels {
			labels = append(labels, l.Name)
		}
		var reviews []review
		for _, rv := range r.Reviews {
			reviews = append(reviews, review{Login: rv.Author.Login, State: rv.State, Commit: rv.Commit.Oid})
		}
		prs = append(prs, pullRequest{Repo: repo, Title: r.Title, URL: r.URL, Author: author, Labels: labels, Number: r.Number, Draft: r.IsDraft, Created: r.CreatedAt, HeadSHA: r.HeadRefOid,
			Reviews: reviewsOf(reviews, r.HeadRefOid)})
		if p := &prs[len(prs)-1]; r.Additions != nil && r.Deletions != nil {
			p.Additions, p.Deletions, p.HasSize = *r.Additions, *r.Deletions, true
			if r.ChangedFiles != nil {
				p.ChangedFiles = *r.ChangedFiles
			}
		}
	}
	return prs, nil
}

// reviewsOf returns each user's review status from reviews (oldest first) of a PR at head, sorted by login.
// Like approvedAt, a user's last review that is not COMMENTED or PENDING decides (DISMISSED gives no entry);
// a user without one gets their last COMMENTED review.
func reviewsOf(reviews []review, head string) []approval {
	decisive, commented := map[string]review{}, map[string]review{}
	for _, r := range reviews {
		if r.Login == "" {
			r.Login = "ghost" // deleted user
		}
		switch r.State {
		case "PENDING":
		case "COMMENTED":
			commented[strings.ToLower(r.Login)] = r
		default:
			decisive[strings.ToLower(r.Login)] = r
		}
	}
	for k, r := range commented {
		if _, ok := decisive[k]; !ok {
			decisive[k] = r
		}
	}
	var out []approval
	for _, r := range decisive {
		if r.State != "DISMISSED" {
			out = append(out, approval{Login: r.Login, Outdated: head != "" && r.Commit != head, State: r.State})
		}
	}
	slices.SortFunc(out, func(a, b approval) int { return strings.Compare(strings.ToLower(a.Login), strings.ToLower(b.Login)) })
	return out
}

// classifyGhError maps a failed gh run to a globalError or a per-repository error.
func classifyGhError(err error, exitCode int, stderr string) error {
	if errors.Is(err, exec.ErrNotFound) {
		return globalError(msgGhNotFound)
	}
	if exitCode == 4 {
		return globalError(msgGhNotLoggedIn)
	}
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = err.Error()
	}
	return errors.New(cutRunes(msg, maxReason))
}

// parseNumbers decodes `gh ... --json number` output into a set of PR numbers.
func parseNumbers(data []byte) (map[int]bool, error) {
	var raw []struct{ Number int }
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse gh output: %w", err)
	}
	set := make(map[int]bool, len(raw))
	for _, r := range raw {
		set[r.Number] = true
	}
	return set, nil
}

// markRequested sets Requested on every PR whose number is in requested.
func markRequested(prs []pullRequest, requested map[int]bool) {
	for i := range prs {
		prs[i].Requested = requested[prs[i].Number]
	}
}

// runGh runs gh with args under ghTimeout and returns stdout, or a classified error.
func runGh(ctx context.Context, args ...string) ([]byte, error) { return runGhInput(ctx, nil, args...) }

// runGhInput is runGh with input on gh's stdin.
func runGhInput(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		code := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		}
		if ctx.Err() == context.DeadlineExceeded {
			err = errors.New("gh timed out") // callers may set a shorter deadline than ghTimeout
		}
		return nil, classifyGhError(err, code, stderr.String())
	}
	return out, nil
}

// prListArgs are the gh arguments that list the open pull requests of repo for parsePRs.
func prListArgs(repo string) []string {
	// shortcut: at most 10000 open PRs per repository are listed, raise the limit if a watched repository has more.
	// shortcut: gh does not page nested reviews, so only the oldest 100 reviews of a PR count toward Reviews;
	// switch to a paged `gh api graphql` query if a watched PR gets more. latestReviews is not used: gh omits its commit.
	return []string{"pr", "list", "--repo", "github.com/" + repo, "--state", "open",
		"--limit", "10000", "--json", "number,title,url,author,createdAt,isDraft,headRefOid,labels,reviews,additions,deletions,changedFiles"}
}

// ghFetch lists the open pull requests of repo (owner/name) with the gh CLI and flags those requesting the user's review.
func ghFetch(ctx context.Context, repo string) ([]pullRequest, error) {
	out, err := runGh(ctx, prListArgs(repo)...)
	if err != nil {
		return nil, err
	}
	prs, err := parsePRs(repo, out)
	if err != nil {
		return nil, classifyGhError(err, 0, "")
	}
	// GitHub resolves direct and team review requests for review-requested:@me.
	// shortcut: GitHub search returns at most 1000 results per query and its index can lag by about a minute,
	// switch to reviewRequests plus team membership if a reviewer ever has more requests or needs instant updates.
	out, err = runGh(ctx, "pr", "list", "--repo", "github.com/"+repo, "--state", "open",
		"--search", "review-requested:@me", "--limit", "10000", "--json", "number")
	if err != nil {
		return nil, err
	}
	requested, err := parseNumbers(out)
	if err != nil {
		return nil, classifyGhError(err, 0, "")
	}
	markRequested(prs, requested)
	return prs, nil
}

// ghLogin returns the login of the active gh account.
func ghLogin(ctx context.Context) (string, error) {
	out, err := runGh(ctx, "api", "user", "--jq", ".login")
	if err != nil {
		return "", err
	}
	login := strings.TrimSpace(string(out))
	if login == "" {
		return "", errors.New("gh returned an empty login")
	}
	return login, nil
}

// reviewComment is an inline comment of a review, on the new side of the diff.
type reviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

// reviewPayload is the JSON body of GitHub's "create a review" call.
type reviewPayload struct {
	CommitID string          `json:"commit_id"`
	Event    string          `json:"event"`
	Body     string          `json:"body"`
	Comments []reviewComment `json:"comments,omitempty"`
	Replies  []threadReply   `json:"-"` // replies to existing threads; sent through GraphQL, see submitWith
}

// threadReply is a reply to an existing review thread, by its GraphQL node id.
type threadReply struct{ Thread, Body string }

func joinBody(a, b string) string {
	if a == "" {
		return b
	}
	return a + "\n\n" + b
}

// newReview is a review of commit with the drafts in order; GitHub cannot place a line 0 draft inline,
// so it goes into the body headed by its file. An Outdated draft's line is of an older commit and could
// land on the wrong line, so it goes into the body too.
func newReview(commit, event, body string, drafts []pendingDraft) reviewPayload {
	rv := reviewPayload{CommitID: commit, Event: event, Body: body}
	for _, d := range drafts {
		if d.OutdatedFrom != "" {
			at := d.File
			if d.Line > 0 {
				at += fmt.Sprintf(":%d", d.Line)
			}
			rv.Body = joinBody(rv.Body, fmt.Sprintf("`%s` (outdated: written on %s)\n\n%s", at, shortSHA(d.OutdatedFrom), d.Text))
		} else if d.Line > 0 {
			rv.Comments = append(rv.Comments, reviewComment{Path: d.File, Line: d.Line, Side: "RIGHT", Body: d.Text})
		} else {
			rv.Body = joinBody(rv.Body, fmt.Sprintf("**`%s`**\n\n%s", d.File, d.Text))
		}
	}
	return rv
}

// inBody is rv with every inline comment moved into the body, headed by file:line.
func (rv reviewPayload) inBody() reviewPayload {
	for _, c := range rv.Comments {
		rv.Body = joinBody(rv.Body, fmt.Sprintf("`%s:%d`\n\n%s", c.Path, c.Line, c.Body))
	}
	rv.Comments = nil
	return rv
}

// reviewArgs are the gh arguments that create a review from the JSON on stdin and print jq of the answer.
func reviewArgs(repo string, number int, jq string) []string {
	return []string{"api", "--method", "POST", fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, number), "--input", "-", "--jq", jq}
}

const (
	addReplyQuery = `mutation($review:ID!,$thread:ID!,$body:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewId:$review,pullRequestReviewThreadId:$thread,body:$body}){comment{id}}}`
	submitQuery   = `mutation($review:ID!,$event:PullRequestReviewEvent!,$body:String){submitPullRequestReview(input:{pullRequestReviewId:$review,event:$event,body:$body}){pullRequestReview{url}}}`
)

// submitWith returns a submit func that runs gh through run. GitHub refuses a whole review with HTTP 422
// when one comment cannot be placed (a line outside the diff); a 422 posts nothing, so it retries once
// with every comment in the body.
// REST cannot reply to a thread, so a review with Replies is created PENDING (no event, no body), gets
// its replies through GraphQL and is then submitted; if that fails, the pending review is deleted.
func submitWith(run func(ctx context.Context, input []byte, args ...string) ([]byte, error)) func(context.Context, string, int, reviewPayload) (string, error) {
	return func(ctx context.Context, repo string, number int, rv reviewPayload) (string, error) {
		send := func(rv reviewPayload) ([]byte, error) {
			var v any = rv
			jq := ".html_url"
			if len(rv.Replies) > 0 {
				v = struct {
					CommitID string          `json:"commit_id"`
					Comments []reviewComment `json:"comments,omitempty"`
				}{rv.CommitID, rv.Comments}
				jq = ".id, .node_id"
			}
			in, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			return run(ctx, in, reviewArgs(repo, number, jq)...)
		}
		out, err := send(rv)
		if err != nil && len(rv.Comments) > 0 && strings.Contains(err.Error(), "(HTTP 422)") {
			rv = rv.inBody()
			out, err = send(rv)
		}
		if err != nil {
			return "", err
		}
		if len(rv.Replies) > 0 {
			ids := strings.Fields(string(out))
			if len(ids) != 2 {
				return "", fmt.Errorf("gh returned no pending review id: %q", cutRunes(string(out), 100))
			}
			if out, err = finishPending(ctx, run, ids[1], rv); err != nil {
				// best effort; a pending review is visible only to its author. Not ctx: it may have timed out.
				// a leftover pending review makes the next submit with replies fail with 422
				if _, derr := run(context.WithoutCancel(ctx), nil, "api", "-X", "DELETE", fmt.Sprintf("repos/%s/pulls/%d/reviews/%s", repo, number, ids[0])); derr != nil {
					fmt.Fprintln(os.Stderr, "deleting pending review", ids[0]+":", derr)
				}
				return "", err
			}
		}
		u := strings.TrimSpace(string(out))
		if u == "" {
			return "", errors.New("gh returned no review URL")
		}
		return u, nil
	}
}

// finishPending adds rv's replies to the pending review with node id review, submits it and returns the jq'd URL.
func finishPending(ctx context.Context, run func(ctx context.Context, input []byte, args ...string) ([]byte, error), review string, rv reviewPayload) ([]byte, error) {
	for _, r := range rv.Replies {
		if _, err := run(ctx, nil, "api", "graphql", "-f", "query="+addReplyQuery,
			"-f", "review="+review, "-f", "thread="+r.Thread, "-f", "body="+r.Body); err != nil {
			return nil, err
		}
	}
	return run(ctx, nil, "api", "graphql", "-f", "query="+submitQuery,
		"-f", "review="+review, "-f", "event="+rv.Event, "-f", "body="+rv.Body,
		"--jq", ".data.submitPullRequestReview.pullRequestReview.url")
}

// ghSubmitReview creates one GitHub review of a pull request with the gh CLI and returns its URL.
var ghSubmitReview = submitWith(runGhInput)

// reviewsArgs returns the gh arguments that list a pull request's reviews as login, state, commit_id TSV lines.
func reviewsArgs(repo string, number int) []string {
	return []string{"api", "--paginate", fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, number),
		"--jq", `.[] | [.user.login, .state, .commit_id] | @tsv`}
}

// ghApproved reports whether login's latest review of a pull request approves commit sha.
func ghApproved(ctx context.Context, repo string, number int, login, sha string) (bool, error) {
	if login == "" {
		return false, errors.New("gh login not known yet")
	}
	// The login stays out of the jq program; approvedAt filters in Go.
	out, err := runGh(ctx, reviewsArgs(repo, number)...)
	if err != nil {
		return false, err
	}
	return approvedAt(string(out), login, sha), nil
}

// approvedAt reports whether, in tsv lines of login, state and commit id (oldest first), login's last
// review that is not COMMENTED or PENDING is APPROVED at sha.
func approvedAt(tsv, login, sha string) bool {
	approved := false
	for _, line := range strings.Split(tsv, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 || !strings.EqualFold(f[0], login) || f[1] == "COMMENTED" || f[1] == "PENDING" {
			continue
		}
		approved = f[1] == "APPROVED" && f[2] == sha
	}
	return approved
}

// threadList is a pull request's review threads and the head commit their lines refer to.
type threadList struct {
	Head    string
	Threads []reviewThread
}

// reviewThread is a GitHub review thread; Line is 0 for outdated and file threads, More counts comments not fetched.
type reviewThread struct {
	ID, Path, Side                                         string
	Line                                                   int
	Resolved, Outdated, CanReply, CanResolve, CanUnresolve bool
	Comments                                               []threadComment
	More                                                   int
}

// threadComment is a comment of a thread; Body is raw, untrusted markdown.
type threadComment struct {
	Author, Body, URL string
	Created           time.Time
}

// shortcut: only the first 100 comments of a thread are fetched (More counts the rest); page comments too if threads get longer.
const threadsQuery = `query($owner:String!,$name:String!,$number:Int!,$endCursor:String){repository(owner:$owner,name:$name){pullRequest(number:$number){headRefOid ` +
	`reviewThreads(first:100,after:$endCursor){pageInfo{hasNextPage endCursor} nodes{id isResolved isOutdated path line diffSide subjectType viewerCanReply viewerCanResolve viewerCanUnresolve ` +
	`comments(first:100){totalCount nodes{author{login} body createdAt url}}}}}}}`

// threadsWith returns a func that fetches the review threads of a pull request of repo (owner/name) through run.
func threadsWith(run func(ctx context.Context, args ...string) ([]byte, error)) func(context.Context, string, int) (threadList, error) {
	return func(ctx context.Context, repo string, number int) (threadList, error) {
		owner, name, _ := strings.Cut(repo, "/")
		out, err := run(ctx, "api", "graphql", "--paginate", "-f", "query="+threadsQuery,
			"-f", "owner="+owner, "-f", "name="+name, "-F", fmt.Sprintf("number=%d", number))
		if err != nil {
			return threadList{}, err
		}
		tl, err := parseThreads(out)
		if err != nil {
			return threadList{}, classifyGhError(err, 0, "")
		}
		return tl, nil
	}
}

// ghThreads fetches the review threads of a pull request with the gh CLI.
var ghThreads = threadsWith(runGh)

// parseThreads decodes the output of the threadsQuery: with --paginate, gh prints one JSON document per page.
func parseThreads(data []byte) (threadList, error) {
	var tl threadList
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var page struct {
			Data struct {
				Repository struct {
					PullRequest struct {
						HeadRefOid    string
						ReviewThreads struct {
							Nodes []struct {
								ID                                                   string
								IsResolved, IsOutdated                               bool
								Path                                                 string
								Line                                                 int // null for outdated and file threads
								DiffSide                                             string
								ViewerCanReply, ViewerCanResolve, ViewerCanUnresolve bool
								Comments                                             struct {
									TotalCount int
									Nodes      []struct {
										Author    struct{ Login string }
										Body      string
										CreatedAt time.Time
										URL       string
									}
								}
							}
						}
					}
				}
			}
		}
		if err := dec.Decode(&page); err == io.EOF {
			return tl, nil
		} else if err != nil {
			return threadList{}, fmt.Errorf("parse gh output: %w", err)
		}
		pr := page.Data.Repository.PullRequest
		tl.Head = pr.HeadRefOid
		for _, n := range pr.ReviewThreads.Nodes {
			t := reviewThread{ID: n.ID, Path: n.Path, Side: n.DiffSide, Line: n.Line, Resolved: n.IsResolved, Outdated: n.IsOutdated,
				CanReply: n.ViewerCanReply, CanResolve: n.ViewerCanResolve, CanUnresolve: n.ViewerCanUnresolve,
				More: n.Comments.TotalCount - len(n.Comments.Nodes)}
			for _, c := range n.Comments.Nodes {
				author := c.Author.Login
				if author == "" {
					author = "ghost" // deleted user
				}
				t.Comments = append(t.Comments, threadComment{Author: author, Body: c.Body, URL: c.URL, Created: c.CreatedAt})
			}
			tl.Threads = append(tl.Threads, t)
		}
	}
}

// resolveWith returns a func that resolves or unresolves a review thread through run and returns GitHub's resulting state.
func resolveWith(run func(ctx context.Context, args ...string) ([]byte, error)) func(context.Context, string, bool) (bool, error) {
	return func(ctx context.Context, threadID string, resolved bool) (bool, error) {
		op := "unresolveReviewThread"
		if resolved {
			op = "resolveReviewThread"
		}
		out, err := run(ctx, "api", "graphql", "-f", "query=mutation($id:ID!){"+op+"(input:{threadId:$id}){thread{isResolved}}}",
			"-f", "id="+threadID, "--jq", ".data."+op+".thread.isResolved")
		if err != nil {
			return false, err
		}
		switch strings.TrimSpace(string(out)) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return false, fmt.Errorf("gh returned no thread state: %q", cutRunes(string(out), 100))
	}
}

// ghResolveThread resolves (or, with resolved false, unresolves) a review thread with the gh CLI.
var ghResolveThread = resolveWith(runGh)
