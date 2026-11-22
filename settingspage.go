package main

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"
)

//go:embed settings.html
var settingsHTML string

//go:embed static/settings.js
var settingsJS []byte

var settingsTmpl = template.Must(template.New("settings").Parse(settingsHTML))

var cliCheckTimeout = 10 * time.Second // tests shorten it

// settingsStep is one prompt tab; Key is the config key, also used in the form field and the ids.
type settingsStep struct {
	Key, Label, Agent, Prompt string
}

type settingsData struct {
	First      bool   // first launch: no config file yet
	Agent      string // the preselected agent, "" for none
	Mixed      bool   // the steps use different agents
	Steps      []settingsStep
	Repos      []string
	Error      string // why Save failed
	ConfigPath string
	OOB        bool   // the agent section is an htmx out-of-band swap
	Tab        string // the step key of the selected prompt tab
}

// builtinPrompts are the embedded prompts by step key.
var builtinPrompts = map[string]string{"code_review": codeReviewPrompt, "security_review": securityReviewPrompt, "risk_assessment": riskPrompt}

// settingsData shows the current config; agent, repos and prompts (by step key) replace its values when not nil,
// for a re-render of the user's input.
func (h *configHolder) settingsData(first bool, agent *string, repos []string, prompts map[string]string) settingsData {
	cfg := h.get()
	s := cfg.Steps
	d := settingsData{First: first, Tab: "code_review", Repos: cfg.Repositories, ConfigPath: h.path, Steps: []settingsStep{
		{"code_review", "Code review", s.CodeReview.Agent, s.CodeReview.Prompt},
		{"security_review", "Security review", s.SecurityReview.Agent, s.SecurityReview.Prompt},
		{"risk_assessment", "Risk assessment", s.RiskAssessment.Agent, s.RiskAssessment.Prompt},
	}}
	if a := s.CodeReview.Agent; a == s.SecurityReview.Agent && a == s.RiskAssessment.Agent {
		d.Agent = a
	} else {
		d.Mixed = true
	}
	if agent != nil {
		d.Agent = *agent
	}
	if repos != nil {
		d.Repos = repos
	}
	for i := range d.Steps {
		if p, ok := prompts[d.Steps[i].Key]; ok {
			d.Steps[i].Prompt = p
		}
	}
	return d
}

// cliView is the status fragment of one CLI; OOBAgent swaps in the agent radio with Codex selected.
type cliView struct {
	Name string
	cliStatus
	OOBAgent *settingsData
}

// claudeLoggedIn reports whether the claude check says logged in, within cliCheckTimeout.
func claudeLoggedIn(parent context.Context) bool {
	ctx, cancel := context.WithTimeout(parent, cliCheckTimeout)
	defer cancel()
	return checkCLI(ctx, "claude").State == cliLoggedIn
}

func registerSettings(mux *http.ServeMux, ch *configHolder) {
	html := func(w http.ResponseWriter) { w.Header().Set("Content-Type", "text/html; charset=utf-8") }
	repos := &repoList{}
	mux.HandleFunc("GET /settings/repos", func(w http.ResponseWriter, r *http.Request) {
		repos.start(r.URL.Query().Get("retry") == "1")
		html(w)
		if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
			settingsTmpl.ExecuteTemplate(w, "repo-suggest", repos.match(q))
		}
	})
	// POST /settings/repo adds q (typed or a picked suggestion) as a chip; repo carries the chips already in the form.
	mux.HandleFunc("POST /settings/repo", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		repo, chips := strings.TrimSpace(r.PostFormValue("q")), r.PostForm["repo"]
		fail := func(msg string) {
			html(w)
			w.Header().Set("HX-Retarget", "#repo-error")
			w.Header().Set("HX-Reswap", "innerHTML")
			w.WriteHeader(http.StatusUnprocessableEntity)
			settingsTmpl.ExecuteTemplate(w, "text", msg)
		}
		if !validGitHubRepo(repo) {
			fail(errNotOwnerName.Error())
			return
		}
		if containsFold(chips, repo) {
			fail(repo + " is already added")
			return
		}
		if len(chips) >= maxRepos {
			fail(fmt.Sprintf("at most %d repositories", maxRepos))
			return
		}
		name, ok := repos.find(repo)
		if !ok {
			var err error
			if name, err = ch.repoView(r.Context(), repo); err != nil {
				fail(err.Error())
				return
			}
		}
		if containsFold(chips, name) { // GitHub's name of a renamed repo
			fail(name + " is already added")
			return
		}
		ch.markValidated(name)
		html(w)
		settingsTmpl.ExecuteTemplate(w, "repo-added", name)
	})
	mux.HandleFunc("GET /settings", func(w http.ResponseWriter, r *http.Request) {
		html(w)
		settingsTmpl.Execute(w, ch.settingsData(r.URL.Query().Get("first") == "1", nil, nil, nil))
	})
	mux.HandleFunc("GET /static/settings.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Write(settingsJS)
	})
	mux.HandleFunc("GET /settings/status/{cli}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("cli")
		if _, ok := cliLogins[name]; !ok {
			http.NotFound(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), cliCheckTimeout)
		defer cancel()
		v := cliView{Name: name, cliStatus: checkCLI(ctx, name)}
		// First launch: if only Codex is logged in, preselect it. Claude gets its own timeout, not codex's leftover.
		if name == "codex" && r.URL.Query().Get("first") == "1" && v.State == cliLoggedIn && !claudeLoggedIn(r.Context()) {
			codex := "codex"
			d := ch.settingsData(true, &codex, nil, nil)
			d.OOB = true
			v.OOBAgent = &d
		}
		html(w)
		settingsTmpl.ExecuteTemplate(w, "cli-status", v)
	})
	mux.HandleFunc("GET /settings/prompt-default", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("step")
		text, ok := builtinPrompts[key]
		if !ok {
			http.Error(w, "unknown step", http.StatusBadRequest)
			return
		}
		html(w)
		settingsTmpl.ExecuteTemplate(w, "prompt", settingsStep{Key: key, Prompt: text})
	})
	mux.HandleFunc("POST /settings", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		in := settingsInput{Repos: r.PostForm["repo"], Agent: r.PostFormValue("agent"),
			CodeReview: r.PostFormValue("prompt_code_review"), SecurityReview: r.PostFormValue("prompt_security_review"),
			RiskAssessment: r.PostFormValue("prompt_risk_assessment")}
		_, err := ch.saveAndApply(r.Context(), in)
		if err == nil {
			seeOther(w, r, "/")
			return
		}
		// Every save error (input, the resulting config, a failed write) re-renders the user's input with it.
		agent := in.Agent
		repos := in.Repos
		if repos == nil {
			repos = []string{}
		}
		d := ch.settingsData(r.PostFormValue("first") == "1", &agent, repos,
			map[string]string{"code_review": in.CodeReview, "security_review": in.SecurityReview, "risk_assessment": in.RiskAssessment})
		d.Error = err.Error()
		if _, ok := builtinPrompts[r.PostFormValue("tab")]; ok {
			d.Tab = r.PostFormValue("tab")
		}
		html(w)
		w.WriteHeader(http.StatusUnprocessableEntity)
		if r.Header.Get("HX-Request") == "true" {
			settingsTmpl.ExecuteTemplate(w, "form", d)
			return
		}
		settingsTmpl.Execute(w, d)
	})
}
