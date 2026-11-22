package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxRepos        = 100
	maxPromptBytes  = 64 << 10
	repoViewTimeout = 15 * time.Second
)

var (
	errNotOwnerName = errors.New("not owner/name")
	errRepoNotFound = errors.New("not found or no access")
	errRepoTimeout  = errors.New("timed out")

	ghOwnerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,99}$`)
	ghNameRe  = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]{0,99}$`)
)

// validGitHubRepo says whether s is owner/name in GitHub's charset; unlike loadConfig's rule it refuses
// anything gh could read as a flag or a path.
func validGitHubRepo(s string) bool {
	owner, name, ok := strings.Cut(s, "/")
	return ok && ghOwnerRe.MatchString(owner) && ghNameRe.MatchString(name) && name != "." && name != ".."
}

// repoViewWith returns ghRepoView over run (runGh, or a fake in tests).
func repoViewWith(run func(ctx context.Context, args ...string) ([]byte, error)) func(ctx context.Context, repo string) (string, error) {
	return func(ctx context.Context, repo string) (string, error) {
		if !validGitHubRepo(repo) {
			return "", errNotOwnerName
		}
		ctx, cancel := context.WithTimeout(ctx, repoViewTimeout)
		defer cancel()
		out, err := run(ctx, "repo", "view", "--json", "nameWithOwner", "--", repo)
		var g globalError
		switch {
		case err != nil && ctx.Err() == context.DeadlineExceeded:
			return "", errRepoTimeout
		case errors.As(err, &g): // gh missing or logged out says more than "not found"
			return "", err
		case err != nil && ghNotFound(err.Error()):
			return "", errRepoNotFound
		case err != nil: // network, rate limit, ...: gh's own reason, already cut by classifyGhError
			return "", errors.New(cutRunes(err.Error(), 200))
		}
		var v struct{ NameWithOwner string }
		if json.Unmarshal(out, &v) != nil || v.NameWithOwner == "" {
			return "", errRepoNotFound
		}
		return v.NameWithOwner, nil
	}
}

// ghNotFound says whether gh's error for repo view means the repo does not exist or is not visible to the user.
func ghNotFound(msg string) bool {
	return strings.Contains(msg, "Could not resolve to a Repository") || strings.Contains(msg, "HTTP 404")
}

// ghRepoView returns GitHub's owner/name of repo if the gh user can see it.
var ghRepoView = repoViewWith(runGh)

// configHolder is the current config shared by the handler, the poll callback and Save.
type configHolder struct {
	mu        sync.Mutex
	cfg       config
	path      string          // fixed at start
	validated map[string]bool // repos gh confirmed this session, lowercased

	saveMu   sync.Mutex                                             // serializes saveAndApply
	repoView func(ctx context.Context, repo string) (string, error) // ghRepoView; tests replace it
	onApply  func(config)                                           // hands a saved config to the poller and the queue; set by main
}

func newConfigHolder(cfg config, path string) *configHolder {
	return &configHolder{cfg: cfg, path: path, validated: map[string]bool{}, repoView: ghRepoView}
}

func (h *configHolder) get() config {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg
}

// markValidated records that gh confirmed repo this session, so Save does not ask again.
func (h *configHolder) markValidated(repo string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.validated[strings.ToLower(repo)] = true
}

func (h *configHolder) isValidated(repo string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.validated[strings.ToLower(repo)]
}

// saveAndApply saves in and makes the result live, holding saveMu across both so that concurrent saves
// go live in the order they were written. The Settings handlers call only this.
func (h *configHolder) saveAndApply(ctx context.Context, in settingsInput) (config, error) {
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	cfg, err := h.save(ctx, in)
	if err != nil {
		return config{}, err
	}
	h.apply(cfg)
	return cfg, nil
}

