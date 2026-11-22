package main

import (
	"testing"
	"time"
)

// postQueue returns a queue in dir with PR 1 reviewed at head h1, with findings a.go:3 and b.go (file level).
func postQueue(t *testing.T, dir string) (*reviewQueue, *fakes, *time.Time) {
	q, f, now := clockQueue(t, dir, 1, time.Hour)
	f.findings = []finding{{Severity: "High", File: "a.go", Line: 3, Description: "d"}, {Severity: "Low", File: "b.go", Description: "e"}}
	reviewed(t, q, f, 0, hpr(1, "h1"))
	return q, f, now
}
