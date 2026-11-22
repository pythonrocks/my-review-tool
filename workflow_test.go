package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// call is one invocation of a fake step; the test ends it by sending on ret.
type call struct {
	step, pr string
	ctx      context.Context
	ret      chan error
}

// fakes hands every step invocation to the test over calls and blocks until the test answers or ctx is done.
type fakes struct {
	calls             chan call
	active, maxActive atomic.Int32
	findings          []finding // returned by every step the test ends without an error
	summary           string    // likewise
}

func newFakes() *fakes { return &fakes{calls: make(chan call, 100)} }

func (f *fakes) step(name string) step {
	return func(ctx context.Context, pr pullRequest, _ workspace) (stepResult, error) {
		n := f.active.Add(1)
		defer f.active.Add(-1)
		for m := f.maxActive.Load(); n > m && !f.maxActive.CompareAndSwap(m, n); m = f.maxActive.Load() {
		}
		c := call{name, prKey(pr), ctx, make(chan error, 1)}
		f.calls <- c
		select {
		case err := <-c.ret:
			if err != nil {
				return stepResult{}, err
			}
			return stepResult{Summary: f.summary, Findings: f.findings}, nil
		case <-ctx.Done():
			return stepResult{}, ctx.Err()
		}
	}
}

func (f *fakes) next(t *testing.T) call {
	t.Helper()
	select {
	case c := <-f.calls:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no step started")
		return call{}
	}
}

func okStep(context.Context, pullRequest, workspace) (stepResult, error) { return stepResult{}, nil }

// asRisk is s as a risk step that ignores the findings.
func asRisk(s step) riskStep {
	return func(ctx context.Context, pr pullRequest, ws workspace, _ []taggedFinding) (stepResult, error) {
		return s(ctx, pr, ws)
	}
}

// noPolicy is a repository without a policy file and a reviewer config without step settings.
func noPolicy(context.Context, string) (effectivePolicy, error) {
	return resolvePolicy(nil, config{}), nil
}

func okPrepare(_ context.Context, _ pullRequest, tmp string) (workspace, error) {
	return workspace{Dir: filepath.Join(tmp, "src"), Tmp: tmp, Diff: "diff"}, nil
}

func neverStep(t *testing.T) step {
	return func(context.Context, pullRequest, workspace) (stepResult, error) {
		t.Error("step must not run")
		return stepResult{}, nil
	}
}

// waitFor polls cond until it holds; the outcome is decided by the steps, the polling only waits for the queue's goroutine.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func waitDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(5 * time.Second):
		return false
	}
}

func pr(n int) pullRequest { return pullRequest{Repo: "o/r", Number: n} }

func key(n int) string { return prKey(pr(n)) }

func TestRunWorkflowShape(t *testing.T) {
	f := newFakes()
	var returned atomic.Int32
	counted := func(s step) step {
		return func(ctx context.Context, p pullRequest, ws workspace) (stepResult, error) {
			defer returned.Add(1)
			return s(ctx, p, ws)
		}
	}
	risk := f.step("risk assessment")
	w := workflow{noPolicy, okPrepare, counted(f.step("code review")), counted(f.step("security review")), asRisk(func(ctx context.Context, p pullRequest, ws workspace) (stepResult, error) {
		if n := returned.Load(); n != 2 {
			t.Errorf("risk assessment started after %d of 2 parallel steps", n)
		}
		return risk(ctx, p, ws)
	}), time.Minute}
	done := make(chan [2]string)
	go func() {
		st := runWorkflow(context.Background(), pr(1), w)
		done <- [2]string{st.FailedStep, st.Reason}
	}()
	c1, c2 := f.next(t), f.next(t) // both blocked: they run at the same time
	if got := c1.step + "+" + c2.step; got != "code review+security review" && got != "security review+code review" {
		t.Fatalf("parallel steps = %s", got)
	}
	c1.ret <- nil
	c2.ret <- nil
	c3 := f.next(t)
	if c3.step != "risk assessment" {
		t.Fatalf("third step = %s", c3.step)
	}
	c3.ret <- nil
	if got := <-done; got != [2]string{"", ""} {
		t.Fatalf("result = %q", got)
	}
}

