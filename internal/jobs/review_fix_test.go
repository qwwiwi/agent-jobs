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

// H3 (#197 fix loop 1): a tag that shares its name with the fix's result branch must not become the
// control review's base. The base is the recorded branch, fully qualified.
func TestControlReviewBaseIgnoresSameNamedTag(t *testing.T) {
	m, repo := newManager(t), newRepo(t)
	review := finishedReview(t, m, repo, readFixtureFile(t, "review-blocking.md"), nil)
	oldCommit := gitT(t, repo, "rev-parse", "HEAD")
	gitT(t, repo, "tag", "fix197", oldCommit) // an older tag with the name of the branch the fix asks for
	fix, err := m.Enqueue(fixFromReview(repo, review.ID, func(r *StartRequest) { r.Branch = "fix197" }))
	if err != nil {
		t.Fatal(err)
	}
	fix = runToEnd(t, m, fix.ID)
	if fix.Status != store.StatusSucceeded || fix.Branch != "fix197" {
		t.Fatalf("fix did not publish fix197: %+v", fix)
	}
	branchHead := gitT(t, repo, "rev-parse", "refs/heads/fix197")
	if branchHead == oldCommit {
		t.Fatal("test setup: the branch did not move past the tag")
	}
	if fix.ControlJob == "" {
		t.Fatalf("no control review: %q", fix.ControlNote)
	}
	ctl := get(t, m, fix.ControlJob)
	if ctl.BaseCommit != branchHead {
		t.Fatalf("control review base is %s (the tag?), want the branch commit %s", ctl.BaseCommit, branchHead)
	}
	if ctl.BaseBranch != "refs/heads/fix197" {
		t.Fatalf("control review base branch %q", ctl.BaseBranch)
	}
}

