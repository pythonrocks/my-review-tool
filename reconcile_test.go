package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestResultID(t *testing.T) {
	done := jobStatus{Status: statusDone, HeadSHA: "h1", Summary: "s",
		Findings: []taggedFinding{{Step: "code", finding: finding{Severity: "high", File: "a.go", Line: 3, Description: "d"}}}}
	done.Points = []pointOfInterest{{File: "a.go", Line: 3, Reason: "r"}}
	id := resultID(done)
	if len(id) != 16 || strings.Trim(id, "0123456789abcdef") != "" {
		t.Fatalf("resultID = %q, want 16 hex characters", id)
	}
	same := done
	same.Summary = "other summary" // not part of the id
	if resultID(same) != id {
		t.Error("identical result got another id")
	}
	changed := done
	changed.Findings = []taggedFinding{{Step: "code", finding: finding{Severity: "high", File: "a.go", Line: 3, Description: "d2"}}}
	if resultID(changed) == id {
		t.Error("changed findings kept the id")
	}
	failed := jobStatus{Status: statusFailed, HeadSHA: "h1", FailedStep: "code", Reason: "boom"}
	if resultID(failed) == "" || resultID(failed) == id {
		t.Errorf("failed result id %q", resultID(failed))
	}
	for _, s := range []string{"queued", "running", ""} {
		if got := resultID(jobStatus{Status: s, HeadSHA: "h1"}); got != "" {
			t.Errorf("resultID(%q) = %q, want empty", s, got)
		}
	}
}

func TestDraftStateOutdated(t *testing.T) {
	if got := draftState(pendingDraft{OutdatedFrom: "h0", InReview: true, Edited: true}); got != "Outdated" {
		t.Errorf("draftState = %q, want Outdated", got)
	}
	if got := draftState(pendingDraft{OutdatedFrom: "h0", PostedURL: "u"}); got != "Posted" {
		t.Errorf("posted outdated draft: %q, want Posted", got)
	}
}

func TestPendingOldFileLoads(t *testing.T) {
	old := `{"rev":2,"checked":[{"id":"f0","file":"a.go","line":3,"text":"d"}],"drafts":[{"id":"c1","file":"a.go","line":4,"commit":"h1","source_text":"","text":"x","edited":true,"in_review":true,"posted_url":""}]}`
	var p pendingPR
	if err := json.Unmarshal([]byte(old), &p); err != nil {
		t.Fatal(err)
	}
	want := pendingPR{Rev: 2, Checked: []pendingCheck{{"f0", "a.go", 3, "d"}},
		Drafts: []pendingDraft{{ID: "c1", File: "a.go", Line: 4, Commit: "h1", Text: "x", Edited: true, InReview: true}}}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("got %+v, want %+v", p, want)
	}
}

