package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParsePRs(t *testing.T) {
	data := []byte(`[
		{"number":7,"title":"Add <b>","url":"https://github.com/a/b/pull/7","author":{"login":"alice"},"createdAt":"2026-11-01T10:00:00Z","isDraft":true,"headRefOid":"abc123","labels":[{"name":"dependencies","color":"0366d6"},{"name":"go"}],
		 "reviews":[
			{"id":"PRR_1","author":{"login":"bob"},"authorAssociation":"MEMBER","body":"","submittedAt":"2026-11-01T11:00:00Z","includesCreatedEdit":false,"reactionGroups":[],"state":"APPROVED","commit":{"oid":"old999"}},
			{"id":"PRR_2","author":{"login":"Carol"},"authorAssociation":"MEMBER","body":"nit","submittedAt":"2026-11-01T12:00:00Z","includesCreatedEdit":false,"reactionGroups":[],"state":"CHANGES_REQUESTED","commit":{"oid":"abc123"}},
			{"id":"PRR_3","author":{"login":"carol"},"authorAssociation":"MEMBER","body":"","submittedAt":"2026-11-01T13:00:00Z","includesCreatedEdit":false,"reactionGroups":[],"state":"APPROVED","commit":{"oid":"abc123"}},
			{"id":"PRR_4","author":null,"authorAssociation":"NONE","body":"lgtm","submittedAt":"2026-11-01T14:00:00Z","includesCreatedEdit":false,"reactionGroups":[],"state":"COMMENTED","commit":{"oid":"abc123"}}
		 ]},
		{"number":8,"title":"Fix","url":"https://github.com/a/b/pull/8","author":{"login":""},"createdAt":"2026-11-02T10:00:00Z","isDraft":false}
	]`)
	prs, err := parsePRs("a/b", data)
	if err != nil {
		t.Fatal(err)
	}
	want := []pullRequest{
		{Repo: "a/b", Title: "Add <b>", URL: "https://github.com/a/b/pull/7", Author: "alice", Labels: []string{"dependencies", "go"}, Number: 7, Draft: true, HeadSHA: "abc123", Created: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
			Approvals: []approval{{Login: "bob", Outdated: true}, {Login: "carol"}}},
		{Repo: "a/b", Title: "Fix", URL: "https://github.com/a/b/pull/8", Author: "ghost", Number: 8, Created: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)},
	}
	if fmt.Sprint(prs) != fmt.Sprint(want) {
		t.Errorf("parsePRs = %+v\nwant %+v", prs, want)
	}
	if prs[1].Labels != nil {
		t.Errorf("labels absent: got %q, want none", prs[1].Labels)
	}
	if prs, err := parsePRs("a/b", []byte("[]")); err != nil || prs == nil || len(prs) != 0 {
		t.Errorf("parsePRs([]) = %v, %v; want empty list", prs, err)
	}
	if _, err := parsePRs("a/b", []byte("not json")); err == nil {
		t.Error("parsePRs(invalid): want error")
	}
}

