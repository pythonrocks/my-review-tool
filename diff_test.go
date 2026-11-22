package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// noFiles and noContent are the handler's diff funcs in tests of pages that must never call them.
func noFiles(context.Context, string, int) ([]prFile, error) { panic("files called") }

func noContent(context.Context, string, string, string) ([]byte, error) { panic("content called") }

// offlineContent fails every read: points outside the diff fall back to their blocks.
func offlineContent(context.Context, string, string, string) ([]byte, error) {
	return nil, errors.New("offline")
}

func TestParsePatch(t *testing.T) {
	ctx := func(o, n int, s string) diffLine { return diffLine{Kind: "context", Old: o, New: n, Text: s} }
	add := func(n int, s string) diffLine { return diffLine{Kind: "add", New: n, Text: s} }
	del := func(o int, s string) diffLine { return diffLine{Kind: "del", Old: o, Text: s} }
	tests := []struct {
		name  string
		patch string
		want  []hunk
	}{
		{"mixed adds and deletes", "@@ -10,4 +10,5 @@ func f() {\n a\n-b\n-c\n+B\n+C\n+D\n d",
			[]hunk{{10, 4, 10, 5, []diffLine{ctx(10, 10, "a"), del(11, "b"), del(12, "c"), add(11, "B"), add(12, "C"), add(13, "D"), ctx(13, 14, "d")}}}},
		{"multiple hunks", "@@ -1,2 +1,2 @@\n-x\n+y\n z\n@@ -20,2 +20,3 @@\n p\n+q\n r\n",
			[]hunk{{1, 2, 1, 2, []diffLine{del(1, "x"), add(1, "y"), ctx(2, 2, "z")}},
				{20, 2, 20, 3, []diffLine{ctx(20, 20, "p"), add(21, "q"), ctx(21, 22, "r")}}}},
		{"omitted counts", "@@ -3 +3 @@\n-a\n+b",
			[]hunk{{3, 1, 3, 1, []diffLine{del(3, "a"), add(3, "b")}}}},
		{"new file", "@@ -0,0 +1,2 @@\n+a\n+b",
			[]hunk{{0, 0, 1, 2, []diffLine{add(1, "a"), add(2, "b")}}}},
		{"no newline marker dropped", "@@ -1 +1 @@\n-a\n\\ No newline at end of file\n+a\n\\ No newline at end of file",
			[]hunk{{1, 1, 1, 1, []diffLine{del(1, "a"), add(1, "a")}}}},
		{"empty context line", "@@ -1,3 +1,3 @@\n a\n\n c",
			[]hunk{{1, 3, 1, 3, []diffLine{ctx(1, 1, "a"), ctx(2, 2, ""), ctx(3, 3, "c")}}}},
	}
	for _, tt := range tests {
		got, err := parsePatch(tt.patch)
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: parsePatch = %+v, %v; want %+v", tt.name, got, err, tt.want)
		}
	}
	for _, bad := range []string{
		"",
		"a\n@@ -1 +1 @@\n a",
		"@@ -a +1 @@\n a",
		"@@ -1,2 @@\n a",
		"@@ -1,2 +1,2 @@\n a",                  // too few lines
		"@@ -1 +1 @@\n a\n b",                  // too many lines
		"@@ -1,2 +1,2 @@\n a\n@@ -5 +5 @@\n b", // first hunk short
		"@@ -1 +1 @@\n*a",                      // unknown kind
		"@@ -1 +1 @@\n a\n@@ -99999999999999999999 +1 @@\n a", // overflow
	} {
		if h, err := parsePatch(bad); err == nil {
			t.Errorf("parsePatch(%q) = %+v, want error", bad, h)
		}
	}
}

const sha = "abc123"

func file(name, status, patch string) prFile {
	return prFile{Filename: name, Status: status, Patch: patch, Additions: 1,
		ContentsURL: "https://api.github.com/repos/o/r/contents/" + name + "?ref=" + sha}
}

