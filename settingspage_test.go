package main

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// settingsReq serves r with a handler over h.
func settingsReq(t *testing.T, h *configHolder, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	q := newReviewQueue(0, workflow{}, resultStore{dir: t.TempDir()})
	newHandler(h, newPoller(nil, nil, noLogin), q, nil, notApproved, noFiles, noContent, noThreads, noResolve).ServeHTTP(rec, r)
	return rec
}

func TestSettingsPageShowsHandEditedConfig(t *testing.T) {
	path := writeConfig(t, handEdited)
	os.MkdirAll(filepath.Join(filepath.Dir(path), "mine"), 0700)
	os.WriteFile(filepath.Join(filepath.Dir(path), "mine", "cr.md"), []byte("my <review> prompt\n"), 0600)
	h, _ := holderFor(t, path)
	rec := settingsReq(t, h, localReq(http.MethodGet, "/settings", ""))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Security-Policy") != csp {
		t.Fatalf("status %d, CSP %q", rec.Code, rec.Header().Get("Content-Security-Policy"))
	}
	body := rec.Body.String()
	mustContain(t, body,
		"<title>Settings — myReview</title>", `<a class="back-link" href="/">`,
		`<input type="hidden" name="repo" value="a/b">`, `<input type="hidden" name="repo" value="c/d">`,
		"Code review uses codex, Security review uses claude, Risk assessment uses claude",
		"my &lt;review&gt; prompt", html.EscapeString(securityReviewPrompt)[:200], `name="prompt_risk_assessment"`,
		`hx-get="/settings/status/gh" hx-trigger="load"`, `<h2 id="repos-h" tabindex="-1">`, `hx-get="/settings/status/codex" hx-trigger="load"`,
		`hx-get="/settings/prompt-default?step=code_review"`, `<button id="save">Save</button>`, "need a restart")
	if strings.Contains(body, `" checked>`) {
		t.Error("an agent is preselected although the steps are mixed")
	}
	if strings.Contains(body, `name="first"`) {
		t.Error("not a first launch")
	}
}