func TestRunWorkflowStepFailure(t *testing.T) {
	f := newFakes()
	w := workflow{noPolicy, okPrepare, f.step("code review"), f.step("security review"), asRisk(neverStep(t)), time.Minute}
	done := make(chan [2]string)
	go func() {
		st := runWorkflow(context.Background(), pr(1), w)
		done <- [2]string{st.FailedStep, st.Reason}
	}()
	calls := map[string]call{}
	for range 2 {
		c := f.next(t)
		calls[c.step] = c
	}
	calls["code review"].ret <- errors.New("boom")
	if !waitDone(calls["security review"].ctx) {
		t.Fatal("security review not cancelled")
	}
	if got := <-done; got != [2]string{"code review", "boom"} {
		t.Fatalf("result = %q", got)
	}
}

func TestRunWorkflowLongErrorIsCut(t *testing.T) {
	long := strings.Repeat("é", 400)
	fail := func(context.Context, pullRequest, workspace) (stepResult, error) {
		return stepResult{}, errors.New(long)
	}
	st := runWorkflow(context.Background(), pr(1), workflow{noPolicy, okPrepare, okStep, okStep, asRisk(fail), time.Minute})
	if st.Status != statusFailed || st.FailedStep != "risk assessment" || st.Reason != long[:600] { // é is 2 bytes
		t.Fatalf("result = %q, %d runes", st.FailedStep, len([]rune(st.Reason)))
	}
}

func TestRunWorkflowTimeout(t *testing.T) {
	block := func(ctx context.Context, _ pullRequest, _ workspace) (stepResult, error) {
		<-ctx.Done()
		return stepResult{}, ctx.Err()
	}
	st := runWorkflow(context.Background(), pr(1), workflow{noPolicy, okPrepare, okStep, block, asRisk(neverStep(t)), 10 * time.Millisecond})
	if st.FailedStep != "security review" || st.Reason != "timed out after 10ms" {
		t.Fatalf("result = %q, %q", st.FailedStep, st.Reason)
	}
}

// queueFakes returns a queue whose workflow blocks in code review until the test answers.
func queueFakes(t *testing.T, limit int) (*reviewQueue, *fakes) {
	return queueIn(t, t.TempDir(), limit)
}

// queueIn is queueFakes with its results saved in dir. At the end of the test it cancels every job
// and waits for them, so no result is written while dir is removed.
func queueIn(t *testing.T, dir string, limit int) (*reviewQueue, *fakes) {
	f := newFakes()
	q := newReviewQueue(limit, workflow{noPolicy, okPrepare, f.step("code review"), okStep, asRisk(okStep), time.Minute}, resultStore{dir: dir})
	t.Cleanup(func() {
		q.sync(nil)
		waitFor(t, "jobs to end", func() bool {
			q.mu.Lock()
			defer q.mu.Unlock()
			return q.running == 0
		})
	})
	return q, f
}

func status(q *reviewQueue, n int) string {
	s := q.statuses()[key(n)]
	if s.Status == statusFailed {
		return fmt.Sprintf("failed: %s: %s", s.FailedStep, s.Reason)
	}
	return s.Status
}

func waitStatus(t *testing.T, q *reviewQueue, n int, want string) {
	t.Helper()
	waitFor(t, key(n)+" "+want, func() bool { return status(q, n) == want })
}

func fifoLen(q *reviewQueue) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.fifo)
}