// apply makes cfg, returned by save, live: the poller's repos, the queue's workflow, then the holder.
func (h *configHolder) apply(cfg config) {
	if h.onApply != nil {
		h.onApply(cfg)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg = cfg
}

// settingsInput is what the Settings page saves. Agent is "claude", "codex" or "" to keep each step's agent.
type settingsInput struct {
	Repos                                      []string
	Agent                                      string
	CodeReview, SecurityReview, RiskAssessment string // the prompts
}

// save validates in, then writes the changed prompt files and the config, each through a temp file and a
// rename, and returns the new config. Nothing is written unless everything, the resulting config included,
// is valid. Keys other than repositories and the steps' agent and prompt_file are kept as they are.
// The caller holds saveMu (saveAndApply).
func (h *configHolder) save(ctx context.Context, in settingsInput) (config, error) {
	cur, path := h.get(), h.path

	// Phase 1: validate the input.
	if len(in.Repos) > maxRepos {
		return config{}, fmt.Errorf("at most %d repositories, got %d", maxRepos, len(in.Repos))
	}
	repos := make([]string, 0, len(in.Repos))
	seen := map[string]bool{}
	for _, r := range in.Repos {
		if !validGitHubRepo(r) {
			return config{}, fmt.Errorf("repository %q: %w", r, errNotOwnerName)
		}
		if seen[strings.ToLower(r)] { // GitHub names are case-insensitive
			return config{}, fmt.Errorf("duplicate repository %q", r)
		}
		seen[strings.ToLower(r)] = true
		if !containsFold(cur.Repositories, r) && !h.isValidated(r) {
			if _, err := h.repoView(ctx, r); err != nil {
				return config{}, fmt.Errorf("repository %s: %w", r, err)
			}
			h.markValidated(r)
		}
		repos = append(repos, r)
	}
	if in.Agent != "" && in.Agent != "claude" && in.Agent != "codex" {
		return config{}, fmt.Errorf("agent must be \"claude\" or \"codex\", got %q", in.Agent)
	}
	steps := []struct {
		key, file, text, builtin string
		cur                      stepConfig
		promptFile               string
	}{
		{key: "code_review", file: "code-review", text: in.CodeReview, builtin: codeReviewPrompt, cur: cur.Steps.CodeReview},
		{key: "security_review", file: "security-review", text: in.SecurityReview, builtin: securityReviewPrompt, cur: cur.Steps.SecurityReview},
		{key: "risk_assessment", file: "risk-assessment", text: in.RiskAssessment, builtin: riskPrompt, cur: cur.Steps.RiskAssessment},
	}
	pending := map[string][]byte{} // new prompt files by relative path
	for i := range steps {
		s := &steps[i]
		text := strings.ReplaceAll(s.text, "\r\n", "\n")
		switch {
		case strings.TrimSpace(text) == "":
			return config{}, fmt.Errorf("%s prompt is blank", s.key)
		case len(text) > maxPromptBytes:
			return config{}, fmt.Errorf("%s prompt is %d bytes, at most %d", s.key, len(text), maxPromptBytes)
		case !utf8.ValidString(text):
			return config{}, fmt.Errorf("%s prompt is not valid UTF-8", s.key)
		case strings.ContainsRune(text, 0):
			return config{}, fmt.Errorf("%s prompt contains a NUL byte", s.key)
		case text == strings.ReplaceAll(s.cur.Prompt, "\r\n", "\n"):
			s.promptFile = s.cur.PromptFile
		case strings.TrimSpace(text) == strings.TrimSpace(s.builtin):
			s.promptFile = ""
		default:
			s.promptFile = "prompts/" + s.file + ".md"
			pending[s.promptFile] = []byte(text)
		}
	}

	// Phase 2: build the new file in memory and validate it against the pending prompts.
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		data, err = exampleConfig, nil
	}
	if err != nil {
		return config{}, fmt.Errorf("config %s: %w", path, err)
	}
	var top, stepsRaw map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return config{}, fmt.Errorf("config %s: %w", path, err)
	}
	if top == nil {
		top = map[string]json.RawMessage{}
	}
	if raw, ok := top["steps"]; ok {
		if err := json.Unmarshal(raw, &stepsRaw); err != nil {
			return config{}, fmt.Errorf("config %s: steps: %w", path, err)
		}
	}
	if stepsRaw == nil {
		stepsRaw = map[string]json.RawMessage{}
	}
	for _, s := range steps {
		var m map[string]json.RawMessage
		if raw, ok := stepsRaw[s.key]; ok {
			if err := json.Unmarshal(raw, &m); err != nil {
				return config{}, fmt.Errorf("config %s: steps.%s: %w", path, s.key, err)
			}
		}
		if m == nil {
			m = map[string]json.RawMessage{}
		}
		if in.Agent != "" {
			m["agent"] = mustJSON(in.Agent)
		}
		m["prompt_file"] = mustJSON(s.promptFile)
		stepsRaw[s.key] = mustJSON(m)
	}
	top["steps"] = mustJSON(stepsRaw)
	top["repositories"] = mustJSON(repos)
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return config{}, err
	}
	out = append(out, '\n')
	dir := filepath.Dir(path)
	cfg, err := parseConfig(path, out, func(rel string) ([]byte, error) {
		if b, ok := pending[filepath.ToSlash(filepath.Clean(rel))]; ok {
			return b, nil
		}
		return os.ReadFile(filepath.Join(dir, rel))
	})
	if err != nil {
		return config{}, err
	}

	// Phase 3: write every file to a temp file, then rename them all, the config last.
	var temps []string
	defer func() {
		for _, t := range temps {
			os.Remove(t) // a renamed temp is gone already
		}
	}()
	type move struct{ tmp, dst string }
	var moves []move
	if len(pending) > 0 {
		if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0700); err != nil {
			return config{}, err
		}
	}
	for rel, b := range pending {
		dst := filepath.Join(dir, rel)
		tmp, err := writeTemp(filepath.Dir(dst), b)
		if tmp != "" {
			temps = append(temps, tmp)
		}
		if err != nil {
			return config{}, err
		}
		moves = append(moves, move{tmp, dst})
	}
	target := path // a symlinked config (dotfiles) is updated at its target, not replaced by a regular file
	if r, err := filepath.EvalSymlinks(path); err == nil {
		target = r
	}
	tmp, err := writeTemp(filepath.Dir(target), out)
	if tmp != "" {
		temps = append(temps, tmp)
	}
	if err != nil {
		return config{}, err
	}
	moves = append(moves, move{tmp, target})
	for _, m := range moves {
		if err := os.Rename(m.tmp, m.dst); err != nil {
			return config{}, err
		}
	}
	return cfg, nil
}

// writeTemp writes b to a new 0600 temp file in dir, synced, and returns its path ("" if none was created).
func writeTemp(dir string, b []byte) (string, error) {
	f, err := os.CreateTemp(dir, ".myreview-*.tmp") // CreateTemp makes it 0600
	if err != nil {
		return "", err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return f.Name(), err
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only strings, string slices and raw maps are marshalled here
	}
	return b
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}
