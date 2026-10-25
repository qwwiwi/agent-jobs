package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
	"github.com/qwwiwi/agent-jobs/internal/guard"
)

// The merge hook (Wave F, F9 (b)): `agent-jobs hook merge-gate`, a PreToolUse hook on Bash in the
// orchestrator session. In a repository with an approved brief/CHECKLIST.md, `gh pr merge` is allowed
// only with `--match-head-commit <full sha>` (GitHub refuses the merge if the head moved) and only when
// release_check passed on exactly that sha at the checklist as it is (store.ReleasePass); `--auto` is
// refused (auto-merge merges a later head), and so is a pass older than evidence release_check would fail
// on (a newer verdict for the sha, a red manifest-check run). A repository whose checklist was never
// approved is not gated; one whose approval is void now (the checklist or brief edited by hand or gone, a
// newer slicing check, an unreadable file) stays gated and is refused until it is approved again (fail
// closed: a void approval never lifts the gate). Another clone of a gated repository (a shared OWNER/REPO
// remote) is gated with it. A command whose merge the reader cannot read is refused while some repository
// is gated. It reads the store and the brief files only. `gh api` merges, `git push` to the base
// branch and the web UI pass by it (PLAN Wave F, Risks): it catches honest mistakes, it is not a boundary.

// MergeGatePrefix starts the hook's refusal line.
const MergeGatePrefix = "agent-jobs merge-gate: blocked: "

// fullSHA1Re is the full commit id --match-head-commit must carry (GitHub's ids are sha1).
var fullSHA1Re = regexp.MustCompile(`^[0-9a-f]{40}$`)

// PreToolUseInput is the part of Claude Code's PreToolUse-hook input the merge hook reads.
type PreToolUseInput struct {
	SessionID string
	Cwd       string
	ToolName  string
	// Command is tool_input.command of a Bash call ("" for another tool).
	Command string
}

// ParsePreToolUseInput reads the PreToolUse-hook JSON: a PreToolUse event with a tool_name; a Bash call
// must carry tool_input.command as a string.
func ParsePreToolUseInput(r io.Reader) (PreToolUseInput, error) {
	raw, err := readHookInput(r)
	if err != nil {
		return PreToolUseInput{}, err
	}
	if *raw.HookEventName != "PreToolUse" {
		return PreToolUseInput{}, fmt.Errorf("hook input: event %q, want PreToolUse", Printable(*raw.HookEventName, 64))
	}
	if raw.ToolName == nil || *raw.ToolName == "" {
		return PreToolUseInput{}, errors.New("hook input: PreToolUse event without tool_name")
	}
	in := PreToolUseInput{ToolName: *raw.ToolName}
	if raw.SessionID != nil {
		in.SessionID = *raw.SessionID
	}
	if raw.Cwd != nil {
		in.Cwd = *raw.Cwd
	}
	if in.ToolName != "Bash" {
		return in, nil
	}
	var ti struct {
		Command *string `json:"command"`
	}
	if len(raw.ToolInput) == 0 || json.Unmarshal(raw.ToolInput, &ti) != nil || ti.Command == nil {
		return PreToolUseInput{}, errors.New("hook input: Bash call without a tool_input.command string")
	}
	in.Command = *ti.Command
	return in, nil
}

// MergeGateNeeded reports whether the command runs, or may run, `gh pr merge`: only then does the hook
// open the store.
func MergeGateNeeded(command string) bool {
	calls, unsure := guard.GhPrMerges(command)
	return len(calls) > 0 || unsure != ""
}

// gatedRepo is the checklist state of one repository's main work tree.
type gatedRepo struct {
	// remotes are the OWNER/REPO names of its remotes (read once, remotesRead).
	remotes     []string
	remotesRead bool
	main        string
	// gated: the checklist is approved now, or it was approved once and the approval is void (void says
	// why; every merge is refused then).
	gated   bool
	void    string
	id      string
	version int
	hash    string
}

