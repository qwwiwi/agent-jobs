package jobs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// DoneLine is the one line delivered to the agent's live session.
func DoneLine(j *store.Job) string {
	title := strings.Join(strings.Fields(j.Title), " ")
	return fmt.Sprintf("[jobs:job-done] job %s %s %s · %d turns · $%.2f · %dm",
		j.ID, title, j.Status, j.Turns, j.CostUSD, (j.DurationSec+30)/60)
}

func (m *Manager) notifyPending() error {
	jobs, err := m.Store.PendingNotify()
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if t := m.Cfg.NotifyTmuxTarget; t != "" {
			if err := m.sendTmux(t, DoneLine(j)); err != nil {
				// Not marked: retried next tick, and cleanup waits for a delivered notify.
				m.Log.Printf("job %s: notify failed: %v", j.ID, err)
				continue
			}
		}
		if err := m.Store.MarkNotified(j.ID, m.Now()); err != nil {
			return err
		}
	}
	return nil
}

// sendTmux types the line literally (-l) and then presses Enter as a separate key.
func (m *Manager) sendTmux(target, line string) error {
	bin := m.Cfg.tmuxBin()
	if out, err := exec.Command(bin, "send-keys", "-t", target, "-l", line).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux send-keys: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command(bin, "send-keys", "-t", target, "Enter").CombinedOutput(); err != nil {
		return fmt.Errorf("tmux send-keys Enter: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *Manager) cleanupDue() error {
	jobs, err := m.Store.PendingCleanup()
	if err != nil {
		return err
	}
	delay := time.Duration(*m.Cfg.CleanupDelayMinutes) * time.Minute
	now := m.Now()
	for _, j := range jobs {
		if j.NotifiedAt == nil || now.Sub(*j.NotifiedAt) < delay {
			continue
		}
		if err := m.Cleanup(j); err != nil {
			m.Log.Printf("job %s: cleanup failed (will retry, worktree kept): %v", j.ID, err)
			continue
		}
		if err := m.Store.MarkCleaned(j.ID, now); err != nil {
			return err
		}
	}
	return nil
}

// Cleanup saves diffstat.txt, commits.patch, uncommitted.patch, sets refs/agent-jobs/jobs/<id>
// in the source repo to the job's last commit, then removes ONLY this job's worktree.
// Any failure before removal keeps the worktree, so no work is lost silently.
func (m *Manager) Cleanup(j *store.Job) error {
	dir := m.JobDir(j.ID)
	work := filepath.Join(dir, "work")
	if j.RepoPath == "" {
		return nil // empty work dir, nothing to preserve; prune comes later
	}
	if _, err := os.Stat(work); os.IsNotExist(err) {
		return nil // already removed
	}
	// Make untracked files visible to diff without staging their content.
	if _, err := git(work, "add", "--all", "--intent-to-add"); err != nil {
		return err
	}
	stat, err := git(work, "diff", "--stat", j.BaseCommit)
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, "diffstat.txt"), []byte(stat+"\n")); err != nil {
		return err
	}
	commits, err := gitRaw(work, "format-patch", j.BaseCommit+"..HEAD", "--stdout")
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, "commits.patch"), commits); err != nil {
		return err
	}
	// Staged + unstaged changes relative to the job's last commit.
	unc, err := gitRaw(work, "diff", "--binary", "HEAD")
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, "uncommitted.patch"), unc); err != nil {
		return err
	}
	head, err := git(work, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if _, err := git(j.RepoPath, "update-ref", "refs/agent-jobs/jobs/"+j.ID, head); err != nil {
		return err
	}
	if _, err := git(j.RepoPath, "worktree", "remove", "--force", work); err != nil {
		return err
	}
	m.Log.Printf("job %s: worktree removed, patches saved, ref refs/agent-jobs/jobs/%s -> %s", j.ID, j.ID, head)
	return nil
}

// gitRaw returns raw stdout (patches must not be trimmed).
func gitRaw(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
