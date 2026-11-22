# Changelog

## [1.0.1](https://github.com/pythonrocks/my-review-tool/compare/v1.0.0...v1.0.1) (2026-11-22)


### Documentation

* commits and releases section in CLAUDE.md ([9ffb54a](https://github.com/pythonrocks/my-review-tool/commit/9ffb54a33732656c0dbdd8282197af3a5538d465))

## 1.0.0 (2026-11-22)

First public release of **myReview**: a personal pull request review dashboard for macOS. It runs AI code review, security review and risk assessment on the open PRs of the repositories you watch, using your own `claude` or `codex` CLI. You check the points it lists, edit the suggested comments and submit one GitHub review.

### Highlights

* **Dashboard**:
  * open PRs of your watched repositories, refreshed live
  * PRs that request your review come first, with a ⭐ column
  * per-severity finding counts, risk and recommendation
  * your approvals
  * instant filters (org, repo, author, risk, recommendation, hide skipped, hide approved by me) that are kept when you come back from a PR
* **AI review pipeline**:
  * code review, security review and risk assessment, run on a sandboxed checkout of each PR with Claude or Codex
  * results are saved across restarts and marked outdated when new commits arrive
  * automatic review for recent PRs (`auto_review_max_age`), on demand for older ones, and Re-run at any time
* **Details page**:
  * a decision bar and a "What to check" list
  * a diff view with inline points, gutter markers and code excerpts; points outside the diff are shown in place as "Not changed in this PR"
  * keyboard navigation (`j`/`k`, `c`)
* **One GitHub review**:
  * check points, edit the suggested comments, add your own line comments, reply to and resolve existing discussion threads
  * submit everything as a single review (comment, approve or request changes) from a confirmation dialog
* **Repository policy**: a `.myreview.json` on the default branch sets that repository's severity threshold, sensitive areas and prompts.
* **Skip rules**: skip Dependabot, release or other PRs by label, title or author.
* **Settings and first launch**:
  * a welcome page and Settings window: watched repositories with autocomplete, Claude or Codex, editable prompts
  * status checks for `gh`, `claude` and `codex`
  * changes apply live, without a restart
* **AI metrics**:
  * cost, time, model and per-step details on each review
  * every run and every submitted review is recorded in a local `metrics.db` (SQLite), with example queries in the docs
* **macOS app**:
  * a universal (Apple Silicon and Intel) Wails app with a menu-bar icon, menus and a single instance
  * no network listener: the UI is served inside the app window
  * data stays in `~/Library/Application Support/myReview`
