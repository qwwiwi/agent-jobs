package jobs

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// MaxTitleRunes caps the title inside the job-done line.
const MaxTitleRunes = 120

// Printable keeps only printable characters: every whitespace (tab, CR, LF, …) becomes a space,
// control and format characters (Ctrl-U, ESC, DEL, C1, bidi overrides, zero-width) are dropped,
// runs of spaces collapse, and the result is capped to max runes (0 = no cap). Text typed into a
// live tmux session must never carry keystrokes of its own.
func Printable(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		case unicode.IsPrint(r):
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if rs := []rune(out); max > 0 && len(rs) > max {
		out = string(rs[:max-1]) + "…"
	}
	return out
}

// DoneLine is the one line delivered to the agent's live session.
func DoneLine(j *store.Job) string {
	return fmt.Sprintf("[jobs:job-done] job %s %s %s · %d turns · $%.2f · %dm",
		Printable(j.ID, 64), Printable(j.Title, MaxTitleRunes), j.Status, j.Turns, j.CostUSD, (j.DurationSec+30)/60)
}

// notifyPending delivers job-done lines. A job is marked notified only after tmux accepted the
// line; with no target configured or a failing tmux it stays pending and is retried next tick
// (cleanup waits for a delivered notify, so the worktree is kept meanwhile).
func (m *Manager) notifyPending() error {
	jobs, err := m.Store.PendingNotify()
	if err != nil || len(jobs) == 0 {
		return err
	}
	t := m.Cfg.NotifyTmuxTarget
	if t == "" {
		m.Log.Printf("notify: notify_tmux_target is not configured; %d job-done line(s) pending", len(jobs))
		return nil
	}
	for _, j := range jobs {
		if err := m.sendTmux(t, DoneLine(j)); err != nil {
			m.Log.Printf("job %s: notify failed, retry next tick: %v", j.ID, err)
			continue
		}
		if err := m.Store.MarkNotified(j.ID, m.Now()); err != nil {
			return err
		}
	}
	return nil
}

// sendTmux types the sanitized line literally (-l) and then presses Enter as a separate key.
func (m *Manager) sendTmux(target, line string) error {
	bin := m.Cfg.tmuxBin()
	line = Printable(line, 0)
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
	now := m.Now()
	for _, j := range jobs {
		if j.NotifiedAt == nil || now.Sub(*j.NotifiedAt) < CleanupDelay {
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

func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// verifyWorktree proves that <jobdir>/work is the worktree this job created: registered in the
// source repo at that path, with the admin dir recorded at creation, carrying this job's owner mark.
func (m *Manager) verifyWorktree(j *store.Job, work string) error {
	var info WorktreeInfo
	b, err := os.ReadFile(filepath.Join(m.JobDir(j.ID), "worktree.json"))
	if err != nil {
		return fmt.Errorf("worktree.json: %w", err)
	}
	if err := json.Unmarshal(b, &info); err != nil {
		return fmt.Errorf("worktree.json: %w", err)
	}
	if !samePath(info.Path, work) {
		return fmt.Errorf("worktree.json path %s is not %s", info.Path, work)
	}
	list, err := git(j.RepoPath, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	registered := false
	for _, line := range strings.Split(list, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok && samePath(p, work) {
			registered = true
		}
	}
	if !registered {
		return fmt.Errorf("%s is not a registered worktree of %s", work, j.RepoPath)
	}
	gitDir, err := git(work, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	if !samePath(gitDir, info.GitDir) {
		return fmt.Errorf("worktree admin dir %s differs from the one recorded at creation (%s)", gitDir, info.GitDir)
	}
	owner, err := os.ReadFile(filepath.Join(gitDir, worktreeOwnerFile))
	if err != nil || strings.TrimSpace(string(owner)) != j.ID {
		return fmt.Errorf("worktree %s does not carry this job's owner mark", work)
	}
	return nil
}

// Cleanup verifies that <jobdir>/work is this job's own worktree, then saves (without touching
// the index or the files): diffstat.txt, commits.patch (format-patch base..HEAD), staged.patch
// (index vs HEAD), uncommitted.patch (working tree vs HEAD), untracked.tar (untracked, not ignored
// files); sets refs/agent-jobs/jobs/<id> to the job's last commit; and only then removes the worktree.
// Any failure before removal keeps the worktree, so no work is lost silently.
// Restore: see the restore block in docs/ARCHITECTURE.md (empty patches are skipped).
func (m *Manager) Cleanup(j *store.Job) error {
	dir := m.JobDir(j.ID)
	work := filepath.Join(dir, "work")
	if j.RepoPath == "" {
		return nil // empty work dir, nothing to preserve; prune comes later
	}
	if _, err := os.Lstat(work); os.IsNotExist(err) {
		return nil // already removed
	}
	if err := m.verifyWorktree(j, work); err != nil {
		return fmt.Errorf("refusing to touch %s: %w", work, err)
	}
	untracked, err := gitRaw(work, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	var files []string
	for _, f := range strings.Split(string(untracked), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	stat, err := git(work, "diff", "--stat", j.BaseCommit)
	if err != nil {
		return err
	}
	if len(files) > 0 {
		stat += "\nuntracked (untracked.tar):\n " + strings.Join(files, "\n ")
	}
	if err := writeAtomic(filepath.Join(dir, "diffstat.txt"), []byte(stat+"\n")); err != nil {
		return err
	}
	for _, p := range []struct {
		file string
		args []string
	}{
		{"commits.patch", []string{"format-patch", j.BaseCommit + "..HEAD", "--stdout"}},
		{"staged.patch", []string{"diff", "--cached", "--binary", "HEAD"}},
		{"uncommitted.patch", []string{"diff", "--binary", "HEAD"}},
	} {
		out, err := gitRaw(work, p.args...)
		if err != nil {
			return err
		}
		if err := writeAtomic(filepath.Join(dir, p.file), out); err != nil {
			return err
		}
	}
	if err := writeTar(filepath.Join(dir, "untracked.tar"), work, files); err != nil {
		return fmt.Errorf("untracked.tar: %w", err)
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

// writeTar archives files (paths relative to root) into dst: regular files and symlinks.
// Anything else (an embedded repository, a socket) fails the cleanup, so the worktree is kept.
func writeTar(dst, root string, files []string) error {
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	for _, rel := range files {
		full := filepath.Join(root, rel)
		fi, err := os.Lstat(full)
		if err != nil {
			return err
		}
		link := ""
		switch {
		case fi.Mode().IsRegular():
		case fi.Mode()&os.ModeSymlink != 0:
			if link, err = os.Readlink(full); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s is not a regular file or symlink (%s)", rel, fi.Mode().Type())
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			src, err := os.Open(full)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, src)
			_ = src.Close()
			if err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
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
