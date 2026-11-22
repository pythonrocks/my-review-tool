package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoginPath(t *testing.T) {
	for _, tc := range []struct {
		name, out, current, want string
		ok                       bool
	}{
		{"noise before marker", "Welcome!\n__MYREVIEW_PATH__\n/a:/b\n", "/c", "/a:/b:/c:/opt/homebrew/bin:/usr/local/bin", true},
		{"missing marker", "/a:/b\n", "/c", "/c:/opt/homebrew/bin:/usr/local/bin", false},
		{"empty output", "", "/c", "/c:/opt/homebrew/bin:/usr/local/bin", false},
		{"marker is last line", "__MYREVIEW_PATH__", "/c", "/c:/opt/homebrew/bin:/usr/local/bin", false},
		{"duplicates", "__MYREVIEW_PATH__\r\n/a:/b:/a::/c\r\n", "/b:/c:/d", "/a:/b:/c:/d:/opt/homebrew/bin:/usr/local/bin", true},
		{"fallback added once", "__MYREVIEW_PATH__\n/usr/local/bin:/a\n", "/opt/homebrew/bin", "/usr/local/bin:/a:/opt/homebrew/bin", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := loginPath(tc.out, tc.current)
			if got != tc.want || ok != tc.ok {
				t.Errorf("loginPath(%q, %q) = %q, %v; want %q, %v", tc.out, tc.current, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestSetLoginShellPath(t *testing.T) {
	t.Setenv("PATH", "/c")
	t.Setenv("SHELL", "")
	setLoginShellPath()
	if got, want := os.Getenv("PATH"), "/c:/opt/homebrew/bin:/usr/local/bin"; got != want {
		t.Errorf("PATH without SHELL = %q, want %q", got, want)
	}
}

func TestSetLoginShellPathRunsShell(t *testing.T) {
	shell := filepath.Join(t.TempDir(), "fakesh")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\necho rc noise\necho __MYREVIEW_PATH__\necho /login:/c\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "/c")
	t.Setenv("SHELL", shell)
	setLoginShellPath()
	if got, want := os.Getenv("PATH"), "/login:/c:/opt/homebrew/bin:/usr/local/bin"; got != want {
		t.Errorf("PATH = %q, want %q", got, want)
	}
}
