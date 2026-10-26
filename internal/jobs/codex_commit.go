package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// Codex write jobs and git (issue #92). Under workspace-write codex keeps the repository's git dirs
// read-only: the worktree's own admin dir (.git/worktrees/<name>, which holds the index, so not even
// `git add` works; codex protects the gitdir a .git file points to) and the common dir, whose objects a
// commit writes. Opening the admin dir as a writable root (--add-dir) would not be enough, and opening the
// common dir would let the job rewrite the refs and objects of every branch of the owner's repository.
// So the job leaves its changes in work/ and the wrapper commits them after the run, outside the sandbox.

// CodexCommitMessageFile is the file in a codex write job's progress folder (CodexProgressDir) whose text
// becomes the message of the commit the wrapper makes of the job's changes.
const CodexCommitMessageFile = "COMMIT_MSG"

// codexCommitMessageMax caps the bytes read from CodexCommitMessageFile.
const codexCommitMessageMax = 8 << 10

// codexCommitTimeout bounds each git command of commitCodexWork.
var codexCommitTimeout = 2 * time.Minute

// CodexCommitMessagePath is the commit message file of a codex write job in jobDir, absolute.
func CodexCommitMessagePath(jobDir string) string {
	return filepath.Join(absPath(jobDir), CodexProgressDir, CodexCommitMessageFile)
}

// commitCodexWork commits what a codex write job left in its worktree, once, after the run and after
// the job's processes are gone: everything is staged (`git add -A`: untracked files too, ignored ones
// not) and committed with the job's git environment (GitEnv: the job identity, no hooks, fsmonitor or
// other programs; commit.gpgSign off) on the worktree's admin dir recorded at creation (worktree.json),
// never through a .git file in work/. The message is the job's CodexCommitMessageFile when it is a
// regular file with text and without an owner name (config owner_names of <state>, jobDir being
// <state>/jobs/<id>), else a default. A repository config that names filter or diff drivers (git add
// would run them here) gets no commit, and neither does a run whose git command fails: the changes then
// stay uncommitted and Cleanup saves them as patches. It returns what it did, for the wrapper's log
// ("" = the job has no worktree).
func commitCodexWork(jobDir, id string) string {
	abs := absPath(jobDir)
	b, err := os.ReadFile(filepath.Join(abs, "worktree.json"))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	var info WorktreeInfo
	if err == nil {
		err = json.Unmarshal(b, &info)
	}
	work := filepath.Join(abs, "work")
	if err == nil && (info.GitDir == "" || !filepath.IsAbs(info.GitDir) || !samePath(info.Path, work)) {
		err = errors.New("does not name this job's work dir and an absolute git dir")
	}
	if err != nil {
		return fmt.Sprintf("codex commit skipped: worktree.json: %v", err)
	}
	env, err := GitEnv(abs, os.Environ())
	if err != nil {
		return fmt.Sprintf("codex commit skipped: git environment: %v", err)
	}
	run := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), codexCommitTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + info.GitDir, "--work-tree=" + work}, args...)...)
		cmd.Dir, cmd.Env = work, env
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			sub := args[0]
			for i := 0; i+2 < len(args) && args[i] == "-c"; i += 2 {
				sub = args[i+2]
			}
			return "", fmt.Errorf("git %s: %w: %s", sub, err, Printable(stderr.String(), 200))
		}
		return string(out), nil
	}
	cfg, err := run(repoDriversArgs...)
	if err != nil {
		return "codex commit skipped: " + err.Error()
	}
	if drivers := driverKeys([]byte(cfg)); len(drivers) > 0 {
		return "codex commit skipped: the repository config names filter/diff drivers (" + strings.Join(drivers, ", ") + ")"
	}
	status, err := run("status", "--porcelain", "--untracked-files=all", "-z")
	if err != nil {
		return "codex commit skipped: " + err.Error()
	}
	if status == "" {
		return "codex commit: no changes in the worktree"
	}
	if _, err := run("add", "-A"); err != nil {
		return "codex commit failed, changes left uncommitted: " + err.Error()
	}
	c, cfgErr := LoadConfig(filepath.Dir(filepath.Dir(abs)))
	msg := codexCommitMessage(abs, id, c.OwnerNames, cfgErr == nil)
	commit := []string{"-c", "commit.gpgSign=false"}
	// The job identity (git_name / git_email) also over a repository-local user.*, as the job's own
	// gitconfig cannot.
	if c.GitName != "" {
		commit = append(commit, "-c", "user.name="+c.GitName)
	}
	if c.GitEmail != "" {
		commit = append(commit, "-c", "user.email="+c.GitEmail)
	}
	if _, err := run(append(commit, "commit", "-q", "--no-verify", "--cleanup=strip", "-m", msg)...); err != nil {
		return "codex commit failed, changes left staged: " + err.Error()
	}
	head, err := run("rev-parse", "HEAD")
	if err != nil {
		return "codex commit made, HEAD unreadable: " + err.Error()
	}
	return "codex commit: committed the job's changes as " + strings.TrimSpace(head)
}

// codexCommitMessage is the commit message of a codex write job: its CodexCommitMessageFile (a regular
// file, not followed if a link; control characters dropped, tabs kept) unless that is missing, empty,
// unreadable or names one of ownerNames, or the owner names are not known (namesKnown false: the config
// could not be read); else a default naming the job.
func codexCommitMessage(jobDir, id string, ownerNames []string, namesKnown bool) string {
	def := fmt.Sprintf("Codex job %s: changes committed by the wrapper\n\nThe job left no usable %s/%s; its "+
		"changes were committed as they were when it ended.", id, CodexProgressDir, CodexCommitMessageFile)
	text, err := readSmall(CodexCommitMessagePath(jobDir), codexCommitMessageMax)
	if err != nil || !namesKnown {
		return def
	}
	text = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || unicode.IsPrint(r) {
			return r
		}
		return -1
	}, strings.ToValidUTF8(text, ""))
	if strings.TrimSpace(text) == "" {
		return def
	}
	low := strings.ToLower(text)
	for _, name := range ownerNames {
		if n := strings.ToLower(strings.TrimSpace(name)); n != "" && strings.Contains(low, n) {
			return def
		}
	}
	return text
}
