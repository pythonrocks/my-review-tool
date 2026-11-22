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
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type entry struct {
	name     string
	typeflag byte
	body     string
	size     int64 // used when body is empty and typeflag is TypeReg; content is zeros
}

// tarball builds a gzipped tarball shaped like GitHub's, with a pax global header first.
func tarball(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	all := append([]entry{{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader}}, entries...)
	for _, e := range all {
		h := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: 0o644, Size: int64(len(e.body))}
		switch e.typeflag {
		case tar.TypeXGlobalHeader:
			h = &tar.Header{Typeflag: e.typeflag, PAXRecords: map[string]string{"comment": "sha"}}
		case tar.TypeSymlink, tar.TypeLink:
			h.Linkname, h.Size = "/etc/passwd", 0
		case tar.TypeReg:
			if e.body == "" {
				h.Size = e.size
			}
		case tar.TypeDir, tar.TypeChar:
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typeflag == tar.TypeReg {
			if e.body != "" {
				io.WriteString(tw, e.body)
			} else {
				io.CopyN(tw, zeros{}, e.size)
			}
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestExtractTarball(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "src")
	tgz := tarball(t,
		entry{name: "o-r-sha/", typeflag: tar.TypeDir},
		entry{name: "o-r-sha/main.go", typeflag: tar.TypeReg, body: "package main"},
		entry{name: "o-r-sha/a/b/c.txt", typeflag: tar.TypeReg, body: "c"}, // no dir entry for a/b
		entry{name: "o-r-sha/link", typeflag: tar.TypeSymlink},
		entry{name: "o-r-sha/hard", typeflag: tar.TypeLink},
		entry{name: "o-r-sha/dev", typeflag: tar.TypeChar},
		entry{name: "o-r-sha/empty/", typeflag: tar.TypeDir},
	)
	if err := extractTarball(bytes.NewReader(tgz), dir, maxCheckout); err != nil {
		t.Fatal(err)
	}
	var got []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		rel, _ := filepath.Rel(dir, p)
		if !d.Type().IsRegular() && !d.IsDir() {
			t.Errorf("%s is not a regular file or directory", rel)
		}
		got = append(got, rel)
		return err
	})
	if want := ". a a/b a/b/c.txt empty main.go"; strings.Join(got, " ") != want {
		t.Errorf("extracted %v, want %s", got, want)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "main.go")); string(b) != "package main" {
		t.Errorf("main.go = %q", b)
	}
}

