package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const version = "0.1.0"

//go:embed index.html
var indexHTML string

//go:embed details.html
var detailsHTML string

//go:embed static/myreview.css
var stylesheet []byte

//go:embed static/htmx.min.js
var htmxJS []byte

//go:embed static/dashboard.js
var dashboardJS []byte

//go:embed static/details.js
var detailsJS []byte

//go:embed static/links.js
var linksJS []byte

// csp is sent on every response; pages load only same-origin scripts and styles, nothing inline.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

var funcs = template.FuncMap{
	"age":         func(created time.Time) string { return formatAge(time.Since(created)) },
	"prKey":       prKey,
	"status":      formatStatus,
	"statusClass": statusClass,
	"lower":       strings.ToLower,
	"recClass":    recClass,
	"reviewDots":  reviewDots,
	"recLabel":    func(rec string) string { return strings.ToUpper(rec[:1]) + rec[1:] },
	"pathEscape":  pathEscape,
	"approvals":   approvalsView,
	"join":        strings.Join,
	"has":         slices.Contains[[]string, string],
	"short":       shortSHA,
	"stateClass":  func(state string) string { return "state-" + strings.ToLower(strings.Fields(state)[0]) }, // Draft edited is state-draft
	"repoParts":   func(r string) []string { o, n, _ := strings.Cut(r, "/"); return []string{o, n} },
	"row":         newRow,
	"base":        path.Base,
	"comment":     func(id string) bool { return commentNo(id) > 0 },
	"runMetrics":  newRunView,
}

// rowData is one dashboard row, rendered by the "row" template for the list and for an htmx Re-run.
type rowData struct {
	PR           pullRequest
	St           jobStatus // zero for a PR the queue does not know yet
	Login, Query string
}

func newRow(pr pullRequest, d indexData) rowData {
	return rowData{PR: pr, St: d.Review[prKey(pr)], Login: d.Poll.Login, Query: d.Query}
}

var (
	indexTmpl   = template.Must(template.New("index").Funcs(funcs).Parse(indexHTML))
	detailsTmpl = template.Must(template.New("details").Funcs(funcs).Parse(detailsHTML))
)

// formatStatus renders a review status as the dashboard shows it; empty for a PR the queue does not know.
func formatStatus(s jobStatus) string {
	text := s.Status
	switch {
	case s.Status == statusDone:
		text = "done: " + formatCounts(s.Findings)
	case s.Status == statusSkipped || s.Status == statusOnDemand:
		text = s.Status + ": " + s.Reason
	case s.FailedStep != "":
		text = fmt.Sprintf("%s: %s: %s", s.Status, s.FailedStep, s.Reason)
	}
	if s.Outdated {
		return "outdated: " + text
	}
	return text
}

// statusClass is the CSS class of a review status badge; outdated keeps the class of its underlying status.
func statusClass(s jobStatus) string {
	switch s.Status {
	case "":
		return "status-not-queued"
	case statusSkipped, statusOnDemand:
		return "plain"
	}
	return "status-" + s.Status
}

// recClass is the CSS class of a recommendation.
func recClass(rec string) string {
	if rec == recHuman {
		return "rec-human"
	}
	return "rec-auto"
}

// pathEscape escapes each segment of a slash-separated file path for a URL path.
func pathEscape(file string) string {
	segs := strings.Split(file, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

var severityRank = map[string]int{"Critical": 0, "High": 1, "Medium": 2, "Low": 3}

// report is all findings of a result, code and security, sorted by severity, file, line, with what links need.
type report struct {
	Repo, SHA string
	Number    int
	Outdated  bool
	Findings  []reportFinding
}

// reportFinding is a finding with its position in jobStatus.Findings, which its row id refers to.
type reportFinding struct {
	taggedFinding
	Index int
}

func newReport(repo string, number int, st jobStatus) report {
	r := report{Repo: repo, SHA: st.HeadSHA, Number: number, Outdated: st.Outdated}
	for i, f := range st.Findings {
		r.Findings = append(r.Findings, reportFinding{f, i})
	}
	slices.SortStableFunc(r.Findings, func(a, b reportFinding) int { return compareFindings(a.taggedFinding, b.taggedFinding) })
	return r
}

// checkItem is one row of the "What to check" list: a finding (Finding.Severity set) or a point of interest.
type checkItem struct {
	ID      string // f<index in Review.Findings> or p<index in Review.Points>
	No      int
	File    string
	Line    int
	Text    string
	Title   string // the stored title, else shortTitle(Text)
	Comment string // the stored suggested comment, else Text
	Report  report
	Finding reportFinding
	// From the pending review (withPending): the row's checked mark, its draft (the suggested comment
	// when it has none) and the draft's state.
	Checked bool
	Draft   pendingDraft
	State   string
	// The point body's Previous / Next targets: the neighbours' ids, prefixed by Anchor ("pt-" in the list, "d-" in the diff).
	Prev, Next, Anchor string
	Excerpt            bool // the point body loads its code excerpt (only bodies that are shown open, so a page load reads no files)
	// The GitHub threads on the row's line (list view only) and the number of comments in the unresolved ones.
	Threads      []threadView
	OpenComments int
}

// threadView is the "thread" block: a GitHub review thread of PR Repo#Number with your pending reply.
type threadView struct {
	Repo   string
	Number int
	T      reviewThread
	URL    string       // the thread on GitHub: its first comment
	Reply  pendingReply // Thread empty: none
	Text   string       // the reply editor's text: the reply's, or a refused POST's
	Error  string
}

func newThreadView(repo string, number int, t reviewThread, p pendingPR) threadView {
	v := threadView{Repo: repo, Number: number, T: t}
	if len(t.Comments) > 0 {
		v.URL = t.Comments[0].URL
	}
	if i := slices.IndexFunc(p.Replies, func(r pendingReply) bool { return r.Thread == t.ID }); i >= 0 {
		v.Reply = p.Replies[i]
	}
	v.Text = v.Reply.Text
	return v
}

// rowThreads maps the id of each row on a line to the threads on that line: same file, right side, not outdated.
// A thread goes on the first such row in list order only, so no two blocks of one thread show diverging replies.
func rowThreads(threads []reviewThread, items []checkItem) map[string][]reviewThread {
	m := map[string][]reviewThread{}
	placed := map[string]bool{}
	for _, it := range items {
		for _, t := range threads {
			if it.Line > 0 && t.Path == it.File && t.Side == "RIGHT" && t.Line == it.Line && !t.Outdated && !placed[t.ID] {
				m[it.ID] = append(m[it.ID], t)
				placed[t.ID] = true
			}
		}
	}
	return m
}

// withThreads sets the Threads and OpenComments of items, rows of the whole list in list order, from the threads
// of repo#number and the pending review p.
func withThreads(items, list []checkItem, threads []reviewThread, repo string, number int, p pendingPR) {
	m := rowThreads(threads, list)
	for i := range items {
		for _, t := range m[items[i].ID] {
			items[i].Threads = append(items[i].Threads, newThreadView(repo, number, t, p))
			if !t.Resolved {
				items[i].OpenComments += len(t.Comments) + t.More
			}
		}
	}
}

// threadsPlaced reports whether threads go on the rows and lines of st: their lines are of the reviewed commit.
func threadsPlaced(st jobStatus, tl threadList) bool {
	return st.Status == statusDone && tl.Head == st.HeadSHA
}

// replyable reports whether tl has the thread id and you can reply to it.
func replyable(tl threadList, id string) bool {
	return slices.ContainsFunc(tl.Threads, func(t reviewThread) bool { return t.ID == id && t.CanReply })
}

// withPending sets the row's checked mark and draft from p for the result at commit. A row without a
// matching draft gets an unsaved Suggestion; a finding posted by v1 shows as Posted.
func withPending(it checkItem, p pendingPR, commit string) checkItem {
	it.Checked = p.checkedAt(it) >= 0
	it.Draft = pendingDraft{ID: it.ID, File: it.File, Line: it.Line, Commit: commit, SourceText: it.Text, Text: it.Comment}
	if i := p.draftAt(it, commit); i >= 0 {
		it.Draft = p.Drafts[i]
	}
	it.Draft.PostedURL = cmp.Or(it.Draft.PostedURL, it.Finding.PostedURL)
	it.State = draftState(it.Draft)
	return it
}

func findingItem(r report, f reportFinding) checkItem {
	return checkItem{ID: fmt.Sprintf("f%d", f.Index), File: f.File, Line: f.Line, Text: f.Description,
		Title: cmp.Or(f.Title, shortTitle(f.Description)), Comment: cmp.Or(f.SuggestedComment, f.Description), Report: r, Finding: f}
}

func pointItem(r report, i int, p pointOfInterest) checkItem {
	return checkItem{ID: fmt.Sprintf("p%d", i), File: p.File, Line: p.Line, Text: p.Reason,
		Title: cmp.Or(p.Title, shortTitle(p.Reason)), Comment: cmp.Or(p.Comment, p.Reason), Report: r}
}

// fromFinding reports whether decideRisk generated p from one of the findings.
func fromFinding(p pointOfInterest, findings []taggedFinding) bool {
	return slices.ContainsFunc(findings, func(f taggedFinding) bool {
		return f.File == p.File && f.Line == p.Line && p.Reason == fmt.Sprintf("%s %s finding: %s", f.Severity, f.Step, f.Description)
	})
}

// checkItems is the "What to check" list: every finding in report order, then the points of interest
// in decision order without those generated from a finding, numbered from 1, with the pending review p.
func checkItems(r report, st jobStatus, p pendingPR) []checkItem {
	var items []checkItem
	for _, f := range r.Findings {
		items = append(items, findingItem(r, f))
	}
	for i, p := range st.Points {
		if !fromFinding(p, st.Findings) {
			items = append(items, pointItem(r, i, p))
		}
	}
	for i := range items {
		items[i].No = i + 1
		items[i] = withPending(items[i], p, st.HeadSHA)
		items[i].Anchor = "pt-"
		if i > 0 {
			items[i].Prev = items[i-1].ID
		}
		if i+1 < len(items) {
			items[i].Next = items[i+1].ID
		}
	}
	return items
}

// commentItem is the line comment d (a c<n> draft) as a row: no number, no checkbox, its own text.
// It has no suggestion to differ from, so in the review it is a plain "Draft".
func commentItem(r report, d pendingDraft) checkItem {
	state := draftState(d)
	if state == "Draft edited" {
		state = "Draft"
	}
	return checkItem{ID: d.ID, File: d.File, Line: d.Line, Text: d.SourceText, Comment: d.Text, Report: r, Draft: d, State: state}
}

// commentNo is n of a line comment id c<n>, or 0 for any other id.
func commentNo(id string) int {
	if !strings.HasPrefix(id, "c") {
		return 0
	}
	n, err := strconv.Atoi(id[1:])
	if err != nil || n < 1 || strconv.Itoa(n) != id[1:] {
		return 0
	}
	return n
}

// commentItems are the line comments of p made on commit, in id order, Outdated ones included: they show in the
// list only, never on a diff line (an Outdated draft's line is of an older commit).
func commentItems(r report, p pendingPR, commit string) []checkItem {
	var out []checkItem
	for _, d := range p.Drafts {
		if commentNo(d.ID) > 0 && d.Commit == commit {
			out = append(out, commentItem(r, d))
		}
	}
	slices.SortFunc(out, func(a, b checkItem) int { return cmp.Compare(commentNo(a.ID), commentNo(b.ID)) })
	return out
}

// lookupItem resolves a point id (f<i> or p<i>, i a canonical non-negative integer in range) of a done result,
// or a line comment id (c<n>) of a draft in p made on the result's commit.
func lookupItem(r report, st jobStatus, p pendingPR, id string) (checkItem, bool) {
	if commentNo(id) > 0 {
		i := slices.IndexFunc(p.Drafts, func(d pendingDraft) bool { return d.ID == id && d.Commit == st.HeadSHA })
		if i < 0 {
			return checkItem{}, false
		}
		return commentItem(r, p.Drafts[i]), true
	}
	if id == "" {
		return checkItem{}, false
	}
	i, err := strconv.Atoi(id[1:])
	if err != nil || i < 0 || strconv.Itoa(i) != id[1:] {
		return checkItem{}, false
	}
	switch {
	case id[0] == 'f' && i < len(st.Findings):
		return findingItem(r, reportFinding{st.Findings[i], i}), true
	case id[0] == 'p' && i < len(st.Points):
		return pointItem(r, i, st.Points[i]), true
	}
	return checkItem{}, false
}

// newReviewRecord counts a review of pr on commit sending drafts and replies, with st's rows items
// (nil without a done result). A row's step is its finding's, or "risk assessment" for a point of interest.
func newReviewRecord(pr pullRequest, commit, event string, st jobStatus, known bool, pend pendingPR, items []checkItem, drafts []pendingDraft, replies int) reviewRecord {
	r := reviewRecord{Repo: pr.Repo, Number: pr.Number, HeadSHA: commit, Event: event, OpenedAt: pend.OpenedAt, Total: len(items), Replies: replies}
	if known && st.Status == statusDone {
		if st.Metrics != nil {
			r.RunID = st.Metrics.RunID
		}
		r.Risk, r.Recommendation = st.Risk, st.Recommendation
	}
	r.Checked, _ = pendingCounts(items)
	for _, d := range drafts {
		if commentNo(d.ID) > 0 {
			r.Own++
		}
	}
	for _, it := range items {
		step := cmp.Or(it.Finding.Step, "risk assessment")
		i := slices.IndexFunc(r.Steps, func(c stepCount) bool { return c.Step == step })
		if i < 0 {
			i = len(r.Steps)
			r.Steps = append(r.Steps, stepCount{Step: step})
		}
		c := &r.Steps[i]
		if it.Comment != it.Text {
			c.Suggested++
		}
		if it.Draft.InReview && it.Draft.PostedURL == "" { // sent: the same test reviewState uses for drafts
			if it.Draft.Edited {
				c.Modified++
			} else {
				c.Accepted++
			}
		}
	}
	return r
}

// pendingCounts is the number of items with a checked mark and of items whose draft is in the pending review.
// Line comments have no mark, so passing them too only adds them to inReview.
func pendingCounts(items []checkItem) (checked, inReview int) {
	for _, it := range items {
		if it.Checked {
			checked++
		}
		if it.Draft.InReview && it.Draft.PostedURL == "" {
			inReview++
		}
	}
	return checked, inReview
}

// verdictCounts is the decision bar's counts line: findings by severity (non-zero, Critical to Low),
// the listed points of interest, and the security findings.
func verdictCounts(findings []taggedFinding, points int) string {
	plural := func(n int, one, many string) string {
		if n == 1 {
			return one
		}
		return many
	}
	n := map[string]int{}
	sec := 0
	for _, f := range findings {
		n[f.Severity]++
		if f.Step == "security review" {
			sec++
		}
	}
	var parts []string
	for _, sev := range []string{"Critical", "High", "Medium", "Low"} {
		if n[sev] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n[sev], strings.ToLower(sev)))
		}
	}
	counts := "No findings"
	if len(parts) > 0 {
		counts = strings.Join(parts, ", ") + plural(len(findings), " finding", " findings")
	}
	counts += fmt.Sprintf(" · %d %s", points, plural(points, "point of interest", "points of interest"))
	if sec == 0 {
		return counts + " · no security findings"
	}
	return counts + fmt.Sprintf(" · %d %s", sec, plural(sec, "security finding", "security findings"))
}

