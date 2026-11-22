package main

import (
	"errors"
	"testing"
	"time"
)

// dpr is a draft PR n at head commit sha.
func dpr(n int, sha string) pullRequest {
	p := hpr(n, sha)
	p.Draft = true
	return p
}

// clockQueue is queueFakes with a fake clock the test moves by advancing *now.
func clockQueue(t *testing.T, dir string, limit int, quiet time.Duration) (*reviewQueue, *fakes, *time.Time) {
	q, f := queueIn(t, dir, limit)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now } // only called from sync, rerun and enqueue, all on the test goroutine
	q.quietPeriod = quiet
	return q, f, &now
}

// reviewed runs prs to the end, failing PR fail if non-zero, and waits for their results.
func reviewed(t *testing.T, q *reviewQueue, f *fakes, fail int, prs ...pullRequest) {
	t.Helper()
	q.sync(prs)
	for range prs {
		c := f.next(t)
		if c.pr == key(fail) {
			c.ret <- errors.New("boom")
		} else {
			c.ret <- nil
		}
	}
	for _, p := range prs {
		waitFor(t, prKey(p)+" finished", func() bool { s := status(q, p.Number); return s != statusQueued && s != statusRunning })
	}
}

func outdated(q *reviewQueue, n int) bool { return q.statuses()[key(n)].Outdated }

// idle fails unless nothing is queued or running.
func idle(t *testing.T, q *reviewQueue, f *fakes) {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.fifo) != 0 || q.running != 0 || len(f.calls) != 0 {
		t.Fatalf("queue busy: fifo %d, running %d, calls %d", len(q.fifo), q.running, len(f.calls))
	}
}

func TestQueueOutdatedOnNewCommit(t *testing.T) {
	q, f, _ := clockQueue(t, t.TempDir(), 1, time.Hour)
	reviewed(t, q, f, 2, hpr(1, "h1"), hpr(2, "h1"))
	if outdated(q, 1) || outdated(q, 2) {
		t.Fatal("current results reported outdated")
	}
	for range 2 {
		q.sync([]pullRequest{hpr(1, "h2"), hpr(2, "h2")})
	}
	st := q.statuses()
	if st[key(1)].Status != statusDone || st[key(2)].Status != statusFailed || !st[key(1)].Outdated || !st[key(2)].Outdated || st[key(1)].HeadSHA != "h1" {
		t.Fatalf("statuses %+v", st)
	}
	idle(t, q, f)
	if !saved(q.store, 1) || !saved(q.store, 2) {
		t.Fatal("outdated result's file removed")
	}
	q.sync([]pullRequest{hpr(1, ""), hpr(2, "h2")})
	if outdated(q, 1) {
		t.Fatal("empty head counted as outdated")
	}
}

func TestQueueQuietPeriod(t *testing.T) {
	q, f, now := clockQueue(t, t.TempDir(), 1, 5*time.Minute)
	reviewed(t, q, f, 0, hpr(1, "h1"))
	q.sync([]pullRequest{hpr(1, "h2")})
	*now = now.Add(4 * time.Minute)
	q.sync([]pullRequest{hpr(1, "h3")}) // a second push resets the wait
	*now = now.Add(4 * time.Minute)
	q.sync([]pullRequest{hpr(1, "h3")})
	if !outdated(q, 1) {
		t.Fatal("not outdated")
	}
	idle(t, q, f)
	*now = now.Add(time.Minute)
	q.sync([]pullRequest{hpr(1, "h3")})
	c := f.next(t)
	q.sync([]pullRequest{hpr(1, "h3")})
	if st := q.statuses()[key(1)]; c.pr != key(1) || st.Status != statusRunning || st.HeadSHA != "h3" || st.Outdated || saved(q.store, 1) {
		t.Fatalf("after the quiet period: %+v, file kept %v", st, saved(q.store, 1))
	}
	c.ret <- nil
	waitStatus(t, q, 1, statusDone)
	idle(t, q, f)
}

func TestQueueRerunSkipsQuietPeriod(t *testing.T) {
	q, f, _ := clockQueue(t, t.TempDir(), 1, time.Hour)
	reviewed(t, q, f, 0, hpr(1, "h1"))
	q.sync([]pullRequest{hpr(1, "h2")})
	q.rerun(key(1))
	f.next(t).ret <- nil
	waitStatus(t, q, 1, statusDone)
	if st := q.statuses()[key(1)]; st.HeadSHA != "h2" || st.Outdated {
		t.Fatalf("re-run result %+v", st)
	}
}

func TestQueueDraftOutdated(t *testing.T) {
	q, f, now := clockQueue(t, t.TempDir(), 1, 0)
	reviewed(t, q, f, 0, dpr(1, "h1")) // drafts are reviewed on first sight
	q.sync([]pullRequest{dpr(1, "h2")})
	*now = now.Add(time.Hour)
	q.sync([]pullRequest{dpr(1, "h2")})
	if !outdated(q, 1) {
		t.Fatal("draft not outdated")
	}
	idle(t, q, f)
	q.rerun(key(1))
	f.next(t).ret <- nil
	waitStatus(t, q, 1, statusDone)

	q.sync([]pullRequest{dpr(1, "h3")})
	idle(t, q, f)
	q.sync([]pullRequest{hpr(1, "h3")}) // marked ready
	f.next(t).ret <- nil
	waitStatus(t, q, 1, statusDone)
	if st := q.statuses()[key(1)]; st.HeadSHA != "h3" {
		t.Fatalf("ready result %+v", st)
	}
}

