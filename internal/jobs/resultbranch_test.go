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

func TestResultBranchRequestedNewAndAncestorTarget(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "ancestor-of-base"}[exists], func(t *testing.T) {
			m := newManager(t)
			repo := newRepo(t)
			if exists {
				gitT(t, repo, "branch", "feature")
				gitT(t, repo, "commit", "-q", "--allow-empty", "-m", "advance base")
			}
			j := runToEnd(t, m, enqueue(t, m, "FAKE_EDIT_TRACKED", func(r *StartRequest) { r.RepoPath, r.Branch = repo, "feature" }).ID)
			if j.Branch != "feature" || (exists && j.BranchNote != "fast-forwarded") || (!exists && j.BranchNote != "") {
				t.Fatalf("target: %+v", j)
			}
		})
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

func TestResultBranchRequested(t *testing.T) {
	for _, diverged := range []bool{false, true} {
		t.Run(map[bool]string{false: "fast-forward", true: "non-fast-forward"}[diverged], func(t *testing.T) {
			m := newManager(t)
			repo := newRepo(t)
			gitT(t, repo, "branch", "feature")
			j := enqueue(t, m, "FAKE_EDIT_TRACKED", func(r *StartRequest) { r.RepoPath, r.Branch = repo, "feature" })
			if diverged {
				gitT(t, repo, "checkout", "-q", "feature")
				gitT(t, repo, "commit", "-q", "--allow-empty", "-m", "concurrent work")
				gitT(t, repo, "checkout", "-q", "main")
			}
			tip := gitT(t, repo, "rev-parse", "feature")
			j = runToEnd(t, m, j.ID)
			head := gitT(t, m.JobDir(j.ID)+"/work", "rev-parse", "HEAD")
			if diverged {
				if j.Branch != "job/"+j.ID || !strings.Contains(j.BranchNote, "requested branch feature not moved: not a fast-forward") {
					t.Fatalf("fallback: %+v", j)
				}
				if got := gitT(t, repo, "rev-parse", "feature"); got != tip {
					t.Fatal("requested branch moved")
				}
			} else if j.Branch != "feature" || j.BranchNote != "fast-forwarded" {
				t.Fatalf("fast-forward: %+v", j)
			}
			if got := gitT(t, repo, "rev-parse", "refs/heads/"+j.Branch); got != head {
				t.Fatal("branch does not hold job head")
			}
			if line := m.DoneLineFor(j); !strings.Contains(line, "branch "+j.Branch+" (") || !strings.Contains(line, j.BranchNote) {
				t.Fatalf("done line: %s", line)
			}
		})
	}
}

func TestResultBranchStartRejectsInvalidAndProtected(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	base := gitT(t, repo, "rev-parse", "HEAD")
	gitT(t, repo, "update-ref", "refs/remotes/origin/trunk", base)
	gitT(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")
	for _, name := range []string{"main", "master", "trunk", "release-1", "bad name", "@{-1}"} {
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
