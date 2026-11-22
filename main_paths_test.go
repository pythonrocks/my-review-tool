package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolvePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "") // os.UserConfigDir reads it on Linux
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "myReview")
	wd, _ := os.Getwd()
	for _, tc := range []struct {
		name, config, data, wantConfig, wantData string
	}{
		{"defaults", "", "", filepath.Join(dir, "myreview.json"), filepath.Join(dir, "myreview-data")},
		{"relative overrides", "my.json", "mydata", filepath.Join(wd, "my.json"), "mydata"},
		{"absolute overrides", "/etc/u.json", "/var/u", "/etc/u.json", "/var/u"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MYREVIEW_CONFIG", tc.config)
			t.Setenv("MYREVIEW_DATA_DIR", tc.data)
			c, d, err := resolvePaths()
			if err != nil || c != tc.wantConfig || d != tc.wantData {
				t.Errorf("resolvePaths() = %q, %q, %v; want %q, %q", c, d, err, tc.wantConfig, tc.wantData)
			}
		})
	}
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0700 {
		t.Fatalf("myReview dir: %v, %v; want mode 0700", fi, err)
	}
}

func TestStartup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "myreview.json")
	t.Setenv("MYREVIEW_CONFIG", path)
	t.Setenv("MYREVIEW_DATA_DIR", filepath.Join(dir, "data"))

	cfg, _, _, _, url, err := startup()
	if err != nil || url != "/settings?first=1" {
		t.Fatalf("no config: url %q, err %v", url, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("first launch wrote the config: %v", err)
	}
	if len(cfg.Repositories) != 0 || cfg.Concurrency != 2 || cfg.autoReviewMaxAge != 168*time.Hour || cfg.Steps.CodeReview.Agent != "claude" {
		t.Errorf("first-launch config = %+v, want the example's", cfg)
	}

	os.WriteFile(path, []byte(`{"repositories": ["a/b"]}`), 0600)
	if cfg, _, _, _, url, err = startup(); err != nil || url != "/" || len(cfg.Repositories) != 1 {
		t.Errorf("with config: url %q, repos %v, err %v", url, cfg.Repositories, err)
	}
	os.WriteFile(path, []byte(`{"repositories": "a/b"}`), 0600)
	if _, _, _, _, _, err = startup(); err == nil {
		t.Error("an invalid config started")
	}
	os.Remove(path)
	os.Symlink(filepath.Join(dir, "dotfiles", "myreview.json"), path)
	if _, _, _, _, url, err = startup(); err == nil {
		t.Errorf("a dangling symlink config started, url %q", url)
	}
}

func TestLeaveApp(t *testing.T) {
	for _, tc := range []struct {
		url, open string
		back      bool
	}{
		{"", "", false},
		{"wails://localhost/", "", false},
		{"wails://localhost/pr/a/b/3?view=diff#f1", "", false},
		{"https://github.com/a/b/pull/3", "https://github.com/a/b/pull/3", true},
		{"http://example.com/", "http://example.com/", true},
		{"wails://localhost.evil/", "", true},
		{"file:///etc/passwd", "", true},
		{"about:blank", "", true},
		{"data:text/html,x", "", true},
		{"javascript:alert(1)", "", true},
		{"https:///nohost", "", true},
	} {
		if back, open := leaveApp(tc.url); back != tc.back || open != tc.open {
			t.Errorf("leaveApp(%q) = %v, %q; want %v, %q", tc.url, back, open, tc.back, tc.open)
		}
	}
}
