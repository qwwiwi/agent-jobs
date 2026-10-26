package jobs

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
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

// CostText is a cost for people: "unknown" when it is not known (a codex job, a killed attempt), never
// $0.00 for it.
func CostText(cost float64, unknown bool) string {
	if unknown {
		return "unknown"
	}
	return fmt.Sprintf("$%.2f", cost)
}

// CodexLimitsNA is how the turn and budget caps of a codex job are shown: codex has none, the timeout
// is its only limit.
const CodexLimitsNA = "n/a (codex)"

// LimitsText is a job's limits for people: turns · budget · timeout.
func LimitsText(j *store.Job) string {
	if j.Runner == store.RunnerCodex {
		return fmt.Sprintf("%s turns · %s budget · %dm", CodexLimitsNA, CodexLimitsNA, j.TimeoutMinutes)
	}
	return fmt.Sprintf("%d turns · $%g · %dm", j.MaxTurns, j.MaxBudgetUSD, j.TimeoutMinutes)
}

// DoneLine is the one line delivered to the agent's live session.
// The model and the reasoning effort follow the duration (rows from before the effort was recorded
// show neither). A job failed because processes survived it says so at the end of the line; a job
// whose folder is at least dirNote bytes (Config.JobDirNoteBytes; 0 = never), or too big to measure
// within the walk's bounds, says how big it is; a resumed attempt
// says «resumed» with its attempt number (and «fresh session» when the session could not be resumed).
// An owner-waits job ends the line with OwnerWaitsMarker.
func DoneLine(j *store.Job, dirNote int64) string { return doneLine(j, dirNote, "") }

// doneLine is DoneLine with the cost text replaced by cost when it is not empty (an estimate).
func doneLine(j *store.Job, dirNote int64, cost string) string {
	if cost == "" {
		cost = CostText(j.CostUSD, j.CostIsUnknown())
		if j.CostIsUnknown() {
			cost = "cost " + cost
		}
	}
	line := fmt.Sprintf("[jobs:job-done] job %s %s %s · %d turns · %s · %dm",
		Printable(j.ID, 64), RedactPrintable(j.Title, MaxTitleRunes), j.Status, j.Turns, cost, (j.DurationSec+30)/60)
	if j.ReasoningEffort != "" {
		line += fmt.Sprintf(" · model %s · effort %s", Printable(j.Model, 64), j.ReasoningEffort)
	}
	if c := Containment(j); c != "" && (j.ScopeUnit == "" || j.ScopePending) {
		line += " · " + c // the weaker containment, or a scope stop still pending, is said on the line
	}
	if strings.Contains(j.Error, ReasonSurvivors) {
		line += " · " + ReasonSurvivors
	}
	if strings.Contains(j.Error, ReviewChangedWarning) {
		line += " · " + ReviewChangedWarning
	}
	if n := acceptanceNote(j); n != "" {
		line += " · " + n
	}
	switch {
	case dirNote > 0 && j.DirPartial:
		line += " · job dir at least " + SizeText(j.DirBytes) + " (not fully measured)"
	case dirNote > 0 && j.DirBytes >= dirNote:
		line += " · job dir " + SizeText(j.DirBytes)
	}
	if n := OffContractNote(j.Error); n != "" {
		line += " · " + n
	}
	if why, ok := autoContinuedFrom(j.PendingContext); ok && j.Attempt > 1 {
		line += fmt.Sprintf(" · auto-continued (attempt %d; %s", j.Attempt, why)
		if j.RunMode == store.RunFallback {
			line += "; fresh session"
		}
		line += ")"
	} else if j.Attempt > 1 {
		line += fmt.Sprintf(" · resumed (attempt %d", j.Attempt)
		if j.RunMode == store.RunFallback {
			line += ", fresh session"
		}
		line += ")"
	}
	if j.MergeNote != "" {
		line += " · " + j.MergeNote
	}
	if j.OwnerWaits {
		line += " · " + OwnerWaitsMarker
	}
	return RedactText(line) // the title and the error come from the brief and the job
}

// SizeText is a byte count for people, in binary units ("512 B", "1.5 MB", "2.0 GB").
func SizeText(n int64) string {
	const unit = 1 << 10
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, i := float64(n)/unit, 0
	for v >= unit && i < 3 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %cB", v, "KMGT"[i])
}

// ReviewChangedWarning marks a review job whose worktree differed from its base when it ended: the
// guard refuses writes into it (--read-only), so this is the second layer (a program that wrote by
// itself, a script the job ran). It is appended to the job's error and shown on the job-done line.
const ReviewChangedWarning = "WARNING: review job changed the worktree"

