package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fullPolicy = `{
  "severity_threshold": "Medium",
  "sensitive_areas": [
    {"name": "payments", "description": "Charging, refunds, invoices", "paths": ["billing/**", "api/pay*.go"]},
    {"name": "data model", "paths": ["migrations/**"]}
  ],
  "steps": {
    "code_review": {"agent": "codex"},
    "security_review": {"agent": "codex", "prompt": "You are a security reviewer."},
    "risk_assessment": {"prompt": "Assess."}
  }
}`

func TestParsePolicy(t *testing.T) {
	area := func(s string) string { return `{"sensitive_areas":[` + s + `]}` }
	many := func(n int, item string) string { return strings.TrimSuffix(strings.Repeat(item+",", n), ",") }
	for _, tc := range []struct {
		name, data, problem string
	}{
		{"full", fullPolicy, ""},
		{"partial", `{"severity_threshold":"Low"}`, ""},
		{"empty object", ` {} `, ""},
		{"no areas", `{"sensitive_areas":[]}`, ""},
		{"50 areas", area(strings.Join(func() []string {
			var s []string
			for i := range 50 {
				s = append(s, fmt.Sprintf(`{"name":"a%d","description":"d"}`, i))
			}
			return s
		}(), ",")), ""},
		{"over 64 KiB", `{"steps":{"code_review":{"prompt":"` + strings.Repeat("x", 64<<10) + `"}}}`, "file is larger than 64 KiB"},
		{"not UTF-8", "{\"severity_threshold\":\"\xff\"}", "file is not valid UTF-8"},
		{"empty file", ``, "file is not a JSON object"},
		{"array", `[]`, "file is not a JSON object"},
		{"null", `null`, "file is not a JSON object"},
		{"trailing JSON", `{} {}`, "more than one JSON value"},
		{"syntax error", `{"severity_threshold":}`, "invalid character '}' looking for beginning of value"},
		{"unknown top-level field", `{"model":"x"}`, `unknown field "model"`},
		{"unknown area field", area(`{"name":"a","description":"d","tools":[]}`), `unknown field "tools"`},
		{"unknown step", `{"steps":{"deploy":{}}}`, `unknown field "deploy"`},
		{"unknown step field", `{"steps":{"code_review":{"prompt_file":"x"}}}`, `unknown field "prompt_file"`},
		{"wrong-case top-level key", `{"severity_threshold":"High","SEVERITY_THRESHOLD":"Low"}`, `unknown field "SEVERITY_THRESHOLD"`},
		{"wrong-case area key", area(`{"Name":"a","description":"d"}`), `unknown field "Name"`},
		{"wrong-case step", `{"steps":{"Code_Review":{}}}`, `unknown field "Code_Review"`},
		{"wrong-case step key", `{"steps":{"code_review":{"AGENT":"codex"}}}`, `unknown field "AGENT"`},
		{"duplicate top-level key", `{"severity_threshold":"High","severity_threshold":"Low"}`, `duplicate field "severity_threshold"`},
		{"duplicate step key", `{"steps":{"risk_assessment":{"agent":"claude","agent":"codex"}}}`, `duplicate field "agent"`},
		{"threshold case", `{"severity_threshold":"high"}`, `severity_threshold "high" is not Low, Medium, High or Critical`},
		{"threshold empty", `{"severity_threshold":""}`, `severity_threshold "" is not Low, Medium, High or Critical`},
		{"51 areas", area(many(51, `{"name":"a","description":"d"}`)), "sensitive_areas has 51 areas, at most 50 are allowed"},
		{"name missing", area(`{"description":"d"}`), "sensitive_areas[0].name is required"},
		{"name too long", area(`{"name":"` + strings.Repeat("é", 101) + `","description":"d"}`), "sensitive_areas[0].name is longer than 100 characters"},
		{"name control char", area(`{"name":"a\nb","description":"d"}`), `sensitive_areas[0].name "a\nb" contains a control character`},
		{"name space", area(`{"name":" a","description":"d"}`), `sensitive_areas[0].name " a" has leading or trailing space`},
		{"name duplicate", area(`{"name":"a","description":"d"},{"name":"a","paths":["x"]}`), `sensitive_areas[1].name "a" is a duplicate`},
		{"description too long", area(`{"name":"a","description":"` + strings.Repeat("é", 1001) + `"}`), "sensitive_areas[0].description is longer than 1000 characters"},
		{"51 paths", area(`{"name":"a","paths":[` + many(51, `"x"`) + `]}`), "sensitive_areas[0].paths has 51 patterns, at most 50 are allowed"},
		{"no description or paths", area(`{"name":"a","description":"  ","paths":[]}`), "sensitive_areas[0] needs a description or paths"},
		{"empty pattern", area(`{"name":"a","paths":[""]}`), `sensitive_areas[0].paths[0] "" is empty`},
		{"trailing slash", area(`{"name":"a","paths":["migrations/"]}`), `sensitive_areas[0].paths[0] "migrations/" ends with /; use dir/** for every file under a directory`},
		{"leading slash", area(`{"name":"a","paths":["/src/*"]}`), `sensitive_areas[0].paths[0] "/src/*" starts with /`},
		{"dotdot", area(`{"name":"a","paths":["src/../x"]}`), `sensitive_areas[0].paths[0] "src/../x" has a .. segment`},
		{"middle **", area(`{"name":"a","paths":["ok/**","src/**/x.go"]}`), `sensitive_areas[0].paths[1] "src/**/x.go" uses ** other than at the end after /`},
		{"bare **", area(`{"name":"a","paths":["**"]}`), `sensitive_areas[0].paths[0] "**" uses ** other than at the end after /`},
		{"bad pattern", area(`{"name":"a","paths":["src/[a"]}`), `sensitive_areas[0].paths[0] "src/[a" is not a valid pattern`},
		{"bad agent", `{"steps":{"security_review":{"agent":"gpt"}}}`, `steps.security_review.agent "gpt" is not claude or codex`},
		{"blank prompt", `{"steps":{"risk_assessment":{"prompt":" \n"}}}`, "steps.risk_assessment.prompt is blank"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := parsePolicy([]byte(tc.data))
			if tc.problem == "" {
				if err != nil || p == nil {
					t.Fatalf("parsePolicy = %v, %v; want a policy", p, err)
				}
				return
			}
			if want := "invalid .myreview.json on the default branch: " + tc.problem; err == nil || err.Error() != want {
				t.Errorf("parsePolicy error = %v\nwant %s", err, want)
			}
		})
	}
}

