package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type fakeCLIResult struct {
	out  string
	code int
	err  error
}

// fakeCLI answers by "name args..." and fails the test on an unexpected command.
func fakeCLI(t *testing.T, results map[string]fakeCLIResult) cliRunner {
	return func(ctx context.Context, name string, args ...string) ([]byte, int, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		r, ok := results[key]
		if !ok {
			t.Errorf("unexpected command %q", key)
			return nil, 0, errors.New("unexpected")
		}
		return []byte(r.out), r.code, r.err
	}
}

const (
	secretToken = "gho_xxxSECRETxxx"
	secretEmail = "someone@example.com"
)

var (
	ghVersion   = "gh version 2.72.0 (2025-04-30)\nhttps://github.com/cli/cli/releases/tag/v2.72.0\n"
	ghAuthOK    = "github.com\n  ✓ Logged in to github.com account octo-cat (keyring)\n  - Active account: true\n  - Git operations protocol: https\n  - Token: gho_***\n  - Token: " + secretToken + "\n  - Email: " + secretEmail + "\n"
	claudeOK    = `{"loggedIn": true, "authMethod": "claude.ai", "email": "` + secretEmail + `", "orgName": "Secret Org", "token": "` + secretToken + `"}`
	claudeOut   = `{"loggedIn": false, "email": "` + secretEmail + `"}`
	claudeVer   = "2.1.329 (Claude Code)\n"
	codexVer    = "codex-cli 0.166.0\n"
	codexAuthOK = "Logged in using ChatGPT " + secretEmail + " " + secretToken + "\n"
)

func TestCheckCLI(t *testing.T) {
	notFound := fmt.Errorf("exec: %w", exec.ErrNotFound)
	tests := []struct {
		name, which string
		results     map[string]fakeCLIResult
		want        cliStatus
	}{
		{"gh logged in", "gh", map[string]fakeCLIResult{
			"gh --version": {out: ghVersion},
			"gh auth status --active --hostname github.com": {out: ghAuthOK},
		}, cliStatus{State: cliLoggedIn, Version: "gh version 2.72.0 (2025-04-30)", Account: "octo-cat", Login: "gh auth login"}},
		{"gh account with an odd charset is dropped", "gh", map[string]fakeCLIResult{
			"gh --version": {out: ghVersion},
			"gh auth status --active --hostname github.com": {out: "Logged in to github.com account " + secretEmail},
		}, cliStatus{State: cliLoggedIn, Version: "gh version 2.72.0 (2025-04-30)", Login: "gh auth login"}},
		{"gh exit 1", "gh", map[string]fakeCLIResult{
			"gh --version": {out: ghVersion},
			"gh auth status --active --hostname github.com": {out: "You are not logged into any GitHub hosts. " + secretToken, code: 1},
		}, cliStatus{State: cliNotLoggedIn, Version: "gh version 2.72.0 (2025-04-30)", Login: "gh auth login"}},
		{"gh not found", "gh", map[string]fakeCLIResult{
			"gh --version": {err: notFound},
		}, cliStatus{State: cliNotFound, Login: "gh auth login"}},
		{"claude logged in", "claude", map[string]fakeCLIResult{
			"claude --version":   {out: claudeVer},
			"claude auth status": {out: claudeOK + "\nwarning on stderr " + secretToken},
		}, cliStatus{State: cliLoggedIn, Version: "2.1.329 (Claude Code)", Login: "claude auth login"}},
		{"claude loggedIn false", "claude", map[string]fakeCLIResult{
			"claude --version":   {out: claudeVer},
			"claude auth status": {out: claudeOut},
		}, cliStatus{State: cliNotLoggedIn, Version: "2.1.329 (Claude Code)", Login: "claude auth login"}},
		{"claude nonzero exit", "claude", map[string]fakeCLIResult{
			"claude --version":   {out: claudeVer},
			"claude auth status": {out: claudeOK, code: 1},
		}, cliStatus{State: cliNotLoggedIn, Version: "2.1.329 (Claude Code)", Login: "claude auth login"}},
		{"claude output is not JSON", "claude", map[string]fakeCLIResult{
			"claude --version":   {out: claudeVer},
			"claude auth status": {out: "Token: " + secretToken},
		}, cliStatus{State: "could not check (exit 0)", Version: "2.1.329 (Claude Code)", Login: "claude auth login"}},
		{"claude not found", "claude", map[string]fakeCLIResult{
			"claude --version": {err: notFound},
		}, cliStatus{State: cliNotFound, Login: "claude auth login"}},
		{"codex logged in", "codex", map[string]fakeCLIResult{
			"codex --version":    {out: codexVer},
			"codex login status": {out: codexAuthOK},
		}, cliStatus{State: cliLoggedIn, Version: "codex-cli 0.166.0", Login: "codex login"}},
		{"codex exit 1", "codex", map[string]fakeCLIResult{
			"codex --version":    {out: codexVer},
			"codex login status": {out: "Not logged in " + secretEmail, code: 1},
		}, cliStatus{State: cliNotLoggedIn, Version: "codex-cli 0.166.0", Login: "codex login"}},
		{"codex not found", "codex", map[string]fakeCLIResult{
			"codex --version": {err: notFound},
		}, cliStatus{State: cliNotFound, Login: "codex login"}},
		{"version exits nonzero", "codex", map[string]fakeCLIResult{
			"codex --version": {out: secretToken, code: 3},
		}, cliStatus{State: "could not check (exit 3)", Login: "codex login"}},
		{"auth command fails to start", "gh", map[string]fakeCLIResult{
			"gh --version": {out: ghVersion},
			"gh auth status --active --hostname github.com": {err: errors.New("permission denied " + secretToken)},
		}, cliStatus{State: "could not check (exit -1)", Version: "gh version 2.72.0 (2025-04-30)", Login: "gh auth login"}},
		{"long version is cut", "codex", map[string]fakeCLIResult{
			"codex --version":    {out: "  " + strings.Repeat("é", 150) + "  \nsecond line " + secretToken},
			"codex login status": {},
		}, cliStatus{State: cliLoggedIn, Version: strings.Repeat("é", 100), Login: "codex login"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := runCLI
			defer func() { runCLI = old }()
			runCLI = fakeCLI(t, tt.results)
			got := checkCLI(context.Background(), tt.which)
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
			assertNoSecrets(t, got)
		})
	}
}