// noteReviewChanges appends ReviewChangedWarning to a finished review job whose worktree has commits
// past the base, staged, unstaged or untracked (not ignored) changes, or whose prepared worktree is
// gone or inaccessible. Run after the skill links are removed (they are not the job's change). A codex
// research job is checked the same way: it is read-only in intent, and under codex_sandbox
// worktree-full-access no codex sandbox keeps it from writing.
func (m *Manager) noteReviewChanges(j *store.Job) {
	readOnly := j.Kind == "review" || (j.Runner == store.RunnerCodex && j.Kind == "research")
	if !readOnly || j.RepoPath == "" {
		return
	}
	work := filepath.Join(m.JobDir(j.ID), "work")
	if _, err := os.Stat(work); err != nil {
		// No worktree.json: prepare never created the worktree, so there is nothing to compare.
		if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), "worktree.json")); err != nil {
			return
		}
		m.appendReviewNote(j, []string{"worktree missing or inaccessible"})
		return
	}
	var what []string
	head, err := git(work, "rev-parse", "HEAD")
	if err == nil {
		var base string
		if base, err = git(work, "rev-parse", "--verify", j.BaseCommit+"^{commit}"); err == nil && head != base {
			what = append(what, "HEAD moved from the base commit")
		}
	}
	if err == nil {
		var st string
		if st, err = git(work, "status", "--porcelain", "--untracked-files=all"); err == nil && st != "" {
			what = append(what, fmt.Sprintf("%d changed or untracked paths", len(strings.Split(st, "\n"))))
		}
	}
	if err != nil {
		m.Log.Printf("job %s: review worktree check: %s", j.ID, RedactText(err.Error()))
		what = append(what, "worktree check failed")
	}
	if len(what) == 0 {
		return
	}
	m.appendReviewNote(j, what)
}

// appendReviewNote logs ReviewChangedWarning with what was found and appends it to the job's error.
func (m *Manager) appendReviewNote(j *store.Job, what []string) {
	note := ReviewChangedWarning + " (" + strings.Join(what, ", ") + ")"
	m.Log.Printf("job %s: %s", j.ID, note)
	if err := m.Store.AppendError(j.ID, note); err != nil {
		m.Log.Printf("job %s: record note: %v", j.ID, err)
	}
}

// Containment says how a launched job's processes were contained: "scope <unit>" (its own systemd user
// scope; "…, stop pending" while a failed stop is retried) or "session only" (the weaker fallback:
// cgroup_scope off, or auto without a working user manager). "" for a job that never launched.
func Containment(j *store.Job) string {
	switch {
	case j.ScopeUnit != "" && j.ScopePending:
		return "scope " + j.ScopeUnit + ", stop pending"
	case j.ScopeUnit != "":
		return "scope " + j.ScopeUnit
	case j.StartedAt == nil:
		return ""
	}
	return "session only"
}

// DoneLineFor is the job-done line as notify delivers it: a job whose cost was not reported but could be
// estimated from its tokens (its stats row) shows "cost $0.42 est." instead of "cost unknown".
func (m *Manager) DoneLineFor(j *store.Job) string {
	cost := ""
	if j.CostIsUnknown() {
		if st, err := m.Store.GetStats(j.ID); err == nil && st.CostSource == store.CostSourceEstimated {
			cost = "cost " + EstimatedCostText(st.CostUSD+st.CostEstimatedUSD)
		}
	}
	return doneLine(j, m.Config().JobDirNoteBytes(), cost)
}

// checkMerge records the merge check of a job about to be notified (MergeNote) on its row and on j;
// a failure to record it is logged, the line then still carries it. Each delivery attempt checks
// again: the job's head or its branch may have moved since a failed one, and MergeNote's cache keeps
// a retry with neither moved from running merge-tree again. A new "" (the head reset to the base, or
// the worktree gone) is recorded too, so an earlier result is not sent.
func (m *Manager) checkMerge(j *store.Job) {
	note := m.MergeNote(j)
	if note == j.MergeNote {
		return
	}
	j.MergeNote = note
	if err := m.Store.SetMergeNote(j.ID, note); err != nil {
		m.Log.Printf("job %s: record merge check: %v", j.ID, err)
	}
}

