package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"
)

const stepTimeout = 10 * time.Minute

const (
	statusQueued   = "queued"
	statusRunning  = "running"
	statusDone     = "done"
	statusFailed   = "failed"
	statusSkipped  = "skipped"   // matches a skip rule; never saved, recomputed on every sync
	statusOnDemand = "on demand" // no saved result and older than maxAge; never saved, recomputed on every sync
)

type stepResult struct {
	Summary  string // code review only
	Findings []finding
	Areas    []areaEntry       // risk assessment only
	Points   []pointOfInterest // risk assessment only
	// Metrics, also set on error returns.
	Usage            *agentUsage // nil when the agent produced no final report
	ErrorKind        string      // of a failed run; runStep sets errTimeout and errCancelled
	Agent, PromptSHA string      // set by agentStep and the risk step of newWorkflow
}

// taggedFinding is a finding with the step that reported it.
type taggedFinding struct {
	Step string
	finding
	PostedURL string // the PR comment it was posted as, if any
}

// step runs one review step on a PR in ws. It must return promptly once ctx is done.
type step func(ctx context.Context, pr pullRequest, ws workspace) (stepResult, error)

// riskStep is the risk assessment step; it also gets both reviews' findings.
type riskStep func(ctx context.Context, pr pullRequest, ws workspace, findings []taggedFinding) (stepResult, error)

// policyFunc returns the policy a workflow run applies to repo; an error fails the run at the policy phase.
type policyFunc func(ctx context.Context, repo string) (effectivePolicy, error)

type workflow struct {
	policy                     policyFunc
	prepare                    prepareFunc
	codeReview, securityReview step
	riskAssessment             riskStep
	stepTimeout                time.Duration
}

// agentRunner is runAgent; tests replace it. areas are the sensitive area names risk assessment may report.
type agentRunner func(ctx context.Context, agent, input, dir, tmp, step string, areas []string) (stepResult, error)

// agentStep is a review step that runs the agent with the prompt pick selects from the run's policy;
// name is the filename-safe step name.
func agentStep(run agentRunner, pick func(effectivePolicy) stepConfig, name string) step {
	return func(ctx context.Context, pr pullRequest, ws workspace) (stepResult, error) {
		sc := pick(ws.Policy)
		res, err := run(ctx, sc.Agent, agentInput(sc.Prompt, pr, pr.HeadSHA, ws.Description, ws.Diff), ws.Dir, ws.Tmp, name, nil)
		return withStepConfig(res, sc), err
	}
}

// withStepConfig returns res with sc's agent and the hash of its resolved prompt.
func withStepConfig(res stepResult, sc stepConfig) stepResult {
	res.Agent, res.PromptSHA = sc.Agent, fmt.Sprintf("%x", sha256.Sum256([]byte(sc.Prompt)))
	return res
}

// newWorkflow builds the real workflow from cfg; fetch reads a repository's policy file (ghPolicy, or a fake in tests).
func newWorkflow(cfg config, fetch func(ctx context.Context, repo string) (*policy, error), run agentRunner) workflow {
	pol := func(ctx context.Context, repo string) (effectivePolicy, error) {
		p, err := fetch(ctx, repo)
		if err != nil {
			return effectivePolicy{}, err
		}
		return resolvePolicy(p, cfg), nil
	}
	risk := func(ctx context.Context, pr pullRequest, ws workspace, findings []taggedFinding) (stepResult, error) {
		sc := ws.Policy.RiskAssessment
		input := riskInput(sc.Prompt, pr, pr.HeadSHA, ws.Description, ws.Diff, ws.Policy.Areas, findings)
		res, err := run(ctx, sc.Agent, input, ws.Dir, ws.Tmp, "risk-assessment", areaNames(ws.Policy.Areas))
		return withStepConfig(res, sc), err
	}
	return workflow{pol, ghPrepare,
		agentStep(run, func(p effectivePolicy) stepConfig { return p.CodeReview }, "code-review"),
		agentStep(run, func(p effectivePolicy) stepConfig { return p.SecurityReview }, "security-review"), risk, stepTimeout}
}

