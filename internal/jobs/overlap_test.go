package jobs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

func overlapRequest(repo string, paths []string, policy string) StartRequest {
	return StartRequest{Title: "overlap test", Kind: "fix", Brief: "edit declared paths", Model: "sonnet", RepoPath: repo, Paths: paths, OnOverlap: policy}
}

func TestOverlapWarnRefuseAndNoPathsFallback(t *testing.T) {
	m, repo := newManager(t), newRepo(t)
	first, err := m.Enqueue(overlapRequest(repo, []string{"src/*.go"}, ""))
	if err != nil {
		t.Fatal(err)
	}
	warning, err := m.Enqueue(overlapRequest(repo, []string{"src/new*.go"}, ""))
	if err != nil {
		t.Fatal(err)
	}
	warning = get(t, m, warning.ID)
	if warning.OnOverlap != "warn" || len(warning.Overlap) != 1 || warning.Overlap[0].ID != first.ID ||
		!slices.Equal(warning.Overlap[0].Paths, []string{"src/new*.go"}) || !slices.Equal(warning.Overlap[0].OtherPaths, first.Paths) || len(warning.WaitFor) != 0 {
		t.Fatalf("warning did not survive storage: %+v", warning)
	}
	if _, err := m.Enqueue(overlapRequest(repo, []string{"src/a.go"}, "refuse")); err == nil || !strings.Contains(err.Error(), first.ID) {
		t.Fatalf("overlap not refused with job id: %v", err)
	}
	disjoint, err := m.Enqueue(overlapRequest(repo, []string{"docs/**"}, "refuse"))
	if err != nil || len(disjoint.Overlap) != 0 {
		t.Fatalf("disjoint start: %+v %v", disjoint, err)
	}
	missing, err := m.Enqueue(overlapRequest(repo, nil, "refuse"))
	if err != nil || len(missing.Overlap) != 0 || !strings.Contains(m.SameRepoNote(missing), first.ID) {
		t.Fatalf("missing scopes must keep advisory: %+v %v", missing, err)
	}
	// The other job omitting paths also leaves only the advisory.
	otherRepo := newRepo(t)
	unknown, err := m.Enqueue(overlapRequest(otherRepo, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	declared, err := m.Enqueue(overlapRequest(otherRepo, []string{"new/*.go"}, "serialize"))
	if err != nil || len(declared.WaitFor) != 0 || len(declared.Overlap) != 0 || !strings.Contains(m.SameRepoNote(declared), unknown.ID) {
		t.Fatalf("undeclared peer changed fallback: %+v %v", declared, err)
	}
}

func TestOverlapSerializeQueueAndAtomicClaim(t *testing.T) {
	m, repo := newManager(t), newRepo(t)
	capacity := 3 // leave a free slot so launchQueued must inspect the dependency
	m.Cfg.MaxRunning = &capacity
	var blockers []*store.Job
	for range 2 {
		j, err := m.Enqueue(overlapRequest(repo, []string{"src/**"}, ""))
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Store.Claim(j.ID, "parent", m.Now()); err != nil {
			t.Fatal(err)
		}
		blockers = append(blockers, j)
	}
	child, err := m.Enqueue(overlapRequest(repo, []string{"src/new.go"}, "serialize"))
	if err != nil {
		t.Fatal(err)
	}
	child = get(t, m, child.ID)
	if !slices.Equal(child.WaitFor, []string{blockers[0].ID, blockers[1].ID}) {
		t.Fatalf("blockers not persisted: %+v", child)
	}
	if err := m.launchQueued(); err != nil {
		t.Fatal(err)
	}
	if got := get(t, m, child.ID); got.Status != store.StatusQueued {
		t.Fatalf("serialized job launched: %+v", got)
	}
	if wait := m.JobWaiting(context.Background(), child); !strings.Contains(wait, blockers[0].ID) {
		t.Fatalf("no waiting status: %s", wait)
	}
	if err := m.claim(child, "too-soon"); !errors.Is(err, store.ErrImmutable) {
		t.Fatalf("claim bypassed dependencies: %v", err)
	}
	for i, dep := range blockers {
		status := store.StatusSucceeded
		if i == 1 {
			status = store.StatusFailed
		} // serialization waits for completion, not acceptance or success
		if err := m.Store.Finish(dep.ID, "parent", status, 0, 0, 0, "", m.Now()); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := m.claim(child, "still-too-soon"); !errors.Is(err, store.ErrImmutable) {
				t.Fatalf("did not wait for every blocker: %v", err)
			}
		}
	}
	if wait, cancel, err := m.jobDependencies(child, true); err != nil || wait != "" || cancel != "" {
		t.Fatalf("completed blockers: %s %s %v", wait, cancel, err)
	}
	// A blocker resumes between the launch verdict and claim: claim's SQL sees it atomically.
	dep := blockers[1]
	if err := m.Store.Requeue(dep.ID, store.ResumeLimits{MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1}, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.claim(child, "raced"); !errors.Is(err, store.ErrImmutable) {
		t.Fatalf("resumed blocker not guarded: %v", err)
	}
	if err := m.Store.Finish(dep.ID, "", store.StatusCancelled, 0, 0, 0, "", m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.claim(child, "ready"); err != nil {
		t.Fatalf("did not release queue: %v", err)
	}
	if line := DoneLine(child, 0); !strings.Contains(line, "serialized wait for "+blockers[0].ID) {
		t.Fatalf("done line omits wait: %s", line)
	}
}

func TestOverlapParentSuccessAndSavedResult(t *testing.T) {
	m, repo := newManager(t), newRepo(t)
	parent, err := m.Enqueue(overlapRequest(repo, []string{"src/**"}, ""))
	if err != nil {
		t.Fatal(err)
	}
	req := overlapRequest(repo, []string{"src/child.go"}, "refuse")
	req.Parent = parent.ID // explicit parent stack handles its overlap even with refuse
	child, err := m.Enqueue(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(child.Overlap) != 1 || child.Parent != parent.ID {
		t.Fatalf("stack not recorded: %+v", child)
	}
	if err := m.claim(child, "early"); !errors.Is(err, store.ErrImmutable) {
		t.Fatalf("parent was not awaited: %v", err)
	}
	if err := m.Store.Claim(parent.ID, "parent", m.Now()); err != nil {
		t.Fatal(err)
	}
	// Use the saved result ref as cleanup does, so the parent's worktree is not required.
	gitT(t, repo, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "parent result")
	head := gitT(t, repo, "rev-parse", "HEAD")
	gitT(t, repo, "update-ref", "refs/agent-jobs/jobs/"+parent.ID, head)
	if err := m.Store.Finish(parent.ID, "parent", store.StatusSucceeded, 0, 0, 0, "", m.Now()); err != nil {
		t.Fatal(err)
	}
	if wait, cancel, err := m.jobDependencies(child, true); err != nil || wait != "" || cancel != "" {
		t.Fatalf("parent result: %s %s %v", wait, cancel, err)
	}
	child = get(t, m, child.ID)
	if child.BaseCommit != head || child.ParentBase != head || child.BaseBranch != parent.BaseBranch {
		t.Fatalf("stack base not frozen: %+v", child)
	}
	if err := m.claim(child, "ready"); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Finish(child.ID, "ready", store.StatusSucceeded, 0, 0, 0, "", m.Now()); err != nil {
		t.Fatal(err)
	}
	// Starting after the parent completed resolves the same saved result at admission.
	later, err := m.Enqueue(req)
	if err != nil || later.BaseCommit != head || later.ParentBase != head {
		t.Fatalf("completed parent start: %+v %v", later, err)
	}
	if line := DoneLine(child, 0); !strings.Contains(line, "parent "+parent.ID) || !strings.Contains(line, "serialized") {
		t.Fatalf("stack done line: %s", line)
	}
}

func TestOverlapParentFailureAndValidation(t *testing.T) {
	for _, status := range []string{store.StatusFailed, store.StatusTimeout, store.StatusCancelled} {
		t.Run(status, func(t *testing.T) {
			m, repo := newManager(t), newRepo(t)
			parent, err := m.Enqueue(overlapRequest(repo, nil, ""))
			if err != nil {
				t.Fatal(err)
			}
			req := overlapRequest(repo, nil, "")
			req.Parent = parent.ID
			child, err := m.Enqueue(req)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Store.Finish(parent.ID, "", status, 0, 0, 0, "", m.Now()); err != nil {
				t.Fatal(err)
			}
			if _, cancel, err := m.jobDependencies(child, true); err != nil || !strings.Contains(cancel, "parent "+parent.ID+" is "+status) {
				t.Fatalf("failed parent: %q %v", cancel, err)
			}
			if _, err := m.Enqueue(req); err == nil || !strings.Contains(err.Error(), status) {
				t.Fatalf("failed parent accepted: %v", err)
			}
			if err := m.launchQueued(); err != nil {
				t.Fatal(err)
			}
			if got := get(t, m, child.ID); got.Status != store.StatusCancelled {
				t.Fatalf("failed parent child launched: %+v", got)
			}
		})
	}
	m, repo := newManager(t), newRepo(t)
	parent, err := m.Enqueue(overlapRequest(repo, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	for name, modify := range map[string]func(*StartRequest){
		"unknown parent":   func(r *StartRequest) { r.Parent = "unknown" },
		"other repository": func(r *StartRequest) { r.Parent = parent.ID; r.RepoPath = newRepo(t) },
		"base and parent":  func(r *StartRequest) { r.Parent = parent.ID; r.Base = "HEAD" },
		"review":           func(r *StartRequest) { r.Kind = "review" },
		"no repo":          func(r *StartRequest) { r.RepoPath = "" },
		"invalid policy":   func(r *StartRequest) { r.OnOverlap = "silent" },
		"outside scope":    func(r *StartRequest) { r.Paths = []string{"../outside"} },
	} {
		t.Run(name, func(t *testing.T) {
			req := overlapRequest(repo, []string{"src/**"}, "")
			modify(&req)
			if _, err := m.Enqueue(req); err == nil {
				t.Fatal("invalid overlap request accepted")
			}
		})
	}
}

func TestOverlapConcurrentAdmission(t *testing.T) {
	m, repo := newManager(t), newRepo(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, err := m.Enqueue(overlapRequest(repo, []string{"src/*.go"}, "refuse")); results <- err })
	}
	wg.Wait()
	close(results)
	started, refused := 0, 0
	for err := range results {
		if err == nil {
			started++
		} else if strings.Contains(err.Error(), "overlap") {
			refused++
		} else {
			t.Fatal(err)
		}
	}
	if started != 1 || refused != 1 {
		t.Fatalf("simultaneous starts bypassed check: %d started, %d refused", started, refused)
	}
}

func TestOverlapParentMissingResultAndOtherConflicts(t *testing.T) {
	m, repo := newManager(t), newRepo(t)
	parent, err := m.Enqueue(overlapRequest(repo, []string{"src/**"}, ""))
	if err != nil {
		t.Fatal(err)
	}
	peer, err := m.Enqueue(overlapRequest(repo, []string{"src/child.go"}, ""))
	if err != nil {
		t.Fatal(err)
	}
	req := overlapRequest(repo, []string{"src/child.go"}, "refuse")
	req.Parent = parent.ID
	if _, err := m.Enqueue(req); err == nil || !strings.Contains(err.Error(), peer.ID) {
		t.Fatalf("parent bypassed unrelated conflict: %v", err)
	}
	req.OnOverlap = "serialize"
	child, err := m.Enqueue(req)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(child.WaitFor, []string{peer.ID}) {
		t.Fatalf("stack must retain other blockers: %+v", child)
	}
	if err := m.Store.Finish(parent.ID, "", store.StatusSucceeded, 0, 0, 0, "", m.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Enqueue(req); err == nil || !strings.Contains(err.Error(), "result unavailable") {
		t.Fatalf("missing result silently used repository HEAD: %v", err)
	}
	if _, cancel, err := m.jobDependencies(child, true); err != nil || !strings.Contains(cancel, "result unavailable") {
		t.Fatalf("queued child with missing result: %s %v", cancel, err)
	}
}
