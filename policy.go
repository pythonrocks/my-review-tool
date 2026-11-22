package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits of a repository policy file (URE-15 spec, "Policy file format").
const (
	maxPolicySize        = 64 << 10
	maxPolicyAreas       = 50
	maxAreaName          = 100
	maxAreaDescription   = 1000
	maxAreaPaths         = 50
	policySourceRepo     = "repository policy"
	policySourceDefault  = "default"
	policyInvalidPrefix  = "invalid .myreview.json on the default branch: "
	policyFileNotFound   = "(HTTP 404)"
	policyContentsAccept = "Accept: application/vnd.github.raw"
)

// sensitiveArea is one sensitive area: from a policy, or a default with only a name.
type sensitiveArea struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Paths       []string `json:"paths"`
}

// policyStep overrides one step's agent and/or prompt; nil means the reviewer's value.
type policyStep struct {
	Agent  *string `json:"agent"`
	Prompt *string `json:"prompt"`
}

// policy is a parsed, valid .myreview.json; nil fields were left out of the file.
type policy struct {
	SeverityThreshold *string          `json:"severity_threshold"`
	SensitiveAreas    *[]sensitiveArea `json:"sensitive_areas"`
	Steps             struct {
		CodeReview     *policyStep `json:"code_review"`
		SecurityReview *policyStep `json:"security_review"`
		RiskAssessment *policyStep `json:"risk_assessment"`
	} `json:"steps"`
}

// effectivePolicy is what one workflow run applies: the policy where it sets a value, the defaults elsewhere.
type effectivePolicy struct {
	Threshold, ThresholdSource string
	Areas                      []sensitiveArea
	AreasSource                string
	CodeReview                 stepConfig
	SecurityReview             stepConfig
	RiskAssessment             stepConfig
}

// parsePolicy decodes and validates .myreview.json bytes. The first problem found is the error.
func parsePolicy(data []byte) (*policy, error) {
	p, problem := decodePolicy(data)
	if problem != "" {
		return nil, errors.New(policyInvalidPrefix + problem)
	}
	return p, nil
}

func decodePolicy(data []byte) (*policy, string) {
	if len(data) > maxPolicySize {
		return nil, "file is larger than 64 KiB"
	}
	if !utf8.Valid(data) {
		return nil, "file is not valid UTF-8"
	}
	// Decode accepts null into a struct, so check for an object first.
	if !bytes.HasPrefix(bytes.TrimLeft(data, " \t\r\n"), []byte("{")) {
		return nil, "file is not a JSON object"
	}
	if problem := checkKeys(json.NewDecoder(bytes.NewReader(data)), ""); problem != "" {
		return nil, problem
	}
	var p policy
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, strings.TrimPrefix(err.Error(), "json: ")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, "more than one JSON value"
	}
	if t := p.SeverityThreshold; t != nil {
		if _, ok := severityRank[*t]; !ok {
			return nil, fmt.Sprintf("severity_threshold %q is not Low, Medium, High or Critical", *t)
		}
	}
	if p.SensitiveAreas != nil {
		if problem := checkAreas(*p.SensitiveAreas); problem != "" {
			return nil, problem
		}
	}
	for _, s := range []struct {
		name string
		step *policyStep
	}{{"code_review", p.Steps.CodeReview}, {"security_review", p.Steps.SecurityReview}, {"risk_assessment", p.Steps.RiskAssessment}} {
		if s.step == nil {
			continue
		}
		if a := s.step.Agent; a != nil && *a != "claude" && *a != "codex" {
			return nil, fmt.Sprintf("steps.%s.agent %q is not claude or codex", s.name, *a)
		}
		if pr := s.step.Prompt; pr != nil && strings.TrimSpace(*pr) == "" {
			return nil, fmt.Sprintf("steps.%s.prompt is blank", s.name)
		}
	}
	return &p, ""
}

// policyKeys lists the exact key names allowed in each object of the file, by position.
var policyKeys = map[string][]string{
	"":                  {"severity_threshold", "sensitive_areas", "steps"},
	"sensitive_areas[]": {"name", "description", "paths"},
	"steps":             {"code_review", "security_review", "risk_assessment"},
	"steps.*":           {"agent", "prompt"},
}

