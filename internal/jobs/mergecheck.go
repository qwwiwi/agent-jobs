package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// MergeCheckTimeout bounds the merge check of one job at notify time (every git call together).
var MergeCheckTimeout = 10 * time.Second

// Caps of the merge check's line: conflicting files and commits the branch gained.
const (
	mergeCheckMaxFiles   = 5
	mergeCheckMaxCommits = 5
)

// baseBranch is the branch a job queued from ref in repo will be landed on: ref itself when it names a
// local or remote-tracking branch, the branch checked out in repo when ref is HEAD, else "" (a tag, a
// commit, a detached HEAD). It is a full ref name.
func baseBranch(repo, ref string) string {
	name := ref
	if ref == "HEAD" {
		out, err := git(repo, "symbolic-ref", "-q", "HEAD")
		if err != nil {
			return ""
		}
		name = out
	} else {
		out, err := git(repo, "rev-parse", "--symbolic-full-name", ref)
		if err != nil {
			return ""
		}
		name = out
	}
	if strings.HasPrefix(name, "refs/heads/") || strings.HasPrefix(name, "refs/remotes/") {
		return name
	}
	return ""
}

// shortBranch is a branch ref without refs/heads/ or refs/remotes/.
func shortBranch(ref string) string {
	if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return b
	}
	return strings.TrimPrefix(ref, "refs/remotes/")
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// gitCtx runs git within ctx (its process group is killed when ctx ends) and returns stdout, the exit
// code (-1 when git did not exit by itself) and stderr.
func gitCtx(ctx context.Context, dir string, args ...string) (string, int, string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = daemonGitEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", -1, "", fmt.Errorf("timed out after %s", MergeCheckTimeout)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return stdout.String(), ee.ExitCode(), strings.TrimSpace(stderr.String()), nil
	}
	if err != nil {
		return "", -1, "", err
	}
	return stdout.String(), 0, strings.TrimSpace(stderr.String()), nil
}

// MergeNote is the job-done line's merge check of a repository job with commits beyond its base:
// whether `git merge-tree --write-tree` (objects only, no ref moves) merges the job's head onto the
// current tip of its base branch, within MergeCheckTimeout. "" for a job without a repository, a
// worktree or commits of its own.
func (m *Manager) MergeNote(j *store.Job) string {
	if j.RepoPath == "" || j.BaseCommit == "" {
		return ""
	}
	work := filepath.Join(m.JobDir(j.ID), "work")
	if fi, err := os.Lstat(work); err != nil || !fi.IsDir() {
		return "" // never launched, or already cleaned up
	}
	branch := "a branch"
	if j.BaseBranch != "" {
		branch = shortBranch(j.BaseBranch)
	}
	notChecked := func(why string) string {
		return fmt.Sprintf("merge onto %s not checked: %s", Printable(branch, 80), RedactPrintable(why, 160))
	}
	if err := m.verifyWorktree(j, work); err != nil {
		return notChecked("the job's worktree is not verified")
	}
	ctx, cancel := context.WithTimeout(context.Background(), MergeCheckTimeout)
	defer cancel()
	run := func(dir string, args ...string) (string, error) {
		out, code, stderr, err := gitCtx(ctx, dir, args...)
		if err == nil && code != 0 {
			err = fmt.Errorf("git %s exited %d: %s", args[0], code, stderr)
		}
		return strings.TrimSpace(out), err
	}
	head, err := run(work, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return notChecked(err.Error())
	}
	if head == j.BaseCommit {
		return ""
	}
	ahead, err := run(j.RepoPath, "rev-list", "--count", j.BaseCommit+".."+head)
	if err != nil {
		return notChecked(err.Error())
	}
	if ahead == "0" {
		return ""
	}
	if j.BaseBranch == "" {
		return notChecked("the job's base is not a branch (a tag, a commit or a detached HEAD)")
	}
	tip, err := run(j.RepoPath, "rev-parse", "--verify", "--end-of-options", j.BaseBranch+"^{commit}")
	if err != nil {
		return notChecked("branch " + shortBranch(j.BaseBranch) + " not found")
	}
	out, code, stderr, err := gitCtx(ctx, j.RepoPath, "merge-tree", "--write-tree", "--name-only", "--no-messages", tip, head)
	switch {
	case err != nil:
		return notChecked(err.Error())
	case code == 0:
		return fmt.Sprintf("merges cleanly onto %s at %s", Printable(branch, 80), shortSHA(tip))
	case code != 1:
		if strings.Contains(stderr, "usage") || strings.Contains(stderr, "unknown option") {
			return notChecked("git older than 2.38 (no merge-tree --write-tree)")
		}
		return notChecked(fmt.Sprintf("git merge-tree exited %d: %s", code, stderr))
	}
	// Exit 1: the first line is the tree, then one conflicted path per line.
	var files []string
	seen := map[string]bool{}
	for _, f := range strings.Split(strings.TrimSpace(out), "\n")[1:] {
		if f = strings.TrimSpace(f); f != "" && !seen[f] {
			seen[f] = true
			files = append(files, Printable(f, 120))
		}
	}
	note := fmt.Sprintf("WARNING: conflicts with %s at %s in %s", Printable(branch, 80), shortSHA(tip), capList(files, mergeCheckMaxFiles))
	gainedRange := j.BaseCommit + ".." + tip
	if gained, err := run(j.RepoPath, "log", "--no-show-signature", "--format=%h %s", "-n", strconv.Itoa(mergeCheckMaxCommits),
		gainedRange); err == nil && gained != "" {
		lines := strings.Split(gained, "\n")
		for i := range lines {
			lines[i] = Printable(lines[i], 80)
		}
		note += "; " + Printable(branch, 80) + " gained since base: " + strings.Join(lines, ", ")
		if total, err := run(j.RepoPath, "rev-list", "--count", gainedRange); err == nil {
			if n, err := strconv.Atoi(total); err == nil && n > len(lines) {
				note += fmt.Sprintf(" (+%d more)", n-len(lines))
			}
		}
	}
	return note
}

// capList joins at most n items, then "(+k more)".
func capList(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" (+%d more)", len(items)-n)
}

// maxSameRepoJobs caps the jobs SameRepoNote names.
const maxSameRepoJobs = 5

// SameRepoNote is job_start's advisory line about the other queued or running jobs on the same
// repository as j (their commits may collide when landed separately); "" when there are none. It
// never refuses a job.
func (m *Manager) SameRepoNote(j *store.Job) string {
	if j.RepoPath == "" {
		return ""
	}
	others, err := m.Store.ListActiveOnRepo(j.RepoPath, j.ID)
	if err != nil {
		m.Log.Printf("job %s: other jobs on the repository: %v", j.ID, err)
		return ""
	}
	if len(others) == 0 {
		return ""
	}
	names := make([]string, 0, len(others))
	for _, o := range others {
		names = append(names, fmt.Sprintf("%s %s «%s»", Printable(o.ID, 64), o.Status, RedactPrintable(o.Title, 60)))
	}
	return fmt.Sprintf("other jobs on this repository (their commits may conflict when landed separately): %s",
		capList(names, maxSameRepoJobs))
}
