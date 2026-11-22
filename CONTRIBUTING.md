# Contributing to myReview

Issues and pull requests go to https://github.com/pythonrocks/my-review-tool.
For anything larger than a small fix, open an issue first so the approach can be
agreed before you write the code.

## Build, test and lint

You need Go (the version in `go.mod`) on macOS with the Xcode command line tools.

```sh
go test ./...   # tests
make lint       # gofmt, go vet, and go vet for linux/arm64
make run        # run from source
make app        # build bin/myReview.app (universal, ad hoc signed)
make dmg        # package bin/myReview.app into bin/myReview.dmg
```

## Pull requests

- Keep each pull request to one change, and explain what it changes and why.
- Add or update tests for the changed behavior.
- `go test ./...` and `make lint` must pass.
- Update the documentation when the change alters documented behavior.
- Do not add dependencies without discussing them in an issue first.
- For a UI change, add a screenshot to the pull request description.

## Contributor License Agreement

A signed Contributor License Agreement is required before a pull request can be
merged. You sign it through the CLA Assistant check on your pull request; the
check stays red until you do. Read the agreement in [docs/CLA.md](docs/CLA.md).
