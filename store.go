package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// resultStore keeps one JSON file per PR with its finished review result.
type resultStore struct{ dir string }

type savedFinding struct {
	Step             string `json:"step"`
	Severity         string `json:"severity"`
	File             string `json:"file"`
	Line             int    `json:"line"`
	Description      string `json:"description"`
	Title            string `json:"title"`
	SuggestedComment string `json:"suggested_comment"`
	PostedURL        string `json:"posted_url"`
}

type savedResult struct {
	Repo           string            `json:"repo"`
	Number         int               `json:"number"`
	HeadSHA        string            `json:"head_sha"`
	Status         string            `json:"status"`
	FailedStep     string            `json:"failed_step"`
	Reason         string            `json:"reason"`
	Summary        string            `json:"summary"`
	Findings       []savedFinding    `json:"findings"`
	Risk           string            `json:"risk"`
	Recommendation string            `json:"recommendation"`
	Rationale      []string          `json:"rationale"`
	Points         []pointOfInterest `json:"points_of_interest"`
	FinishedAt     string            `json:"finished_at"`
	// The policy a done run applied.
	Threshold            string   `json:"threshold"`
	ThresholdSource      string   `json:"threshold_source"`
	SensitiveAreas       []string `json:"sensitive_areas"`
	SensitiveAreasSource string   `json:"sensitive_areas_source"`
	// The latest run's metrics; absent in files saved before metrics existed.
	Metrics *reviewMetrics `json:"metrics,omitempty"`
}

// openStore creates dir and its pending subdirectory if needed, refuses a group/other-accessible one and removes leftover temp files.
func openStore(dir string) (resultStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return resultStore{}, fmt.Errorf("data dir %s: %w", dir, err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return resultStore{}, fmt.Errorf("data dir %s: %w", dir, err)
	}
	if perm := fi.Mode().Perm(); perm&0077 != 0 {
		return resultStore{}, fmt.Errorf("data dir %s: must not be accessible by group or others (mode %04o); run chmod 700 %s", dir, perm, dir)
	}
	if err := os.MkdirAll(filepath.Join(dir, "pending"), 0700); err != nil {
		return resultStore{}, fmt.Errorf("data dir %s: %w", dir, err)
	}
	tmps, _ := filepath.Glob(filepath.Join(dir, "*.tmp")) // only fails on a bad pattern
	for _, f := range tmps {
		if err := os.Remove(f); err != nil {
			fmt.Fprintln(os.Stderr, "removing", f+":", err)
		}
	}
	return resultStore{dir: dir}, nil
}

func (s resultStore) file(key string) string {
	return filepath.Join(s.dir, url.PathEscape(key)+".json")
}

// save writes the result atomically; errors are logged, never returned.
func (s resultStore) save(pr pullRequest, st jobStatus) {
	r := savedResult{Repo: pr.Repo, Number: pr.Number, HeadSHA: st.HeadSHA, Status: st.Status,
		FailedStep: st.FailedStep, Reason: st.Reason, Summary: st.Summary, Findings: []savedFinding{},
		Risk: st.Risk, Recommendation: st.Recommendation, Rationale: append([]string{}, st.Rationale...),
		Points: append([]pointOfInterest{}, st.Points...), FinishedAt: time.Now().UTC().Format(time.RFC3339),
		Threshold: st.Threshold, ThresholdSource: st.ThresholdSource, SensitiveAreas: append([]string{}, st.SensitiveAreas...),
		SensitiveAreasSource: st.AreasSource, Metrics: st.Metrics}
	for _, f := range st.Findings {
		r.Findings = append(r.Findings, savedFinding{Step: f.Step, Severity: f.Severity, File: f.File, Line: f.Line, Description: f.Description,
			Title: f.Title, SuggestedComment: f.SuggestedComment, PostedURL: f.PostedURL})
	}
	name := s.file(prKey(pr))
	if err := s.write(name, r); err != nil {
		fmt.Fprintln(os.Stderr, "saving result", name+":", err)
	}
}

// write saves v as JSON at name atomically, through a temp file in the data dir.
func (s resultStore) write(name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, "*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), name)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// load returns the saved result if, when done, it carries a recommendation and the threshold it applied.
// It may be for an older commit than the PR's current head; the queue then treats it as outdated.
func (s resultStore) load(pr pullRequest) (jobStatus, bool) {
	name := s.file(prKey(pr))
	data, err := os.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return jobStatus{}, false
	}
	ignore := func(reason any) (jobStatus, bool) {
		fmt.Fprintln(os.Stderr, "ignoring saved result", name+":", reason)
		return jobStatus{}, false
	}
	if err != nil {
		return ignore(err)
	}
	var r savedResult
	if err := json.Unmarshal(data, &r); err != nil {
		return ignore(err)
	}
	if r.Repo != pr.Repo || r.Number != pr.Number {
		return ignore(fmt.Sprintf("it is for %s#%d", r.Repo, r.Number))
	}
	if r.Status != statusDone && r.Status != statusFailed {
		return ignore(fmt.Sprintf("unknown status %q", r.Status))
	}
	if r.Status == statusDone && r.Recommendation != recHuman && r.Recommendation != recAutomated {
		return jobStatus{}, false // saved before risk assessment existed: review again
	}
	if r.Status == statusDone && r.Threshold == "" {
		return jobStatus{}, false // saved before repository policies existed: review again
	}
	st := jobStatus{Status: r.Status, FailedStep: r.FailedStep, Reason: r.Reason, Summary: r.Summary, HeadSHA: r.HeadSHA, Metrics: r.Metrics}
	if r.Status == statusDone {
		st.riskDecision = riskDecision{Recommendation: r.Recommendation, Risk: r.Risk, Rationale: r.Rationale, Points: r.Points}
		st.Threshold, st.ThresholdSource, st.SensitiveAreas, st.AreasSource = r.Threshold, r.ThresholdSource, r.SensitiveAreas, r.SensitiveAreasSource
	}
	for _, f := range r.Findings {
		st.Findings = append(st.Findings, taggedFinding{f.Step, finding{Severity: f.Severity, File: f.File, Line: f.Line, Description: f.Description,
			Title: f.Title, SuggestedComment: f.SuggestedComment}, f.PostedURL})
	}
	return st, true
}

// remove deletes the file of the PR with this key, if any.
func (s resultStore) remove(key string) {
	if err := os.Remove(s.file(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "removing saved result:", err)
	}
}

// prune deletes the files of repo's PRs whose key is not in open.
func (s resultStore) prune(repo string, open map[string]bool) {
	prefix := url.PathEscape(repo + "#")
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pruning saved results:", err)
		return
	}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !strings.HasPrefix(name, prefix) {
			continue
		}
		if key, err := url.PathUnescape(name); err == nil && !open[key] {
			s.remove(key)
		}
	}
}