func TestReconcile(t *testing.T) {
	st := jobStatus{Status: statusDone, HeadSHA: "h2"}
	id := resultID(st)
	rows := []checkItem{
		{ID: "f0", File: "a.go", Line: 12, Text: "nil check"},
		{ID: "p0", File: "b.go", Line: 5, Text: "risky"},
		{ID: "p1", File: "b.go", Line: 9, Text: "risky"},
	}
	// moved: line 10 of a.go at h1 is now 12; line 7 of b.go is now 9; everything else is gone
	moved := func(d pendingDraft) (int, bool) {
		if d.Commit == "h2" {
			return d.Line, true
		}
		switch {
		case d.File == "a.go" && d.Line == 10:
			return 12, true
		case d.File == "b.go" && d.Line == 7:
			return 9, true
		case d.File == "c.go" && d.Line == 1:
			return 2, true
		}
		return 0, false
	}
	for _, c := range []struct {
		name string
		in   pendingPR
		want pendingPR // Result and Rows are set by the test
	}{
		{"untouched replaced, also in review",
			pendingPR{Drafts: []pendingDraft{
				{ID: "f0", File: "a.go", Line: 10, Commit: "h1", SourceText: "nil check", Text: "s"},
				{ID: "p3", File: "b.go", Line: 1, Commit: "h1", SourceText: "gone", Text: "s", InReview: true}}},
			pendingPR{}},
		{"touched re-ided to a matching row",
			pendingPR{Drafts: []pendingDraft{{ID: "f4", File: "a.go", Line: 10, Commit: "h1", SourceText: "nil check", Text: "mine", Edited: true, InReview: true}}},
			pendingPR{Drafts: []pendingDraft{{ID: "f0", File: "a.go", Line: 12, Commit: "h2", SourceText: "nil check", Text: "mine", Edited: true, InReview: true}}}},
		{"several rows qualify: the moved line wins",
			pendingPR{Drafts: []pendingDraft{{ID: "p0", File: "b.go", Line: 7, Commit: "h1", SourceText: "risky", Text: "mine", Edited: true}}},
			pendingPR{Drafts: []pendingDraft{{ID: "p1", File: "b.go", Line: 9, Commit: "h2", SourceText: "risky", Text: "mine", Edited: true}}}},
		{"several rows qualify, line gone: the first unclaimed",
			pendingPR{Drafts: []pendingDraft{
				{ID: "p1", File: "b.go", Line: 9, Commit: "h2", SourceText: "risky", Text: "kept", Edited: true},
				{ID: "p0", File: "b.go", Line: 1, Commit: "h1", SourceText: "risky", Text: "mine", Edited: true}}},
			pendingPR{Drafts: []pendingDraft{
				{ID: "p1", File: "b.go", Line: 9, Commit: "h2", SourceText: "risky", Text: "kept", Edited: true},
				{ID: "p0", File: "b.go", Line: 5, Commit: "h2", SourceText: "risky", Text: "mine", Edited: true}}}},
		{"kind must match: a finding draft does not take a point row",
			pendingPR{Drafts: []pendingDraft{{ID: "f1", File: "b.go", Line: 7, Commit: "h1", SourceText: "risky", Text: "mine", Edited: true}}},
			pendingPR{Drafts: []pendingDraft{{ID: "c1", File: "b.go", Line: 9, Commit: "h2", SourceText: "risky", Text: "mine", Edited: true, Unflagged: true}}}},
		{"finding gone, line gone: outdated c<n> after the highest id",
			pendingPR{Drafts: []pendingDraft{
				{ID: "c4", File: "x.go", Line: 1, Commit: "h2", Text: "c", Edited: true, InReview: true},
				{ID: "f2", File: "a.go", Line: 3, Commit: "h1", SourceText: "old", Text: "mine", Edited: true, InReview: true}}},
			pendingPR{Drafts: []pendingDraft{
				{ID: "c4", File: "x.go", Line: 1, Commit: "h2", Text: "c", Edited: true, InReview: true},
				{ID: "c5", File: "a.go", Line: 3, Commit: "h2", SourceText: "old", Text: "mine", Edited: true, InReview: true, Unflagged: true, OutdatedFrom: "h1"}}}},
		{"finding gone on the same commit: anchored c<n>",
			pendingPR{Drafts: []pendingDraft{{ID: "f2", File: "a.go", Line: 3, Commit: "h2", SourceText: "old", Text: "mine", Edited: true}}},
			pendingPR{Drafts: []pendingDraft{{ID: "c1", File: "a.go", Line: 3, Commit: "h2", SourceText: "old", Text: "mine", Edited: true, Unflagged: true}}}},
		{"finding gone, line moved: anchored c<n> on the new line",
			pendingPR{Drafts: []pendingDraft{{ID: "f2", File: "a.go", Line: 10, Commit: "h1", SourceText: "old", Text: "mine", Edited: true}}},
			pendingPR{Drafts: []pendingDraft{{ID: "c1", File: "a.go", Line: 12, Commit: "h2", SourceText: "old", Text: "mine", Edited: true, Unflagged: true}}}},
		{"line comments moved, outdated, already outdated",
			pendingPR{Drafts: []pendingDraft{
				{ID: "c1", File: "c.go", Line: 1, Commit: "h1", Text: "a", Edited: true, InReview: true},
				{ID: "c2", File: "c.go", Line: 8, Commit: "h1", Text: "b", Edited: true, InReview: true},
				{ID: "c3", File: "c.go", Line: 1, Commit: "h1", Text: "c", Edited: true, OutdatedFrom: "h0"}}},
			pendingPR{Drafts: []pendingDraft{
				{ID: "c1", File: "c.go", Line: 2, Commit: "h2", Text: "a", Edited: true, InReview: true},
				{ID: "c2", File: "c.go", Line: 8, Commit: "h2", Text: "b", Edited: true, InReview: true, OutdatedFrom: "h1"},
				{ID: "c3", File: "c.go", Line: 1, Commit: "h2", Text: "c", Edited: true, OutdatedFrom: "h0"}}}},
		{"posted drafts untouched",
			pendingPR{Drafts: []pendingDraft{
				{ID: "f0", File: "a.go", Line: 10, Commit: "h1", SourceText: "nil check", Text: "s", PostedURL: "u1"},
				{ID: "c1", File: "c.go", Line: 8, Commit: "h1", Text: "b", Edited: true, PostedURL: "u2"}}},
			pendingPR{Drafts: []pendingDraft{
				{ID: "f0", File: "a.go", Line: 10, Commit: "h1", SourceText: "nil check", Text: "s", PostedURL: "u1"},
				{ID: "c1", File: "c.go", Line: 8, Commit: "h1", Text: "b", Edited: true, PostedURL: "u2"}}}},
		{"already applies: unchanged, touched or not, and claims its row",
			pendingPR{Drafts: []pendingDraft{
				{ID: "f0", File: "a.go", Line: 12, Commit: "h2", SourceText: "nil check", Text: "s"},
				{ID: "p0", File: "b.go", Line: 5, Commit: "h2", SourceText: "risky", Text: "t", Edited: true},
				{ID: "f9", File: "a.go", Line: 10, Commit: "h1", SourceText: "nil check", Text: "mine", Edited: true}}},
			pendingPR{Drafts: []pendingDraft{
				{ID: "f0", File: "a.go", Line: 12, Commit: "h2", SourceText: "nil check", Text: "s"},
				{ID: "p0", File: "b.go", Line: 5, Commit: "h2", SourceText: "risky", Text: "t", Edited: true},
				{ID: "c1", File: "a.go", Line: 12, Commit: "h2", SourceText: "nil check", Text: "mine", Edited: true, Unflagged: true}}}},
		{"checked marks carry over or reset",
			pendingPR{Checked: []pendingCheck{{"f3", "a.go", 12, "nil check"}, {"p0", "b.go", 9, "risky"}, {"p7", "b.go", 9, "risky"}, {"f1", "a.go", 10, "nil check"}}},
			pendingPR{Checked: []pendingCheck{{"f0", "a.go", 12, "nil check"}, {"p1", "b.go", 9, "risky"}}}},
	} {
		in := c.in
		in.Rev = 7
		c.want.Rev, c.want.Result, c.want.Rows = 7, id, len(rows)
		if got := reconcile(in, st, rows, moved); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}

	// identical result: no-op, moved not called
	p := pendingPR{Rev: 3, Result: id, Rows: 1, Drafts: []pendingDraft{{ID: "f0", File: "a.go", Line: 10, Commit: "h1", Text: "s"}}}
	if got := reconcile(p, st, rows, func(pendingDraft) (int, bool) { t.Fatal("moved called"); return 0, false }); !reflect.DeepEqual(got, p) {
		t.Errorf("identical result: got %+v", got)
	}
}