// notifyPending delivers job-done lines. A job is marked notified only after tmux accepted the
// line; a failing tmux keeps it pending and it is retried next tick (cleanup waits for a delivered
// notify, so the worktree is kept meanwhile). With no target configured, notify is off: the line is
// written to the log and the job is marked notified right away, so the 30-minute cleanup still runs.
func (m *Manager) notifyPending() error {
	jobs, err := m.Store.PendingNotify()
	if err != nil || len(jobs) == 0 {
		return err
	}
	t := m.Config().NotifyTmuxTarget
	if t == "" {
		for _, j := range jobs {
			m.checkMerge(j)
			m.Log.Printf("notify off (notify_tmux_target not configured), marked notified: %s", m.DoneLineFor(j))
			if j.OwnerWaits {
				if err := m.Store.BeginOwnerPending(j.ID, store.OwnerNotifyOff, m.Now()); err != nil {
					return err
				}
			}
			if err := m.Store.MarkNotified(j.ID, m.Now()); err != nil {
				return err
			}
			m.forgetMergeNote(j.ID)
		}
		return nil
	}
	for _, j := range jobs {
		// An owner-waits job's pending record exists before its line can reach the session, so an
		// owner_replied right after the line (before the mark below) has a record to clear.
		if j.OwnerWaits {
			if err := m.Store.BeginOwnerPending(j.ID, store.OwnerDelivering, m.Now()); err != nil {
				return err
			}
		}
		m.checkMerge(j)
		if err := m.sendTmuxLocked(t, m.DoneLineFor(j)); err != nil {
			m.Log.Printf("job %s: notify failed, retry next tick: %s", j.ID, RedactText(err.Error()))
			if errors.Is(err, errTmuxTimeout) {
				// A stalled tmux (or lock holder) would cost every other line the same wait: the
				// rest are retried next tick, the tick goes on.
				break
			}
			continue
		}
		if afterDoneLineSent != nil {
			afterDoneLineSent(j)
		}
		if err := m.Store.MarkNotified(j.ID, m.Now()); err != nil {
			return err
		}
		m.forgetMergeNote(j.ID)
	}
	return nil
}

// afterDoneLineSent, when set (tests only), runs after tmux accepted a job-done line and before the
// job is marked notified: the gap where the session may already act on the line.
var afterDoneLineSent func(j *store.Job)

// sendTmux types the sanitized line literally (-l) and then presses Enter as a separate key,
// on the configured tmux socket (-L) when there is one. Every tmux call is bound to ctx: on its
// end the call's process group is killed and errTmuxTimeout returned.
func (m *Manager) sendTmux(ctx context.Context, target, line string) error {
	line = tmuxArgEscape(Printable(line, 0))
	if err := m.runTmux(ctx, "send-keys", "-t", target, "-l", line); err != nil {
		return fmt.Errorf("tmux send-keys: %w", err)
	}
	if err := m.runTmux(ctx, "send-keys", "-t", target, "Enter"); err != nil {
		return fmt.Errorf("tmux send-keys Enter: %w", err)
	}
	return nil
}

// tmuxArgEscape keeps a final ';' of an argument literal. tmux parses its command-line arguments
// for command separators (cmd-parse.y, cmd_parse_from_arguments) by the argument's final byte: one
// ending in ';' ends a command there, and how preceding backslashes are taken varies (tmux 3.4 types
// both "a\;" and "a\\;" as "a;"). So a line ending in ';' gets one trailing space, which keeps the
// final byte off ';' regardless of what precedes it. It runs after Printable, which would trim the
// space. A ';' anywhere else is literal already.
func tmuxArgEscape(s string) string {
	if strings.HasSuffix(s, ";") {
		return s + " "
	}
	return s
}

// runTmux runs one tmux command within ctx, in a process group of its own that is killed when ctx
// ends (WaitDelay bounds the wait for pipes a straggler may hold open).
func (m *Manager) runTmux(ctx context.Context, args ...string) error {
	_, err := m.runTmuxOutput(ctx, args...)
	return err
}

// runTmuxOutput is runTmux that returns the command's stdout.
func (m *Manager) runTmuxOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, m.Config().tmuxBin(), m.Config().tmuxArgs(args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), nil
	}
	if ctx.Err() != nil {
		return "", fmt.Errorf("%w (%v)", errTmuxTimeout, ctx.Err())
	}
	return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()+stdout.String()))
}

// After cleanupBackoffAfter consecutive failed cleanups of a job, the next tries wait twice as long
// each time (one tick first), at most cleanupBackoffMax, and only the back-off itself is logged.
const (
	cleanupBackoffAfter = 3
	cleanupBackoffMax   = time.Hour
)

