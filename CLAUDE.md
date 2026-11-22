# CLAUDE.md

Instructions for coding agents and contributors working in this repository.

## What this is

myReview is a macOS app built with Wails v3, in Go, with an htmx UI. All Go code
is one package, `main`, at the repository root. Other directories: `static/`
(JS, CSS, vendored htmx), `prompts/` (review prompts and schemas),
`build/darwin/` (Info.plist and icon) and `docs/`. See
[docs/architecture.md](docs/architecture.md) for how it fits together.

## Build, test and lint

```sh
go test ./...   # tests
make lint       # gofmt, go vet, and go vet for linux/arm64 with the purego tag
make app        # build bin/myReview.app (universal, ad hoc signed)
make dmg        # package bin/myReview.app into bin/myReview.dmg
```

`go test ./...` and `make lint` must pass before a pull request. CI
(`.github/workflows/ci.yml`) runs on pushes to `main` and on pull requests in
the public repository; `.github/workflows/release.yml` makes releases there.

## Dependencies and third-party notices (required)

Any change that adds, removes or upgrades a Go module (`go.mod`/`go.sum`) or a
vendored asset (for example `static/htmx.min.js`) must update
`THIRD_PARTY_NOTICES.md` in the same pull request:

- Rerun the `go list -deps` command from the header of `THIRD_PARTY_NOTICES.md`
  with `GOARCH=arm64` and with `GOARCH=amd64`.
- Add or remove entries to match. For each new or changed module, copy every
  LICENSE, NOTICE, PATENTS and LICENSE-* file verbatim from the module's
  directory in the module cache (`go env GOMODCACHE`).
- On a Wails upgrade, recheck the "Third-party code inside wails/v3" subsection.
  It lists code embedded in the Wails runtime that has no LICENSE file of its own.
- For htmx, also update `static/htmx.md` (version, source URL, SHA-256).
- Do not add a copyleft (GPL, LGPL, AGPL) or MPL dependency to what ships.
  Stop and ask instead.
- `make app` copies `LICENSE.md` and `THIRD_PARTY_NOTICES.md` into the app
  bundle, so keep both file names.

## Conventions

- User-facing docs (`README.md`, `docs/*.md`) carry no internal process
  references and no real organisation or repository names. Tests use
  placeholder repositories such as `a/b` and `o/r`.
- Never commit `myreview.json`, `myreview-data/`, `.env` or `bin/`. They are
  already in `.gitignore`.

## Commits and releases

- Commit messages and PR titles use conventional commits, because they feed
  the changelog and the version (release-please).
- `fix:` gives a patch release, `feat:` a minor release, and `feat!:` or a
  `BREAKING CHANGE:` footer a major release. `docs:`, `chore:`, `ci:`, `test:`
  and `refactor:` do not cause a release on their own.
- The PR title becomes the squash merge commit.
- release-please keeps a release PR open; merging it tags `vX.Y.Z`, creates the
  GitHub release, and the workflow attaches `myReview.dmg`.
- `CHANGELOG.md` and `.release-please-manifest.json` are maintained by
  release-please; do not edit them by hand.

## License

FSL-1.1-ALv2, see [LICENSE.md](LICENSE.md). Contributions need the Contributor
License Agreement: see [CONTRIBUTING.md](CONTRIBUTING.md) and
[docs/CLA.md](docs/CLA.md).
