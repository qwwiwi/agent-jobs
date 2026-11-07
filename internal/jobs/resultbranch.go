package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// publishResultBranch runs after either runner has ended (including the Codex wrapper's commit).
// The result is published create-only (see publishResultRefs): the caller's optional target when it
// is free, job/<id> otherwise. A recorded outcome is never replayed at notification retries;
// resuming clears it for the next attempt.
func (m *Manager) publishResultBranch(j *store.Job) error {
	if j.RepoPath == "" || j.BaseCommit == "" || j.Kind == "review" || j.Kind == "research" || IsGateKind(j.Kind) ||
		j.Status == store.StatusQueued || j.Status == store.StatusRunning || j.Branch != "" || j.BranchNote != "" {
		return nil
	}
	work := filepath.Join(m.JobDir(j.ID), "work")
	if _, err := os.Stat(work); os.IsNotExist(err) {
		return nil // never prepared, or a legacy job whose worktree was already cleaned up.
	}
	record := func(branch, note string) error {
		if err := m.Store.SetResultBranch(j.ID, branch, note); err != nil {
			return err
		}
		j.Branch, j.BranchNote = branch, note
		return nil
	}
	fail := func(err error) error {
		note := "result branch not published: " + RedactPrintable(err.Error(), 240)
		m.Log.Printf("job %s: %s", j.ID, note)
		return record("", note)
	}
	if err := m.verifyWorktree(j, work); err != nil {
		return fail(fmt.Errorf("job worktree not verified: %w", err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	env := append(daemonGitEnv(), "GIT_GRAFT_FILE="+os.DevNull)
	runAt := func(dir string, args ...string) (string, error) {
		out, code, stderr, err := gitEnvCtx(ctx, env, dir, append([]string{noReplace}, args...)...)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if err == nil && code != 0 {
			err = fmt.Errorf("git %s exited %d: %s", args[0], code, stderr)
		}
		return strings.TrimSpace(out), err
	}
	head, err := runAt(work, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil {
		return fail(err)
	}
	if head == j.BaseCommit {
		return nil
	}
	ahead, err := runAt(work, "rev-list", "--count", j.BaseCommit+".."+head)
	if err != nil {
		return fail(err)
	}
	if ahead == "0" {
		return nil
	}
	// A positive base..head count also holds for a sibling or an unrelated history the job switched
	// to; only a descendant of the base is the job's own work.
	if _, err := runAt(work, "merge-base", "--is-ancestor", j.BaseCommit, head); err != nil {
		return fail(errors.New("job head is not a descendant of the base commit"))
	}
	run := branchGit(func(args ...string) (string, error) { return runAt(j.RepoPath, args...) })
	branch, note, err := publishResultRefs(run, j.ID, j.RequestedBranch, head)
	if err != nil {
		return fail(err)
	}
	return record(branch, note)
}