// errRepoGone is a cleanup refused because the job's repo_path no longer exists. Only the daemon's
// row names the repository (job-writable files are never used to find another one, #57), so such a
// cleanup cannot succeed: once the back-off reaches its cap the row is marked skipped for good.
var errRepoGone = errors.New("repo_path is gone")

// cleanupFail is a job's run of failed cleanups (in memory: a daemon restart retries at once).
type cleanupFail struct {
	n    int
	next time.Time // no retry before this
}

func (m *Manager) cleanupDue() error {
	jobs, err := m.Store.PendingCleanup()
	if err != nil {
		return err
	}
	now := m.Now()
	for id := range m.cleanupFails {
		if !slices.ContainsFunc(jobs, func(j *store.Job) bool { return j.ID == id }) {
			delete(m.cleanupFails, id) // cleaned, skipped, resumed or pruned meanwhile
		}
	}
	for _, j := range jobs {
		if j.NotifiedAt == nil || now.Sub(*j.NotifiedAt) < CleanupDelay {
			continue
		}
		if j.ScopePending {
			continue // processes of the job may still use the worktree; retried after the scope stop
		}
		f := m.cleanupFails[j.ID]
		if now.Before(f.next) {
			continue // backing off
		}
		if err := m.Cleanup(j); err != nil {
			if err := m.cleanupFailed(j, f, now, err); err != nil {
				return err
			}
			continue
		}
		if f.n > 0 {
			delete(m.cleanupFails, j.ID)
			if f.n >= cleanupBackoffAfter {
				m.Log.Printf("job %s: cleanup succeeded after %d failed attempts", j.ID, f.n)
			}
		}
		if err := m.Store.MarkCleaned(j.ID, now); err != nil {
			return err
		}
	}
	return nil
}

