package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Restack moves a stacked branch onto the new main after its parent was squash-merged. Branch C was
// built on P; P landed on main as one new commit S, so C still carries P's original commits and
// merging it conflicts on code that is identical in substance. Restack builds a NEW branch from onto
// and cherry-picks only the child's own commits (parent..child) on it.
//
// It is store-free and create-only, in the spirit of the result branches (branch_refs.go): it never
// moves, deletes or checks out an existing ref, never touches child, parent or the repository's own
// work tree, and never pushes. It refuses rather than guesses: when P's change is not provably in
// onto, when parent is not an ancestor of child (the own commits cannot be told apart), when the
// child's own history has a merge commit, when newName is taken, and on any cherry-pick conflict
// (the error names the files; nothing is left behind).

// ErrRestackNotMerged is returned when the change of the parent is not contained in onto.
var ErrRestackNotMerged = errors.New("parent's change is not in the target branch")

// RestackConflictError is a cherry-pick that conflicted: the commit and the files in conflict. The
// temporary worktree is already gone and no ref was created.
type RestackConflictError struct {
	Commit string   // short ID and subject of the commit that did not apply
	Files  []string // paths in conflict
}

func (e *RestackConflictError) Error() string {
	return fmt.Sprintf("cherry-pick of %s conflicts in %s; nothing was created, resolve it by hand",
		e.Commit, strings.Join(e.Files, ", "))
}

// RestackResult is a finished restack.
type RestackResult struct {
	Branch  string   `json:"branch"`            // the new branch
	Head    string   `json:"head"`              // its tip, full commit ID
	Onto    string   `json:"onto"`              // the commit it starts from, full commit ID
	Commits int      `json:"commits"`           // the child's commits carried over
	Carried []string `json:"carried,omitempty"` // their original IDs, oldest first
	Skipped []string `json:"skipped,omitempty"` // original IDs that became empty on onto (already there)
	// Warnings are problems that did not stop the restack (the temporary directory could not be removed).
	Warnings []string `json:"warnings,omitempty"`
}

// restackTimeout bounds one Restack; a variable so that tests can shorten it.
var restackTimeout = 10 * time.Minute

// restackPickConfig, when set, returns extra "key=value" git config for the cherry-pick of one commit
// (passed as -c). A test seam: it lets a test make a single pick fail after the index was updated.
var restackPickConfig func(commit string) []string

// restackCleanupTimeout bounds the removal of the temporary directory; a variable for tests.
var restackCleanupTimeout = time.Minute

