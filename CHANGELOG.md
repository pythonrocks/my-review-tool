# Changelog

## [1.1.0](https://github.com/pythonrocks/my-review-tool/compare/v1.0.1...v1.1.0) (2026-11-24)


### Features

* kind tags on findings next to the severity badge ([#8](https://github.com/pythonrocks/my-review-tool/issues/8)) ([a194c4c](https://github.com/pythonrocks/my-review-tool/commit/a194c4cd259484cd4de90f888a8716c6f86de41d))
* one editable comment per point with description and original comment in tooltips ([#3](https://github.com/pythonrocks/my-review-tool/issues/3)) ([f8a8e70](https://github.com/pythonrocks/my-review-tool/commit/f8a8e7030ac6e7e618e5606c6ec4a0a59f5599d9))
* show PR diff stats on the list and details views ([#6](https://github.com/pythonrocks/my-review-tool/issues/6)) ([732d719](https://github.com/pythonrocks/my-review-tool/commit/732d71948ff97e9945741122f719c0bbc66dce4d))
* status column with approved, changes requested and commented markers ([#5](https://github.com/pythonrocks/my-review-tool/issues/5)) ([68cfe0b](https://github.com/pythonrocks/my-review-tool/commit/68cfe0b174dc1ffa6013444e857673e965b8681d))


### Bug Fixes

* send each response as one write so the Diff tab does not hang ([#7](https://github.com/pythonrocks/my-review-tool/issues/7)) ([ef1b127](https://github.com/pythonrocks/my-review-tool/commit/ef1b12757bc3c348961735bf56bef13f4a41992d))

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
