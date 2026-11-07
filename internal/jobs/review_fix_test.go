package jobs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// finishedReview queues a review job and ends it by hand with the given report: the fix-from-review
// tests need a finished review of this agent, not a model run.
func finishedReview(t *testing.T, m *Manager, repo, report string, mod func(*StartRequest)) *store.Job {
	t.Helper()
	return endedReview(t, m, repo, report, store.StatusSucceeded, mod)
}

// endedReview is finishedReview with the terminal status of the review job.
func endedReview(t *testing.T, m *Manager, repo, report, status string, mod func(*StartRequest)) *store.Job {
	t.Helper()
	j := enqueue(t, m, "review the importer", func(r *StartRequest) {
		r.Kind, r.Title = "review", "review of the importer"
		if repo != "" {
			r.RepoPath = repo
		}
		if mod != nil {
			mod(r)
		}
	})
	if err := m.Store.Claim(j.ID, "review-token", m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.JobDir(j.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.JobDir(j.ID), "report.md"), []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Finish(j.ID, "review-token", status, 3, 0.5, 60, "", m.Now()); err != nil {
		t.Fatal(err)
	}
	return get(t, m, j.ID)
}

func fixFromReview(repo, review string, mod func(*StartRequest)) StartRequest {
	r := StartRequest{Title: "fix the importer", Kind: "fix", Brief: "FAKE_EDIT_TRACKED close the review findings",
		Model: "sonnet", RepoPath: repo, Review: review}
	if mod != nil {
		mod(&r)
	}
	return r
}

// A fix job started with review refuses every review it cannot build a brief from, each with its own
// reason, before anything is queued.
func TestFixFromReviewRefusals(t *testing.T) {
	m, repo := newManager(t), newRepo(t)
	good := readFixtureFile(t, "review-blocking.md")
	ok := finishedReview(t, m, repo, good, nil)
	ship := finishedReview(t, m, repo, readFixtureFile(t, "review-ship.md"), nil)
	noIDs := finishedReview(t, m, repo, "## Summary\nVerdict: BLOCK\n\n## Findings / Changes\n\n### Blocking\n- `a.go:1` — the first problem\n- `b.go:2` — the second problem\n\n### Non-blocking\nnone\n", nil)
	noSection := finishedReview(t, m, repo, "## Summary\nVerdict: BLOCK\n\n## Findings / Changes\nsomething\n", nil)
	notReview := enqueue(t, m, "a general job", nil)
	queued := enqueue(t, m, "still queued", func(r *StartRequest) { r.Kind = "review" })
	foreign := &store.Job{ID: "other-agent-review", Agent: "someone-else", Title: "foreign", Kind: "review", Brief: "b",
		Status: store.StatusSucceeded, Model: "sonnet", TimeoutMinutes: 1, CreatedAt: m.Now()}
	if err := m.Store.Insert(foreign); err != nil {
		t.Fatal(err)
	}
	failed := endedReview(t, m, repo, good, store.StatusFailed, nil)
	before, err := m.Store.List(100)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		req  StartRequest
		want string
	}{
		"unknown review":       {fixFromReview(repo, "no-such-job", nil), "no-such-job"},
		"another agent's":      {fixFromReview(repo, foreign.ID, nil), "same agent"},
		"not a review job":     {fixFromReview(repo, notReview.ID, nil), "not a review"},
		"review not finished":  {fixFromReview(repo, queued.ID, nil), "not finished"},
		"review failed":        {fixFromReview(repo, failed.ID, nil), "did not succeed"},
		"SHIP nothing to fix":  {fixFromReview(repo, ship.ID, nil), "nothing to fix"},
		"no parsable IDs":      {fixFromReview(repo, noIDs.ID, nil), "no finding ID"},
		"no Blocking section":  {fixFromReview(repo, noSection.ID, nil), "Blocking"},
		"general job":          {fixFromReview(repo, ok.ID, func(r *StartRequest) { r.Kind = "general" }), "fix job"},
		"fix job without repo": {fixFromReview("", ok.ID, nil), "repo"},
		"control without review": {fixFromReview(repo, "", func(r *StartRequest) {
			f := true
			r.ControlReview = &f
		}), "control_review"},
	}
	for name, c := range cases {
		if _, err := m.Enqueue(c.req); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want an error with %q, got %v", name, c.want, err)
		}
	}
	after, err := m.Store.List(100)
	if err != nil || len(after) != len(before) {
		t.Fatalf("a refused start queued a job: %d -> %d (%v)", len(before), len(after), err)
	}
}