// checkKeys reads one JSON value and rejects wrong-case and duplicate keys, which encoding/json accepts silently.
// Syntax errors are left to the struct decode, which reports them.
func checkKeys(dec *json.Decoder, at string) string {
	tok, err := dec.Token()
	if err != nil {
		return ""
	}
	switch tok {
	case json.Delim('{'):
		known, seen := policyKeys[at], map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return ""
			}
			key := kt.(string)
			if known != nil && !slices.Contains(known, key) {
				return fmt.Sprintf("unknown field %q", key)
			}
			if seen[key] {
				return fmt.Sprintf("duplicate field %q", key)
			}
			seen[key] = true
			child := at + "." + key
			if at == "" {
				child = key
			} else if at == "steps" {
				child = "steps.*"
			}
			if problem := checkKeys(dec, child); problem != "" {
				return problem
			}
		}
		dec.Token()
	case json.Delim('['):
		for dec.More() {
			if problem := checkKeys(dec, at+"[]"); problem != "" {
				return problem
			}
		}
		dec.Token()
	}
	return ""
}

func checkAreas(areas []sensitiveArea) string {
	if len(areas) > maxPolicyAreas {
		return fmt.Sprintf("sensitive_areas has %d areas, at most %d are allowed", len(areas), maxPolicyAreas)
	}
	seen := make(map[string]bool, len(areas))
	for i, a := range areas {
		field := fmt.Sprintf("sensitive_areas[%d]", i)
		switch n := utf8.RuneCountInString(a.Name); {
		case n == 0:
			return field + ".name is required"
		case n > maxAreaName:
			return fmt.Sprintf("%s.name is longer than %d characters", field, maxAreaName)
		case strings.ContainsFunc(a.Name, unicode.IsControl):
			return fmt.Sprintf("%s.name %q contains a control character", field, a.Name)
		case strings.TrimSpace(a.Name) != a.Name:
			return fmt.Sprintf("%s.name %q has leading or trailing space", field, a.Name)
		case seen[a.Name]:
			return fmt.Sprintf("%s.name %q is a duplicate", field, a.Name)
		}
		seen[a.Name] = true
		if utf8.RuneCountInString(a.Description) > maxAreaDescription {
			return fmt.Sprintf("%s.description is longer than %d characters", field, maxAreaDescription)
		}
		if len(a.Paths) > maxAreaPaths {
			return fmt.Sprintf("%s.paths has %d patterns, at most %d are allowed", field, len(a.Paths), maxAreaPaths)
		}
		if strings.TrimSpace(a.Description) == "" && len(a.Paths) == 0 {
			return field + " needs a description or paths"
		}
		for j, pat := range a.Paths {
			if problem := checkPattern(pat); problem != "" {
				return fmt.Sprintf("%s.paths[%d] %q %s", field, j, pat, problem)
			}
		}
	}
	return ""
}

// checkPattern returns why pat is not a valid path pattern, or "".
func checkPattern(pat string) string {
	switch {
	case pat == "":
		return "is empty"
	case strings.HasPrefix(pat, "/"):
		return "starts with /"
	case strings.HasSuffix(pat, "/"):
		return "ends with /; use dir/** for every file under a directory"
	case slices.Contains(strings.Split(pat, "/"), ".."):
		return "has a .. segment"
	}
	dir := strings.TrimSuffix(pat, "/**")
	if strings.Contains(dir, "**") {
		return "uses ** other than at the end after /"
	}
	if _, err := path.Match(dir, ""); err != nil {
		return "is not a valid pattern"
	}
	return ""
}

// matchPattern reports whether the repo-relative slash path file matches a valid pattern pat.
func matchPattern(pat, file string) bool {
	dir, ok := strings.CutSuffix(pat, "/**")
	if !ok {
		m, _ := path.Match(pat, file)
		return m
	}
	for i := range len(file) {
		if file[i] == '/' {
			if m, _ := path.Match(dir, file[:i]); m {
				return true
			}
		}
	}
	return false
}

// resolvePolicy applies p (nil: no policy file) over the defaults and cfg; each setting falls back on its own.
func resolvePolicy(p *policy, cfg config) effectivePolicy {
	e := effectivePolicy{Threshold: defaultThreshold, ThresholdSource: policySourceDefault, AreasSource: policySourceDefault,
		CodeReview: cfg.Steps.CodeReview, SecurityReview: cfg.Steps.SecurityReview, RiskAssessment: cfg.Steps.RiskAssessment}
	for _, name := range defaultSensitiveAreas {
		e.Areas = append(e.Areas, sensitiveArea{Name: name})
	}
	if p == nil {
		return e
	}
	if p.SeverityThreshold != nil {
		e.Threshold, e.ThresholdSource = *p.SeverityThreshold, policySourceRepo
	}
	if p.SensitiveAreas != nil {
		e.Areas, e.AreasSource = *p.SensitiveAreas, policySourceRepo
	}
	for _, s := range []struct {
		sc   *stepConfig
		step *policyStep
	}{{&e.CodeReview, p.Steps.CodeReview}, {&e.SecurityReview, p.Steps.SecurityReview}, {&e.RiskAssessment, p.Steps.RiskAssessment}} {
		if s.step == nil {
			continue
		}
		if s.step.Agent != nil {
			s.sc.Agent = *s.step.Agent
		}
		if s.step.Prompt != nil {
			s.sc.Prompt, s.sc.PromptFile = *s.step.Prompt, ""
		}
	}
	return e
}

