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

// oldInstall writes a pre-rename uReview dir under base: config, data dir with log and metrics DB.
func oldInstall(t *testing.T, base string) string {
	t.Helper()
	old := filepath.Join(base, "uReview")
	if err := os.MkdirAll(filepath.Join(old, "ureview-data"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"ureview.json":                `{"repositories": ["a/b"]}`,
		"ureview-data/ureview.log":    "log",
		"ureview-data/metrics.db":     "db",
		"ureview-data/metrics.db-wal": "wal",
	} {
		if err := os.WriteFile(filepath.Join(old, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return old
}

func TestMigrateOldDir(t *testing.T) {
	setup := func(t *testing.T) string {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("MYREVIEW_CONFIG", "")
		t.Setenv("MYREVIEW_DATA_DIR", "")
		migrationNote = ""
		base, err := os.UserConfigDir()
		if err != nil {
			t.Fatal(err)
		}
		return base
	}
	read := func(path string) string { b, _ := os.ReadFile(path); return string(b) }

	t.Run("old dir only is moved", func(t *testing.T) {
		base := setup(t)
		old := oldInstall(t, base)
		c, d, err := resolvePaths()
		dir := filepath.Join(base, "myReview")
		if err != nil || c != filepath.Join(dir, "myreview.json") || d != filepath.Join(dir, "myreview-data") {
			t.Fatalf("resolvePaths() = %q, %q, %v", c, d, err)
		}
		if read(c) != `{"repositories": ["a/b"]}` || read(filepath.Join(d, "myreview.log")) != "log" ||
			read(filepath.Join(d, "metrics.db")) != "db" || read(filepath.Join(d, "metrics.db-wal")) != "wal" {
			t.Error("config, log or metrics DB not moved")
		}
		if _, err := os.Lstat(old); !os.IsNotExist(err) {
			t.Errorf("old dir still there: %v", err)
		}
		if migrationNote != "moved "+old+" to "+dir {
			t.Errorf("migrationNote = %q", migrationNote)
		}
	})
	t.Run("both dirs exist: untouched", func(t *testing.T) {
		base := setup(t)
		old := oldInstall(t, base)
		os.MkdirAll(filepath.Join(base, "myReview"), 0700)
		c, _, err := resolvePaths()
		if err != nil || read(filepath.Join(old, "ureview.json")) == "" || read(filepath.Join(old, "ureview-data/ureview.log")) != "log" {
			t.Errorf("old dir changed, err %v", err)
		}
		if _, err := os.Lstat(c); !os.IsNotExist(err) || migrationNote != "" {
			t.Errorf("new config appeared: %v, note %q", err, migrationNote)
		}
	})
	t.Run("neither exists: fresh setup", func(t *testing.T) {
		base := setup(t)
		if _, err := os.Lstat(filepath.Join(base, "uReview")); !os.IsNotExist(err) {
			t.Fatal(err)
		}
		cfg, _, _, _, url, err := startup()
		if err != nil || url != "/settings?first=1" || len(cfg.Repositories) != 0 || migrationNote != "" {
			t.Errorf("startup() = url %q, repos %v, err %v, note %q", url, cfg.Repositories, err, migrationNote)
		}
		if _, err := os.Lstat(filepath.Join(base, "uReview")); !os.IsNotExist(err) {
			t.Errorf("old dir created: %v", err)
		}
	})
	t.Run("env vars set: no migration", func(t *testing.T) {
		base := setup(t)
		old := oldInstall(t, base)
		scratch := t.TempDir()
		t.Setenv("MYREVIEW_CONFIG", filepath.Join(scratch, "c.json"))
		t.Setenv("MYREVIEW_DATA_DIR", filepath.Join(scratch, "data"))
		if _, _, err := resolvePaths(); err != nil {
			t.Fatal(err)
		}
		if read(filepath.Join(old, "ureview.json")) == "" {
			t.Error("old config moved")
		}
		if _, err := os.Lstat(filepath.Join(base, "myReview")); !os.IsNotExist(err) {
			t.Errorf("new dir created: %v", err)
		}
	})
	for _, env := range []string{"MYREVIEW_CONFIG", "MYREVIEW_DATA_DIR"} {
		t.Run("only "+env+" set: moved before myReview is created", func(t *testing.T) {
			base := setup(t)
			oldInstall(t, base)
			t.Setenv(env, filepath.Join(t.TempDir(), "x"))
			if _, _, err := resolvePaths(); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(base, "myReview")
			if read(filepath.Join(dir, "myreview.json")) == "" || read(filepath.Join(dir, "myreview-data/myreview.log")) != "log" {
				t.Error("old dir not moved; an empty myReview would block the migration on every later start")
			}
		})
	}
	t.Run("rename error is returned", func(t *testing.T) {
		base := setup(t)
		old := oldInstall(t, base)
		os.Chmod(old, 0500) // entries in it cannot be renamed
		t.Cleanup(func() { os.Chmod(old, 0700) })
		if _, _, err := resolvePaths(); err == nil {
			t.Fatal("resolvePaths() succeeded over a failed rename")
		}
		if _, err := os.Lstat(filepath.Join(base, "myReview")); !os.IsNotExist(err) {
			t.Errorf("new dir created after the failure: %v", err)
		}
		if _, _, _, _, _, err := startup(); err == nil {
			t.Error("startup() succeeded over a failed rename")
		}
	})
}