// Every blocking ID of the review, with its whole body, is in the fix job's prompt; the review's
// full report is attached once as a job:<id> source; the fix checklist asks for an answer per ID.
func TestFixFromReviewPromptHasEveryFinding(t *testing.T) {
	m, repo := newManager(t), newRepo(t)
	review := finishedReview(t, m, repo, readFixtureFile(t, "review-blocking.md"), nil)
	// The caller already attached the report itself: it must not be copied twice.
	j, err := m.Enqueue(fixFromReview(repo, review.ID, func(r *StartRequest) { r.Sources = []string{"job:" + review.ID} }))
	if err != nil {
		t.Fatal(err)
	}
	j = get(t, m, j.ID)
	if j.Review != review.ID || !j.ControlReview {
		t.Fatalf("review not stored, control review not on by default: %+v", j)
	}
	if n := slices.Index(j.SourceFiles, "job:"+review.ID); len(j.SourceFiles) != 1 || n != 0 {
		t.Fatalf("sources: %v", j.SourceFiles)
	}
	if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), "sources", review.ID+"-report.md")); err != nil {
		t.Fatalf("the full report was not copied: %v", err)
	}
	prompt, err := m.BuildPrompt(j, "", nil, MemoryNotes{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Blocking findings to close", review.ID,
		"H1", "truncates `events` before the new rows are validated", "Reproduced with `go test ./internal/importer -run TestBadFile`",
		"validate first, then truncate inside one transaction", "this fenced line is body text",
		"H2", "advisory lock is taken after the read", "sub-point: the lock file is never removed", "take the lock before reading",
		"closed / not closed", "every ID",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if strings.Contains(prompt, "log message has a typo") {
		t.Error("a non-blocking finding is in the blocking block")
	}
	// A fix without review has neither the block nor the extra checklist.
	plain := enqueue(t, m, "plain fix", func(r *StartRequest) { r.Kind, r.RepoPath = "fix", repo })
	pp, err := m.BuildPrompt(get(t, m, plain.ID), "", nil, MemoryNotes{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(pp, "Blocking findings to close") {
		t.Error("plain fix job has the blocking block")
	}
}

// A successful fix started with review queues one control review: kind review, the model, runner and
// effort of the original review, the fix job's result branch as its base, the original report and the
// fix report as sources. The job-done line names it.
func TestControlReviewQueuedAfterSuccessfulFix(t *testing.T) {
	m, repo := newManager(t), newRepo(t)
	logf := notifyTo(t, m)
	review := finishedReview(t, m, repo, readFixtureFile(t, "review-blocking.md"), func(r *StartRequest) {
		r.Model, r.ReasoningEffort = "opus", "xhigh"
	})
	fix, err := m.Enqueue(fixFromReview(repo, review.ID, nil))
	if err != nil {
		t.Fatal(err)
	}
	fix = runToEnd(t, m, fix.ID)
	if fix.Status != store.StatusSucceeded || fix.Branch != "job/"+fix.ID {
		t.Fatalf("fix did not publish its branch: %+v", fix)
	}
	if fix.ControlJob == "" || fix.ControlNote != "" {
		t.Fatalf("no control review recorded: job %q note %q", fix.ControlJob, fix.ControlNote)
	}
	ctl := get(t, m, fix.ControlJob)
	if ctl.Kind != "review" || ctl.Model != review.Model || ctl.Runner != review.Runner || ctl.ReasoningEffort != "xhigh" ||
		ctl.RepoPath != review.RepoPath {
		t.Fatalf("control review does not match the original review: %+v (review %+v)", ctl, review)
	}
	head := gitT(t, repo, "rev-parse", "refs/heads/"+fix.Branch)
	if ctl.BaseBranch != "refs/heads/"+fix.Branch || ctl.BaseCommit != head {
		t.Fatalf("control review base %s@%s, want %s@%s", ctl.BaseBranch, ctl.BaseCommit, fix.Branch, head)
	}
	if want := []string{"job:" + review.ID, "job:" + fix.ID}; !slices.Equal(ctl.SourceFiles, want) {
		t.Fatalf("sources %v, want %v", ctl.SourceFiles, want)
	}
	if ctl.Review != "" || ctl.ControlReview {
		t.Fatalf("the control review must not chain again: %+v", ctl)
	}
	for _, want := range []string{"H1", "H2", fix.ID, review.ID, "SHIP WITH FIXES", "BLOCK", "regression", fix.BaseCommit} {
		if !strings.Contains(ctl.Brief, want) {
			t.Errorf("control review brief lacks %q:\n%s", want, ctl.Brief)
		}
	}
	line := m.DoneLineFor(fix)
	if !strings.Contains(line, "control review "+ctl.ID+" queued") {
		t.Fatalf("done line does not name the control review: %s", line)
	}
	if b, _ := os.ReadFile(logf); !strings.Contains(string(b), "control review "+ctl.ID+" queued") {
		t.Fatalf("delivered line lacks the control review: %s", b)
	}
	// Another tick queues nothing more.
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	all, err := m.Store.List(100)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, j := range all {
		if j.Kind == "review" && j.ID != review.ID {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d control reviews queued, want 1", n)
	}
}

func TestControlReviewOptOut(t *testing.T) {
	m, repo := newManager(t), newRepo(t)
	review := finishedReview(t, m, repo, readFixtureFile(t, "review-blocking.md"), nil)
	off := false
	fix, err := m.Enqueue(fixFromReview(repo, review.ID, func(r *StartRequest) { r.ControlReview = &off }))
	if err != nil {
		t.Fatal(err)
	}
	fix = runToEnd(t, m, fix.ID)
	if fix.Status != store.StatusSucceeded || fix.ControlJob != "" || fix.ControlNote != "" || fix.ControlReview {
		t.Fatalf("opt-out still chained: %+v", fix)
	}
	if line := m.DoneLineFor(fix); strings.Contains(line, "control review") {
		t.Fatalf("done line mentions a control review: %s", line)
	}
	if reviews, _ := m.Store.ListByStatus(store.StatusQueued); len(reviews) != 0 {
		t.Fatalf("queued after opt-out: %+v", reviews)
	}
}

// A fix that failed or was cancelled queues nothing, and neither does a plain fix without review.
func TestControlReviewNotQueuedForFailedOrCancelledFix(t *testing.T) {
	for _, status := range []string{store.StatusFailed, store.StatusCancelled, store.StatusTimeout} {
		m, repo := newManager(t), newRepo(t)
		review := finishedReview(t, m, repo, readFixtureFile(t, "review-blocking.md"), nil)
		fix, err := m.Enqueue(fixFromReview(repo, review.ID, nil))
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Store.Claim(fix.ID, "fix-token", m.Now()); err != nil {
			t.Fatal(err)
		}
		if err := m.Store.Finish(fix.ID, "fix-token", status, 1, 0, 1, "ended", m.Now()); err != nil {
			t.Fatal(err)
		}
		if err := m.notifyPending(); err != nil {
			t.Fatal(err)
		}
		fix = get(t, m, fix.ID)
		if fix.ControlJob != "" || fix.ControlNote != "" {
			t.Errorf("%s fix chained: %+v", status, fix)
		}
		if queued, _ := m.Store.ListByStatus(store.StatusQueued); len(queued) != 0 {
			t.Errorf("%s fix queued %d jobs", status, len(queued))
		}
	}
}

// The control review counts against the normal caps: a codex review past the daily cap is not
// queued, and the refusal goes into the done line instead of failing the fix.
func TestControlReviewCapRefusalIsInTheDoneLine(t *testing.T) {
	m, repo := newManager(t), newRepo(t)
	review := finishedReview(t, m, repo, readFixtureFile(t, "review-blocking.md"), func(r *StartRequest) {
		r.Model = "gpt-5.5"
	})
	if review.Runner != store.RunnerCodex {
		t.Fatalf("original review runner %q", review.Runner)
	}
	fix, err := m.Enqueue(fixFromReview(repo, review.ID, nil))
	if err != nil {
		t.Fatal(err)
	}
	one := 1 // the original review already used the day's only codex job
	m.Cfg.CodexJobsPerDay = &one
	fix = runToEnd(t, m, fix.ID)
	if fix.Status != store.StatusSucceeded || fix.ControlJob != "" {
		t.Fatalf("fix: %+v", fix)
	}
	if !strings.Contains(fix.ControlNote, "codex daily cap reached") {
		t.Fatalf("refusal not recorded: %q", fix.ControlNote)
	}
	line := m.DoneLineFor(fix)
	if !strings.Contains(line, "control review not queued") || !strings.Contains(line, "codex daily cap reached") {
		t.Fatalf("done line: %s", line)
	}
	// A codex original keeps its runner and model on the control review when the cap allows it.
	two := 2
	m.Cfg.CodexJobsPerDay = &two
	again, err := m.Enqueue(fixFromReview(repo, review.ID, nil))
	if err != nil {
		t.Fatal(err)
	}
	again = runToEnd(t, m, again.ID)
	if again.ControlJob == "" {
		t.Fatalf("not queued under the cap: %+v", again)
	}
	if ctl := get(t, m, again.ControlJob); ctl.Runner != store.RunnerCodex || ctl.Model != "gpt-5.5" {
		t.Fatalf("control review %s/%s", ctl.Runner, ctl.Model)
	}
}

// A fix with no commits publishes no branch: there is nothing to review, and the line says so.
func TestControlReviewNeedsAResultBranch(t *testing.T) {
	m, repo := newManager(t), newRepo(t)
	review := finishedReview(t, m, repo, readFixtureFile(t, "review-blocking.md"), nil)
	fix, err := m.Enqueue(fixFromReview(repo, review.ID, func(r *StartRequest) { r.Brief = "change nothing" }))
	if err != nil {
		t.Fatal(err)
	}
	fix = runToEnd(t, m, fix.ID)
	if fix.Status != store.StatusSucceeded || fix.Branch != "" {
		t.Fatalf("fix: %+v", fix)
	}
	if fix.ControlJob != "" || !strings.Contains(fix.ControlNote, "no result branch") {
		t.Fatalf("control review: job %q note %q", fix.ControlJob, fix.ControlNote)
	}
	if line := m.DoneLineFor(fix); !strings.Contains(line, "control review not queued: no result branch") {
		t.Fatalf("done line: %s", line)
	}
}