// runTimed runs f with its own timeout and returns the failure reason, if any.
func runTimed(ctx context.Context, timeout time.Duration, f func(context.Context) error) (reason string, failed bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := f(ctx); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Sprintf("timed out after %s", timeout), true
		}
		return cutRunes(err.Error(), maxReason), true
	}
	return "", false
}

// runStep runs s, named name, under timeout and returns its result and metrics; a failure has
// status statusFailed or, when ctx was done, statusCancelled, and the reason in Error.
func runStep(ctx context.Context, name string, s step, pr pullRequest, ws workspace, timeout time.Duration) (res stepResult, m stepMetrics) {
	start := time.Now()
	var stepCtx context.Context
	reason, failed := runTimed(ctx, timeout, func(ctx context.Context) (err error) {
		stepCtx = ctx
		res, err = s(ctx, pr, ws)
		return err
	})
	m = stepMetrics{Step: name, Agent: res.Agent, PromptSHA: res.PromptSHA, StartedAt: start.UTC(), FinishedAt: time.Now().UTC(),
		WallMs: time.Since(start).Milliseconds(), Status: statusDone, Usage: res.Usage}
	switch {
	case !failed:
		m.Items = len(res.Findings) + len(res.Areas) + len(res.Points)
		return res, m
	case stepCtx.Err() == context.DeadlineExceeded: // stays set after runTimed's cancel
		m.Status, m.ErrorKind = statusFailed, errTimeout
	case ctx.Err() != nil:
		m.Status, m.ErrorKind = statusCancelled, errCancelled
	default:
		m.Status, m.ErrorKind = statusFailed, cmp.Or(res.ErrorKind, errOther)
	}
	m.Error = reason
	return res, m
}

