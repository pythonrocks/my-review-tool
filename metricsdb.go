package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// migrations take metrics.db from version i (PRAGMA user_version) to i+1. Only append; a shipped entry never changes.
var migrations = []string{
	`CREATE TABLE ai_runs (
  run_id TEXT PRIMARY KEY, repo TEXT NOT NULL, number INTEGER NOT NULL, head_sha TEXT NOT NULL,
  attempt INTEGER NOT NULL, queued_at TEXT, started_at TEXT NOT NULL, finished_at TEXT NOT NULL,
  outcome TEXT NOT NULL CHECK (outcome IN ('done','failed','cancelled')), failed_step TEXT,
  threshold TEXT, threshold_source TEXT, diff_files INTEGER, diff_bytes INTEGER,
  app_version TEXT NOT NULL
) STRICT;
CREATE INDEX ai_runs_repo_started ON ai_runs(repo, started_at);
CREATE TABLE ai_steps (
  run_id TEXT NOT NULL REFERENCES ai_runs(run_id), seq INTEGER NOT NULL,
  step TEXT NOT NULL, agent TEXT NOT NULL, prompt_sha TEXT,
  started_at TEXT NOT NULL, finished_at TEXT NOT NULL, wall_ms INTEGER NOT NULL,
  api_ms INTEGER, turns INTEGER, input_tokens INTEGER, output_tokens INTEGER,
  cache_read_tokens INTEGER, cache_write_tokens INTEGER,
  cost_usd REAL,
  status TEXT NOT NULL CHECK (status IN ('done','failed','cancelled')),
  error_kind TEXT CHECK (error_kind IN ('timeout','cancelled','not_found','exit','agent_error','bad_output','other')),
  items INTEGER,
  PRIMARY KEY (run_id, seq)
) STRICT;
CREATE TABLE ai_step_models (
  run_id TEXT NOT NULL, seq INTEGER NOT NULL, model TEXT NOT NULL,
  input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER, cache_write_tokens INTEGER,
  cost_usd REAL,
  PRIMARY KEY (run_id, seq, model),
  FOREIGN KEY (run_id, seq) REFERENCES ai_steps(run_id, seq)
) STRICT;`,
	`CREATE TABLE review_submissions (
  id INTEGER PRIMARY KEY, submitted_at TEXT NOT NULL,
  repo TEXT NOT NULL, number INTEGER NOT NULL, head_sha TEXT NOT NULL,
  run_id TEXT REFERENCES ai_runs(run_id),
  event TEXT NOT NULL CHECK (event IN ('APPROVE','REQUEST_CHANGES','COMMENT')),
  opened_at TEXT, seconds_to_submit INTEGER,
  points_total INTEGER NOT NULL, points_checked INTEGER NOT NULL, ai_suggested INTEGER NOT NULL,
  accepted_as_is INTEGER NOT NULL, modified INTEGER NOT NULL, own_comments INTEGER NOT NULL, replies INTEGER NOT NULL,
  risk TEXT, recommendation TEXT,
  uncertain INTEGER NOT NULL CHECK (uncertain IN (0,1)),
  app_version TEXT NOT NULL
) STRICT;
CREATE INDEX review_submissions_submitted ON review_submissions(submitted_at);
CREATE TABLE review_step_counts (
  submission_id INTEGER NOT NULL REFERENCES review_submissions(id),
  step TEXT NOT NULL,
  suggested INTEGER NOT NULL, accepted_as_is INTEGER NOT NULL, modified INTEGER NOT NULL,
  PRIMARY KEY (submission_id, step)
) STRICT;`,
}

// runRecord is one finished or dropped workflow run. Outcome is done, failed or cancelled (dropped).
type runRecord struct {
	PR      pullRequest
	Status  jobStatus
	Outcome string
}

// reviewRecord is one submitted review: counts only, no text. Its suggested/accepted/modified totals are the
// sums over Steps.
type reviewRecord struct {
	SubmittedAt, OpenedAt        time.Time // OpenedAt zero: unknown
	Repo                         string
	Number                       int
	HeadSHA, RunID, Event        string // RunID "": none
	Total, Checked, Own, Replies int
	Risk, Recommendation         string
	Uncertain                    bool
	Steps                        []stepCount // in order of first row
}

// stepCount is the "What to check" rows of one step (ai_steps.step) in a submitted review.
type stepCount struct {
	Step                          string
	Suggested, Accepted, Modified int
}

// metricsDB is the permanent run history in <data dir>/metrics.db. A nil *metricsDB is disabled.
type metricsDB struct {
	db   *sql.DB
	ch   chan any      // runRecord or reviewRecord
	done chan struct{} // closed when the writer has drained ch

	mu     sync.Mutex // guards closed and sends on ch
	closed bool
}

// openMetricsDB opens or creates <dataDir>/metrics.db (0600), migrates it and starts the writer.
func openMetricsDB(dataDir string) (*metricsDB, error) {
	path := filepath.Join(dataDir, "metrics.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err := os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one connection, so the per-connection pragmas below hold for every query
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("metrics.db: %w", err)
	}
	m := &metricsDB{db: db, ch: make(chan any, 64), done: make(chan struct{})}
	go m.write()
	return m, nil
}

