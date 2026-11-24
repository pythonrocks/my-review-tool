package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// stderrOf returns what fn prints on stderr.
func stderrOf(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old }()
	fn()
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

func newStore(t *testing.T) resultStore {
	t.Helper()
	s, err := openStore(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var storePR = pullRequest{Repo: "octo/app", Number: 42, HeadSHA: "abc123", Title: "t"}

func TestStoreRoundTrip(t *testing.T) {
	s := newStore(t)
	for _, st := range []jobStatus{
		{Status: statusDone, Summary: "Adds a.\nBecause b.", HeadSHA: "abc123", Findings: []taggedFinding{
			{Step: "code review", finding: finding{Severity: "high", File: "a.go", Line: 3, Description: "bug", Title: "Bug in a", SuggestedComment: "Fix **a**.", Tags: []string{"bug", "test"}}, PostedURL: "https://github.com/o/r/pull/1#discussion_r1"},
			{Step: "security review", finding: finding{Severity: "low", File: "b.go", Description: "meh", Tags: []string{"security", "performance"}}},
		}, Threshold: "Medium", ThresholdSource: policySourceRepo, SensitiveAreas: []string{"payments", "data model"}, AreasSource: policySourceRepo,
			riskDecision: riskDecision{Recommendation: recHuman, Risk: "Medium", Rationale: []string{"r1", "r2"},
				Points: []pointOfInterest{{File: "a.go", Line: 3, Reason: "High code review finding: bug"}, {File: "c.go", Line: 0, Reason: "extra", Title: "Extra", Comment: "Why c?"}}}},
		{Status: statusDone, HeadSHA: "abc123", Threshold: "High", ThresholdSource: policySourceDefault, SensitiveAreas: []string{},
			AreasSource: policySourceRepo, riskDecision: riskDecision{Recommendation: recAutomated, Risk: "Low", Rationale: []string{"r"}, Points: []pointOfInterest{}}},
		{Status: statusFailed, FailedStep: "security review", Reason: "timed out", HeadSHA: "abc123"},
	} {
		s.save(storePR, st)
		got, ok := s.load(storePR)
		if !ok || !reflect.DeepEqual(got, st) {
			t.Errorf("load = %+v, %v; want %+v", got, ok, st)
		}
	}
	name := filepath.Join(s.dir, "octo%2Fapp%2342.json")
	fi, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("file mode %o", fi.Mode().Perm())
	}
	data, _ := os.ReadFile(name)
	for _, field := range []string{`"repo":"octo/app"`, `"number":42`, `"head_sha":"abc123"`, `"status":"failed"`,
		`"failed_step":"security review"`, `"reason":"timed out"`, `"summary":""`, `"findings":[]`, `"risk":""`, `"recommendation":""`,
		`"rationale":[]`, `"points_of_interest":[]`, `"finished_at":"`} {
		if !strings.Contains(string(data), field) {
			t.Errorf("file lacks %s: %s", field, data)
		}
	}
	if tmps, _ := filepath.Glob(filepath.Join(s.dir, "*.tmp")); len(tmps) != 0 {
		t.Errorf("temp files left: %v", tmps)
	}

	s.save(storePR, jobStatus{Status: statusDone, HeadSHA: "abc123", Threshold: "Low", ThresholdSource: policySourceRepo,
		SensitiveAreas: []string{"authentication"}, AreasSource: policySourceDefault, riskDecision: riskDecision{Recommendation: recHuman, Risk: "High",
			Rationale: []string{"r"}, Points: []pointOfInterest{{File: "a.go", Line: 3, Reason: "why"}}}})
	data, _ = os.ReadFile(name)
	for _, field := range []string{`"threshold":"Low"`, `"threshold_source":"repository policy"`, `"sensitive_areas":["authentication"]`,
		`"sensitive_areas_source":"default"`, `"risk":"High"`, `"recommendation":"human review required"`, `"rationale":["r"]`,
		`"points_of_interest":[{"file":"a.go","line":3,"reason":"why","title":"","suggested_comment":""}]`} {
		if !strings.Contains(string(data), field) {
			t.Errorf("file lacks %s: %s", field, data)
		}
	}
}

func TestStoreLoadWithoutTags(t *testing.T) {
	s := newStore(t)
	// saved before tags existed
	os.WriteFile(s.file(prKey(storePR)), []byte(`{"repo":"octo/app","number":42,"head_sha":"abc123","status":"done","recommendation":"automated review is sufficient",`+
		`"threshold":"High","findings":[{"step":"code review","severity":"Low","file":"a.go","line":1,"description":"d"},`+
		`{"step":"security review","severity":"High","file":"b.go","line":2,"description":"e","tags":["bug","style"]}]}`), 0600)
	st, ok := s.load(storePR)
	if !ok || len(st.Findings) != 2 || st.Findings[0].Tags != nil || !reflect.DeepEqual(st.Findings[1].Tags, []string{"security", "bug"}) {
		t.Fatalf("load = %+v, %v", st.Findings, ok)
	}
}

func TestStoreLoadRejects(t *testing.T) {
	done := jobStatus{Status: statusDone}
	for _, c := range []struct {
		name    string
		pr      pullRequest
		content string // written instead of saving when set
		logged  bool
	}{
		{name: "missing"},
		{name: "empty head", pr: pullRequest{Repo: "octo/app", Number: 42}, content: `{"repo":"octo/app","number":42,"head_sha":"","status":"done"}`},
		{name: "corrupt", content: `{"repo":`, logged: true},
		{name: "wrong repo", content: `{"repo":"octo/other","number":42,"head_sha":"abc123","status":"done"}`, logged: true},
		{name: "wrong number", content: `{"repo":"octo/app","number":7,"head_sha":"abc123","status":"done"}`, logged: true},
		{name: "unknown status", content: `{"repo":"octo/app","number":42,"head_sha":"abc123","status":"running"}`, logged: true},
		// saved before risk assessment existed
		{name: "done without recommendation", content: `{"repo":"octo/app","number":42,"head_sha":"abc123","status":"done","failed_step":"","reason":"","findings":[],"finished_at":"2026-11-01T00:00:00Z"}`},
		// saved before repository policies existed
		{name: "done without threshold", content: `{"repo":"octo/app","number":42,"head_sha":"abc123","status":"done","recommendation":"human review required","risk":"Medium"}`},
		{name: "done with unknown recommendation", content: `{"repo":"octo/app","number":42,"head_sha":"abc123","status":"done","recommendation":"Human review required"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newStore(t)
			pr := storePR
			if c.pr.Repo != "" {
				pr = c.pr
			}
			switch {
			case c.content != "":
				os.WriteFile(s.file(prKey(pr)), []byte(c.content), 0600)
			case c.name != "missing":
				s.save(storePR, done)
			}
			var ok bool
			out := stderrOf(t, func() { _, ok = s.load(pr) })
			if ok {
				t.Error("load succeeded")
			}
			if logged := strings.Contains(out, "ignoring saved result "+s.file(prKey(pr))+": "); logged != c.logged || (!c.logged && out != "") {
				t.Errorf("stderr %q, want logged=%v", out, c.logged)
			}
		})
	}
}

func TestOpenStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if _, err := openStore(dir); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0700 {
		t.Errorf("dir mode %o", fi.Mode().Perm())
	}

	os.WriteFile(filepath.Join(dir, "123.tmp"), nil, 0600)
	os.WriteFile(filepath.Join(dir, "keep.json"), nil, 0600)
	if _, err := openStore(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "123.tmp")); !os.IsNotExist(err) {
		t.Error("leftover temp file not removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "keep.json")); err != nil {
		t.Error("result file removed")
	}

	os.Chmod(dir, 0750)
	_, err := openStore(dir)
	want := "data dir " + dir + ": must not be accessible by group or others (mode 0750); run chmod 700 " + dir
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %s", err, want)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0750 {
		t.Error("openStore changed the directory mode")
	}
}

func TestStorePruneAndRemove(t *testing.T) {
	s := newStore(t)
	prs := []pullRequest{
		{Repo: "a/b", Number: 1}, {Repo: "a/b", Number: 2},
		{Repo: "a/bc", Number: 1}, {Repo: "x/y", Number: 1},
	}
	for _, pr := range prs {
		s.save(pr, jobStatus{Status: statusDone})
	}
	exists := func(pr pullRequest) bool {
		_, err := os.Stat(s.file(prKey(pr)))
		return err == nil
	}

	s.prune("a/b", map[string]bool{"a/b#2": true})
	for i, want := range []bool{false, true, true, true} {
		if exists(prs[i]) != want {
			t.Errorf("after prune %s exists = %v, want %v", prKey(prs[i]), !want, want)
		}
	}

	s.remove("x/y#1")
	if exists(prs[3]) {
		t.Error("remove left the file")
	}
	if out := stderrOf(t, func() { s.remove("x/y#1") }); out != "" {
		t.Errorf("remove of missing file printed %q", out)
	}
}

func TestStoreLoadOlderHead(t *testing.T) {
	s := newStore(t)
	st := jobStatus{Status: statusFailed, FailedStep: "prepare", Reason: "boom", HeadSHA: "abc123"}
	s.save(pullRequest{Repo: "octo/app", Number: 42, HeadSHA: "newer"}, st) // the reviewed SHA is saved, not the PR's head
	got, ok := s.load(pullRequest{Repo: "octo/app", Number: 42, HeadSHA: "other"})
	if !ok || !reflect.DeepEqual(got, st) {
		t.Fatalf("load = %+v, %v; want %+v", got, ok, st)
	}
}

// A result saved before titles and suggested comments existed loads with them empty.
func TestStoreLoadWithoutTitles(t *testing.T) {
	s := newStore(t)
	os.WriteFile(s.file(prKey(storePR)), []byte(`{"repo":"octo/app","number":42,"head_sha":"abc123","status":"done","summary":"",`+
		`"findings":[{"step":"code review","severity":"High","file":"a.go","line":3,"description":"bug","posted_url":""}],`+
		`"risk":"Medium","recommendation":"human review required","rationale":["r"],"points_of_interest":[{"file":"c.go","line":0,"reason":"extra"}],`+
		`"finished_at":"2026-11-01T00:00:00Z","threshold":"High","threshold_source":"default","sensitive_areas":[],"sensitive_areas_source":"default"}`), 0600)
	st, ok := s.load(storePR)
	want := []taggedFinding{{Step: "code review", finding: finding{Severity: "High", File: "a.go", Line: 3, Description: "bug"}}}
	if !ok || !reflect.DeepEqual(st.Findings, want) || !reflect.DeepEqual(st.Points, []pointOfInterest{{File: "c.go", Reason: "extra"}}) {
		t.Errorf("load = %+v, %v", st, ok)
	}
}