// succeededFixAwaitingChain is a fix job started from a review, ended by hand as succeeded with a
// published result branch, before any tick: the daemon state in which the control review is due.
func succeededFixAwaitingChain(t *testing.T, m *Manager, repo string) (fix, review *store.Job) {
	t.Helper()
	review = finishedReview(t, m, repo, readFixtureFile(t, "review-blocking.md"), nil)
	f, err := m.Enqueue(fixFromReview(repo, review.ID, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Claim(f.ID, "fix-token", m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.JobDir(f.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.JobDir(f.ID), "report.md"), []byte("## Summary\nfixed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Finish(f.ID, "fix-token", store.StatusSucceeded, 1, 0, 1, "", m.Now()); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "branch", "fix-result")
	if err := m.Store.SetResultBranch(f.ID, "fix-result", ""); err != nil {
		t.Fatal(err)
	}
	return get(t, m, f.ID), review
}

func controlReviewChildren(t *testing.T, m *Manager, original string) []*store.Job {
	t.Helper()
	all, err := m.Store.List(200)
	if err != nil {
		t.Fatal(err)
	}
	var out []*store.Job
	for _, j := range all {
		if j.Kind == "review" && j.ID != original {
			out = append(out, j)
		}
	}
	return out
}

// H2 (#197 fix loop 1): a crash after the control review was queued but before it was linked to the fix
// must not queue a second child on the next tick; the existing child is found by its key and linked.
func TestControlReviewCrashBeforeLinkQueuesNoSecondChild(t *testing.T) {
	m, repo := newManager(t), newRepo(t)
	logf := notifyTo(t, m)
	fix, review := succeededFixAwaitingChain(t, m, repo)
	// The daemon queued the child and died before SetControlReview.
	queued, note := m.controlReviewFor(fix)
	if queued == "" {
		t.Fatalf("setup: control review not queued: %s", note)
	}
	if got := get(t, m, fix.ID); got.ControlJob != "" {
		t.Fatalf("setup: the crash left a link: %+v", got)
	}
	for i := 0; i < 2; i++ { // the restarted daemon, and one more tick
		if err := m.notifyPending(); err != nil {
			t.Fatal(err)
		}
	}
	fix = get(t, m, fix.ID)
	if fix.ControlJob != queued {
		t.Fatalf("fix links control review %q, want the one already queued %q", fix.ControlJob, queued)
	}
	if kids := controlReviewChildren(t, m, review.ID); len(kids) != 1 {
		t.Fatalf("%d control reviews exist, want 1", len(kids))
	}
	if b, _ := os.ReadFile(logf); !strings.Contains(string(b), "control review "+queued+" queued") {
		t.Fatalf("delivered line lacks the control review: %s", b)
	}
}

// H2, second half: when linking the child to the fix fails, the done line is not delivered without the
// child's id. The fix stays pending, the next tick links the same child, and the line names it.
func TestControlReviewRecordErrorKeepsTheIDInTheDoneLine(t *testing.T) {
	m, repo := newManager(t), newRepo(t)
	logf := notifyTo(t, m)
	fix, review := succeededFixAwaitingChain(t, m, repo)
	db := rawDB(t, m)
	mustExec(t, db, `CREATE TRIGGER fail_link BEFORE UPDATE OF control_job ON jobs BEGIN SELECT RAISE(ABORT, 'link boom'); END`)
	if err := m.notifyPending(); err != nil {
		t.Fatal(err)
	}
	if got := get(t, m, fix.ID); got.NotifiedAt != nil {
		t.Fatalf("the done line went out without the control review's id: %+v", got)
	}
	if b, _ := os.ReadFile(logf); strings.Contains(string(b), fix.ID) {
		t.Fatalf("a line for the fix was delivered: %s", b)
	}
	mustExec(t, db, `DROP TRIGGER fail_link`)
	if err := m.notifyPending(); err != nil {
		t.Fatal(err)
	}
	kids := controlReviewChildren(t, m, review.ID)
	if len(kids) != 1 {
		t.Fatalf("%d control reviews exist, want 1", len(kids))
	}
	fix = get(t, m, fix.ID)
	if fix.ControlJob != kids[0].ID || fix.NotifiedAt == nil {
		t.Fatalf("fix not linked and notified after the retry: %+v", fix)
	}
	if b, _ := os.ReadFile(logf); !strings.Contains(string(b), "control review "+kids[0].ID+" queued") {
		t.Fatalf("delivered line lacks the control review: %s", b)
	}
}

// A start that carries a key an existing job already has is a retry: it returns that job, queues nothing.
func TestEnqueueWithTheSameKeyReturnsTheExistingJob(t *testing.T) {
	m := newManager(t)
	req := StartRequest{Title: "keyed", Kind: "general", Brief: "b", Model: "sonnet", IdemKey: "test-key:1"}
	first, err := m.Enqueue(req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := m.Enqueue(req)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID {
		t.Fatalf("a retry queued %s, want the existing %s", again.ID, first.ID)
	}
	if all, _ := m.Store.List(10); len(all) != 1 {
		t.Fatalf("%d jobs, want 1", len(all))
	}
	// Even when the retry's own request would no longer validate, the existing job is the answer.
	req.Model = "no-such-model"
	if got, err := m.Enqueue(req); err != nil || got.ID != first.ID {
		t.Fatalf("retry with a changed request: %v, %v", got, err)
	}
}

// fixWithControl starts a fix job from review, ends it as succeeded and links a finished control
// review (with blocking findings) to it, as the daemon's chain does: the next link of a fix loop.
func fixWithControl(t *testing.T, m *Manager, repo, review string) (fix, control *store.Job) {
	t.Helper()
	f, err := m.Enqueue(fixFromReview(repo, review, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Claim(f.ID, "fix-token", m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Finish(f.ID, "fix-token", store.StatusSucceeded, 1, 0, 1, "", m.Now()); err != nil {
		t.Fatal(err)
	}
	control = finishedReview(t, m, repo, readFixtureFile(t, "review-blocking.md"), nil)
	if err := m.Store.SetControlReview(f.ID, control.ID, ""); err != nil {
		t.Fatal(err)
	}
	return get(t, m, f.ID), control
}

// fixLoopChain builds review -> fix -> control review -> fix -> control review: a chain that holds
// two fix jobs. It returns the ids in chain order.
func fixLoopChain(t *testing.T, m *Manager, repo string) []string {
	t.Helper()
	r0 := finishedReview(t, m, repo, readFixtureFile(t, "review-blocking.md"), nil)
	f1, c1 := fixWithControl(t, m, repo, r0.ID)
	f2, c2 := fixWithControl(t, m, repo, c1.ID)
	return []string{r0.ID, f1.ID, c1.ID, f2.ID, c2.ID}
}

// endedJob queues a job of this agent and ends it by hand with the given status.
func endedJob(t *testing.T, m *Manager, kind, status string) *store.Job {
	t.Helper()
	j := enqueue(t, m, "design the loop", func(r *StartRequest) { r.Kind = kind })
	if err := m.Store.Claim(j.ID, "tok", m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Finish(j.ID, "tok", status, 1, 0, 1, "", m.Now()); err != nil {
		t.Fatal(err)
	}
	return get(t, m, j.ID)
}

// G04: with review_max_fix_loops at its default 2, a third fix on one chain is refused, and the error
// names the chain (job ids in order); nothing is queued.
func TestFixLoopCapRefusesThirdFix(t *testing.T) {
	m, repo := newManager(t), newRepo(t)
	chain := fixLoopChain(t, m, repo)
	before, err := m.Store.List(200)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Enqueue(fixFromReview(repo, chain[len(chain)-1], nil))
	if err == nil {
		t.Fatal("a third fix on the chain was queued")
	}
	for _, want := range []string{strings.Join(chain, " -> "), "review_max_fix_loops", "2 fix"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q: %v", want, err)
		}
	}
	after, err := m.Store.List(200)
	if err != nil || len(after) != len(before) {
		t.Fatalf("a refused start queued a job: %d -> %d (%v)", len(before), len(after), err)
	}
}

// G04: the cap is the config key review_max_fix_loops; at 1 the second fix is already refused, and a
// first fix from a fresh review is always allowed.
func TestFixLoopCapFromConfig(t *testing.T) {
	m, repo := newManager(t), newRepo(t)
	m.Cfg.ReviewMaxFixLoops = 1
	r0 := finishedReview(t, m, repo, readFixtureFile(t, "review-blocking.md"), nil)
	f1, c1 := fixWithControl(t, m, repo, r0.ID)
	_, err := m.Enqueue(fixFromReview(repo, c1.ID, nil))
	if err == nil || !strings.Contains(err.Error(), strings.Join([]string{r0.ID, f1.ID, c1.ID}, " -> ")) {
		t.Fatalf("second fix with review_max_fix_loops 1: %v", err)
	}
}

// G05: design=<succeeded research or general job of this agent> bypasses the cap; it is stored on the
// job and shown in the job-done line.
func TestFixLoopCapDesignAccepted(t *testing.T) {
	m, repo := newManager(t), newRepo(t)
	chain := fixLoopChain(t, m, repo)
	for _, kind := range []string{"research", "general"} {
		design := endedJob(t, m, kind, store.StatusSucceeded)
		j, err := m.Enqueue(fixFromReview(repo, chain[len(chain)-1], func(r *StartRequest) { r.Design = design.ID }))
		if err != nil {
			t.Fatalf("design %s job: %v", kind, err)
		}
		j = get(t, m, j.ID)
		if j.LoopDesign != design.ID || j.LoopOverride != "" {
			t.Fatalf("design not stored: %q %q", j.LoopDesign, j.LoopOverride)
		}
		if line := m.DoneLineFor(j); !strings.Contains(line, "fix loop: design "+design.ID) {
			t.Fatalf("done line lacks the design: %s", line)
		}
	}
}

// G05: loop_override="<reason>" bypasses the cap; the reason is stored and shown in the job-done line.
func TestFixLoopCapOverrideAccepted(t *testing.T) {
	m, repo := newManager(t), newRepo(t)
	chain := fixLoopChain(t, m, repo)
	reason := "owner asked for a third loop on the lock race"
	j, err := m.Enqueue(fixFromReview(repo, chain[len(chain)-1], func(r *StartRequest) { r.LoopOverride = &reason }))
	if err != nil {
		t.Fatal(err)
	}
	j = get(t, m, j.ID)
	if j.LoopOverride != reason || j.LoopDesign != "" {
		t.Fatalf("override not stored: %q %q", j.LoopOverride, j.LoopDesign)
	}
	if line := m.DoneLineFor(j); !strings.Contains(line, "fix loop: override: "+reason) {
		t.Fatalf("done line lacks the override: %s", line)
	}
}

// G05: an empty or blank loop_override is refused, also below the cap; design and loop_override need
// review.
func TestFixLoopBypassRefusals(t *testing.T) {
	m, repo := newManager(t), newRepo(t)
	chain := fixLoopChain(t, m, repo)
	last := chain[len(chain)-1]
	fresh := finishedReview(t, m, repo, readFixtureFile(t, "review-blocking.md"), nil)
	designOK := endedJob(t, m, "research", store.StatusSucceeded)
	failed := endedJob(t, m, "research", store.StatusFailed)
	running := enqueue(t, m, "still queued", func(r *StartRequest) { r.Kind = "general" })
	fixJob := get(t, m, chain[1])
	foreign := &store.Job{ID: "foreign-design", Agent: "someone-else", Title: "foreign", Kind: "research", Brief: "b",
		Status: store.StatusSucceeded, Model: "sonnet", TimeoutMinutes: 1, CreatedAt: m.Now()}
	if err := m.Store.Insert(foreign); err != nil {
		t.Fatal(err)
	}
	empty, blank := "", "  \n "
	before, err := m.Store.List(200)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		req  StartRequest
		want string
	}{
		"empty override":        {fixFromReview(repo, last, func(r *StartRequest) { r.LoopOverride = &empty }), "loop_override"},
		"blank override":        {fixFromReview(repo, last, func(r *StartRequest) { r.LoopOverride = &blank }), "loop_override"},
		"empty override, fresh": {fixFromReview(repo, fresh.ID, func(r *StartRequest) { r.LoopOverride = &empty }), "loop_override"},
		"unknown design":        {fixFromReview(repo, last, func(r *StartRequest) { r.Design = "no-such-job" }), "no-such-job"},
		"failed design":         {fixFromReview(repo, last, func(r *StartRequest) { r.Design = failed.ID }), "did not succeed"},
		"queued design":         {fixFromReview(repo, last, func(r *StartRequest) { r.Design = running.ID }), "did not succeed"},
		"foreign design":        {fixFromReview(repo, last, func(r *StartRequest) { r.Design = foreign.ID }), "same agent"},
		"review as design":      {fixFromReview(repo, last, func(r *StartRequest) { r.Design = fresh.ID }), "research or general"},
		"fix as design":         {fixFromReview(repo, last, func(r *StartRequest) { r.Design = fixJob.ID }), "research or general"},
		"design without review": {fixFromReview(repo, "", func(r *StartRequest) { r.Design = designOK.ID }), "review"},
		"override without review": {fixFromReview(repo, "", func(r *StartRequest) {
			s := "reason"
			r.LoopOverride = &s
		}), "review"},
	}
	for name, c := range cases {
		if _, err := m.Enqueue(c.req); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want an error with %q, got %v", name, c.want, err)
		}
	}
	after, err := m.Store.List(200)
	if err != nil || len(after) != len(before) {
		t.Fatalf("a refused start queued a job: %d -> %d (%v)", len(before), len(after), err)
	}
}

// G06: the control-review brief and kind-review.md carry the stop rule: a finding is closed when its
// original scenario no longer reproduces; a deeper scenario is Non-blocking unless it shows
// reproducible money or data loss with steps.
func TestControlReviewStopRule(t *testing.T) {
	fix := &store.Job{ID: "fix-1", Title: "fix", Branch: "job/fix-1", BaseCommit: "abc123",
		ReviewBlocking: []store.BlockingFinding{{ID: "H1", Body: "H1: data loss"}}}
	rev := &store.Job{ID: "rev-1"}
	checklist, err := template("prompts/kind-review.md")
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"control review brief": controlReviewBrief(fix, rev), "kind-review.md": checklist} {
		flat := strings.Join(strings.Fields(text), " ")
		for _, want := range []string{
			"closed when its original scenario no longer reproduces",
			"deeper scenario",
			"Non-blocking unless it shows reproducible money or data loss, with steps",
		} {
			if !strings.Contains(flat, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
		if strings.Contains(flat, "can still be defeated another way stays") {
			t.Errorf("%s still holds the old rule that keeps a finding open on any other way to defeat it", name)
		}
	}
}

// G04: review_max_fix_loops defaults to 2, takes 1 to 10, and anything outside 0 to 10 is a config error.
func TestReviewMaxFixLoopsConfig(t *testing.T) {
	c, err := loadConfigBody(t, `{}`)
	if err != nil || c.reviewMaxFixLoops() != DefaultReviewMaxFixLoops || DefaultReviewMaxFixLoops != 2 {
		t.Fatalf("default: %d %v", c.reviewMaxFixLoops(), err)
	}
	if c, err = loadConfigBody(t, `{"review_max_fix_loops": 3}`); err != nil || c.reviewMaxFixLoops() != 3 {
		t.Fatalf("set: %d %v", c.reviewMaxFixLoops(), err)
	}
	for _, body := range []string{`{"review_max_fix_loops": -1}`, `{"review_max_fix_loops": 11}`} {
		if _, err := loadConfigBody(t, body); err == nil || !strings.Contains(err.Error(), "review_max_fix_loops") {
			t.Fatalf("%s: %v; want a review_max_fix_loops error", body, err)
		}
	}
}