// mergeGate is one hook call: the repositories it has read, by main work tree.
type mergeGate struct {
	m     *Manager
	repos map[string]*gatedRepo
	all   []*gatedRepo // the gated stored repositories (nil = not read yet)
}

// MergeGate is the hook's answer for a Bash command run at cwd: a refusal line ("" = allowed). envRepo is
// GH_REPO of the session's environment. An error (unreadable store or brief files) is returned as is;
// the caller refuses on it.
func (m *Manager) MergeGate(cwd, command, envRepo string) (string, error) {
	calls, unsure := guard.GhPrMerges(command)
	if len(calls) == 0 && unsure == "" {
		return "", nil
	}
	g := &mergeGate{m: m, repos: map[string]*gatedRepo{}}
	if unsure != "" {
		if r, err := g.refuseUnsure(unsure); r != "" || err != nil {
			return r, err
		}
	}
	for _, call := range calls {
		if call.Unsure != "" {
			if r, err := g.refuseUnsure(call.Unsure); r != "" || err != nil {
				return r, err
			}
			continue
		}
		targets, why, err := g.targets(cwd, call, envRepo)
		if err != nil {
			return "", err
		}
		if why != "" {
			if r, err := g.refuseUnsure(why); r != "" || err != nil {
				return r, err
			}
			continue
		}
		for _, t := range targets {
			if r, err := g.check(t, call); r != "" || err != nil {
				return r, err
			}
		}
	}
	return "", nil
}

// refuseUnsure refuses a merge the hook cannot read while some stored repository has an approved
// checklist ("" when none has: nothing is gated).
func (g *mergeGate) refuseUnsure(why string) (string, error) {
	all, err := g.approvedRepos()
	if err != nil || len(all) == 0 {
		return "", err
	}
	names := make([]string, 0, len(all))
	for _, r := range all {
		names = append(names, Printable(r.main, 200))
	}
	return MergeGatePrefix + "cannot tell whether this command runs gh pr merge in a repo with an approved checklist (" +
		Printable(why, 200) + "; gated: " + strings.Join(names, ", ") + "); write gh pr merge as a plain command with -R " +
		"OWNER/REPO and --match-head-commit <full sha>.", nil
}