func assertNoSecrets(t *testing.T, st cliStatus) {
	t.Helper()
	for _, f := range []string{st.State, st.Version, st.Account, st.Login} {
		for _, bad := range []string{"gho_", "Token", "@", "Secret"} {
			if strings.Contains(f, bad) {
				t.Errorf("field %q contains %q", f, bad)
			}
		}
	}
}

func TestCheckCLITimeout(t *testing.T) {
	for _, step := range []string{"--version", "auth"} {
		t.Run(step, func(t *testing.T) {
			old := runCLI
			defer func() { runCLI = old }()
			runCLI = func(ctx context.Context, name string, args ...string) ([]byte, int, error) {
				if args[0] == step {
					<-ctx.Done()
					return []byte(secretToken), -1, ctx.Err()
				}
				return []byte(ghVersion), 0, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			got := checkCLI(ctx, "gh")
			if got.State != cliTimedOut {
				t.Errorf("state = %q, want %q", got.State, cliTimedOut)
			}
			assertNoSecrets(t, got)
		})
	}
}

// TestExecCLI checks the real runner: no shell, missing binaries are exec.ErrNotFound,
// exit codes come back without an error, and the context stops the process.
func TestExecCLI(t *testing.T) {
	ctx := context.Background()
	if _, _, err := execCLI(ctx, "myreview-no-such-cli-xyz", "--version"); !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("missing CLI: err = %v, want exec.ErrNotFound", err)
	}
	out, code, err := execCLI(ctx, "sh", "-c", "echo out; echo err >&2; exit 3")
	if err != nil || code != 3 || string(out) != "out\nerr\n" {
		t.Errorf("exit 3: out %q code %d err %v", out, code, err)
	}
	// Arguments are passed as-is, never through a shell.
	out, _, err = execCLI(ctx, "echo", "$HOME;", "`id`")
	if err != nil || string(out) != "$HOME; `id`\n" {
		t.Errorf("echo: out %q err %v", out, err)
	}
	tctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := execCLI(tctx, "sleep", "10"); err == nil {
		t.Error("sleep: want an error after the timeout")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("sleep ran %v after a 50ms timeout", d)
	}
}
