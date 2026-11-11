package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// MergeCheckTimeout bounds the merge check of one job at notify time (every git call together).
var MergeCheckTimeout = 10 * time.Second

// Caps of the merge check's line: conflicting files, the bytes of one file's name and commits the
// branch gained.
const (
	mergeCheckMaxFiles   = 5
	mergeCheckMaxName    = 120
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
	return gitEnvCtx(ctx, daemonGitEnv(), dir, args...)
}

// gitEnvCtx is gitCtx with the given environment.
func gitEnvCtx(ctx context.Context, env []string, dir string, args ...string) (string, int, string, error) {
	stdout, stderr, err := runGit(ctx, "", env, append([]string{"-C", dir}, args...)...)
	if ctx.Err() != nil {
		return "", -1, "", fmt.Errorf("timed out after %s", MergeCheckTimeout)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(stdout), ee.ExitCode(), strings.TrimSpace(string(stderr)), nil
	}
	if err != nil {
		return "", -1, "", err
	}
	return string(stdout), 0, strings.TrimSpace(string(stderr)), nil
}

// isolatedMergeTree runs `git merge-tree --write-tree` of tip and head so that nothing the repository
// (or a job writing to its shared config) controls can run a command: the merge happens in a temporary
// bare repository with an empty config, no system or global config, an empty HOME and XDG_CONFIG_HOME,
// no work tree or index to take .gitattributes from, and the real repository's objects reached as an
// alternate. No merge driver is defined there, so a `merge=<driver>` attribute falls back to the
// built-in text merge; the trees merge-tree writes stay in the temporary repository.
func isolatedMergeTree(ctx context.Context, repo, tip, head string) (string, int, string, error) {
	common, err := gitCtxOK(ctx, repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", -1, "", err
	}
	format, err := gitCtxOK(ctx, repo, "rev-parse", "--show-object-format")
	if err != nil {
		return "", -1, "", err
	}
	tmp, err := os.MkdirTemp("", "agent-jobs-merge-check-")
	if err != nil {
		return "", -1, "", err
	}
	defer os.RemoveAll(tmp)
	home := filepath.Join(tmp, "home")
	bare := filepath.Join(tmp, "repo.git")
	if err := os.Mkdir(home, 0o700); err != nil {
		return "", -1, "", err
	}
	env := append(daemonGitEnv(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"HOME="+home, "XDG_CONFIG_HOME="+home, "GIT_ATTR_NOSYSTEM=1")
	run := func(dir string, args ...string) (string, int, string, error) {
		return gitEnvCtx(ctx, env, dir, args...)
	}
	if _, code, stderr, err := run(tmp, "init", "-q", "--bare", "--template=", "--object-format="+format, bare); err != nil || code != 0 {
		if err == nil {
			err = fmt.Errorf("git init exited %d: %s", code, stderr)
		}
		return "", -1, "", err
	}
	alternates := filepath.Join(bare, "objects", "info", "alternates")
	if err := os.MkdirAll(filepath.Dir(alternates), 0o700); err != nil {
		return "", -1, "", err
	}
	if err := os.WriteFile(alternates, []byte(filepath.Join(common, "objects")+"\n"), 0o600); err != nil {
		return "", -1, "", err
	}
	return run(bare, "-c", "core.attributesFile="+os.DevNull, "merge-tree", "--write-tree", "--name-only", "--no-messages", tip, head)
}

// gitCtxOK is gitCtx for a call that must exit 0: its trimmed stdout, else an error.
func gitCtxOK(ctx context.Context, dir string, args ...string) (string, error) {
	out, code, stderr, err := gitCtx(ctx, dir, args...)
	if err == nil && code != 0 {
		err = fmt.Errorf("git %s exited %d: %s", args[0], code, stderr)
	}
	return strings.TrimSpace(out), err
}

// MergeNote is the job-done line's merge check of a repository job with commits beyond its base:
// whether `git merge-tree --write-tree` (objects only, no ref moves) merges the job's head onto the
// current tip of its base branch, within MergeCheckTimeout. "" for a job without a repository, a
// worktree or commits of its own; a job whose head is still its base records that finding in the
// cache instead (noCommitsOverBase reuses it, no git call of its own). A merge-tree result is cached
// against the job's head and the branch tip it was checked at (until the job is notified); a failed
// or timed out check is not. The zero-commits finding (#132) is always the latest check's own: every
// check clears it first (clearNoCommits) and records it again only when it sees the head at the base
// itself, so a retry whose check fails (a timeout, a missing branch) never flags a job that
// committed meanwhile.
func (m *Manager) MergeNote(j *store.Job) string {
	// The zero-commits finding (#132) never survives into a new check: it is set again below only
	// when this check positively sees the head at the base commit.
	m.clearNoCommits(j.ID)
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
	run := func(dir string, args ...string) (string, error) { return gitCtxOK(ctx, dir, args...) }
	head, err := run(work, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return notChecked(err.Error())
	}
	if head == j.BaseCommit {
		m.cacheNoCommits(j.ID, head)
		return ""
	}
	ahead, err := run(j.RepoPath, "rev-list", "--count", j.BaseCommit+".."+head)
	if err != nil {
		return notChecked(err.Error())
	}
	if ahead == "0" {
		m.cacheNoCommits(j.ID, head)
		return ""
	}
	// rev-list confirmed commits over the base: the fast-empty note (#132) can never fire on this
	// check, whatever the branch tip lookup or the merge itself does next.
	m.cacheHasCommits(j.ID, head)
	if j.BaseBranch == "" {
		return notChecked("the job's base is not a branch (a tag, a commit or a detached HEAD)")
	}
	tip, err := run(j.RepoPath, "rev-parse", "--verify", "--end-of-options", j.BaseBranch+"^{commit}")
	if err != nil {
		return notChecked("branch " + shortBranch(j.BaseBranch) + " not found")
	}
	if note, ok := m.cachedMergeNote(j.ID, head, tip); ok {
		return note
	}
	out, code, stderr, err := isolatedMergeTree(ctx, j.RepoPath, tip, head)
	switch {
	case err != nil:
		return notChecked(err.Error())
	case code == 0:
		note := fmt.Sprintf("merges cleanly onto %s at %s", Printable(branch, 80), shortSHA(tip))
		m.cacheMergeNote(j.ID, head, tip, note)
		return note
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
			files = append(files, noteFileName(f, mergeCheckMaxName))
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
	m.cacheMergeNote(j.ID, head, tip, note)
	return note
}

// mergeCheck is a cached merge-tree result: the note for the job's head merged onto tip, or — with
// noCommits set — that the head was still the base commit, so the job has no commits of its own
// (tip is then empty).
type mergeCheck struct {
	head, tip, note string
	noCommits       bool
}

func (m *Manager) cachedMergeNote(id, head, tip string) (string, bool) {
	m.mergeMu.Lock()
	defer m.mergeMu.Unlock()
	c, ok := m.mergeChecks[id]
	if !ok || c.head != head || c.tip != tip {
		return "", false
	}
	return c.note, true
}

func (m *Manager) cacheMergeNote(id, head, tip, note string) {
	m.mergeMu.Lock()
	defer m.mergeMu.Unlock()
	if m.mergeChecks == nil {
		m.mergeChecks = map[string]mergeCheck{}
	}
	m.mergeChecks[id] = mergeCheck{head: head, tip: tip, note: note}
}

// cacheNoCommits records that the job's head is its base commit — no commits of its own — for the
// job-done line's suspicious note (#132). It lives like a merge note, until the job is notified;
// each check clears it first (clearNoCommits) and sets it again only from its own finding.
func (m *Manager) cacheNoCommits(id, head string) {
	m.mergeMu.Lock()
	defer m.mergeMu.Unlock()
	if m.mergeChecks == nil {
		m.mergeChecks = map[string]mergeCheck{}
	}
	m.mergeChecks[id] = mergeCheck{head: head, noCommits: true}
}

// cacheHasCommits records the opposite of cacheNoCommits (#132): this check's rev-list saw commits
// over the base, so the fast-empty note cannot fire on it — whatever the branch tip lookup or the
// merge itself does next.
func (m *Manager) cacheHasCommits(id, head string) {
	m.mergeMu.Lock()
	defer m.mergeMu.Unlock()
	if m.mergeChecks == nil {
		m.mergeChecks = map[string]mergeCheck{}
	}
	c, ok := m.mergeChecks[id]
	if !ok {
		c = mergeCheck{head: head}
	}
	c.noCommits = false
	m.mergeChecks[id] = c
}

// clearNoCommits drops the job's cached zero-commits finding (#132) without touching its cached
// merge note. Every merge check starts with it clear, and a requeue (resume) clears it too: the
// fast-empty note fires only on a finding the latest check confirmed itself.
func (m *Manager) clearNoCommits(id string) {
	m.mergeMu.Lock()
	defer m.mergeMu.Unlock()
	if c, ok := m.mergeChecks[id]; ok && c.noCommits {
		c.noCommits = false
		m.mergeChecks[id] = c
	}
}

// noCommitsOverBase reports whether the merge check found the job with no commits over its base
// (its head still the base commit): the check's own finding, no git call of its own. ok is false
// when it is not known — no repository or worktree, or a check that did not run (or could not) —
// so the suspicious note is never added on a guess.
func (m *Manager) noCommitsOverBase(j *store.Job) (no, ok bool) {
	m.mergeMu.Lock()
	defer m.mergeMu.Unlock()
	c, ok := m.mergeChecks[j.ID]
	return c.noCommits, ok
}

// forgetMergeNote drops the cached merge check of a job that has been notified.
func (m *Manager) forgetMergeNote(id string) {
	m.mergeMu.Lock()
	defer m.mergeMu.Unlock()
	delete(m.mergeChecks, id)
}

// noteFileNameRune reports whether r may stand in a file name of a job-done note: an ASCII letter, a
// digit or one of ". _ / -".
func noteFileNameRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
		r == '.' || r == '_' || r == '/' || r == '-'
}

// noteFileName is a file's name as a note of the job-done line shows it (the UI note's changed files,
// the merge note's conflicted files). A file name is the job's own text, so it passes an allowlist
// instead of an escape per separator: every rune that is not noteFileNameRune (a space, ";", "·", ":",
// a quote, a bracket, a control character, a non-ASCII letter, a broken byte) becomes "_". What is
// left cannot spell a separator of the job-done formats: the fields' " · ", the "; " between the jobs
// of a coalesced line, the ", " and " (+k more)" of the list. Redacted first (a value changed by the
// allowlist no longer matches its format), then cut to max bytes, a cut name ending in "...".
func noteFileName(name string, max int) string {
	out := strings.Map(func(r rune) rune {
		if noteFileNameRune(r) {
			return r
		}
		return '_'
	}, strings.ToValidUTF8(RedactText(name), "_"))
	if len(out) > max {
		out = out[:max-3] + "..."
	}
	return out
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
