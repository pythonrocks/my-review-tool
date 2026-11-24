package main

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// hpr is pr(n) at head commit sha.
func hpr(n int, sha string) pullRequest {
	p := pr(n)
	p.HeadSHA = sha
	return p
}

func saved(s resultStore, n int) bool {
	_, err := os.Stat(s.file(key(n)))
	return err == nil
}

func TestQueueSavesAndRestores(t *testing.T) {
	dir := t.TempDir()
	q, f := queueIn(t, dir, 1)
	f.findings = []finding{{Severity: "High", File: "a.go", Line: 3, Description: "d"}}
	f.summary = "Adds a."
	prs := []pullRequest{hpr(1, "h1"), hpr(2, "h2"), hpr(3, "h3")}
	q.sync(prs)
	f.next(t).ret <- nil
	f.next(t).ret <- errors.New("boom")
	f.next(t).ret <- nil
	waitStatus(t, q, 3, statusDone)
	for n := 1; n <= 3; n++ {
		if !saved(q.store, n) {
			t.Fatalf("no file for %s", key(n))
		}
	}
	want := q.statuses()

	q2, f2 := queueIn(t, dir, 1)
	q2.sync([]pullRequest{prs[0], prs[1], hpr(3, "new head"), hpr(4, "h4")})
	got := q2.statuses()
	for n := 1; n <= 2; n++ {
		if g, w := got[key(n)], want[key(n)]; g.Status != w.Status || g.FailedStep != w.FailedStep || g.Reason != w.Reason || g.Summary != w.Summary || g.HeadSHA != w.HeadSHA || !reflect.DeepEqual(g.riskDecision, w.riskDecision) || len(g.Findings) != len(w.Findings) || (len(g.Findings) > 0 && !reflect.DeepEqual(g.Findings[0], w.Findings[0])) {
			t.Fatalf("%s restored as %+v, want %+v", key(n), g, w)
		}
	}
	if got[key(1)].Summary != "Adds a." || got[key(2)].HeadSHA != "h2" {
		t.Fatalf("restored %+v", got)
	}
	if status(q2, 2) != "failed: code review: boom" {
		t.Fatalf("restored failed status = %q", status(q2, 2))
	}
	for _, n := range []int{3, 4} { // different head, no file
		if c := f2.next(t); c.pr != key(n) {
			t.Fatalf("started %s, want %s", c.pr, key(n))
		} else {
			c.ret <- nil
		}
	}
	waitStatus(t, q2, 4, statusDone)
	if len(f2.calls) != 0 {
		t.Fatal("a restored PR was run")
	}
}

func TestQueueCancelledAndReplacedSaveNothing(t *testing.T) {
	q, f := queueFakes(t, 1)
	q.sync([]pullRequest{hpr(1, "h"), hpr(2, "h")})
	c1 := f.next(t)
	q.rerun(key(1)) // replaces the running job; it is queued behind 2
	c1.ret <- nil
	c2 := f.next(t) // 1's old run has finished
	if saved(q.store, 1) {
		t.Fatal("replaced job saved its result")
	}
	q.sync([]pullRequest{hpr(1, "h")}) // cancels 2
	c2.ret <- nil
	f.next(t).ret <- errors.New("boom") // 1's fresh run
	waitStatus(t, q, 1, "failed: code review: boom")
	if saved(q.store, 2) {
		t.Fatal("cancelled job saved its result")
	}
	if !saved(q.store, 1) {
		t.Fatal("failed job not saved")
	}
}

