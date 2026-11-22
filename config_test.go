package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "myreview.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigValid(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `{"repositories": ["a/b", "c/d"], "concurrency": 3, "extra": 1}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repositories) != 2 || cfg.Repositories[0] != "a/b" || cfg.Repositories[1] != "c/d" || cfg.Concurrency != 3 {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	for _, body := range []string{`{}`, `{"repositories": []}`, `{"repositories": null}`} {
		cfg, err := loadConfig(writeConfig(t, body))
		if err != nil {
			t.Errorf("%s: %v", body, err)
		}
		if len(cfg.Repositories) != 0 || cfg.Concurrency != 2 {
			t.Errorf("%s: cfg = %+v, want empty list and concurrency 2", body, cfg)
		}
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.json")
	_, err := loadConfig(path)
	if err == nil || !strings.HasPrefix(err.Error(), "config "+path+": ") {
		t.Errorf("err = %v, want config %s: <os error>", err, path)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	tests := []struct{ body, want string }{
		{`{"repositories": "a/b"}`, "cannot unmarshal"},
		{`{"concurrency": "2"}`, "cannot unmarshal"},
		{`{"concurrency": 2.5}`, "cannot unmarshal"},
		{`not json`, "invalid character"},
		{`{"repositories": ["a/b/c"]}`, `repository "a/b/c" is not owner/name`},
		{`{"repositories": ["foo"]}`, `repository "foo" is not owner/name`},
		{`{"repositories": ["/b"]}`, `repository "/b" is not owner/name`},
		{`{"repositories": ["a/"]}`, `repository "a/" is not owner/name`},
		{`{"repositories": ["a /b"]}`, `repository "a /b" is not owner/name`},
		{"{\"repositories\": [\"a\u00a0x/b\"]}", `repository "a\u00a0x/b" is not owner/name`}, // %q escapes U+00A0
		{`{"repositories": ["a/b", "c/d", "a/b"]}`, `duplicate repository "a/b"`},
		{`{"concurrency": 0}`, "concurrency must be positive, got 0"},
		{`{"concurrency": -1}`, "concurrency must be positive, got -1"},
		{`{"quiet_period": "-1m"}`, `quiet_period must be a non-negative duration such as "5m", got "-1m"`},
		{`{"quiet_period": "soon"}`, `quiet_period must be a non-negative duration such as "5m", got "soon"`},
		{`{"quiet_period": 300}`, `quiet_period must be a duration string`},
		{`{"auto_review_max_age": "-1h"}`, `auto_review_max_age must be a non-negative duration such as "168h", got "-1h"`},
		{`{"auto_review_max_age": "abc"}`, `auto_review_max_age must be a non-negative duration such as "168h", got "abc"`},
		{`{"auto_review_max_age": 7}`, "cannot unmarshal"},
	}
	for _, tt := range tests {
		path := writeConfig(t, tt.body)
		_, err := loadConfig(path)
		if err == nil || !strings.HasPrefix(err.Error(), "config "+path+": ") || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want prefix %q and %q", tt.body, err, "config "+path+": ", tt.want)
		}
	}
}