func TestResolvePolicy(t *testing.T) {
	var cfg config
	cfg.Steps.CodeReview = stepConfig{Agent: "claude", Prompt: "CR"}
	cfg.Steps.SecurityReview = stepConfig{Agent: "claude", PromptFile: "sec.md", Prompt: "SEC"}
	cfg.Steps.RiskAssessment = stepConfig{Agent: "codex", Prompt: "RISK"}
	defaults := []sensitiveArea{{Name: "authentication"}, {Name: "authorization"}, {Name: "data model"}}
	parse := func(s string) *policy {
		p, err := parsePolicy([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		name string
		p    *policy
		want effectivePolicy
	}{
		{"no policy", nil, effectivePolicy{"High", "default", defaults, "default", cfg.Steps.CodeReview, cfg.Steps.SecurityReview, cfg.Steps.RiskAssessment}},
		{"empty policy", parse(`{}`), effectivePolicy{"High", "default", defaults, "default", cfg.Steps.CodeReview, cfg.Steps.SecurityReview, cfg.Steps.RiskAssessment}},
		{"full policy", parse(fullPolicy), effectivePolicy{"Medium", "repository policy",
			[]sensitiveArea{{"payments", "Charging, refunds, invoices", []string{"billing/**", "api/pay*.go"}}, {"data model", "", []string{"migrations/**"}}},
			"repository policy",
			stepConfig{Agent: "codex", Prompt: "CR"},                           // agent-only override keeps the reviewer's prompt
			stepConfig{Agent: "codex", Prompt: "You are a security reviewer."}, // prompt replaces the prompt_file text
			stepConfig{Agent: "codex", Prompt: "Assess."}}},                    // prompt-only override keeps the reviewer's agent
		{"threshold only", parse(`{"severity_threshold":"Critical"}`), effectivePolicy{"Critical", "repository policy", defaults, "default",
			cfg.Steps.CodeReview, cfg.Steps.SecurityReview, cfg.Steps.RiskAssessment}},
		{"no areas", parse(`{"sensitive_areas":[]}`), effectivePolicy{"High", "default", []sensitiveArea{}, "repository policy",
			cfg.Steps.CodeReview, cfg.Steps.SecurityReview, cfg.Steps.RiskAssessment}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvePolicy(tc.p, cfg); fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", tc.want) {
				t.Errorf("resolvePolicy =\n%#v\nwant\n%#v", got, tc.want)
			}
		})
	}
}

func TestMatchPattern(t *testing.T) {
	for _, tc := range []struct {
		pat, file string
		want      bool
	}{
		{"billing/**", "billing/a.go", true},
		{"billing/**", "billing/x/y/z.go", true},
		{"billing/**", "billing", false},
		{"billing/**", "billingx/a.go", false},
		{"billing/**", "src/billing/a.go", false},
		{"pkg/*/**", "pkg/a/b/c.go", true},
		{"pkg/*/**", "pkg/a.go", false},
		{"api/pay*.go", "api/payments.go", true},
		{"api/pay*.go", "api/pay/x.go", false}, // * does not cross /
		{"*.go", "a/b.go", false},
		{"*.go", "b.go", true},
	} {
		if got := matchPattern(tc.pat, tc.file); got != tc.want {
			t.Errorf("matchPattern(%q, %q) = %v, want %v", tc.pat, tc.file, got, tc.want)
		}
	}
}

