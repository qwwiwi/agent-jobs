package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
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

// codexCommitTimeout bounds the whole of commitCodexWork: the worktree scan and every git command.
var codexCommitTimeout = 2 * time.Minute

// codexCommitLimits bound the changes the wrapper stages of a codex write job. `git add` writes every
// staged file into the object database shared with the owner's repository, so a job that leaves huge
// (or sparse) files, or a huge number of them, could fill the disk or the memory of the wrapper's git.
type codexCommitLimits struct {
	files      int   // changed paths (a rename counts both)
	fileBytes  int64 // size of one changed file, by Lstat (its content is never read)
	totalBytes int64 // sum of those sizes
}

// codexCommitMax are the limits of commitCodexWork; past any of them the job's changes are not committed.
var codexCommitMax = codexCommitLimits{files: 2000, fileBytes: 20 << 20, totalBytes: 200 << 20}

// codexCommitOutputMax caps the stdout of a commitCodexWork git command other than its status.
const codexCommitOutputMax = 1 << 20

// codexStatusEntryMax bounds one record of `git status --porcelain=v2 -z`: its header and two paths
// (a rename's), each at most PATH_MAX.
const codexStatusEntryMax = 256 + 2*4096

// CodexCommitMessagePath is the commit message file of a codex write job in jobDir, absolute.
func CodexCommitMessagePath(jobDir string) string {
	return filepath.Join(absPath(jobDir), CodexProgressDir, CodexCommitMessageFile)
}

// commitCodexWork commits what a codex write job left in its worktree, once, after the run and after
// the job's processes are gone: everything is staged (`git add -A`: untracked files too, ignored ones
// not) when the changes, listed by a status whose output is capped, are within codexCommitMax, and
// committed with the job's git environment (GitEnv: no hooks, fsmonitor or other programs;
// commit.gpgSign off) and the job identity of the launch (codexJobIdentity) as author and committer, on
// the worktree's admin dir recorded at creation (worktree.json), never through a .git file in work/.
// The message is the job's CodexCommitMessageFile when it is a regular file with text and without an
// owner name (config owner_names of <state>, jobDir being <state>/jobs/<id>), else a default. A
// repository config that names filter or diff drivers (git add would run them here) gets no commit, and
// neither do a launch without both identity fields, changes over the limits, a step past
// codexCommitTimeout or a git command that fails: the changes then stay uncommitted and Cleanup saves
// them as patches. It returns what it did, for the wrapper's log ("" = the job has no worktree).
func commitCodexWork(jobDir, id string) string {
	ctx, cancel := context.WithTimeout(context.Background(), codexCommitTimeout)
	defer cancel()
	line := commitCodexWorkCtx(ctx, jobDir, id)
	if ctx.Err() == nil {
		return line
	}
	deadline := fmt.Sprintf("the commit step passed its %s deadline", codexCommitTimeout)
	if strings.HasPrefix(line, "codex commit skipped: ") {
		return "codex commit skipped: " + deadline
	}
	return line + " (" + deadline + ")"
}

