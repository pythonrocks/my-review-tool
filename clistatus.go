package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// cliStatus states; a failed check is "could not check (exit N)".
const (
	cliLoggedIn    = "logged in"
	cliNotLoggedIn = "not logged in"
	cliNotFound    = "not found"
	cliTimedOut    = "timed out"
)

// cliStatus is what the Settings page shows for one CLI. It never holds raw output.
type cliStatus struct {
	State   string
	Version string // first line of --version, at most 100 chars
	Account string // gh only
	Login   string // the command that logs in
}

// cliRunner runs name with args and returns stdout followed by stderr and the exit code;
// err is set only when the command did not run to an exit.
type cliRunner func(ctx context.Context, name string, args ...string) (out []byte, exitCode int, err error)

// runCLI is replaced by a fake in tests.
var runCLI cliRunner = execCLI

// execCLI runs name without a shell; the name is resolved on PATH, which main sets
// from the login shell (setLoginShellPath), so a missing CLI fails with exec.ErrNotFound.
func execCLI(ctx context.Context, name string, args ...string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second // a child holding the pipes must not outlive the timeout
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && ctx.Err() == nil {
		return append(stdout.Bytes(), stderr.Bytes()...), exitErr.ExitCode(), nil
	}
	return append(stdout.Bytes(), stderr.Bytes()...), 0, err
}

var cliLogins = map[string]string{"gh": "gh auth login", "claude": "claude auth login", "codex": "codex login"}

var cliAuthArgs = map[string][]string{
	"gh":     {"auth", "status", "--active", "--hostname", "github.com"},
	"claude": {"auth", "status"},
	"codex":  {"login", "status"},
}

var (
	ghAccountRe = regexp.MustCompile(`Logged in to github\.com account (\S+)`)
	ghLoginRe   = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}$`) // GitHub's username charset; anything else is not shown
)

// checkCLI reports the version and login state of which (gh, claude or codex).
// The caller bounds ctx (10 s); every exec stops when ctx is done.
func checkCLI(ctx context.Context, which string) cliStatus {
	st := cliStatus{Login: cliLogins[which]}
	authArgs, ok := cliAuthArgs[which]
	if !ok {
		st.State = "could not check (exit -1)"
		return st
	}
	// failed classifies a command that did not run to a usable exit.
	failed := func(code int, err error) string {
		switch {
		case ctx.Err() != nil:
			return cliTimedOut
		case errors.Is(err, exec.ErrNotFound):
			return cliNotFound
		case err != nil:
			return "could not check (exit -1)"
		}
		return fmt.Sprintf("could not check (exit %d)", code)
	}

	out, code, err := runCLI(ctx, which, "--version")
	if err != nil || code != 0 || ctx.Err() != nil {
		st.State = failed(code, err)
		return st
	}
	st.Version, _, _ = strings.Cut(string(out), "\n")
	st.Version = strings.TrimSpace(st.Version)
	if r := []rune(st.Version); len(r) > 100 {
		st.Version = string(r[:100])
	}

	out, code, err = runCLI(ctx, which, authArgs...)
	if err != nil || ctx.Err() != nil {
		st.State = failed(code, err)
		return st
	}
	if code != 0 {
		st.State = cliNotLoggedIn
		return st
	}
	st.State = cliLoggedIn
	switch which {
	case "gh":
		if m := ghAccountRe.FindSubmatch(out); m != nil && ghLoginRe.Match(m[1]) {
			st.Account = string(m[1])
		}
	case "claude":
		var v struct {
			LoggedIn *bool `json:"loggedIn"`
		}
		// Decode reads the first JSON value only, so stderr after it does not matter.
		if json.NewDecoder(bytes.NewReader(out)).Decode(&v) != nil || v.LoggedIn == nil {
			st.State = "could not check (exit 0)"
		} else if !*v.LoggedIn {
			st.State = cliNotLoggedIn
		}
	}
	return st
}