func TestQueueBadFileIsQueued(t *testing.T) {
	dir := t.TempDir()
	s := resultStore{dir: dir}
	if err := os.WriteFile(s.file(key(1)), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	s.save(hpr(3, "h"), jobStatus{Status: statusDone})
	if err := os.Rename(s.file(key(3)), s.file(key(2))); err != nil { // file of #3 under #2's name
		t.Fatal(err)
	}
	q, f := queueIn(t, dir, 1)
	log := stderrOf(t, func() { q.sync([]pullRequest{hpr(1, "h"), hpr(2, "h")}) })
	if strings.Count(log, "ignoring saved result") != 2 {
		t.Fatalf("stderr = %q", log)
	}
	f.next(t).ret <- nil
	f.next(t).ret <- nil
	waitStatus(t, q, 2, statusDone)
	if _, ok := s.load(hpr(2, "h")); !ok {
		t.Fatal("bad file not replaced by the new result")
	}
}

func TestQueueDoneFileWithoutRecommendationIsQueued(t *testing.T) {
	dir := t.TempDir()
	s := resultStore{dir: dir}
	s.save(hpr(1, "h"), jobStatus{Status: statusDone, HeadSHA: "h"}) // as saved before risk assessment existed
	q, f := queueIn(t, dir, 1)
	if log := stderrOf(t, func() { q.sync([]pullRequest{hpr(1, "h")}) }); log != "" {
		t.Fatalf("stderr = %q, want silent", log)
	}
	f.next(t).ret <- nil
	waitStatus(t, q, 1, statusDone)
	if st, ok := s.load(hpr(1, "h")); !ok || st.Recommendation != recAutomated {
		t.Fatalf("new result = %+v, %v", st, ok)
	}
}

func TestQueueRerunAndSyncRemoveFile(t *testing.T) {
	dir := t.TempDir()
	q, f := queueIn(t, dir, 1)
	q.sync([]pullRequest{hpr(1, "h"), hpr(2, "h")})
	f.next(t).ret <- nil
	c := f.next(t)
	waitStatus(t, q, 1, statusDone)
	q.rerun(key(1))
	if saved(q.store, 1) {
		t.Fatal("re-run left the file")
	}
	q2, f2 := queueIn(t, dir, 1) // restart while #1 is queued and #2 running
	q2.sync([]pullRequest{hpr(1, "h"), hpr(2, "h")})
	if status(q2, 1) != statusRunning || status(q2, 2) != statusQueued {
		t.Fatalf("after restart: %q %q", status(q2, 1), status(q2, 2))
	}
	f2.next(t).ret <- nil
	c.ret <- nil
	waitStatus(t, q, 2, statusDone)
	q.sync([]pullRequest{hpr(1, "h")})
	if saved(q.store, 2) {
		t.Fatal("dropped PR's file kept")
	}
}

func TestQueuePrune(t *testing.T) {
	s := resultStore{dir: t.TempDir()}
	q := newReviewQueue(0, workflow{}, s)
	files := map[string]pullRequest{}
	for _, repo := range []string{"a/b", "c/d", "x/y"} {
		for n := 1; n <= 2; n++ {
			p := pullRequest{Repo: repo, Number: n, HeadSHA: "h"}
			s.save(p, jobStatus{Status: statusDone})
			files[prKey(p)] = p
		}
	}
	open := []pullRequest{{Repo: "a/b", Number: 1}, {Repo: "c/d", Number: 1}}
	left := func() (out []string) {
		for k := range files {
			if _, err := os.Stat(s.file(k)); err == nil {
				out = append(out, k)
			}
		}
		return out
	}
	repos := []string{"a/b", "c/d"} // x/y is not watched
	q.prune(repos, pollSnapshot{PRs: open, Global: "gh is not logged in"})
	if n := len(left()); n != 6 {
		t.Fatalf("global error pruned: %d files left", n)
	}
	q.prune(repos, pollSnapshot{PRs: open, Errors: map[string]string{"c/d": "boom"}})
	for k := range files {
		_, err := os.Stat(s.file(k))
		if gone := err != nil; gone != (k == "a/b#2") {
			t.Errorf("%s gone = %v", k, gone)
		}
	}
}

func TestQueueRecordsSavedAndDroppedRuns(t *testing.T) {
	q, f := queueFakes(t, 1)
	var mu sync.Mutex
	var got []runRecord
	q.record = func(r runRecord) {
		q.statuses() // deadlocks if called with q.mu held
		mu.Lock()
		defer mu.Unlock()
		got = append(got, r)
	}
	recorded := func(n int) func() bool {
		return func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == n }
	}
	q.sync([]pullRequest{hpr(1, "h")})
	f.next(t)
	q.rerun(key(1)) // drops the running run
	waitFor(t, "dropped run recorded", recorded(1))
	f.next(t).ret <- nil
	waitFor(t, "saved run recorded", recorded(2))
	saved := q.statuses()[key(1)].Metrics
	mu.Lock()
	defer mu.Unlock()
	if got[0].Outcome != statusCancelled || got[1].Outcome != statusDone || got[1].PR.HeadSHA != "h" {
		t.Fatalf("outcomes %q %q", got[0].Outcome, got[1].Outcome)
	}
	if got[1].Status.Metrics.RunID != saved.RunID || got[0].Status.Metrics.RunID == saved.RunID {
		t.Fatalf("run ids %q %q, saved %q", got[0].Status.Metrics.RunID, got[1].Status.Metrics.RunID, saved.RunID)
	}
}

func TestQueueShutdownRecordsRunEndingInItsWait(t *testing.T) {
	dir := t.TempDir()
	q, f := queueFakes(t, 1)
	mdb := openTestDB(t, dir)
	q.record = func(r runRecord) {
		time.Sleep(200 * time.Millisecond) // widens the gap between the run ending and its record being queued
		mdb.record(r)
	}
	q.sync([]pullRequest{pr(1)})
	f.next(t)
	q.shutdown(5 * time.Second) // cancels the run, which ends within the wait
	mdb.close(5 * time.Second)
	if n := count(t, readDB(t, dir), "ai_runs"); n != 1 {
		t.Fatalf("ai_runs = %d, want the run that ended during shutdown", n)
	}
}
