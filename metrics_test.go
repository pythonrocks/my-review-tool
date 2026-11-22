package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func fp(f float64) *float64 { return &f }

// A trimmed copy of a real claude 2.1.329 envelope, with two models.
const claudeTwoModels = `{"type":"result","subtype":"success","is_error":false,"duration_ms":5000,"duration_api_ms":4200,"num_turns":3,` +
	`"result":"","session_id":"s1","total_cost_usd":0.42,` +
	`"usage":{"input_tokens":12,"cache_creation_input_tokens":100,"cache_read_input_tokens":2000,"output_tokens":300},` +
	`"modelUsage":{"claude-opus-5-5":{"inputTokens":10,"outputTokens":250,"cacheReadInputTokens":2000,"cacheCreationInputTokens":100,"costUSD":0.4},` +
	`"claude-haiku-5-5":{"inputTokens":2,"outputTokens":50,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":0.02}},` +
	`"structured_output":{"summary":"S","findings":[]}}`

func TestRunAgentClaudeUsage(t *testing.T) {
	for _, tc := range []struct {
		name, body, err, kind string
		want                  *agentUsage
	}{
		{"two models", `echo '` + claudeTwoModels + `'`, "", "", &agentUsage{
			Models: []modelUsage{{"claude-haiku-5-5", tokens{2, 50, 0, 0}, fp(0.02)}, {"claude-opus-5-5", tokens{10, 250, 2000, 100}, fp(0.4)}},
			APIMs:  4200, Turns: 3, tokens: tokens{12, 300, 2000, 100}, CostUSD: fp(0.42)}},
		{"is_error with cost", `echo '{"is_error":true,"result":"Credit balance is too low","total_cost_usd":0.05,"num_turns":1,` +
			`"usage":{"input_tokens":7,"output_tokens":1}}'; exit 1`, "Credit balance is too low", errAgentError,
			&agentUsage{Models: []modelUsage{}, Turns: 1, tokens: tokens{InputTokens: 7, OutputTokens: 1}, CostUSD: fp(0.05)}},
		{"no cost", `echo '{"structured_output":{"summary":"S","findings":[]}}'`, "", "", &agentUsage{Models: []modelUsage{}}},
		{"mistyped metrics field", `echo '{"num_turns":"many","total_cost_usd":0.1,"structured_output":{"summary":"S","findings":[]}}'`, "", "",
			&agentUsage{Models: []modelUsage{}, CostUSD: fp(0.1)}},
		{"bad output keeps usage", `echo '{"total_cost_usd":0.1,"structured_output":{"findings":[]}}'`, "summary is missing", errBadOutput,
			&agentUsage{Models: []modelUsage{}, CostUSD: fp(0.1)}},
		{"undecodable envelope", `echo 'not json'`, "invalid character", errBadOutput, nil},
		{"exit without envelope", `echo boom >&2; exit 3`, "claude exited with status 3: boom", errExit, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeAgent(t, "claude", tc.body)
			res, err := runAgent(context.Background(), "claude", "x", t.TempDir(), t.TempDir(), "code-review", nil)
			if (tc.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.err)) || res.ErrorKind != tc.kind {
				t.Fatalf("err = %v, kind %q; want %q, %q", err, res.ErrorKind, tc.err, tc.kind)
			}
			if !reflect.DeepEqual(res.Usage, tc.want) {
				t.Errorf("usage = %s\nwant %s", js(res.Usage), js(tc.want))
			}
		})
	}
}