// verdictReasons is up to 2 rationale lines for the decision bar, without the "Risk …" line that restates the verdict.
func verdictReasons(rationale []string) []string {
	var out []string
	for _, l := range rationale {
		if len(out) < 2 && !strings.HasPrefix(l, "Risk ") {
			out = append(out, l)
		}
	}
	return out
}

// shortSHA returns the first 7 characters of a commit SHA.
func shortSHA(sha string) string { return sha[:min(7, len(sha))] }

// compareFindings orders findings like the reports: severity, file, line.
func compareFindings(a, b taggedFinding) int {
	return cmp.Or(cmp.Compare(severityRank[a.Severity], severityRank[b.Severity]), strings.Compare(a.File, b.File), cmp.Compare(a.Line, b.Line))
}

type detailsData struct {
	PR          pullRequest
	Login       string    // active gh login from the poll; empty until known
	Review      jobStatus // zero when the queue does not know the PR yet
	QuietPeriod time.Duration
	View        string // "list" or "diff"
	Items       []checkItem
	Comments    []checkItem // your line comments of the reviewed commit: not counted as points
	Counts      string
	Reasons     []string
	LongSummary bool   // the summary gets a Show more toggle
	Compact     bool   // the one-line decision bar: done, Low risk, nothing to confirm
	CheckedN    int    // Items with a checked mark
	InReview    int    // Items whose draft is in the pending review
	Rev         int    // the pending review's rev, for the compact Approve form
	OwnPR       bool   // the poll login is the author: no Approve or Request changes
	Submitted   string // the success line after a submitted review
	Back        string // the back link: the dashboard with its last-used filters
	// Approve: the lines an approval must confirm, their hash for the forms,
	// and whether GitHub says you approved the current head (or why it could not tell).
	ConfirmLines  []string
	ApproveExpect string
	Approved      bool
	ApprovedError string
	Diff          diffView // the diff view
	ThreadsError  string   // the GitHub discussions could not be loaded
	ThreadsHead   string   // set when there are discussions but they are of another commit than the reviewed one
	Results       string   // resultID of Review, sent back by every htmx request (stale-page guard)
}

// eventNames are the review events POST …/review accepts, as the success line names them.
var eventNames = map[string]string{"APPROVE": "Approved", "REQUEST_CHANGES": "Changes requested", "COMMENT": "Commented"}

// plural is n with word, plural unless n is 1.
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// submittedLine is the success line of ?submitted=<event>&n=<count>, empty unless both are valid.
func submittedLine(v url.Values) string {
	n, err := strconv.Atoi(v.Get("n"))
	if name := eventNames[v.Get("submitted")]; name != "" && err == nil && n >= 0 {
		return fmt.Sprintf("Review submitted: %s, %s.", name, plural(n, "comment"))
	}
	return ""
}

// dialogData is the submit dialog of GET …/review/summary, and of a refused POST …/review with its Error.
type dialogData struct {
	PR                 pullRequest
	Event, Body, Error string
	Rev, Head, Expect  string // the form's hidden fields
	OwnPR              bool
	Confirm            []string // the lines Approve anyway confirms
	OutdatedSHA        string   // an outdated done result: the reviewed commit the review goes to
	Comments           []dialogComment
	Unchecked, Points  int
	Label              string
}

// dialogComment is one comment to post: file:line, the first line of its text cut to 80 characters,
// and the whole trimmed text shown when the row is opened; Discard marks a reply whose thread is gone or locked.
type dialogComment struct {
	At, Snip, Text string
	Discard        bool
}

func newDialogComment(d pendingDraft) dialogComment {
	at := d.File
	if d.Line > 0 {
		at += fmt.Sprintf(":%d", d.Line)
	}
	text := strings.TrimSpace(d.Text)
	line, _, _ := strings.Cut(text, "\n")
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > 80 {
		line = string(r[:80]) + "…"
	}
	return dialogComment{At: at, Snip: line, Text: text}
}

// submitLabel is the dialog's submit button: the outcome of the event with n comments.
func submitLabel(event string, n int) string {
	switch {
	case n == 0:
		return map[string]string{"APPROVE": "Approve", "REQUEST_CHANGES": "Request changes", "COMMENT": "Comment"}[event]
	case event == "APPROVE":
		return "Approve and post " + plural(n, "comment")
	case event == "REQUEST_CHANGES":
		return "Request changes with " + plural(n, "comment")
	}
	return "Post " + plural(n, "comment")
}

// approveHash is the hex SHA-256 of approval reasons joined with newlines; it ties a form to what it showed.
func approveHash(lines []string) string {
	h := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(h[:])
}

// approvedCheckTimeout bounds the approval check of each details page load, so a slow GitHub cannot hold the page.
const approvedCheckTimeout = 5 * time.Second

// threadsTimeout bounds each fetch of a PR's GitHub discussions.
const threadsTimeout = 10 * time.Second

// maxThreadID is the longest thread id looked up; GitHub's node ids are far shorter.
const maxThreadID = 200

// reviewTimeout bounds POST …/review's gh call; a variable so tests can reach the timeout path.
var reviewTimeout = ghTimeout

// maxCommentLen is GitHub's limit on a comment body, in characters.
const maxCommentLen = 65536

// staleMsg refuses a request from a page that shows an older result than the current one.
const staleMsg = "New review results arrived; reload the review. Text typed since then was not saved."

// errUnchanged aborts a pending update that has nothing to change.
var errUnchanged = errors.New("unchanged")

// maxLineComments is the most line comments (c<n> drafts) a PR's pending review keeps.
const maxLineComments = 200

// rowGroup is a PR's dashboard group: 1 human review required, 2 failed, 3 automated review is sufficient,
// 4 waiting for a review (queued, running, unknown, or outdated on a ready PR), 5 outdated on a draft, 6 on demand, 7 skipped.
func rowGroup(pr pullRequest, st jobStatus, known bool) int {
	switch {
	case !known:
		return 4
	case st.Status == statusSkipped:
		return 7
	case st.Status == statusOnDemand:
		return 6
	case st.Outdated && pr.Draft:
		return 5
	case st.Outdated:
		return 4
	case st.Status == statusDone && st.Recommendation == recHuman:
		return 1
	case st.Status == statusFailed:
		return 2
	case st.Status == statusDone:
		return 3
	}
	return 4
}

