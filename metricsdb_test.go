package main

import (
	"bytes"
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func fptr(f float64) *float64 { return &f }

// sampleRun is a done run of three steps: claude with two priced models, codex (no price), and a
// risk step that timed out without a report.
func sampleRun(runID string, started time.Time) runRecord {
	claude := &agentUsage{APIMs: 900, Turns: 3, tokens: tokens{100, 20, 300, 40}, CostUSD: fptr(0.42), Models: []modelUsage{
		{Model: "claude-haiku", tokens: tokens{40, 5, 100, 10}, CostUSD: fptr(0.12)},
		{Model: "claude-opus", tokens: tokens{60, 15, 200, 30}, CostUSD: fptr(0.30)},
	}}
	codex := &agentUsage{Turns: 2, tokens: tokens{70, 7, 10, 0}, Models: []modelUsage{{Model: "codex default", tokens: tokens{70, 7, 10, 0}}}}
	step := func(name, agent string, u *agentUsage) stepMetrics {
		return stepMetrics{Step: name, Agent: agent, PromptSHA: "sha-" + agent, StartedAt: started, FinishedAt: started.Add(time.Minute),
			WallMs: 60000, Status: statusDone, Items: 2, Usage: u}
	}
	risk := step("risk assessment", "claude", nil)
	risk.Status, risk.ErrorKind, risk.Error = statusFailed, errTimeout, "SECRET agent reason"
	return runRecord{PR: pullRequest{Repo: "o/r", Number: 7, HeadSHA: "abc"}, Outcome: statusFailed,
		Status: jobStatus{Status: statusFailed, FailedStep: "risk assessment", Reason: "SECRET agent reason", Summary: "SECRET summary",
			Findings: []taggedFinding{{Step: "code review", finding: finding{Description: "SECRET finding"}}},
			Metrics: &reviewMetrics{RunID: runID, QueuedAt: started.Add(-time.Second), StartedAt: started, FinishedAt: started.Add(2 * time.Minute),
				Attempt: 1, DiffFiles: 3, DiffBytes: 1000,
				Steps: []stepMetrics{step("code review", "claude", claude), step("security review", "codex", codex), risk}}}}
}

// sampleReview is a COMMENT review of o/r#7 on run runID: code review 2 suggested, 1 as is; risk assessment 1
// suggested, 1 modified; 2 own comments, 1 reply.
func sampleReview(runID string, submitted time.Time) reviewRecord {
	return reviewRecord{SubmittedAt: submitted, OpenedAt: submitted.Add(-90 * time.Second), Repo: "o/r", Number: 7, HeadSHA: "abc",
		RunID: runID, Event: "COMMENT", Total: 5, Checked: 3, Own: 2, Replies: 1, Risk: "High", Recommendation: recHuman,
		Steps: []stepCount{{Step: "code review", Suggested: 2, Accepted: 1}, {Step: "risk assessment", Suggested: 1, Modified: 1}}}
}

func openTestDB(t *testing.T, dir string) *metricsDB {
	t.Helper()
	m, err := openMetricsDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// recordAll records rs and closes m, so every record is written.
func recordAll(t *testing.T, m *metricsDB, rs ...runRecord) {
	for _, r := range rs {
		m.record(r)
	}
	m.close(5 * time.Second)
}

func readDB(t *testing.T, dir string) *sql.DB {
	db, err := sql.Open("sqlite", filepath.Join(dir, "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func count(t *testing.T, db *sql.DB, table string) int {
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMetricsDBMigratesAndReopens(t *testing.T) {
	dir := t.TempDir()
	recordAll(t, openTestDB(t, dir), sampleRun("r1", time.Now()))
	if fi, err := os.Stat(filepath.Join(dir, "metrics.db")); err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v, %v", fi.Mode(), err)
	}
	recordAll(t, openTestDB(t, dir), sampleRun("r2", time.Now()))
	db := readDB(t, dir)
	var v int
	db.QueryRow("PRAGMA user_version").Scan(&v)
	if v != 2 || count(t, db, "ai_runs") != 2 || count(t, db, "ai_steps") != 6 || count(t, db, "ai_step_models") != 6 {
		t.Fatalf("version %d, runs %d, steps %d, models %d", v, count(t, db, "ai_runs"), count(t, db, "ai_steps"), count(t, db, "ai_step_models"))
	}
}

func TestMetricsDBNewerVersionUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.db")
	db, _ := sql.Open("sqlite", path)
	if _, err := db.Exec("CREATE TABLE x (a); PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, _ := os.ReadFile(path)
	if m, err := openMetricsDB(dir); err == nil || m != nil {
		t.Fatalf("opened a v99 file: %v", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("v99 file changed")
	}
	var v int
	readDB(t, dir).QueryRow("PRAGMA user_version").Scan(&v)
	if v != 99 {
		t.Fatalf("user_version = %d", v)
	}
}

func TestMetricsDBRows(t *testing.T) {
	dir := t.TempDir()
	r := sampleRun("r1", time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC))
	recordAll(t, openTestDB(t, dir), r)
	db := readDB(t, dir)

	var outcome, failed, ver, started string
	var queued sql.NullString
	var threshold sql.NullString
	if err := db.QueryRow("SELECT outcome, failed_step, app_version, started_at, queued_at, threshold FROM ai_runs WHERE run_id='r1'").
		Scan(&outcome, &failed, &ver, &started, &queued, &threshold); err != nil {
		t.Fatal(err)
	}
	if outcome != "failed" || failed != "risk assessment" || ver != version || started != "2026-11-20T10:00:00Z" || queued.String != "2026-11-20T09:59:59Z" || threshold.Valid {
		t.Fatalf("run %s %s %s %s %v %v", outcome, failed, ver, started, queued, threshold)
	}

	type stepRow struct {
		sha, kind     sql.NullString
		cost          sql.NullFloat64
		in, items     sql.NullInt64
		status, agent string
	}
	rows, err := db.Query("SELECT prompt_sha, error_kind, cost_usd, input_tokens, items, status, agent FROM ai_steps WHERE run_id='r1' ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	var steps []stepRow
	for rows.Next() {
		var s stepRow
		rows.Scan(&s.sha, &s.kind, &s.cost, &s.in, &s.items, &s.status, &s.agent)
		steps = append(steps, s)
	}
	rows.Close()
	if len(steps) != 3 {
		t.Fatalf("%d steps", len(steps))
	}
	for i, s := range steps {
		if s.sha.String != r.Status.Metrics.Steps[i].PromptSHA {
			t.Fatalf("step %d prompt_sha %v", i, s.sha)
		}
	}
	if c := steps[0]; c.cost.Float64 != 0.42 || c.kind.Valid || c.items.Int64 != 2 {
		t.Fatalf("claude step %+v", c)
	}
	if c := steps[1]; c.cost.Valid || c.in.Int64 != 70 { // codex: tokens, no price
		t.Fatalf("codex step %+v", c)
	}
	if c := steps[2]; c.cost.Valid || c.in.Valid || c.items.Valid || c.kind.String != errTimeout || c.status != statusFailed { // no report
		t.Fatalf("risk step %+v", c)
	}

	var n int
	var sum float64
	db.QueryRow("SELECT COUNT(*), SUM(cost_usd) FROM ai_step_models WHERE run_id='r1' AND seq=0").Scan(&n, &sum)
	if n != 2 || math.Abs(sum-0.42) > 1e-9 {
		t.Fatalf("claude models %d, cost %v", n, sum)
	}
	var model string
	var cost sql.NullFloat64
	db.QueryRow("SELECT model, cost_usd FROM ai_step_models WHERE run_id='r1' AND seq=1").Scan(&model, &cost)
	if model != "codex default" || cost.Valid {
		t.Fatalf("codex model %q %v", model, cost)
	}
	if db.QueryRow("SELECT COUNT(*) FROM ai_step_models WHERE run_id='r1' AND seq=2").Scan(&n); n != 0 {
		t.Fatalf("no-report step has %d model rows", n)
	}

	raw, _ := os.ReadFile(filepath.Join(dir, "metrics.db"))
	wal, _ := os.ReadFile(filepath.Join(dir, "metrics.db-wal"))
	if bytes.Contains(append(raw, wal...), []byte("SECRET")) {
		t.Fatal("agent text in the database")
	}
}

func TestMetricsDBPolicyFailureHasNoSteps(t *testing.T) {
	dir := t.TempDir()
	recordAll(t, openTestDB(t, dir), runRecord{PR: pullRequest{Repo: "o/r", Number: 1, HeadSHA: "h"}, Outcome: statusFailed,
		Status: jobStatus{Status: statusFailed, FailedStep: "policy", Metrics: &reviewMetrics{RunID: "p", StartedAt: time.Now(), FinishedAt: time.Now(), Attempt: 1, Steps: []stepMetrics{}}}})
	db := readDB(t, dir)
	var files sql.NullInt64
	db.QueryRow("SELECT diff_files FROM ai_runs WHERE run_id='p'").Scan(&files)
	if count(t, db, "ai_runs") != 1 || count(t, db, "ai_steps") != 0 || files.Valid {
		t.Fatalf("runs %d steps %d diff_files %v", count(t, db, "ai_runs"), count(t, db, "ai_steps"), files)
	}
}

func TestMetricsDBReadmeQueries(t *testing.T) {
	t.Setenv("TZ", "America/New_York")
	readme, err := os.ReadFile("docs/user-guide.md")
	if err != nil {
		t.Fatal(err)
	}
	queries := regexp.MustCompile(`(?s)sqlite3 -readonly "<data dir>/metrics\.db" "\n(.*?)"\n`).FindAllStringSubmatch(string(readme), -1)
	if len(queries) != 6 {
		t.Fatalf("%d user guide queries", len(queries))
	}
	dir := t.TempDir()
	// 01:00 UTC on 1 November is still October in New York.
	oct, nov := time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC)
	m := openTestDB(t, dir)
	m.record(sampleRun("r1", oct))
	m.record(sampleRun("r2", nov))
	// October: 60s and 120s (median 90), plus an uncertain row left out; November: 30s, 300s, 600s (median 300) and one never opened.
	for _, r := range []reviewRecord{sampleReview("r1", oct.Add(time.Minute)), sampleReview("r1", oct.Add(2*time.Minute)), sampleReview("r1", oct.Add(time.Hour)),
		sampleReview("r2", nov.Add(30*time.Second)), sampleReview("r2", nov.Add(5*time.Minute)), sampleReview("r2", nov.Add(10*time.Minute)), sampleReview("", nov)} {
		r.OpenedAt = r.SubmittedAt
		switch r.RunID {
		case "r1", "r2":
			r.OpenedAt = map[string]time.Time{"r1": oct, "r2": nov}[r.RunID]
		default:
			r.OpenedAt, r.Event = time.Time{}, "APPROVE"
		}
		if r.SubmittedAt.Equal(oct.Add(time.Hour)) {
			r.Uncertain = true
		}
		m.recordReview(r)
	}
	m.close(5 * time.Second)
	db := readDB(t, dir)
	want := []string{
		"2026-10|o/r|1|0.42|1;2026-11|o/r|1|0.42|1",
		"2026-10|claude-haiku|0.12|45;2026-10|claude-opus|0.30|75;2026-10|codex default|0.00|77;2026-11|claude-haiku|0.12|45;2026-11|claude-opus|0.30|75;2026-11|codex default|0.00|77",
		"2026-10|COMMENT|2;2026-11|APPROVE|1;2026-11|COMMENT|3",
		"2026-10|2|90;2026-11|3|300",
		"2026-10|6|2|2|4|67;2026-11|12|4|4|8|67",
		"code review|sha-claude|10|5|0|50;risk assessment|sha-claude|5|0|5|100",
	}
	for i, q := range queries {
		rows, err := db.Query(q[1])
		if err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
		cols, _ := rows.Columns()
		var got []string
		for rows.Next() {
			vals := make([]sql.NullString, len(cols))
			ptrs := make([]any, len(cols))
			for j := range vals {
				ptrs[j] = &vals[j]
			}
			rows.Scan(ptrs...)
			var cells []string
			for _, v := range vals {
				cells = append(cells, v.String)
			}
			got = append(got, strings.Join(cells, "|"))
		}
		rows.Close()
		if g := strings.Join(got, ";"); g != want[i] {
			t.Fatalf("query %d:\n got %s\nwant %s", i, g, want[i])
		}
	}
}

func TestMetricsDBFullChannelDrops(t *testing.T) {
	m := &metricsDB{ch: make(chan any, 1)} // no writer
	m.record(sampleRun("a", time.Now()))
	done := make(chan bool)
	go func() { m.record(sampleRun("b", time.Now())); done <- true }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("record blocked on a full channel")
	}
	m.recordReview(sampleReview("a", time.Now())) // dropped too
	if len(m.ch) != 1 || (<-m.ch).(runRecord).Status.Metrics.RunID != "a" {
		t.Fatal("wrong record kept")
	}
	var disabled *metricsDB
	disabled.record(sampleRun("c", time.Now())) // no panic
	disabled.recordReview(sampleReview("c", time.Now()))
	disabled.close(time.Second)
}

func TestMetricsDBCloseDrains(t *testing.T) {
	dir := t.TempDir()
	m := openTestDB(t, dir)
	for i := range 50 {
		m.record(sampleRun(string(rune('A'+i)), time.Now()))
	}
	m.close(5 * time.Second)
	m.record(sampleRun("late", time.Now())) // after close: dropped, no panic
	m.recordReview(sampleReview("late", time.Now()))
	if n := count(t, readDB(t, dir), "ai_runs"); n != 50 {
		t.Fatalf("%d runs after close, want 50", n)
	}
}

func TestMetricsDBMigratesV1(t *testing.T) {
	dir := t.TempDir()
	all := migrations
	migrations = all[:1]
	recordAll(t, openTestDB(t, dir), sampleRun("r1", time.Now()))
	migrations = all
	var v int
	if readDB(t, dir).QueryRow("PRAGMA user_version").Scan(&v); v != 1 {
		t.Fatalf("setup at version %d", v)
	}
	m := openTestDB(t, dir)
	m.recordReview(sampleReview("r1", time.Now()))
	m.close(5 * time.Second)
	db := readDB(t, dir)
	db.QueryRow("PRAGMA user_version").Scan(&v)
	if v != 2 || count(t, db, "ai_runs") != 1 || count(t, db, "ai_steps") != 3 || count(t, db, "review_submissions") != 1 || count(t, db, "review_step_counts") != 2 {
		t.Fatalf("version %d, runs %d, steps %d, reviews %d", v, count(t, db, "ai_runs"), count(t, db, "ai_steps"), count(t, db, "review_submissions"))
	}
}

func TestMetricsDBReviewRows(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	m := openTestDB(t, dir)
	m.record(sampleRun("r1", at))
	m.recordReview(sampleReview("r1", at))
	unknown := sampleReview("nope", at.Add(time.Hour))
	unknown.OpenedAt, unknown.Risk, unknown.Uncertain = time.Time{}, "", true
	m.recordReview(unknown)
	future := sampleReview("", at)
	future.OpenedAt = at.Add(time.Minute) // a clock step back: at least 0
	m.recordReview(future)
	m.close(5 * time.Second)
	db := readDB(t, dir)

	type row struct {
		submitted, event, ver          string
		runID, opened, risk            sql.NullString
		secs                           sql.NullInt64
		total, checked, sugg, acc, mod int
		own, replies, uncertain        int
	}
	rows, err := db.Query(`SELECT submitted_at, event, app_version, run_id, opened_at, risk, seconds_to_submit, points_total, points_checked,
		ai_suggested, accepted_as_is, modified, own_comments, replies, uncertain FROM review_submissions ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.submitted, &r.event, &r.ver, &r.runID, &r.opened, &r.risk, &r.secs, &r.total, &r.checked,
			&r.sugg, &r.acc, &r.mod, &r.own, &r.replies, &r.uncertain); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	rows.Close()
	if len(got) != 3 {
		t.Fatalf("%d rows", len(got))
	}
	if r := got[0]; r.submitted != "2026-11-20T10:00:00Z" || r.event != "COMMENT" || r.ver != version || r.runID.String != "r1" ||
		r.opened.String != "2026-11-20T09:58:30Z" || r.secs.Int64 != 90 || r.risk.String != "High" || r.total != 5 || r.checked != 3 ||
		r.sugg != 3 || r.acc != 1 || r.mod != 1 || r.own != 2 || r.replies != 1 || r.uncertain != 0 {
		t.Fatalf("row %+v", r)
	}
	if r := got[1]; r.runID.Valid || r.opened.Valid || r.secs.Valid || r.risk.Valid || r.uncertain != 1 {
		t.Fatalf("unknown run row %+v", r)
	}
	if r := got[2]; r.runID.Valid || r.secs.Int64 != 0 {
		t.Fatalf("future opened_at row %+v", r)
	}
	var steps string
	db.QueryRow("SELECT group_concat(step || ':' || suggested || '/' || accepted_as_is || '/' || modified, ';') FROM review_step_counts WHERE submission_id = 1").Scan(&steps)
	if steps != "code review:2/1/0;risk assessment:1/0/1" {
		t.Fatalf("steps %q", steps)
	}
}