func TestSettingsPageFirstLaunch(t *testing.T) {
	cfg, err := parseConfig("myreview.json", exampleConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := newConfigHolder(cfg, filepath.Join(t.TempDir(), "myreview.json"))
	body := settingsReq(t, h, localReq(http.MethodGet, "/settings?first=1", "")).Body.String()
	mustContain(t, body, "Welcome to myReview", `<button id="save">Save and start</button>`, `<input type="hidden" name="first" value="1">`,
		`value="claude" checked`, `hx-get="/settings/status/codex?first=1" hx-trigger="load"`)
	for _, bad := range []string{"back-link", "different agents", `name="repo"`} {
		if strings.Contains(body, bad) {
			t.Errorf("first launch page has %q", bad)
		}
	}
}

// TestSettingsTemplateHasNoInlineCode backs the CSP: no inline script, style or event handler.
func TestSettingsTemplateHasNoInlineCode(t *testing.T) {
	for _, tag := range regexp.MustCompile(`<script[^>]*>`).FindAllString(settingsHTML, -1) {
		if !strings.Contains(tag, ` src="/`) {
			t.Errorf("inline script %s", tag)
		}
	}
	if m := regexp.MustCompile(`<style|\sstyle=|\son[a-z]+=|hx-on|javascript:`).FindString(settingsHTML); m != "" {
		t.Errorf("template has %q", m)
	}
}

func TestSettingsStatus(t *testing.T) {
	notFound := fmt.Errorf("exec: %w", exec.ErrNotFound)
	all := map[string]fakeCLIResult{
		"gh --version": {out: ghVersion},
		"gh auth status --active --hostname github.com": {out: ghAuthOK},
		"claude --version":   {out: claudeVer},
		"claude auth status": {out: claudeOut},
		"codex --version":    {out: codexVer},
		"codex login status": {out: codexAuthOK},
	}
	tests := []struct {
		path    string
		results map[string]fakeCLIResult
		want    []string
		oob     bool
	}{
		{"/settings/status/gh", all, []string{"logged in</span> gh version 2.72.0 (2025-04-30) as <strong>octo-cat</strong>"}, false},
		{"/settings/status/claude", all, []string{"not logged in</span> 2.1.329 (Claude Code). Run <code>claude auth login</code> in Terminal."}, false},
		{"/settings/status/codex", all, []string{"logged in</span> codex-cli 0.166.0"}, false},
		{"/settings/status/codex?first=1", all, []string{`<section class="panel" id="agent" aria-labelledby="agent-h" hx-swap-oob="true">`, `value="codex" checked`}, true},
		{"/settings/status/codex", map[string]fakeCLIResult{"codex --version": {err: notFound}}, []string{"not found</span> Install it, then run <code>codex login</code>"}, false},
		{"/settings/status/gh", map[string]fakeCLIResult{"gh --version": {out: ghVersion}, "gh auth status --active --hostname github.com": {code: 1}},
			[]string{"not logged in</span>", "<code>gh auth login</code>"}, false},
		{"/settings/status/gh", map[string]fakeCLIResult{"gh --version": {out: "boom", code: 3}}, []string{"could not check (exit 3)"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			old := runCLI
			defer func() { runCLI = old }()
			runCLI = fakeCLI(t, tc.results)
			h := newConfigHolder(defaultConfig(), "myreview.json")
			rec := settingsReq(t, h, localReq(http.MethodGet, tc.path, ""))
			body := rec.Body.String()
			mustContain(t, body, tc.want...)
			if got := strings.Contains(body, "hx-swap-oob"); got != tc.oob {
				t.Errorf("oob agent = %v, want %v", got, tc.oob)
			}
			for _, bad := range []string{"gho_", "Token", "@", "Secret"} {
				if strings.Contains(body, bad) {
					t.Errorf("body shows %q: %s", bad, body)
				}
			}
		})
	}
	t.Run("codex first with claude logged in: no oob", func(t *testing.T) {
		old := runCLI
		defer func() { runCLI = old }()
		res := map[string]fakeCLIResult{}
		for k, v := range all {
			res[k] = v
		}
		res["claude auth status"] = fakeCLIResult{out: claudeOK}
		runCLI = fakeCLI(t, res)
		body := settingsReq(t, newConfigHolder(defaultConfig(), "myreview.json"), localReq(http.MethodGet, "/settings/status/codex?first=1", "")).Body.String()
		if strings.Contains(body, "hx-swap-oob") {
			t.Error("agent swapped although Claude is logged in")
		}
	})
	t.Run("codex first, slow codex: claude gets its own timeout", func(t *testing.T) {
		old, oldT := runCLI, cliCheckTimeout
		defer func() { runCLI, cliCheckTimeout = old, oldT }()
		cliCheckTimeout = 300 * time.Millisecond
		res := map[string]fakeCLIResult{}
		for k, v := range all {
			res[k] = v
		}
		res["claude auth status"] = fakeCLIResult{out: claudeOK}
		fake := fakeCLI(t, res)
		runCLI = func(ctx context.Context, name string, args ...string) ([]byte, int, error) {
			if name == "codex" && args[0] == "login" {
				time.Sleep(250 * time.Millisecond) // leaves 50 ms of codex's timeout, less than claude needs
			} else if name == "claude" {
				time.Sleep(60 * time.Millisecond)
			}
			if ctx.Err() != nil {
				return nil, -1, ctx.Err()
			}
			return fake(ctx, name, args...)
		}
		body := settingsReq(t, newConfigHolder(defaultConfig(), "myreview.json"), localReq(http.MethodGet, "/settings/status/codex?first=1", "")).Body.String()
		mustContain(t, body, "logged in</span> codex-cli")
		if strings.Contains(body, "hx-swap-oob") {
			t.Error("Codex preselected although Claude is logged in")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		old, oldT := runCLI, cliCheckTimeout
		defer func() { runCLI, cliCheckTimeout = old, oldT }()
		cliCheckTimeout = 20 * time.Millisecond
		runCLI = func(ctx context.Context, name string, args ...string) ([]byte, int, error) {
			<-ctx.Done()
			return []byte(secretToken), -1, ctx.Err()
		}
		body := settingsReq(t, newConfigHolder(defaultConfig(), "myreview.json"), localReq(http.MethodGet, "/settings/status/gh", "")).Body.String()
		mustContain(t, body, "check timed out")
		if strings.Contains(body, "gho_") {
			t.Error("raw output shown")
		}
	})
	t.Run("unknown cli", func(t *testing.T) {
		if rec := settingsReq(t, newConfigHolder(defaultConfig(), "myreview.json"), localReq(http.MethodGet, "/settings/status/sh", "")); rec.Code != http.StatusNotFound {
			t.Errorf("status %d, want 404", rec.Code)
		}
	})
}

func TestSettingsPromptDefault(t *testing.T) {
	h := newConfigHolder(defaultConfig(), "myreview.json")
	rec := settingsReq(t, h, localReq(http.MethodGet, "/settings/prompt-default?step=risk_assessment", ""))
	want := `<textarea id="prompt-risk_assessment" name="prompt_risk_assessment" rows="16" spellcheck="false">` + "\n" + html.EscapeString(riskPrompt) + "</textarea>"
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Errorf("status %d, body %q", rec.Code, rec.Body)
	}
	for _, step := range []string{"", "nope", "code-review"} {
		if rec := settingsReq(t, h, localReq(http.MethodGet, "/settings/prompt-default?step="+step, "")); rec.Code != http.StatusBadRequest {
			t.Errorf("step %q: status %d, want 400", step, rec.Code)
		}
	}
}

// settingsForm is a POST /settings body for cfg's current values, with the repos and agent given.
func settingsForm(cfg config, agent string, repos ...string) url.Values {
	v := url.Values{"agent": {agent}, "repo": repos,
		"prompt_code_review": {cfg.Steps.CodeReview.Prompt}, "prompt_security_review": {cfg.Steps.SecurityReview.Prompt},
		"prompt_risk_assessment": {cfg.Steps.RiskAssessment.Prompt}}
	return v
}

func postSettings(t *testing.T, h *configHolder, v url.Values, htmx bool, origin string) *httptest.ResponseRecorder {
	t.Helper()
	r := localReq(http.MethodPost, "/settings", v.Encode())
	if htmx {
		r.Header.Set("HX-Request", "true")
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return settingsReq(t, h, r)
}

func TestSettingsSaveFirstLaunch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "myreview.json")
	cfg, _ := parseConfig(path, exampleConfig, nil)
	h := newConfigHolder(cfg, path)
	f := &fakeRepoView{}
	h.repoView = f.view
	var applied []string
	h.onApply = func(c config) { applied = c.Repositories }
	v := settingsForm(cfg, "codex", "a/b")
	v.Set("first", "1")
	rec := postSettings(t, h, v, true, appOrigin)
	if rec.Code != http.StatusOK || rec.Header().Get("HX-Redirect") != "/" {
		t.Fatalf("status %d, HX-Redirect %q: %s", rec.Code, rec.Header().Get("HX-Redirect"), rec.Body)
	}
	if m := mode(t, path); m != 0600 {
		t.Errorf("config mode %v, want 0600", m)
	}
	got, err := loadConfig(path)
	if err != nil || len(got.Repositories) != 1 || got.Steps.CodeReview.Agent != "codex" || got.Steps.SecurityReview.Agent != "codex" || got.Steps.RiskAssessment.Agent != "codex" {
		t.Errorf("saved %+v, %v", got, err)
	}
	if len(applied) != 1 || h.get().Repositories[0] != "a/b" {
		t.Errorf("not applied live: %v, %v", applied, h.get().Repositories)
	}
	// Without htmx (JS off) the browser gets a 303.
	if rec := postSettings(t, h, settingsForm(h.get(), "", "a/b"), false, ""); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Errorf("plain POST: status %d, Location %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestSettingsSaveKeepsMixedAgents(t *testing.T) {
	path := writeConfig(t, handEdited)
	os.MkdirAll(filepath.Join(filepath.Dir(path), "mine"), 0700)
	os.WriteFile(filepath.Join(filepath.Dir(path), "mine", "cr.md"), []byte("mine\n"), 0600)
	h, _ := holderFor(t, path)
	if rec := postSettings(t, h, settingsForm(h.get(), "", "a/b", "c/d"), true, ""); rec.Header().Get("HX-Redirect") != "/" {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	s := h.get().Steps
	if s.CodeReview.Agent != "codex" || s.SecurityReview.Agent != "claude" || s.CodeReview.PromptFile != "mine/cr.md" {
		t.Errorf("per-step agents or prompt file not kept: %+v", s)
	}
	if rec := postSettings(t, h, settingsForm(h.get(), "claude", "a/b", "c/d"), true, ""); rec.Header().Get("HX-Redirect") != "/" {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if s := h.get().Steps; s.CodeReview.Agent != "claude" || s.SecurityReview.Agent != "claude" || s.RiskAssessment.Agent != "claude" {
		t.Errorf("choosing claude did not set every step: %+v", s)
	}
}

func TestSettingsSaveRefused(t *testing.T) {
	path := writeConfig(t, `{"repositories": ["a/b"]}`)
	h, _ := holderFor(t, path)
	before := readFile(t, path)
	big := strings.Repeat("x", maxPromptBytes+1)
	tests := []struct {
		name string
		edit func(v url.Values)
		want string
	}{
		{"blank prompt", func(v url.Values) { v.Set("prompt_code_review", " \n") }, "code_review prompt is blank"},
		{"big prompt", func(v url.Values) { v.Set("prompt_security_review", big) }, "security_review prompt is 65537 bytes, at most 65536"},
		{"NUL", func(v url.Values) { v.Set("prompt_risk_assessment", "a\x00b") }, "risk_assessment prompt contains a NUL byte"},
		{"bad repo", func(v url.Values) { v.Add("repo", "-R/x") }, "repository &#34;-R/x&#34;: not owner/name"},
		{"bad agent", func(v url.Values) { v.Set("agent", "gpt") }, "agent must be"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := settingsForm(h.get(), "", "a/b")
			v.Set("prompt_code_review", "my edited prompt")
			tc.edit(v)
			for _, htmx := range []bool{true, false} {
				rec := postSettings(t, h, v, htmx, "")
				if rec.Code != http.StatusUnprocessableEntity {
					t.Fatalf("htmx %v: status %d, want 422", htmx, rec.Code)
				}
				body := rec.Body.String()
				mustContain(t, body, `<p class="alert alert-error" role="alert"><span>`, tc.want, `<input type="hidden" name="repo" value="a/b">`)
				if tc.name != "blank prompt" {
					mustContain(t, body, "my edited prompt")
				}
				if htmx == strings.Contains(body, "<!DOCTYPE html>") {
					t.Errorf("htmx %v: whole page = %v", htmx, !htmx)
				}
			}
			if readFile(t, path) != before {
				t.Error("config changed")
			}
		})
	}
	t.Run("selected tab kept", func(t *testing.T) {
		for tab, want := range map[string]string{"risk_assessment": "risk_assessment", "": "code_review", "evil\"x": "code_review"} {
			v := settingsForm(h.get(), "", "a/b")
			v.Set("prompt_code_review", "")
			v.Set("tab", tab)
			body := postSettings(t, h, v, true, "").Body.String()
			mustContain(t, body, `<input type="hidden" name="tab" value="`+want+`">`, `id="tab-`+want+`" aria-controls="panel-`+want+`" aria-selected="true">`)
			if strings.Count(body, `aria-selected="true"`) != 1 {
				t.Errorf("tab %q: not exactly one selected tab", tab)
			}
		}
	})
}

func TestSettingsSaveForeignOrigin(t *testing.T) {
	path := writeConfig(t, `{"repositories": ["a/b"]}`)
	h, _ := holderFor(t, path)
	before := readFile(t, path)
	rec := postSettings(t, h, settingsForm(h.get(), "codex", "a/b"), true, "https://evil.example")
	if rec.Code != http.StatusForbidden || rec.Header().Get("Content-Security-Policy") != csp {
		t.Errorf("status %d, CSP %q", rec.Code, rec.Header().Get("Content-Security-Policy"))
	}
	if readFile(t, path) != before {
		t.Error("config changed")
	}
}