// compareRows orders the dashboard: review requested first, then ready before draft, group, risk (group 1 only), oldest first, repository, number.
func compareRows(review map[string]jobStatus) func(a, b pullRequest) int {
	key := func(pr pullRequest) (req, draft, group, risk int) {
		st, known := review[prKey(pr)]
		if !pr.Requested {
			req = 1
		}
		if pr.Draft {
			draft = 1
		}
		if group = rowGroup(pr, st, known); group == 1 {
			risk = severityRank[st.Risk]
		}
		return
	}
	return func(a, b pullRequest) int {
		aq, ad, ag, ar := key(a)
		bq, bd, bg, br := key(b)
		return cmp.Or(cmp.Compare(aq, bq), cmp.Compare(ad, bd), cmp.Compare(ag, bg), cmp.Compare(ar, br), a.Created.Compare(b.Created),
			strings.Compare(a.Repo, b.Repo), cmp.Compare(a.Number, b.Number))
	}
}

// formatCounts renders findings as per-severity counts, or "no findings".
func formatCounts(findings []taggedFinding) string {
	if len(findings) == 0 {
		return "no findings"
	}
	n := map[string]int{}
	for _, f := range findings {
		n[f.Severity]++
	}
	return fmt.Sprintf("%d critical, %d high, %d medium, %d low", n["Critical"], n["High"], n["Medium"], n["Low"])
}

// reviewIndicator is a done review as dashboard circles plus the label that reads them out.
type reviewIndicator struct {
	Dots  []dot
	Label string
}

// dot is one review circle: a CSS class and the count shown inside it.
type dot struct {
	Class string
	N     int
}

// reviewDots renders a done review as one circle per severity, muted when zero.
func reviewDots(s jobStatus) reviewIndicator {
	n := map[string]int{}
	for _, f := range s.Findings {
		n[f.Severity]++
	}
	var ind reviewIndicator
	var parts []string
	for _, sev := range []string{"Critical", "High", "Medium", "Low"} {
		d := dot{"zero", 0}
		if n[sev] > 0 {
			d = dot{"sev-" + strings.ToLower(sev), n[sev]}
		}
		ind.Dots = append(ind.Dots, d)
		parts = append(parts, fmt.Sprintf("%d %s", n[sev], strings.ToLower(sev)))
	}
	text := strings.Join(parts, ", ")
	ind.Label = "Review done: " + text
	if s.Outdated {
		ind.Label = "Outdated: review done: " + text
	}
	return ind
}

// approvalInd is one Status indicator: my ✅, ⛔ or 💬, or 👥 for others' approvals, with its label.
type approvalInd struct {
	Symbol, Label string
	Outdated      bool
}

// approvalSummary is a PR's review status split into my indicator and the others' approvals, me first,
// with Text joining their labels for the details page ("none" when there are none).
type approvalSummary struct {
	Inds []approvalInd
	Text string
}

// reviewStates maps a submitted review event to the review state GitHub records for it.
var reviewStates = map[string]string{"APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED", "COMMENT": "COMMENTED"}

// myStatus is my indicator's symbol and label per review state.
var myStatus = map[string][2]string{"APPROVED": {"✅", "Approved by me"}, "CHANGES_REQUESTED": {"⛔", "Changes requested by me"}, "COMMENTED": {"💬", "Commented by me"}}

// approvalsView splits pr.Reviews by login (case-insensitive); with no login known every approval is others'.
func approvalsView(pr pullRequest, login string) approvalSummary {
	var s approvalSummary
	var current, outdated []string
	for _, a := range pr.Reviews {
		switch {
		case login != "" && strings.EqualFold(a.Login, login):
			if m, ok := myStatus[a.State]; ok {
				label := m[1]
				if a.Outdated {
					label += " on an older commit"
				}
				s.Inds = append(s.Inds, approvalInd{m[0], label, a.Outdated})
			}
		case a.State != "APPROVED":
		case a.Outdated:
			outdated = append(outdated, a.Login)
		default:
			current = append(current, a.Login)
		}
	}
	var label string
	switch {
	case len(current) > 0 && len(outdated) > 0:
		label = "Approved by " + strings.Join(current, ", ") + "; on an older commit by " + strings.Join(outdated, ", ")
	case len(current) > 0:
		label = "Approved by " + strings.Join(current, ", ")
	case len(outdated) > 0:
		label = "Approved on an older commit by " + strings.Join(outdated, ", ")
	}
	if label != "" {
		s.Inds = append(s.Inds, approvalInd{"👥", label, len(current) == 0})
	}
	parts := []string{"none"}
	for i, ind := range s.Inds {
		if i == 0 {
			parts = []string{ind.Label}
		} else {
			parts = append(parts, strings.ToLower(ind.Label[:1])+ind.Label[1:]) // every label starts with an ASCII letter
		}
	}
	s.Text = strings.Join(parts, "; ")
	return s
}

// formatAge renders d rounded down: minutes under 1 h, hours under 24 h, days otherwise.
func formatAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// runView is a review's saved metrics as the details page shows them; totals are computed here.
type runView struct {
	Summary string // cost · time · models · tokens
	Steps   []stepRow
	Footer  string // queue wait, attempt, diff size
}

type stepRow struct {
	Step, Agent, Model, Time, APITime, Turns, In, Out, CacheRead, CacheWrite, Cost, Result string
}

func newRunView(m *reviewMetrics) *runView {
	if m == nil {
		return nil
	}
	const none = "—"
	var v runView
	var cost float64
	var priced, noPrice, unknown int
	var total int64
	var models []string
	for _, s := range m.Steps {
		r := stepRow{Step: s.Step, Agent: s.Agent, Time: formatMs(s.WallMs), Result: stepResultText(s),
			Model: none, APITime: none, Turns: none, In: none, Out: none, CacheRead: none, CacheWrite: none, Cost: "unknown"}
		if u := s.Usage; u != nil {
			var names []string
			for _, mu := range u.Models {
				names = append(names, mu.Model)
				if !slices.Contains(models, mu.Model) {
					models = append(models, mu.Model)
				}
			}
			if len(names) > 0 {
				r.Model = strings.Join(names, ", ")
			}
			if u.APIMs > 0 { // codex reports no API time
				r.APITime = formatMs(u.APIMs)
			}
			r.Turns = strconv.Itoa(u.Turns)
			r.In, r.Out = formatTokens(u.InputTokens), formatTokens(u.OutputTokens)
			r.CacheRead, r.CacheWrite = formatTokens(u.CacheReadTokens), formatTokens(u.CacheWriteTokens)
			total += u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
			if u.CostUSD != nil {
				priced++
				cost += *u.CostUSD
				r.Cost = formatCost(*u.CostUSD)
			} else {
				noPrice++
				r.Cost = "no price"
			}
		} else {
			unknown++
		}
		v.Steps = append(v.Steps, r)
	}
	var c string
	switch {
	case priced > 0 || len(m.Steps) == 0:
		c = formatCost(cost)
		if noPrice > 0 {
			c += " + codex (no price)"
		}
		if unknown > 0 {
			c += " + unknown"
		}
	case unknown > 0:
		c = "unknown" // never $0.00 for a step that did not report
	default:
		c = "no price"
	}
	parts := []string{c, formatMs(m.FinishedAt.Sub(m.StartedAt).Milliseconds())}
	if len(models) > 0 {
		parts = append(parts, strings.Join(models, ", "))
	}
	v.Summary = strings.Join(append(parts, formatTokens(total)+" tokens"), " · ")
	var f []string
	if !m.QueuedAt.IsZero() {
		f = append(f, "Queued "+formatMs(m.StartedAt.Sub(m.QueuedAt).Milliseconds()))
	}
	if m.Attempt > 0 {
		f = append(f, fmt.Sprintf("Attempt %d", m.Attempt))
	}
	v.Footer = strings.Join(append(f, fmt.Sprintf("Diff %d files, %s", m.DiffFiles, formatBytes(m.DiffBytes))), " · ")
	return &v
}

// stepResultText is a step's outcome: its findings count when done, else its failure reason.
func stepResultText(s stepMetrics) string {
	if s.Status == statusDone {
		if s.Items == 1 {
			return "1 finding"
		}
		return fmt.Sprintf("%d findings", s.Items)
	}
	return cmp.Or(s.Error, s.ErrorKind, s.Status)
}

// formatCost renders dollars to cents; a positive amount that rounds to zero reads <$0.01.
func formatCost(usd float64) string {
	if usd > 0 && usd < 0.005 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", usd)
}

// formatMs renders a duration as 850 ms, 12s, 1m 05s or 1h 02m.
func formatMs(ms int64) string {
	s := ms / 1000
	switch {
	case ms < 1000:
		return fmt.Sprintf("%d ms", max(ms, 0))
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm %02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh %02dm", s/3600, s%3600/60)
}

// formatTokens renders a count as 950, 23.7k or 1.2M.
func formatTokens(n int64) string {
	switch {
	case n < 1000:
		return strconv.FormatInt(n, 10)
	case n < 999_950:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e3), ".0") + "k"
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e6), ".0") + "M"
}

func formatBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%d KiB", (n+512)/1024)
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1024*1024))
}

type health struct {
	Status             string     `json:"status"`
	Version            string     `json:"version"`
	Repositories       int        `json:"repositories"`
	PullRequests       int        `json:"pull_requests"`
	LastSuccessfulPoll *time.Time `json:"last_successful_poll"`
}

type indexData struct {
	Repositories []string
	Poll         pollSnapshot
	Filter       dashFilter
	Query        string // Filter.query(): empty when no filter is active
	Orgs, Repos  []string
	Authors      []string
	Rows         []pullRequest        // Poll.PRs that match Filter
	Hidden       int                  // Poll.PRs hidden by Filter
	Chips        []filterChip         // Filter.chips()
	Review       map[string]jobStatus // by prKey; missing for PRs the queue does not know yet
	PollEvery    int                  // seconds between list refreshes: 10 while loading or a visible row is queued or running, else 60
	OOB          bool                 // the "poll" template renders the poller as an htmx out-of-band swap
}

// dashFilter is the dashboard filters of a GET / query; the zero value filters nothing.
type dashFilter struct {
	Org, Repo, Author               string
	Risk, Rec                       []string // sorted, deduplicated, no empty values
	Mine, HideSkipped, HideApproved bool
}

// recTokens maps the rec query values to recommendations.
var recTokens = map[string]string{"human": recHuman, "automated": recAutomated}

// parseFilter reads the filter parameters of an untrusted query: first value for org, repo and author,
// every non-empty risk and rec value, and only "1" turns a boolean on. Values are not checked; unknown ones match nothing.
func parseFilter(v url.Values) dashFilter {
	multi := func(vs []string) []string {
		vs = slices.DeleteFunc(slices.Clone(vs), func(s string) bool { return s == "" })
		slices.Sort(vs)
		return slices.Compact(vs)
	}
	return dashFilter{Org: v.Get("org"), Repo: v.Get("repo"), Author: v.Get("author"), Risk: multi(v["risk"]), Rec: multi(v["rec"]),
		Mine: v.Get("mine") == "1", HideSkipped: v.Get("hideskipped") == "1", HideApproved: v.Get("hideapproved") == "1"}
}