// cleanupFailed records one more failed cleanup of job j (f is its run so far). The first
// cleanupBackoffAfter failures are logged and retried on the next tick; at that count the back-off
// is logged once, and later tries wait 2, 4, 8 … ticks (at most cleanupBackoffMax) and log nothing.
// A cleanup refused with errRepoGone whose wait has reached the cap is marked skipped (one log line):
// it is never retried and the job's folder is kept (prune leaves an uncleaned repo job alone).
func (m *Manager) cleanupFailed(j *store.Job, f cleanupFail, now time.Time, err error) error {
	f.n++
	msg := RedactText(err.Error())
	wait := time.Duration(0)
	if f.n >= cleanupBackoffAfter {
		wait = cleanupBackoffMax
		if k := f.n - cleanupBackoffAfter + 1; k < 16 {
			wait = min(TickInterval<<k, cleanupBackoffMax)
		}
		f.next = now.Add(wait)
	}
	if errors.Is(err, errRepoGone) && wait >= cleanupBackoffMax {
		if err := m.Store.MarkCleanupSkipped(j.ID, now); err != nil {
			return err
		}
		delete(m.cleanupFails, j.ID)
		m.Log.Printf("job %s: repo_path gone; cleanup skipped, remove manually (failed %d times, not retried, "+
			"worktree and job folder kept): %s", j.ID, f.n, msg)
		return nil
	}
	switch {
	case f.n < cleanupBackoffAfter:
		m.Log.Printf("job %s: cleanup failed (will retry, worktree kept): %s", j.ID, msg)
	case f.n == cleanupBackoffAfter:
		m.Log.Printf("job %s: cleanup failed (worktree kept): %s; failed %d times in a row, backing off "+
			"(retries up to every %s, not logged until it succeeds)", j.ID, msg, f.n, cleanupBackoffMax)
	}
	if m.cleanupFails == nil {
		m.cleanupFails = make(map[string]cleanupFail)
	}
	m.cleanupFails[j.ID] = f
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

// under reports whether path (symlinks resolved) lies strictly inside dir (symlinks resolved).
func under(path, dir string) bool {
	rp, err1 := filepath.EvalSymlinks(path)
	rd, err2 := filepath.EvalSymlinks(dir)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(rd, rp)
	return err == nil && rel != "." && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// verifyWorktree proves that <jobdir>/work is the worktree this job created: registered in the
// source repo at that path, with its admin dir under <repo's common git dir>/worktrees/ and equal to
// the one recorded at creation, carrying this job's owner mark.
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
	common, err := git(j.RepoPath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if !under(gitDir, filepath.Join(common, "worktrees")) {
		return fmt.Errorf("worktree admin dir %s is not under %s", gitDir, filepath.Join(common, "worktrees"))
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
	if m.spawnedGroupAlive(j) {
		return fmt.Errorf("refusing to touch %s: process group %d of the job is still alive", work, j.PGID)
	}
	if m.acceptanceGroupAlive(j) {
		return fmt.Errorf("refusing to touch %s: acceptance process group %d of the job is still alive", work, j.AcceptancePGID)
	}
	fi, err := os.Lstat(work)
	if os.IsNotExist(err) {
		return nil // already removed
	}
	if err != nil {
		return fmt.Errorf("refusing to touch %s: %w", work, err)
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to touch %s: not a real directory (%s)", work, fi.Mode().Type())
	}
	// Fail closed: the repository comes only from the row, never from a job-writable file (#57).
	if _, err := os.Lstat(j.RepoPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("refusing to touch %s: repo %s: %w", work, j.RepoPath, errRepoGone)
	}
	if err := m.verifyWorktree(j, work); err != nil {
		return fmt.Errorf("refusing to touch %s: %w", work, err)
	}
	m.removeSkills(j) // removed when the attempt ended; again here, so no link reaches a patch
	drivers, err := repoDrivers(work)
	if err != nil {
		return err
	}
	if len(drivers) > 0 {
		return m.keepWithDrivers(j, dir, work, drivers)
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
	// --no-ext-diff --no-textconv: the job may have set diff.external / a textconv in the repo config.
	stat, err := git(work, "diff", "--no-ext-diff", "--no-textconv", "--stat", j.BaseCommit)
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
		{"commits.patch", []string{"format-patch", "--no-ext-diff", "--no-textconv", j.BaseCommit + "..HEAD", "--stdout"}},
		{"staged.patch", []string{"diff", "--no-ext-diff", "--no-textconv", "--cached", "--binary", "HEAD"}},
		{"uncommitted.patch", []string{"diff", "--no-ext-diff", "--no-textconv", "--binary", "HEAD"}},
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

// driverKey reports a git config key that names a program git runs while diffing or reading the
// working tree: a filter driver (filter.<name>.clean/smudge/process), a diff driver command or textconv.
func driverKey(k string) bool {
	k = strings.ToLower(k)
	return strings.HasPrefix(k, "filter.") ||
		(strings.HasPrefix(k, "diff.") && (strings.HasSuffix(k, ".command") || strings.HasSuffix(k, ".textconv")))
}

// repoDrivers returns the driver keys (driverKey) in the repository-level config the job can write:
// the shared repo config (scope local) and the worktree's own config (scope worktree), includes
// followed (an included file reports the scope of the file that includes it). The daemon owner's
// global/system config (e.g. filter.lfs) is not the job's and does not count.
func repoDrivers(work string) ([]string, error) {
	out, err := gitRaw(work, repoDriversArgs...)
	if err != nil {
		return nil, err
	}
	return driverKeys(out), nil
}

// repoDriversArgs lists the config keys with their scopes for driverKeys.
var repoDriversArgs = []string{"config", "--list", "--show-scope", "--name-only", "--includes", "-z"}

// driverKeys returns the driver keys (driverKey) of scope local or worktree in the output of
// `git` repoDriversArgs.
func driverKeys(out []byte) []string {
	f := strings.Split(string(out), "\x00")
	var keys []string
	for i := 0; i+1 < len(f); i += 2 {
		scope, k := f[i], strings.TrimSpace(f[i+1])
		if (scope == "local" || scope == "worktree") && driverKey(k) && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

// keepWithDrivers is the cleanup of a job whose repository config names filter/diff drivers: any
// diff, format-patch or status would run them in the daemon, so no patch is made and the worktree is
// kept for a human to inspect. The job's last commit is still saved under refs/agent-jobs/jobs/<id>
// and the cleanup counts as done (it is not retried).
func (m *Manager) keepWithDrivers(j *store.Job, dir, work string, drivers []string) error {
	note := "cleanup skipped: the repository config names filter/diff drivers (" + strings.Join(drivers, ", ") +
		"); no patches were made, the worktree is kept: " + work
	if err := writeAtomic(filepath.Join(dir, "diffstat.txt"), []byte(note+"\n")); err != nil {
		return err
	}
	head, err := git(work, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if _, err := git(j.RepoPath, "update-ref", "refs/agent-jobs/jobs/"+j.ID, head); err != nil {
		return err
	}
	m.Log.Printf("job %s: %s; ref refs/agent-jobs/jobs/%s -> %s", j.ID, note, j.ID, head)
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
	cmd.Env = daemonGitEnv()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