// migrate sets the connection pragmas and applies the migrations the file lacks. A file newer than this
// binary is left untouched: the version is read before journal_mode, the only pragma that writes.
func migrate(db *sql.DB) error {
	if _, err := db.Exec("PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON"); err != nil {
		return err
	}
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("schema version %d is newer than this myReview knows (%d)", v, len(migrations))
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// record queues r for the writer without blocking; it is dropped, with one log line, when the database
// is disabled or closed or the queue is full.
func (m *metricsDB) record(r runRecord) { m.send(r, "run "+r.Status.Metrics.RunID) }

// recordReview queues r like record does.
func (m *metricsDB) recordReview(r reviewRecord) { m.send(r, reviewName(r)) }

func reviewName(r reviewRecord) string { return fmt.Sprintf("review %s#%d", r.Repo, r.Number) }

func (m *metricsDB) send(v any, name string) {
	if m == nil {
		fmt.Fprintf(os.Stderr, "metrics.db disabled: %s not recorded\n", name)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		fmt.Fprintf(os.Stderr, "metrics.db closed: %s not recorded\n", name)
		return
	}
	select {
	case m.ch <- v:
	default:
		fmt.Fprintf(os.Stderr, "metrics.db queue full: %s not recorded\n", name)
	}
}

func (m *metricsDB) write() {
	defer close(m.done)
	for v := range m.ch {
		switch r := v.(type) {
		case runRecord:
			if err := m.insert(r); err != nil {
				fmt.Fprintf(os.Stderr, "metrics.db: run %s not recorded: %v\n", r.Status.Metrics.RunID, err)
			}
		case reviewRecord:
			if err := m.insertReview(r); err != nil {
				fmt.Fprintf(os.Stderr, "metrics.db: %s not recorded: %v\n", reviewName(r), err)
			}
		}
	}
}

// close stops accepting records, waits up to wait for the writer to drain, then closes the database.
func (m *metricsDB) close(wait time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.closed = true
	close(m.ch)
	m.mu.Unlock()
	select {
	case <-m.done:
	case <-time.After(wait):
		fmt.Fprintln(os.Stderr, "metrics.db: writer did not drain in time")
	}
	m.db.Close()
}

// insert writes r's run, steps and per-model rows in one transaction. It stores no agent text:
// no error reason, findings, summary or prompt.
func (m *metricsDB) insert(r runRecord) error {
	st, mt := r.Status, r.Status.Metrics
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var diffFiles, diffBytes any
	if st.FailedStep != "policy" && st.FailedStep != "prepare" { // no diff before prepare finished
		diffFiles, diffBytes = mt.DiffFiles, mt.DiffBytes
	}
	if _, err := tx.Exec(`INSERT INTO ai_runs VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		mt.RunID, r.PR.Repo, r.PR.Number, r.PR.HeadSHA, mt.Attempt, nullTime(mt.QueuedAt), dbTime(mt.StartedAt),
		dbTime(mt.FinishedAt), r.Outcome, nullStr(st.FailedStep), nullStr(st.Threshold), nullStr(st.ThresholdSource),
		diffFiles, diffBytes, version); err != nil {
		return err
	}
	for seq, s := range mt.Steps {
		var items any
		if s.Status == statusDone {
			items = s.Items
		}
		// A step without a report (usage nil) has every usage column NULL.
		var apiMs, turns, in, out, cr, cw any
		var cost *float64
		if u := s.Usage; u != nil {
			apiMs, turns, in, out, cr, cw = u.APIMs, u.Turns, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens
			cost = u.CostUSD
		}
		if _, err := tx.Exec(`INSERT INTO ai_steps VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			mt.RunID, seq, s.Step, s.Agent, nullStr(s.PromptSHA), dbTime(s.StartedAt), dbTime(s.FinishedAt), s.WallMs,
			apiMs, turns, in, out, cr, cw, cost, s.Status, nullStr(s.ErrorKind), items); err != nil {
			return err
		}
		if s.Usage == nil {
			continue
		}
		for _, mu := range s.Usage.Models {
			if _, err := tx.Exec(`INSERT INTO ai_step_models VALUES (?,?,?,?,?,?,?,?)`, mt.RunID, seq, mu.Model,
				mu.InputTokens, mu.OutputTokens, mu.CacheReadTokens, mu.CacheWriteTokens, mu.CostUSD); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// insertReview writes r and its step rows in one transaction. A run_id missing from ai_runs is stored as NULL.
func (m *metricsDB) insertReview(r reviewRecord) error {
	var sugg, acc, mod int
	for _, c := range r.Steps {
		sugg, acc, mod = sugg+c.Suggested, acc+c.Accepted, mod+c.Modified
	}
	var secs any
	if !r.OpenedAt.IsZero() {
		secs = max(0, int64(r.SubmittedAt.Sub(r.OpenedAt)/time.Second))
	}
	uncertain := 0
	if r.Uncertain {
		uncertain = 1
	}
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO review_submissions VALUES (NULL,?,?,?,?,(SELECT run_id FROM ai_runs WHERE run_id = ?),?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		dbTime(r.SubmittedAt), r.Repo, r.Number, r.HeadSHA, r.RunID, r.Event, nullTime(r.OpenedAt), secs,
		r.Total, r.Checked, sugg, acc, mod, r.Own, r.Replies, nullStr(r.Risk), nullStr(r.Recommendation), uncertain, version)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	for _, c := range r.Steps {
		if _, err := tx.Exec(`INSERT INTO review_step_counts VALUES (?,?,?,?,?)`, id, c.Step, c.Suggested, c.Accepted, c.Modified); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func dbTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return dbTime(t)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