// runWorkflow reads the repository policy, prepares the workspace, runs code and security review in parallel on it,
// then risk assessment with their findings if both succeeded. It returns a failed status with the first
// failing step and its reason, or a done status with the findings, the code review's summary and the
// risk decision. The workspace is removed before it returns.
func runWorkflow(ctx context.Context, pr pullRequest, w workflow) jobStatus {
	m := &reviewMetrics{StartedAt: time.Now().UTC(), Steps: []stepMetrics{}}
	fail := func(step, reason string) jobStatus {
		m.FinishedAt = time.Now().UTC()
		return jobStatus{Status: statusFailed, FailedStep: step, Reason: reason, HeadSHA: pr.HeadSHA, Metrics: m}
	}
	// Fetched fresh for every run and kept only for it: a running review keeps the policy it started with.
	var pol effectivePolicy
	if reason, failed := runTimed(ctx, w.stepTimeout, func(ctx context.Context) (err error) {
		pol, err = w.policy(ctx, pr.Repo)
		return err
	}); failed {
		return fail("policy", reason)
	}
	tmp, err := os.MkdirTemp("", "myreview-*")
	if err != nil {
		return fail("prepare", cutRunes(err.Error(), maxReason))
	}
	defer os.RemoveAll(tmp)
	var ws workspace
	if reason, failed := runTimed(ctx, w.stepTimeout, func(ctx context.Context) (err error) {
		ws, err = w.prepare(ctx, pr, tmp)
		return err
	}); failed {
		return fail("prepare", reason)
	}
	ws.Policy = pol
	m.DiffFiles, m.DiffBytes = len(changedFiles(ws.Diff)), len(ws.Diff)

	pairCtx, cancelPair := context.WithCancel(ctx)
	defer cancelPair()
	type result struct {
		i   int
		res stepResult
		m   stepMetrics
	}
	var findings []taggedFinding
	var summary, failedStep, reason string
	results := make(chan result, 2)
	pair := []string{"code review", "security review"}
	for i, s := range []step{w.codeReview, w.securityReview} {
		go func() {
			res, m := runStep(pairCtx, pair[i], s, pr, ws, w.stepTimeout)
			results <- result{i, res, m}
		}()
	}
	pairMetrics := make([]stepMetrics, 2) // in pair order, whichever ends first
	for range 2 {
		// The sibling of the first failure fails with a cancellation error, which is ignored.
		r := <-results
		pairMetrics[r.i] = r.m
		if r.m.Status != statusDone && failedStep == "" {
			failedStep, reason = r.m.Step, r.m.Error
			cancelPair()
		}
		for _, f := range r.res.Findings {
			findings = append(findings, taggedFinding{Step: r.m.Step, finding: f})
		}
		if r.m.Step == "code review" {
			summary = r.res.Summary
		}
	}
	m.Steps = append(m.Steps, pairMetrics...)
	if failedStep != "" {
		return fail(failedStep, reason)
	}
	risk := func(ctx context.Context, pr pullRequest, ws workspace) (stepResult, error) {
		return w.riskAssessment(ctx, pr, ws, findings)
	}
	res, rm := runStep(ctx, "risk assessment", risk, pr, ws, w.stepTimeout)
	m.Steps = append(m.Steps, rm)
	if rm.Status != statusDone {
		return fail("risk assessment", rm.Error)
	}
	// Go path matches go first, so an area with paths is touched even if the agent misses it.
	areas := append(pathMatches(pol.Areas, changedFiles(ws.Diff)), res.Areas...)
	names := areaNames(pol.Areas)
	m.FinishedAt = time.Now().UTC()
	return jobStatus{Status: statusDone, Summary: summary, Findings: findings, HeadSHA: pr.HeadSHA, Metrics: m,
		Threshold: pol.Threshold, ThresholdSource: pol.ThresholdSource, SensitiveAreas: names, AreasSource: pol.AreasSource,
		riskDecision: decideRisk(findings, areas, res.Points, pol.Threshold, names)}
}

type jobStatus struct {
	Status, FailedStep, Reason string
	Summary                    string          // the code review's overview of a done workflow
	Findings                   []taggedFinding // of a done workflow
	HeadSHA                    string          // the commit the workflow reviews or reviewed
	// The policy a done workflow applied; sources are policySourceRepo or policySourceDefault.
	Threshold, ThresholdSource string
	SensitiveAreas             []string // names
	AreasSource                string
	riskDecision               // of a done workflow
	// Outdated is set by the queue: a done or failed result for an older commit than the PR's current head.
	Outdated bool
	SkipRule int // the index of the matching skip rule of a skipped PR
	// Metrics of a done or failed workflow; nil for one saved before metrics existed.
	// runWorkflow fills the run, the queue sets RunID, QueuedAt and Attempt.
	Metrics *reviewMetrics
}

type job struct {
	pr pullRequest
	jobStatus
	cancel context.CancelFunc // set while running
	seenAt time.Time          // when the queue first saw pr's head
	// queuedAt is when the job was queued; attempt counts runs of the same head, from 1.
	queuedAt time.Time
	attempt  int
}

// reviewQueue runs at most limit workflows at once, in FIFO order, one live job per PR.
type reviewQueue struct {
	wf           workflow // guarded by mu; setWorkflow replaces it
	limit        int
	store        resultStore
	pending      *pendingStore      // checked marks and drafts; kept across Re-run and new commits, deleted only by prune
	skip         []skipRule         // PRs matching one are not reviewed
	quietPeriod  time.Duration      // how long a ready PR's head must stay unchanged before its outdated result is reviewed again
	maxAge       time.Duration      // PRs with no saved result created longer ago are not queued automatically; 0 means no limit
	now          func() time.Time   // the clock; tests replace it
	record       func(runRecord)    // called once per finished or dropped run, outside mu; nil records nothing
	recordReview func(reviewRecord) // called once per posted (or maybe posted) review; nil records nothing

	mu      sync.Mutex
	jobs    map[string]*job // by prKey
	fifo    []string        // keys of queued jobs
	running int
}