func js(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// codexJSONL prints more than 1 MiB of items, a turn.completed line over 64 KiB (discarded unparsed),
// a warning error item and two turn.completed events.
const codexJSONL = `echo '{"type":"thread.started","thread_id":"t"}'
echo '{"type":"turn.started"}'
awk 'BEGIN { s = sprintf("%1000s", ""); gsub(/ /, "x", s); for (i = 0; i < 1100; i++) printf "{\"type\":\"item.completed\",\"item\":{\"type\":\"command_execution\",\"aggregated_output\":\"%s\"}}\n", s }'
awk 'BEGIN { s = sprintf("%70000s", ""); gsub(/ /, "x", s); printf "{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":999999},\"pad\":\"%s\"}\n", s }'
echo '{"type":"item.completed","item":{"id":"w","type":"error","message":"warning: model metadata not found"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":40,"cache_write_input_tokens":5,"output_tokens":20,"reasoning_output_tokens":7}}'
echo '{"type":"turn.completed","usage":{"input_tokens":50,"cached_input_tokens":10,"output_tokens":5}}'
`

func TestRunAgentCodexUsage(t *testing.T) {
	rec := fakeAgent(t, "codex", codexJSONL+`echo '{"findings":[]}' > "$out"`)
	tmp := t.TempDir()
	res, err := runAgent(context.Background(), "codex", "x", t.TempDir(), tmp, "security-review", nil)
	if err != nil {
		t.Fatal(err)
	}
	if args := strings.Split(strings.TrimSuffix(readFile(t, rec+"/args"), "\x00"), "\x00"); !slices.Equal(args[len(args)-4:], []string{"-o", tmp + "/security-review.out.json", "--json", "-"}) {
		t.Errorf("args = %q", args)
	}
	sum := tokens{InputTokens: 150, OutputTokens: 25, CacheReadTokens: 50, CacheWriteTokens: 5}
	want := &agentUsage{Models: []modelUsage{{Model: "codex default", tokens: sum}}, Turns: 2, tokens: sum}
	if !reflect.DeepEqual(res.Usage, want) {
		t.Errorf("usage = %s\nwant %s", js(res.Usage), js(want))
	}
	if b, _ := json.Marshal(res.Usage); strings.Contains(string(b), "cost_usd") {
		t.Errorf("codex usage has a cost: %s", b)
	}
}

func TestRunAgentCodexFailureEvents(t *testing.T) {
	for _, tc := range []struct {
		name, body, err string
		usage           bool
	}{
		{"turn.failed, no stderr", `echo '{"type":"turn.completed","usage":{"input_tokens":3}}'; echo '{"type":"turn.failed","error":{"message":"rate limited"}}'; exit 1`,
			"codex exited with status 1: rate limited", true},
		{"error event, no stderr", `echo '{"type":"error","message":"stream disconnected"}'; exit 2`, "codex exited with status 2: stream disconnected", false},
		{"stderr wins", `echo '{"type":"error","message":"event"}'; echo 'auth failed' >&2; exit 1`, "codex exited with status 1: auth failed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeAgent(t, "codex", tc.body)
			res, err := runAgent(context.Background(), "codex", "x", t.TempDir(), t.TempDir(), "code-review", nil)
			if err == nil || err.Error() != tc.err || res.ErrorKind != errExit || (res.Usage != nil) != tc.usage {
				t.Errorf("err = %v, kind %q, usage %s", err, res.ErrorKind, js(res.Usage))
			}
		})
	}
}

