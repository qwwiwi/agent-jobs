package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

const blockingReport = "## Summary\nVerdict: BLOCK\n\n## Findings / Changes\n\n### Blocking\n" +
	"- `a.go:1` — H1: the first blocking problem\n  with a second line\n- `b.go:2` — H2: the second blocking problem\n\n### Non-blocking\nnone\n\n## Evidence\nx\n"

const shipReport = "## Summary\nVerdict: SHIP\n\n## Findings / Changes\n\n### Blocking\nnone\n\n### Non-blocking\nnone\n\n## Evidence\nx\n"

// endReview queues a review job through job_start and ends it by hand with the report.
func endReview(t *testing.T, m *jobs.Manager, id, report string) {
	t.Helper()
	if err := m.Store.Claim(id, "tok", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.JobDir(id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.JobDir(id), "report.md"), []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Finish(id, "tok", store.StatusSucceeded, 1, 0, 1, "", time.Now()); err != nil {
		t.Fatal(err)
	}
}

// job_start takes review and control_review: the parsed blocking IDs and the chained control review
// show in job_status, and a review with nothing to fix is refused with the reason.
func TestJobStartFixFromReview(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	repo := overlapRepo(t)
	var review, ship map[string]any
	call(t, cs, "job_start", startArgs(func(a map[string]any) { a["kind"], a["repo"], a["title"] = "review", repo, "review" }), &review)
	call(t, cs, "job_start", startArgs(func(a map[string]any) { a["kind"], a["repo"], a["title"] = "review", repo, "ship" }), &ship)
	endReview(t, m, review["id"].(string), blockingReport)
	endReview(t, m, ship["id"].(string), shipReport)

	fixArgs := func(id string, mod func(map[string]any)) map[string]any {
		return startArgs(func(a map[string]any) {
			a["kind"], a["repo"], a["review"] = "fix", repo, id
			if mod != nil {
				mod(a)
			}
		})
	}
	var fix, status map[string]any
	call(t, cs, "job_start", fixArgs(review["id"].(string), nil), &fix)
	call(t, cs, "job_status", map[string]any{"id": fix["id"]}, &status)
	ids, _ := status["review_blocking"].([]any)
	if status["review"] != review["id"] || len(ids) != 2 || ids[0] != "H1" || ids[1] != "H2" || status["control_review"] != true {
		t.Fatalf("fix views: %+v", status)
	}
	var off map[string]any
	call(t, cs, "job_start", fixArgs(review["id"].(string), func(a map[string]any) { a["control_review"] = false }), &off)
	if off["control_review"] == true {
		t.Fatalf("control_review false was ignored: %+v", off)
	}
	if msg := callErr(t, cs, "job_start", fixArgs(ship["id"].(string), nil)); !strings.Contains(msg, "nothing to fix") {
		t.Fatalf("SHIP review: %s", msg)
	}
	if msg := callErr(t, cs, "job_start", startArgs(func(a map[string]any) { a["control_review"] = true })); !strings.Contains(msg, "control_review") {
		t.Fatalf("control_review without review: %s", msg)
	}
}

// G05: job_start takes design and loop_override; both show in job_status (design, loop_override) and a
// blank loop_override is refused.
func TestJobStartFixLoopBypass(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	repo := overlapRepo(t)
	var review, design map[string]any
	call(t, cs, "job_start", startArgs(func(a map[string]any) { a["kind"], a["repo"], a["title"] = "review", repo, "review" }), &review)
	call(t, cs, "job_start", startArgs(func(a map[string]any) { a["kind"], a["title"] = "research", "design" }), &design)
	endReview(t, m, review["id"].(string), blockingReport)
	endReview(t, m, design["id"].(string), shipReport)
	fixArgs := func(mod func(map[string]any)) map[string]any {
		return startArgs(func(a map[string]any) {
			a["kind"], a["repo"], a["review"] = "fix", repo, review["id"]
			mod(a)
		})
	}
	var withDesign, withOverride, status map[string]any
	call(t, cs, "job_start", fixArgs(func(a map[string]any) { a["design"] = design["id"] }), &withDesign)
	call(t, cs, "job_status", map[string]any{"id": withDesign["id"]}, &status)
	if status["design"] != design["id"] {
		t.Fatalf("design not in job_status: %+v", status)
	}
	call(t, cs, "job_start", fixArgs(func(a map[string]any) { a["loop_override"] = "owner approved" }), &withOverride)
	call(t, cs, "job_status", map[string]any{"id": withOverride["id"]}, &status)
	if status["loop_override"] != "owner approved" {
		t.Fatalf("loop_override not in job_status: %+v", status)
	}
	if msg := callErr(t, cs, "job_start", fixArgs(func(a map[string]any) { a["loop_override"] = " " })); !strings.Contains(msg, "loop_override") {
		t.Fatalf("blank loop_override: %s", msg)
	}
}