// shortcut: file I/O under the queue lock; files are small and local, move it out if it shows up in latency.
func newReviewQueue(limit int, wf workflow, store resultStore) *reviewQueue {
	return &reviewQueue{wf: wf, limit: limit, store: store, pending: newPendingStore(store), jobs: map[string]*job{}, now: time.Now}
}

func prKey(pr pullRequest) string { return fmt.Sprintf("%s#%d", pr.Repo, pr.Number) }

// sync restores or queues PRs it has not seen, in the order given (marking ones older than maxAge with no saved result on demand), follows new commits of known PRs,
// queues a ready PR's outdated result once its head has been quiet for quietPeriod,
// marks PRs matching a skip rule as skipped (cancelling their job and deleting their result),
// and cancels and forgets PRs no longer listed.
func (q *reviewQueue) sync(prs []pullRequest) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	open := make(map[string]bool, len(prs))
	for _, pr := range prs {
		k := prKey(pr)
		open[k] = true
		j := q.jobs[k]
		if reason, rule := matchSkip(q.skip, pr); reason != "" {
			if j == nil || j.Status != statusSkipped {
				if j != nil && j.cancel != nil {
					j.cancel() // dispatch drops its result: the job is replaced below
				}
				q.fifo = slices.DeleteFunc(q.fifo, func(s string) bool { return s == k })
				q.store.remove(k)
			}
			q.jobs[k] = &job{pr: pr, jobStatus: jobStatus{Status: statusSkipped, Reason: reason, SkipRule: rule}, seenAt: now}
			continue
		}
		switch {
		case j == nil || j.Status == statusSkipped || j.Status == statusOnDemand:
			if st, ok := q.store.load(pr); ok {
				q.jobs[k] = &job{pr: pr, jobStatus: st, seenAt: now}
				if st.Metrics != nil {
					q.jobs[k].attempt = st.Metrics.Attempt // a Re-run after a restart counts on
				}
			} else if q.maxAge > 0 && now.Sub(pr.Created) > q.maxAge {
				q.jobs[k] = &job{pr: pr, jobStatus: jobStatus{Status: statusOnDemand, Reason: fmt.Sprintf("created more than %s ago", q.maxAge)}, seenAt: now}
			} else {
				q.enqueue(k, pr)
			}
		case pr.HeadSHA != j.pr.HeadSHA:
			j.seenAt = now
			switch j.Status {
			case statusQueued: // keeps its place in the queue
				j.HeadSHA, j.attempt = pr.HeadSHA, 1
			case statusRunning: // its result is dropped by the q.jobs[key] == j check in dispatch
				j.cancel()
				q.enqueue(k, pr)
			}
		}
		j = q.jobs[k]
		j.pr = pr
		j.Outdated = (j.Status == statusDone || j.Status == statusFailed) && pr.HeadSHA != "" && j.HeadSHA != pr.HeadSHA
		if j.Outdated && !pr.Draft && now.Sub(j.seenAt) >= q.quietPeriod {
			q.enqueue(k, pr)
		}
	}
	for k, j := range q.jobs {
		if open[k] {
			continue
		}
		if j.cancel != nil {
			j.cancel()
		}
		delete(q.jobs, k)
		q.store.remove(k)
		q.fifo = slices.DeleteFunc(q.fifo, func(s string) bool { return s == k })
	}
	q.dispatch()
}