func TestQueueConcurrencyAndOrder(t *testing.T) {
	q, f := queueFakes(t, 2)
	q.sync([]pullRequest{pr(1), pr(2), pr(3), pr(4), pr(5)})
	count := func(s string) (n int) {
		for _, st := range q.statuses() {
			if st.Status == s {
				n++
			}
		}
		return n
	}
	if r, w := count(statusRunning), count(statusQueued); r != 2 || w != 3 {
		t.Fatalf("running %d queued %d, want 2 and 3", r, w)
	}
	running := []call{f.next(t), f.next(t)}
	if got := running[0].pr + " " + running[1].pr; got != key(1)+" "+key(2) && got != key(2)+" "+key(1) { // they start together
		t.Fatalf("first runs = %s %s", running[0].pr, running[1].pr)
	}
	for n := 3; n <= 5; n++ {
		running[0].ret <- nil
		c := f.next(t) // the next one starts only when a slot frees up
		if c.pr != key(n) {
			t.Fatalf("started %s, want %s", c.pr, key(n))
		}
		running = append(running[1:], c)
	}
	running[0].ret <- nil
	running[1].ret <- nil
	for n := 1; n <= 5; n++ {
		waitStatus(t, q, n, statusDone)
	}
	if m := f.maxActive.Load(); m != 2 {
		t.Fatalf("max concurrent runs = %d, want 2", m)
	}
}

func TestQueueRerunGoesToBack(t *testing.T) {
	q, f := queueFakes(t, 1)
	q.sync([]pullRequest{pr(1), pr(2), pr(3)})
	f.next(t).ret <- nil // 1
	c2 := f.next(t)
	waitStatus(t, q, 1, statusDone)
	q.rerun(key(1))
	c2.ret <- nil
	for _, want := range []int{3, 1} {
		c := f.next(t)
		if c.pr != key(want) {
			t.Fatalf("started %s, want %s", c.pr, key(want))
		}
		c.ret <- nil
	}
}

func TestQueueRerun(t *testing.T) {
	q, f := queueFakes(t, 1)
	q.sync([]pullRequest{pr(1), pr(2)})
	c1 := f.next(t)
	if q.rerun("o/r#9") {
		t.Fatal("rerun of an unknown PR succeeded")
	}
	if !q.rerun(key(2)) || fifoLen(q) != 1 || status(q, 2) != statusQueued {
		t.Fatalf("rerun of queued: fifo %d, status %s", fifoLen(q), status(q, 2))
	}

	c1.ret <- nil // 1 done, 2 starts
	c2 := f.next(t)
	waitStatus(t, q, 1, statusDone)
	q.rerun(key(1))
	if status(q, 1) != statusQueued {
		t.Fatalf("rerun of done: %s", status(q, 1))
	}

	c2.ret <- errors.New("bad diff") // 2 failed, 1 runs again
	c1 = f.next(t)
	if c1.pr != key(1) {
		t.Fatalf("started %s", c1.pr)
	}
	waitStatus(t, q, 2, "failed: code review: bad diff")
	q.rerun(key(2))
	if status(q, 2) != statusQueued {
		t.Fatalf("rerun of failed: %s", status(q, 2))
	}

	c1.ret <- nil
	c2 = f.next(t)
	if c2.pr != key(2) {
		t.Fatalf("started %s", c2.pr)
	}
	q.rerun(key(2)) // while running
	if !waitDone(c2.ctx) {
		t.Fatal("running job not cancelled")
	}
	c2 = f.next(t) // the new job starts once the cancelled run frees its slot
	if c2.pr != key(2) || status(q, 2) != statusRunning {
		t.Fatalf("started %s, status %s; the cancelled run's result must be dropped", c2.pr, status(q, 2))
	}
	c2.ret <- nil
	waitStatus(t, q, 2, statusDone)
	if fifoLen(q) != 0 || len(f.calls) != 0 { // exactly one new job was queued
		t.Fatalf("left over: fifo %d, calls %d", fifoLen(q), len(f.calls))
	}
}