func TestQueueReadyBecomesDraftWhileWaiting(t *testing.T) {
	q, f, now := clockQueue(t, t.TempDir(), 1, 5*time.Minute)
	reviewed(t, q, f, 0, hpr(1, "h1"))
	q.sync([]pullRequest{hpr(1, "h2")})
	*now = now.Add(10 * time.Minute)
	q.sync([]pullRequest{dpr(1, "h2")})
	if !outdated(q, 1) {
		t.Fatal("not outdated")
	}
	idle(t, q, f)
}

func TestQueueQueuedJobFollowsHead(t *testing.T) {
	q, f, _ := clockQueue(t, t.TempDir(), 1, time.Hour)
	q.sync([]pullRequest{hpr(1, "h1"), hpr(2, "h1"), hpr(3, "h1")})
	c := f.next(t)
	q.sync([]pullRequest{hpr(1, "h1"), dpr(2, "h2"), hpr(3, "h1")})
	if st := q.statuses()[key(2)]; fifoLen(q) != 2 || st.Status != statusQueued || st.HeadSHA != "h2" {
		t.Fatalf("fifo %d, #2 %+v", fifoLen(q), st)
	}
	c.ret <- nil
	for _, n := range []int{2, 3} {
		if c := f.next(t); c.pr != key(n) {
			t.Fatalf("started %s, want %s", c.pr, key(n))
		} else {
			c.ret <- nil
		}
	}
	waitStatus(t, q, 3, statusDone)
	if st, ok := q.store.load(hpr(2, "h2")); !ok || st.HeadSHA != "h2" {
		t.Fatalf("#2 saved %+v, %v", st, ok)
	}
	idle(t, q, f)
}

func TestQueueRunningJobReplaced(t *testing.T) {
	q, f, _ := clockQueue(t, t.TempDir(), 1, time.Hour)
	q.sync([]pullRequest{hpr(1, "h1"), hpr(2, "h1")})
	c1 := f.next(t)
	q.sync([]pullRequest{hpr(1, "h2"), hpr(2, "h1")})
	if !waitDone(c1.ctx) {
		t.Fatal("running job not cancelled")
	}
	if st := q.statuses()[key(1)]; st.Status != statusQueued || st.HeadSHA != "h2" {
		t.Fatalf("#1 %+v", st)
	}
	for _, n := range []int{2, 1} { // the replacement goes to the back
		c := f.next(t)
		if c.pr != key(n) {
			t.Fatalf("started %s, want %s", c.pr, key(n))
		}
		if n == 2 && status(q, 1) != statusQueued {
			t.Fatalf("the cancelled run's result appeared: %s", status(q, 1))
		}
		c.ret <- nil
	}
	waitStatus(t, q, 1, statusDone)
	if st := q.statuses()[key(1)]; st.HeadSHA != "h2" {
		t.Fatalf("#1 %+v", st)
	}
	idle(t, q, f)
}

func TestQueueRunningDraftReplaced(t *testing.T) {
	q, f, _ := clockQueue(t, t.TempDir(), 1, time.Hour)
	q.sync([]pullRequest{dpr(1, "h1")})
	c1 := f.next(t)
	q.sync([]pullRequest{dpr(1, "h2")})
	if !waitDone(c1.ctx) {
		t.Fatal("running draft not cancelled")
	}
	f.next(t).ret <- nil
	waitStatus(t, q, 1, statusDone)
	if st := q.statuses()[key(1)]; st.HeadSHA != "h2" {
		t.Fatalf("#1 %+v", st)
	}
}

func TestQueueHeadReturnsToReviewed(t *testing.T) {
	q, f, now := clockQueue(t, t.TempDir(), 1, 5*time.Minute)
	reviewed(t, q, f, 0, hpr(1, "h1"))
	q.sync([]pullRequest{hpr(1, "h2")})
	q.sync([]pullRequest{hpr(1, "h1")})
	*now = now.Add(time.Hour)
	q.sync([]pullRequest{hpr(1, "h1")})
	if outdated(q, 1) || status(q, 1) != statusDone {
		t.Fatalf("#1 %+v", q.statuses()[key(1)])
	}
	idle(t, q, f)
}

func TestQueueRestartWithOlderHead(t *testing.T) {
	dir := t.TempDir()
	q, f, _ := clockQueue(t, dir, 1, 0)
	reviewed(t, q, f, 0, hpr(1, "h1"), dpr(2, "h1"))

	q2, f2, now := clockQueue(t, dir, 1, 5*time.Minute)
	q2.sync([]pullRequest{hpr(1, "h2"), dpr(2, "h2")})
	if !outdated(q2, 1) || !outdated(q2, 2) {
		t.Fatalf("restored %+v", q2.statuses())
	}
	idle(t, q2, f2)
	*now = now.Add(5 * time.Minute) // measured from startup
	q2.sync([]pullRequest{hpr(1, "h2"), dpr(2, "h2")})
	if c := f2.next(t); c.pr != key(1) {
		t.Fatalf("started %s", c.pr)
	} else {
		c.ret <- nil
	}
	waitStatus(t, q2, 1, statusDone)
	if !outdated(q2, 2) {
		t.Fatal("draft lost its outdated result")
	}
	idle(t, q2, f2)
}
