package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// pendingCheck is a checked mark on a "What to check" row.
type pendingCheck struct {
	ID   string `json:"id"`
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// pendingDraft is a comment on a row: the suggested text, edited or not, in the pending review or not.
type pendingDraft struct {
	ID         string `json:"id"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	Commit     string `json:"commit"` // the result's HeadSHA when the draft was made
	SourceText string `json:"source_text"`
	Text       string `json:"text"`
	Edited     bool   `json:"edited"` // touched: a PUT changed the text; never cleared
	InReview   bool   `json:"in_review"`
	PostedURL  string `json:"posted_url"`
	// OutdatedFrom is the commit whose Line the draft was written on, set when reconcile finds the line gone; empty means anchored.
	OutdatedFrom string `json:"outdated_from"`
	Unflagged    bool   `json:"no_longer_flagged"` // a point draft whose finding is gone, kept as a line comment
}

// pendingReply is a reply to a GitHub review thread; Path and Line are the thread's, for the dialog.
type pendingReply struct {
	Thread string `json:"thread"`
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Text   string `json:"text"`
}

// pendingPR is a PR's checked marks and drafts; Rev grows by one on every saved change.
type pendingPR struct {
	Rev     int            `json:"rev"`
	Checked []pendingCheck `json:"checked"`
	Drafts  []pendingDraft `json:"drafts"`
	Replies []pendingReply `json:"replies"` // replies to GitHub threads; reconcile leaves them alone
	Result  string         `json:"result"`  // the resultID the review was last reconciled with
	Rows    int            `json:"rows"`    // that result's number of "What to check" rows
	// OpenedAt is when the details page was first opened after the result OpenedResult in this review cycle; a submit clears both.
	OpenedAt     time.Time `json:"opened_at"`
	OpenedResult string    `json:"opened_result"` // resultID of the result shown when the clock started; "" for none
}

// draftState is the state badge of a draft.
func draftState(d pendingDraft) string {
	switch {
	case d.PostedURL != "":
		return "Posted"
	case d.OutdatedFrom != "":
		return "Outdated"
	case d.InReview && d.Edited:
		return "Draft edited"
	case d.InReview:
		return "Draft"
	case d.Edited:
		return "Edited"
	}
	return "Suggestion"
}

// checkedAt is the index of the mark on it, or -1. A mark applies only while id, file, line and text all match.
func (p pendingPR) checkedAt(it checkItem) int {
	return slices.IndexFunc(p.Checked, func(c pendingCheck) bool {
		return c.ID == it.ID && c.File == it.File && c.Line == it.Line && c.Text == it.Text
	})
}

// draftAt is the index of the draft on it for the result at commit, or -1. Other drafts stay stored but do not apply.
func (p pendingPR) draftAt(it checkItem, commit string) int {
	return slices.IndexFunc(p.Drafts, func(d pendingDraft) bool {
		return d.ID == it.ID && d.File == it.File && d.Line == it.Line && d.SourceText == it.Text && d.Commit == commit
	})
}

// resultID identifies a done or failed result by its content, so an identical re-run keeps the id; "" otherwise.
func resultID(st jobStatus) string {
	if st.Status != statusDone && st.Status != statusFailed {
		return ""
	}
	data, err := json.Marshal(struct {
		Status, HeadSHA, FailedStep, Reason string
		Findings                            []taggedFinding
		Points                              []pointOfInterest
	}{st.Status, st.HeadSHA, st.FailedStep, st.Reason, st.Findings, st.Points})
	if err != nil {
		panic(err) // plain strings, ints and slices of them always marshal
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}

// reconcile carries p over to the result st with rows (checkItems(rep, st, pendingPR{})): touched drafts are
// kept, re-anchored by moved (a draft's line at st.HeadSHA, see lineMap) or marked Outdated, untouched
// suggestions and unmatched checked marks are dropped. It is a no-op when p was already reconciled with st.
func reconcile(p pendingPR, st jobStatus, rows []checkItem, moved func(d pendingDraft) (int, bool)) pendingPR {
	id := resultID(st)
	if p.Result == id {
		return p
	}
	kind := func(id string) string { return id[:min(1, len(id))] } // f or p
	claimed := make([]bool, len(rows))
	applies := make([]bool, len(p.Drafts))
	next := 0
	for i, d := range p.Drafts {
		next = max(next, commentNo(d.ID))
		for r, row := range rows {
			if !claimed[r] && p.draftAt(row, st.HeadSHA) == i {
				claimed[r], applies[i] = true, true
				break
			}
		}
	}
	var drafts []pendingDraft
	for i, d := range p.Drafts {
		if d.PostedURL != "" || applies[i] {
			drafts = append(drafts, d)
			continue
		}
		if commentNo(d.ID) == 0 { // a finding or point draft
			if !d.Edited {
				continue
			}
			line, ok := moved(d)
			match := -1
			for r, row := range rows {
				if claimed[r] || kind(row.ID) != kind(d.ID) || row.File != d.File || row.Text != d.SourceText {
					continue
				}
				if match < 0 || ok && row.Line == line && rows[match].Line != line {
					match = r
				}
			}
			if match >= 0 {
				claimed[match] = true
				d.ID, d.Line, d.Commit = rows[match].ID, rows[match].Line, st.HeadSHA
				drafts = append(drafts, d)
				continue
			}
			next++
			d.ID, d.Unflagged = fmt.Sprintf("c%d", next), true
		}
		if d.Commit != st.HeadSHA && d.OutdatedFrom == "" { // an Outdated draft stays Outdated
			if line, ok := moved(d); ok {
				d.Line = line
			} else {
				d.OutdatedFrom = d.Commit
			}
		}
		d.Commit = st.HeadSHA
		drafts = append(drafts, d)
	}
	var checked []pendingCheck
	marked := make([]bool, len(rows))
	for _, c := range p.Checked {
		for r, row := range rows {
			if !marked[r] && row.File == c.File && row.Line == c.Line && row.Text == c.Text {
				marked[r] = true
				c.ID = row.ID
				checked = append(checked, c)
				break
			}
		}
	}
	p.Checked, p.Drafts, p.Result, p.Rows = checked, drafts, id, len(rows)
	return p
}

const (
	maxLineMapFiles = 50
	lineMapTimeout  = 20 * time.Second
)

// lineMap reads, with content, the files of the drafts reconcile may re-anchor and returns its moved func:
// a draft's line at newSHA, found as the same text at the same line, else as the only line with that text.
// Not ok means the line is gone or could not be checked (unsafe path, read error, missing or binary file,
// over maxLineMapFiles files, or a draft not among drafts).
func lineMap(ctx context.Context, content func(ctx context.Context, repo, sha, path string) ([]byte, error),
	repo string, drafts []pendingDraft, newSHA string) func(d pendingDraft) (int, bool) {
	ctx, cancel := context.WithTimeout(ctx, lineMapTimeout)
	defer cancel()
	type fileAt struct{ path, sha string }
	type lineAt struct {
		path, sha string
		line      int
	}
	files := map[fileAt][]string{} // nil: not readable
	read := func(path, sha string) []string {
		k := fileAt{path, sha}
		if lines, ok := files[k]; ok {
			return lines
		}
		var lines []string
		if b, err := content(ctx, repo, sha, path); err == nil {
			lines = strings.Split(string(b), "\n")
		}
		files[k] = lines
		return lines
	}
	paths := map[string]bool{}
	found := map[lineAt]int{}
	for _, d := range drafts {
		// only drafts reconcile asks about: unposted, anchored, touched or line comments, of another commit
		if d.PostedURL != "" || d.OutdatedFrom != "" || !d.Edited && commentNo(d.ID) == 0 || d.Commit == newSHA || !validRepoPath(d.File) {
			continue
		}
		if !paths[d.File] && len(paths) >= maxLineMapFiles {
			continue
		}
		paths[d.File] = true
		k := lineAt{d.File, d.Commit, d.Line}
		if _, done := found[k]; done {
			continue
		}
		found[k] = -1
		cur := read(d.File, newSHA)
		if cur == nil {
			continue
		}
		if d.Line == 0 {
			found[k] = 0
			continue
		}
		old := read(d.File, d.Commit)
		if d.Line > len(old) {
			continue
		}
		t := old[d.Line-1]
		if d.Line <= len(cur) && cur[d.Line-1] == t {
			found[k] = d.Line
			continue
		}
		at := -1
		for i, l := range cur {
			if l == t {
				if at >= 0 {
					at = -1
					break
				}
				at = i + 1
			}
		}
		found[k] = at
	}
	return func(d pendingDraft) (int, bool) {
		if d.Commit == newSHA {
			return d.Line, true
		}
		if line, ok := found[lineAt{d.File, d.Commit, d.Line}]; ok && line >= 0 {
			return line, true
		}
		return 0, false
	}
}

// statusError is a refused change; code is the HTTP status the handler answers with.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

// pendingStore keeps one JSON file per PR under <data dir>/pending, loaded lazily and cached.
// It has its own lock; it never takes the queue's.
type pendingStore struct {
	store resultStore // writes go through store.write, so temp files land in the data dir and openStore cleans them

	mu  sync.Mutex
	prs map[string]pendingPR // by prKey
}

func newPendingStore(store resultStore) *pendingStore {
	return &pendingStore{store: store, prs: map[string]pendingPR{}}
}

func (s *pendingStore) dir() string { return filepath.Join(s.store.dir, "pending") }

func (s *pendingStore) file(key string) string {
	return filepath.Join(s.dir(), url.PathEscape(key)+".json")
}

// load needs s.mu held. A file that cannot be read or parsed is logged and the PR starts empty.
func (s *pendingStore) load(key string) pendingPR {
	if p, ok := s.prs[key]; ok {
		return p
	}
	var p pendingPR
	data, err := os.ReadFile(s.file(key))
	if err == nil {
		err = json.Unmarshal(data, &p)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "ignoring pending review", s.file(key)+":", err)
		p = pendingPR{}
	}
	s.prs[key] = p
	return p
}

// get returns the PR's state; callers must not modify its slices.
func (s *pendingStore) get(key string) pendingPR {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(key)
}

// update applies fn to a copy of the PR's state and, unless fn fails, saves it with Rev+1.
// The cached state changes only after the file is written, so a failed write leaves nothing behind.
func (s *pendingStore) update(key string, fn func(*pendingPR) error) (pendingPR, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.load(key)
	p.Checked, p.Drafts, p.Replies = slices.Clone(p.Checked), slices.Clone(p.Drafts), slices.Clone(p.Replies)
	if err := fn(&p); err != nil {
		return pendingPR{}, err
	}
	p.Rev++
	if err := s.store.write(s.file(key), p); err != nil {
		return pendingPR{}, fmt.Errorf("saving pending review: %w", err)
	}
	s.prs[key] = p
	return p, nil
}

// prune deletes the files and cached state of repo's PRs whose key is not in open.
func (s *pendingStore) prune(repo string, open map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resultStore{dir: s.dir()}.prune(repo, open)
	for k := range s.prs {
		if strings.HasPrefix(k, repo+"#") && !open[k] {
			delete(s.prs, k)
		}
	}
}