func TestBuildDiff(t *testing.T) {
	ok := "@@ -1 +1 @@\n-a\n+b"
	renamed := file("new.go", "renamed", "")
	renamed.Additions = 0
	files := []prFile{
		file("a.go", "modified", ok),
		file("bin.png", "added", ""),
		renamed,
		file("big.go", "modified", "@@ -1 +1 @@\n+"+strings.Repeat("x", maxPatchBytes)),
		file("bad.go", "modified", "@@ nope"),
	}
	d, err := buildDiff(files, sha)
	if err != nil {
		t.Fatal(err)
	}
	notes := []string{"", "Diff not available (binary or too large on GitHub)", "Renamed without changes",
		"Diff not available (over 128 KiB)", "Diff not available (could not parse)"}
	for i, f := range d.Files {
		if f.Note != notes[i] || (f.Note == "") != (len(f.Hunks) > 0) || f.Patch != "" {
			t.Errorf("file %s: note %q, %d hunks, patch %q; want note %q", f.Filename, f.Note, len(f.Hunks), f.Patch, notes[i])
		}
	}
	if d.Truncated {
		t.Error("5 files: truncated")
	}

	// 2 MiB total: 17 patches just under 128 KiB fit 16 times; the 17th and every later one are missing.
	patch := "@@ -0,0 +1 @@\n+" + strings.Repeat("x", maxPatchBytes-20)
	files = nil
	for i := range 18 {
		files = append(files, file(fmt.Sprintf("f%d", i), "modified", patch))
	}
	files[17].Patch = ok // small, but after the limit
	d, _ = buildDiff(files, sha)
	for i, f := range d.Files {
		if want := i >= 16; (f.Note == "Diff not available (diff too large)") != want {
			t.Errorf("file %d: note %q, want too large %v", i, f.Note, want)
		}
	}

	files = make([]prFile, 300)
	for i := range files {
		files[i] = file(fmt.Sprint(i), "modified", ok)
	}
	if d, _ := buildDiff(files, sha); !d.Truncated {
		t.Error("300 files: not truncated")
	}

	moved := file("a.go", "modified", ok)
	moved.ContentsURL = strings.Replace(moved.ContentsURL, sha, "other", 1)
	for _, f := range []prFile{moved, {Filename: "x", Patch: ok}} {
		if _, err := buildDiff([]prFile{file("b.go", "modified", ok), f}, sha); !errors.Is(err, errDiffOutdated) {
			t.Errorf("contents_url %q: err %v, want outdated", f.ContentsURL, err)
		}
	}
	removed := file("gone.go", "removed", "@@ -1 +0,0 @@\n-a")
	removed.ContentsURL = strings.Replace(removed.ContentsURL, sha, "base", 1)
	if _, err := buildDiff([]prFile{file("b.go", "modified", ok), removed}, sha); err != nil {
		t.Errorf("removed file at base sha: err %v, want nil", err)
	}
}

func TestFilesPagination(t *testing.T) {
	for _, tt := range []struct{ total, calls, got int }{{0, 1, 0}, {99, 1, 99}, {100, 2, 100}, {250, 3, 250}, {300, 3, 300}, {450, 3, 300}} {
		var args [][]string
		run := func(_ context.Context, a ...string) ([]byte, error) {
			args = append(args, a)
			p := len(args)
			n := min(100, max(0, tt.total-(p-1)*100))
			out, _ := json.Marshal(make([]prFile, n))
			return out, nil
		}
		files, err := filesWith(run)(context.Background(), "o/r", 7)
		if err != nil || len(files) != tt.got || len(args) != tt.calls {
			t.Errorf("%d files: got %d files in %d calls (%v), want %d in %d", tt.total, len(files), len(args), err, tt.got, tt.calls)
		}
		if len(args) > 0 && !reflect.DeepEqual(args[0], []string{"api", "repos/o/r/pulls/7/files?per_page=100&page=1"}) {
			t.Errorf("args %q", args[0])
		}
	}
	fail := func(context.Context, ...string) ([]byte, error) { return nil, errors.New("boom") }
	if _, err := filesWith(fail)(context.Background(), "o/r", 7); err == nil {
		t.Error("gh error not returned")
	}
}

