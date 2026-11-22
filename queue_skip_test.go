package main

import (
	"regexp"
	"testing"
)

var depRule = []skipRule{{Label: "^dependencies$", re: regexp.MustCompile("^dependencies$")}}

const depReason = `label "dependencies" matches rule skip[0] ^dependencies$`

// lpr is hpr(n, "h") with labels.
func lpr(n int, labels ...string) pullRequest {
	p := hpr(n, "h")
	p.Labels = labels
	return p
}

func TestQueueSkip(t *testing.T) {
	q, f := queueIn(t, t.TempDir(), 1)
	q.skip = depRule
	q.sync([]pullRequest{lpr(1, "dependencies"), lpr(2, "go"), lpr(3)})
	if c := f.next(t); c.pr != key(2) {
		t.Fatalf("started %s, want %s", c.pr, key(2))
	} else {
		c.ret <- nil
	}
	waitStatus(t, q, 2, statusDone)
	if !saved(q.store, 2) {
		t.Fatal("no file for #2")
	}
	if st := q.statuses()[key(1)]; st.Status != statusSkipped || st.Reason != depReason || st.SkipRule != 0 {
		t.Fatalf("#1 = %+v", st)
	}
	c3 := f.next(t) // #3 running

	// #1 stops matching: queued like a new PR; #2 (done) and #3 (running) start matching.
	q.sync([]pullRequest{lpr(1), lpr(2, "dependencies"), lpr(3, "dependencies")})
	if !waitDone(c3.ctx) {
		t.Fatal("running job not cancelled")
	}
	if saved(q.store, 2) || status(q, 2) != statusSkipped || status(q, 3) != statusSkipped {
		t.Fatalf("after match: file %v, #2 %q, #3 %q", saved(q.store, 2), status(q, 2), status(q, 3))
	}
	if c := f.next(t); c.pr != key(1) {
		t.Fatalf("started %s, want %s", c.pr, key(1))
	} else {
		c.ret <- nil
	}
	waitStatus(t, q, 1, statusDone)
	if status(q, 3) != statusSkipped {
		t.Fatalf("cancelled job's result kept: #3 %q", status(q, 3))
	}
	q.sync([]pullRequest{lpr(1)}) // skipped #2 and #3 closed
	if len(q.statuses()) != 1 {
		t.Fatalf("closed skipped PRs kept: %v", q.statuses())
	}
}

func TestQueueSkipDropsQueued(t *testing.T) {
	q, f := queueIn(t, t.TempDir(), 1)
	q.skip = depRule
	q.sync([]pullRequest{lpr(1), lpr(2)})
	c := f.next(t)
	q.sync([]pullRequest{lpr(1), lpr(2, "dependencies")})
	if fifoLen(q) != 0 || status(q, 2) != statusSkipped {
		t.Fatalf("fifo %d, #2 %q", fifoLen(q), status(q, 2))
	}
	if q.rerun(key(2)); fifoLen(q) != 0 || status(q, 2) != statusSkipped {
		t.Fatalf("rerun queued a skipped PR: fifo %d, #2 %q", fifoLen(q), status(q, 2))
	}
	c.ret <- nil
	waitStatus(t, q, 1, statusDone)
	select {
	case c := <-f.calls:
		t.Fatalf("skipped PR started: %s", c.pr)
	default:
	}
}

func TestQueueSkipNotRestored(t *testing.T) {
	dir := t.TempDir()
	q, f := queueIn(t, dir, 1)
	p := lpr(1)
	q.sync([]pullRequest{p})
	f.next(t).ret <- nil
	waitStatus(t, q, 1, statusDone)

	q2, f2 := queueIn(t, dir, 1)
	q2.skip = []skipRule{{Title: "^Bump ", re: regexp.MustCompile("^Bump ")}}
	p.Title = "Bump x"
	q2.sync([]pullRequest{p})
	if st := q2.statuses()[key(1)]; st.Status != statusSkipped || st.Reason != "title matches rule skip[0] ^Bump " || saved(q2.store, 1) {
		t.Fatalf("restored %+v, file %v", st, saved(q2.store, 1))
	}
	select {
	case c := <-f2.calls:
		t.Fatalf("skipped PR started: %s", c.pr)
	default:
	}
}
