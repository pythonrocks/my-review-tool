# myReview

myReview is a personal, single-user pull request review dashboard for macOS that runs on your machine.
It runs AI code review, security review and risk assessment on the open pull requests of the repositories you watch, using your own `claude` or `codex` CLI.
You check the points it lists, edit the suggested comments and submit one GitHub review.

See [docs/project-overview.md](docs/project-overview.md) for the full picture, the [user guide](docs/user-guide.md) for daily use, [configuration](docs/configuration.md) for every setting, and [architecture](docs/architecture.md) for the internals.

## Install

myReview is a macOS app (macOS 12 or newer, Apple Silicon and Intel). Download `myReview.dmg` from a [GitHub release](https://github.com/pythonrocks/my-review-tool/releases), open it and drag `myReview.app` onto `Applications`.

The app is not signed with a Developer ID or notarized, so on first launch macOS says it cannot check it for malicious software. Either right-click `myReview.app` in Applications, choose **Open** and confirm (on macOS 15 and newer: try to open it once, then click **Open Anyway** in System Settings → Privacy & Security), or remove the quarantine flag:

```sh
xattr -dr com.apple.quarantine /Applications/myReview.app
```

## Build from source

Needs macOS with Xcode or the Command Line Tools (`xcode-select --install`): myReview is a [Wails v3](https://v3.wails.io) app and builds with cgo.
Go 1.21 or newer. The toolchain pinned in `go.mod` (1.26) is fetched automatically on first use, which needs network access.
If `GOTOOLCHAIN=local` is set, Go 1.26 or newer must be installed by hand.

```sh
make app   # bin/myReview.app, universal (arm64 + x86_64), ad hoc signed
make dmg   # bin/myReview.dmg (after make app) with myReview.app and an Applications link
make run   # build and start it from the terminal
```

Versioning, the release steps and the tests are in [architecture](docs/architecture.md#release).

## Requirements and login

- The [GitHub CLI](https://cli.github.com), logged in: `gh auth login` (or `GH_TOKEN` set in the environment myReview runs in).
- [Claude Code](https://docs.claude.com/en/docs/claude-code) (`claude`, the default for every step) and/or the [Codex CLI](https://github.com/openai/codex) (`codex`), installed and logged in.

myReview reads `PATH` from your login shell, so tools installed through your shell profile are found when it is started from Finder. Details, including the CLI versions the flags were checked on, are in [Requirements](docs/user-guide.md#requirements).

## Configuration

Config, saved results and the log live in `~/Library/Application Support/myReview/`: `myreview.json`, `myreview-data/` and `myreview-data/myreview.log`. `MYREVIEW_CONFIG` and `MYREVIEW_DATA_DIR` override the two paths.

On the first launch the Settings page opens as an onboarding page: it checks `gh`, `claude` and `codex`, and lets you pick the agent, the repositories to watch and the prompts. Nothing is written until you click **Save and start**. Later, open it with **Settings…** (Cmd-,) or the **Settings** link on the dashboard; a Save applies repositories, agents and prompts without a restart. See [First launch and Settings](docs/configuration.md#first-launch-and-settings).

A minimal `myreview.json`:

```json
{
  "repositories": ["owner/name"],
  "steps": {"security_review": {"agent": "codex"}}
}
```

| Key | Default | Meaning |
|---|---|---|
| `repositories` | `[]` | `owner/name` repositories to watch |
| `concurrency` | `2` | how many reviews run at once |
| `quiet_period` | `"5m"` | how long a pushed head must stay unchanged before it is reviewed again |
| `auto_review_max_age` | `"168h"` | older PRs are reviewed only when you click Review |
| `steps` | `claude`, built-in prompts | agent (`claude` or `codex`) and `prompt_file` per step |
| `skip` | `[]` | rules for PRs that are never reviewed |

Every key, the validation rules and the environment variables are in [docs/configuration.md](docs/configuration.md).

## Tips

- **Skip rules**: skip Dependabot, release or other PRs by label, title or author. See [Skip rules](docs/configuration.md#skip-rules).
- **Repository policy**: a repository's `.myreview.json` sets its own severity threshold, sensitive areas and prompts. See [Repository policy](docs/configuration.md#repository-policy).
- **Keys in the details page**: `j` / `k` move between points in the list and the diff, `c` opens the comment of the current point. See [Diff view](docs/user-guide.md#diff-view) and `GET /static/details.js` in [Endpoints](docs/architecture.md#endpoints).
- **Logs and data**: the log is `myreview.log`, results are JSON files and the run history is `metrics.db`, all in the data directory. See [Saved results](docs/architecture.md#saved-results) and [AI metrics](docs/user-guide.md#ai-metrics).

## License

myReview is free to use for any purpose, including at work. The one exception: you may not offer myReview, or a substitute for it, as a commercial product or service.
Each version becomes Apache-2.0 licensed two years after its release.
For commercial or enterprise licensing, open an issue titled "Commercial licensing" at https://github.com/pythonrocks/my-review-tool/issues.

SPDX: `FSL-1.1-ALv2`. See [LICENSE.md](LICENSE.md) for the full text and [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) for the bundled third-party code.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).