// query is the filter encoded as a URL query, built only from the parsed fields; empty when no filter is active.
func (f dashFilter) query() string {
	v := url.Values{"risk": f.Risk, "rec": f.Rec}
	for k, s := range map[string]string{"org": f.Org, "repo": f.Repo, "author": f.Author} {
		if s != "" {
			v.Set(k, s)
		}
	}
	for k, on := range map[string]bool{"mine": f.Mine, "hideskipped": f.HideSkipped, "hideapproved": f.HideApproved} {
		if on {
			v.Set(k, "1")
		}
	}
	return v.Encode()
}

func (f dashFilter) active() bool { return f.query() != "" }

// filterChip is one removable active-filter value: a stable id from its parameter and value, its label, and the dashboard URL without it.
type filterChip struct{ ID, Label, Href string }

// chips returns one chip per active value in form order, each linking to the query with only that value removed.
func (f dashFilter) chips() []filterChip {
	var cs []filterChip
	add := func(param, value, label string, g dashFilter) {
		href := "/"
		if q := g.query(); q != "" {
			href = "/?" + q
		}
		cs = append(cs, filterChip{"chip-" + param + "-" + value, label, href})
	}
	without := func(vs []string, v string) []string {
		return slices.DeleteFunc(slices.Clone(vs), func(s string) bool { return s == v })
	}
	if g := f; g.Org != "" {
		g.Org = ""
		add("org", f.Org, "Org: "+f.Org, g)
	}
	if g := f; g.Repo != "" {
		g.Repo = ""
		add("repo", f.Repo, "Repository: "+f.Repo, g)
	}
	if g := f; g.Author != "" {
		g.Author = ""
		add("author", f.Author, "Author: "+f.Author, g)
	}
	for _, r := range f.Risk {
		g := f
		g.Risk = without(f.Risk, r)
		add("risk", r, "Risk: "+r, g)
	}
	recNames := map[string]string{"human": "Human review required", "automated": "Automated review is sufficient"}
	for _, r := range f.Rec {
		g := f
		g.Rec = without(f.Rec, r)
		add("rec", r, "Recommendation: "+cmp.Or(recNames[r], r), g)
	}
	if g := f; g.Mine {
		g.Mine = false
		add("mine", "1", "Requested", g)
	}
	if g := f; g.HideSkipped {
		g.HideSkipped = false
		add("hideskipped", "1", "Hide skipped", g)
	}
	if g := f; g.HideApproved {
		g.HideApproved = false
		add("hideapproved", "1", "Hide approved", g)
	}
	return cs
}

// match reports whether a PR passes every active filter. Risk and rec need a done review (current or outdated)
// showing that value; hide approved hides only my approval of the current head, and nothing while login is unknown.
func (f dashFilter) match(pr pullRequest, st jobStatus, known bool, login string) bool {
	owner, _, _ := strings.Cut(pr.Repo, "/")
	done := known && st.Status == statusDone
	recOK := func(t string) bool { return st.Recommendation != "" && recTokens[t] == st.Recommendation }
	mine := func(a approval) bool {
		return !a.Outdated && a.State == "APPROVED" && strings.EqualFold(a.Login, login)
	}
	switch {
	case f.Org != "" && owner != f.Org,
		f.Repo != "" && pr.Repo != f.Repo,
		f.Author != "" && pr.Author != f.Author,
		len(f.Risk) > 0 && !(done && st.Risk != "" && slices.Contains(f.Risk, st.Risk)),
		len(f.Rec) > 0 && !(done && slices.ContainsFunc(f.Rec, recOK)),
		f.Mine && !pr.Requested,
		f.HideSkipped && st.Status == statusSkipped,
		f.HideApproved && login != "" && slices.ContainsFunc(pr.Reviews, mine):
		return false
	}
	return true
}

// choices returns the distinct non-empty values of key over prs plus selected (so an unknown active value
// stays visible in the form), sorted case-insensitively.
func choices(prs []pullRequest, key func(pullRequest) string, selected string) []string {
	var vs []string
	for _, pr := range prs {
		vs = append(vs, key(pr))
	}
	vs = slices.DeleteFunc(append(vs, selected), func(s string) bool { return s == "" })
	slices.SortFunc(vs, func(a, b string) int {
		return cmp.Or(strings.Compare(strings.ToLower(a), strings.ToLower(b)), strings.Compare(a, b))
	})
	return slices.Compact(vs)
}

// appOrigin is the origin of the app window's pages: the Wails app serves them from wails://localhost.
const appOrigin = "wails://localhost"