func TestContent(t *testing.T) {
	var gotArgs []string
	out := []byte("package main\n")
	run := func(_ context.Context, a ...string) ([]byte, error) { gotArgs = a; return out, nil }
	got, err := contentWith(run)(context.Background(), "o/r", sha, "dir/a b#.go")
	want := []string{"api", "-H", "Accept: application/vnd.github.raw+json", "repos/o/r/contents/dir/a%20b%23.go?ref=" + sha}
	if err != nil || string(got) != string(out) || !reflect.DeepEqual(gotArgs, want) {
		t.Errorf("content = %q, %v, args %q", got, err, gotArgs)
	}
	for _, bad := range [][]byte{make([]byte, maxContentBytes+1), []byte("a\x00b")} {
		out = bad
		if _, err := contentWith(run)(context.Background(), "o/r", sha, "a"); !errors.Is(err, errNotAvailable) {
			t.Errorf("%d bytes: err %v, want not available", len(bad), err)
		}
	}
	out = []byte(strings.Repeat("x", maxContentBytes))
	if _, err := contentWith(run)(context.Background(), "o/r", sha, "a"); err != nil {
		t.Errorf("exactly 1 MiB: %v", err)
	}
}

// countingFiles is a files fake that counts calls per PR and blocks until release is closed.
type countingFiles struct {
	mu      sync.Mutex
	calls   map[string]int
	release chan struct{}
	err     error
	sha     string
}

func (c *countingFiles) files(_ context.Context, repo string, number int) ([]prFile, error) {
	c.mu.Lock()
	c.calls[fmt.Sprintf("%s#%d", repo, number)]++
	err, s := c.err, c.sha
	c.mu.Unlock()
	<-c.release
	if err != nil {
		return nil, err
	}
	f := file("a.go", "modified", "@@ -1 +1 @@\n-a\n+b")
	f.ContentsURL = "https://x/?ref=" + s
	return []prFile{f}, nil
}

func (c *countingFiles) count(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[key]
}