func TestExtractTarballRejects(t *testing.T) {
	for _, name := range []string{"o-r-sha/../../evil", "../evil", "/etc/evil", "o-r-sha/a/../../../evil"} {
		root := t.TempDir()
		err := extractTarball(bytes.NewReader(tarball(t, entry{name: name, typeflag: tar.TypeReg, body: "x"})), filepath.Join(root, "src"), maxCheckout)
		if err == nil || !strings.Contains(err.Error(), "unsafe path in tarball") {
			t.Errorf("%s: err = %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(root), "evil")); err == nil {
			t.Errorf("%s: written outside the checkout", name)
		}
	}
	if err := extractTarball(strings.NewReader("not gzip"), t.TempDir(), maxCheckout); err == nil || !strings.HasPrefix(err.Error(), "read tarball: ") {
		t.Errorf("garbage: err = %v", err)
	}
}

func TestExtractTarballSizeCap(t *testing.T) {
	const limit, half = 2 << 10, 1 << 10
	at := tarball(t, entry{name: "p/a", typeflag: tar.TypeReg, size: half}, entry{name: "p/b", typeflag: tar.TypeReg, size: half})
	if err := extractTarball(bytes.NewReader(at), t.TempDir(), limit); err != nil {
		t.Fatalf("exactly at the cap: %v", err)
	}
	over := tarball(t, entry{name: "p/a", typeflag: tar.TypeReg, size: half}, entry{name: "p/b", typeflag: tar.TypeReg, size: half + 1})
	if err := extractTarball(bytes.NewReader(over), t.TempDir(), limit); err == nil || err.Error() != "checkout too large: over 500 MiB" {
		t.Fatalf("cap + 1: err = %v", err)
	}
}

func TestPrepare(t *testing.T) {
	tgz := tarball(t, entry{name: "o-r-sha/f.go", typeflag: tar.TypeReg, body: "x"})
	var gotRepo, gotSHA string
	tarFn := func(_ context.Context, repo, sha string, extract func(io.Reader) error) error {
		gotRepo, gotSHA = repo, sha
		return extract(bytes.NewReader(tgz))
	}
	diffOf := func(d string) func(context.Context, string, int) ([]byte, error) {
		return func(context.Context, string, int) ([]byte, error) { return []byte(d), nil }
	}
	p := pullRequest{Repo: "o/r", Number: 3, HeadSHA: "abc"}
	body := diffOf("")

	tmp := t.TempDir()
	ws, err := newPrepare(tarFn, diffOf(""), body)(context.Background(), p, tmp)
	if err != nil || !reflect.DeepEqual(ws, workspace{Dir: filepath.Join(tmp, "src"), Tmp: tmp, Diff: ""}) || gotRepo != "o/r" || gotSHA != "abc" {
		t.Fatalf("empty diff: %+v, %v, %s@%s", ws, err, gotRepo, gotSHA)
	}
	if _, err := os.Stat(filepath.Join(tmp, "src", "f.go")); err != nil {
		t.Fatal(err)
	}

	limit := strings.Repeat("x", maxDiff)
	if ws, err := newPrepare(tarFn, diffOf(limit), body)(context.Background(), p, t.TempDir()); err != nil || ws.Diff != limit {
		t.Fatalf("256 KiB diff: %v", err)
	}
	_, err = newPrepare(tarFn, diffOf(limit+"x"), body)(context.Background(), p, t.TempDir())
	if err == nil || err.Error() != "diff too large for review: 257 KiB, limit 256 KiB" {
		t.Fatalf("256 KiB + 1 diff: %v", err)
	}

	ghErr := errors.New("HTTP 404: Not Found")
	failTar := func(context.Context, string, string, func(io.Reader) error) error { return ghErr }
	if _, err := newPrepare(failTar, diffOf(""), body)(context.Background(), p, t.TempDir()); err != ghErr {
		t.Fatalf("tarball error: %v", err)
	}
	failDiff := func(context.Context, string, int) ([]byte, error) { return nil, ghErr }
	if _, err := newPrepare(tarFn, failDiff, body)(context.Background(), p, t.TempDir()); err != ghErr {
		t.Fatalf("diff error: %v", err)
	}
	if _, err := newPrepare(tarFn, diffOf(""), failDiff)(context.Background(), p, t.TempDir()); err != ghErr {
		t.Fatalf("body error: %v", err)
	}

	var gotBodyPR string
	bodyOf := func(b string) func(context.Context, string, int) ([]byte, error) {
		return func(_ context.Context, repo string, n int) ([]byte, error) {
			gotBodyPR = fmt.Sprintf("%s#%d", repo, n)
			return []byte(b), nil
		}
	}
	for in, want := range map[string]string{
		"Because.\n\nDetails\n":              "Because.\n\nDetails",
		"\n":                                 "",
		"x" + strings.Repeat("é", maxBody/2): "x" + strings.Repeat("é", maxBody/2-1), // cut at 16 KiB, inside the last character
		strings.Repeat("x", maxBody+1):       strings.Repeat("x", maxBody),
	} {
		ws, err := newPrepare(tarFn, diffOf(""), bodyOf(in))(context.Background(), p, t.TempDir())
		if err != nil || ws.Description != want || gotBodyPR != "o/r#3" {
			t.Errorf("body %.20q: description %.20q (%d bytes), %v, %s", in, ws.Description, len(ws.Description), err, gotBodyPR)
		}
	}
}

func TestGhTarball(t *testing.T) {
	good := filepath.Join(t.TempDir(), "good.tgz")
	os.WriteFile(good, tarball(t, entry{name: "o-r-sha/f.go", typeflag: tar.TypeReg, body: "x"}), 0o600)
	bad := filepath.Join(t.TempDir(), "bad.tgz")
	os.WriteFile(bad, tarball(t, entry{name: "../evil", typeflag: tar.TypeReg, body: "x"}), 0o600)
	extractTo := func(dir string) func(io.Reader) error {
		return func(r io.Reader) error { return extractTarball(r, dir, maxCheckout) }
	}

	rec := fakeAgent(t, "gh", "cat "+good)
	dir := t.TempDir()
	if err := ghTarball(context.Background(), "o/r", "abc", extractTo(dir)); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(rec, "args")); got != "api\x00repos/o/r/tarball/abc\x00" {
		t.Errorf("gh args = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "f.go")); err != nil {
		t.Error(err)
	}

	fakeAgent(t, "gh", "echo 'HTTP 404: Not Found' >&2; exit 1")
	if err := ghTarball(context.Background(), "o/r", "abc", extractTo(t.TempDir())); err == nil || err.Error() != "HTTP 404: Not Found" {
		t.Errorf("gh failure: err = %v", err)
	}

	// Real gh prints the HTTP error body on stdout before its message on stderr; gzip fails first.
	fakeAgent(t, "gh", `echo '{"message":"Not Found","status":"404"}'; sleep 0.1; echo 'gh: Not Found (HTTP 404)' >&2; exit 1`)
	if err := ghTarball(context.Background(), "o/r", "abc", extractTo(t.TempDir())); err == nil || err.Error() != "gh: Not Found (HTTP 404)" {
		t.Errorf("gh 404 body on stdout: err = %v", err)
	}

	fakeAgent(t, "gh", "cat "+bad+"; exec sleep 30") // gh still running when extraction fails
	if err := ghTarball(context.Background(), "o/r", "abc", extractTo(t.TempDir())); err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Errorf("unsafe entry: err = %v", err)
	}
}