// seeOther sends a form POST on to url: HX-Redirect for htmx, because the app's WebKit scheme handler
// does not follow a 303 after a plain form POST, and 303 otherwise.
func seeOther(w http.ResponseWriter, r *http.Request, url string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", url)
		return
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

func newHandler(ch *configHolder, p *poller, q *reviewQueue,
	submit func(ctx context.Context, repo string, number int, rv reviewPayload) (string, error),
	approved func(ctx context.Context, repo string, number int, login, sha string) (bool, error),
	files func(ctx context.Context, repo string, number int) ([]prFile, error),
	content func(ctx context.Context, repo, sha, path string) ([]byte, error),
	threads func(ctx context.Context, repo string, number int) (threadList, error),
	resolve func(ctx context.Context, threadID string, resolved bool) (bool, error)) http.Handler {
	mux := http.NewServeMux()
	diffs := newDiffCache(files)
	var threadsMu sync.Mutex
	threadCache := map[string]threadList{} // by prKey: the PR's GitHub threads as last fetched
	// fetchThreads fetches pr's threads into the cache; a failed fetch removes the entry.
	fetchThreads := func(ctx context.Context, pr pullRequest) (threadList, error) {
		ctx, cancel := context.WithTimeout(ctx, threadsTimeout)
		defer cancel()
		tl, err := threads(ctx, pr.Repo, pr.Number)
		threadsMu.Lock()
		defer threadsMu.Unlock()
		if err != nil {
			delete(threadCache, prKey(pr))
			return threadList{}, err
		}
		threadCache[prKey(pr)] = tl
		return tl, nil
	}
	// cachedThreads is pr's cached threads, fetched only when there is no entry.
	cachedThreads := func(ctx context.Context, pr pullRequest) (threadList, error) {
		threadsMu.Lock()
		tl, ok := threadCache[prKey(pr)]
		threadsMu.Unlock()
		if ok {
			return tl, nil
		}
		return fetchThreads(ctx, pr)
	}
	// threadFor is the thread id of pr's cached threads (the thread check): an id of another PR is not found.
	threadFor := func(ctx context.Context, pr pullRequest, id string) (reviewThread, bool, error) {
		if len(id) > maxThreadID {
			return reviewThread{}, false, nil
		}
		tl, err := cachedThreads(ctx, pr)
		i := slices.IndexFunc(tl.Threads, func(t reviewThread) bool { return t.ID == id })
		if err != nil || i < 0 {
			return reviewThread{}, false, err
		}
		return tl.Threads[i], true, nil
	}
	// findPR returns the dashboard PR repo#number.
	findPR := func(repo string, number int) (pullRequest, bool) {
		prs := p.snapshot().PRs
		i := slices.IndexFunc(prs, func(pr pullRequest) bool { return pr.Repo == repo && pr.Number == number })
		if i < 0 {
			return pullRequest{}, false
		}
		return prs[i], true
	}
	// pathPR returns the dashboard PR of a /pr/{owner}/{name}/{number} path.
	pathPR := func(r *http.Request) (pullRequest, bool) {
		number, err := strconv.Atoi(r.PathValue("number"))
		pr, ok := findPR(r.PathValue("owner")+"/"+r.PathValue("name"), number)
		return pr, err == nil && ok
	}
	ownPR := func(pr pullRequest) bool {
		login := p.snapshot().Login
		return login != "" && strings.EqualFold(login, pr.Author)
	}
	// reviewState is what a review of pr would submit now: the rows and line comments of a done result (nil otherwise)
	// with the pending review p, and the included, matching, non-posted drafts in row order, line comments last.
	reviewState := func(pr pullRequest) (st jobStatus, known bool, pend pendingPR, items, comments []checkItem, drafts []pendingDraft) {
		st, known = q.statuses()[prKey(pr)]
		pend = q.pending.get(prKey(pr))
		if known && st.Status == statusDone {
			rep := newReport(pr.Repo, pr.Number, st)
			items, comments = checkItems(rep, st, pend), commentItems(rep, pend, st.HeadSHA)
		}
		for _, it := range slices.Concat(items, comments) {
			if it.Draft.InReview && it.Draft.PostedURL == "" {
				drafts = append(drafts, it.Draft)
			}
		}
		return
	}
	// dialog is the submit dialog for event (COMMENT on your own PR) with the summary body, as the PR is now;
	// threadsErr is a thread fetch that just failed, so the replies are not looked up again.
	dialog := func(ctx context.Context, pr pullRequest, event, body string, threadsErr error) dialogData {
		st, known, pend, items, _, drafts := reviewState(pr)
		d := dialogData{PR: pr, Event: event, Body: body, Rev: strconv.Itoa(pend.Rev), Head: pr.HeadSHA, OwnPR: ownPR(pr), Points: len(items)}
		if d.OwnPR {
			d.Event = "COMMENT"
		}
		d.Confirm = confirmReasons(pr, st, known)
		d.Expect = approveHash(d.Confirm)
		if st.Outdated && known && st.Status == statusDone {
			d.OutdatedSHA = st.HeadSHA
		}
		checked, _ := pendingCounts(items)
		d.Unchecked = len(items) - checked
		for _, dr := range drafts {
			d.Comments = append(d.Comments, newDialogComment(dr))
		}
		sent := len(drafts)
		if len(pend.Replies) > 0 {
			tl, err := threadList{}, threadsErr
			if err == nil {
				tl, err = cachedThreads(ctx, pr)
			}
			for _, rp := range pend.Replies {
				c := newDialogComment(pendingDraft{File: rp.Path, Line: rp.Line, Text: rp.Text})
				c.At, c.Discard = "Reply · "+c.At, err == nil && !replyable(tl, rp.Thread)
				if !c.Discard {
					sent++
				}
				d.Comments = append(d.Comments, c)
			}
		}
		d.Label = submitLabel(d.Event, sent)
		return d
	}
	writeDialog := func(w http.ResponseWriter, code int, d dialogData) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(code)
		detailsTmpl.ExecuteTemplate(w, "dialog", d)
	}
	writeHTML := func(w http.ResponseWriter, code int, name string, data any) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(code)
		detailsTmpl.ExecuteTemplate(w, name, data)
	}
	// stale reports whether r comes from a page showing another result than pr's current one: a write must send
	// the page's results (details.js adds it), a GET is checked only when it sends one.
	stale := func(r *http.Request, pr pullRequest) bool {
		r.ParseForm()
		v, sent := r.Form["results"]
		if !sent {
			return r.Method != http.MethodGet
		}
		return v[0] != resultID(q.statuses()[prKey(pr)])
	}
	// writeStale answers a stale fragment request with 409 and the error in the bar, swapping nothing else, so the
	// page keeps what it shows (a textarea keeps its text); editor is the id whose editor status says Not saved.
	writeStale := func(w http.ResponseWriter, editor string) {
		w.Header().Set("HX-Reswap", "none")
		writeHTML(w, http.StatusConflict, "stale", struct{ ID, Error string }{editor, staleMsg})
	}
	// reconcileReview carries pr's pending review over to its current done result (see reconcile). The files are read
	// before the pending lock is taken; a draft added meanwhile has no line and becomes Outdated.
	reconcileReview := func(ctx context.Context, pr pullRequest) error {
		key := prKey(pr)
		st, known := q.statuses()[key]
		if !known || st.Status != statusDone || q.pending.get(key).Result == resultID(st) {
			return nil // never for queued, running or failed: it would drop the untouched suggestions and marks
		}
		moved := lineMap(ctx, content, pr.Repo, q.pending.get(key).Drafts, st.HeadSHA)
		rows := checkItems(newReport(pr.Repo, pr.Number, st), st, pendingPR{})
		_, err := q.pending.update(key, func(p *pendingPR) error {
			if p.Result == resultID(st) {
				return errUnchanged // another request reconciled it meanwhile
			}
			*p = reconcile(*p, st, rows, moved)
			return nil
		})
		if errors.Is(err, errUnchanged) {
			return nil
		}
		return err
	}
	// diffPage is the diff view of pr: the diff of a current done result, else one alert.
	diffPage := func(ctx context.Context, pr pullRequest, st jobStatus, known bool, items, comments []checkItem, threads []threadView) diffView {
		moved := diffView{Alert: fmt.Sprintf("The diff is shown for the reviewed commit only; the pull request moved to %s. Re-run or wait for the new review.", shortSHA(pr.HeadSHA))}
		switch {
		case !known || st.Status != statusDone:
			return diffView{Alert: "The diff is shown once the review is done."}
		case st.Outdated || st.HeadSHA != pr.HeadSHA:
			return moved
		}
		d, err := diffs.get(ctx, pr.Repo, pr.Number, st.HeadSHA)
		switch {
		case errors.Is(err, errDiffOutdated):
			return moved
		case err != nil:
			return diffView{Alert: "Could not load the diff: " + err.Error()}
		}
		return newDiffView(d, items, comments, threads, diffs.texts(ctx, pr.Repo, pr.Number, st.HeadSHA, contentPaths(d, items), content))
	}
	// lastQuery is the canonical filter query of the latest GET /, for the details page's back link; memory only.
	// shortcut: two dashboards open at once means the last GET / wins, fine for a single-user single-window app; carry a validated param if multi-window is ever supported.
	var lastQueryMu sync.Mutex
	var lastQuery string
	renderDetails := func(w http.ResponseWriter, r *http.Request, pr pullRequest) {
		st, known, pend, items, comments, _ := reviewState(pr)
		reasons := approveReasons(pr, st, known)
		confirm := confirmReasons(pr, st, known)
		login := p.snapshot().Login
		d := detailsData{PR: pr, Login: login, Review: st, QuietPeriod: q.quietPeriod, View: "list", Items: items, Comments: comments, Rev: pend.Rev, OwnPR: ownPR(pr),
			Back: "/", Submitted: submittedLine(r.URL.Query()), ConfirmLines: confirm, ApproveExpect: approveHash(confirm), Results: resultID(st)}
		lastQueryMu.Lock()
		if lastQuery != "" {
			d.Back = "/?" + lastQuery
		}
		lastQueryMu.Unlock()
		tl, err := fetchThreads(r.Context(), pr)
		placed := err == nil && known && threadsPlaced(st, tl)
		if err != nil {
			d.ThreadsError = err.Error()
		} else if !placed && len(tl.Threads) > 0 {
			d.ThreadsHead = tl.Head
		}
		if r.URL.Query().Get("view") == "diff" {
			d.View = "diff"
			point := r.URL.Query().Get("point")
			for i := range items {
				items[i].Anchor = "d-"
				items[i].Excerpt = items[i].ID == point // the focused block renders open
			}
			var tv []threadView
			if placed {
				for _, t := range tl.Threads {
					tv = append(tv, newThreadView(pr.Repo, pr.Number, t, pend))
				}
			}
			d.Diff = diffPage(r.Context(), pr, st, known, items, comments, tv)
		} else if placed {
			all := slices.Concat(d.Items, d.Comments)
			withThreads(d.Items, all, tl.Threads, pr.Repo, pr.Number, pend)
			withThreads(d.Comments, all, tl.Threads, pr.Repo, pr.Number, pend)
		}
		if known && st.Status == statusDone {
			d.CheckedN, d.InReview = pendingCounts(slices.Concat(d.Items, d.Comments))
			d.InReview += len(pend.Replies)
			d.Counts = verdictCounts(st.Findings, len(d.Items)-len(st.Findings))
			d.Reasons = verdictReasons(st.Rationale)
			sum := strings.TrimSpace(st.Summary)
			d.LongSummary = strings.Count(sum, "\n") >= 2 || utf8.RuneCountInString(sum) > 240
			d.Compact = st.Risk == "Low" && len(reasons) == 0 && d.InReview == 0
		}
		ctx, cancel := context.WithTimeout(r.Context(), approvedCheckTimeout)
		defer cancel()
		if d.Approved, err = approved(ctx, pr.Repo, pr.Number, login, pr.HeadSHA); err != nil {
			d.ApprovedError = err.Error()
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		detailsTmpl.Execute(w, d)
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Vary", "HX-Request")
		snap := p.snapshot()
		if snap.Global != "" {
			snap.Errors = nil // the poller keeps per-repository errors from before the global one; they are stale
		}
		review := q.statuses()
		slices.SortFunc(snap.PRs, compareRows(review)) // snapshot returns a fresh slice
		f := parseFilter(r.URL.Query())
		d := indexData{Repositories: ch.get().Repositories, Poll: snap, Filter: f, Query: f.query(), Chips: f.chips(), Review: review,
			Orgs:    choices(snap.PRs, func(pr pullRequest) string { o, _, _ := strings.Cut(pr.Repo, "/"); return o }, f.Org),
			Repos:   choices(snap.PRs, func(pr pullRequest) string { return pr.Repo }, f.Repo),
			Authors: choices(snap.PRs, func(pr pullRequest) string { return pr.Author }, f.Author)}
		lastQueryMu.Lock()
		lastQuery = d.Query
		lastQueryMu.Unlock()
		for _, pr := range snap.PRs {
			if st, known := review[prKey(pr)]; f.match(pr, st, known, snap.Login) {
				d.Rows = append(d.Rows, pr)
			}
		}
		d.Hidden = len(snap.PRs) - len(d.Rows)
		d.PollEvery = 60
		for _, pr := range d.Rows {
			if st := review[prKey(pr)].Status; st == statusQueued || st == statusRunning {
				d.PollEvery = 10
			}
		}
		if !snap.FirstPollDone {
			d.PollEvery = 10
		}
		// htmx filter change or poll: only the list and the page status, with the canonical URL to push on a filter change;
		// a history restore needs the whole page
		if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-History-Restore-Request") == "" {
			if r.Header.Get("HX-Trigger") != "pr-poll" {
				w.Header().Set("HX-Push-Url", "/?"+d.Query)
				if d.Query == "" {
					w.Header().Set("HX-Push-Url", "/")
				}
			}
			indexTmpl.ExecuteTemplate(w, "list", d)
			indexTmpl.ExecuteTemplate(w, "oob", d)
			return
		}
		indexTmpl.Execute(w, d)
	})
	mux.HandleFunc("GET /myreview.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Write(stylesheet)
	})
	mux.HandleFunc("GET /static/htmx.min.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Write(htmxJS)
	})
	mux.HandleFunc("GET /static/dashboard.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Write(dashboardJS)
	})
	mux.HandleFunc("GET /static/details.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Write(detailsJS)
	})
	mux.HandleFunc("GET /static/links.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Write(linksJS)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		snap := p.snapshot()
		h := health{Status: "ok", Version: version, Repositories: len(ch.get().Repositories), PullRequests: len(snap.PRs)}
		if !snap.LastSuccessfulPoll.IsZero() {
			h.LastSuccessfulPoll = &snap.LastSuccessfulPoll
		}
		json.NewEncoder(w).Encode(h)
	})
	mux.HandleFunc("GET /pr/{owner}/{name}/{number}", func(w http.ResponseWriter, r *http.Request) {
		pr, ok := pathPR(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if err := reconcileReview(r.Context(), pr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// the review speed clock: starts on the first open after a new result; the post-submit redirect must not restart it
		if key, cur := prKey(pr), resultID(q.statuses()[prKey(pr)]); !r.URL.Query().Has("submitted") {
			if p := q.pending.get(key); p.OpenedAt.IsZero() || p.OpenedResult != cur {
				if _, err := q.pending.update(key, func(p *pendingPR) error {
					p.OpenedAt, p.OpenedResult = time.Now().UTC(), cur
					return nil
				}); err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
			}
		}
		renderDetails(w, r, pr)
	})
	// GET …/status?results=&head= is the details page's update banner: new results since the page's results, or a
	// new head since its head; 204 otherwise. It reads only memory and the pending store, never GitHub.
	mux.HandleFunc("GET /pr/{owner}/{name}/{number}/status", func(w http.ResponseWriter, r *http.Request) {
		pr, ok := pathPR(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		qv := r.URL.Query()
		st := q.statuses()[prKey(pr)]
		id, pend := resultID(st), q.pending.get(prKey(pr))
		b := struct {
			PR         pullRequest
			View, Text string
			Reload     bool
		}{PR: pr, View: "list"}
		if qv.Get("view") == "diff" {
			b.View = "diff"
		}
		switch {
		case id != qv.Get("results") && st.Status == statusDone && pend.Result == id:
			b.Text, b.Reload = fmt.Sprintf("New review results for commit %s were loaded in another tab.", shortSHA(st.HeadSHA)), true
		case id != qv.Get("results") && st.Status == statusDone:
			kept, edited := 0, 0
			for _, d := range pend.Drafts {
				if d.PostedURL != "" {
					continue
				}
				if d.Edited || commentNo(d.ID) > 0 {
					kept++
				}
				if d.Edited && commentNo(d.ID) == 0 {
					edited++
				}
			}
			b.Text, b.Reload = fmt.Sprintf("New review results for commit %s. Your %s will be kept; %s will be replaced.", shortSHA(st.HeadSHA),
				plural(kept, "edited draft"), plural(max(pend.Rows-edited, 0), "untouched suggestion")), true
		case id != qv.Get("results") && st.Status == statusFailed:
			b.Text, b.Reload = fmt.Sprintf("The review of commit %s failed at %s: %s.", shortSHA(st.HeadSHA), st.FailedStep, st.Reason), true
		case qv.Get("head") != pr.HeadSHA && st.Status == statusOnDemand:
			b.Text = fmt.Sprintf("PR updated to %s; click Review to review it.", shortSHA(pr.HeadSHA))
		case qv.Get("head") != pr.HeadSHA && st.Status == statusSkipped:
			b.Text = fmt.Sprintf("PR updated to %s; not reviewed (skipped by rule skip[%d]).", shortSHA(pr.HeadSHA), st.SkipRule)
		case qv.Get("head") != pr.HeadSHA && pr.Draft:
			b.Text = fmt.Sprintf("PR updated to %s; Re-run to review it.", shortSHA(pr.HeadSHA))
		case qv.Get("head") != pr.HeadSHA:
			b.Text = fmt.Sprintf("PR updated to %s; review queued.", shortSHA(pr.HeadSHA))
		default:
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeHTML(w, http.StatusOK, "update", b)
	})
	// POST …/reload reconciles the pending review with the current done result and returns to the page.
	mux.HandleFunc("POST /pr/{owner}/{name}/{number}/reload", func(w http.ResponseWriter, r *http.Request) {
		pr, ok := pathPR(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if err := reconcileReview(r.Context(), pr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		view := "list"
		if r.PostFormValue("view") == "diff" {
			view = "diff"
		}
		seeOther(w, r, fmt.Sprintf("/pr/%s/%d?view=%s", pr.Repo, pr.Number, view)) // built from the validated PR
	})
	// pendingItem resolves the row of a /pr/{owner}/{name}/{number}/…/{id} path: a dashboard PR with a done result
	// and an id lookupItem accepts; a line comment id only when comments is set.
	pendingItem := func(r *http.Request, comments bool) (key string, rep report, st jobStatus, it checkItem, ok bool) {
		pr, found := pathPR(r)
		st, known := q.statuses()[prKey(pr)]
		id := r.PathValue("id")
		if !found || !known || st.Status != statusDone || (!comments && strings.HasPrefix(id, "c")) {
			return "", report{}, st, checkItem{}, false
		}
		rep = newReport(pr.Repo, pr.Number, st)
		it, ok = lookupItem(rep, st, q.pending.get(prKey(pr)), id)
		// a line comment's editor sends its path and line, so an editor left open after its comment was deleted
		// (in another tab) cannot change a later comment that reuses the id
		if ok && commentNo(id) > 0 && (r.PostFormValue("path") != it.File || r.PostFormValue("line") != strconv.Itoa(it.Line)) {
			ok = false
		}
		return prKey(pr), rep, st, it, ok
	}
	// pendingRow is the row it with the pending review p, numbered as in the list, and the list counts.
	pendingRow := func(rep report, st jobStatus, it checkItem, p pendingPR) (checkItem, detailsData) {
		d := detailsData{Items: checkItems(rep, st, p), Comments: commentItems(rep, p, st.HeadSHA)}
		all := slices.Concat(d.Items, d.Comments)
		d.CheckedN, d.InReview = pendingCounts(all)
		d.InReview += len(p.Replies)
		if i := slices.IndexFunc(all, func(x checkItem) bool { return x.ID == it.ID }); i >= 0 {
			return all[i], d
		}
		return withPending(it, p, st.HeadSHA), d // a point generated from a finding is not listed; a deleted line comment
	}
	// changePending applies fn to the pending review of the row of r and answers with template name,
	// given the changed row and the list counts. fn gets the row as the pending review it changes shows it.
	// A refused or failed change answers with the row as stored and the error, so the page shows the server state.
	changePending := func(w http.ResponseWriter, r *http.Request, name string, comments bool, fn func(p *pendingPR, it checkItem) error) {
		if pr, ok := pathPR(r); ok && stale(r, pr) {
			editor := ""
			if name != "checked" {
				editor = r.PathValue("id")
			}
			writeStale(w, editor)
			return
		}
		key, rep, st, it, ok := pendingItem(r, comments)
		if !ok {
			http.NotFound(w, r)
			return
		}
		p, err := q.pending.update(key, func(p *pendingPR) error { return fn(p, withPending(it, *p, st.HeadSHA)) })
		code, msg := http.StatusOK, ""
		var se *statusError
		switch {
		case errors.As(err, &se):
			code, msg = se.code, se.msg
		case err != nil:
			fmt.Fprintln(os.Stderr, err)
			code, msg = http.StatusInternalServerError, err.Error()
		}
		if err != nil {
			p = q.pending.get(key)
		}
		row, d := pendingRow(rep, st, it, p)
		deleted := commentNo(it.ID) > 0 && p.draftAt(row, st.HeadSHA) < 0 // a line comment that is gone
		if deleted {
			// the whole block goes (its diff block, or the list row's details), so its stale editor cannot
			// save over a later comment that gets the same id
			w.Header().Set("HX-Retarget", "#d-"+it.ID+", #pt-"+it.ID+" > details")
			w.Header().Set("HX-Reswap", "outerHTML")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(code)
		detailsTmpl.ExecuteTemplate(w, name, struct {
			Item    checkItem
			Details detailsData
			Error   string
			Deleted bool
		}{row, d, msg, deleted})
	}
	// draftFor returns the index of the row's draft in p, adding the row's suggestion when it has none;
	// a posted comment cannot change.
	draftFor := func(p *pendingPR, it checkItem) (int, error) {
		if it.Draft.PostedURL != "" {
			return 0, &statusError{http.StatusConflict, "comment already posted"}
		}
		i := p.draftAt(it, it.Draft.Commit)
		if i < 0 && commentNo(it.ID) > 0 {
			return 0, &statusError{http.StatusNotFound, "comment not found; it was deleted"}
		}
		if i < 0 {
			p.Drafts = append(p.Drafts, it.Draft)
			i = len(p.Drafts) - 1
		}
		return i, nil
	}
	mux.HandleFunc("GET /pr/{owner}/{name}/{number}/points/{id}", func(w http.ResponseWriter, r *http.Request) {
		if pr, ok := pathPR(r); ok && stale(r, pr) {
			writeStale(w, "")
			return
		}
		key, rep, st, it, ok := pendingItem(r, false)
		if !ok {
			http.NotFound(w, r)
			return
		}
		pend := q.pending.get(key)
		row, list := pendingRow(rep, st, it, pend)
		row.Excerpt = true
		if r.URL.Query().Get("view") == "diff" {
			row.Anchor = "d-"
		} else if tl, err := cachedThreads(r.Context(), pullRequest{Repo: rep.Repo, Number: rep.Number}); err == nil && threadsPlaced(st, tl) {
			rows := []checkItem{row}
			withThreads(rows, slices.Concat(list.Items, list.Comments), tl.Threads, rep.Repo, rep.Number, pend)
			row = rows[0]
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		detailsTmpl.ExecuteTemplate(w, "point-body", row)
	})
	// GET …/points/{id}/excerpt is a few lines of the point's file at the reviewed commit; the file comes from
	// content, not the diff, so it works for points outside the diff and for outdated results.
	mux.HandleFunc("GET /pr/{owner}/{name}/{number}/points/{id}/excerpt", func(w http.ResponseWriter, r *http.Request) {
		if pr, ok := pathPR(r); ok && stale(r, pr) {
			writeStale(w, "")
			return
		}
		_, rep, st, it, ok := pendingItem(r, false)
		if !ok {
			http.NotFound(w, r)
			return
		}
		var lines []diffLine
		if it.Line > 0 && validRepoPath(it.File) { // File is agent output
			if text, err := content(r.Context(), rep.Repo, st.HeadSHA, it.File); err == nil {
				lines = excerpt(text, it.Line)
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		detailsTmpl.ExecuteTemplate(w, "excerpt", lines)
	})
	mux.HandleFunc("POST /pr/{owner}/{name}/{number}/points/{id}/checked", func(w http.ResponseWriter, r *http.Request) {
		v := r.PostFormValue("checked")
		changePending(w, r, "checked", false, func(p *pendingPR, it checkItem) error {
			if v != "1" && v != "0" {
				return &statusError{http.StatusBadRequest, "checked must be 1 or 0"}
			}
			if i := p.checkedAt(it); i >= 0 {
				p.Checked = slices.Delete(p.Checked, i, i+1)
			}
			if v == "1" {
				p.Checked = append(p.Checked, pendingCheck{ID: it.ID, File: it.File, Line: it.Line, Text: it.Text})
			}
			return nil
		})
	})
	mux.HandleFunc("PUT /pr/{owner}/{name}/{number}/drafts/{id}", func(w http.ResponseWriter, r *http.Request) {
		text := r.PostFormValue("text")
		changePending(w, r, "saved", true, func(p *pendingPR, it checkItem) error {
			if utf8.RuneCountInString(text) > maxCommentLen {
				return &statusError{http.StatusBadRequest, fmt.Sprintf("comment text is over %d characters", maxCommentLen)}
			}
			i, err := draftFor(p, it)
			if err == nil && strings.TrimSpace(text) == "" && commentNo(it.ID) > 0 { // emptying a line comment discards it
				p.Drafts = slices.Delete(p.Drafts, i, i+1)
				return nil
			}
			if err == nil && p.Drafts[i].Text != text {
				p.Drafts[i].Text, p.Drafts[i].Edited = text, true
			}
			return err
		})
	})
	mux.HandleFunc("POST /pr/{owner}/{name}/{number}/drafts/{id}/include", func(w http.ResponseWriter, r *http.Request) {
		v := r.PostFormValue("include")
		changePending(w, r, "included", true, func(p *pendingPR, it checkItem) error {
			if v != "1" && v != "0" {
				return &statusError{http.StatusBadRequest, "include must be 1 or 0"}
			}
			i, err := draftFor(p, it)
			if err != nil {
				return err
			}
			if v == "1" && strings.TrimSpace(p.Drafts[i].Text) == "" {
				return &statusError{http.StatusBadRequest, "comment text is empty"}
			}
			p.Drafts[i].InReview = v == "1"
			return nil
		})
	})
	// GET …/review/summary is the submit dialog; body is the summary to keep when the decision changes.
	mux.HandleFunc("GET /pr/{owner}/{name}/{number}/review/summary", func(w http.ResponseWriter, r *http.Request) {
		pr, ok := pathPR(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		event := cmp.Or(r.URL.Query().Get("event"), "COMMENT")
		if eventNames[event] == "" {
			http.Error(w, "event must be APPROVE, REQUEST_CHANGES or COMMENT", http.StatusBadRequest)
			return
		}
		d := dialog(r.Context(), pr, event, r.URL.Query().Get("body"), nil)
		if stale(r, pr) {
			d.Error = staleMsg
			writeDialog(w, http.StatusConflict, d)
			return
		}
		writeDialog(w, http.StatusOK, d)
	})
	var approvingMu sync.Mutex
	approving := map[string]bool{} // prKey of reviews being submitted
	// POST …/review submits one GitHub review: the decision, the summary and the included drafts.
	// It is the only path that posts to GitHub; nothing posts or approves automatically.
	mux.HandleFunc("POST /pr/{owner}/{name}/{number}/review", func(w http.ResponseWriter, r *http.Request) {
		pr, ok := pathPR(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		key := prKey(pr)
		event, body, rev := r.PostFormValue("event"), r.PostFormValue("body"), r.PostFormValue("rev")
		st, known, pend, items, _, drafts := reviewState(pr)
		commit := pr.HeadSHA
		if known && st.Status == statusDone {
			commit = st.HeadSHA
		}
		confirm := confirmReasons(pr, st, known)
		var threadsErr error
		// fail answers with the dialog as the form showed it (rev, head, expect), the summary kept, and the error
		fail := func(code int, msg string) {
			ev := event
			if eventNames[ev] == "" {
				ev = "COMMENT"
			}
			d := dialog(r.Context(), pr, ev, body, threadsErr)
			d.Error, d.Rev, d.Head, d.Expect = msg, rev, r.PostFormValue("head"), r.PostFormValue("expect")
			writeDialog(w, code, d)
		}
		staleRev := "your drafts changed since the dialog opened; open it again"
		verb := map[string]string{"APPROVE": "approve", "REQUEST_CHANGES": "request changes", "COMMENT": "comment"}
		switch {
		case stale(r, pr):
			fail(http.StatusConflict, staleMsg)
			return
		case verb[event] == "":
			fail(http.StatusBadRequest, "event must be APPROVE, REQUEST_CHANGES or COMMENT")
			return
		case utf8.RuneCountInString(body) > maxCommentLen:
			fail(http.StatusBadRequest, fmt.Sprintf("summary is over %d characters", maxCommentLen))
			return
		case event != "COMMENT" && ownPR(pr):
			fail(http.StatusConflict, "GitHub does not allow approving or requesting changes on your own pull request")
			return
		case r.PostFormValue("head") != pr.HeadSHA:
			fail(http.StatusConflict, fmt.Sprintf("the pull request has new commits since you loaded the page (now at %s). Check the page and submit again.", shortSHA(pr.HeadSHA)))
			return
		case rev != strconv.Itoa(pend.Rev):
			fail(http.StatusConflict, staleRev)
			return
		case event == "APPROVE" && approveHash(confirm) != r.PostFormValue("expect"):
			fail(http.StatusConflict, "the review changed since you loaded the page. Check the page and approve again.")
			return
		case event == "APPROVE" && len(confirm) > 0 && r.PostFormValue("confirm") != "1":
			fail(http.StatusConflict, "confirm the approval first.")
			return
		}
		approvingMu.Lock()
		busy := approving[key]
		approving[key] = true
		approvingMu.Unlock()
		if busy {
			fail(http.StatusConflict, "a review is already being submitted")
			return
		}
		defer func() {
			approvingMu.Lock()
			delete(approving, key)
			approvingMu.Unlock()
		}()
		// a submit that finished between the rev check and taking the guard changed rev; same rev, same drafts
		if q.pending.get(key).Rev != pend.Rev {
			fail(http.StatusConflict, staleRev)
			return
		}
		// replies go only to threads that are still this PR's and open to replies; the others are discarded
		var replies []threadReply
		var discarded []string
		if len(pend.Replies) > 0 {
			tl, err := fetchThreads(r.Context(), pr)
			if err != nil {
				threadsErr = err
				fail(http.StatusBadGateway, "Could not load GitHub discussions: "+err.Error())
				return
			}
			for _, rp := range pend.Replies {
				if replyable(tl, rp.Thread) {
					replies = append(replies, threadReply{rp.Thread, rp.Text})
				} else {
					discarded = append(discarded, rp.Thread)
				}
			}
		}
		if event != "APPROVE" && len(drafts)+len(replies) == 0 && strings.TrimSpace(body) == "" {
			if len(discarded) > 0 { // replies that can never be sent go now, or they would keep this review from being empty
				p, err := q.pending.update(key, func(p *pendingPR) error {
					p.Replies = slices.DeleteFunc(p.Replies, func(x pendingReply) bool { return slices.Contains(discarded, x.Thread) })
					return nil
				})
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
				} else {
					rev = strconv.Itoa(p.Rev)
				}
			}
			fail(http.StatusConflict, "Add a comment or a summary to "+verb[event])
			return
		}
		rv := newReview(commit, event, body, drafts)
		rv.Replies = replies
		// The call outlives a closed tab so its outcome is always recorded.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), reviewTimeout)
		defer cancel()
		u, err := submit(ctx, pr.Repo, pr.Number, rv)
		timedOut := err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded)
		if err != nil && !timedOut {
			fail(http.StatusBadGateway, err.Error())
			return
		}
		if q.recordReview != nil {
			rr := newReviewRecord(pr, commit, event, st, known, pend, items, drafts, len(replies))
			rr.SubmittedAt, rr.Uncertain = time.Now().UTC(), timedOut
			q.recordReview(rr)
		}
		// on success the drafts are posted; on a timeout the review may be posted, so only rev grows to refuse a blind resubmit
		_, serr := q.pending.update(key, func(p *pendingPR) error {
			for _, d := range drafts {
				if i := slices.IndexFunc(p.Drafts, func(x pendingDraft) bool {
					return x.ID == d.ID && x.File == d.File && x.Line == d.Line && x.SourceText == d.SourceText && x.Commit == d.Commit
				}); i >= 0 && !timedOut {
					p.Drafts[i].PostedURL, p.Drafts[i].InReview = u, false
				}
			}
			if !timedOut { // a reply edited meanwhile was not sent and stays
				p.OpenedAt, p.OpenedResult = time.Time{}, ""
				p.Replies = slices.DeleteFunc(p.Replies, func(x pendingReply) bool {
					return slices.Contains(discarded, x.Thread) || slices.Contains(replies, threadReply{x.Thread, x.Text})
				})
			}
			return nil
		})
		if serr != nil {
			fmt.Fprintln(os.Stderr, serr)
		}
		switch {
		case timedOut:
			fail(http.StatusBadGateway, err.Error()+"; the review may have been posted, check GitHub")
		case serr != nil:
			fail(http.StatusInternalServerError, "the review was posted ("+u+") but saving the drafts failed: "+serr.Error())
		default:
			p.markReviewed(pr.Repo, pr.Number, p.snapshot().Login, commit, reviewStates[event])
			w.Header().Set("HX-Redirect", fmt.Sprintf("/pr/%s/%d?submitted=%s&n=%d", pr.Repo, pr.Number, event, len(drafts)+len(replies)))
			w.WriteHeader(http.StatusOK)
		}
	})
	// GET …/diff/expand answers with the lines of one gap of a file at the reviewed commit, as context rows.
	mux.HandleFunc("GET /pr/{owner}/{name}/{number}/diff/expand", func(w http.ResponseWriter, r *http.Request) {
		pr, ok := pathPR(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if stale(r, pr) {
			http.Error(w, staleMsg, http.StatusConflict)
			return
		}
		st, known := q.statuses()[prKey(pr)]
		if !known || st.Status != statusDone || st.Outdated || st.HeadSHA != pr.HeadSHA {
			http.Error(w, "the review is not done for the current commit; reload the page", http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fail := func(reason string) { detailsTmpl.ExecuteTemplate(w, "expand-error", reason) }
		d, err := diffs.get(r.Context(), pr.Repo, pr.Number, st.HeadSHA)
		if err != nil {
			fail(err.Error())
			return
		}
		qv := r.URL.Query()
		f, err := newSideFile(d, qv.Get("path"))
		var from, to int
		if err == nil {
			from, to, err = checkGap(f, qv.Get("from"), qv.Get("to"))
		}
		var se *statusError
		if errors.As(err, &se) {
			http.Error(w, se.msg, se.code)
			return
		}
		text, err := content(r.Context(), pr.Repo, st.HeadSHA, f.Filename) // f.Filename is from GitHub's file list
		if err != nil {
			fail(err.Error())
			return
		}
		detailsTmpl.ExecuteTemplate(w, "expanded", expandRows(f, from, to, textLines(text)))
	})
	// lineTarget validates path and line of a new line comment against the diff of pr's current done result:
	// 409 when the result is not current, 404 for a path, 400 for a line not in the diff, 502 when GitHub fails.
	lineTarget := func(ctx context.Context, pr pullRequest, path, line string) (jobStatus, diffFile, int, error) {
		st, known := q.statuses()[prKey(pr)]
		if !known || st.Status != statusDone || st.Outdated || st.HeadSHA != pr.HeadSHA {
			return st, diffFile{}, 0, &statusError{http.StatusConflict, "the review is not done for the current commit; reload the page"}
		}
		d, err := diffs.get(ctx, pr.Repo, pr.Number, st.HeadSHA)
		switch {
		case errors.Is(err, errDiffOutdated):
			return st, diffFile{}, 0, &statusError{http.StatusConflict, err.Error() + "; reload the page"}
		case err != nil:
			return st, diffFile{}, 0, &statusError{http.StatusBadGateway, "Could not load the diff: " + err.Error()}
		}
		f, err := newSideFile(d, path)
		n := 0
		if err == nil {
			n, err = checkLine(f, line)
		}
		return st, f, n, err
	}
	// commentForm is the new line comment form, or a refused POST …/drafts with its text kept and the error.
	type commentForm struct {
		PR                      pullRequest
		Path, Line, Text, Error string
	}
	// GET …/diff/comment-form is the editor of your comment on a diff line, or a form to add one.
	mux.HandleFunc("GET /pr/{owner}/{name}/{number}/diff/comment-form", func(w http.ResponseWriter, r *http.Request) {
		pr, ok := pathPR(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		qv := r.URL.Query()
		st, f, line, err := lineTarget(r.Context(), pr, qv.Get("path"), qv.Get("line"))
		if stale(r, pr) {
			err = &statusError{http.StatusConflict, staleMsg}
		}
		var se *statusError
		if errors.As(err, &se) {
			writeHTML(w, se.code, "inline-error", se.msg)
			return
		}
		pend := q.pending.get(prKey(pr))
		for _, c := range commentItems(newReport(pr.Repo, pr.Number, st), pend, st.HeadSHA) {
			if c.File == f.Filename && c.Line == line && c.Draft.OutdatedFrom == "" {
				writeHTML(w, http.StatusOK, "line-comment", c)
				return
			}
		}
		writeHTML(w, http.StatusOK, "comment-form", commentForm{PR: pr, Path: f.Filename, Line: strconv.Itoa(line)})
	})
	// POST …/drafts adds your comment on a diff line (path, line, text) to the review, or replaces the text of
	// the one already there, so a double submit never makes two.
	mux.HandleFunc("POST /pr/{owner}/{name}/{number}/drafts", func(w http.ResponseWriter, r *http.Request) {
		pr, ok := pathPR(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		form := commentForm{PR: pr, Path: r.PostFormValue("path"), Line: r.PostFormValue("line"), Text: r.PostFormValue("text")}
		key := prKey(pr)
		st, f, line, err := lineTarget(r.Context(), pr, form.Path, form.Line)
		switch {
		case stale(r, pr):
			err = &statusError{http.StatusConflict, staleMsg}
		case err != nil:
		case strings.TrimSpace(form.Text) == "":
			err = &statusError{http.StatusBadRequest, "comment text is empty"}
		case utf8.RuneCountInString(form.Text) > maxCommentLen:
			err = &statusError{http.StatusBadRequest, fmt.Sprintf("comment text is over %d characters", maxCommentLen)}
		}
		var id string
		if err == nil {
			_, err = q.pending.update(key, func(p *pendingPR) error {
				last, count := 0, 0
				for i, d := range p.Drafts {
					n := commentNo(d.ID)
					if n == 0 {
						continue
					}
					// an Outdated draft's line is of an older commit: a comment on that line is a new one
					if d.File == f.Filename && d.Line == line && d.Commit == st.HeadSHA && d.OutdatedFrom == "" {
						if d.PostedURL != "" {
							return &statusError{http.StatusConflict, "comment already posted"}
						}
						id = d.ID
						p.Drafts[i].Text, p.Drafts[i].Edited, p.Drafts[i].InReview = form.Text, true, true
						return nil
					}
					last, count = max(last, n), count+1
				}
				if count >= maxLineComments {
					return &statusError{http.StatusConflict, fmt.Sprintf("at most %d line comments per pull request", maxLineComments)}
				}
				id = fmt.Sprintf("c%d", last+1)
				p.Drafts = append(p.Drafts, pendingDraft{ID: id, File: f.Filename, Line: line, Commit: st.HeadSHA, Text: form.Text, Edited: true, InReview: true})
				return nil
			})
		}
		var se *statusError
		switch {
		case errors.As(err, &se):
			form.Error = se.msg
			writeHTML(w, se.code, "comment-form", form)
			return
		case err != nil:
			fmt.Fprintln(os.Stderr, err)
			form.Error = err.Error()
			writeHTML(w, http.StatusInternalServerError, "comment-form", form)
			return
		}
		rep, pend := newReport(pr.Repo, pr.Number, st), q.pending.get(key)
		d := detailsData{Items: checkItems(rep, st, pend), Comments: commentItems(rep, pend, st.HeadSHA)}
		_, d.InReview = pendingCounts(slices.Concat(d.Items, d.Comments))
		d.InReview += len(pend.Replies)
		lookup, _ := lookupItem(rep, st, pend, id)
		writeHTML(w, http.StatusOK, "comment-added", struct {
			Item    checkItem
			Details detailsData
		}{lookup, d})
	})
	// pathThread is the thread of a /pr/{owner}/{name}/{number}/threads/{id} path, after the thread check;
	// it answers the request itself when there is none.
	pathThread := func(w http.ResponseWriter, r *http.Request) (pullRequest, reviewThread, bool) {
		pr, ok := pathPR(r)
		if !ok {
			http.NotFound(w, r)
			return pr, reviewThread{}, false
		}
		t, found, err := threadFor(r.Context(), pr, r.PathValue("id"))
		switch {
		case err != nil: // the error goes in the bar; the block keeps its comments, typed text and buttons
			w.Header().Set("HX-Reswap", "none")
			writeHTML(w, http.StatusBadGateway, "stale", struct{ ID, Error string }{"", "Could not load GitHub discussions: " + err.Error()})
		case !found:
			http.NotFound(w, r)
		}
		return pr, t, err == nil && found
	}
	// POST …/threads/{id}/replies saves your reply to a thread in the pending review (empty text deletes it);
	// it is posted only with the review. Replies belong to threads, not to a result, so there is no stale check.
	mux.HandleFunc("POST /pr/{owner}/{name}/{number}/threads/{id}/replies", func(w http.ResponseWriter, r *http.Request) {
		pr, t, ok := pathThread(w, r)
		if !ok {
			return
		}
		text := r.PostFormValue("text")
		var err error
		switch {
		case !t.CanReply && strings.TrimSpace(text) != "": // deleting stays possible, or a locked thread's reply is stuck
			err = &statusError{http.StatusConflict, "you cannot reply to this conversation"}
		case utf8.RuneCountInString(text) > maxCommentLen:
			err = &statusError{http.StatusBadRequest, fmt.Sprintf("reply text is over %d characters", maxCommentLen)}
		default:
			_, err = q.pending.update(prKey(pr), func(p *pendingPR) error {
				i := slices.IndexFunc(p.Replies, func(x pendingReply) bool { return x.Thread == t.ID })
				switch {
				case strings.TrimSpace(text) == "" && i < 0:
					return errUnchanged
				case strings.TrimSpace(text) == "":
					p.Replies = slices.Delete(p.Replies, i, i+1)
				case i < 0:
					p.Replies = append(p.Replies, pendingReply{Thread: t.ID, Path: t.Path, Line: t.Line, Text: text})
				default:
					p.Replies[i].Text = text
				}
				return nil
			})
		}
		code, msg := http.StatusOK, ""
		var se *statusError
		switch {
		case errors.Is(err, errUnchanged):
		case errors.As(err, &se):
			code, msg = se.code, se.msg
		case err != nil:
			fmt.Fprintln(os.Stderr, err)
			code, msg = http.StatusInternalServerError, err.Error()
		}
		_, _, pend, _, _, drafts := reviewState(pr)
		v := newThreadView(pr.Repo, pr.Number, t, pend)
		if msg != "" {
			v.Error, v.Text = msg, text // the editor keeps what was typed
		}
		writeHTML(w, code, "thread-saved", struct {
			Thread  threadView
			Details detailsData
			Error   string
		}{v, detailsData{InReview: len(drafts) + len(pend.Replies)}, ""})
	})
	// POST …/threads/{id}/resolve resolves (resolved=1) or unresolves (0) a thread on GitHub right away.
	mux.HandleFunc("POST /pr/{owner}/{name}/{number}/threads/{id}/resolve", func(w http.ResponseWriter, r *http.Request) {
		v := r.PostFormValue("resolved")
		if v != "1" && v != "0" {
			http.Error(w, "resolved must be 1 or 0", http.StatusBadRequest)
			return
		}
		pr, t, ok := pathThread(w, r)
		if !ok {
			return
		}
		code, msg := http.StatusOK, ""
		if res, err := resolve(r.Context(), t.ID, v == "1"); err != nil {
			code, msg = http.StatusBadGateway, err.Error()
		} else {
			// whoever may resolve a thread may unresolve it, so the block offers the way back until the next fetch
			t.Resolved, t.CanResolve, t.CanUnresolve = res, true, true
			threadsMu.Lock()
			if tl, ok := threadCache[prKey(pr)]; ok {
				tl.Threads = slices.Clone(tl.Threads) // page renders may still read the old slice
				if i := slices.IndexFunc(tl.Threads, func(x reviewThread) bool { return x.ID == t.ID }); i >= 0 {
					tl.Threads[i] = t
				}
				threadCache[prKey(pr)] = tl
			}
			threadsMu.Unlock()
		}
		pend := q.pending.get(prKey(pr))
		if row := r.PostFormValue("row"); row != "" && msg == "" {
			// diff view: the thread moves between the line's icon and the open threads, so the row's slots re-render
			threadsMu.Lock()
			tl := threadCache[prKey(pr)]
			threadsMu.Unlock()
			dr := diffRow{Key: row, Swap: true}
			for _, id := range strings.Fields(r.PostFormValue("row_threads")) {
				if i := slices.IndexFunc(tl.Threads, func(x reviewThread) bool { return x.ID == id }); i >= 0 {
					dr.Threads = append(dr.Threads, newThreadView(pr.Repo, pr.Number, tl.Threads[i], pend))
				} else if id == t.ID { // cache gone since: the resolved thread at least
					dr.Threads = append(dr.Threads, newThreadView(pr.Repo, pr.Number, t, pend))
				}
			}
			w.Header().Set("HX-Reswap", "none")
			writeHTML(w, code, "row-threads", dr)
			return
		}
		tv := newThreadView(pr.Repo, pr.Number, t, pend)
		tv.Error = msg
		writeHTML(w, code, "thread", tv)
	})
	mux.HandleFunc("POST /rerun", func(w http.ResponseWriter, r *http.Request) {
		// the dashboard's htmx Re-run swaps its own row; errors come back as a row too, swapped by dashboard.js
		inPlace := r.Header.Get("HX-Request") == "true" && r.PostFormValue("from") != "details"
		fail := func(code int, msg string) {
			if !inPlace {
				http.Error(w, msg, code)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(code)
			indexTmpl.ExecuteTemplate(w, "row-error", msg)
		}
		repo := r.PostFormValue("repo")
		number, err := strconv.Atoi(r.PostFormValue("number"))
		if repo == "" || err != nil {
			fail(http.StatusBadRequest, "repo and a numeric number are required")
			return
		}
		key := prKey(pullRequest{Repo: repo, Number: number})
		if st := q.statuses()[key]; st.Status == statusSkipped {
			fail(http.StatusConflict, fmt.Sprintf("pull request is skipped by rule skip[%d]", st.SkipRule))
			return
		}
		// onPoll syncs the queue with the dashboard list, so an unknown key is a PR not on the dashboard.
		if !q.rerun(key) {
			fail(http.StatusNotFound, "pull request not on the dashboard")
			return
		}
		var f dashFilter
		if v, err := url.ParseQuery(r.PostFormValue("filter")); err == nil {
			f = parseFilter(v)
		}
		if inPlace {
			snap := p.snapshot()
			i := slices.IndexFunc(snap.PRs, func(pr pullRequest) bool { return prKey(pr) == key })
			if i < 0 { // closed between the queue lookup and this snapshot
				fail(http.StatusNotFound, "pull request not on the dashboard")
				return
			}
			// the query is re-encoded from parsed fields, never the raw client string; 10 s while the row is queued or running
			d := indexData{Poll: snap, Query: f.query(), Review: q.statuses(), PollEvery: 10, OOB: true}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			indexTmpl.ExecuteTemplate(w, "row", newRow(snap.PRs[i], d))
			indexTmpl.ExecuteTemplate(w, "poll", d)
			return
		}
		to := "/"
		if r.PostFormValue("from") == "details" {
			to = fmt.Sprintf("/pr/%s/%d", repo, number) // repo and number matched a dashboard PR above
		} else if f.active() {
			to = "/?" + f.query() // re-encoded from parsed fields, never the raw client string
		}
		seeOther(w, r, to)
	})
	registerSettings(mux, ch)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("Cache-Control", "no-store") // WebKit caches scheme-handler responses, so polls would never reach us
		// A foreign page that got into the window (main.go sends it back at once) cannot change anything.
		// htmx requests from the app's wails://localhost pages carry no Origin, so absent is allowed.
		if o := r.Header.Get("Origin"); o != "" && o != appOrigin && r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}
