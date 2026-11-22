package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const pathMarker = "__MYREVIEW_PATH__"

// pathFallback is appended so Homebrew CLIs are found even when the login shell fails.
var pathFallback = []string{"/opt/homebrew/bin", "/usr/local/bin"}

// loginPath merges the PATH printed after pathMarker in out with current and pathFallback,
// deduplicated in order; ok is false when out has no marker line followed by a PATH line.
func loginPath(out, current string) (path string, ok bool) {
	lines := strings.Split(out, "\n")
	var login string
	for i, l := range lines {
		if strings.TrimSpace(l) == pathMarker && i+1 < len(lines) {
			login, ok = strings.TrimSpace(lines[i+1]), true
			break
		}
	}
	seen := map[string]bool{}
	var dirs []string
	for _, d := range append(strings.Split(login+":"+current, ":"), pathFallback...) {
		if d != "" && !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	return strings.Join(dirs, ":"), ok
}

// setLoginShellPath sets PATH from the user's login shell, so CLIs installed through
// shell rc files are found when myReview is not started from a terminal.
func setLoginShellPath() {
	var out []byte
	err := fmt.Errorf("SHELL is not set")
	if shell := os.Getenv("SHELL"); shell != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, shell, "-lic", "echo "+pathMarker+"; printenv PATH")
		// Own session: an interactive shell must not take over the terminal; on timeout the whole group is killed.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = time.Second
		out, err = cmd.Output() // stdin is /dev/null
	}
	if err != nil {
		out = nil // a failed shell's output is not trusted
	}
	path, ok := loginPath(string(out), os.Getenv("PATH"))
	if err == nil && !ok {
		err = fmt.Errorf("no %s line in its output", pathMarker)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "login shell PATH: %v; using the current PATH plus %s\n", err, strings.Join(pathFallback, ", "))
	}
	os.Setenv("PATH", path)
}