// commitCodexWorkCtx is commitCodexWork with every step bound to ctx.
func commitCodexWorkCtx(ctx context.Context, jobDir, id string) string {
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
	// Before any git command: git runs a submodule's status (and so its filter drivers) with the
	// submodule's own config and without GitEnv's command-level overrides.
	if nested, err := nestedGitDir(ctx, work); err != nil {
		return fmt.Sprintf("codex commit skipped: worktree scan: %v", err)
	} else if nested != "" {
		return "codex commit skipped: the worktree holds a nested git repository (" + nested + ")"
	}
	// Before GitEnv, which would create a missing gitconfig empty.
	name, email, err := codexJobIdentity(ctx, abs)
	if err != nil {
		return "codex commit skipped: the job's git identity: " + err.Error()
	}
	env, err := GitEnv(abs, os.Environ())
	if err != nil {
		return fmt.Sprintf("codex commit skipped: git environment: %v", err)
	}
	// runCapped runs git with at most limit bytes of stdout: past them the output pipe closes, so git stops.
	runCapped := func(limit int, extraEnv []string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + info.GitDir, "--work-tree=" + work}, args...)...)
		cmd.Dir, cmd.Env = work, append(slices.Clip(env), extraEnv...)
		var stderr bytes.Buffer
		stdout := &cappedOutput{max: limit}
		cmd.Stdout, cmd.Stderr = stdout, &limitedWriter{w: &stderr, n: 4 << 10}
		err := cmd.Run()
		sub := args[0]
		for i := 0; i+2 < len(args) && args[i] == "-c"; i += 2 {
			sub = args[i+2]
		}
		if stdout.over {
			return "", fmt.Errorf("git %s: output over %d bytes", sub, limit)
		}
		if err != nil {
			return "", fmt.Errorf("git %s: %w: %s", sub, err, Printable(stderr.String(), 200))
		}
		return stdout.buf.String(), nil
	}
	run := func(args ...string) (string, error) { return runCapped(codexCommitOutputMax, nil, args...) }
	cfg, err := run(repoDriversArgs...)
	if err != nil {
		return "codex commit skipped: " + err.Error()
	}
	if drivers := driverKeys([]byte(cfg)); len(drivers) > 0 {
		return "codex commit skipped: the repository config names filter/diff drivers (" + strings.Join(drivers, ", ") + ")"
	}
	limits := codexCommitMax
	status, err := runCapped((limits.files+1)*codexStatusEntryMax, nil,
		"status", "--porcelain=v2", "--untracked-files=all", "--ignore-submodules=all", "-z")
	if err != nil {
		return "codex commit skipped: " + err.Error()
	}
	if status == "" {
		return "codex commit: no changes in the worktree"
	}
	paths, err := statusPaths(status)
	if err != nil {
		return "codex commit skipped: " + err.Error()
	}
	if reason := limits.check(work, paths); reason != "" {
		return "codex commit skipped: " + reason
	}
	if _, err := run("add", "-A"); err != nil {
		return "codex commit failed, changes left uncommitted: " + err.Error()
	}
	c, cfgErr := LoadConfig(filepath.Dir(filepath.Dir(abs)))
	msg := codexCommitMessage(abs, id, c.OwnerNames, cfgErr == nil)
	// The launch's job identity as author and committer: the environment beats a repository-local
	// user.* (and author.* / committer.*), which the job's gitconfig as GIT_CONFIG_GLOBAL cannot.
	identity := []string{"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email}
	if _, err := runCapped(codexCommitOutputMax, identity,
		"-c", "commit.gpgSign=false", "commit", "-q", "--no-verify", "--cleanup=strip", "-m", msg); err != nil {
		return "codex commit failed, changes left staged: " + err.Error()
	}
	head, err := run("rev-parse", "HEAD")
	if err != nil {
		return "codex commit made, HEAD unreadable: " + err.Error()
	}
	return "codex commit: committed the job's changes as " + strings.TrimSpace(head)
}

// codexJobIdentity is the job identity of the launch: user.name and user.email of <jobDir>/gitconfig,
// which prepare wrote from config git_name / git_email and a workspace-write job cannot change (its job
// dir is not a writable root). Both must be set; a later config.json is not consulted.
func codexJobIdentity(ctx context.Context, jobDir string) (name, email string, err error) {
	path := filepath.Join(jobDir, gitConfigFile)
	fi, err := os.Lstat(path)
	if err != nil {
		return "", "", err
	}
	if !fi.Mode().IsRegular() {
		return "", "", fmt.Errorf("%s is not a regular file", gitConfigFile)
	}
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "GIT_") })
	env = append(env, "GIT_CONFIG_NOSYSTEM=1")
	get := func(key string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", "config", "--file", path, "--get", key)
		out := &cappedOutput{max: 4 << 10}
		cmd.Dir, cmd.Env, cmd.Stdout = jobDir, env, out
		err := cmd.Run()
		var exit *exec.ExitError
		switch {
		case out.over:
			return "", fmt.Errorf("%s over %d bytes", key, out.max)
		case errors.As(err, &exit) && exit.ExitCode() == 1: // the key is not set
			return "", nil
		case err != nil:
			return "", fmt.Errorf("read %s from %s: %w", key, gitConfigFile, err)
		}
		return strings.TrimSuffix(out.buf.String(), "\n"), nil
	}
	if name, err = get("user.name"); err != nil {
		return "", "", err
	}
	if email, err = get("user.email"); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(name) == "" || strings.TrimSpace(email) == "" {
		return "", "", fmt.Errorf("the launch's %s does not set both user.name and user.email (config git_name / git_email)", gitConfigFile)
	}
	return name, email, nil
}

// codexScanBatch is how many entries of a directory nestedGitDir reads at a time.
var codexScanBatch = 256

// codexScanMax caps the entries nestedGitDir visits in all; past it the job's changes are not committed.
var codexScanMax = 200000

