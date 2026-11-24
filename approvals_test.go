package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// approvedInd is a Status column indicator as the dashboard renders it.
func approvedInd(label, symbol string, outdated bool) string {
	class := "ind"
	if outdated {
		class += " outdated"
	}
	return fmt.Sprintf(`<span class="%s" role="img" aria-label="%s" title="%s">%s</span>`, class, label, label, symbol)
}

func TestApprovalsRendering(t *testing.T) {
	const head = "h2"
	at := func(login, state, commit string) review { return review{Login: login, State: state, Commit: commit} }
	ok := func(login string) review { return at(login, "APPROVED", head) }
	old := func(login string) review { return at(login, "APPROVED", "h1") }
	me := func(outdated bool) string {
		if outdated {
			return approvedInd("Approved by me on an older commit", "✅", true)
		}
		return approvedInd("Approved by me", "✅", false)
	}
	changes := approvedInd("Changes requested by me", "⛔", false)
	commented := approvedInd("Commented by me", "💬", false)
	for _, c := range []struct {
		name, login string
		reviews     []review
		cell, text  string
	}{
		{"no reviews", "Me", nil, "", "none"},
		{"only mine", "Me", []review{ok("me")}, me(false), "Approved by me"},
		{"changes requested", "Me", []review{at("me", "CHANGES_REQUESTED", head)}, changes, "Changes requested by me"},
		{"commented", "Me", []review{at("me", "COMMENTED", head)}, commented, "Commented by me"},
		{"only others", "Me", []review{ok("bob"), ok("alice")},
			approvedInd("Approved by alice, bob", "👥", false), "Approved by alice, bob"},
		{"both", "Me", []review{ok("alice"), ok("me")},
			me(false) + approvedInd("Approved by alice", "👥", false), "Approved by me; approved by alice"},
		{"approve then request changes", "Me", []review{ok("me"), at("me", "CHANGES_REQUESTED", head)}, changes, "Changes requested by me"},
		{"request changes then approve", "Me", []review{at("me", "CHANGES_REQUESTED", head), ok("me")}, me(false), "Approved by me"},
		{"approval then commented", "Me", []review{ok("me"), at("me", "COMMENTED", head)}, me(false), "Approved by me"},
		{"dismissed", "Me", []review{ok("alice"), at("alice", "DISMISSED", head), ok("me"), at("me", "DISMISSED", head)}, "", "none"},
		{"dismissed then commented", "Me", []review{ok("me"), at("me", "DISMISSED", head), at("me", "COMMENTED", head)}, "", "none"},
		{"mine outdated", "Me", []review{old("me")}, me(true), "Approved by me on an older commit"},
		{"changes requested outdated", "Me", []review{at("me", "CHANGES_REQUESTED", "h1")},
			approvedInd("Changes requested by me on an older commit", "⛔", true), "Changes requested by me on an older commit"},
		{"commented outdated", "Me", []review{at("me", "COMMENTED", "h1")},
			approvedInd("Commented by me on an older commit", "💬", true), "Commented by me on an older commit"},
		{"others outdated", "Me", []review{old("carol")},
			approvedInd("Approved on an older commit by carol", "👥", true), "Approved on an older commit by carol"},
		{"others' changes requested and comments not shown", "Me", []review{at("bob", "CHANGES_REQUESTED", head), at("carol", "COMMENTED", head), ok("alice")},
			approvedInd("Approved by alice", "👥", false), "Approved by alice"},
		{"changes requested and others", "Me", []review{ok("alice"), at("me", "CHANGES_REQUESTED", head), ok("bob")},
			changes + approvedInd("Approved by alice, bob", "👥", false), "Changes requested by me; approved by alice, bob"},
		{"commented and others outdated", "Me", []review{old("carol"), at("me", "COMMENTED", head)},
			commented + approvedInd("Approved on an older commit by carol", "👥", true), "Commented by me; approved on an older commit by carol"},
		{"others mixed", "Me", []review{ok("alice"), old("carol"), ok("bob"), ok("me")},
			me(false) + approvedInd("Approved by alice, bob; on an older commit by carol", "👥", false),
			"Approved by me; approved by alice, bob; on an older commit by carol"},
		{"unknown login", "", []review{ok("me"), old("carol")},
			approvedInd("Approved by me; on an older commit by carol", "👥", false), "Approved by me; on an older commit by carol"},
		{"unknown login hides my other states", "", []review{at("me", "CHANGES_REQUESTED", head), at("dan", "COMMENTED", head)}, "", "none"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := config{Repositories: []string{"a/b"}, Concurrency: 2}
			pr := pullRequest{Repo: "a/b", Number: 1, HeadSHA: head, Reviews: reviewsOf(c.reviews, head)}
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
			// Draft is empty; the Review column follows the Status one.
			mustContain(t, rec.Body.String(), "<th>Status</th><th>Review</th>",
				`<td></td><td>`+c.cell+`</td><td><span class="badge status-not-queued">`)

			_, body := details(t, cfg, p, q, "/pr/a/b/1")
			mustContain(t, body, "<li>Status <strong>"+c.text+"</strong></li>")
		})
	}
}

func TestStylesheetOutdatedIndicator(t *testing.T) {
	mustContain(t, string(stylesheet), ".ind.outdated {")
}