func TestQueueSyncCancels(t *testing.T) {
	q, f := queueFakes(t, 1)
	q.sync([]pullRequest{pr(1), pr(2), pr(3)})
	c1 := f.next(t)
	q.sync([]pullRequest{pr(1), pr(3)}) // 2 closed while queued
	if _, ok := q.statuses()[key(2)]; ok || fifoLen(q) != 1 {
		t.Fatalf("closed queued PR still known: fifo %d", fifoLen(q))
	}
	q.sync([]pullRequest{pr(3)}) // 1 closed while running
	if !waitDone(c1.ctx) {
		t.Fatal("running job not cancelled")
	}
	if c := f.next(t); c.pr != key(3) {
		t.Fatalf("started %s, want %s", c.pr, key(3))
	} else {
		c.ret <- nil
	}
	waitStatus(t, q, 3, statusDone)
	if got := len(q.statuses()); got != 1 {
		t.Fatalf("%d jobs known, want 1", got)
	}
}

// tmpPrepare is okPrepare that also writes a file into tmp and reports tmp to the test.
func tmpPrepare(t *testing.T, tmps *[]string) prepareFunc {
	return func(ctx context.Context, pr pullRequest, tmp string) (workspace, error) {
		*tmps = append(*tmps, tmp)
		if err := os.WriteFile(filepath.Join(tmp, "x"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return okPrepare(ctx, pr, tmp)
	}
}

func assertRemoved(t *testing.T, tmps []string) {
	t.Helper()
	if len(tmps) != 1 {
		t.Fatalf("prepare ran %d times, want 1", len(tmps))
	}
	if _, err := os.Stat(tmps[0]); !os.IsNotExist(err) {
		t.Errorf("tmp %s not removed: %v", tmps[0], err)
	}
}

func TestRunWorkflowPrepareFailure(t *testing.T) {
	var tmps []string
	prep := func(ctx context.Context, pr pullRequest, tmp string) (workspace, error) {
		tmpPrepare(t, &tmps)(ctx, pr, tmp)
		return workspace{}, errors.New("diff too large for review: 300 KiB, limit 256 KiB")
	}
	st := runWorkflow(context.Background(), pr(1), workflow{noPolicy, prep, neverStep(t), neverStep(t), asRisk(neverStep(t)), time.Minute})
	if st.Findings != nil || st.Status != statusFailed || st.FailedStep != "prepare" || st.Reason != "diff too large for review: 300 KiB, limit 256 KiB" {
		t.Fatalf("result = %+v", st)
	}
	assertRemoved(t, tmps)

	slow := func(ctx context.Context, _ pullRequest, _ string) (workspace, error) {
		<-ctx.Done()
		return workspace{}, ctx.Err()
	}
	if st := runWorkflow(context.Background(), pr(1), workflow{noPolicy, slow, neverStep(t), neverStep(t), asRisk(neverStep(t)), 10 * time.Millisecond}); st.FailedStep != "prepare" || st.Reason != "timed out after 10ms" {
		t.Fatalf("timeout result = %q, %q", st.FailedStep, st.Reason)
	}
}

func TestRunWorkflowFindingsAndCleanup(t *testing.T) {
	var tmps []string
	review := func(sev string) step {
		return func(_ context.Context, _ pullRequest, ws workspace) (stepResult, error) {
			if ws.Dir != filepath.Join(ws.Tmp, "src") || ws.Diff != "diff" || ws.Tmp != tmps[0] {
				t.Errorf("step got workspace %+v", ws)
			}
			return stepResult{Summary: "summary " + sev, Findings: []finding{{Severity: sev, File: "a.go", Line: 1, Description: "d"}}}, nil
		}
	}
	byStep := func(a, b taggedFinding) int { return strings.Compare(a.Step, b.Step) }
	var riskGot []taggedFinding
	riskStep := func(_ context.Context, _ pullRequest, _ workspace, findings []taggedFinding) (stepResult, error) {
		riskGot = slices.SortedFunc(slices.Values(findings), byStep)
		return stepResult{Areas: []areaEntry{{Area: "authentication", File: "b.go", Line: 2, Reason: "login"}}, Points: []pointOfInterest{{File: "c.go", Line: 0, Reason: "extra"}}}, nil
	}
	st := runWorkflow(context.Background(), pr(1), workflow{noPolicy, tmpPrepare(t, &tmps), review("High"), review("Low"), riskStep, time.Minute})
	slices.SortFunc(st.Findings, byStep)
	want := []taggedFinding{{Step: "code review", finding: finding{Severity: "High", File: "a.go", Description: "d", Line: 1}}, {Step: "security review", finding: finding{Severity: "Low", File: "a.go", Description: "d", Line: 1}}}
	if st.Status != statusDone || st.FailedStep != "" || st.Reason != "" || !slices.Equal(st.Findings, want) || st.Summary != "summary High" {
		t.Fatalf("result = %+v", st)
	}
	if !slices.Equal(riskGot, want) {
		t.Errorf("risk step got findings %+v, want %+v", riskGot, want)
	}
	wantRisk := decideRisk(want, []areaEntry{{Area: "authentication", File: "b.go", Line: 2, Reason: "login"}}, []pointOfInterest{{File: "c.go", Line: 0, Reason: "extra"}}, defaultThreshold, defaultSensitiveAreas)
	if !reflect.DeepEqual(st.riskDecision, wantRisk) || st.Risk != "High" || st.Recommendation != recHuman {
		t.Errorf("risk decision = %+v, want %+v", st.riskDecision, wantRisk)
	}
	assertRemoved(t, tmps)

	tmps = nil // failure keeps no findings, even from the steps that succeeded
	fail := func(context.Context, pullRequest, workspace) (stepResult, error) {
		return stepResult{}, errors.New("boom")
	}
	if st := runWorkflow(context.Background(), pr(1), workflow{noPolicy, tmpPrepare(t, &tmps), review("High"), okStep, asRisk(fail), time.Minute}); st.Findings != nil || st.Summary != "" || st.FailedStep != "risk assessment" || st.Reason != "boom" || !reflect.DeepEqual(st.riskDecision, riskDecision{}) {
		t.Fatalf("failure result = %+v", st)
	}
	assertRemoved(t, tmps)

	tmps = nil
	ctx, cancel := context.WithCancel(context.Background())
	block := func(ctx context.Context, _ pullRequest, _ workspace) (stepResult, error) {
		cancel()
		<-ctx.Done()
		return stepResult{}, ctx.Err()
	}
	if st := runWorkflow(ctx, pr(1), workflow{noPolicy, tmpPrepare(t, &tmps), block, okStep, asRisk(neverStep(t)), time.Minute}); st.Findings != nil || st.FailedStep != "code review" {
		t.Fatalf("cancelled result = %+v", st)
	}
	assertRemoved(t, tmps)
}

func TestQueueFindings(t *testing.T) {
	q, f := queueFakes(t, 1)
	f.findings = []finding{{Severity: "High", File: "a.go", Line: 3, Description: "d"}}
	f.summary = "Adds a."
	p := pr(1)
	p.HeadSHA = "h1"
	q.sync([]pullRequest{p})
	if got := q.statuses()[key(1)].HeadSHA; got != "h1" {
		t.Fatalf("queued HeadSHA = %q", got)
	}
	f.next(t).ret <- nil
	waitStatus(t, q, 1, statusDone)
	want := []taggedFinding{{Step: "code review", finding: f.findings[0]}}
	if got := q.statuses()[key(1)]; !slices.Equal(got.Findings, want) || got.Summary != "Adds a." || got.HeadSHA != "h1" ||
		!reflect.DeepEqual(got.riskDecision, decideRisk(want, nil, nil, defaultThreshold, defaultSensitiveAreas)) {
		t.Fatalf("done = %+v, want findings %+v", got, want)
	}
	q.rerun(key(1))
	if got := q.statuses()[key(1)]; got.Findings != nil || got.Summary != "" || got.HeadSHA != "h1" {
		t.Fatalf("re-run kept results %+v", got)
	}
	f.next(t).ret <- errors.New("boom")
	waitStatus(t, q, 1, "failed: code review: boom")
	if got := q.statuses()[key(1)]; got.Findings != nil || got.Summary != "" || got.HeadSHA != "h1" || got.Recommendation != "" {
		t.Fatalf("failed run = %+v", got)
	}
}

func TestNewWorkflowPassesStepConfig(t *testing.T) {
	var cfg config
	cfg.Steps.CodeReview = stepConfig{Agent: "claude", Prompt: "CODE PROMPT"}
	cfg.Steps.SecurityReview = stepConfig{Agent: "codex", Prompt: "SEC PROMPT"}
	cfg.Steps.RiskAssessment = stepConfig{Agent: "codex", Prompt: "RISK PROMPT"}
	type call struct {
		agent, input, dir, tmp, step string
		areas                        []string
	}
	var got []call
	run := func(_ context.Context, agent, input, dir, tmp, step string, areas []string) (stepResult, error) {
		got = append(got, call{agent, input, dir, tmp, step, areas})
		return stepResult{Summary: "S", Findings: []finding{{Severity: "Low", File: "a.go", Description: step, Line: 1}}}, nil
	}
	var fetched []string
	w := newWorkflow(cfg, func(_ context.Context, repo string) (*policy, error) {
		fetched = append(fetched, repo)
		return nil, nil
	}, run)
	pr := pullRequest{Repo: "a/b", Number: 3, Title: "T", HeadSHA: "abc"}
	pol, err := w.policy(context.Background(), pr.Repo)
	if err != nil || !slices.Equal(fetched, []string{"a/b"}) || !reflect.DeepEqual(pol, resolvePolicy(nil, cfg)) {
		t.Fatalf("policy = %+v, %v; fetched %v", pol, err, fetched)
	}
	ws := workspace{Dir: "/t/src", Tmp: "/t", Diff: "DIFF", Description: "DESC", Policy: pol}
	for _, s := range []step{w.codeReview, w.securityReview} {
		if _, err := s(context.Background(), pr, ws); err != nil {
			t.Fatal(err)
		}
	}
	findings := []taggedFinding{{Step: "code review", finding: finding{Severity: "High", File: "a.go", Description: "d", Line: 1}}}
	if _, err := w.riskAssessment(context.Background(), pr, ws, findings); err != nil {
		t.Fatal(err)
	}
	want := []call{
		{"claude", agentInput("CODE PROMPT", pr, "abc", "DESC", "DIFF"), "/t/src", "/t", "code-review", nil},
		{"codex", agentInput("SEC PROMPT", pr, "abc", "DESC", "DIFF"), "/t/src", "/t", "security-review", nil},
		{"codex", riskInput("RISK PROMPT", pr, "abc", "DESC", "DIFF", pol.Areas, findings), "/t/src", "/t", "risk-assessment", defaultSensitiveAreas},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("runner calls = %+v\nwant %+v", got, want)
	}
	res, _ := w.codeReview(context.Background(), pr, ws)
	if len(res.Findings) != 1 || res.Findings[0].Description != "code-review" || res.Summary != "S" {
		t.Errorf("findings = %+v", res.Findings)
	}
	if w.prepare == nil || w.riskAssessment == nil || w.stepTimeout != stepTimeout {
		t.Errorf("workflow not fully built: %+v", w)
	}
}

// fixedPolicy is a policy fetcher that returns the policy parsed from text, or err.
func fixedPolicy(t *testing.T, text string, err error) func(context.Context, string) (*policy, error) {
	return func(context.Context, string) (*policy, error) {
		if err != nil || text == "" {
			return nil, err
		}
		p, perr := parsePolicy([]byte(text))
		if perr != nil {
			t.Fatal(perr)
		}
		return p, nil
	}
}

func TestRunWorkflowAppliesPolicy(t *testing.T) {
	var cfg config
	cfg.Steps.CodeReview = stepConfig{Agent: "claude", Prompt: "CODE PROMPT"}
	cfg.Steps.SecurityReview = stepConfig{Agent: "codex", Prompt: "SEC PROMPT"}
	cfg.Steps.RiskAssessment = stepConfig{Agent: "codex", Prompt: "RISK PROMPT"}
	var mu sync.Mutex
	got := map[string][3]string{} // step: agent, input, areas
	run := func(_ context.Context, agent, input, _, _, step string, areas []string) (stepResult, error) {
		mu.Lock()
		defer mu.Unlock()
		got[step] = [3]string{agent, input, strings.Join(areas, ",")}
		return stepResult{}, nil // the agent reports no sensitive area
	}
	w := newWorkflow(cfg, fixedPolicy(t, `{"sensitive_areas":[{"name":"payments","paths":["billing/**"]}],
		"steps":{"security_review":{"agent":"claude","prompt":"POLICY SEC"},"risk_assessment":{"prompt":"POLICY RISK"}}}`, nil), run)
	diff := "diff --git a/billing/pay.go b/billing/pay.go\n--- a/billing/pay.go\n+++ b/billing/pay.go\n@@ -1 +1 @@\n-a\n+b\n"
	w.prepare = func(_ context.Context, _ pullRequest, tmp string) (workspace, error) {
		return workspace{Dir: filepath.Join(tmp, "src"), Tmp: tmp, Diff: diff}, nil
	}
	p := pullRequest{Repo: "o/r", Number: 1, HeadSHA: "abc"}
	st := runWorkflow(context.Background(), p, w)
	if st.Status != statusDone || st.Threshold != "High" || st.ThresholdSource != policySourceDefault ||
		!slices.Equal(st.SensitiveAreas, []string{"payments"}) || st.AreasSource != policySourceRepo {
		t.Fatalf("result = %+v", st)
	}
	if g := got["code-review"]; g[0] != "claude" || !strings.HasPrefix(g[1], "CODE PROMPT\n\n## Pull request\n") {
		t.Errorf("code review got %q", g)
	}
	if g := got["security-review"]; g[0] != "claude" || g[1] != agentInput("POLICY SEC", p, "abc", "", diff) {
		t.Errorf("security review got %q", g)
	}
	if g := got["risk-assessment"]; g[0] != "codex" || !strings.HasPrefix(g[1], "POLICY RISK\n\n## Pull request\n") || g[2] != "payments" ||
		!strings.Contains(g[1], `[{"name":"payments","description":"","paths":["billing/**"]}]`) {
		t.Errorf("risk assessment got %q", g)
	}
	// The Go path match makes the area touched although the agent reported none.
	if !slices.Contains(st.Rationale, "Touches sensitive areas: payments.") || st.Recommendation != recHuman ||
		len(st.Points) != 1 || st.Points[0] != (pointOfInterest{File: "billing/pay.go", Line: 0, Reason: "Sensitive area payments: changed file matches billing/**",
		Title: "Sensitive area payments", Comment: "This change touches payments code. Please confirm it has been checked with that in mind."}) {
		t.Errorf("decision = %+v", st.riskDecision)
	}
}

func TestRunWorkflowIgnoresCheckoutPolicy(t *testing.T) {
	w := newWorkflow(config{}, fixedPolicy(t, "", nil), nil) // 404: no policy on the default branch
	w.prepare = func(_ context.Context, _ pullRequest, tmp string) (workspace, error) {
		dir := filepath.Join(tmp, "src")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return workspace{}, err
		}
		return workspace{Dir: dir, Tmp: tmp}, os.WriteFile(filepath.Join(dir, ".myreview.json"), []byte(`{"severity_threshold":"Low","sensitive_areas":[]}`), 0o600)
	}
	w.codeReview, w.securityReview, w.riskAssessment = okStep, okStep, asRisk(okStep)
	st := runWorkflow(context.Background(), pr(1), w)
	if st.Status != statusDone || st.Threshold != "High" || st.ThresholdSource != policySourceDefault ||
		!slices.Equal(st.SensitiveAreas, defaultSensitiveAreas) || st.AreasSource != policySourceDefault {
		t.Fatalf("result = %+v", st)
	}
}

func TestRunWorkflowPolicyFailures(t *testing.T) {
	_, invalid := parsePolicy([]byte(`{"severity_threshold":"high"}`))
	long := strings.Repeat("x", 400)
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"gh error", errors.New("gh: Server Error (HTTP 502)"), "gh: Server Error (HTTP 502)"},
		{"invalid file", invalid, `invalid .myreview.json on the default branch: severity_threshold "high" is not Low, Medium, High or Critical`},
		{"long reason", errors.New(long), long[:300]},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorkflow(config{}, fixedPolicy(t, "", c.err), nil)
			w.prepare = func(context.Context, pullRequest, string) (workspace, error) {
				t.Error("prepare must not run")
				return workspace{}, nil
			}
			st := runWorkflow(context.Background(), pr(1), w)
			if st.Status != statusFailed || st.FailedStep != "policy" || st.Reason != c.want || st.Threshold != "" {
				t.Fatalf("result = %+v", st)
			}
			if got, want := formatStatus(st), "failed: policy: "+c.want; got != want {
				t.Errorf("status = %q, want %q", got, want)
			}
		})
	}
	w := workflow{func(ctx context.Context, _ string) (effectivePolicy, error) {
		<-ctx.Done()
		return effectivePolicy{}, ctx.Err()
	},
		okPrepare, neverStep(t), neverStep(t), asRisk(neverStep(t)), 10 * time.Millisecond}
	if st := runWorkflow(context.Background(), pr(1), w); st.FailedStep != "policy" || st.Reason != "timed out after 10ms" {
		t.Errorf("slow fetch = %+v", st)
	}
}

