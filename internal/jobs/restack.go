package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
}

// restackTimeout bounds one Restack; a variable so that tests can shorten it.
var restackTimeout = 10 * time.Minute

// restackPatchIDScan is how many commits of base..onto are compared by patch-id.
const restackPatchIDScan = 2000

// Restack creates newName (default <child>-restacked) from onto (default main) in repo and
// cherry-picks the commits of parent..child on it. See the package comment above.
func Restack(repo, child, parent, onto, newName string) (RestackResult, error) {
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

	// Cherry-pick in a throw-away worktree on a detached HEAD: no ref exists until the very end.
	tmp, err := os.MkdirTemp("", "agent-jobs-restack-")
	if err != nil {
		return RestackResult{}, err
	}
	work := filepath.Join(tmp, "work")
	defer func() {
		// Not bound by ctx: the cleanup must also run after a timeout.
		cleanup := &restackGit{ctx: context.Background(), env: r.env}
		_, _ = cleanup.in(repo, "worktree", "remove", "--force", work)
		_ = os.RemoveAll(tmp)
		_, _ = cleanup.in(repo, "worktree", "prune")
	}()
	if _, err := r.in(repo, "worktree", "add", "--quiet", "--detach", work, ontoSHA); err != nil {
		return RestackResult{}, fmt.Errorf("create temporary worktree: %w", err)
	}
	// Cherry-picking commits needs a committer; do not fail on a machine without one configured.
	if _, err := r.in(work, "var", "GIT_COMMITTER_IDENT"); err != nil {
		r.env = append(r.env, "GIT_COMMITTER_NAME=agent-jobs", "GIT_COMMITTER_EMAIL=agent-jobs@localhost")
	}

	res := RestackResult{Branch: newName, Onto: ontoSHA}
	for _, c := range commits {
		if _, err := r.in(work, "cherry-pick", "--allow-empty", "--no-edit", c); err != nil {
			if err := r.settleCherryPick(work, c, err); err != nil {
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
	head, err := r.in(work, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return RestackResult{}, err
	}
	// Create-only and atomic: a branch that appeared meanwhile is errBranchExists, never overwritten.
	if _, err := publishBranchRef(run, newName, head); err != nil {
		if errors.Is(err, errBranchExists) {
			err = fmt.Errorf("%w: %s (restack never moves an existing branch; pass --name)", errBranchExists, newName)
		}
		return RestackResult{}, err
	}
	res.Head = head
	return res, nil
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
func (r *restackGit) settleCherryPick(work, c string, pickErr error) error {
	files, _ := r.in(work, "diff", "--name-only", "--diff-filter=U")
	if files != "" {
		_, _ = r.in(work, "cherry-pick", "--abort")
		subject, _ := r.in(work, "log", "-1", "--format=%s", c)
		return &RestackConflictError{Commit: shortSHA(c) + " " + Printable(subject, 80), Files: strings.Split(files, "\n")}
	}
	if _, err := r.in(work, "rev-parse", "-q", "--verify", "CHERRY_PICK_HEAD"); err == nil {
		// Stopped without conflicts: the result is empty. Drop it and go on.
		if _, err := r.in(work, "cherry-pick", "--skip"); err != nil {
			return fmt.Errorf("skip an empty cherry-pick of %s: %w", shortSHA(c), err)
		}
		return nil
	}
	return fmt.Errorf("cherry-pick of %s: %w", shortSHA(c), pickErr)
}

// parentInOnto proves that the change of parent (its merge base with onto .. parent) is already in
// onto, or returns an error wrapping ErrRestackNotMerged. Two independent proofs, either suffices:
//  1. a merge of onto and parent yields exactly onto's tree (P adds nothing onto lacks);
//  2. one commit of base..onto has the same stable patch-id as the cumulative diff base..parent (the
//     squash itself), which also holds after main changed the squashed lines.
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
	want, err := r.patchIDs(repo, "diff", "--no-ext-diff", "--no-textconv", base, parentSHA)
	if err != nil {
		return err
	}
	if len(want) == 0 {
		return nil // an empty change is contained anywhere.
	}
	have, err := r.patchIDs(repo, "log", "-p", "--no-merges", "--no-ext-diff", "--no-textconv",
		fmt.Sprintf("--max-count=%d", restackPatchIDScan), base+".."+ontoSHA)
	if err != nil {
		return err
	}
	for id := range have {
		if want[id] {
			return nil
		}
	}
	return notMerged("no clean merge and no commit with the same patch-id")
}

// patchIDs is the set of stable patch-ids that `git patch-id --stable` prints for the output of
// the git command args run in repo.
func (r *restackGit) patchIDs(repo string, args ...string) (map[string]bool, error) {
	patch, code, stderr, err := gitEnvCtx(r.ctx, r.env, repo, append([]string{noReplace}, args...)...)
	if err == nil && code != 0 {
		err = fmt.Errorf("git %s exited %d: %s", args[0], code, RedactPrintable(stderr, 240))
	}
	if err != nil {
		return nil, err
	}
	var out, errOut bytes.Buffer
	p, err := startGit("", r.env, func(c *exec.Cmd) {
		c.Stdin, c.Stdout, c.Stderr = strings.NewReader(patch), &out, &errOut
	}, "-C", repo, "patch-id", "--stable")
	if err != nil {
		return nil, err
	}
	if err := p.wait(r.ctx); err != nil {
		return nil, fmt.Errorf("git patch-id: %w: %s", err, RedactPrintable(errOut.String(), 240))
	}
	ids := map[string]bool{}
	for _, line := range strings.Split(out.String(), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			ids[f[0]] = true
		}
	}
	return ids, nil
}