func TestRunAgentCodexLastLineNoNewline(t *testing.T) {
	fakeAgent(t, "codex", `echo '{"findings":[]}' > "$out"; printf '%s' '{"type":"turn.completed","usage":{"input_tokens":7,"output_tokens":3}}'`)
	res, err := runAgent(context.Background(), "codex", "x", t.TempDir(), t.TempDir(), "security-review", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage == nil || res.Usage.InputTokens != 7 || res.Usage.OutputTokens != 3 || res.Usage.Turns != 1 {
		t.Errorf("usage = %s", js(res.Usage))
	}
}

func TestCodexEventsSplitWrites(t *testing.T) {
	var c codexEvents
	in := "{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":4,\"output_tokens\":2}}\n" + `{"type":"turn.failed","error":{"message":"m"}}` + "\npartial"
	for i := range len(in) {
		c.Write([]byte{in[i]})
	}
	if c.usage == nil || c.usage.InputTokens != 4 || c.usage.OutputTokens != 2 || c.usage.Turns != 1 || c.failure != "m" || string(c.line) != "partial" {
		t.Errorf("events = %+v, usage %s", c, js(c.usage))
	}
}

func TestRunAgentErrorKinds(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for agent, want := range map[string]string{"claude": errNotFound, "codex": errNotFound, "gemini": errNotFound} {
		if res, err := runAgent(context.Background(), agent, "x", t.TempDir(), t.TempDir(), "s", nil); err == nil || res.ErrorKind != want || res.Usage != nil {
			t.Errorf("%s: %v, kind %q", agent, err, res.ErrorKind)
		}
	}
}

func promptHash(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }

func TestRunWorkflowMetricsDone(t *testing.T) {
	u := &agentUsage{Models: []modelUsage{}, Turns: 1, CostUSD: fp(0.1)}
	review := func(_ context.Context, _ pullRequest, _ workspace) (stepResult, error) {
		time.Sleep(2 * time.Millisecond)
		return stepResult{Findings: []finding{{Severity: "Low", File: "a.go", Description: "d"}}, Usage: u, Agent: "claude", PromptSHA: "p"}, nil
	}
	risk := func(context.Context, pullRequest, workspace, []taggedFinding) (stepResult, error) {
		return stepResult{Areas: []areaEntry{{Area: "authentication", File: "b.go"}}, Points: []pointOfInterest{{File: "c.go"}, {File: "d.go"}}}, nil
	}
	diff := "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-a\n+b\n"
	prep := func(_ context.Context, _ pullRequest, tmp string) (workspace, error) {
		return workspace{Tmp: tmp, Diff: diff}, nil
	}
	before := time.Now().UTC()
	st := runWorkflow(context.Background(), pr(1), workflow{noPolicy, prep, review, review, risk, time.Minute})
	m := st.Metrics
	if st.Status != statusDone || m == nil || m.DiffFiles != 1 || m.DiffBytes != len(diff) || m.StartedAt.Before(before) || m.FinishedAt.Before(m.StartedAt) {
		t.Fatalf("metrics = %s", js(m))
	}
	if names := []string{m.Steps[0].Step, m.Steps[1].Step, m.Steps[2].Step}; len(m.Steps) != 3 || !slices.Equal(names, []string{"code review", "security review", "risk assessment"}) {
		t.Fatalf("steps = %s", js(m.Steps))
	}
	for i, s := range m.Steps {
		if s.Status != statusDone || s.ErrorKind != "" || s.Error != "" || s.FinishedAt.Before(s.StartedAt) || s.StartedAt.Before(m.StartedAt) {
			t.Errorf("step %d = %s", i, js(s))
		}
	}
	if s := m.Steps[0]; s.Items != 1 || s.Usage != u || s.Agent != "claude" || s.PromptSHA != "p" || s.WallMs < 2 {
		t.Errorf("code review = %s", js(s))
	}
	if s := m.Steps[2]; s.Items != 3 || s.Usage != nil {
		t.Errorf("risk = %s", js(s))
	}
}

func TestRunWorkflowMetricsFailedAndCancelledSibling(t *testing.T) {
	u := &agentUsage{Models: []modelUsage{}, CostUSD: fp(0.05)}
	fail := func(context.Context, pullRequest, workspace) (stepResult, error) {
		return stepResult{Usage: u, ErrorKind: errAgentError, Agent: "claude", PromptSHA: "p"}, errors.New("Credit balance is too low")
	}
	block := func(ctx context.Context, _ pullRequest, _ workspace) (stepResult, error) {
		<-ctx.Done()
		return stepResult{Agent: "codex", PromptSHA: "q"}, ctx.Err()
	}
	st := runWorkflow(context.Background(), pr(1), workflow{noPolicy, okPrepare, fail, block, asRisk(neverStep(t)), time.Minute})
	if st.Status != statusFailed || st.FailedStep != "code review" || st.Metrics == nil || len(st.Metrics.Steps) != 2 {
		t.Fatalf("result = %+v, metrics %s", st, js(st.Metrics))
	}
	if s := st.Metrics.Steps[0]; s.Status != statusFailed || s.ErrorKind != errAgentError || s.Error != "Credit balance is too low" || s.Usage != u || s.PromptSHA != "p" {
		t.Errorf("failed step = %s", js(s))
	}
	if s := st.Metrics.Steps[1]; s.Status != statusCancelled || s.ErrorKind != errCancelled || s.Usage != nil || s.Agent != "codex" || s.PromptSHA != "q" {
		t.Errorf("cancelled sibling = %s", js(s))
	}

	// An error without a kind is "other".
	plain := func(context.Context, pullRequest, workspace) (stepResult, error) {
		return stepResult{}, errors.New("boom")
	}
	st = runWorkflow(context.Background(), pr(1), workflow{noPolicy, okPrepare, okStep, okStep, asRisk(plain), time.Minute})
	if s := st.Metrics.Steps[2]; len(st.Metrics.Steps) != 3 || s.Step != "risk assessment" || s.Status != statusFailed || s.ErrorKind != errOther {
		t.Errorf("risk step = %s", js(st.Metrics.Steps))
	}
}

func TestRunWorkflowMetricsTimeout(t *testing.T) {
	block := func(ctx context.Context, _ pullRequest, _ workspace) (stepResult, error) {
		<-ctx.Done()
		return stepResult{}, ctx.Err()
	}
	st := runWorkflow(context.Background(), pr(1), workflow{noPolicy, okPrepare, okStep, block, asRisk(neverStep(t)), 10 * time.Millisecond})
	if s := st.Metrics.Steps[1]; s.Status != statusFailed || s.ErrorKind != errTimeout || s.Usage != nil || s.WallMs < 10 || s.Error != "timed out after 10ms" {
		t.Errorf("timed out step = %s", js(s))
	}
	if s := st.Metrics.Steps[0]; s.Status != statusDone {
		t.Errorf("sibling = %s", js(s))
	}
}

func TestRunWorkflowMetricsWholeRunCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	block := func(ctx context.Context, _ pullRequest, _ workspace) (stepResult, error) {
		cancel() // Re-run, a new commit or shutdown
		<-ctx.Done()
		return stepResult{}, ctx.Err()
	}
	st := runWorkflow(ctx, pr(1), workflow{noPolicy, okPrepare, block, block, asRisk(neverStep(t)), time.Minute})
	for _, s := range st.Metrics.Steps {
		if s.Status != statusCancelled || s.ErrorKind != errCancelled {
			t.Errorf("step = %s", js(s))
		}
	}
}

