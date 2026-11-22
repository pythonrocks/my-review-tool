package main

import (
	"testing"
	"time"
)

const week = 168 * time.Hour

// ageQueue is clockQueue with maxAge set to a week.
func ageQueue(t *testing.T, dir string) (*reviewQueue, *fakes, *time.Time) {
	q, f, now := clockQueue(t, dir, 1, 5*time.Minute)
	q.maxAge = week
	return q, f, now
}

// apr is hpr(n, sha) created age before now.
func apr(n int, sha string, now time.Time, age time.Duration) pullRequest {
	p := hpr(n, sha)
	p.Created = now.Add(-age)
	return p
}

// noCall fails if a review started.
func noCall(t *testing.T, f *fakes) {
	t.Helper()
	select {
	case c := <-f.calls:
		t.Fatalf("review started: %s", c.pr)
	default:
	}
}

func TestQueueAgeYoungQueued(t *testing.T) {
	q, f, now := ageQueue(t, t.TempDir())
	q.sync([]pullRequest{apr(1, "h", *now, week)}) // exactly max age is still young
	if c := f.next(t); c.pr != key(1) {
		t.Fatalf("started %s", c.pr)
	}
}

func TestQueueAgeOnDemand(t *testing.T) {
	q, f, now := ageQueue(t, t.TempDir())
	old := apr(1, "h1", *now, week+time.Second)
	q.sync([]pullRequest{old})
	st := q.statuses()[key(1)]
	if st.Status != statusOnDemand || st.Reason != "created more than 168h0m0s ago" || saved(q.store, 1) {
		t.Fatalf("status %+v, file %v", st, saved(q.store, 1))
	}
	old.HeadSHA = "h2" // a head change keeps it on demand
	q.sync([]pullRequest{old})
	if status(q, 1) != statusOnDemand {
		t.Fatalf("after push: %q", status(q, 1))
	}
	idle(t, q, f)
	noCall(t, f)

	if !q.rerun(key(1)) {
		t.Fatal("rerun: unknown PR")
	}
	f.next(t).ret <- nil
	waitStatus(t, q, 1, statusDone)
	if !saved(q.store, 1) {
		t.Fatal("no file after review")
	}
	q.sync([]pullRequest{old}) // the saved result is kept
	if status(q, 1) != statusDone {
		t.Fatalf("after sync: %q", status(q, 1))
	}
}

func TestQueueAgeRestoresSaved(t *testing.T) {
	dir := t.TempDir()
	q, f, now := clockQueue(t, dir, 1, 5*time.Minute)
	old := apr(1, "h1", *now, 30*24*time.Hour)
	reviewed(t, q, f, 0, old)

	q2, f2, _ := ageQueue(t, dir)
	q2.sync([]pullRequest{old})
	if st := q2.statuses()[key(1)]; st.Status != statusDone || st.Outdated {
		t.Fatalf("restored %+v", st)
	}
	idle(t, q2, f2)
}

func TestQueueAgeReviewedFollowsQuietPeriod(t *testing.T) {
	q, f, now := ageQueue(t, t.TempDir())
	old := apr(1, "h1", *now, 30*24*time.Hour)
	q.sync([]pullRequest{old})
	q.rerun(key(1))
	f.next(t).ret <- nil
	waitStatus(t, q, 1, statusDone)

	old.HeadSHA = "h2"
	q.sync([]pullRequest{old})
	if !outdated(q, 1) {
		t.Fatal("not outdated after push")
	}
	idle(t, q, f)
	*now = now.Add(5 * time.Minute)
	q.sync([]pullRequest{old})
	if c := f.next(t); c.pr != key(1) {
		t.Fatalf("started %s", c.pr)
	}
}

func TestQueueAgeSkipWins(t *testing.T) {
	q, f, now := ageQueue(t, t.TempDir())
	q.skip = depRule
	old := apr(1, "h", *now, 30*24*time.Hour)
	old.Labels = []string{"dependencies"}
	q.sync([]pullRequest{old})
	if status(q, 1) != statusSkipped {
		t.Fatalf("status %q", status(q, 1))
	}
	old.Labels = nil // stops matching: treated like a new PR
	q.sync([]pullRequest{old})
	if status(q, 1) != statusOnDemand {
		t.Fatalf("after unmatch: %q", status(q, 1))
	}
	noCall(t, f)
}

func TestQueueAgeZeroQueuesAll(t *testing.T) {
	q, f, now := clockQueue(t, t.TempDir(), 1, 5*time.Minute)
	q.sync([]pullRequest{apr(1, "h", *now, 365*24*time.Hour)})
	if c := f.next(t); c.pr != key(1) {
		t.Fatalf("started %s", c.pr)
	}
}
