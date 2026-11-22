package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

type config struct {
	Repositories []string `json:"repositories"`
	Concurrency  int      `json:"concurrency"`
	QuietPeriod  duration `json:"quiet_period"`
	// AutoReviewMaxAge is the raw auto_review_max_age; loadConfig parses it into autoReviewMaxAge.
	AutoReviewMaxAge string `json:"auto_review_max_age"`
	autoReviewMaxAge time.Duration
	Steps            struct {
		CodeReview     stepConfig `json:"code_review"`
		SecurityReview stepConfig `json:"security_review"`
		RiskAssessment stepConfig `json:"risk_assessment"`
	} `json:"steps"`
	Skip []skipRule `json:"skip"`
}

// skipRule skips pull requests whose label, title or author matches a regular expression; exactly one field is set.
type skipRule struct {
	Label  string `json:"label"`
	Title  string `json:"title"`
	Author string `json:"author"`
	re     *regexp.Regexp
}

// matchSkip returns why the first matching rule skips pr and that rule's index, or "" and -1 when no rule matches.
func matchSkip(rules []skipRule, pr pullRequest) (string, int) {
	for i, r := range rules {
		switch {
		case r.Label != "":
			for _, l := range pr.Labels {
				if r.re.MatchString(l) {
					return fmt.Sprintf("label %q matches rule skip[%d] %s", l, i, r.Label), i
				}
			}
		case r.Title != "":
			if r.re.MatchString(pr.Title) {
				return fmt.Sprintf("title matches rule skip[%d] %s", i, r.Title), i
			}
		case r.re.MatchString(pr.Author):
			return fmt.Sprintf("author %q matches rule skip[%d] %s", pr.Author, i, r.Author), i
		}
	}
	return "", -1
}

// stepConfig selects the agent and prompt of one agent step (BR-15).
type stepConfig struct {
	Agent      string `json:"agent"`
	PromptFile string `json:"prompt_file"`
	Prompt     string `json:"-"` // the prompt text: the embedded default or the content of PromptFile
}

// duration is a time.Duration read from a Go duration string such as "5m".
type duration struct{ time.Duration }

func (d *duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("quiet_period must be a duration string such as \"5m\"")
	}
	v, err := time.ParseDuration(s)
	if err != nil || v < 0 {
		return fmt.Errorf("quiet_period must be a non-negative duration such as \"5m\", got %q", s)
	}
	d.Duration = v
	return nil
}

// loadConfig reads and validates the JSON config file at path.
func loadConfig(path string) (config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return defaultConfig(), fmt.Errorf("config %s: %w", path, err)
	}
	return parseConfig(path, data, func(rel string) ([]byte, error) { return os.ReadFile(filepath.Join(filepath.Dir(path), rel)) })
}

// defaultConfig is the config before the file is applied.
func defaultConfig() config {
	cfg := config{Concurrency: 2, QuietPeriod: duration{5 * time.Minute}, AutoReviewMaxAge: "168h"}
	cfg.Steps.CodeReview = stepConfig{Agent: "claude", Prompt: codeReviewPrompt}
	cfg.Steps.SecurityReview = stepConfig{Agent: "claude", Prompt: securityReviewPrompt}
	cfg.Steps.RiskAssessment = stepConfig{Agent: "claude", Prompt: riskPrompt}
	return cfg
}

// parseConfig validates data, the config file at path; readPrompt reads a prompt_file relative to path's dir.
func parseConfig(path string, data []byte, readPrompt func(rel string) ([]byte, error)) (config, error) {
	cfg := defaultConfig()
	var err error
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("config %s: %w", path, err)
	}
	if cfg.autoReviewMaxAge, err = time.ParseDuration(cfg.AutoReviewMaxAge); err != nil || cfg.autoReviewMaxAge < 0 {
		return cfg, fmt.Errorf("config %s: auto_review_max_age must be a non-negative duration such as \"168h\", got %q", path, cfg.AutoReviewMaxAge)
	}
	seen := make(map[string]bool, len(cfg.Repositories))
	for _, repo := range cfg.Repositories {
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") || strings.IndexFunc(repo, unicode.IsSpace) >= 0 {
			return cfg, fmt.Errorf("config %s: repository %q is not owner/name", path, repo)
		}
		if seen[repo] {
			return cfg, fmt.Errorf("config %s: duplicate repository %q", path, repo)
		}
		seen[repo] = true
	}
	if cfg.Concurrency < 1 {
		return cfg, fmt.Errorf("config %s: concurrency must be positive, got %d", path, cfg.Concurrency)
	}
	for i := range cfg.Skip {
		r := &cfg.Skip[i]
		field, pattern, set := "", "", 0
		for _, f := range []struct{ name, value string }{{"label", r.Label}, {"title", r.Title}, {"author", r.Author}} {
			if f.value != "" {
				field, pattern, set = f.name, f.value, set+1
			}
		}
		if set != 1 {
			return cfg, fmt.Errorf(`config %s: skip[%d]: must set exactly one of "label", "title" or "author"`, path, i)
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return cfg, fmt.Errorf("config %s: skip[%d].%s: invalid regular expression %q: %w", path, i, field, pattern, err)
		}
		r.re = re
	}
	for name, sc := range map[string]*stepConfig{"code_review": &cfg.Steps.CodeReview, "security_review": &cfg.Steps.SecurityReview,
		"risk_assessment": &cfg.Steps.RiskAssessment} {
		if sc.Agent != "claude" && sc.Agent != "codex" {
			return cfg, fmt.Errorf("config %s: steps.%s.agent must be \"claude\" or \"codex\", got %q", path, name, sc.Agent)
		}
		if sc.PromptFile == "" {
			continue
		}
		prompt, err := readPrompt(sc.PromptFile)
		if err == nil && strings.TrimSpace(string(prompt)) == "" {
			err = fmt.Errorf("%s is empty", sc.PromptFile)
		}
		if err != nil {
			return cfg, fmt.Errorf("config %s: steps.%s.prompt_file: %w", path, name, err)
		}
		sc.Prompt = string(prompt)
	}
	return cfg, nil
}
