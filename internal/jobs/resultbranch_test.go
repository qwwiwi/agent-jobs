package jobs

import (
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

func TestResultBranchDefault(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_EDIT_TRACKED", func(r *StartRequest) { r.RepoPath = repo }).ID)
	head := gitT(t, m.JobDir(j.ID)+"/work", "rev-parse", "HEAD")
	if j.Branch != "job/"+j.ID || !strings.Contains(m.DoneLineFor(j), "branch "+j.Branch) {
		t.Fatalf("branch not recorded/shown: %+v", j)
	}
	if got := gitT(t, repo, "rev-parse", "refs/heads/job/"+j.ID); got != head {
		t.Fatalf("result branch %s, want %s", got, head)
	}
}

func TestResultBranchRequestedNew(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_EDIT_TRACKED", func(r *StartRequest) { r.RepoPath, r.Branch = repo, "feature" }).ID)
	head := gitT(t, m.JobDir(j.ID)+"/work", "rev-parse", "HEAD")
	if j.Branch != "feature" || j.BranchNote != "" {
		t.Fatalf("target: %+v", j)
	}
	if got := gitT(t, repo, "rev-parse", "refs/heads/feature"); got != head {
		t.Fatalf("feature at %s, want %s", got, head)
	}
	if refs := gitT(t, repo, "for-each-ref", "--format=%(refname)", "refs/heads/job/"); refs != "" {
		t.Fatalf("a second ref was created: %s", refs)
	}
}

func TestResultBranchNotifyRecovery(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	j := enqueue(t, m, "FAKE_EDIT_TRACKED", func(r *StartRequest) { r.RepoPath = repo })
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "exit_code", 15*time.Second, func() bool { _, ok := readExitCode(m.JobDir(j.ID)); return ok })
	j = get(t, m, j.ID)
	// Simulate a daemon crash after the terminal row was written but before settled.
	if err := m.Store.Finish(j.ID, j.LaunchToken, store.StatusSucceeded, 1, 0, 1, "", m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.notifyPending(); err != nil {
		t.Fatal(err)
	}
	j = get(t, m, j.ID)
	if j.Branch != "job/"+j.ID {
		t.Fatalf("publication not recovered: %+v", j)
	}
	// A notification retry must not move a target again after a recorded publication.
	if err := m.publishResultBranch(j); err != nil {
		t.Fatal(err)
	}
}

// An existing requested branch is never touched, whether it is an ancestor of the job head (a
// fast-forward would have been possible), diverged, or checked out in a linked worktree: the daemon
// publishes job/<id> and tells the owner to fast-forward it themselves.
func TestResultBranchRequestedExistingIsNotMoved(t *testing.T) {
	for _, shape := range []string{"ancestor", "diverged", "checked-out"} {
		t.Run(shape, func(t *testing.T) {
			m := newManager(t)
			repo := newRepo(t)
			gitT(t, repo, "branch", "feature")
			j := enqueue(t, m, "FAKE_EDIT_TRACKED", func(r *StartRequest) { r.RepoPath, r.Branch = repo, "feature" })
			switch shape {
			case "diverged":
				gitT(t, repo, "checkout", "-q", "feature")
				gitT(t, repo, "commit", "-q", "--allow-empty", "-m", "concurrent work")
				gitT(t, repo, "checkout", "-q", "main")
			case "checked-out":
				gitT(t, repo, "worktree", "add", "-q", t.TempDir()+"/linked", "feature")
			}
			tip := gitT(t, repo, "rev-parse", "feature")
			j = runToEnd(t, m, j.ID)
			head := gitT(t, m.JobDir(j.ID)+"/work", "rev-parse", "HEAD")
			want := "requested branch feature exists; not moved - published job/" + j.ID +
				"; fast-forward it yourself with git merge --ff-only"
			if j.Branch != "job/"+j.ID || j.BranchNote != want {
				t.Fatalf("fallback: branch=%q note=%q", j.Branch, j.BranchNote)
			}
			if got := gitT(t, repo, "rev-parse", "feature"); got != tip {
				t.Fatal("requested branch moved")
			}
			if got := gitT(t, repo, "rev-parse", "refs/heads/job/"+j.ID); got != head {
				t.Fatal("job branch does not hold the job head")
			}
			if line := m.DoneLineFor(j); !strings.Contains(line, "branch job/"+j.ID+" (") || !strings.Contains(line, want) {
				t.Fatalf("done line: %s", line)
			}
		})
	}
}

// Two jobs asking for the same new name: the first creates it, the second finds it existing and
// falls back, leaving the winner's branch alone.
func TestResultBranchSameNameTwoJobs(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	first := runToEnd(t, m, enqueue(t, m, "FAKE_EDIT_TRACKED", func(r *StartRequest) { r.RepoPath, r.Branch = repo, "feature" }).ID)
	tip := gitT(t, repo, "rev-parse", "feature")
	second := runToEnd(t, m, enqueue(t, m, "FAKE_EDIT_TRACKED", func(r *StartRequest) { r.RepoPath, r.Branch = repo, "feature" }).ID)
	if first.Branch != "feature" || second.Branch != "job/"+second.ID || !strings.Contains(second.BranchNote, "exists; not moved") {
		t.Fatalf("first=%q second=%q note=%q", first.Branch, second.Branch, second.BranchNote)
	}
	if got := gitT(t, repo, "rev-parse", "feature"); got != tip {
		t.Fatal("the winner's branch moved")
	}
}

func TestResultBranchStartRejectsInvalidAndProtected(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	for _, name := range []string{"job", "Job", "job/x", "main", "Master", "release-1", "bad name", "@{-1}", "-flag", "a..b", "refs/heads/x"} {
		if _, err := m.Enqueue(StartRequest{Title: "test", Kind: "fix", Brief: "test", Model: "sonnet", RepoPath: repo, Branch: name}); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
}

func TestResultBranchNoCommitsOrReadOnly(t *testing.T) {
	for _, kind := range []string{"general", "review", "research"} {
		t.Run(kind, func(t *testing.T) {
			m := newManager(t)
			repo := newRepo(t)
			brief := "plain"
			if kind != "general" {
				brief = "FAKE_EDIT_TRACKED"
			}
			j := runToEnd(t, m, enqueue(t, m, brief, func(r *StartRequest) { r.RepoPath, r.Kind, r.Branch = repo, kind, "feature" }).ID)
			if j.Branch != "" || strings.Contains(m.DoneLineFor(j), " · branch ") {
				t.Fatalf("unexpected branch: %+v", j)
			}
			if refs := gitT(t, repo, "for-each-ref", "--format=%(refname)", "refs/heads/job/", "refs/heads/feature"); refs != "" {
				t.Fatalf("created branches: %s", refs)
			}
		})
	}
}

func TestResultBranchCodexWrapper(t *testing.T) {
	m := newCodexWriteManager(t, `{"codex_write_allowed":true,"git_name":"Job Bot","git_email":"job@example.com"}`)
	repo := newRepo(t)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_CODEX_WRITE FAKE_CODEX_COMMITMSG", codexFix(repo)).ID)
	if j.Status != store.StatusSucceeded || j.Branch != "job/"+j.ID {
		t.Fatalf("codex result: %+v", j)
	}
	if subject := gitT(t, repo, "log", "-1", "--format=%s", j.Branch); subject != "Add codex-wrote.txt" {
		t.Fatalf("wrapper commit missing: %s", subject)
	}
}

// A job that moves its worktree to a commit that is not a descendant of its base (a sibling or an
// orphan history) still has a positive base..head count; it must not be published (#197).
func TestResultBranchRejectsNonDescendantHead(t *testing.T) {
	for _, shape := range []string{"sibling", "orphan"} {
		t.Run(shape, func(t *testing.T) {
			m := newManager(t)
			repo := newRepo(t)
			gitT(t, repo, "commit", "-q", "--allow-empty", "-m", "second")
			j := runToEnd(t, m, enqueue(t, m, "plain", func(r *StartRequest) { r.RepoPath, r.Branch = repo, "feature" }).ID)
			work := m.JobDir(j.ID) + "/work"
			if shape == "sibling" {
				gitT(t, work, "checkout", "-q", "--detach", "HEAD~1")
			} else {
				gitT(t, work, "checkout", "-q", "--orphan", "unrelated")
			}
			gitT(t, work, "commit", "-q", "--allow-empty", "-m", "elsewhere")
			if err := m.publishResultBranch(get(t, m, j.ID)); err != nil {
				t.Fatal(err)
			}
			j = get(t, m, j.ID)
			if j.Branch != "" || !strings.Contains(j.BranchNote, "not a descendant") {
				t.Fatalf("non-descendant head published: branch=%q note=%q", j.Branch, j.BranchNote)
			}
			if refs := gitT(t, repo, "for-each-ref", "--format=%(refname)", "refs/heads/job/", "refs/heads/feature"); refs != "" {
				t.Fatalf("created branches: %s", refs)
			}
		})
	}
}

// Result branches are create-only, so an existing default branch can never be moved: no knowledge
// of the repository's default is needed, and a custom default is an ordinary name at start (it
// exists, so the job falls back to job/<id> at publication).
func TestResultBranchStartDoesNotNeedDefaultBranch(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	gitT(t, repo, "branch", "-m", "main", "develop")
	gitT(t, repo, "checkout", "-q", "-b", "feature")
	gitT(t, repo, "config", "init.defaultBranch", "") // empty overrides any host-level value
	for _, name := range []string{"develop", "other"} {
		if _, err := m.Enqueue(StartRequest{Title: "test", Kind: "fix", Brief: "test", Model: "sonnet", RepoPath: repo, Branch: name}); err != nil {
			t.Errorf("refused %q: %v", name, err)
		}
	}
}

func TestResultBranchExistingCustomDefaultIsNotMoved(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	gitT(t, repo, "branch", "-m", "main", "develop")
	gitT(t, repo, "checkout", "-q", "-b", "feature")
	tip := gitT(t, repo, "rev-parse", "develop")
	j := runToEnd(t, m, enqueue(t, m, "FAKE_EDIT_TRACKED", func(r *StartRequest) { r.RepoPath, r.Branch = repo, "develop" }).ID)
	if j.Branch != "job/"+j.ID || !strings.Contains(j.BranchNote, "requested branch develop exists; not moved") {
		t.Fatalf("fallback: branch=%q note=%q", j.Branch, j.BranchNote)
	}
	if got := gitT(t, repo, "rev-parse", "develop"); got != tip {
		t.Fatal("default branch moved")
	}
}

// A stopped job is never notified (PendingNotify skips it), so its publication must be recovered
// on its own. Boundary "before": the daemon died before any ref was written. Boundary "after": it
// died after the refs were written but before SetResultBranch recorded them.
func TestResultBranchRecoversStoppedJob(t *testing.T) {
	for _, boundary := range []string{"before-ref-publication", "after-ref-publication"} {
		t.Run(boundary, func(t *testing.T) {
			m := newManager(t)
			repo := newRepo(t)
			j := enqueue(t, m, "FAKE_EDIT_TRACKED", func(r *StartRequest) { r.RepoPath, r.Branch = repo, "feature" })
			if err := m.Tick(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "exit_code", 15*time.Second, func() bool { _, ok := readExitCode(m.JobDir(j.ID)); return ok })
			j = get(t, m, j.ID)
			if err := m.Store.Finish(j.ID, j.LaunchToken, store.StatusStopped, 1, 0, 1, "stopped", m.Now()); err != nil {
				t.Fatal(err)
			}
			head := gitT(t, m.JobDir(j.ID)+"/work", "rev-parse", "HEAD")
			if boundary == "after-ref-publication" {
				gitT(t, repo, "update-ref", "refs/heads/job/"+j.ID, head)
				gitT(t, repo, "update-ref", "refs/heads/feature", head)
			}
			if got := get(t, m, j.ID); got.Status != store.StatusStopped || got.Branch != "" {
				t.Fatalf("setup: %+v", got)
			}
			if err := m.notifyPending(); err != nil {
				t.Fatal(err)
			}
			got := get(t, m, j.ID)
			if got.Status != store.StatusStopped || got.Branch != "feature" {
				t.Fatalf("stopped job's publication not recovered: status=%s branch=%q note=%q", got.Status, got.Branch, got.BranchNote)
			}
			if tip := gitT(t, repo, "rev-parse", "refs/heads/feature"); tip != head {
				t.Fatalf("feature at %s, want %s", tip, head)
			}
		})
	}
}