func TestRunWorkflowMetricsPolicyAndPrepareFailures(t *testing.T) {
	badPolicy := func(context.Context, string) (effectivePolicy, error) {
		return effectivePolicy{}, errors.New("gh: 502")
	}
	badPrepare := func(context.Context, pullRequest, string) (workspace, error) {
		return workspace{}, errors.New("too large")
	}
	for _, w := range []workflow{
		{badPolicy, okPrepare, neverStep(t), neverStep(t), asRisk(neverStep(t)), time.Minute},
		{noPolicy, badPrepare, neverStep(t), neverStep(t), asRisk(neverStep(t)), time.Minute},
	} {
		st := runWorkflow(context.Background(), pr(1), w)
		if st.Status != statusFailed || st.Metrics == nil || st.Metrics.FinishedAt.IsZero() || !strings.Contains(js(st.Metrics), `"steps":[]`) {
			t.Errorf("%s: metrics = %s", st.FailedStep, js(st.Metrics))
		}
	}
}

func TestNewWorkflowPromptSHAAndErrorKind(t *testing.T) {
	var cfg config
	cfg.Steps.CodeReview = stepConfig{Agent: "claude", Prompt: "CODE PROMPT"}
	cfg.Steps.SecurityReview = stepConfig{Agent: "claude", Prompt: "SEC PROMPT"}
	cfg.Steps.RiskAssessment = stepConfig{Agent: "codex", Prompt: "RISK PROMPT"}
	run := func(_ context.Context, agent, _, _, _, step string, _ []string) (stepResult, error) {
		if step == "risk-assessment" {
			return stepResult{ErrorKind: errNotFound}, errors.New(agent + " not found in PATH")
		}
		return stepResult{}, nil
	}
	shas := func(policyText string) []string {
		w := newWorkflow(cfg, fixedPolicy(t, policyText, nil), run)
		w.prepare = okPrepare
		st := runWorkflow(context.Background(), pr(1), w)
		if len(st.Metrics.Steps) != 3 {
			t.Fatalf("steps = %s", js(st.Metrics.Steps))
		}
		if s := st.Metrics.Steps[2]; s.Status != statusFailed || s.ErrorKind != errNotFound || s.Agent != "codex" {
			t.Errorf("risk = %s", js(s))
		}
		var out []string
		for _, s := range st.Metrics.Steps {
			out = append(out, s.PromptSHA)
		}
		return out
	}
	if got, want := shas(`{"steps":{"security_review":{"prompt":"POLICY SEC v1"}}}`), []string{promptHash("CODE PROMPT"), promptHash("POLICY SEC v1"), promptHash("RISK PROMPT")}; !slices.Equal(got, want) {
		t.Errorf("prompt_sha = %q, want %q", got, want)
	}
	if got := shas(`{"steps":{"security_review":{"prompt":"POLICY SEC v2"}}}`); got[1] != promptHash("POLICY SEC v2") || got[0] != promptHash("CODE PROMPT") {
		t.Errorf("prompt_sha after the override changed = %q", got)
	}
}