func TestChangedFiles(t *testing.T) {
	diff := `diff --git a/new.go b/new.go
new file mode 100644
--- /dev/null
+++ b/new.go
@@ -0,0 +1 @@
+x
diff --git a/old.go b/old.go
deleted file mode 100644
--- a/old.go
+++ /dev/null
@@ -1 +0,0 @@
-x
diff --git a/from.go b/to.go
similarity index 100%
rename from from.go
rename to to.go
diff --git a/mod.go b/mod.go
--- a/mod.go
+++ b/mod.go
@@ -1 +1 @@
-a
+b
--- a/removed-line.go
+++ b/added-line.go
rename from not-a-rename.go
diff --git "a/sp ace\t\303\251.go" "b/sp ace\t\303\251.go"
--- "a/sp ace\t\303\251.go"
+++ "b/sp ace\t\303\251.go"	
diff --git a/tab name.go b/tab name.go
--- a/tab name.go	
+++ b/tab name.go	
diff --git a/auth/empty.py b/auth/empty.py
new file mode 100644
index 0000000..e69de29
diff --git a/gone empty.py b/gone empty.py
deleted file mode 100644
index e69de29..0000000
diff --git a/run.sh b/run.sh
old mode 100644
new mode 100755
diff --git "a/q\303\251 empty" "b/q\303\251 empty"
new file mode 100644
index 0000000..e69de29
diff --git a/img.png b/img.png
Binary files a/img.png and b/img.png differ
`
	want := []string{"new.go", "old.go", "from.go", "to.go", "mod.go", "sp ace\té.go", "tab name.go",
		"auth/empty.py", "gone empty.py", "run.sh", "qé empty", "img.png"}
	if got := changedFiles(diff); fmt.Sprintf("%q", got) != fmt.Sprintf("%q", want) {
		t.Errorf("changedFiles = %q\nwant %q", got, want)
	}
}

func TestPathMatches(t *testing.T) {
	areas := []sensitiveArea{
		{Name: "payments", Paths: []string{"billing/**", "billing/*.go"}},
		{Name: "data model", Paths: []string{"migrations/**"}},
		{Name: "auth", Description: "no paths"},
	}
	got := pathMatches(areas, []string{"billing/a.go", "migrations/1.sql", "README.md", "billing/b/c.go"})
	want := []areaEntry{
		{Area: "payments", File: "billing/a.go", Line: 0, Reason: "changed file matches billing/**", Title: "Sensitive area payments",
			Comment: "This change touches payments code. Please confirm it has been checked with that in mind."},
		{Area: "payments", File: "billing/b/c.go", Line: 0, Reason: "changed file matches billing/**", Title: "Sensitive area payments",
			Comment: "This change touches payments code. Please confirm it has been checked with that in mind."},
		{Area: "data model", File: "migrations/1.sql", Line: 0, Reason: "changed file matches migrations/**", Title: "Sensitive area data model",
			Comment: "This change touches data model code. Please confirm it has been checked with that in mind."},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("pathMatches = %v\nwant %v", got, want)
	}
}

func TestGhPolicy(t *testing.T) {
	good := filepath.Join(t.TempDir(), "p.json")
	os.WriteFile(good, []byte(`{"severity_threshold":"Low"}`), 0o600)
	rec := fakeAgent(t, "gh", "cat "+good)
	p, err := ghPolicy(context.Background(), "o/r")
	if err != nil || p == nil || *p.SeverityThreshold != "Low" {
		t.Fatalf("ghPolicy = %v, %v", p, err)
	}
	if got := readFile(t, filepath.Join(rec, "args")); got != "api\x00repos/o/r/contents/.myreview.json\x00-H\x00Accept: application/vnd.github.raw\x00" {
		t.Errorf("gh args = %q", got)
	}

	fakeAgent(t, "gh", `echo '{"message":"Not Found","status":"404"}'; echo 'gh: Not Found (HTTP 404)' >&2; exit 1`)
	if p, err := ghPolicy(context.Background(), "o/r"); p != nil || err != nil {
		t.Errorf("404: ghPolicy = %v, %v; want no policy", p, err)
	}

	fakeAgent(t, "gh", `echo 'gh: Server Error (HTTP 502)' >&2; exit 1`)
	if p, err := ghPolicy(context.Background(), "o/r"); p != nil || err == nil || err.Error() != "gh: Server Error (HTTP 502)" {
		t.Errorf("502: ghPolicy = %v, %v; want the gh message", p, err)
	}

	fakeAgent(t, "gh", `echo '{"severity_threshold":"high"}'`)
	if _, err := ghPolicy(context.Background(), "o/r"); err == nil || !strings.HasPrefix(err.Error(), "invalid .myreview.json on the default branch: ") {
		t.Errorf("invalid: ghPolicy error = %v", err)
	}
}