// targets are the gated repositories a call may merge in. A repository the call names (PR URL, -R) is
// the one gh uses; without one, every candidate counts (each GH_REPO value, the cwd and every cd target
// before the call), since the reader cannot tell which one gh takes. why is set when a candidate cannot
// be read.
func (g *mergeGate) targets(cwd string, call guard.GhMerge, envRepo string) (out []*gatedRepo, why string, err error) {
	add := func(r *gatedRepo) {
		if r.gated && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	named := []string{call.Repo}
	if call.Repo == "" {
		named = append(slices.Clone(call.GHRepos), envRepo)
	}
	for _, n := range named {
		if n == "" {
			continue
		}
		name := gitHubRepoName(n)
		if name == "" {
			return nil, fmt.Sprintf("repository %q is not OWNER/REPO", n), nil
		}
		all, err := g.approvedRepos()
		if err != nil {
			return nil, "", err
		}
		for _, r := range all {
			names, why := g.remoteNames(r)
			if why != "" {
				return nil, why, nil
			}
			if slices.Contains(names, name) {
				add(r)
			}
		}
	}
	if call.Repo != "" {
		return out, "", nil
	}
	if cwd == "" || !filepath.IsAbs(cwd) {
		return nil, "the hook input has no absolute cwd", nil
	}
	home, _ := os.UserHomeDir()
	dirs, prev := []string{cwd}, cwd
	for _, d := range call.Dirs {
		switch {
		case d == "" || d == "~":
			d = home
		case strings.HasPrefix(d, "~/"):
			d = filepath.Join(home, d[2:])
		case strings.HasPrefix(d, "~"):
			return nil, "cd to another user's home " + d, nil
		case !filepath.IsAbs(d):
			d = filepath.Join(prev, d)
		}
		if d == "" {
			return nil, "cd to the home directory, which is unknown", nil
		}
		dirs, prev = append(dirs, d), d
	}
	for _, d := range dirs {
		_, main, err := ChecklistRepo(d)
		if err != nil {
			continue // not a git work tree (or gone): gh has no repository there
		}
		r, err := g.repo(main)
		if err != nil {
			return nil, "", err
		}
		if r.gated {
			add(r)
			continue
		}
		// Another clone of a gated repository merges the same pull requests: match it by its remotes.
		if why, err := g.byRemotes(r, add); why != "" || err != nil {
			return nil, why, err
		}
	}
	return out, "", nil
}

// byRemotes adds every gated repository that shares an OWNER/REPO remote with r; why is set when a
// remote list is unreadable.
func (g *mergeGate) byRemotes(r *gatedRepo, add func(*gatedRepo)) (string, error) {
	all, err := g.approvedRepos()
	if err != nil || len(all) == 0 {
		return "", err
	}
	mine, why := g.remoteNames(r)
	if why != "" {
		return why, nil
	}
	for _, gr := range all {
		names, why := g.remoteNames(gr)
		if why != "" {
			return why, nil
		}
		if slices.ContainsFunc(mine, func(n string) bool { return slices.Contains(names, n) }) {
			add(gr)
		}
	}
	return "", nil
}

// remoteNames are r's remote OWNER/REPO names (cached); why is set when they are unreadable.
func (g *mergeGate) remoteNames(r *gatedRepo) ([]string, string) {
	if !r.remotesRead {
		names, err := remoteRepoNames(r.main)
		if err != nil {
			return nil, fmt.Sprintf("the remotes of %s are unreadable: %v", r.main, err)
		}
		r.remotes, r.remotesRead = names, true
	}
	return r.remotes, ""
}

// check is the refusal for a merge in gated repository r ("" = allowed).
func (g *mergeGate) check(r *gatedRepo, call guard.GhMerge) (string, error) {
	if r.void != "" {
		return MergeGatePrefix + fmt.Sprintf("gh pr merge in %s: its checklist approval is void (%s); approve the checklist "+
			"again (checklist_set) and run release_check on the sha first.", Printable(r.main, 200), RedactPrintable(r.void, 200)), nil
	}
	where := fmt.Sprintf("%s (checklist %s v%d is approved)", Printable(r.main, 200), Printable(r.id, 80), r.version)
	switch {
	case call.DisableAuto && !call.Auto:
		return "", nil // gh only turns auto-merge off
	case call.Auto:
		return MergeGatePrefix + "gh pr merge --auto in " + where + ": auto-merge merges a later head; merge without --auto, " +
			"with --match-head-commit <sha> of a commit release_check passed on.", nil
	case !call.HasMatchHead:
		return MergeGatePrefix + "gh pr merge in " + where + " without --match-head-commit <full sha>; add the 40-hex sha " +
			"release_check passed on.", nil
	}
	sha := strings.ToLower(call.MatchHead)
	if !fullSHA1Re.MatchString(sha) {
		return MergeGatePrefix + fmt.Sprintf("gh pr merge in %s: --match-head-commit %q is not a full 40-hex sha.", where,
			Printable(call.MatchHead, 80)), nil
	}
	p, err := g.m.Store.ReleasePassed(r.main, sha, r.id, r.version, r.hash)
	if err != nil {
		return "", err
	}
	if p == nil {
		return MergeGatePrefix + fmt.Sprintf("gh pr merge in %s: release_check has not passed on %s at this checklist; "+
			"run release_check(repo, sha) first.", where, sha), nil
	}
	// Evidence stored after the pass that release_check would fail on: a newer verdict for the sha, or a
	// red manifest-check run. The pass then no longer counts until release_check runs again.
	stale := ""
	if red, err := g.m.Store.RedManifestCheck(r.main, sha, r.id, r.version, r.hash); err != nil {
		return "", err
	} else if red != nil {
		stale = fmt.Sprintf("manifest-check of job %s exited %d after it", red.JobID, red.ExitCode)
	}
	vs, err := g.m.Store.VerdictsAt(r.main, r.id, r.version, sha)
	if err != nil {
		return "", err
	}
	latest := ""
	for _, v := range vs {
		if v.ChecklistSHA256 == r.hash {
			latest = v.JobID
		}
	}
	if stale == "" && latest != p.VerdictJob {
		stale = fmt.Sprintf("the latest verdict for the sha is of job %s, the pass read job %s", latest, p.VerdictJob)
	}
	if stale != "" {
		return MergeGatePrefix + fmt.Sprintf("gh pr merge in %s: the release_check pass on %s is stale (%s); run "+
			"release_check(repo, sha) again.", where, sha, RedactPrintable(stale, 200)), nil
	}
	return "", nil
}

// repo reads the checklist state of a main work tree (cached), without the checklist lock like the
// brief reminder: a read that races a checklist_set can misjudge one call. A repository never approved
// is not gated; one approved once stays gated (void when its checklist is not approved now, also when the
// file is gone); a store error is returned.
func (g *mergeGate) repo(main string) (*gatedRepo, error) {
	if r, ok := g.repos[main]; ok {
		return r, nil
	}
	last, err := g.m.Store.LatestApproval(main)
	if err != nil {
		return nil, err
	}
	r := &gatedRepo{main: main}
	voided := func(why string) (*gatedRepo, error) {
		r.gated, r.void = last != nil, why
		g.repos[main] = r
		return r, nil
	}
	f, err := checklist.Read(main)
	switch {
	case err != nil:
		return voided("the brief files are unreadable: " + err.Error())
	case f.Checklist == nil && last == nil:
		g.repos[main] = r
		return r, nil
	case f.Checklist == nil:
		return voided("the work tree has no " + checklist.ChecklistRel)
	case f.Brief == nil:
		return voided("the work tree has no " + checklist.BriefRel)
	}
	text, _ := Redact(string(f.Checklist))
	c, err := checklist.Parse(text)
	if err != nil {
		return voided(checklist.ChecklistRel + " does not parse: " + err.Error())
	}
	approved, hash, reason, _, err := g.m.ChecklistStatus(main, c, f.Brief)
	if err != nil {
		return nil, err
	}
	r.id, r.version, r.hash = c.ID, c.Version, hash
	if !approved {
		return voided(reason)
	}
	r.gated = true
	g.repos[main] = r
	return r, nil
}

// approvedRepos are the gated repositories among those with a stored approval.
func (g *mergeGate) approvedRepos() ([]*gatedRepo, error) {
	if g.all != nil {
		return g.all, nil
	}
	mains, err := g.m.Store.ApprovalRepos()
	if err != nil {
		return nil, err
	}
	all := []*gatedRepo{}
	for _, main := range mains {
		r, err := g.repo(main)
		if err != nil {
			return nil, err
		}
		if r.gated {
			all = append(all, r)
		}
	}
	g.all = all
	return all, nil
}

// gitHubRepoName is the lower-cased OWNER/REPO of a repository as gh takes it (OWNER/REPO,
// HOST/OWNER/REPO, a URL, an scp-like git remote), "" when it names none.
func gitHubRepoName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		if at := strings.LastIndex(strings.SplitN(s, "/", 2)[0], "@"); at >= 0 {
			s = s[at+1:]
		}
	} else if host, path, ok := strings.Cut(s, ":"); ok && !strings.Contains(host, "/") {
		s = host + "/" + path // git@github.com:owner/repo.git
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return ""
	}
	owner, name := parts[len(parts)-2], parts[len(parts)-1]
	if owner == "" || name == "" || len(parts) > 3 {
		return ""
	}
	return strings.ToLower(owner + "/" + name)
}

// remoteRepoNames are the OWNER/REPO names of a work tree's remotes.
func remoteRepoNames(dir string) ([]string, error) {
	out, err := git(dir, "remote", "-v")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if n := gitHubRepoName(f[1]); n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return names, nil
}