func TestDiffCache(t *testing.T) {
	ctx := context.Background()
	fake := &countingFiles{calls: map[string]int{}, release: make(chan struct{}), sha: sha}
	c := newDiffCache(fake.files)
	var wg sync.WaitGroup
	var failed atomic.Int32
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d, err := c.get(ctx, "o/r", 1, sha); err != nil || len(d.Files) != 1 {
				failed.Add(1)
			}
		}()
	}
	for fake.count("o/r#1") == 0 {
		// wait until the fetch has started
	}
	close(fake.release)
	wg.Wait()
	if _, err := c.get(ctx, "o/r", 1, sha); err != nil || failed.Load() > 0 {
		t.Fatalf("get failed: %v, %d failures", err, failed.Load())
	}
	if n := fake.count("o/r#1"); n != 1 {
		t.Errorf("concurrent and repeated gets: %d files calls, want 1", n)
	}

	// A new sha refetches; the fake answers for it.
	fake.mu.Lock()
	fake.sha = "def"
	fake.mu.Unlock()
	if _, err := c.get(ctx, "o/r", 1, "def"); err != nil || fake.count("o/r#1") != 2 {
		t.Errorf("new sha: err %v, %d calls, want 2", err, fake.count("o/r#1"))
	}
	// The old sha is no longer cached, and the answer is now outdated for it.
	if _, err := c.get(ctx, "o/r", 1, sha); !errors.Is(err, errDiffOutdated) || fake.count("o/r#1") != 3 {
		t.Errorf("old sha again: err %v, %d calls, want outdated in 3", err, fake.count("o/r#1"))
	}

	// Errors are not cached.
	fake.mu.Lock()
	fake.err = errors.New("gh down")
	fake.mu.Unlock()
	for i := range 2 {
		if _, err := c.get(ctx, "o/r", 2, sha); err == nil || fake.count("o/r#2") != i+1 {
			t.Errorf("error %d: err %v, %d calls", i, err, fake.count("o/r#2"))
		}
	}
	fake.mu.Lock()
	fake.err = nil
	fake.sha = sha
	fake.mu.Unlock()

	// 21 PRs evict the oldest fetch, o/r#1.
	for n := 2; n <= 22; n++ {
		c.get(ctx, "o/r", n, sha)
	}
	c.get(ctx, "o/r", 22, sha)
	if fake.count("o/r#22") != 1 {
		t.Errorf("o/r#22 refetched")
	}
	if len(c.entries) != maxCachedDiffs {
		t.Errorf("%d entries, want %d", len(c.entries), maxCachedDiffs)
	}
	c.get(ctx, "o/r", 1, sha) // evicted (first fetched), so fetched again
	if fake.count("o/r#1") != 4 {
		t.Errorf("o/r#1: %d calls, want 4 after eviction", fake.count("o/r#1"))
	}
	c.get(ctx, "o/r", 4, sha) // refetching o/r#1 evicted o/r#3, the oldest left; #4 is still cached
	if fake.count("o/r#4") != 1 || fake.count("o/r#3") != 1 {
		t.Errorf("o/r#4: %d calls, want 1", fake.count("o/r#4"))
	}

	// A cancelled waiter returns, and the fetch it started still completes for others.
	slow := &countingFiles{calls: map[string]int{}, release: make(chan struct{}), sha: sha}
	c = newDiffCache(slow.files)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.get(cctx, "o/r", 1, sha); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	close(slow.release)
	if _, err := c.get(ctx, "o/r", 1, sha); err != nil || slow.count("o/r#1") != 1 {
		t.Errorf("after cancel: err %v, %d calls, want 1", err, slow.count("o/r#1"))
	}
}

func statusOf(err error) int {
	var se *statusError
	if errors.As(err, &se) {
		return se.code
	}
	return 0
}