func TestRunWorkflowKeepsPolicyItStartedWith(t *testing.T) {
	var answer atomic.Value
	answer.Store(`{"severity_threshold":"Low"}`)
	var fetches atomic.Int32
	w := newWorkflow(config{}, func(context.Context, string) (*policy, error) {
		fetches.Add(1)
		return parsePolicy([]byte(answer.Load().(string)))
	}, nil)
	f := newFakes()
	w.prepare, w.codeReview, w.securityReview, w.riskAssessment = okPrepare, f.step("code review"), okStep, asRisk(okStep)
	done := make(chan jobStatus)
	go func() { done <- runWorkflow(context.Background(), pr(1), w) }()
	c := f.next(t)
	answer.Store(`{"severity_threshold":"Critical"}`) // the default branch changes while the review runs
	c.ret <- nil
	if st := <-done; st.Threshold != "Low" || st.ThresholdSource != policySourceRepo || fetches.Load() != 1 {
		t.Fatalf("result = %+v after %d fetches", st, fetches.Load())
	}
	go func() { done <- runWorkflow(context.Background(), pr(1), w) }() // the next run reads it again
	f.next(t).ret <- nil
	if st := <-done; st.Threshold != "Critical" || fetches.Load() != 2 {
		t.Fatalf("second result = %+v after %d fetches", st, fetches.Load())
	}
}

func TestQueueShutdown(t *testing.T) {
	dir := t.TempDir()
	q, f := queueIn(t, dir, 1)
	q.sync([]pullRequest{pr(1), pr(2)})
	c1 := f.next(t)
	q.shutdown(5 * time.Second)
	if !waitDone(c1.ctx) {
		t.Fatal("running job not cancelled")
	}
	q.mu.Lock()
	running := q.running
	q.mu.Unlock()
	if running != 0 {
		t.Fatalf("shutdown returned with %d running", running)
	}
	if _, ok := q.statuses()[key(1)]; ok {
		t.Error("cancelled job still known")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("shutdown saved %d results; the cancelled run must be queued again on the next start", len(entries))
	}
	select {
	case c := <-f.calls:
		t.Errorf("queued %s started after shutdown", c.pr)
	case <-time.After(100 * time.Millisecond):
	}
}
