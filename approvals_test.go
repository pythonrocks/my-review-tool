package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// approvedInd is an Approved column indicator as the dashboard renders it.
func approvedInd(label, symbol string, outdated bool) string {
	class := "ind"
	if outdated {
		class += " outdated"
	}
	return fmt.Sprintf(`<span class="%s" role="img" aria-label="%s" title="%s">%s</span>`, class, label, label, symbol)
}

func TestApprovalsRendering(t *testing.T) {
	const head = "h2"
	ok := func(login string) review { return review{Login: login, State: "APPROVED", Commit: head} }
	old := func(login string) review { return review{Login: login, State: "APPROVED", Commit: "h1"} }
	me := func(outdated bool) string {
		if outdated {
			return approvedInd("Approved by me on an older commit", "✅", true)
		}
		return approvedInd("Approved by me", "✅", false)
	}
	for _, c := range []struct {
		name, login string
		reviews     []review
		cell, text  string
	}{
		{"no reviews", "Me", nil, "", "none"},
		{"only mine", "Me", []review{ok("me")}, me(false), "Approved by me"},
		{"only others", "Me", []review{ok("bob"), ok("alice")},
			approvedInd("Approved by alice, bob", "👥", false), "Approved by alice, bob"},
		{"both", "Me", []review{ok("alice"), ok("me")},
			me(false) + approvedInd("Approved by alice", "👥", false), "Approved by me; approved by alice"},
		{"mine then changes requested", "Me", []review{ok("me"), {Login: "me", State: "CHANGES_REQUESTED", Commit: head}}, "", "none"},
		{"approval then commented", "Me", []review{ok("me"), {Login: "me", State: "COMMENTED", Commit: head}}, me(false), "Approved by me"},
		{"dismissed", "Me", []review{ok("alice"), {Login: "alice", State: "DISMISSED", Commit: head}}, "", "none"},
		{"mine outdated", "Me", []review{old("me")}, me(true), "Approved by me on an older commit"},
		{"others outdated", "Me", []review{old("carol")},
			approvedInd("Approved on an older commit by carol", "👥", true), "Approved on an older commit by carol"},
		{"others mixed", "Me", []review{ok("alice"), old("carol"), ok("bob"), ok("me")},
			me(false) + approvedInd("Approved by alice, bob; on an older commit by carol", "👥", false),
			"Approved by me; approved by alice, bob; on an older commit by carol"},
		{"unknown login", "", []review{ok("me"), old("carol")},
			approvedInd("Approved by me; on an older commit by carol", "👥", false), "Approved by me; on an older commit by carol"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := config{Repositories: []string{"a/b"}, Concurrency: 2}
			pr := pullRequest{Repo: "a/b", Number: 1, HeadSHA: head, Approvals: approvalsOf(c.reviews, head)}
			login := func(context.Context) (string, error) { return c.login, nil }
			p := newPoller(cfg.Repositories, (&fakeFetch{prs: map[string][]pullRequest{"a/b": {pr}}}).fetch, login)
			p.poll(context.Background())
			q := newReviewQueue(0, workflow{}, resultStore{dir: t.TempDir()})

			// Loading / must use the poll data only: any approval check fails the test.
			failApproved := func(context.Context, string, int, string, string) (bool, error) {
				t.Error("GET / called approved")
				return false, nil
			}
			rec := httptest.NewRecorder()
			newHandler(newConfigHolder(cfg, "myreview.json"), p, q, nil, failApproved, noFiles, noContent, noThreads, noResolve).ServeHTTP(rec, localReq(http.MethodGet, "/", ""))
			// Draft is empty; the Review column follows the Approved one.
			mustContain(t, rec.Body.String(), "<th>Approved</th><th>Review</th>",
				`<td></td><td>`+c.cell+`</td><td><span class="badge status-not-queued">`)

			_, body := details(t, cfg, p, q, "/pr/a/b/1")
			mustContain(t, body, "<li>Approvals <strong>"+c.text+"</strong></li>")
		})
	}
}

func TestStylesheetOutdatedIndicator(t *testing.T) {
	mustContain(t, string(stylesheet), ".ind.outdated {")
}