// prune deletes saved results and pending reviews of closed PRs in every repo of repos that s fetched successfully.
// It does nothing when s has a global gh error.
func (q *reviewQueue) prune(repos []string, s pollSnapshot) {
	if s.Global != "" {
		return
	}
	open := map[string]bool{}
	for _, pr := range s.PRs {
		open[prKey(pr)] = true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, repo := range repos {
		if _, failed := s.Errors[repo]; !failed {
			q.store.prune(repo, open) // prune matches only repo's files, so other repos' keys in open are harmless
			q.pending.prune(repo, open)
		}
	}
}

// rerun queues a fresh run of the PR, cancelling a running one; it does nothing for a queued or skipped PR.
// It returns false for an unknown PR.
func (q *reviewQueue) rerun(key string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	j := q.jobs[key]
	if j == nil {
		return false
	}
	if j.Status == statusQueued || j.Status == statusSkipped {
		return true
	}
	if j.cancel != nil {
		j.cancel()
	}
	q.enqueue(key, j.pr)
	q.dispatch()
	return true
}

// shutdown stops starting jobs, cancels the running ones and drops their results, so they are queued
// again on the next start, then waits up to wait for them to end (agents killed, temp dirs removed).
func (q *reviewQueue) shutdown(wait time.Duration) {
	q.mu.Lock()
	q.limit = 0
	for k, j := range q.jobs {
		if j.cancel != nil {
			j.cancel()
			delete(q.jobs, k) // dispatch drops the result of a job no longer in q.jobs
		}
	}
	q.mu.Unlock()
	for deadline := time.Now().Add(wait); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		q.mu.Lock()
		n := q.running
		q.mu.Unlock()
		if n == 0 {
			return
		}
	}
}

// setWorkflow makes the next dispatched jobs use wf; running jobs keep theirs.
func (q *reviewQueue) setWorkflow(wf workflow) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.wf = wf
}

// statuses returns a copy of every job's status by prKey.
func (q *reviewQueue) statuses() map[string]jobStatus {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make(map[string]jobStatus, len(q.jobs))
	for k, j := range q.jobs {
		out[k] = j.jobStatus
	}
	return out
}

// enqueue and dispatch need q.mu held.
func (q *reviewQueue) enqueue(key string, pr pullRequest) {
	attempt := 1
	if prev := q.jobs[key]; prev != nil && prev.HeadSHA == pr.HeadSHA {
		attempt = prev.attempt + 1
	}
	now := q.now()
	q.jobs[key] = &job{pr: pr, jobStatus: jobStatus{Status: statusQueued, HeadSHA: pr.HeadSHA}, seenAt: now, queuedAt: now, attempt: attempt}
	q.store.remove(key) // a queued or running job has no file, so it is queued again after a restart
	q.fifo = append(q.fifo, key)
}

func (q *reviewQueue) dispatch() {
	for q.running < q.limit && len(q.fifo) > 0 {
		key := q.fifo[0]
		q.fifo = q.fifo[1:]
		j := q.jobs[key]
		ctx, cancel := context.WithCancel(context.Background())
		j.Status, j.cancel = statusRunning, cancel
		q.running++
		pr, wf := j.pr, q.wf // sync replaces j.pr and setWorkflow q.wf under the lock; a running job keeps its workflow
		runID, queuedAt, attempt := rand.Text(), j.queuedAt.UTC(), j.attempt
		go func() {
			st := runWorkflow(ctx, pr, wf)
			cancel()
			st.Metrics.RunID, st.Metrics.QueuedAt, st.Metrics.Attempt = runID, queuedAt, attempt
			outcome := statusCancelled
			q.mu.Lock()
			if q.jobs[key] == j { // a cancelled or replaced job's result is dropped
				j.cancel = nil
				j.jobStatus = st
				q.store.save(pr, j.jobStatus)
				outcome = st.Status
			}
			record := q.record
			q.mu.Unlock()
			if record != nil {
				record(runRecord{PR: pr, Status: st, Outcome: outcome})
			}
			q.mu.Lock()
			q.running-- // only after record, so shutdown waits until the record is queued before metrics.db closes
			q.dispatch()
			q.mu.Unlock()
		}()
	}
}