func TestApprovalsOf(t *testing.T) {
	const head = "h2"
	ap := func(login, commit string) review { return review{login, "APPROVED", commit} }
	for _, tc := range []struct {
		name    string
		reviews []review
		head    string
		want    []approval
	}{
		{"no reviews", nil, head, nil},
		{"only mine", []review{ap("me", head)}, head, []approval{{Login: "me"}}},
		{"only others", []review{ap("bob", head), ap("alice", head)}, head, []approval{{Login: "alice"}, {Login: "bob"}}},
		{"mine and others", []review{ap("me", head), ap("bob", head)}, head, []approval{{Login: "bob"}, {Login: "me"}}},
		{"approval then changes requested", []review{ap("me", head), {"me", "CHANGES_REQUESTED", head}}, head, nil},
		{"approval then commented", []review{ap("me", head), {"me", "COMMENTED", head}, {"me", "PENDING", ""}}, head, []approval{{Login: "me"}}},
		{"dismissed approval", []review{ap("me", head), {"me", "DISMISSED", head}}, head, nil},
		{"changes requested then approval", []review{{"me", "CHANGES_REQUESTED", "h1"}, ap("me", head)}, head, []approval{{Login: "me"}}},
		{"outdated", []review{ap("me", "h1")}, head, []approval{{Login: "me", Outdated: true}}},
		{"mixed current and outdated", []review{ap("carol", "h1"), ap("alice", head), ap("bob", head)}, head,
			[]approval{{Login: "alice"}, {Login: "bob"}, {Login: "carol", Outdated: true}}},
		{"reapproved at head", []review{ap("me", "h1"), ap("me", head)}, head, []approval{{Login: "me"}}},
		{"case-insensitive login", []review{ap("Me", head), {"me", "CHANGES_REQUESTED", head}, ap("bob", "h1"), ap("BOB", head)}, head, []approval{{Login: "BOB"}}},
		{"ghost author", []review{ap("", head)}, head, []approval{{Login: "ghost"}}},
		{"empty head", []review{ap("me", "h1")}, "", []approval{{Login: "me"}}},
		{"sort order", []review{ap("zed", head), ap("Bob", head), ap("alice", head), ap("carl", head)}, head,
			[]approval{{Login: "alice"}, {Login: "Bob"}, {Login: "carl"}, {Login: "zed"}}},
	} {
		if got := approvalsOf(tc.reviews, tc.head); !slices.Equal(got, tc.want) {
			t.Errorf("%s: approvalsOf = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestClassifyGhError(t *testing.T) {
	long := strings.Repeat("é", 350)
	for _, tc := range []struct {
		name     string
		err      error
		code     int
		stderr   string
		want     string
		isGlobal bool
	}{
		{"not found", &exec.Error{Name: "gh", Err: exec.ErrNotFound}, 0, "", msgGhNotFound, true},
		{"not logged in", errors.New("exit status 4"), 4, "To get started run gh auth login", msgGhNotLoggedIn, true},
		{"stderr", errors.New("exit status 1"), 1, "  GraphQL: Could not resolve to a Repository\n", "GraphQL: Could not resolve to a Repository", false},
		{"empty stderr", errors.New("gh timed out"), -1, " \n", "gh timed out", false},
		{"truncated", errors.New("exit status 1"), 1, long, long[:600], false},
	} {
		err := classifyGhError(tc.err, tc.code, tc.stderr)
		var g globalError
		if err.Error() != tc.want || errors.As(err, &g) != tc.isGlobal {
			t.Errorf("%s: got %q (global %v), want %q (global %v)", tc.name, err, errors.As(err, &g), tc.want, tc.isGlobal)
		}
	}
}

func TestParseNumbers(t *testing.T) {
	if set, err := parseNumbers([]byte(`[{"number":7}]`)); err != nil || len(set) != 1 || !set[7] {
		t.Errorf("parseNumbers([{7}]) = %v, %v; want {7}", set, err)
	}
	if set, err := parseNumbers([]byte(`[]`)); err != nil || len(set) != 0 {
		t.Errorf("parseNumbers([]) = %v, %v; want empty set", set, err)
	}
	if _, err := parseNumbers([]byte("not json")); err == nil {
		t.Error("parseNumbers(invalid): want error")
	}
}

// A direct request and a team request look the same here: GitHub's review-requested:@me search resolves both.
func TestRequestedDirectOrTeam(t *testing.T) {
	prs := []pullRequest{{Number: 7}, {Number: 8}}
	markRequested(prs, map[int]bool{7: true})
	if !prs[0].Requested {
		t.Error("#7 is in the review-requested result (direct or team): want Requested")
	}
	if prs[1].Requested {
		t.Error("#8 is not in the review-requested result: want not Requested")
	}
}

func TestPRListArgs(t *testing.T) {
	want := []string{"pr", "list", "--repo", "github.com/o/r", "--state", "open", "--limit", "10000",
		"--json", "number,title,url,author,createdAt,isDraft,headRefOid,labels,reviews"}
	if got := prListArgs("o/r"); !slices.Equal(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestReviewsArgs(t *testing.T) {
	want := []string{"api", "--paginate", "repos/o/r/pulls/7/reviews", "--jq", ".[] | [.user.login, .state, .commit_id] | @tsv"}
	if got := reviewsArgs("o/r", 7); !slices.Equal(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestApprovedAt(t *testing.T) {
	for _, c := range []struct {
		name, tsv string
		want      bool
	}{
		{"other user", "bob\tAPPROVED\tabc\n", false},
		{"old sha", "me\tAPPROVED\told\n", false},
		{"dismissed", "me\tAPPROVED\tabc\nme\tDISMISSED\tabc\n", false},
		{"changes requested", "me\tAPPROVED\tabc\nme\tCHANGES_REQUESTED\tabc\n", false},
		{"then commented", "me\tAPPROVED\tabc\nme\tCOMMENTED\tabc\nbob\tCHANGES_REQUESTED\tabc\n", true},
		{"case-insensitive", "Me\tAPPROVED\tabc\n", true},
		{"empty", "", false},
	} {
		if got := approvedAt(c.tsv, "me", "abc"); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestGhApprovedEmptyLogin(t *testing.T) {
	if _, err := ghApproved(context.Background(), "o/r", 1, "", "abc"); err == nil || err.Error() != "gh login not known yet" {
		t.Errorf("err = %v", err)
	}
}

func TestParseThreads(t *testing.T) {
	page1 := `{"data":{"repository":{"pullRequest":{"headRefOid":"h1","reviewThreads":{"pageInfo":{"hasNextPage":true,"endCursor":"c1"},"nodes":[
		{"id":"T1","isResolved":false,"isOutdated":false,"path":"a.go","line":7,"diffSide":"RIGHT","subjectType":"LINE","viewerCanReply":true,"viewerCanResolve":true,"viewerCanUnresolve":false,
		 "comments":{"totalCount":103,"nodes":[{"author":{"login":"bob"},"body":"<script>x</script>","createdAt":"2026-11-01T10:00:00Z","url":"u1"},{"author":null,"body":"gone","createdAt":"2026-11-01T11:00:00Z","url":"u2"}]}}]}}}}}`
	page2 := `{"data":{"repository":{"pullRequest":{"headRefOid":"h1","reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":"c2"},"nodes":[
		{"id":"T2","isResolved":true,"isOutdated":true,"path":"b.go","line":null,"diffSide":"LEFT","subjectType":"LINE","viewerCanReply":false,"viewerCanResolve":false,"viewerCanUnresolve":true,
		 "comments":{"totalCount":1,"nodes":[{"author":{"login":"carol"},"body":"old","createdAt":"2026-11-02T10:00:00Z","url":"u3"}]}},
		{"id":"T3","isResolved":false,"isOutdated":false,"path":"c.go","line":null,"diffSide":"RIGHT","subjectType":"FILE","viewerCanReply":true,"viewerCanResolve":true,"viewerCanUnresolve":false,
		 "comments":{"totalCount":0,"nodes":[]}}]}}}}}`
	got, err := parseThreads([]byte(page1 + "\n" + page2 + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	at := func(h int) time.Time { return time.Date(2026, 11, 1, h, 0, 0, 0, time.UTC) }
	want := threadList{Head: "h1", Threads: []reviewThread{
		{ID: "T1", Path: "a.go", Side: "RIGHT", Line: 7, CanReply: true, CanResolve: true, More: 101,
			Comments: []threadComment{{"bob", "<script>x</script>", "u1", at(10)}, {"ghost", "gone", "u2", at(11)}}},
		{ID: "T2", Path: "b.go", Side: "LEFT", Resolved: true, Outdated: true, CanUnresolve: true,
			Comments: []threadComment{{"carol", "old", "u3", time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)}}},
		{ID: "T3", Path: "c.go", Side: "RIGHT", CanReply: true, CanResolve: true},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseThreads = %+v\nwant %+v", got, want)
	}
	for _, bad := range []string{"not json", page1 + "{"} {
		if _, err := parseThreads([]byte(bad)); err == nil {
			t.Errorf("parseThreads(%.20q): want error", bad)
		}
	}
	if tl, err := parseThreads(nil); err != nil || !reflect.DeepEqual(tl, threadList{}) {
		t.Errorf("parseThreads(empty) = %+v, %v", tl, err)
	}
}

func TestResolveWith(t *testing.T) {
	var args []string
	out, outErr := "true\n", error(nil)
	resolve := resolveWith(func(_ context.Context, a ...string) ([]byte, error) {
		args = a
		return []byte(out), outErr
	})
	got, err := resolve(context.Background(), "T_1", true)
	want := []string{"api", "graphql", "-f", "query=mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}",
		"-f", "id=T_1", "--jq", ".data.resolveReviewThread.thread.isResolved"}
	if err != nil || !got || !slices.Equal(args, want) {
		t.Errorf("resolve: %v %v, args %q", got, err, args)
	}
	out = "false\n"
	got, err = resolve(context.Background(), "T_1", false)
	if err != nil || got || args[3] != "query=mutation($id:ID!){unresolveReviewThread(input:{threadId:$id}){thread{isResolved}}}" ||
		args[7] != ".data.unresolveReviewThread.thread.isResolved" {
		t.Errorf("unresolve: %v %v, args %q", got, err, args)
	}
	out = ""
	if _, err := resolve(context.Background(), "T_1", true); err == nil {
		t.Error("empty answer: want error")
	}
	outErr = errors.New("GraphQL: no")
	if _, err := resolve(context.Background(), "T_1", true); err != outErr {
		t.Errorf("gh error: got %v", err)
	}
}
