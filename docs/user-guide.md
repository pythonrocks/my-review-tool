# User guide

How myReview runs and how you review pull requests with it. Setup is in [Configuration](configuration.md); internals are in [Architecture](architecture.md).

## Requirements

The [GitHub CLI](https://cli.github.com) (`gh`), logged in with `gh auth login` (or with `GH_TOKEN` set in the environment myReview runs in). myReview polls at startup and then every 60 seconds, and keeps the open pull requests in memory. Each poll runs two `gh pr list` calls per watched repository: one for the open pull requests, and one with `--search "review-requested:@me"` to flag those requesting your review. It also runs `gh api user` once per poll until the login is known. Team review requests are matched by GitHub's `review-requested:@me` search itself, so no `read:org` scope is needed; search indexing may lag by about a minute, so a new request can show up one poll late. Without `gh`, or when it is not logged in, the service still starts and retries on the next poll.

A review agent CLI for each agent your config uses (see [Review agents](configuration.md#review-agents)), installed, in `PATH` and logged in: [Claude Code](https://docs.claude.com/en/docs/claude-code) (`claude`, the default for every step) and/or the [Codex CLI](https://github.com/openai/codex) (`codex`). The flags myReview passes were verified on Claude Code 2.1.323 and codex-cli 0.166.0; other versions may reject or change them. A missing CLI fails the step with `<agent> not found in PATH`; one that is not logged in fails it with its own error message.

## Run

```sh
make run
```

or open the installed `myReview.app`. myReview opens one window, `myReview`, with the dashboard. It runs no web server and listens on no TCP port: the window's requests go straight to myReview's handler inside the app (`wails://localhost`). Links to other sites (GitHub pull requests, threads, files) open in your default browser; the window itself never leaves myReview.

- Closing the window only hides it: polling and reviews keep running. Click the Dock icon, or **Open myReview** in the menu-bar icon (a check mark in a circle), to show it again.
- **Quit** (myReview menu, Cmd-Q, **Quit myReview** in the menu-bar icon, or Ctrl-C / `SIGTERM` when started from a terminal) stops polling, kills running review agents and exits.
- Starting myReview while it already runs shows the running window, and the second copy exits before it reads the config or touches the data directory.
- The menu bar has the myReview menu (About, **Settings…** (Cmd-,), Services, Hide, Quit) and an Edit menu (Undo, Redo, Cut, Copy, Paste, Select All).

At startup myReview prints `myReview logging to <data dir>/myreview.log` on stderr and from then on writes everything, including crash output, to that file (appended, mode 0600, never rotated: delete it to start over). Errors before that point (an invalid config, a bad data directory, a log file it cannot open) are printed on stderr and shown in an error dialog; its Quit button quits myReview with status 1. A failure to start the window comes after that point: it is written to stderr, which is then the log file, and myReview exits with status 1 without a dialog.

myReview asks your login shell for `PATH` (`$SHELL -lic`, 5 s timeout), so `gh`, `claude` and `codex` installed through your shell profile are found even when it is not started from a terminal. The new `PATH` is the login shell's entries, then the inherited ones, then `/opt/homebrew/bin` and `/usr/local/bin`, without duplicates. If `SHELL` is unset or the shell fails or times out, it uses the inherited `PATH` plus those two directories and logs `login shell PATH: <reason>; …`.

## Review workflow

Every open pull request on the dashboard gets a review workflow automatically, without any action from you. After each poll the full list of open pull requests goes to an in-memory review queue:

- a pull request the queue has not seen and that has no saved result is queued, in dashboard order (ready before drafts, oldest first), drafts included, if it was created at most `auto_review_max_age` ago (default 7 days, `"0s"` for no limit). An older one is `on demand`: nothing runs until you click Review (`POST /rerun`), and from then on it follows the normal rules, including re-review after new commits. On demand is never saved and is recomputed on every poll; a new commit keeps it on demand;
- a pull request with new commits is handled as described in [New commits and drafts](#new-commits-and-drafts);
- at most `concurrency` workflows run at once; the rest wait in first-in, first-out order;
- a pull request that a successful fetch no longer lists (merged or closed) is dropped from the queue, and its run is cancelled. A failed fetch, or `gh` missing or logged out, keeps the last good list, so it never cancels anything.

A workflow first reads the repository's policy file in a **policy** phase (see [Repository policy](configuration.md#repository-policy)), under the same `stepTimeout` as a step. When it fails the workflow shows `failed: policy: <reason>` and nothing else runs.

Then it runs a **prepare** phase once, shared by all agent steps. It creates a temp directory (`myreview-*` in the OS temp dir) and:

- downloads the PR's head commit with `gh api repos/<owner>/<name>/tarball/<headSHA>` and extracts it into `src/` inside it, without GitHub's top-level `owner-name-sha/` directory. Only regular files and directories are written; symlinks, hard links and devices are skipped. An entry with an absolute path or `..` fails prepare, and so does a checkout over 500 MiB (`checkout too large: over 500 MiB`). The checkout has no `.git`;
- fetches the diff with `gh pr diff <number> --repo github.com/<owner>/<name>`. A diff over 256 KiB fails prepare (`diff too large for review: <N> KiB, limit 256 KiB`); an empty diff is reviewed normally;
- fetches the PR description with `gh pr view <number> --repo github.com/<owner>/<name> --json body --jq .body` (one more `gh` call per workflow), cut to 16 KiB. A failure of this call fails prepare like the diff fetch; an empty description is not an error.

Prepare has the same `stepTimeout` as a step. When it fails the workflow shows `failed: prepare: <reason>` (for a `gh` error, its message) and no agent step runs. The temp directory is removed when the workflow ends: done, failed or cancelled. The limits are constants, not configurable.

Then it runs code review and security review in parallel, then risk assessment with both reviews' findings if both succeeded. The first step to fail cancels the other one and is reported; risk assessment does not run after a failure. Each step has its own timeout, `stepTimeout`, a 10-minute constant (not configurable); a step that hits it fails with `timed out after 10m0s`. Other failure reasons are cut to 300 characters.

Code review and security review each run a review agent on the checkout (see [Review agents](configuration.md#review-agents)). Besides its findings, the code review returns the **overview**: a short plain-language summary of what the PR changes and why. Risk assessment runs a third agent (see [Risk assessment](#risk-assessment)), so each PR costs three agent runs. It fails with `failed: risk assessment: <reason>`, and then nothing of the run is kept.

A done workflow keeps the overview, the findings of code review and security review, each tagged with its step, and the risk level, recommendation, rationale and points of interest in memory, along with the head commit it reviewed. A re-run clears them when it queues; a failed workflow keeps none.

Statuses: `queued` (waiting for a slot), `running`, `done: <N> critical, <N> high, <N> medium, <N> low` or `done: no findings`, `failed: <step>: <reason>` (kept until a re-run).

Re-run: on a `queued` PR it changes nothing (the PR keeps its place); on a `running` PR it cancels the current run and queues a fresh one at the back; on a `done`, `failed` or `on demand` PR it queues a fresh run at the back. A PR never has more than one live run.

### New commits and drafts

A done or failed result is **outdated** when it was produced for an older commit than the PR's current head (as of the latest poll). An empty head from GitHub never makes a result outdated. When the queue sees a new head commit:

- `done` or `failed`: the result is kept and becomes outdated; the wait for the quiet period starts now;
- `queued`: the job is switched to the new head and keeps its place in the queue;
- `running`: the run is cancelled, its result is dropped, and the PR is queued for the new head at the back. Drafts too.

After every poll, a **ready** PR with an outdated result is queued once its head has stayed unchanged for `quiet_period` (default 5 minutes); every new push restarts the wait. Pushes are seen on polls (every 60 s), so a re-review starts up to a minute after the quiet period ends. A **draft** with an outdated result is never queued automatically: Re-run it, or mark it ready, which queues it once its quiet period has passed (normally at once). Marking a PR as draft does not stop a queued or running review, but an outdated result waiting for its quiet period stays as it is. If the head returns to the reviewed commit (for example after a force-push), the result is current again and nothing is queued. Re-run works on any PR at any time and skips the quiet period. Queuing clears the outdated result, as a re-run does.

### Risk assessment

The risk agent gets the same input as the review agents (see [Review agents](configuration.md#review-agents)), followed by:

```

## Sensitive areas
Set by the repository's maintainers on its default branch, or myReview's defaults.
<JSON array of {"name","description","paths"} for the sensitive areas in effect, or the line: None.>

## Findings
<JSON array of {"step","severity","file","line","description"} from both reviews, or the line: None.>
```

with a note that the findings are untrusted data derived from the PR. It reports which sensitive areas the PR touches and any extra points of interest; it never decides the recommendation or the risk level. myReview computes those itself from the findings and the agent's answer.

The sensitive areas and the severity threshold come from the repository's policy file; without one they are `authentication`, `authorization` and `data model` (a name only: empty description, no paths) and `High`. The reviewer's config cannot change them.

**Path matches.** For every sensitive area with `paths`, each file the PR diff changes (from its `diff --git`, `---`/`+++` and rename lines) that matches one of the patterns adds an entry for that area: the file, line 0, reason `changed file matches <pattern>`, title `Sensitive area <area>`, suggested comment `This change touches <area> code. Please confirm it has been checked with that in mind.`, one per area and file. These entries come before the agent's, so an area with paths is touched even if the agent misses it.

**Decision.** Severities rank Critical > High > Medium > Low. A finding is *at or above the threshold* when its severity ranks at or above the threshold in effect (`High` by default; the examples below use it). An area is *touched* when it has at least one entry, from a path match or from the agent.

- Recommendation `human review required` if any finding is at or above the threshold or any sensitive area is touched; otherwise `automated review is sufficient`.
- Risk level:

| Risk | When |
|---|---|
| `High` | any Critical finding, or a sensitive area touched together with any High finding |
| `Medium` | human review required, and not High |
| `Low` | automated review is sufficient |

**Rationale**: these lines, in this order, only those that apply:

- `<n> finding(s) at or above the High threshold.` (`1 finding ...`, `2 findings ...`);
- `Touches sensitive areas: <areas in the order of the areas in effect, comma-separated>.`;
- `No finding at or above the High threshold and no sensitive area touched.` (only when automated review is sufficient);
- one risk line: `Risk High: a Critical finding.` (also when an area and a High finding are present too), `Risk High: a sensitive area touched together with a High finding.`, `Risk Medium: human review required.` or `Risk Low: automated review is sufficient.`

**Points of interest**, each a file, line, reason, title and suggested comment, in this order:

1. every finding at or above the threshold, sorted by severity, file and line, reason `<Severity> <step> finding: <description>`, the finding's title and suggested comment;
2. every sensitive-area entry, path matches first, then the agent's in its order, reason `Sensitive area <area>: <reason>`, the entry's title and suggested comment;
3. the agent's own `points_of_interest` in its order, as given.

### Diff view

`?view=diff` on the details page shows the PR's diff for a done review of the current head only. Otherwise it shows one alert: `The diff is shown once the review is done.`, or, when the result is outdated or GitHub answers for another commit, `The diff is shown for the reviewed commit only; the pull request moved to <short head>. Re-run or wait for the new review.` A `gh` error shows `Could not load the diff: <error>`; reloading retries.

- **Source.** `gh api "repos/<owner>/<name>/pulls/<number>/files?per_page=100&page=<p>"`, at most 3 pages. Only the diff view and its endpoints call it; the list view, the point endpoints and polling never do. The parsed diff is kept in memory per PR for the reviewed commit (at most 20 PRs, oldest fetch evicted first; errors are not kept).
- **Limits** (constants): 300 files (then `Only the first 300 files are shown. Open on GitHub for the rest.`), 128 KiB per file patch, 2 MiB of patches in total, 1 MiB per file read by Expand. A file over a limit, a binary file, or one GitHub sends without a patch still gets its header, with `Diff not available (<reason>)`; a renamed file with no changes shows `Renamed without changes`.
- **Left panel.** One link per file (base name, full path on hover, the number of listed items on that file), a `Files with points only` checkbox (needs JavaScript, `static/details.js`; without it every file shows), and a **Points** index: each "What to check" item with its number and title, linked to its block in the diff.
- **Files.** A header with the path (`old → new` for a rename), `+<additions> −<deletions>` and the status, then the hunks: old number, new number, markers, sign, code. Long lines scroll horizontally. The new-side number of a context or added line opens the line comment form below the line (see [Line comments](#line-comments)), or, on a line with your comment, links to it; removed files and deleted lines have none. Between hunks, before the first and after the last (not for added or removed files), `↕ Expand` loads the hidden lines in place.
- **Markers and blocks.** Each item on a new-side diff line gets a numbered marker on that line, linked to its block below the line: the same row as in the list (checkbox, number, badge, title, state) that opens to the same body. A line with your line comment, or with an item whose comment is in the review, also gets a comment marker `C`. Each line comment of the reviewed commit shows open below its line with its editor.
- **Items outside the diff.** An item on an unchanged line is shown on that line, with 3 lines before and after it (clamped to the file; overlapping or touching ranges merged), each range headed `Not changed in this PR` and outlined with a muted band. In a file of the PR the range sits in its gap (the rest of the gap still has `↕ Expand`; old numbers as for Expand); for a file not in the PR (or past the file limit) it is under the file's name in **Other files**, with old = new. Its marker and block are on the line as for diff lines; the line has no comment link, and GitHub threads stay at the top of the file. The file text is read only for paths of such items (agent paths must be safe relative paths), at most 8 at a time within 5 s, and kept with the cached diff (`errNotAvailable` too; errors and timeouts are not kept). Otherwise the item's block is at the top of its file (`Not on a diff line`) or under its name in Other files: line 0 or past the end of the file, a removed or added file, a file without a diff, a binary or >1 MiB file, an unsafe path, or a failed or slow read.
- **Open in diff.** Each item's body links `Open in diff →` to `?view=diff&point=<id>#d-<id>`, which renders that item's block open and highlighted; an unknown `point` is ignored.

Patch and file text are shown as plain text, never as HTML.

### Discussions

The details page shows the PR's existing GitHub review threads ("discussions").

- **Fetch.** Every full page load fetches them with one `gh api graphql --paginate` call (`reviewThreads`, the first 100 comments of each thread; 10 s cap). Fragments use that copy and fetch only when there is none. When the fetch fails the page shows `Could not load GitHub discussions: <error>` and everything else works. Polling never calls GitHub.
- **Placement.** Thread lines refer to the PR's current head, so threads are placed only when the done result is of that commit; otherwise the list view shows `GitHub discussions are shown when the review is of the current head (<short sha>).` In the list view, a row with a line shows the threads on the RIGHT side of that line: a marker with the number of comments in unresolved threads, and in its body the unresolved threads open and resolved ones under a closed `Resolved conversation`. In the diff view, a RIGHT thread goes under the added or context line with its new number, a LEFT thread under the first deleted or context line with its old number. Unresolved threads show open under the line, after the points (`Conversation`); resolved ones are a small `✓` icon at the right edge of the line (`aria-label="Resolved conversation (N comments)"`), a `<details>` that expands the thread in place without JavaScript. Outdated threads, file-level threads and threads whose line is not in the diff go to a block at the top of their file, labelled `Outdated`, `File` or `Line <n>`. Threads on files not in the diff (past the file limit) are not shown.
- **Replies** are drafts: `Reply` saves yours in the pending review (shown as `Pending`), and it is posted only when you submit the review, inside that one review (see [Submitting a review](#submitting-a-review)); they count in `Submit review (N)` and are listed in the dialog. Replies to threads that are gone or locked at submit are not sent and are removed.
- **Resolve / Unresolve** is an immediate GitHub write on that click, the only one outside Submit.
- Thread text (bodies, logins, paths) is untrusted: it is shown as escaped plain text, never as Markdown or HTML, and never put into htmx attributes.
- A review with replies is submitted in three steps (create a pending review, add the replies, submit it). GitHub allows one pending review per user and PR, so if you started a review in GitHub's web UI and left it pending, a submit with replies fails with 422 and nothing is posted; submit or discard that review on GitHub first.

### Line comments

In the diff view you can comment on any new-side line (context or added) of the reviewed commit, with or without a point on it. A line comment is a draft with the id `c<n>` (n is one more than the highest `c` number in the PR's pending file), the validated file and line, the reviewed commit and an empty source text. It is created in the review (`Draft`), at most one per line and commit, at most 200 per PR. Edit it like any draft (autosave, `Remove from review` / `Add to review`); empty its text to delete it.

The list view shows them after the points in a **Your comments** group, in id order: the badge `Comment`, `file:line` and the state, without number or checkbox, expanding to the editor and `Open in diff →` (`?view=diff#d-c<n>`). An **Outdated** comment (its line is gone in the reviewed commit, see below) shows `file:line (on <short sha of the commit it was written on>)`, the state `Outdated`, the editor, Add/Remove and `Discard` (saves an empty text, which deletes it), without `Open in diff`; the diff view does not place it on any line, and a new comment on its old line is a new comment. An **unflagged** comment (a point draft whose finding is gone) shows `No longer flagged by the review.` above its editor and is a line comment everywhere. They do not count in `N of M checked` or the `What to check` number, but do count in `Submit review (N)`. Submit posts them after the items' comments, inline on the RIGHT side of their line, and marks them `Posted`.

### Checked marks and comment drafts

Checked marks and comment drafts are saved per pull request in `<MYREVIEW_DATA_DIR>/pending/<repo#number, URL-escaped>.json` (`rev`, a counter raised by every change; `checked`, `[{"id","file","line","text"}]`; `drafts`, `[{"id","file","line","commit","source_text","text","edited","in_review","posted_url","outdated_from","no_longer_flagged"}]`; `result`, the result id it was last reconciled with; `rows`, that result's number of rows; `opened_at`, when you first opened the details page after the latest AI result in the current review cycle, and `opened_result`, the id of that result; a full page load sets both when they are empty or the result changed, the `?submitted=` redirect and the page's partial requests never do, and a successful submit clears them while a timed-out one keeps them), written the same way as results. It is a separate file because a re-run or a new commit deletes the result; the pending file stays until the pull request is closed or merged (same rule as results: after a successful fetch of its repository). An unreadable or corrupt file is reported on stderr and the PR starts with no marks and drafts.

- A mark applies to a row while its id, file, line and text (the finding's description or the point's reason) all match; a draft while id, file, line and source text match and its `commit` is the reviewed commit. Other records stay in the file, unchanged, and are not shown or counted.
- The editor of an expanded row starts with the suggested comment. States: `Suggestion` (not in the review, never edited), `Edited`, `Draft` (in the review), `Draft edited`, and `Posted` (read-only editor with a link to the review; also for a finding posted by an earlier version, whose `posted_url` in the result file is still read).
- Checking, saving, Add/Remove and submitting need JavaScript (htmx); without it the rows still expand.
- A **result id** is the first 16 hex characters of the SHA-256 of a done or failed result's status, head, failed step, reason, findings and points, so an identical re-run keeps it. When a page load or `Reload review` sees a done result whose id differs from `result`, it **reconciles** the pending review (the files below are read with `gh` before, at most 50 files, 20 s in all):
  1. posted drafts, and drafts that already apply to a row of the new result, stay as they are;
  2. untouched suggestions (not edited, not posted) are dropped, even in the review: the new rows show their new suggestions;
  3. an edited finding or point draft moves to a new row of the same kind and file with exactly its source text (no other draft on it; the one on the draft's moved line first), keeping its text, edited and in-review flags;
  4. an edited one with no such row becomes a line comment `c<n>` marked `no_longer_flagged`, text and flags kept;
  5. a line comment (and one from 4) of another commit moves to its line in the new commit: the same line if its text is unchanged, else the only line with that text; when there is none, or the file cannot be read (unsafe path, missing, binary, over 1 MiB, a `gh` error), it stays on its old line with `outdated_from` set to its old commit: the state **Outdated**. Nothing typed is lost;
  6. a checked mark moves to a new row with the same file, line and text, other marks are dropped;
  7. `result`, `rows` and `rev` are updated, so a submit dialog opened before is refused.
- **Stale pages.** Every htmx request of the details page sends `results`, the result id the page shows (`static/details.js`). Checking, saving, Add/Remove, `POST …/drafts` and `POST …/review` require it to be the current result's id, and the point body, excerpt, comment form, Expand and the submit dialog check it when sent; otherwise they answer 409 `New review results arrived; reload the review. Text typed since then was not saved.` and change nothing. The message shows at the bottom of the decision bar (the editor says `Not saved` and keeps its text), in the comment form, or in the dialog.

### Submitting a review

Nothing is posted or approved automatically: only a submitted review posts, through `POST …/review`, under the `gh` login shown on the dashboard. It creates **one** GitHub review (`gh api --method POST repos/<owner>/<name>/pulls/<number>/reviews --input -`) with the decision (`APPROVE`, `REQUEST_CHANGES` or `COMMENT`), the summary and every included comment, in row order. Comments go to the reviewed commit of a done result (inline on the RIGHT side of their line; line 0 comments go into the review body under `` **`<file>`** ``), otherwise to the PR head. An Outdated comment goes into the body too, headed `` `<file>:<line>` (outdated: written on <short sha>) `` (no `:<line>` for line 0), since its line belongs to an older commit. When GitHub refuses the inline comments (HTTP 422, for example a line outside the diff), it is sent once more with every comment in the body, each headed `` `<file>:<line>` ``.

The dialog (opened by `Submit review (N)` or `Comment or request changes…`) shows:

- the decision radios (on your own PR only `Comment`); changing one reloads the dialog with the summary kept;
- `Summary (optional)`;
- `Comments to post (N)`: each comment's `file:line` and the first line of its text, cut to 80 characters with `…`, or `No comments in this review.`;
- `K of M points are not checked yet. You can still submit.` while points are unchecked (a warning only);
- for Request changes or Comment with no comment: `Add a comment or a summary to request changes.` (or `… to comment.`), and the summary becomes required;
- for an outdated result: a warning that the comments and the decision go to the reviewed commit `<short sha>`, not the current head;
- for Approve: `Approving needs confirmation:` with the reasons and a required "Approve anyway" checkbox, when the results are outdated, there are none yet (unknown, queued, running or on demand), the review was skipped or failed. Human review required (the rationale and the findings above the threshold) is shown in the bar but needs no confirmation;
- `Back to review` (closes the dialog) and a button naming the outcome, such as `Approve`, `Approve and post 2 comments`, `Request changes with 1 comment`, `Comment` or `Post 3 comments`.

The server checks, in order, and on any refusal changes nothing: the event (400); your own PR with Approve or Request changes (409); new commits since the page loaded (409); drafts changed since the dialog opened (`rev`, 409: open the dialog again); for Approve, confirmation lines that changed or are not confirmed (409); Request changes or Comment with no comment and an empty summary (409); a review of the PR already being submitted (409). After a successful submit every posted comment shows `Posted`, linked to the review, and `rev` grows, so the same dialog cannot post twice. A `gh` failure posts nothing and the dialog can be submitted again. If `gh` times out (30 s) the message says the review may have been posted, check GitHub; `rev` grows anyway, so a blind resubmit is refused.

- Each details page load asks GitHub for your reviews of the PR (`gh api .../reviews`). When your latest review approves the current head the page shows `You approved commit <short sha>.`; the controls stay. If the check fails, or the `gh` login is not known yet, the page shows `Could not check your approvals on GitHub: <message>`.
- The dashboard's Approved column and the details page's Approvals line come from the regular poll, not from this check, so they show a new approval by the next poll (at most 60 s). An approval submitted through myReview shows right away (on an older commit when the reviewed commit is not the head); the next poll replaces it with what GitHub returns, and a poll that started before the submit can hide it until the following one. Request changes and Comment change nothing until the next poll. A user's latest review that is not `COMMENTED` or `PENDING` counts: a later `CHANGES_REQUESTED` or a dismissal cancels an approval. `gh` returns only the oldest 100 reviews of a PR, so on a PR with more reviews a newer one can be missed and the status can be wrong.

## AI metrics

Every run that started is also recorded, for good, in `<MYREVIEW_DATA_DIR>/metrics.db`, an SQLite database (mode 0600), and so is every review you submit (see [Your reviews](#your-reviews)). This includes failed runs and dropped ones (a Re-run, a new commit, a closed PR, or quitting), which count with outcome `cancelled` because they cost money too. Nothing is ever deleted from it: closing a PR, pruning results and deleting result files leave it alone, and it has no size cap or retention. Delete the file to start over. The details page never reads it.

- `ai_runs`, one row per run: `run_id` (the same as in the result file's `metrics`), `repo`, `number`, `head_sha`, `attempt`, `queued_at`, `started_at`, `finished_at`, `outcome` (`done`, `failed` or `cancelled`), `failed_step`, `threshold`, `threshold_source`, `diff_files`, `diff_bytes` (NULL when the run failed at policy or prepare) and `app_version` (as `GET /healthz` reports it). A run that failed at policy or prepare has no steps.
- `ai_steps`, one row per step that started (`seq` 0, 1, 2 in the result file's order): `step`, `agent`, `prompt_sha`, `started_at`, `finished_at`, `wall_ms`, `api_ms`, `turns`, the four token counts, `cost_usd`, `status` (`done`, `failed`, `cancelled`), `error_kind` and `items` (NULL unless the step finished).
- `ai_step_models`, one row per model a step reported, with its own token counts and `cost_usd`: one per claude model, one `codex default` row for codex.
- Times are UTC RFC 3339 text.
- `error_kind`, for failed and cancelled steps only: `timeout`, `cancelled`, `not_found` (unknown agent, or its CLI is not in `PATH`), `exit` (non-zero exit, or a codex failure event), `agent_error` (claude reported an error), `bad_output` (an unreadable reply) or `other`. The failure reason itself stays in the result file only.
- `prompt_sha` is the hex SHA-256 of the step's prompt. It changes when the prompt is edited in Settings, in a `prompt_file` or in `.myreview.json`, so `GROUP BY step, prompt_sha` compares prompt versions.
- Cost has three states. **Priced**: `cost_usd` set (claude). **No price**: tokens set, `cost_usd` NULL (codex). **Unknown**: every usage column NULL and no model rows (the agent was killed or its reply unreadable). Claude's cost is the list price, an estimate, not necessarily what a subscription pays.
- No agent text goes in: no failure reasons, findings, summaries, rationale, points, prompts or code.
- The database is opened at startup. If it cannot be opened, or was written by a newer myReview (left untouched then), the history is off until the next start and only `myreview.log` says so; reviews work as before. A write that fails, or a record that finds the writer 64 records behind, is logged and skipped. Runs still running when quitting takes too long are not recorded.

Examples (months in local time; `-readonly` is safe while myReview runs). `known_usd` sums priced steps only, so it reads `0.00` for codex-only rows; `unknown_steps` counts steps with no report:

```sh
# Cost per month per repository
sqlite3 -readonly "<data dir>/metrics.db" "
SELECT strftime('%Y-%m', r.started_at, 'localtime') AS month, r.repo, COUNT(DISTINCT r.run_id) AS runs,
       printf('%.2f', SUM(s.cost_usd)) AS known_usd,
       SUM(s.input_tokens IS NULL) AS unknown_steps
FROM ai_runs r JOIN ai_steps s USING (run_id) GROUP BY 1, 2 ORDER BY 1, 2;"

# Cost and tokens per model per month
sqlite3 -readonly "<data dir>/metrics.db" "
SELECT strftime('%Y-%m', r.started_at, 'localtime') AS month, m.model,
       printf('%.2f', SUM(m.cost_usd)) AS known_usd,
       SUM(m.input_tokens + m.output_tokens) AS tokens
FROM ai_runs r JOIN ai_step_models m USING (run_id) GROUP BY 1, 2 ORDER BY 1, 2;"
```

### Your reviews

Each review you submit (see [Submitting a review](#submitting-a-review)) adds one row to `review_submissions`, so a PR reviewed in several rounds has one row per submit. Only counts go in: no comment text, summaries, file paths, line numbers, thread ids or code.

- `review_submissions`: `id`, `submitted_at`, `repo`, `number`, `head_sha` (the commit the review was posted on), `run_id` (the AI run whose result you reviewed, joins `ai_runs`; NULL without a done result or when that run is not in the database), `event` (`APPROVE`, `REQUEST_CHANGES`, `COMMENT`), `opened_at`, `seconds_to_submit`, `points_total` (the "What to check" rows), `points_checked` (rows with a checked mark), `ai_suggested` (rows with a comment the agent suggested, not the description or reason it falls back to), `accepted_as_is` and `modified` (row drafts posted unedited and edited), `own_comments` (your line comments posted), `replies` (thread replies posted), `risk`, `recommendation`, `uncertain` and `app_version`. Without a done result `points_*`, `ai_suggested`, `risk` and `recommendation` are 0 or NULL.
- `review_step_counts`, one row per step that has "What to check" rows in the review: `submission_id` (`review_submissions.id`), `step` (`code review`, `security review` or `risk assessment` for points of interest, the same values as `ai_steps.step`), `suggested`, `accepted_as_is` and `modified`. A submission's `ai_suggested`, `accepted_as_is` and `modified` are the sums over its step rows; `own_comments` and `replies` have no step.
- `uncertain` is 1 when GitHub did not answer in time and the review may or may not have been posted. Submitting again then adds a second row. The examples below leave these rows out with `uncertain = 0`. A posted review whose drafts could not be saved afterwards counts as posted (`uncertain = 0`).
- Speed: `opened_at` is the first time you opened the details page after the latest AI result, and `seconds_to_submit` is wall time from then to the submit, so a page opened and left for days counts those days. Both are NULL when the page was not opened in that round (for example, a pending file from before this version).
- Drafts on rows without an AI suggestion count in `accepted_as_is` or `modified` too, so the used share below can pass 100%.

```sh
# Reviews per month and event
sqlite3 -readonly "<data dir>/metrics.db" "
SELECT strftime('%Y-%m', submitted_at, 'localtime') AS month, event, COUNT(*) AS reviews
FROM review_submissions WHERE uncertain = 0 GROUP BY 1, 2 ORDER BY 1, 2;"

# Median time to submit per month (seconds)
sqlite3 -readonly "<data dir>/metrics.db" "
WITH t AS (SELECT strftime('%Y-%m', submitted_at, 'localtime') AS month, seconds_to_submit AS secs
           FROM review_submissions WHERE uncertain = 0 AND seconds_to_submit IS NOT NULL),
     s AS (SELECT month, secs, ROW_NUMBER() OVER (PARTITION BY month ORDER BY secs) AS i,
                  COUNT(*) OVER (PARTITION BY month) AS n FROM t)
SELECT month, n AS reviews, AVG(secs) AS median_seconds
FROM s WHERE i IN ((n + 1) / 2, (n + 2) / 2) GROUP BY month ORDER BY month;"

# How AI comments were used per month
sqlite3 -readonly "<data dir>/metrics.db" "
SELECT strftime('%Y-%m', submitted_at, 'localtime') AS month, SUM(ai_suggested) AS suggested,
       SUM(accepted_as_is) AS as_is, SUM(modified) AS modified, SUM(own_comments) AS own,
       ROUND(100.0 * SUM(accepted_as_is + modified) / NULLIF(SUM(ai_suggested), 0)) AS used_pct
FROM review_submissions WHERE uncertain = 0 GROUP BY 1 ORDER BY 1;"

# How AI comments were used per step and prompt version
sqlite3 -readonly "<data dir>/metrics.db" "
SELECT c.step, s.prompt_sha, SUM(c.suggested) AS suggested, SUM(c.accepted_as_is) AS as_is, SUM(c.modified) AS modified,
       ROUND(100.0 * SUM(c.accepted_as_is + c.modified) / NULLIF(SUM(c.suggested), 0)) AS used_pct
FROM review_step_counts c JOIN review_submissions r ON r.id = c.submission_id
JOIN ai_steps s ON s.run_id = r.run_id AND s.step = c.step
WHERE r.uncertain = 0 GROUP BY c.step, s.prompt_sha ORDER BY 1, 2;"
```