func TestLineMap(t *testing.T) {
	files := map[string]string{
		"old:a.go": "x\nkeep\nmove\ndup\nz",
		"new:a.go": "x\nkeep\nnew\nmove\ndup\ndup",
		"old:b.go": "b",
		"new:b.go": "b",
	}
	var reads []string
	content := func(ctx context.Context, repo, sha, path string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok || repo != "o/r" {
			t.Errorf("read %s without a deadline or for repo %q", path, repo)
		}
		reads = append(reads, sha+":"+path)
		if s, ok := files[sha+":"+path]; ok {
			return []byte(s), nil
		}
		return nil, errors.New("404")
	}
	d := func(file string, line int, commit string) pendingDraft {
		return pendingDraft{ID: "c1", File: file, Line: line, Commit: commit, Edited: true}
	}
	drafts := []pendingDraft{
		d("a.go", 2, "old"),                              // same line
		d("a.go", 3, "old"),                              // moved, unique
		d("a.go", 4, "old"),                              // ambiguous
		d("a.go", 5, "old"),                              // gone
		d("a.go", 9, "old"),                              // past the end of the old file
		d("a.go", 0, "old"),                              // line 0, file readable
		d("gone.go", 0, "old"),                           // line 0, read error
		d("gone.go", 1, "old"),                           // read error
		d("../x.go", 1, "old"),                           // unsafe path
		d("b.go", 1, "new"),                              // same commit
		{ID: "f0", File: "b.go", Line: 1, Commit: "old"}, // untouched suggestion: not read
		{ID: "c2", File: "b.go", Line: 1, Commit: "old", PostedURL: "u"}, // posted: not read
	}
	moved := lineMap(context.Background(), content, "o/r", drafts, "new")
	for _, c := range []struct {
		d    pendingDraft
		line int
		ok   bool
	}{
		{drafts[0], 2, true}, {drafts[1], 4, true}, {drafts[2], 0, false}, {drafts[3], 0, false}, {drafts[4], 0, false},
		{drafts[5], 0, true}, {drafts[6], 0, false}, {drafts[7], 0, false}, {drafts[8], 0, false},
		{drafts[9], 1, true}, {d("b.go", 7, "new"), 7, true},
		{d("b.go", 1, "old"), 0, false}, // not in the map: added after it was built
	} {
		if line, ok := moved(c.d); line != c.line || ok != c.ok {
			t.Errorf("moved(%s:%d@%s) = %d %v, want %d %v", c.d.File, c.d.Line, c.d.Commit, line, ok, c.line, c.ok)
		}
	}
	if want := []string{"new:a.go", "old:a.go", "new:gone.go"}; !reflect.DeepEqual(reads, want) {
		t.Errorf("reads %v, want each file once: %v", reads, want)
	}

	// at most 50 distinct files are read
	files = map[string]string{}
	drafts = nil
	for i := range maxLineMapFiles + 1 {
		f := fmt.Sprintf("f%d.go", i)
		files["old:"+f], files["new:"+f] = "a", "a"
		drafts = append(drafts, d(f, 1, "old"), d(f, 1, "old"))
	}
	reads = nil
	moved = lineMap(context.Background(), content, "o/r", drafts, "new")
	if len(reads) != 2*maxLineMapFiles {
		t.Errorf("%d reads, want %d", len(reads), 2*maxLineMapFiles)
	}
	if _, ok := moved(d("f49.go", 1, "old")); !ok {
		t.Error("50th file not ok")
	}
	if _, ok := moved(d("f50.go", 1, "old")); ok {
		t.Error("51st file ok")
	}
}

func TestNewReviewOutdated(t *testing.T) {
	rv := newReview("h2", "COMMENT", "sum", []pendingDraft{
		{ID: "c1", File: "a.go", Line: 3, Commit: "h2", Text: "inline"},
		{ID: "c2", File: "b.go", Line: 7, Commit: "h2", OutdatedFrom: "0123456789abc", Text: "old"},
		{ID: "c3", File: "c.go", Line: 0, Commit: "h2", OutdatedFrom: "0123456789abc", Text: "file"},
	})
	want := reviewPayload{CommitID: "h2", Event: "COMMENT", Body: "sum\n\n`b.go:7` (outdated: written on 0123456)\n\nold\n\n`c.go` (outdated: written on 0123456)\n\nfile",
		Comments: []reviewComment{{Path: "a.go", Line: 3, Side: "RIGHT", Body: "inline"}}}
	if !reflect.DeepEqual(rv, want) {
		t.Errorf("got %+v\nwant %+v", rv, want)
	}
}