// testDiff has a.go with hunks at new lines 3..5 and 10..11 (line 4 is a deletion followed by an add),
// a removed file, a binary file and an added file.
func testDiff(t *testing.T) prDiff {
	d, err := buildDiff([]prFile{
		file("a.go", "modified", "@@ -3,3 +3,3 @@\n c3\n-d4\n+a4\n c5\n@@ -10,1 +10,2 @@\n c10\n+a11"),
		file("gone.go", "removed", "@@ -1,2 +0,0 @@\n-x\n-y"),
		file("bin.png", "modified", ""),
		file("new.go", "added", "@@ -0,0 +1,2 @@\n+a\n+b"),
	}, sha)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPathValidators(t *testing.T) {
	d := testDiff(t)
	for _, tt := range []struct {
		path      string
		find, new int
	}{{"a.go", 0, 0}, {"gone.go", 0, 404}, {"bin.png", 0, 404}, {"new.go", 0, 0}, {"nope.go", 404, 404}, {"A.go", 404, 404}, {"", 404, 404}} {
		_, err := findFile(d, tt.path)
		_, nerr := newSideFile(d, tt.path)
		if statusOf(err) != tt.find || statusOf(nerr) != tt.new {
			t.Errorf("%q: findFile %v, newSideFile %v; want %d, %d", tt.path, err, nerr, tt.find, tt.new)
		}
	}
	if _, err := findFile(d, "nope.go"); err.Error() != "file not in this pull request" {
		t.Errorf("message %q", err)
	}
}

func TestCheckLine(t *testing.T) {
	f, _ := newSideFile(testDiff(t), "a.go")
	for _, tt := range []struct {
		line string
		code int
	}{
		{"3", 0}, {"4", 0}, {"5", 0}, {"10", 0}, {"11", 0},
		{"2", 400}, {"6", 400}, {"9", 400}, {"12", 400},
		{"0", 400}, {"01", 400}, {"+1", 400}, {"1e3", 400}, {"-3", 400}, {" 3", 400}, {"", 400}, {"12345678", 400}, {"3.0", 400},
	} {
		n, err := checkLine(f, tt.line)
		if statusOf(err) != tt.code || (err == nil && fmt.Sprint(n) != tt.line) {
			t.Errorf("line %q: %d, %v; want code %d", tt.line, n, err, tt.code)
		}
	}
	if _, err := checkLine(f, "9"); err.Error() != "line 9 is not in the diff of a.go" {
		t.Errorf("message %q", err)
	}
	if _, err := parseLineNumber("line", "9999999", false); err != nil {
		t.Errorf("7 digits: %v", err)
	}
}

func TestCheckGap(t *testing.T) {
	d := testDiff(t)
	a, _ := newSideFile(d, "a.go")
	added, _ := newSideFile(d, "new.go")
	for _, tt := range []struct {
		f        diffFile
		from, to string
		code     int
	}{
		{a, "1", "2", 0}, {a, "6", "9", 0}, {a, "12", "0", 0},
		// sub-ranges of one gap: the pieces left around an auto-expanded range
		{a, "1", "1", 0}, {a, "2", "2", 0}, {a, "6", "8", 0}, {a, "7", "7", 0}, {a, "12", "20", 0}, {a, "13", "0", 0},
		{a, "5", "9", 400}, {a, "6", "10", 400}, {a, "2", "6", 400}, {a, "3", "4", 400}, {a, "1", "12", 400}, // in or across a hunk
		{a, "1", "0", 400}, {a, "6", "0", 400}, // to 0 on a gap that is not the trailing one
		{a, "8", "7", 400}, {a, "13", "12", 400}, // from > to
		{a, "0", "2", 400}, {a, "01", "2", 400}, {a, "1", "+2", 400}, {a, "6", "00", 400}, {a, "1", "", 400},
		{added, "3", "0", 400}, // an added file has no trailing gap
	} {
		_, _, err := checkGap(tt.f, tt.from, tt.to)
		if statusOf(err) != tt.code {
			t.Errorf("%s %s..%s: %v, want code %d", tt.f.Filename, tt.from, tt.to, err, tt.code)
		}
	}
	// A hunk with no new lines sits after line NewStart: the gaps around it are 1..5 and 6.. .
	f := diffFile{prFile: prFile{Status: "modified"}, Hunks: []hunk{{OldStart: 6, OldCount: 2, NewStart: 5, NewCount: 0}}}
	if got := gaps(f); !reflect.DeepEqual(got, [][2]int{{1, 5}, {6, 0}}) {
		t.Errorf("gaps around a pure deletion: %v", got)
	}
	// Adjacent hunks have no gap between them.
	f.Hunks = []hunk{{NewStart: 1, NewCount: 2}, {NewStart: 3, NewCount: 1}}
	if got := gaps(f); !reflect.DeepEqual(got, [][2]int{{4, 0}}) {
		t.Errorf("gaps of adjacent hunks: %v", got)
	}
}

func TestValidRepoPath(t *testing.T) {
	for p, want := range map[string]bool{
		"a.go": true, "dir/sub/a.go": true, "ünï/código.go": true, "a b.go": true, ".github/ci.yml": true, "a..b": true,
		"": false, "/etc/passwd": false, "a//b": false, "a/": false, "./a": false, "a/./b": false, "../a": false, "a/../b": false, "..": false,
		`a\b`: false, "a\nb": false, "a\x00b": false, "a\x7fb": false, "\xff.go": false,
		strings.Repeat("a", 1024): true, strings.Repeat("a", 1025): false,
	} {
		if got := validRepoPath(p); got != want {
			t.Errorf("validRepoPath(%q) = %v, want %v", p, got, want)
		}
	}
}