func TestQueueMetrics(t *testing.T) {
	dir := t.TempDir()
	q, f := queueIn(t, dir, 1)
	run := func(q *reviewQueue, f *fakes) *reviewMetrics {
		t.Helper()
		f.next(t).ret <- nil
		waitStatus(t, q, 1, statusDone)
		return q.statuses()[key(1)].Metrics
	}
	q.sync([]pullRequest{hpr(1, "a")})
	m1 := run(q, f)
	if m1.Attempt != 1 || m1.RunID == "" || m1.QueuedAt.IsZero() || m1.StartedAt.Before(m1.QueuedAt) {
		t.Fatalf("first run = %s", js(m1))
	}
	q.rerun(key(1))
	m2 := run(q, f)
	if m2.Attempt != 2 || m2.RunID == m1.RunID || !m2.QueuedAt.After(m1.QueuedAt) {
		t.Fatalf("re-run = %s", js(m2))
	}

	q2, f2 := queueIn(t, dir, 1) // a restart
	q2.sync([]pullRequest{hpr(1, "a")})
	if m := q2.statuses()[key(1)].Metrics; m == nil || m.RunID != m2.RunID {
		t.Fatalf("restored = %s", js(m))
	}
	q2.rerun(key(1))
	if m := run(q2, f2); m.Attempt != 3 {
		t.Fatalf("re-run after restart = %s", js(m))
	}
	q2.sync([]pullRequest{hpr(1, "b")}) // a new head
	if m := run(q2, f2); m.Attempt != 1 {
		t.Fatalf("new head = %s", js(m))
	}
}

// A file saved before metrics existed loads without them and is not reviewed again.
func TestQueueOldFileWithoutMetrics(t *testing.T) {
	dir := t.TempDir()
	q, f := queueIn(t, dir, 1)
	os.WriteFile(q.store.file(key(1)), []byte(`{"repo":"o/r","number":1,"head_sha":"a","status":"done","summary":"","findings":[],`+
		`"risk":"Low","recommendation":"automated review is sufficient","rationale":["r"],"points_of_interest":[],`+
		`"finished_at":"2026-11-01T00:00:00Z","threshold":"High","threshold_source":"default","sensitive_areas":[],"sensitive_areas_source":"default"}`), 0600)
	q.sync([]pullRequest{hpr(1, "a")})
	if st := q.statuses()[key(1)]; st.Status != statusDone || st.Metrics != nil {
		t.Fatalf("restored = %+v", st)
	}
	noCall(t, f)
	q.rerun(key(1))
	f.next(t).ret <- nil
	waitStatus(t, q, 1, statusDone)
	if m := q.statuses()[key(1)].Metrics; m.Attempt != 1 {
		t.Errorf("re-run of an old file = %s", js(m))
	}
}

func TestStoreRoundTripMetrics(t *testing.T) {
	s := newStore(t)
	at := time.Date(2026, 11, 20, 17, 0, 0, 0, time.UTC)
	m := &reviewMetrics{RunID: "r1", QueuedAt: at, StartedAt: at.Add(time.Second), FinishedAt: at.Add(time.Minute), Attempt: 2, DiffFiles: 3, DiffBytes: 400,
		Steps: []stepMetrics{
			{Step: "code review", Agent: "claude", PromptSHA: "p", StartedAt: at, FinishedAt: at, WallMs: 5, Status: statusDone, Items: 2,
				Usage: &agentUsage{Models: []modelUsage{{"m", tokens{1, 2, 3, 4}, fp(0.1)}}, APIMs: 4, Turns: 1, tokens: tokens{1, 2, 3, 4}, CostUSD: fp(0.1)}},
			{Step: "security review", Agent: "codex", StartedAt: at, FinishedAt: at, Status: statusCancelled, ErrorKind: errCancelled, Error: "context canceled"},
		}}
	st := jobStatus{Status: statusFailed, FailedStep: "security review", Reason: "x", HeadSHA: "abc123", Metrics: m}
	s.save(storePR, st)
	got, ok := s.load(storePR)
	if !ok || !reflect.DeepEqual(got, st) {
		t.Fatalf("load = %s, %v; want %s", js(got.Metrics), ok, js(m))
	}
	data := readFile(t, s.file(prKey(storePR)))
	for _, field := range []string{`"metrics":{"run_id":"r1","queued_at":"2026-11-20T17:00:00Z"`, `"attempt":2`, `"diff_files":3`, `"diff_bytes":400`,
		`"prompt_sha":"p"`, `"wall_ms":5`, `"error_kind":"cancelled"`, `"models":[{"model":"m","input_tokens":1,"output_tokens":2,"cache_read_tokens":3,"cache_write_tokens":4,"cost_usd":0.1}]`} {
		if !strings.Contains(data, field) {
			t.Errorf("file lacks %s: %s", field, data)
		}
	}
	if strings.Count(data, `"usage"`) != 1 {
		t.Errorf("a step without a report must have no usage: %s", data)
	}
}