// nestedGitDir returns the first `.git` (a dir, a file or a link) it meets below work other than
// work/.git itself, which it does not look into, relative to work and slash-separated, or "". Links are
// not followed. It reads each directory codexScanBatch entries at a time, checks ctx before each batch and
// fails past codexScanMax entries in all, so a job that left millions of entries in one directory costs
// the wrapper neither its memory nor time past the commit deadline.
func nestedGitDir(ctx context.Context, work string) (string, error) {
	// A directory still to read: its name and its parent's, nil being work (names, not whole paths, so
	// the pending ones cost at most a name each).
	type dir struct {
		parent *dir
		name   string
	}
	var rel func(d *dir) string
	rel = func(d *dir) string {
		if d == nil {
			return ""
		}
		return path.Join(rel(d.parent), d.name)
	}
	visited := 0
	pending := []*dir{nil} // depth first
	// read reads directory d: the nested .git it holds, if any; its subdirectories go to pending.
	read := func(d *dir) (string, error) {
		f, err := os.Open(filepath.Join(work, filepath.FromSlash(rel(d))))
		if err != nil {
			return "", err
		}
		defer f.Close()
		for {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			entries, err := f.ReadDir(codexScanBatch)
			for _, e := range entries {
				if visited++; visited > codexScanMax {
					return "", fmt.Errorf("over the limit of %d entries", codexScanMax)
				}
				switch {
				case e.Name() == ".git" && d == nil: // work/.git itself
				case e.Name() == ".git":
					return path.Join(rel(d), e.Name()), nil
				case e.IsDir(): // a link to a directory is not one
					pending = append(pending, &dir{parent: d, name: e.Name()})
				}
			}
			if errors.Is(err, io.EOF) {
				return "", nil
			} else if err != nil {
				return "", err
			}
		}
	}
	for len(pending) > 0 {
		d := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if found, err := read(d); err != nil || found != "" {
			return found, err
		}
	}
	return "", nil
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

// statusPaths are the paths of `git status --porcelain=v2 -z` output, relative to the worktree: each
// changed, unmerged or untracked entry's path, and a rename's or copy's source too.
func statusPaths(out string) ([]string, error) {
	recs := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	var paths []string
	for i := 0; i < len(recs); i++ {
		rec := recs[i]
		var fields int // space-separated fields before the path
		switch {
		case strings.HasPrefix(rec, "1 "):
			fields = 8
		case strings.HasPrefix(rec, "2 "):
			fields = 9
		case strings.HasPrefix(rec, "u "):
			fields = 10
		case strings.HasPrefix(rec, "? "), strings.HasPrefix(rec, "! "):
			fields = 1
		case strings.HasPrefix(rec, "# "):
			continue
		default:
			return nil, fmt.Errorf("git status: unknown entry %q", Printable(rec, 80))
		}
		parts := strings.SplitN(rec, " ", fields+1)
		if len(parts) != fields+1 || parts[fields] == "" {
			return nil, fmt.Errorf("git status: malformed entry %q", Printable(rec, 80))
		}
		paths = append(paths, parts[fields])
		if rec[0] == '2' {
			// The source path is the next NUL-separated field.
			if i++; i >= len(recs) {
				return nil, errors.New("git status: rename without its source path")
			}
			paths = append(paths, recs[i])
		}
	}
	return paths, nil
}

// check returns why the changed paths (relative to work) are over the limits, or "". Sizes are Lstat
// sizes, the content is never read: a link counts as itself, a sparse file in full, a deleted path as 0.
func (l codexCommitLimits) check(work string, paths []string) string {
	if len(paths) > l.files {
		return fmt.Sprintf("%d changed paths, over the limit of %d", len(paths), l.files)
	}
	var total int64
	for _, p := range paths {
		fi, err := os.Lstat(filepath.Join(work, filepath.FromSlash(p)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Sprintf("cannot stat %s: %v", Printable(p, 200), err)
		}
		if fi.IsDir() {
			continue
		}
		if fi.Size() > l.fileBytes {
			return fmt.Sprintf("%s has %d bytes, over the limit of %d per file", Printable(p, 200), fi.Size(), l.fileBytes)
		}
		total += fi.Size()
	}
	if total > l.totalBytes {
		return fmt.Sprintf("the changed files have %d bytes, over the limit of %d in all", total, l.totalBytes)
	}
	return ""
}

// cappedOutput keeps a command's stdout up to max bytes; a write past them fails (exec then closes the
// pipe, so the command gets EPIPE and stops) and sets over.
type cappedOutput struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *cappedOutput) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.max {
		c.over = true
		return 0, errors.New("output over its cap")
	}
	return c.buf.Write(p)
}