// ghPolicy reads .myreview.json from repo's default branch (no ref). nil without error means there is no file.
func ghPolicy(ctx context.Context, repo string) (*policy, error) {
	out, err := runGh(ctx, "api", "repos/"+repo+"/contents/.myreview.json", "-H", policyContentsAccept)
	if err != nil {
		if strings.Contains(err.Error(), policyFileNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return parsePolicy(out)
}

// changedFiles lists the files a unified diff touches, from its "diff --git", ---/+++ and rename lines, without duplicates.
// shortcut: a pure copy ("copy from"/"copy to", no ---/+++ lines) is not listed, handle those lines if diffs start to detect copies.
func changedFiles(diff string) []string {
	var files []string
	seen := map[string]bool{}
	header := false // between a "diff --git" line and its first hunk, where the ---/+++ and rename lines are
	for _, line := range strings.Split(diff, "\n") {
		var name, prefix string
		var ok bool
		if strings.HasPrefix(line, "@@") {
			header = false
		}
		if !header && !strings.HasPrefix(line, "diff --git ") {
			continue
		}
		if name, ok = strings.CutPrefix(line, "diff --git "); ok {
			header = true
			// Empty files, mode-only and binary changes have no ---/+++ lines; take the path when both halves agree.
			if name, ok = gitHeaderPath(name); ok && !seen[name] {
				seen[name] = true
				files = append(files, name)
			}
			continue
		}
		if name, ok = strings.CutPrefix(line, "--- "); ok {
			prefix = "a/"
		} else if name, ok = strings.CutPrefix(line, "+++ "); ok {
			prefix = "b/"
		} else if name, ok = strings.CutPrefix(line, "rename from "); !ok {
			name, ok = strings.CutPrefix(line, "rename to ")
		}
		if !ok {
			continue
		}
		name = strings.TrimSuffix(name, "\t")
		if strings.HasPrefix(name, `"`) {
			var err error
			if name, err = strconv.Unquote(name); err != nil {
				continue
			}
		}
		if name, ok = strings.CutPrefix(name, prefix); !ok || name == "" || name == "/dev/null" {
			continue
		}
		if !seen[name] {
			seen[name] = true
			files = append(files, name)
		}
	}
	return files
}

// gitHeaderPath returns p from `a/<p> b/<p>` or `"a/<p>" "b/<p>"`; ok is false when the halves differ (renames).
func gitHeaderPath(s string) (string, bool) {
	var a, b string
	if strings.HasPrefix(s, `"`) {
		qa, err := strconv.QuotedPrefix(s)
		if err != nil {
			return "", false
		}
		qb, _ := strings.CutPrefix(s[len(qa):], " ")
		if a, err = strconv.Unquote(qa); err != nil {
			return "", false
		}
		if b, err = strconv.Unquote(qb); err != nil {
			return "", false
		}
	} else {
		if len(s)%2 == 0 || s[len(s)/2] != ' ' {
			return "", false
		}
		a, b = s[:len(s)/2], s[len(s)/2+1:]
	}
	p, okA := strings.CutPrefix(a, "a/")
	q, okB := strings.CutPrefix(b, "b/")
	return p, okA && okB && p == q && p != ""
}

// pathMatches returns one area entry per (area, changed file) where the file matches one of the area's patterns.
func pathMatches(areas []sensitiveArea, files []string) []areaEntry {
	var entries []areaEntry
	for _, a := range areas {
		for _, f := range files {
			for _, pat := range a.Paths {
				if matchPattern(pat, f) {
					entries = append(entries, areaEntry{Area: a.Name, File: f, Reason: "changed file matches " + pat, Title: "Sensitive area " + a.Name,
						Comment: "This change touches " + a.Name + " code. Please confirm it has been checked with that in mind."})
					break
				}
			}
		}
	}
	return entries
}