func TestLoadConfigStepsDefault(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `{"repositories": ["a/b"]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := config{}.Steps
	want.CodeReview = stepConfig{Agent: "claude", Prompt: codeReviewPrompt}
	want.SecurityReview = stepConfig{Agent: "claude", Prompt: securityReviewPrompt}
	want.RiskAssessment = stepConfig{Agent: "claude", Prompt: riskPrompt}
	if cfg.Steps != want {
		t.Errorf("steps = %+v, want claude with the built-in prompts", cfg.Steps)
	}
}

func TestLoadConfigStepsOverride(t *testing.T) {
	path := writeConfig(t, `{"steps": {"security_review": {"agent": "codex", "prompt_file": "prompts/sec.md"}, "risk_assessment": {"agent": "codex"}, "lint": {"agent": "x"}}}`)
	if err := os.MkdirAll(filepath.Join(filepath.Dir(path), "prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "prompts", "sec.md"), []byte("MY PROMPT"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := (stepConfig{Agent: "codex", PromptFile: "prompts/sec.md", Prompt: "MY PROMPT"}); cfg.Steps.SecurityReview != want {
		t.Errorf("security_review = %+v, want %+v", cfg.Steps.SecurityReview, want)
	}
	if want := (stepConfig{Agent: "claude", Prompt: codeReviewPrompt}); cfg.Steps.CodeReview != want {
		t.Errorf("code_review = %+v, want the default", cfg.Steps.CodeReview)
	}
	if want := (stepConfig{Agent: "codex", Prompt: riskPrompt}); cfg.Steps.RiskAssessment != want {
		t.Errorf("risk_assessment = %+v, want %+v", cfg.Steps.RiskAssessment, want)
	}
}

func TestLoadConfigStepsErrors(t *testing.T) {
	path := writeConfig(t, `{}`)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "empty.md"), []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct{ body, want string }{
		{`{"steps": {"code_review": {"agent": "gpt"}}}`, `steps.code_review.agent must be "claude" or "codex", got "gpt"`},
		{`{"steps": {"security_review": {"agent": ""}}}`, `steps.security_review.agent must be "claude" or "codex", got ""`},
		{`{"steps": {"code_review": {"prompt_file": "nope.md"}}}`, "steps.code_review.prompt_file: open "},
		{`{"steps": {"security_review": {"prompt_file": "empty.md"}}}`, "steps.security_review.prompt_file: empty.md is empty"},
		{`{"steps": {"risk_assessment": {"agent": "gpt"}}}`, `steps.risk_assessment.agent must be "claude" or "codex", got "gpt"`},
		{`{"steps": {"risk_assessment": {"prompt_file": "nope.md"}}}`, "steps.risk_assessment.prompt_file: open "},
	}
	for _, tt := range tests {
		if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := loadConfig(path)
		if err == nil || !strings.HasPrefix(err.Error(), "config "+path+": "+tt.want) {
			t.Errorf("%s: err = %v, want prefix %q", tt.body, err, "config "+path+": "+tt.want)
		}
	}
}

func TestLoadConfigQuietPeriod(t *testing.T) {
	for body, want := range map[string]time.Duration{`{}`: 5 * time.Minute, `{"quiet_period": "90s"}`: 90 * time.Second, `{"quiet_period": "0s"}`: 0} {
		cfg, err := loadConfig(writeConfig(t, body))
		if err != nil || cfg.QuietPeriod.Duration != want {
			t.Errorf("%s: quiet period %v, %v; want %v", body, cfg.QuietPeriod, err, want)
		}
	}
}

func TestLoadConfigAutoReviewMaxAge(t *testing.T) {
	for body, want := range map[string]time.Duration{`{}`: 168 * time.Hour, `{"auto_review_max_age": "24h"}`: 24 * time.Hour, `{"auto_review_max_age": "0s"}`: 0} {
		cfg, err := loadConfig(writeConfig(t, body))
		if err != nil || cfg.autoReviewMaxAge != want {
			t.Errorf("%s: max age %v, %v; want %v", body, cfg.autoReviewMaxAge, err, want)
		}
	}
}

func TestLoadConfigSkipValid(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `{"skip": [{"label": "^dependencies$"}, {"title": "^Bump ", "extra": 1}, {"author": "^app/dependabot$"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Skip) != 3 || cfg.Skip[0].Label != "^dependencies$" || cfg.Skip[1].Title != "^Bump " || cfg.Skip[2].Author != "^app/dependabot$" {
		t.Fatalf("skip = %+v", cfg.Skip)
	}
	for i, r := range cfg.Skip {
		if r.re == nil {
			t.Errorf("skip[%d] not compiled", i)
		}
	}
	for _, body := range []string{`{}`, `{"skip": []}`} {
		if cfg, err := loadConfig(writeConfig(t, body)); err != nil || len(cfg.Skip) != 0 {
			t.Errorf("%s: skip = %v, err = %v", body, cfg.Skip, err)
		}
	}
}

func TestLoadConfigSkipInvalid(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{`{"skip": [{"label": "a"}, {"label": "b"}, {"title": "("}]}`, `: skip[2].title: invalid regular expression "(": `},
		{`{"skip": [{"author": "a"}, {}]}`, `: skip[1]: must set exactly one of "label", "title" or "author"`},
		{`{"skip": [{"label": "a", "title": "b"}]}`, `: skip[0]: must set exactly one of`},
		{`{"skip": [{"title": ""}]}`, `: skip[0]: must set exactly one of`},
	} {
		_, err := loadConfig(writeConfig(t, c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.body, err, c.want)
		}
	}
}

func TestMatchSkip(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `{"skip": [
		{"label": "^dependencies$"}, {"title": "deps"}, {"title": "^Bump "}, {"author": "^app/dependabot$"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		pr   pullRequest
		want string
	}{
		{"label", pullRequest{Title: "x", Labels: []string{"go", "dependencies"}}, `label "dependencies" matches rule skip[0] ^dependencies$`},
		{"title", pullRequest{Title: "Bump foo"}, "title matches rule skip[2] ^Bump "},
		{"author", pullRequest{Title: "x", Author: "app/dependabot"}, `author "app/dependabot" matches rule skip[3] ^app/dependabot$`},
		{"no match", pullRequest{Title: "bump foo", Author: "dependabot[bot]", Labels: []string{"dependencies-x"}}, ""},
		{"first rule wins", pullRequest{Title: "Bump deps", Author: "app/dependabot", Labels: []string{"dependencies"}}, `label "dependencies" matches rule skip[0] ^dependencies$`},
		{"unanchored", pullRequest{Title: "chore(deps): x"}, "title matches rule skip[1] deps"},
		{"no labels", pullRequest{Title: "Bump foo", Author: "alice"}, "title matches rule skip[2] ^Bump "},
	} {
		if got, _ := matchSkip(cfg.Skip, c.pr); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	if got, i := matchSkip(nil, pullRequest{Title: "Bump"}); got != "" || i != -1 {
		t.Errorf("no rules: got %q", got)
	}
}
