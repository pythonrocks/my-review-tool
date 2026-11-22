package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

const (
	maxCheckout = 500 << 20 // extracted bytes
	maxDiff     = 256 << 10
	maxBody     = 16 << 10 // bytes kept of the PR description
)

// Errors prepare raises itself about the tarball's content; only these stop gh early.
var (
	errCheckoutTooLarge = errors.New("checkout too large: over 500 MiB")
	errUnsafePath       = errors.New("unsafe path in tarball")
)

// workspace is what prepare hands to the review steps: the checkout of the head commit, the per-run temp dir,
// the PR diff and the PR description. The workflow adds the run's policy.
type workspace struct {
	Dir, Tmp, Diff, Description string
	Policy                      effectivePolicy
}

// prepareFunc fills tmp, a fresh per-run directory removed by the workflow, with the workspace for pr.
type prepareFunc func(ctx context.Context, pr pullRequest, tmp string) (workspace, error)

// newPrepare builds a prepareFunc on top of the gh calls, which tests replace.
// tarball streams the tarball of repo at sha into extract; diff returns the PR diff; body returns the PR description.
func newPrepare(tarball func(ctx context.Context, repo, sha string, extract func(io.Reader) error) error,
	diff, body func(ctx context.Context, repo string, number int) ([]byte, error)) prepareFunc {
	return func(ctx context.Context, pr pullRequest, tmp string) (workspace, error) {
		dir := filepath.Join(tmp, "src")
		if err := tarball(ctx, pr.Repo, pr.HeadSHA, func(r io.Reader) error { return extractTarball(r, dir, maxCheckout) }); err != nil {
			return workspace{}, err
		}
		d, err := diff(ctx, pr.Repo, pr.Number)
		if err != nil {
			return workspace{}, err
		}
		if len(d) > maxDiff {
			return workspace{}, fmt.Errorf("diff too large for review: %d KiB, limit 256 KiB", (len(d)+1023)/1024)
		}
		b, err := body(ctx, pr.Repo, pr.Number)
		if err != nil {
			return workspace{}, err
		}
		b = bytes.TrimSuffix(b, []byte("\n")) // added by --jq
		if len(b) > maxBody {
			b = b[:maxBody]
		}
		return workspace{Dir: dir, Tmp: tmp, Diff: string(d), Description: strings.ToValidUTF8(string(b), "")}, nil
	}
}

var ghPrepare = newPrepare(ghTarball, ghDiff, ghBody)

// extractTarball writes the regular files and directories of a gzipped GitHub tarball into dir,
// without GitHub's top-level owner-name-sha/ directory. Links and devices are skipped.
// More than limit extracted bytes fails with errCheckoutTooLarge (limit is maxCheckout outside tests).
func extractTarball(r io.Reader, dir string, limit int64) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("read tarball: %w", err)
	}
	tr := tar.NewReader(gz)
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tarball: %w", err)
		}
		name := path.Clean(h.Name)
		if path.IsAbs(name) || slices.Contains(strings.Split(name, "/"), "..") {
			return fmt.Errorf("%w: %q", errUnsafePath, h.Name)
		}
		_, rel, _ := strings.Cut(name, "/")
		if rel == "" { // the top-level directory itself, or pax_global_header
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(rel))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			n, err := io.Copy(f, io.LimitReader(tr, limit-total+1))
			total += n
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return fmt.Errorf("read tarball: %w", err)
			}
			if total > limit {
				return errCheckoutTooLarge
			}
		}
	}
}

// ghTarball streams `gh api repos/<repo>/tarball/<sha>` into extract. The caller's ctx bounds it (stepTimeout).
func ghTarball(ctx context.Context, repo, sha string, extract func(io.Reader) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "api", "repos/"+repo+"/tarball/"+sha)
	var stderr cappedBuffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return classifyGhError(err, 0, "")
	}
	extractErr := extract(out)
	if errors.Is(extractErr, errUnsafePath) || errors.Is(extractErr, errCheckoutTooLarge) {
		cancel() // we rejected the content; stop gh
	}
	// Otherwise drain: on a read/gzip error gh may be printing an HTTP error body and about to exit with its message.
	io.Copy(io.Discard, out)
	err = cmd.Wait()
	var exitErr *exec.ExitError
	// gh failing on its own (bad SHA, no access) exits >0 and explains why; a gh we killed exits -1.
	if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 || err != nil && extractErr == nil {
		code := 0
		if exitErr != nil {
			code = exitErr.ExitCode()
		}
		return classifyGhError(err, code, stderr.String())
	}
	return extractErr
}

// ghDiff returns `gh pr diff <number> --repo github.com/<repo>`.
func ghDiff(ctx context.Context, repo string, number int) ([]byte, error) {
	return runGh(ctx, "pr", "diff", fmt.Sprint(number), "--repo", "github.com/"+repo)
}

// ghBody returns `gh pr view <number> --repo github.com/<repo> --json body --jq .body`, the PR description.
func ghBody(ctx context.Context, repo string, number int) ([]byte, error) {
	return runGh(ctx, "pr", "view", fmt.Sprint(number), "--repo", "github.com/"+repo, "--json", "body", "--jq", ".body")
}
