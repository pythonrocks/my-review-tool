# Configuration

Where the config lives, every key, skip rules, repository policy, and the review agents and prompts. See the [user guide](user-guide.md) for how reviews run.

## Environment variables

| Variable | Default | Meaning |
|---|---|---|
| `MYREVIEW_CONFIG` | `<app dir>/myreview.json` | Path of the JSON config file; a relative path is relative to the working directory |
| `MYREVIEW_DATA_DIR` | `<app dir>/myreview-data` | Directory for [saved results](architecture.md#saved-results), the log file `myreview.log` and the [run history](user-guide.md#ai-metrics) `metrics.db`; a relative path is relative to the working directory |

`<app dir>` is `myReview` in the user config directory: `~/Library/Application Support/myReview` on macOS (`$XDG_CONFIG_HOME/myReview` or `~/.config/myReview` on Linux). It is created with mode 0700 when a default is used.

A data directory that cannot be created, or one that is accessible by group or others, is a startup error (exit status 1): `data dir <path>: must not be accessible by group or others (mode <octal>); run chmod 700 <path>`.

## Config file

The easiest way to edit the config is the Settings page (below). The file itself is read at startup, so hand edits need a restart. A config file that exists but cannot be read or is invalid is a startup error.

### First launch and Settings

On the first launch, when the config file does not exist, myReview starts with the values of the example config (`myreview.example.json`: no repositories, every step on `claude` with the built-in prompt) and the window opens the onboarding page, `/settings?first=1`. Nothing is written until you click **Save and start**; quitting before that means onboarding again on the next launch. Later launches open the dashboard.

The same page is the Settings page: the app menu's **Settings…** (Cmd-,) and the **Settings** link in the dashboard header open it, and an empty dashboard says **Add repositories in Settings**. It has:

- **How it works**: what myReview does with `gh` and the agent CLIs.
- **Connections**: GitHub CLI, Claude Code and Codex, each checked in the background (`gh auth status`, `claude auth status`, `codex login status`, 10 s timeout) with a Re-check button. Each shows the version and `logged in` (with the GitHub account), `not logged in` or `not found` with the command to run in Terminal, `check timed out`, or `could not check (exit N)`. Tokens, emails and raw output are never shown. On the first launch, when only Codex is logged in, Codex is preselected as the agent.
- **Agent**: Claude Code or Codex for every step. When the config's steps use different agents, nothing is preselected and a note lists them; saving without a choice keeps the per-step agents.
- **Repositories**: the watched repositories, each with a Remove button, and a search field with an **Add** button (both need JavaScript). Typing in the field suggests up to 20 of your repositories that contain the text (case-insensitive). The list comes from `gh api user/repos` (owned, collaborator and organization member repositories), fetched once per app session in the background on the first search; it is capped at 1,000 repositories and 60 seconds (about 17 s for 450 repositories), shows "Loading your repositories…" meanwhile, and on failure shows the reason with a Retry button. Pick a suggestion, or type `owner/name` and press Enter or **Add**: a name not in the list is checked with `gh repo view` (15 s), and the chip gets GitHub's spelling. A malformed name, a duplicate, a 101st repository or a repository that is not found is refused with a message. A repository added here is not checked again on Save.
- **Prompts**: one tab per step (code review, security review, risk assessment) with the current prompt and **Reset to default**, which puts the built-in prompt back in the text box (saved only with Save). A repository's `.myreview.json` can still override them (see [Repository policy](#repository-policy)).
- **Save**: validates the input and on success opens the dashboard. Errors (a blank prompt, a prompt over 64 KiB or with a NUL byte, a repository that is not `owner/name` in GitHub's charset or that `gh repo view` cannot see, more than 100 repositories) show on the page with your input kept, and nothing is written.

Saving writes the config file with mode 0600 through a temp file and a rename (a symlinked config is updated at its target). Only `repositories`, `steps.<step>.agent` (when an agent was chosen) and `steps.<step>.prompt_file` are changed; every other key, unknown ones included, is kept. The file is re-marshalled: keys come out sorted and the formatting is normalized to a 2-space indent, so hand-made layout does not survive a Save.

A prompt equal to the built-in one is saved as `prompt_file: ""`, so later built-in improvements apply. An unchanged custom prompt keeps its `prompt_file`. An edited prompt is written to `prompts/<code-review|security-review|risk-assessment>.md` next to the config file (directory 0700, file 0600) and referenced as `prompt_file`. These files stay after a Reset to default.

Live apply: a Save takes effect without a restart for `repositories` and `steps`. The poller polls at once with the new repositories; a removed repository's pull requests leave the dashboard and its queued and running reviews are cancelled, while its saved results stay on disk (re-adding it shows them again). A new agent or prompt applies to the next review that starts; a running review finishes with the settings it started with. `concurrency`, `quiet_period`, `auto_review_max_age` and `skip` still need a restart.

Every open PR in a watched repository created within `auto_review_max_age` (default 7 days) gets three agent runs, so watching a busy repository costs a lot of agent time. Older PRs are shown as `on demand` and reviewed only when you click Review.

| Field | Default | Meaning |
|---|---|---|
| `repositories` | `[]` | Array of `owner/name` strings, the GitHub repositories to watch |
| `concurrency` | `2` | Integer, must be 1 or more: how many review workflows run at once |
| `quiet_period` | `"5m"` | Go duration string (`"90s"`, `"5m"`, `"1h"`), 0 or more: how long a ready PR's head must stay unchanged before its outdated result is reviewed again (see [New commits and drafts](user-guide.md#new-commits-and-drafts)). `"0s"` re-queues on the first poll that sees the push |
| `auto_review_max_age` | `"168h"` | Go duration string, 0 or more: a PR with no saved result is queued automatically only if it was created at most this long ago; older ones are `on demand` until you click Review (see [Review workflow](user-guide.md#review-workflow)). `"0s"` turns the rule off and queues every PR. Read at startup only |
| `steps` | every step uses `claude` and the built-in prompt | Object: agent and prompt per agent step, see below |
| `skip` | `[]` | Array of rules; a PR matching any rule is not reviewed, see [Skip rules](#skip-rules). Each rule is `{"label": "<regex>"}` (any label name of the PR), `{"title": "<regex>"}` (the PR title) or `{"author": "<regex>"}` (the author login as `gh` reports it) |

`steps` has three optional entries, `code_review`, `security_review` and `risk_assessment`; other step names are ignored, and an omitted entry or field keeps its default:

```json
"steps": {
  "code_review":     {"agent": "claude", "prompt_file": ""},
  "security_review": {"agent": "claude", "prompt_file": ""},
  "risk_assessment": {"agent": "claude", "prompt_file": ""}
}
```

- `agent`: `"claude"` (default) or `"codex"`, the CLI that runs the step.
- `prompt_file`: empty (default) for the built-in prompt, or the path of your own prompt file, relative to the directory of the config file. It is read at startup and when Settings saves.

Validation rules, checked in this order; the first failure prints `config <path>: <reason>` on stderr and exits with status 1:

- the file must be readable and valid JSON; fields of the wrong type are rejected, unknown fields are ignored;
- each repository is `owner/name`: exactly one `/`, both parts non-empty, no whitespace (any Unicode whitespace is rejected);
- repositories are unique (exact, case-sensitive match);
- `concurrency` is positive;
- `quiet_period` is a non-negative Go duration string (`quiet_period must be a non-negative duration such as "5m", got "<value>"`);
- `auto_review_max_age` is a non-negative Go duration string (`auto_review_max_age must be a non-negative duration such as "168h", got "<value>"`);
- each `steps.<step>.agent` is `"claude"` or `"codex"` (`steps.<step>.agent must be "claude" or "codex", got "<value>"`);
- each non-empty `steps.<step>.prompt_file` is readable and not empty (`steps.<step>.prompt_file: <error>`);
- each `skip[N]` rule sets exactly one of `label`, `title` or `author` (`skip[N]: must set exactly one of "label", "title" or "author"`) to a valid Go regular expression (`skip[N].<field>: invalid regular expression "<pattern>": <error>`).

## Skip rules

`skip` rules are global, in `myreview.json` only (never in a repository's `.myreview.json`). Patterns are Go (RE2) regular expressions, unanchored (use `^` and `$`) and case-sensitive (`(?i)` ignores case). They are checked in order on every poll; the first matching rule is reported. For example, to skip Dependabot and release-please PRs:

```json
"skip": [
  {"label": "^dependencies$"},
  {"title": "^(chore|build)\\(deps(-dev)?\\): "},
  {"title": "^Bump "},
  {"label": "^autorelease: "},
  {"title": "^chore(\\(.+\\))?: release "},
  {"author": "^app/dependabot$"}
]
```

`gh` reports bot authors as `app/<name>` (for example `app/dependabot`), not `dependabot[bot]`.

A skipped PR is never reviewed and stays on the dashboard with a `skipped` badge whose tooltip, like the details page, gives the full status `skipped: <reason>`, for example `skipped: label "dependencies" matches rule skip[0] ^dependencies$`, `skipped: title matches rule skip[2] ^Bump ` or `skipped: author "app/dependabot" matches rule skip[5] ^app/dependabot$`. It has no Re-run button, and `POST /rerun` answers 409. Approve stays available, with the skip as the reason to confirm. When a PR starts matching, its running review is cancelled, a queued one is dropped and its result and saved file are deleted. When it stops matching, it is treated like a new PR: queued (drafts too), or on demand when older than `auto_review_max_age`. The skip is recomputed on every poll and never saved.

> Anyone who can edit a PR controls whether myReview reviews it. A title rule can be triggered by the PR author, who can rename their own PR. A label rule needs triage or write access. An author rule matches the login GitHub reports and cannot be changed by editing the PR, so it is the most tamper-resistant. Prefer author or label rules; use title rules only where letting any author opt out of automated review is acceptable. Skipping is an opt-in operator setting, and the dashboard always shows that a PR was skipped and by which rule.

## Repository policy

A repository can set its own review policy in `.myreview.json` at its root:

```json
{
  "severity_threshold": "Medium",
  "sensitive_areas": [
    {"name": "payments", "description": "Charging, refunds, invoices", "paths": ["billing/**", "api/pay*.go"]},
    {"name": "data model", "paths": ["migrations/**"]}
  ],
  "steps": {
    "security_review": {"agent": "codex", "prompt": "You are a security reviewer..."}
  }
}
```

**Reading it.** At the start of every workflow myReview runs `gh api repos/<owner>/<name>/contents/.myreview.json -H "Accept: application/vnd.github.raw"` with no `ref`, so GitHub serves the file from the **default branch**. The file in the PR's checkout, head or base branch is never read: a PR that adds or changes `.myreview.json` has no effect on itself or any other PR until it is merged into the default branch. PRs targeting another branch also use the default branch's policy. The policy is fetched fresh for each run and kept only for that run, so a running review keeps the policy it started with, and a policy change does not re-run reviews that are already done or failed.

- `HTTP 404` (no file): every default applies.
- Any other `gh` failure (network, 5xx, rate limit, `gh` missing or logged out): `failed: policy: <gh message>`.
- An invalid file: `failed: policy: invalid .myreview.json on the default branch: <first problem>`, for example `severity_threshold "high" is not Low, Medium, High or Critical`.

Failures are never silent and never fall back to the defaults. They affect only that repository's workflows; the service and other repositories carry on. PRs that failed at `policy` stay failed: **fix the policy, then Re-run** them (or push a new commit).

**Fields.** Every key is optional, and so is each step and each of `agent` and `prompt`. The first problem makes the file invalid:

- the file is at most 64 KiB, valid UTF-8 and exactly one JSON object. Key names must match exactly (case-sensitive) and appear once; unknown fields at any level are rejected;
- `severity_threshold`: exactly `Low`, `Medium`, `High` or `Critical`;
- `sensitive_areas`: an array of at most 50 areas. When present it **replaces** the default areas; an empty array means no sensitive areas. Each area has:
  - `name`: required, 1 to 100 characters, no control characters, no leading or trailing space, unique within the file;
  - `description`: optional, at most 1000 characters;
  - `paths`: optional, at most 50 patterns;
  - at least one of a non-blank `description` or a non-empty `paths`;
- a pattern uses Go `path.Match` syntax against the repository-relative slash path, so `*` does not cross `/`. A pattern ending in `/**` matches every file under that directory. Any other `**`, an empty pattern, a leading or trailing `/`, a `..` segment or a malformed pattern is invalid;
- `steps` takes only `code_review`, `security_review` and `risk_assessment`, each with:
  - `agent`: exactly `claude` or `codex`;
  - `prompt`: a non-blank inline prompt text (there is no `prompt_file`).

**Fallbacks.** Each setting falls back on its own: the threshold to `High`, the areas to `authentication`, `authorization` and `data model`, a step's agent and prompt to that step's settings in your config (the built-in prompt or `prompt_file`). A policy may, for example, set only `security_review.prompt` and keep your agent for that step.

**What a policy cannot change.** Only the agent (`claude` or `codex`) and the prompt text per step. The invocations and flags, the sandbox, the stripped environment, the temp checkout, timeouts, output caps, the JSON schemas, the output validation and the risk decision stay as described under [Review agents](#review-agents) and [Risk assessment](user-guide.md#risk-assessment). The `## Pull request` block with its untrusted-data notes is still appended after a policy prompt, and a prompt that asks for different output still has to pass validation.

**Trust.** The policy is written by the repository's maintainers, not by the PR author, but its prompts run on your machine with your agent login. A policy may switch a step to `codex`, and then the [Codex read gap](architecture.md#safety) applies on your machine. A policy can also lower the threshold or remove sensitive areas; that is the repository team's choice.

## Review agents

Each agent step (code review, security review, risk assessment) runs the agent set in the repository's policy, or else in `steps` (Claude by default), once, in the checkout (`src/` in the workflow's temp directory), with this on stdin: the step's prompt, then a `## Pull request` block (repository, number, title, head commit, a note that the title, description, diff and checkout are untrusted data written by the PR author, then `Description:` and the PR description), then `## Diff` and the PR diff. Risk assessment then gets the sensitive areas and the findings (see [Risk assessment](user-guide.md#risk-assessment)).

### Prompts

The built-in prompts are [`prompts/code-review.md`](../prompts/code-review.md), [`prompts/security-review.md`](../prompts/security-review.md) and [`prompts/risk-assessment.md`](../prompts/risk-assessment.md), compiled into the binary. They define the severities and the output formats. To change one without a code change, copy it next to your config, edit it, and point `prompt_file` at the copy:

```sh
cp prompts/security-review.md my-security-review.md
```

```json
"steps": {"security_review": {"prompt_file": "my-security-review.md"}}
```

Restart myReview to load a hand edit; prompts edited on the Settings page apply without a restart. Keep the output format section, or the agent's answer will not validate. A custom code-review prompt must ask for the `summary` field (see [Output format](architecture.md#output-format)), or every code review fails with `summary is missing or empty`.