// Restack creates newName (default <child>-restacked) from onto (default main) in repo and
// cherry-picks the commits of parent..child on it. See the package comment above. A failure to remove
// the temporary directory is appended to the error, or listed in RestackResult.Warnings on success.
func Restack(repo, child, parent, onto, newName string) (res RestackResult, err error) {
	if repo == "" || child == "" || parent == "" {
		return RestackResult{}, errors.New("restack needs a repository, a child and a parent")
	}
	if onto == "" {
		onto = "main"
	}
	if newName == "" {
		newName = strings.TrimPrefix(child, "refs/heads/") + "-restacked"
	}
	ctx, cancel := context.WithTimeout(context.Background(), restackTimeout)
	defer cancel()
	r := &restackGit{ctx: ctx, env: append(daemonGitEnv(), "GIT_GRAFT_FILE="+os.DevNull)}
	if _, err := r.in(repo, "rev-parse", "--git-dir"); err != nil {
		return RestackResult{}, fmt.Errorf("%s is not a git repository: %w", repo, err)
	}
	run := branchGit(func(args ...string) (string, error) { return r.in(repo, args...) })

	// The name first: a taken or invalid name must cost nothing.
	if err := validateResultBranch(run, newName); err != nil {
		return RestackResult{}, err
	}
	if exists, _ := existingRef(run, "refs/heads/"+newName, ""); exists {
		return RestackResult{}, fmt.Errorf("%w: %s (restack never moves an existing branch; pass --name)", errBranchExists, newName)
	}
	resolve := func(role, ref string) (string, error) {
		sha, err := r.in(repo, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
		if err != nil {
			return "", fmt.Errorf("%s %q is not a commit in %s", role, ref, repo)
		}
		return sha, nil
	}
	childSHA, err := resolve("child", child)
	if err != nil {
		return RestackResult{}, err
	}
	parentSHA, err := resolve("parent", parent)
	if err != nil {
		return RestackResult{}, err
	}
	ontoSHA, err := resolve("onto", onto)
	if err != nil {
		return RestackResult{}, err
	}

	// The child's own commits.
	if _, err := r.in(repo, "merge-base", "--is-ancestor", parentSHA, childSHA); err != nil {
		return RestackResult{}, fmt.Errorf("parent %s is not an ancestor of child %s: the child's own commits cannot be told apart from the parent's", parent, child)
	}
	rng := parentSHA + ".." + childSHA
	if merges, err := r.in(repo, "rev-list", "--merges", rng); err != nil {
		return RestackResult{}, err
	} else if merges != "" {
		return RestackResult{}, fmt.Errorf("%s has a merge commit among its own commits (%s); restack carries linear history only", child, shortSHA(strings.Fields(merges)[0]))
	}
	list, err := r.in(repo, "rev-list", "--reverse", "--topo-order", rng)
	if err != nil {
		return RestackResult{}, err
	}
	if list == "" {
		return RestackResult{}, fmt.Errorf("child %s has no commits beyond parent %s: nothing to restack", child, parent)
	}
	commits := strings.Fields(list)

	if err := r.parentInOnto(repo, parent, parentSHA, onto, ontoSHA); err != nil {
		return RestackResult{}, err
	}

	// Cherry-pick in a throw-away repository of its own (see newReplay) on a detached HEAD: no ref
	// exists in the real repository until the very end.
	tmp, err := os.MkdirTemp("", "agent-jobs-restack-")
	if err != nil {
		return RestackResult{}, err
	}
	defer func() {
		// Bounded on its own clock, not by ctx: it must also run after a timeout, but not forever.
		if rmErr := removeAllWithin(tmp, restackCleanupTimeout); rmErr != nil {
			if err != nil {
				err = fmt.Errorf("%w (cleanup: %v)", err, rmErr)
			} else {
				res.Warnings = append(res.Warnings, rmErr.Error())
			}
		}
	}()
	replay, work, err := r.newReplay(repo, tmp, ontoSHA)
	if err != nil {
		return RestackResult{}, err
	}

	res = RestackResult{Branch: newName, Onto: ontoSHA}
	for _, c := range commits {
		pick := []string{}
		if restackPickConfig != nil {
			for _, kv := range restackPickConfig(c) {
				pick = append(pick, "-c", kv)
			}
		}
		if _, err := replay.in(work, append(pick, "cherry-pick", "--allow-empty", "--no-edit", c)...); err != nil {
			if err := replay.settleCherryPick(work, c, err); err != nil {
				return RestackResult{}, err
			}
			res.Skipped = append(res.Skipped, c)
			continue
		}
		res.Carried = append(res.Carried, c)
	}
	res.Commits = len(res.Carried)
	if res.Commits == 0 {
		return RestackResult{}, fmt.Errorf("every commit of %s is already in %s: nothing to restack", child, onto)
	}
	head, err := replay.in(work, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return RestackResult{}, err
	}
	// Create-only and atomic: a branch that appeared meanwhile is errBranchExists, never overwritten.
	// created == false means the name already holds exactly this head (a concurrent identical restack):
	// that is still a taken name, not a result of this call.
	created, pubErr := publishBranchRef(run, newName, head)
	if pubErr == nil && !created {
		pubErr = errBranchExists
	}
	if pubErr != nil {
		if errors.Is(pubErr, errBranchExists) {
			pubErr = fmt.Errorf("%w: %s (restack never moves an existing branch; pass --name)", errBranchExists, newName)
		}
		return RestackResult{}, pubErr
	}
	res.Head = head
	return res, nil
}

// removeAllWithin removes dir (os.RemoveAll) and gives up reporting an error after limit. The removal
// goroutine may outlive a timeout; it only deletes a private temporary directory.
func removeAllWithin(dir string, limit time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- os.RemoveAll(dir) }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("remove temporary directory %s: %w", dir, err)
		}
		return nil
	case <-time.After(limit):
		return fmt.Errorf("remove temporary directory %s: not finished after %s", dir, limit)
	}
}

// newReplay prepares the throw-away repository the cherry-picks run in and returns a git runner for it
// and its work tree, checked out detached at ontoSHA.
//
// It is NOT a worktree of repo: a worktree reads the repository's config and .git/info/attributes, so a
// configured merge.<driver>.driver, filter.<name>.clean/smudge or diff textconv would execute during the
// pick, and those cannot be switched off by an override (the names are not enumerable). Instead it is a
// fresh repository with an empty config (no system or global config, an empty HOME, no template, so
// no hooks), whose object database is repo's own (GIT_OBJECT_DIRECTORY): the commits are readable and
// the picked commits are written where publishing needs them. A .gitattributes in the tree that names
// a driver or filter finds none defined and falls back to the built-in text merge and no conversion.
// Only the committer identity is carried over from repo (read, never executed).
func (r *restackGit) newReplay(repo, tmp, ontoSHA string) (*restackGit, string, error) {
	common, err := r.in(repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, "", err
	}
	format, err := r.in(repo, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, "", err
	}
	home, work := filepath.Join(tmp, "home"), filepath.Join(tmp, "work")
	if err := os.Mkdir(home, 0o700); err != nil {
		return nil, "", err
	}
	env := append(slices.Clone(r.env), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"HOME="+home, "XDG_CONFIG_HOME="+home, "GIT_ATTR_NOSYSTEM=1")
	name, _ := r.in(repo, "config", "--get", "user.name")
	email, _ := r.in(repo, "config", "--get", "user.email")
	if name == "" || email == "" {
		name, email = "agent-jobs", "agent-jobs@localhost"
	}
	replay := &restackGit{ctx: r.ctx, env: env}
	if _, err := replay.in(tmp, "init", "-q", "--template=", "--object-format="+format, work); err != nil {
		return nil, "", fmt.Errorf("create temporary repository: %w", err)
	}
	replay.env = append(replay.env, "GIT_OBJECT_DIRECTORY="+filepath.Join(common, "objects"),
		"GIT_COMMITTER_NAME="+name, "GIT_COMMITTER_EMAIL="+email)
	if _, err := replay.in(work, "checkout", "-q", "--detach", ontoSHA); err != nil {
		return nil, "", fmt.Errorf("check out %s in the temporary repository: %w", shortSHA(ontoSHA), err)
	}
	return replay, work, nil
}

