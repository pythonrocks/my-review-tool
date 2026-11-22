package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	filesPerPage    = 100
	maxFilePages    = 3 // so at most 300 files
	maxPatchBytes   = 128 << 10
	maxDiffBytes    = 2 << 20
	maxContentBytes = 1 << 20
	maxCachedDiffs  = 20
	maxContentReads = 8 // concurrent content calls of one page
)

// contentTimeout bounds the content calls of one diff page; a var so tests can shorten it.
var contentTimeout = 5 * time.Second

var (
	// errDiffOutdated means GitHub answered for another commit than the reviewed one: the PR moved.
	errDiffOutdated = errors.New("the pull request moved to another commit")
	// errNotAvailable is file content that is missing, binary or over maxContentBytes.
	errNotAvailable = errors.New("not available")
)

// prFile is one entry of GitHub's "list pull request files"; Patch is empty when GitHub omits it.
type prFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
	Status           string `json:"status"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	Patch            string `json:"patch"`
	ContentsURL      string `json:"contents_url"`
}

// diffLine is one patch line; Old or New is 0 on the side the line is not on.
type diffLine struct {
	Kind     string // "context", "add" or "del"
	Old, New int
	Text     string
}

// hunk is one `@@ -OldStart,OldCount +NewStart,NewCount @@` block.
type hunk struct {
	OldStart, OldCount, NewStart, NewCount int
	Lines                                  []diffLine
}

// newFirst and newEnd are the first and last new-side line of h. A hunk with no new lines sits after
// line NewStart, so newEnd is NewStart and newFirst NewStart+1.
func (h hunk) newFirst() int {
	if h.NewCount == 0 {
		return h.NewStart + 1
	}
	return h.NewStart
}

func (h hunk) newEnd() int { return h.newFirst() + h.NewCount - 1 }

// diffFile is a file of the PR with its parsed hunks; Note says why there are none ("" when there are).
type diffFile struct {
	prFile
	Hunks []hunk
	Note  string
}

// prDiff is the files of a PR at one commit; Truncated means GitHub may have more than were fetched.
type prDiff struct {
	Files     []diffFile
	Truncated bool
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// parsePatch parses a GitHub file patch into hunks, numbering every line on its side(s).
func parsePatch(patch string) ([]hunk, error) {
	var hunks []hunk
	var oldN, newN, oldLeft, newLeft int
	lines := strings.Split(strings.TrimSuffix(patch, "\n"), "\n")
	for _, l := range lines {
		if strings.HasPrefix(l, "@@") {
			if oldLeft != 0 || newLeft != 0 {
				return nil, fmt.Errorf("hunk %d does not match its header", len(hunks))
			}
			m := hunkHeader.FindStringSubmatch(l)
			if m == nil {
				return nil, fmt.Errorf("malformed hunk header %q", cutRunes(l, 80))
			}
			var n [4]int
			for i, s := range m[1:] {
				if s == "" {
					n[i] = 1 // an omitted count is 1
					continue
				}
				v, err := strconv.Atoi(s)
				if err != nil {
					return nil, fmt.Errorf("malformed hunk header %q", cutRunes(l, 80))
				}
				n[i] = v
			}
			hunks = append(hunks, hunk{OldStart: n[0], OldCount: n[1], NewStart: n[2], NewCount: n[3]})
			oldN, oldLeft, newN, newLeft = n[0], n[1], n[2], n[3]
			continue
		}
		if len(hunks) == 0 {
			return nil, errors.New("patch does not start with a hunk header")
		}
		if strings.HasPrefix(l, `\`) { // "\ No newline at end of file"
			continue
		}
		h := &hunks[len(hunks)-1]
		kind, text := " ", ""
		if l != "" {
			kind, text = l[:1], l[1:]
		}
		switch kind {
		case " ":
			h.Lines = append(h.Lines, diffLine{Kind: "context", Old: oldN, New: newN, Text: text})
			oldN, newN, oldLeft, newLeft = oldN+1, newN+1, oldLeft-1, newLeft-1
		case "+":
			h.Lines = append(h.Lines, diffLine{Kind: "add", New: newN, Text: text})
			newN, newLeft = newN+1, newLeft-1
		case "-":
			h.Lines = append(h.Lines, diffLine{Kind: "del", Old: oldN, Text: text})
			oldN, oldLeft = oldN+1, oldLeft-1
		default:
			return nil, fmt.Errorf("unexpected patch line %q", cutRunes(l, 80))
		}
		if oldLeft < 0 || newLeft < 0 {
			return nil, fmt.Errorf("hunk %d does not match its header", len(hunks))
		}
	}
	if len(hunks) == 0 {
		return nil, errors.New("patch has no hunks")
	}
	if oldLeft != 0 || newLeft != 0 {
		return nil, fmt.Errorf("hunk %d does not match its header", len(hunks))
	}
	return hunks, nil
}

// buildDiff checks that files are of commit sha and parses their patches within the size limits.
func buildDiff(files []prFile, sha string) (prDiff, error) {
	d := prDiff{Truncated: len(files) >= filesPerPage*maxFilePages}
	total, over := 0, false
	for _, f := range files {
		u, err := url.Parse(f.ContentsURL)
		// GitHub points a removed file's contents_url at the base commit, so only existing files prove the commit.
		if err != nil || (f.Status != "removed" && u.Query().Get("ref") != sha) {
			return prDiff{}, errDiffOutdated
		}
		df := diffFile{prFile: f}
		switch {
		case f.Patch == "" && f.Status == "renamed" && f.Additions == 0 && f.Deletions == 0:
			df.Note = "Renamed without changes"
		case f.Patch == "":
			df.Note = "Diff not available (binary or too large on GitHub)"
		case len(f.Patch) > maxPatchBytes:
			df.Note = "Diff not available (over 128 KiB)"
		case over || total+len(f.Patch) > maxDiffBytes:
			over = true
			df.Note = "Diff not available (diff too large)"
		default:
			total += len(f.Patch)
			if df.Hunks, err = parsePatch(f.Patch); err != nil {
				df.Note = "Diff not available (could not parse)"
			}
		}
		df.Patch = "" // the hunks hold it; the cache keeps no second copy
		d.Files = append(d.Files, df)
	}
	return d, nil
}

// filesWith returns a files func that lists a PR's files with gh through run, at most maxFilePages pages.
func filesWith(run func(ctx context.Context, args ...string) ([]byte, error)) func(context.Context, string, int) ([]prFile, error) {
	return func(ctx context.Context, repo string, number int) ([]prFile, error) {
		var all []prFile
		for p := 1; p <= maxFilePages; p++ {
			out, err := run(ctx, "api", fmt.Sprintf("repos/%s/pulls/%d/files?per_page=%d&page=%d", repo, number, filesPerPage, p))
			if err != nil {
				return nil, err
			}
			var page []prFile
			if err := json.Unmarshal(out, &page); err != nil {
				return nil, fmt.Errorf("parse gh output: %w", err)
			}
			all = append(all, page...)
			if len(page) < filesPerPage {
				break
			}
		}
		return all, nil
	}
}

// ghFiles lists the files of a pull request with the gh CLI.
var ghFiles = filesWith(runGh)

// contentWith returns a content func that reads a file at commit sha with gh through run.
// path must be validated or come from a stored result.
func contentWith(run func(ctx context.Context, args ...string) ([]byte, error)) func(context.Context, string, string, string) ([]byte, error) {
	return func(ctx context.Context, repo, sha, path string) ([]byte, error) {
		// shortcut: one gh call per click, cache if Expand feels slow
		out, err := run(ctx, "api", "-H", "Accept: application/vnd.github.raw+json",
			fmt.Sprintf("repos/%s/contents/%s?ref=%s", repo, pathEscape(path), url.QueryEscape(sha)))
		if err != nil {
			return nil, err
		}
		if len(out) > maxContentBytes || bytes.IndexByte(out, 0) >= 0 {
			return nil, errNotAvailable
		}
		return out, nil
	}
}

// ghContent reads a file of a repository at a commit with the gh CLI.
var ghContent = contentWith(runGh)

// diffCache keeps the diff of the last fetched commit of at most maxCachedDiffs PRs.
type diffCache struct {
	files   func(ctx context.Context, repo string, number int) ([]prFile, error)
	mu      sync.Mutex
	seq     int
	entries map[string]*diffEntry // by prKey
}

type diffEntry struct {
	sha  string
	seq  int           // fetch order, for eviction
	done chan struct{} // closed when diff and err are set
	diff prDiff
	err  error
	// texts are the file texts read for this diff by path; nil is errNotAvailable. Guarded by diffCache.mu.
	// shortcut: whole texts cached (≤1 MiB/file, 20 PRs); keep only needed lines if memory shows up.
	texts map[string][]byte
}

func newDiffCache(files func(ctx context.Context, repo string, number int) ([]prFile, error)) *diffCache {
	return &diffCache{files: files, entries: map[string]*diffEntry{}}
}

// get returns the diff of PR repo#number at sha, sharing one fetch among concurrent callers. Errors are not kept.
func (c *diffCache) get(ctx context.Context, repo string, number int, sha string) (prDiff, error) {
	key := prKey(pullRequest{Repo: repo, Number: number})
	c.mu.Lock()
	e := c.entries[key]
	if e == nil || e.sha != sha {
		c.seq++
		e = &diffEntry{sha: sha, seq: c.seq, done: make(chan struct{})}
		c.entries[key] = e
		if len(c.entries) > maxCachedDiffs {
			oldest := ""
			for k, v := range c.entries {
				if oldest == "" || v.seq < c.entries[oldest].seq {
					oldest = k
				}
			}
			delete(c.entries, oldest)
		}
		// The fetch outlives the request that started it, so other waiters are not failed by its cancel; gh has its own timeout.
		go func(ctx context.Context) {
			files, err := c.files(ctx, repo, number)
			var d prDiff
			if err == nil {
				d, err = buildDiff(files, sha)
			}
			c.mu.Lock()
			e.diff, e.err = d, err
			if err != nil && c.entries[key] == e {
				delete(c.entries, key)
			}
			c.mu.Unlock()
			close(e.done)
		}(context.WithoutCancel(ctx))
	}
	c.mu.Unlock()
	select {
	case <-e.done:
		return e.diff, e.err
	case <-ctx.Done():
		return prDiff{}, ctx.Err()
	}
}

// texts reads paths of PR repo#number at sha with content, at most maxContentReads at a time and within
// contentTimeout, and keeps texts and errNotAvailable on the diff's cache entry. A path that failed or timed out is
// missing from the result; one not available maps to nil. paths must be validated.
func (c *diffCache) texts(ctx context.Context, repo string, number int, sha string, paths []string,
	content func(ctx context.Context, repo, sha, path string) ([]byte, error)) map[string][]byte {
	out := map[string][]byte{}
	var todo []string
	c.mu.Lock()
	e := c.entries[prKey(pullRequest{Repo: repo, Number: number})]
	if e != nil && e.sha != sha {
		e = nil
	}
	for _, p := range paths {
		if t, ok := e.cached(p); ok {
			out[p] = t
		} else {
			todo = append(todo, p)
		}
	}
	c.mu.Unlock()
	if len(todo) == 0 {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, contentTimeout)
	defer cancel()
	type result struct {
		path string
		text []byte
		ok   bool
	}
	results := make(chan result, len(todo)) // buffered: a read that ends after the timeout does not block
	sem := make(chan struct{}, maxContentReads)
	for _, p := range todo {
		go func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results <- result{path: p}
				return
			}
			defer func() { <-sem }()
			if ctx.Err() != nil { // select picks at random when both are ready
				results <- result{path: p}
				return
			}
			t, err := content(ctx, repo, sha, p)
			if err != nil {
				t = nil
			}
			results <- result{p, t, err == nil || errors.Is(err, errNotAvailable)}
		}()
	}
	for range todo {
		select {
		case r := <-results:
			if r.ok {
				out[r.path] = r.text
				c.mu.Lock()
				if e != nil {
					if e.texts == nil {
						e.texts = map[string][]byte{}
					}
					e.texts[r.path] = r.text
				}
				c.mu.Unlock()
			}
		case <-ctx.Done():
			return out
		}
	}
	return out
}

// cached is the text of path kept on e, if any; e may be nil.
func (e *diffEntry) cached(path string) ([]byte, bool) {
	if e == nil {
		return nil, false
	}
	t, ok := e.texts[path]
	return t, ok
}

// findFile returns the file of d named path, or a 404.
func findFile(d prDiff, path string) (diffFile, error) {
	i := slices.IndexFunc(d.Files, func(f diffFile) bool { return f.Filename == path })
	if i < 0 {
		return diffFile{}, &statusError{http.StatusNotFound, "file not in this pull request"}
	}
	return d.Files[i], nil
}

// newSideFile is findFile for a file with new-side diff lines: not removed, with parsed hunks.
func newSideFile(d prDiff, path string) (diffFile, error) {
	f, err := findFile(d, path)
	if err == nil && (f.Status == "removed" || len(f.Hunks) == 0) {
		err = &statusError{http.StatusNotFound, "file has no new-side lines in this pull request"}
	}
	return f, err
}

var canonicalNumber = regexp.MustCompile(`^[1-9][0-9]{0,6}$`)

// parseLineNumber parses a canonical positive decimal of at most 7 digits; zeroOK also accepts "0".
func parseLineNumber(name, s string, zeroOK bool) (int, error) {
	if !canonicalNumber.MatchString(s) && !(zeroOK && s == "0") {
		return 0, &statusError{http.StatusBadRequest, name + " must be a positive number of at most 7 digits"}
	}
	n, _ := strconv.Atoi(s)
	return n, nil
}

// checkLine parses s as a new-side line (context or add) inside a hunk of f.
func checkLine(f diffFile, s string) (int, error) {
	n, err := parseLineNumber("line", s, false)
	if err != nil {
		return 0, err
	}
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.New == n && l.Kind != "del" {
				return n, nil
			}
		}
	}
	return 0, &statusError{http.StatusBadRequest, fmt.Sprintf("line %d is not in the diff of %s", n, f.Filename)}
}

// gaps are the new-side line ranges of f outside its hunks; to 0 means up to the end of the file.
func gaps(f diffFile) [][2]int {
	if len(f.Hunks) == 0 {
		return nil
	}
	var out [][2]int
	if first := f.Hunks[0].newFirst(); first > 1 {
		out = append(out, [2]int{1, first - 1})
	}
	for i := 1; i < len(f.Hunks); i++ {
		from, to := f.Hunks[i-1].newEnd()+1, f.Hunks[i].newFirst()-1
		if from <= to {
			out = append(out, [2]int{from, to})
		}
	}
	if f.Status != "added" { // an added file ends with its only hunk
		out = append(out, [2]int{f.Hunks[len(f.Hunks)-1].newEnd() + 1, 0})
	}
	return out
}

// gapOf returns the gap holding new-side line n of f, a file with hunks that is not added or removed.
func gapOf(f diffFile, n int) ([2]int, bool) {
	if f.Status == "added" || f.Status == "removed" || n < 1 {
		return [2]int{}, false
	}
	for _, g := range gaps(f) {
		if n >= g[0] && (g[1] == 0 || n <= g[1]) {
			return g, true
		}
	}
	return [2]int{}, false
}

// checkGap parses from and to, which must be lines inside one gap of f; to 0 (end of file) only in the trailing gap.
func checkGap(f diffFile, fromS, toS string) (from, to int, err error) {
	if from, err = parseLineNumber("from", fromS, false); err != nil {
		return 0, 0, err
	}
	if to, err = parseLineNumber("to", toS, true); err != nil {
		return 0, 0, err
	}
	g, ok := gapOf(f, from)
	if !ok || to == 0 && g[1] != 0 || to != 0 && (to < from || g[1] != 0 && to > g[1]) {
		return 0, 0, &statusError{http.StatusBadRequest, fmt.Sprintf("lines %d..%d are not a gap in the diff of %s", from, to, f.Filename)}
	}
	return from, to, nil
}

// validRepoPath reports whether p (agent output) is a safe relative file path for a GitHub API path.
func validRepoPath(p string) bool {
	if p == "" || len(p) > 1024 || !utf8.ValidString(p) || strings.ContainsFunc(p, unicode.IsControl) || strings.Contains(p, `\`) {
		return false
	}
	for _, seg := range strings.Split(p, "/") { // a leading "/" gives an empty first segment
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// oldFirst is the first old-side line of h, as newFirst is for the new side.
func (h hunk) oldFirst() int {
	if h.OldCount == 0 {
		return h.OldStart + 1
	}
	return h.OldStart
}

// diffView is the diff page: one alert instead of the diff, or the files with their rows and the points index.
type diffView struct {
	Alert     string
	Files     []fileView
	Points    []checkItem
	Other     []otherFile // the files of items not in the diff (not in the PR, or past the truncation), in first-item order
	OtherN    int         // the number of items in Other
	Truncated bool
}

// otherFile is a file not in the diff: its items with no line in Texts in Outside, the others on Unchanged Rows.
type otherFile struct {
	Path    string
	Outside []checkItem
	Rows    []diffRow
}

// fileView is a file of the diff with its rows; PathQ is its query-escaped path for the hx-get URLs.
type fileView struct {
	diffFile
	Index, Points int
	PathQ         string
	Rows          []diffRow
	Threads       []threadView // GitHub threads with no line in the diff: outdated, file-level or unmatched
	Outside       []checkItem  // the file's items not on a new-side diff line, shown at the head of the file
}

// diffRow is a gap (From set; To 0 means to the end of the file) or a line with the items marked on it: a patch line,
// or an Unchanged one, a context line shown around an item outside the diff (Label: the first line of its range),
// your line comment on it, and Mine, the id of the block the comment mark links to ("" for no mark); Threads are the
// GitHub threads on the line; Key ("<file index>-<row index>") names the row's thread slots, and Swap renders them
// as htmx out-of-band swaps (the resolve response).
type diffRow struct {
	From, To         int
	Line             diffLine
	Items            []checkItem
	Comments         []checkItem
	Mine             string
	Threads          []threadView
	Key              string
	Swap             bool
	Unchanged, Label bool
}

// ResolvedComments is the number of comments in the row's resolved threads.
func (r diffRow) ResolvedComments() int {
	n := 0
	for _, t := range r.Threads {
		if t.T.Resolved {
			n += len(t.T.Comments) + t.T.More
		}
	}
	return n
}

// Vals is the hx-vals JSON of the row's thread slots: a resolve inside them re-renders the row's threads.
func (r diffRow) Vals() string {
	ids := make([]string, len(r.Threads))
	for i, t := range r.Threads {
		ids[i] = t.T.ID
	}
	b, _ := json.Marshal(map[string]string{"row": r.Key, "row_threads": strings.Join(ids, " ")})
	return string(b)
}

// newDiffView lays out d with items and your line comments on their new-side lines, and threads on their side's line
// or at the head of their file (threads of files not in d are dropped). Every item gets one place: its line, the head
// of its file, or Other. With its file's text in texts (by path; nil or missing: not available), an item on a line
// of a gap, or of a file not in d, is shown on that line with lineContext lines around it, marked Unchanged.
func newDiffView(d prDiff, items, comments []checkItem, threads []threadView, texts map[string][]byte) diffView {
	v := diffView{Points: items, Truncated: d.Truncated}
	type at struct {
		file string
		line int
	}
	onLine := map[at][]checkItem{}
	files := map[string]diffFile{}
	for _, f := range d.Files {
		files[f.Filename] = f
		if f.Status == "removed" {
			continue
		}
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				if l.Kind != "del" {
					onLine[at{f.Filename, l.New}] = nil
				}
			}
		}
	}
	lines := map[string][]string{}
	for p, t := range texts {
		if t != nil {
			lines[p] = textLines(t)
		}
	}
	unchanged := map[string][]int{} // the lines of items on unchanged lines, by path
	outside := map[string][]checkItem{}
	otherAt := map[string]int{} // index in v.Other by path
	for _, it := range items {
		k := at{it.File, it.Line}
		f, inPR := files[it.File]
		_, inGap := gapOf(f, it.Line)
		inText := it.Line > 0 && it.Line <= len(lines[it.File])
		marks, inDiff := onLine[k]
		switch {
		case inDiff && it.Line > 0 && !inGap:
			onLine[k] = append(marks, it)
		case inText && (inGap || !inPR):
			onLine[k] = append(marks, it)
			unchanged[it.File] = append(unchanged[it.File], it.Line)
		case inPR:
			outside[it.File] = append(outside[it.File], it)
		}
		if !inPR {
			i, ok := otherAt[it.File]
			if !ok {
				i = len(v.Other)
				otherAt[it.File] = i
				v.Other = append(v.Other, otherFile{Path: it.File})
			}
			if !inText {
				v.Other[i].Outside = append(v.Other[i].Outside, it)
			}
			v.OtherN++
		}
	}
	// rangeRows are the rows of one range of unchanged lines.
	rangeRows := func(path string, ls []diffLine) []diffRow {
		var rows []diffRow
		for i, l := range ls {
			r := diffRow{Line: l, Unchanged: true, Label: i == 0, Items: onLine[at{path, l.New}]}
			if j := slices.IndexFunc(r.Items, func(it checkItem) bool { return it.Draft.InReview }); j >= 0 {
				r.Mine = r.Items[j].ID
			}
			rows = append(rows, r)
		}
		return rows
	}
	for i, o := range v.Other {
		ls := lines[o.Path]
		for _, rg := range lineRanges(unchanged[o.Path], 1, len(ls)) {
			var dl []diffLine
			for n := rg[0]; n <= rg[1]; n++ {
				dl = append(dl, diffLine{Kind: "context", Old: n, New: n, Text: ls[n-1]}) // the PR did not change the file
			}
			v.Other[i].Rows = append(v.Other[i].Rows, rangeRows(o.Path, dl)...)
		}
	}
	// gapRows are the rows of gap g of f: its ranges of unchanged lines, and gap rows for the pieces between them.
	gapRows := func(f diffFile, g [2]int) []diffRow {
		ls := lines[f.Filename]
		end := g[1]
		if end == 0 {
			end = len(ls)
		}
		var rows []diffRow
		next := g[0]
		for _, rg := range lineRanges(unchanged[f.Filename], g[0], end) {
			if rg[0] > next {
				rows = append(rows, diffRow{From: next, To: rg[0] - 1})
			}
			rows = append(rows, rangeRows(f.Filename, expandRows(f, rg[0], rg[1], ls))...)
			next = rg[1] + 1
		}
		if g[1] != 0 && next <= g[1] || g[1] == 0 && (next == g[0] || next <= len(ls)) {
			rows = append(rows, diffRow{From: next, To: g[1]})
		}
		return rows
	}
	mine := map[at][]checkItem{}
	for _, c := range comments {
		if c.Draft.OutdatedFrom != "" {
			continue // its line is of an older commit
		}
		mine[at{c.File, c.Line}] = append(mine[at{c.File, c.Line}], c)
	}
	for i, f := range d.Files {
		fv := fileView{diffFile: f, Index: i, PathQ: url.QueryEscape(f.Filename), Outside: outside[f.Filename]}
		for _, it := range items {
			if it.File == f.Filename {
				fv.Points++
			}
		}
		var gs [][2]int
		if f.Status != "added" && f.Status != "removed" {
			gs = gaps(f)
		}
		for _, h := range f.Hunks {
			if len(gs) > 0 && gs[0][1] != 0 && gs[0][1] < h.newFirst() {
				fv.Rows = append(fv.Rows, gapRows(f, gs[0])...)
				gs = gs[1:]
			}
			for _, l := range h.Lines {
				r := diffRow{Line: l}
				if l.Kind != "del" && f.Status != "removed" {
					r.Items, r.Comments = onLine[at{f.Filename, l.New}], mine[at{f.Filename, l.New}]
					if len(r.Comments) > 0 {
						r.Mine = r.Comments[0].ID
					} else if i := slices.IndexFunc(r.Items, func(it checkItem) bool { return it.Draft.InReview }); i >= 0 {
						r.Mine = r.Items[i].ID
					}
				}
				fv.Rows = append(fv.Rows, r)
			}
		}
		for _, g := range gs { // the trailing gap
			fv.Rows = append(fv.Rows, gapRows(f, g)...)
		}
		for _, tv := range threads {
			t := tv.T
			if t.Path != f.Filename {
				continue
			}
			i := -1
			if t.Line > 0 && !t.Outdated { // gap rows have Line.New and Line.Old 0, so they never match
				i = slices.IndexFunc(fv.Rows, func(r diffRow) bool {
					if r.Unchanged { // GitHub threads stay at the head of the file
						return false
					}
					if t.Side == "LEFT" {
						return r.Line.Kind != "add" && r.Line.Old == t.Line
					}
					return r.Line.Kind != "del" && r.Line.New == t.Line
				})
			}
			if i < 0 {
				fv.Threads = append(fv.Threads, tv)
				continue
			}
			fv.Rows[i].Threads = append(fv.Rows[i].Threads, tv)
			fv.Rows[i].Key = fmt.Sprintf("%d-%d", fv.Index, i)
		}
		v.Files = append(v.Files, fv)
	}
	return v
}

// contentPaths are the paths whose text newDiffView needs: those of items on a gap line of a file of d, or in a file
// not in d. Item paths are agent output: invalid ones are left out.
func contentPaths(d prDiff, items []checkItem) []string {
	var out []string
	for _, it := range items {
		if it.Line < 1 || !validRepoPath(it.File) || slices.Contains(out, it.File) {
			continue
		}
		f, err := findFile(d, it.File)
		if _, inGap := gapOf(f, it.Line); err != nil || inGap {
			out = append(out, it.File)
		}
	}
	return out
}

// lineContext is the number of lines shown before and after the line of an item outside the diff.
const lineContext = 3

// lineRanges are the ranges of lines ns ± lineContext, clamped to lo..hi, with overlapping or touching ones merged;
// lines outside lo..hi are ignored.
func lineRanges(ns []int, lo, hi int) [][2]int {
	ns = slices.Sorted(slices.Values(ns))
	var out [][2]int
	for _, n := range ns {
		if n < lo || n > hi {
			continue
		}
		from, to := max(lo, n-lineContext), min(hi, n+lineContext)
		if k := len(out) - 1; k >= 0 && from <= out[k][1]+1 {
			out[k][1] = max(out[k][1], to)
			continue
		}
		out = append(out, [2]int{from, to})
	}
	return out
}

// textLines splits a file text into its lines.
func textLines(text []byte) []string {
	if len(text) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(text), "\n"), "\n")
}

// expandRows returns the context lines from..to (to 0: end of file) of a gap of f from the file's lines, clamped to its length.
// Old numbers keep the offset of the next hunk, or that after the last hunk for the trailing gap.
func expandRows(f diffFile, from, to int, lines []string) []diffLine {
	if to == 0 || to > len(lines) {
		to = len(lines)
	}
	last := f.Hunks[len(f.Hunks)-1]
	offset := last.oldFirst() - last.newFirst() + last.OldCount - last.NewCount
	for _, h := range f.Hunks {
		if h.newFirst() > from {
			offset = h.oldFirst() - h.newFirst()
			break
		}
	}
	var out []diffLine
	for n := from; n <= to; n++ {
		out = append(out, diffLine{Kind: "context", Old: n + offset, New: n, Text: lines[n-1]})
	}
	return out
}

// excerpt is lines line-3..line+3 of text, clamped to the file, as context lines with line itself of Kind "focus";
// nil when line is not in the file.
func excerpt(text []byte, line int) []diffLine {
	lines := textLines(text)
	if line < 1 || line > len(lines) {
		return nil
	}
	var out []diffLine
	for n := max(1, line-3); n <= min(len(lines), line+3); n++ {
		kind := "context"
		if n == line {
			kind = "focus"
		}
		out = append(out, diffLine{Kind: kind, New: n, Text: lines[n-1]})
	}
	return out
}