// restackGit runs git in one context with one environment.
type restackGit struct {
	ctx context.Context
	env []string
}

// in runs git in dir and returns trimmed stdout; a non-zero exit is an error carrying stderr.
func (r *restackGit) in(dir string, args ...string) (string, error) {
	out, code, stderr, err := gitEnvCtx(r.ctx, r.env, dir, append([]string{noReplace}, args...)...)
	if err == nil && code != 0 {
		err = fmt.Errorf("git %s exited %d: %s", args[0], code, RedactPrintable(stderr, 240))
	}
	return strings.TrimSpace(out), err
}

// settleCherryPick handles a failed cherry-pick of commit c in work. A pick that stopped because the
// commit became empty on onto (its change is already there) is skipped and returns nil; a conflict is
// aborted and returned as *RestackConflictError; anything else is returned as it is.
//
// "Empty" is proven, not assumed: CHERRY_PICK_HEAD is also left behind when the commit step fails after
// the changes were staged (a signing failure, a full disk), and skipping that would silently drop the
// commit. So the pick is skipped only when no path is unmerged and the index equals HEAD.
func (r *restackGit) settleCherryPick(work, c string, pickErr error) error {
	unmerged, _ := r.in(work, "ls-files", "-u", "-z")
	if files := unmergedPaths(unmerged); len(files) > 0 {
		_, _ = r.in(work, "cherry-pick", "--abort")
		subject, _ := r.in(work, "log", "-1", "--format=%s", c)
		return &RestackConflictError{Commit: shortSHA(c) + " " + Printable(subject, 80), Files: files}
	}
	failed := fmt.Errorf("cherry-pick of %s: %w", shortSHA(c), pickErr)
	if _, err := r.in(work, "rev-parse", "-q", "--verify", "CHERRY_PICK_HEAD"); err != nil {
		return failed
	}
	// Any error here (staged changes, or git itself failing) is "not provably empty".
	if _, err := r.in(work, "diff", "--cached", "--quiet", "--no-ext-diff"); err != nil {
		return failed
	}
	if _, err := r.in(work, "cherry-pick", "--skip"); err != nil {
		return fmt.Errorf("skip an empty cherry-pick of %s: %w", shortSHA(c), err)
	}
	return nil
}

// unmergedPaths is the sorted, de-duplicated paths of `git ls-files -u -z` output
// ("<mode> <object> <stage>\t<path>" records separated by NUL).
func unmergedPaths(out string) []string {
	var paths []string
	for _, rec := range strings.Split(out, "\x00") {
		if _, path, ok := strings.Cut(rec, "\t"); ok && path != "" {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths)
}

// parentInOnto proves that the change of parent (its merge base with onto .. parent) is already in
// onto, or returns an error wrapping ErrRestackNotMerged. The proof is exact and against the CURRENT
// onto: either parent is an ancestor of onto, or merging parent into onto (merge-tree, isolated from
// repository config) is clean and yields exactly onto's tree, so parent adds nothing onto lacks.
//
// No hash of a patch is accepted as evidence: a stable patch-id ignores whitespace (also inside
// string literals), so a parent value "a b" and an onto value "ab" would match. The price is a false
// refusal when onto changed the squashed lines after the squash (the merge conflicts); a false
// acceptance would silently drop the parent's change.
func (r *restackGit) parentInOnto(repo, parent, parentSHA, onto, ontoSHA string) error {
	notMerged := func(why string) error {
		return fmt.Errorf("%w: %s is not contained in %s (%s); merge %s into %s first, or pass the right --onto",
			ErrRestackNotMerged, parent, onto, why, parent, onto)
	}
	base, err := r.in(repo, "merge-base", parentSHA, ontoSHA)
	if err != nil {
		return notMerged("no common history")
	}
	if base == parentSHA {
		return nil // parent is an ancestor of onto: merged without a squash.
	}
	ontoTree, err := r.in(repo, "rev-parse", "--verify", ontoSHA+"^{tree}")
	if err != nil {
		return err
	}
	out, code, _, err := isolatedMergeTree(r.ctx, repo, ontoSHA, parentSHA)
	if err != nil {
		return fmt.Errorf("merge-tree: %w", err)
	}
	if code == 0 && strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]) == ontoTree {
		return nil
	}
	return notMerged("merging it into the target changes the target or conflicts")
}
