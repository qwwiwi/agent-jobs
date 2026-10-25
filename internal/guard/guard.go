// Package guard implements the PreToolUse hook that rejects tool calls whose
// failure would look like success, or that misattribute authorship or reach outside the job:
//   - `$?` read right after a pipeline (it is the status of the last stage only);
//   - a wait-loop that uses `pgrep -f` (the loop's own shell matches its pattern, so it never ends);
//   - the owner's name inside a commit;
//   - a foreign git identity (author/committer other than the configured one);
//   - anything that goes outside: git push/tag/send-pack/http-push; any gh call except read-only
//     subcommands (view, list, diff, checks, status …), so PR/issue create/comment/edit/merge/review/
//     close --comment are rejected;
//   - git configuration that runs programs (hooks, pager, editor, ssh command, credential helper,
//     external diff, aliases …): `git -c` / `git config` only for an allowlist of harmless keys, and no
//     change to the git environment the wrapper sets up (GIT_*, PAGER, EDITOR, VISUAL; env -i / -u, unset);
//   - git subcommands outside git's own command list (aliases, external git-*) or that run commands
//     (rebase -x, submodule foreach, bisect run, difftool, filter-branch, …), and writes into
//     .git/config, .git/hooks or $GIT_DIR;
//   - nested agents (claude, claude-code, codex, gemini, aider, cursor-agent): a job is a leaf;
//   - agent-jobs itself in any form except `agent-jobs mcp --role job` (no nested jobs), and any change
//     to the AGENT_JOBS_* environment (the AGENT_JOBS_JOB marker the wrapper sets);
//   - more than three subagents per job (Agent/Task tool), or a subagent that would leave the job
//     (isolation worktree/remote, background run, agent team);
//   - in a review job (Config.ReadOnly), any write into the job's worktree (readonly.go);
//   - any tool outside the job allowlist (toolallow.go): schedules, wakeups, messages to other
//     sessions, notifications, monitors (their command bypasses the Bash checks), worktrees,
//     questions, MCP tools other than the exact job-role agent-jobs tools, and every unknown tool.
//
// Commands are parsed (quotes, separators, env prefixes, wrappers), not matched as raw text, so
// spelling variants such as `git -C x 'push'`, `/usr/lib/git-core/git-push` or `{git,push}` are still
// seen, and a form the guard cannot resolve statically around git/gh (git/gh inside a substitution,
// eval or `sh -c`; stdin, here-doc or here-string into git/gh; a command name or git subcommand from a
// variable or a glob/brace; `env -S`; `xargs git`; attached `-cKEY=V` / `-C.`; a shell reading
// commands from stdin) is rejected, not guessed at.
//
// The guard is NOT a sandbox. It blocks the direct spellings of outward actions in a Bash call, and
// together with the git environment scrub in the wrapper (own GIT_CONFIG_GLOBAL, no system config,
// empty hooks dir, cat/true as pager and editor) it keeps git from running programs the guard does
// not see. A script the job writes and then runs is allowed by design (as in the reference): what
// such a script does is not inspected.
package guard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/qwwiwi/agent-jobs/internal/casefold"
)

// Config tells the guard who the owner is and which git identity is allowed.
type Config struct {
	OwnerNames []string // names that must never appear in a commit
	GitName    string   // allowed git user.name ("" = no identity override allowed)
	GitEmail   string   // allowed git user.email ("" = no identity override allowed)
	GitDirs    []string // the job worktree's real git dirs (admin dir, common dir): file tools never write there
	Dir        string   // working directory of the call (resolves `git commit -F <file>`)
	// SubagentLog is the job's subagent log (one line per allowed subagent, at most MaxSubagentsPerJob).
	// Empty = subagents are refused.
	SubagentLog string
	// SubagentReserve is the job's monotonic subagent counter (the store row): it counts one more
	// subagent unless max are used and returns the count before the call. Nil = the log alone counts.
	SubagentReserve func(max int) (prev int, allowed bool, err error)
	// Protected are paths (files or directories) a job never writes: the subagent logs and the job
	// database that holds the counter, the agent's config.json, owner rules and project cards, the
	// job's gitconfig and no-hooks dir, skill roots/links, the job's session transcript directories, and
	// a repository job's brief/BRIEF.md and brief/CHECKLIST.md (Wave F: the orchestrator's checklist
	// tools are their only writers). File tools and literal Bash targets into them are refused.
	Protected []string
	// StateDir is the agent's state dir (jobs.db, every job's folder, archive, config): a job never
	// reads it, except its own folder OwnDir. Empty = reads are not checked.
	StateDir string
	// OwnDir is the job's own folder inside StateDir (prompt, report, work): it stays readable.
	OwnDir string
	// NoRead are directories outside StateDir a job never reads or writes, checked like StateDir without
	// an own folder: the checklist locks every agent of the user shares (a job holding one would block
	// the orchestrator's checklist tools).
	NoRead []string
	// ReadOnly is the worktree of a review job: no tool call writes inside it (readonly.go). "" = off.
	ReadOnly string
	// ReadAllow, when not empty, are the only paths (directories or files) a job reads, a gate job's
	// (Wave F: its own folder with its work dir, its session dirs, the system dirs programs run from):
	// every other path is refused for the read tools and as a Bash word, as agent-jobs state is, with
	// the same resolution (symlinks, cd, -C, globs, recursive readers, xargs). Two rules are stricter
	// than the state check: after a cd the guard cannot resolve every relative word is refused, and so is
	// a command or redirection word it cannot expand ($VAR, a substitution). A path under a top-level
	// name that does not exist (`/api/v1` in a pattern) cannot be read and is not refused.
	ReadAllow []string
	// readAllowScope marks the read scope readScopes builds from ReadAllow (StateDir "/", the allowed
	// paths its own folders).
	readAllowScope bool
}

// HookInput is the subset of the Claude Code PreToolUse payload the guard reads.
type HookInput struct {
	ToolName  string `json:"tool_name"`
	Cwd       string `json:"cwd"`
	ToolInput struct {
		Command      string `json:"command"`
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		Path         string `json:"path"`    // Grep, Glob
		Pattern      string `json:"pattern"` // Glob (Grep's pattern is a regexp, not a path)
	} `json:"tool_input"`
	// RawInput is tool_input as sent (subagent calls are checked on fields of their own).
	RawInput json.RawMessage `json:"-"`
}

// fileTools write the file named in their input (file_path, or notebook_path for NotebookEdit).
var fileTools = map[string]bool{"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true}

// Decide parses the hook JSON and returns a non-empty reason when the call must be blocked, within
// decideBudget (a refusal past it) and with the symlink resolutions of the call memoized.
func Decide(r io.Reader, cfg Config) (string, error) {
	return withDeadline(func() (string, error) {
		defer startMemo()()
		return decideCall(r, cfg)
	})
}

func decideCall(r io.Reader, cfg Config) (string, error) {
	var raw struct {
		ToolInput json.RawMessage `json:"tool_input"`
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("guard: read hook input: %w", err)
	}
	var in HookInput
	if err := json.Unmarshal(b, &in); err != nil {
		return "", fmt.Errorf("guard: parse hook input: %w", err)
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return "", fmt.Errorf("guard: parse hook input: %w", err)
	}
	in.RawInput = raw.ToolInput
	if cfg.Dir == "" {
		cfg.Dir = in.Cwd
	}
	if cfg.StateDir == "" {
		state, own, ok := StateLayout(cfg.SubagentLog)
		if !ok && cfg.SubagentLog != "" {
			// A misconfigured hook must not silently turn the state-read rule off: fail closed.
			return "blocked by guard: the subagent log `" + cfg.SubagentLog + "` is not <state>/subagents/<id>.log, " +
				"so the guard cannot derive the job's folder; every tool call is refused until the hook is fixed.", nil
		}
		cfg.StateDir, cfg.OwnDir = state, own
	}
	if !ToolAllowed(in.ToolName) {
		return toolRefused(in.ToolName), nil
	}
	if readTools[in.ToolName] {
		p, pattern := in.ToolInput.Path, ""
		switch in.ToolName {
		case "Read":
			p = in.ToolInput.FilePath
		case "Glob":
			pattern = in.ToolInput.Pattern
		}
		return CheckFileRead(in.ToolName, p, pattern, cfg), nil
	}
	if fileTools[in.ToolName] {
		p := in.ToolInput.FilePath
		if in.ToolName == "NotebookEdit" {
			p = in.ToolInput.NotebookPath
		}
		return CheckFileWrite(in.ToolName, p, cfg), nil
	}
	if IsSubagentTool(in.ToolName) {
		return CheckSubagent(in.RawInput, cfg), nil
	}
	if in.ToolName != "Bash" {
		return "", nil
	}
	if len(cfg.ReadAllow) > 0 && backgroundBash(in.RawInput) {
		// claude prints another result event when a background task ends: a gate job's result must be the
		// only result event of its run (a second one is taken for a line another process wrote).
		return "blocked by guard: a gate job runs Bash in the foreground only (run_in_background is refused): " +
			"run the command without it.", nil
	}
	return Check(in.ToolInput.Command, cfg), nil
}

// resolvePath returns path made absolute (against dir), cleaned, with its symlinks resolved the way
// the kernel looks it up (walkPath). Past the step budget it returns the cleaned path: callers that
// must refuse then use resolveBounded.
func resolvePath(path, dir string) string {
	r, ok := resolveBounded(path, dir)
	if !ok {
		return filepath.Clean(r)
	}
	return r
}

// resolveBounded is resolvePath with ok false when the resolution follows more than maxLinks links or
// more than maxResolveSteps components of link targets (the path itself was returned cleaned, not
// resolved).
func resolveBounded(path, dir string) (string, bool) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) { // dir was relative: nothing to resolve against
		return path, true
	}
	if r, ok := memoWalk(path); ok {
		return r, true
	}
	return path, false
}

// maxLinks is how many symlinks one lookup follows: the kernel gives up past 40 (ELOOP), so no file
// is created there.
const maxLinks = 40

// maxResolveSteps is how many path components of symlink targets one resolution walks, all links
// together (one Lstat and at most one Readlink each). The path itself costs one step per component,
// but 40 links with long targets could make a lookup the kernel performs in microseconds cost the
// guard minutes; past this budget the guard refuses instead of resolving.
const maxResolveSteps = 4096

// walkPath resolves an absolute path component by component like the kernel: a symlink is replaced
// by its target text, which is walked the same way (a `..` in it leaves the directory the part before
// it resolves to, not lexically: `cfg -> jump/../hooks/pre-commit` with `jump -> .git/objects` creates
// .git/hooks/pre-commit), and `..` steps up from the physical directory reached so far. Past a
// component that does not exist the rest is taken lexically (Write creates the missing directories);
// a `..` back onto an existing directory makes its symlinks count again. ok is false past maxLinks
// links or maxResolveSteps components of link targets (the path is then returned as given).
func walkPath(abs string) (string, bool) {
	vol := filepath.VolumeName(abs)
	sep := byte(filepath.Separator)
	// cur is the path reached so far, grown and cut in place: each component costs O(1) past a
	// missing one (no string rebuilt per component), one Lstat while the path exists.
	cur := append(make([]byte, 0, len(abs)), vol...)
	rootLen := len(cur)
	str := func() string {
		if len(cur) == rootLen {
			return string(cur) + string(sep)
		}
		return string(cur)
	}
	exists := true
	todo := strings.Split(filepath.ToSlash(abs[len(vol):]), "/")
	links, steps := 0, 0
	for len(todo) > 0 {
		seg := todo[0]
		todo = todo[1:]
		if seg == "" || seg == "." {
			continue
		}
		if seg == ".." {
			if i := bytes.LastIndexByte(cur[rootLen:], sep); i >= 0 {
				cur = cur[:rootLen+i]
			}
			if !exists {
				_, err := os.Lstat(str())
				exists = err == nil
			}
			continue
		}
		n := len(cur)
		cur = append(append(cur, sep), seg...)
		if !exists {
			continue
		}
		next := str()
		fi, err := os.Lstat(next)
		if err != nil {
			exists = false
			continue
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target, err := os.Readlink(next)
		if err != nil {
			continue
		}
		if links++; links > maxLinks {
			// The kernel fails this lookup, but it counts links from the physical cwd and a tool
			// that looks a path up component by component (mkdir -p) starts over for each lookup:
			// the guard cannot tell where such a path leads, so it refuses (ok false).
			return abs, false
		}
		cur = cur[:n]
		if filepath.IsAbs(target) {
			cur = cur[:rootLen]
		}
		segs := strings.Split(filepath.ToSlash(target), "/")
		if steps += len(segs); steps > maxResolveSteps {
			return abs, false
		}
		todo = append(segs, todo...)
	}
	return str(), true
}

// unresolvedRoot returns a configured path (the state dir, the job's folder, the read-only worktree,
// a git dir, a protected path, a NoRead directory) whose symlinks take more than the resolution
// budget to resolve: where it physically lies is unknown, so comparing a target with it as written
// would leave the real location unprotected (fail open). Every write is refused instead.
func unresolvedRoot(cfg Config) (string, bool) {
	roots := append(append(append([]string{cfg.StateDir, cfg.OwnDir, cfg.ReadOnly}, cfg.GitDirs...), cfg.Protected...), cfg.NoRead...)
	for _, p := range roots {
		if p == "" {
			continue
		}
		if _, ok := resolveBounded(p, "/"); !ok {
			return p, true
		}
	}
	return "", false
}

func rootBlocked(p string) string {
	return "blocked by guard: the configured path `" + p + "` (agent-jobs state, configuration or a git dir) runs " +
		"through more than " + strconv.Itoa(maxLinks) + " symlinks or symlink targets of more than " +
		strconv.Itoa(maxResolveSteps) + " path components, so the guard cannot tell where it lies; every write is " +
		"refused until the configuration names the real path."
}

// inGitDir reports a path with a `.git` segment: the git dir itself, a file in it, or a worktree's
// `.git` file.
func inGitDir(p string) bool {
	return slices.ContainsFunc(strings.Split(filepath.ToSlash(p), "/"), func(s string) bool {
		return strings.EqualFold(s, ".git")
	})
}

// foldCase is set where the default file system ignores case (macOS APFS and HFS+, Windows NTFS):
// there `JOBS.DB` names the same file as `jobs.db`, so protected paths are compared case-folded. On
// a case-sensitive volume of such a system the guard refuses more, never less. Tests set it to
// simulate a case-insensitive file system on Linux.
var foldCase = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

// fold returns a path or text as the file system compares names: Unicode case-folded under
// foldCase (`JOBſ.DB` names `jobs.db`, see casefold.Fold).
func fold(p string) string {
	if foldCase {
		return casefold.Fold(p)
	}
	return p
}

// progName is the name a command's rules are looked up by: the base name of argv[0], Unicode
// case-folded (`rſync` runs rsync from a case-insensitive volume) and mapped through toolAliases.
func progName(argv0 string) string {
	n := casefold.Fold(base(argv0))
	for _, interp := range []string{"python", "pypy", "perl", "ruby", "php", "lua", "luajit"} {
		if suffix := strings.TrimPrefix(n, interp); suffix != n && versionSuffix(suffix) {
			n = interp
			break
		}
	}
	if a, ok := toolAliases[n]; ok {
		return a
	}
	return n
}

// versionSuffix accepts numeric executable versions without normalizing unrelated names.
func versionSuffix(s string) bool {
	if s == "" {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if !isDigits(part) {
			return false
		}
	}
	return true
}

// within reports whether p is d or lies inside it (both absolute and clean), case-folded under
// foldCase.
func within(p, d string) bool {
	rel, err := filepath.Rel(fold(d), fold(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// inJobGitDir reports a path inside one of the job's real git dirs (as given or with symlinks resolved
// like a write target, resolvePath: also a dir that does not exist yet behind a symlinked parent, or
// the target of a dangling link): they need not be named .git (a separate git dir, a worktree admin
// dir). It is used the same way for the protected paths and the read-only worktree.
func inJobGitDir(p string, gitDirs []string) bool {
	for _, d := range gitDirs {
		if d == "" {
			continue
		}
		d = filepath.Clean(d)
		if within(p, d) || within(p, resolvePath(d, "/")) {
			return true
		}
	}
	return false
}

// hasDotDot reports a `..` segment in a path as written: the kernel resolves `link/..` after the
// symlink, while filepath.Clean drops it lexically, so such a path is not checked, it is rejected.
func hasDotDot(path string) bool {
	return slices.Contains(strings.Split(filepath.ToSlash(path), "/"), "..")
}

// CheckFileWrite returns a non-empty reason when a file tool (Write, Edit, MultiEdit, NotebookEdit)
// would write inside a git dir: any `.git` directory, the job's own git dirs (cfg.GitDirs), git
// config/hooks paths; into a protected path (also through a file descriptor alias or a hard link of
// a protected file); or into the state dir outside the job's own folder. A path with a `..` segment
// is rejected outright; otherwise both the path as given and the path with symlinks resolved are
// checked (a link in the job dir pointing into .git), and a path whose symlinks take more than
// maxResolveSteps to resolve is refused, as is every write while a configured path is such a path
// (unresolvedRoot).
func CheckFileWrite(tool, path string, cfg Config) string {
	if path == "" {
		return ""
	}
	if p, bad := unresolvedRoot(cfg); bad {
		return rootBlocked(p)
	}
	if hasDotDot(path) {
		return "blocked by guard: " + tool + " path `" + path + "` contains `..`; write the path without `..` " +
			"(through a symlink `..` is resolved by the kernel, not as written)."
	}
	given := filepath.Clean(path)
	if !filepath.IsAbs(given) {
		given = filepath.Join(cfg.Dir, given)
	}
	resolved, ok := resolveBounded(path, cfg.Dir)
	if !ok {
		return "blocked by guard: " + tool + " path `" + path + "` runs through more than " + strconv.Itoa(maxLinks) +
			" symlinks or symlink targets of more than " + strconv.Itoa(maxResolveSteps) + " path components; the " +
			"guard cannot verify it. Write the real path."
	}
	if len(cfg.Protected) > 0 {
		for _, p := range []string{given, resolved} {
			if alias, _ := fdAlias(p); alias {
				return fdAliasBlocked(path)
			}
		}
	}
	// The state dir keeps the job's own folder writable; the NoRead directories (the shared checklist
	// locks) are state dirs without an own folder.
	var state *stateReadScope
	var noRead []*stateReadScope
	for _, c := range readScopes(cfg) {
		if c.StateDir == cfg.StateDir && state == nil {
			state = newStateReadScope(nil, c)
			continue
		}
		noRead = append(noRead, newStateReadScope(nil, c))
	}
	for _, p := range []string{given, resolved} {
		if inGitDir(p) || gitFile(p) || inJobGitDir(p, cfg.GitDirs) {
			return "blocked by guard: " + tool + " into `" + p + "`: a job does not write inside a .git directory " +
				"(git would run programs from its config and hooks)."
		}
		if inJobGitDir(p, cfg.Protected) {
			return protectedBlocked(p)
		}
		if state != nil {
			if q, in := state.blocked(p, "", false); in {
				return stateWriteBlocked(tool, q)
			}
		}
		for _, s := range noRead {
			if q, in := s.blocked(p, "", false); in {
				return stateBlocked(tool+" into `"+p+"`", q)
			}
		}
		if readOnlyFile(p, cfg) {
			return readOnlyBlocked(tool + " into `" + p + "`")
		}
	}
	if len(cfg.Protected) > 0 {
		links := &inodeSet{forms: protectedForms(cfg.Protected)}
		if q, ok := links.hardlink(resolved); ok {
			return protectedBlocked(q + " (`" + path + "` is a hard link of it)")
		}
	}
	return ""
}

// protectedKinds names what the protected paths (cfg.Protected) are, for the refusals.
const protectedKinds = "the subagent logs or the job database, agent-jobs configuration (config.json, the owner " +
	"rules, the project cards), the job's own setup (its gitconfig, no-hooks dir, settings.json or mcp.json), a skill " +
	"catalog (a skill root or a skill link), a job session transcript directory, or the brief files (brief/BRIEF.md, " +
	"brief/CHECKLIST.md: only the orchestrator's checklist tools write them)"

func protectedBlocked(p string) string {
	return "blocked by guard: `" + p + "` is agent-jobs state: " + protectedKinds + "; a job does not write there."
}

func ancestorBlocked(w, p string) string {
	return "blocked by guard: argument `" + w + "` is an ancestor of agent-jobs state (`" + p + "`: " + protectedKinds +
		"); a job does not remove, move or write into it."
}

// checkProtected rejects Bash writes into cfg.Protected. Paths are resolved by pathScope (pathres.go):
// `~/` and $HOME expanded, relative words against the call's directory and every literal cd/pushd
// target in the call, symlinks resolved on the longest existing prefix. Refused:
//   - an output redirection to a protected path;
//   - a protected path (or one inside it) as an argument of any program that is not a plain reader
//     (rm, mv, cp, ln, tee, truncate, sed -i, dd, sqlite3, cd, …), also inside an option value, inline
//     code, a here-doc or a here-string (`dd of=…`, `python3 -c "open('…')"`, `python3 <<< "…"`),
//     and the directory holding one (`cp f <dir>`);
//   - for rm/mv/cp/ln/rsync/chmod/chown/tar/… and find with an action, an ancestor of a protected
//     path (`rm -r <state>`, `find <state> -delete`, `ln -s <state> S`); for cp/install/ln/mv a
//     destination D is refused when it is or holds a protected path, when D/<source name> reaches
//     one, or when it is an ancestor of one, except D written as $HOME itself without -T or --parents
//     and with every source a plain name (`cp f ~` stays allowed, `cp -r m/. ~` does not);
//   - inside option values and inline code, every path-like token (split on `=`, `:`, quotes,
//     brackets, …; tokens that clean to `/` or `.` skipped) resolved like an argument; in inline code
//     (-c/-e values, here-docs, here-strings) and `--opt=`/`key=` values also as an ancestor when
//     spelled with `//`, `/./`, `..` or lying strictly below $HOME; a `file:` URI also percent-decoded
//     and with an empty or `localhost` authority dropped; `..` walked like the kernel (after the
//     symlinks of its prefix) as well as lexically, and refused outright when the call may create a
//     symlink (ln, cp -s, a word naming symlink) and the part before the first `..` does not exist;
//   - `xargs <non-reader>`, and an interpreter that may read its program from the pipe (python,
//     perl, node, ruby, php, sqlite3, whatever its arguments), fed by an earlier stage of its pipeline
//     that names a protected path or an ancestor of one;
//   - after a cd the guard cannot resolve (`cd $X`, `cd -`, CDPATH), a relative word that could lead
//     to a protected path; likewise `~user/…`;
//   - a call whose check takes longer than protectedBudget (fail closed: «too complex to verify»).
//
// Residual gap (DEVIATIONS #12), not resolved: a path built at run time (a variable other than
// $HOME, a substitution, a file list read by xargs or a program read by an interpreter from a file
// or from a pipe stage the guard does not see the text of, a path assembled or escaped in inline
// code), a plain ancestor of a protected path in free text or outside $HOME (`os.chdir('/home')`,
// then a relative path), a relative path inside inline code after an unresolved cd, a symlink made
// in an earlier Bash call that is not on disk yet when the hook runs, a cd made in an earlier Bash
// call (only the hook's cwd and the cds of this call are known), and a script the job writes and runs.
func checkProtected(scripts []*script, _ string, cfg Config) string {
	if len(cfg.Protected) == 0 {
		return ""
	}
	s := newPathScope(scripts, cfg)
	s.inodes = &inodeSet{forms: s.protected}
	r := s.checkScripts(scripts)
	if s.expired {
		return "blocked by guard: the command is too complex to verify against agent-jobs state and configuration " +
			"(protected paths) within the guard's time budget or its symlink resolution budget; split it or write " +
			"the code to a file and run that."
	}
	return r
}

// stdinInterpreters may run a program they read from stdin. Options taking a value (`python3 -W
// ignore`, `sqlite3 -separator , x.db`, `node -r fs`) make their arguments ambiguous, so as a later
// pipe stage one is treated as reading its program from the pipe whatever its arguments.
var stdinInterpreters = map[string]bool{
	"python": true, "sqlite3": true, "perl": true, "node": true, "nodejs": true,
	"deno": true, "bun": true, "ruby": true, "php": true, "lua": true, "luajit": true,
	"osascript": true, "rscript": true,
}

// stdinInterpreter reports a program that may read its program from stdin.
func stdinInterpreter(name string) bool {
	return strings.HasPrefix(name, "python") || strings.HasPrefix(name, "pypy") || stdinInterpreters[name]
}

// interpSpec describes how an interpreter takes its program on the command line: short options
// that give it (code, a module or a file) and the subset giving inline code as the next word, short
// options with a value attached or next, short options with an attached value only (`perl -i.bak`,
// `-0777`), short options taking no value, and the same for long options. Any other option may take
// the next word as its value, so the guard does not know what follows it.
type interpSpec struct {
	codeShort, inlineShort, valueShort, attachShort, flagShort string
	codeLong, valueLong                                        []string
}

// interpSpecs are the interpreters whose inline code inlineCode finds by their options.
var interpSpecs = map[string]interpSpec{
	"python": {codeShort: "cm", inlineShort: "c", valueShort: "WX", flagShort: "bBdEIOPqsSu",
		valueLong: []string{"--check-hash-based-pycs"}},
	"perl": {codeShort: "eE", inlineShort: "eE", valueShort: "IMm", attachShort: "xiFCdD0", flagShort: "acnpslwtTUWX"},
	"node": {codeShort: "ep", inlineShort: "ep", valueShort: "rC", codeLong: []string{"--eval", "--print"},
		valueLong: []string{"--require", "--import", "--loader", "--experimental-loader", "--conditions", "--input-type"}},
	"ruby": {codeShort: "e", inlineShort: "e", valueShort: "IrCE", attachShort: "FKTWx0i", flagShort: "acdlnpswU"},
	"php":  {codeShort: "rfBRFE", inlineShort: "rBRE", valueShort: "cdz", flagShort: "eHlnqsw"},
}

// interpCode parses an interpreter call by its interpSpec and returns the args index of its inline
// program word (`-c <code>`, `--eval <code>`), -1 if none or after any option not in the spec (it
// may take the next word). It does not tell whether the interpreter also runs stdin (`node -e 0
// -i`, `node -p`): a pipe feeder into an interpreter is always scanned strictly.
func interpCode(name string, args []string) int {
	if strings.HasPrefix(name, "python") || strings.HasPrefix(name, "pypy") {
		name = "python"
	}
	spec, ok := interpSpecs[name]
	if !ok {
		return -1
	}
	inline := func(i int) int {
		if i < len(args) {
			return i
		}
		return -1
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--" || a == "-":
			return -1
		case strings.HasPrefix(a, "--"):
			opt, _, eq := strings.Cut(a, "=")
			switch {
			case slices.Contains(spec.codeLong, opt):
				if eq {
					return -1
				}
				return inline(i + 1)
			case slices.Contains(spec.valueLong, opt):
				if !eq {
					i++
				}
			default:
				return -1
			}
		case strings.HasPrefix(a, "-"):
		cluster:
			for j := 1; j < len(a); j++ {
				switch c := a[j]; {
				case strings.IndexByte(spec.codeShort, c) >= 0:
					if j == len(a)-1 && strings.IndexByte(spec.inlineShort, c) >= 0 {
						return inline(i + 1)
					}
					return -1
				case strings.IndexByte(spec.valueShort, c) >= 0:
					if j == len(a)-1 {
						i++
					}
					break cluster
				case strings.IndexByte(spec.attachShort, c) >= 0:
					break cluster
				case strings.IndexByte(spec.flagShort, c) < 0:
					return -1
				}
			}
		default:
			return -1
		}
	}
	return -1
}

func (s *pathScope) checkScripts(scripts []*script) string {
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for i, c := range pl.cmds {
				if s.over() {
					return ""
				}
				s.useLinks(c)
				v := split(c.words)
				// A redirection opens its file, also for reading (`exec 9<gitconfig`): the descriptor
				// is a second name of it (/proc/self/fd/9), so no redirection names a protected path.
				for _, t := range c.outRedirs {
					if p, ok := s.hit(t, false); ok {
						return protectedBlocked(p)
					}
					if alias, std := s.fdWord(t); alias && !std {
						return fdAliasBlocked(t)
					}
				}
				for _, t := range c.inRedirs {
					if r := s.inputRedirBlocked(t, slices.Contains(v.wrappers, "exec")); r != "" {
						return r
					}
				}
				argv := c.words[v.start:]
				if len(argv) == 0 {
					continue
				}
				name := progName(argv[0])
				if r := fdInCode(name, argv, c); r != "" {
					return r
				}
				if gitFileReaders[name] {
					continue
				}
				for _, a := range argv[1:] {
					if alias, std := s.fdWord(a); alias && (fdMeta[name] || !std) {
						return fdAliasBlocked(a)
					}
					if linkMakers[name] && s.fdRootWord(a) {
						return fdAliasBlocked(a)
					}
				}
				pyAll, pyDirs := pythonTreeTargets(name, argv)
				tree := treeWriters[name] || (name == "find" && findActs(argv[1:])) || fdWrites(name, argv) || pyAll
				for _, d := range pyDirs {
					if p, ok := s.hit(d, true); ok {
						if _, in := s.hit(d, false); !in {
							return ancestorBlocked(d, p)
						}
						return protectedBlocked(p)
					}
				}
				var ca copyArgs
				if destCopiers[name] {
					ca = parseCopyArgs(name, argv[1:])
				}
				for _, d := range ca.dests {
					if r := s.checkDest(d, ca); r != "" {
						return r
					}
				}
				for k, a := range argv[1:] {
					code := codeArg(argv, k+1)
					if bigText(a) && inlineCode(name, argv, k+1) {
						if h, ok := s.contains(a, true); ok {
							return h.reason()
						}
						continue
					}
					for _, d := range attachedDirs(name, a) {
						if p, ok := s.hit(d, true); ok {
							if _, in := s.hit(d, false); !in {
								return ancestorBlocked(a, p)
							}
							return protectedBlocked(p)
						}
					}
					if p, ok := s.hit(a, tree && !ca.destIdx[k]); ok {
						if _, in := s.hit(a, false); tree && !in {
							return ancestorBlocked(a, p)
						}
						return protectedBlocked(p)
					}
					if h, ok := s.contains(a, code); ok {
						return h.reason()
					}
					if name != "cd" && name != "pushd" && !dirReader(name, argv) {
						if p, ok := s.parentHit(a); ok {
							return protectedBlocked(p)
						}
					}
				}
				if h, ok := s.contains(c.heredoc, true); ok && c.heredoc != "" && !dataHeredoc(sc, pl, c) {
					return h.reason()
				}
				// Whatever its arguments, an interpreter may still run the pipe (`node -p`, `node -e 0
				// -i`): its feeder gets the strict scan, ancestors included (`ls ~ | python3 x.py` is
				// refused, a known false refusal).
				xargs := slices.Contains(v.wrappers, "xargs")
				if !xargs && !stdinInterpreter(name) {
					continue
				}
				words, fed := commandInputWords(c, pl.cmds[:i], s.over)
				if s.over() {
					return ""
				}
				feeder := ""
				switch {
				case xargs:
					feeder = "`xargs " + name + "`"
				case stdinInterpreter(name) && fed:
					feeder = "`" + name + "` (its program may come from the pipe)"
				}
				if feeder == "" {
					continue
				}
				for _, w := range words {
					if s.feederHit(w) {
						return "blocked by guard: " + feeder + " reads `" + w + "`, which names agent-jobs " +
							"state (" + protectedKinds + "); the guard cannot verify what it is fed."
					}
				}
			}
		}
	}
	return ""
}

func (s *pathScope) feederHit(w string) bool {
	w = strings.TrimSpace(w)
	if w == "" {
		return false
	}
	if !bigText(w) {
		if _, ok := s.hit(w, true); ok {
			return true
		}
	}
	if _, ok := s.contains(w, true); ok {
		return true
	}
	for _, t := range pathTokens(w) {
		if clean := filepath.Clean(t); clean == "/" || clean == "." {
			continue // division or punctuation inside code, rather than a path operand
		}
		if _, ok := s.hit(t, true); ok {
			return true
		}
	}
	return false
}

// Check returns a non-empty reason when the shell command must be blocked.
func Check(cmd string, cfg Config) string {
	if len(cmd) > maxCommandBytes {
		return commandSizeBlocked(len(cmd))
	}
	if p, bad := unresolvedRoot(cfg); bad {
		return rootBlocked(p)
	}
	scripts := parseAll(cmd)
	for _, check := range []func([]*script, string, Config) string{
		checkUnverifiable, checkNestedAgents, checkSelf, checkJobEnv, checkUserManager, checkPackageRunners, checkNothingOut, checkGitEnv, checkGitFiles, checkLinkScheduling, checkProtected, checkReadOnly, checkStateRead, checkStatusAfterPipe, checkSelfMatchingWait,
		checkOwnerInCommit, checkForeignIdentity,
	} {
		if r := check(scripts, cmd, cfg); r != "" {
			return r
		}
	}
	return ""
}

// eachCmd calls f with the real argv (keywords, assignments, wrappers stripped) of every command.
func eachCmd(scripts []*script, f func(c *simpleCmd, argv []string) string) string {
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				if r := f(c, argvOf(c.words)); r != "" {
					return r
				}
			}
		}
	}
	return ""
}

// gitCall is a parsed `git [global options] <sub> <args>`.
type gitCall struct {
	configs  []string // -c key=value
	cfgIdx   []int    // argv indices of the -c values
	sub      string
	subIdx   int // argv index of sub (len(argv) when there is none)
	args     []string
	attached string // first global option the guard cannot resolve (`-cK=V`, `-C.`, --config-env, --exec-path)
}

// isProg reports whether argv0 runs the program name (case-insensitive: macOS file systems are).
func isProg(argv0, name string) bool { return strings.EqualFold(base(argv0), name) }

// isGitName reports a lower-cased base name that runs git: `git` itself or a dashed form such as
// `git-push` (/usr/lib/git-core/git-push is `git push`).
func isGitName(name string) bool {
	return name == "git" || (strings.HasPrefix(name, "git-") && len(name) > 4)
}

// hasGlob reports a glob or brace character: the shell expands such a word into names the guard
// does not see (`{git,push}`, `/usr/bin/g[i]t`, `git pu?h` with a file named push).
func hasGlob(w string) bool { return strings.ContainsAny(w, "{[?*") }

// gitValueOpts are git global options that take a separate value.
var gitValueOpts = map[string]bool{
	"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true,
	"--super-prefix": true, "--config-env": true, "--exec-path": true, "--list-cmds": true, "--attr-source": true,
}

func parseGit(argv []string) (gitCall, bool) {
	var g gitCall
	if len(argv) == 0 {
		return g, false
	}
	name := progName(argv[0])
	if !isGitName(name) {
		return g, false
	}
	if name != "git" { // git-<sub>: the subcommand is in the program name
		g.sub, g.subIdx, g.args = name[len("git-"):], 0, argv[1:]
		return g, true
	}
	i := 1
	for i < len(argv) {
		a := argv[i]
		if !strings.HasPrefix(a, "-") {
			break
		}
		if ((strings.HasPrefix(a, "-c") || strings.HasPrefix(a, "-C")) && len(a) > 2) ||
			strings.HasPrefix(a, "--config-env") || strings.HasPrefix(a, "--exec-path") {
			if g.attached == "" {
				g.attached = a
			}
			i++
			continue
		}
		if gitValueOpts[a] && i+1 < len(argv) {
			if a == "-c" {
				g.configs = append(g.configs, argv[i+1])
				g.cfgIdx = append(g.cfgIdx, i+1)
			}
			i += 2
			continue
		}
		i++
	}
	g.subIdx = i
	if i < len(argv) {
		g.sub, g.args = argv[i], argv[i+1:]
	}
	return g, true
}

// ghPositional returns the argv indices of the first two positional words of a `gh` call.
// Before the subcommand only `-R`/`--repo`/`--hostname` with their value (separate, `=` or attached
// `-Rowner/repo`) are understood; any other flag there could take a value and shift which words are
// read as the subcommand, so it is returned as unknown (the caller rejects the call as unverifiable).
func ghPositional(argv []string) (pos []int, unknown string) {
	for i := 1; i < len(argv) && len(pos) < 2; i++ {
		a := argv[i]
		switch {
		case a == "-R" || a == "--repo" || a == "--hostname":
			i++
		case strings.HasPrefix(a, "--repo=") || strings.HasPrefix(a, "--hostname=") ||
			(strings.HasPrefix(a, "-R") && len(a) > 2):
		case (a == "--version" || a == "--help" || a == "-h") && len(pos) == 0 && i == len(argv)-1:
			// `gh --version` / `gh --help` alone only print.
		case strings.HasPrefix(a, "-"):
			return pos, a
		default:
			pos = append(pos, i)
		}
	}
	return pos, ""
}

// parseGh returns the first two positional words of a `gh` call (group, subcommand).
func parseGh(argv []string) (string, string, bool) {
	if len(argv) == 0 || !isProg(argv[0], "gh") {
		return "", "", false
	}
	idx, unknown := ghPositional(argv)
	if unknown != "" { // rejected as unverifiable first; never read as an allowed group
		return "unverifiable " + unknown, "", true
	}
	pos := []string{"", ""}
	for k, i := range idx {
		pos[k] = argv[i]
	}
	return pos[0], pos[1], true
}

func cannot(form string) string {
	return "blocked by guard: guard cannot verify this form (" + form + "); write it as a plain command " +
		"(no substitution, eval, sh -c, stdin/here-doc, variable or attached option around git/gh)."
}

// indirect are programs that, as an argument of another program (find -exec, flock, watch, …),
// would run something the guard does not see as a command.
var indirect = map[string]bool{
	"git": true, "gh": true, "eval": true, "env": true, "xargs": true, "exec": true, "command": true,
	"nohup": true, "sudo": true, "doas": true, "timeout": true, "nice": true, "stdbuf": true, "source": true,
	"npx": true, "pnpx": true, "bunx": true, "uvx": true,
}

// agents are coding agents a job must not start: a job is a leaf, nested agents would run outside
// the job's settings, hook and limits. Package names (`claude-code`, `gemini-cli`, `aider-chat`) and
// any other `<agent>-<suffix>` count too (isAgent).
var agents = map[string]bool{
	"claude": true, "claude-code": true, "codex": true, "gemini": true, "gemini-cli": true, "aider": true,
	"aider-chat": true, "cursor-agent": true,
}

// isAgent reports a lower-cased base name that is an agent or an agent with a `-suffix`.
func isAgent(b string) bool {
	if agents[b] {
		return true
	}
	for a := range agents {
		if strings.HasPrefix(b, a+"-") {
			return true
		}
	}
	return false
}

// execName reports a lower-cased base name that, as an argument, names something that runs commands
// the guard would not see: git (any git-<sub>), gh, a shell, a wrapper/indirect program, a package
// runner or an agent.
func execName(b string) bool {
	return indirect[b] || shells[b] || isAgent(b) || isGitName(b) || isSelf(b)
}

// isSelf reports a lower-cased base name that runs agent-jobs (also `agent-jobs-<suffix>` builds).
func isSelf(b string) bool { return b == "agent-jobs" || strings.HasPrefix(b, "agent-jobs-") }

// jobRoleMCP reports whether args (after the program name) are exactly `mcp --role job` with an
// optional --agent value: the one agent-jobs form a job may run (it is the job's own MCP server).
func jobRoleMCP(args []string) bool {
	if len(args) == 0 || args[0] != "mcp" {
		return false
	}
	roles, agent := 0, 0
	for k := 1; k < len(args); k++ {
		name, val, eq := strings.Cut(strings.TrimLeft(args[k], "-"), "=")
		if !strings.HasPrefix(args[k], "-") || (name != "role" && name != "agent") {
			return false
		}
		if !eq {
			if k+1 >= len(args) {
				return false
			}
			k++
			val = args[k]
		}
		switch name {
		case "role":
			if val != "job" {
				return false
			}
			roles++
		case "agent":
			agent++
		}
	}
	return roles == 1 && agent <= 1
}

// mentionsExec reports a word (e.g. a substitution `$(which git)`) that names an execName program.
func mentionsExec(w string) bool {
	return slices.ContainsFunc(strings.FieldsFunc(strings.ToLower(w), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}), execName)
}

// copiers place a file at another name; with git/gh/a shell/an agent as an argument they make a
// renamed executable (`cp /usr/bin/git ./x; ./x push`), so such a call is not data.
var copiers = map[string]bool{"cp": true, "mv": true, "install": true, "ln": true}

// dataReaders only print, match, inspect or move files named by their arguments: a word "git"
// there is data. Every program NOT positively classified as data-only (here, dataSubcommands or a
// python script file) has its arguments scanned, and a git/gh/shell word among them is rejected
// (fail closed: awk system(), vim -c '!…', docker exec, gdb -ex shell, sed …/e and any runner the
// guard does not know).
var dataReaders = map[string]bool{
	"echo": true, "printf": true, "grep": true, "egrep": true, "fgrep": true, "rg": true, "cat": true,
	"ls": true, "head": true, "tail": true, "wc": true, "file": true, "stat": true, "which": true,
	"type": true, "whereis": true, "man": true, "test": true, "[": true, "cd": true, "pgrep": true,
	"ps": true, "mkdir": true, "touch": true, "rm": true, "find": true,
}

// dataSubcommands are package managers whose listed subcommands only take package names.
var dataSubcommands = map[string]map[string]bool{
	"brew":    {"install": true, "info": true, "search": true, "list": true},
	"apt-get": {"install": true},
	"go":      {"get": true, "install": true},
	"pip":     {"install": true},
	"pip3":    {"install": true},
	"npm":     {"install": true},
}

// findExec are the find actions that run a command.
var findExec = map[string]bool{"-exec": true, "-execdir": true, "-ok": true, "-okdir": true}

// dataOnly reports whether name (lower-cased base name) with args is positively known to treat
// its arguments as data only.
func dataOnly(name string, args []string, dyn []bool) bool {
	switch {
	case copiers[name]:
		for k, a := range args {
			if !strings.HasPrefix(a, "-") && (execName(progName(a)) || (dyn[k] && mentionsExec(a))) {
				return false
			}
		}
		return scopeToolArg(name, args, dyn) == ""
	case name == "find":
		return !slices.ContainsFunc(args, func(a string) bool { return findExec[a] })
	case name == "rg": // --pre runs a command on every file
		return !slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--pre") && !strings.HasPrefix(a, "--pre-glob") })
	case name == "man": // -P / --pager / -H run a program
		return !slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "-") })
	case dataReaders[name]:
		return true
	case dataSubcommands[name] != nil:
		return len(args) > 0 && !dyn[0] && dataSubcommands[name][args[0]]
	case strings.HasPrefix(name, "python"):
		return pythonScriptFile(args, dyn)
	}
	return false
}

// pythonValueOpts are the python short options that take a value (attached or as the next word).
const pythonValueOpts = "WX"

// pythonScriptFile reports a plain `python3 [opts] script.py args…` call: no -c / -m anywhere
// (fail closed even after the script name), option values skipped, a literal script file.
func pythonScriptFile(args []string, dyn []bool) bool {
	for _, a := range args {
		if a == "--command" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsAny(a[1:], "cm")) {
			return false
		}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return i+1 < len(args) && !dyn[i+1] && args[i+1] != "-"
		case a == "-":
			return false // program from stdin
		case strings.HasPrefix(a, "--"):
			if a == "--check-hash-based-pycs" {
				i++
			}
		case strings.HasPrefix(a, "-"):
			for k := 1; k < len(a); k++ {
				if strings.IndexByte(pythonValueOpts, a[k]) >= 0 {
					if k == len(a)-1 {
						i++ // value is the next word
					}
					break
				}
			}
		default:
			return !dyn[i] // the script file; the rest are its arguments, i.e. data
		}
	}
	return false
}

var (
	// reMention finds git/gh, an agent or a shell as a word inside an argument, e.g. in interpreter code
	// (`python3 -c '…subprocess.run(["claude", …])'`, `node -e '…codex…'`) or a package path
	// (`npx @anthropic-ai/claude-code`, `node …/claude-code/cli.js`).
	// Agent names may carry a `-suffix` (`gemini-cli`, `aider-chat`, `@google/gemini-cli`, `codex-cli`).
	reMention = regexp.MustCompile(`(?i)(^|[^a-z0-9_.-])((git|gh|sh|bash|zsh|dash|fish)($|[^a-z0-9_-])|` +
		`(claude|codex|gemini|aider|cursor-agent|agent-jobs)($|[^a-z0-9_]))`)
	// reScopeMention finds an out-of-scope tool (outOfScopeTools, systemctl) as a word inside code
	// (`python3 -c '…["tmux", …]'`, `watch 'docker run …'`); not `at`, `batch`, `screen`: plain words.
	// It is not used on every argument: a package, directory or make target named ssh or docker is data.
	reScopeMention = regexp.MustCompile(`(?i)(^|[^a-z0-9_.-])((ssh)($|[^a-z0-9_-])|` +
		`(systemd-run|systemctl|busctl|dbus-send|gdbus|qdbus|dbus-run-session|crontab|tmux|pm2|docker|podman|` +
		`machinectl|loginctl|unshare|nsenter)($|[^a-z0-9_]))`)
	reGitDashCmd = regexp.MustCompile(`(?i)(^|[^a-z0-9_.-])git-[a-z]`)
)

// checkUnverifiable fails closed on every form whose effect on git/gh cannot be resolved statically.
func checkUnverifiable(scripts []*script, _ string, _ Config) string {
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				if r := unverifiable(sc, c); r != "" {
					return r
				}
			}
		}
	}
	return ""
}

func unverifiable(sc *script, c *simpleCmd) string {
	v := split(c.words)
	if v.splitString {
		return cannot("env -S")
	}
	if v.wrapperUnknown != "" {
		return cannot("unknown or ambiguous wrapper option " + v.wrapperUnknown)
	}
	if v.envUnknown != "" {
		return cannot("abbreviated or unknown env option " + v.envUnknown)
	}
	// A glob/brace in any word that decides which program runs (wrappers, their option values, the
	// timeout duration, the program itself) is expanded by the shell into words the guard never sees.
	// Leading keywords (`{`, `!`) and shell assignments (not glob-expanded) are exempt, as are `[`/`[[`.
	for i := v.lead; i <= v.start && i < len(c.words); i++ {
		if w := c.words[i]; w != "[" && w != "[[" && hasGlob(w) {
			return cannot("glob/brace in the command or its wrappers: " + w)
		}
	}
	if v.start >= len(c.words) {
		return ""
	}
	argv, dyn := c.words[v.start:], c.dyn[v.start:]
	name := progName(argv[0])
	if dyn[0] {
		return cannot("command name from a variable or substitution")
	}
	if name == "systemctl" && slices.Contains(v.wrappers, "xargs") {
		return cannot("xargs systemctl (its arguments, e.g. --user, come from stdin)")
	}
	if isGitName(name) || name == "gh" {
		switch {
		case sc.depth > 0 && !substGitRead(sc, argv):
			return cannot(name + " inside a substitution, eval or sh -c")
		case c.stdin:
			return cannot("stdin redirect, here-doc or here-string into " + name)
		case slices.Contains(v.wrappers, "xargs"):
			return cannot("xargs " + name)
		}
		if name != "gh" {
			g, _ := parseGit(argv)
			for k := 1; k <= g.subIdx && k < len(argv); k++ {
				if hasGlob(argv[k]) {
					return cannot("glob/brace in a git option or subcommand: " + argv[k])
				}
			}
			if g.attached != "" {
				return cannot("attached git option " + g.attached)
			}
			if g.subIdx < len(argv) && dyn[g.subIdx] {
				return cannot("git subcommand from a variable")
			}
			for _, k := range g.cfgIdx {
				if dyn[k] {
					return cannot("git -c value from a variable")
				}
			}
			return ""
		}
		pos, unknown := ghPositional(argv)
		if unknown != "" {
			return cannot("gh flag " + unknown + " before the subcommand (only -R/--repo/--hostname)")
		}
		for _, k := range pos {
			if dyn[k] {
				return cannot("gh subcommand from a variable")
			}
			if hasGlob(argv[k]) {
				return cannot("glob/brace in a gh subcommand: " + argv[k])
			}
		}
		return ""
	}
	switch {
	case name == "eval":
		// The text is parsed as a nested script (git there is rejected); a variable is not visible.
		if slices.Contains(dyn[1:], true) {
			return cannot("eval of a variable or substitution")
		}
	case shells[name]:
		cflag, file, stdin := false, false, false
		for k := 1; k < len(argv); k++ {
			a := argv[k]
			if strings.HasPrefix(a, "--") {
				cflag = cflag || a == "--command" || strings.HasPrefix(a, "--command=") // fish
				continue
			}
			if strings.HasPrefix(a, "-") && len(a) > 1 {
				cflag = cflag || strings.ContainsRune(a, 'c')
				stdin = stdin || strings.ContainsRune(a, 's')
				continue
			}
			file = true
			break
		}
		switch {
		case cflag:
			if slices.Contains(dyn[1:], true) {
				return cannot(name + " -c of a variable or substitution")
			}
		case !file || stdin:
			return cannot(name + " reading commands from stdin")
		}
	case name == "alias":
		for k, a := range argv[1:] {
			if dyn[k+1] || reMention.MatchString(a) {
				return cannot("alias")
			}
		}
	}
	if name == "eval" || dataOnly(name, argv[1:], dyn[1:]) {
		return ""
	}
	// Any other program (find -exec, flock, trap 'git push' EXIT, python -c, awk system(), vim -c '!…',
	// an unknown runner): the guard cannot see what it runs, so a git/gh (or shell/eval) word among its
	// arguments is rejected.
	for k, a := range argv[1:] {
		if b := progName(a); execName(b) || reMention.MatchString(a) || reGitDashCmd.MatchString(a) ||
			(dyn[k+1] && mentionsExec(a)) {
			return cannot(name + " running git/gh, a shell or an agent indirectly")
		}
	}
	if t := scopeToolArg(name, argv[1:], dyn[1:]); t != "" {
		return cannot(name + " running " + t + " (a tool that starts processes outside the job's scope) indirectly")
	}
	return ""
}

// scopeTool reports a lower-cased base name that starts processes outside the job's scope.
func scopeTool(b string) bool { return outOfScopeTools[b] || b == "systemctl" }

// execRunners run their positional arguments as a command (`setsid tmux …`, `flock f docker …`).
var execRunners = map[string]bool{
	"setsid": true, "watch": true, "flock": true, "chronic": true, "unbuffer": true, "strace": true, "ltrace": true,
	"ionice": true, "taskset": true, "chrt": true, "prlimit": true, "chroot": true, "runuser": true, "su": true,
	"script": true, "parallel": true, "entr": true, "systemd-inhibit": true, "caffeinate": true, "time": true,
	"daemonize": true, "start-stop-daemon": true, "firejail": true, "bwrap": true, "fakeroot": true, "proot": true,
	"faketime": true, "valgrind": true, "numactl": true, "cgexec": true, "sg": true, "expect": true,
}

// execOpts are options whose value is a command or code (`find -exec`, `rsync -e`, `go test -exec`,
// `perl -e`, `node -e`, `python -c`, `--rsh=`).
var execOpts = map[string]bool{
	"-exec": true, "-execdir": true, "-ok": true, "-okdir": true, "-e": true, "-c": true, "-x": true,
	"--exec": true, "--rsh": true, "--command": true, "--run": true, "--eval": true,
}

// scopeToolArg returns the out-of-scope tool an argument of name runs (as found in args), or "". Only the
// places where it runs count: the value of an exec option, a positional of an execRunner, an absolute
// path given to a copier (a renamed binary), a substitution naming it, or a word inside code (an
// argument with spaces, quotes or parentheses). A package, directory, make target or test name such as
// `./internal/ssh/...`, `make -C docker` or `-run Tmux` is data.
func scopeToolArg(name string, args []string, dyn []bool) string {
	data := runnerDataValues(name, args)
	for k, a := range args {
		if data[k] {
			continue
		}
		v := optValue(a)
		b := progName(v)
		opt, _, _ := strings.Cut(a, "=")
		switch {
		case dyn[k] && reScopeMention.MatchString(a):
		case scopeTool(b) && v != a && execOpts[opt]:
		case scopeTool(b) && k > 0 && execOpts[args[k-1]]:
		case scopeTool(b) && execRunners[name] && !strings.HasPrefix(a, "-"):
		case scopeTool(b) && copiers[name] && filepath.IsAbs(v):
		case strings.ContainsAny(a, " \t\n'\"(){};`") && reScopeMention.MatchString(a):
		default:
			continue
		}
		return a
	}
	return ""
}

// packageRunners fetch and run a package by name (npx, pnpm dlx, …): what runs is decided by a
// registry, not by the command line, and a job has no need for them. Value: the subcommand words
// that make the program a runner ("" = the program itself).
var packageRunners = map[string][]string{
	"npx": {""}, "pnpx": {""}, "bunx": {""}, "uvx": {""},
	"npm": {"exec", "x"}, "pnpm": {"dlx"}, "yarn": {"dlx"}, "bun": {"x"}, "pipx": {"run"}, "uv": {"tool run"},
}

// checkPackageRunners rejects package runners: npx/pnpx/bunx/uvx, npm exec|x, pnpm dlx, yarn dlx,
// bun x, pipx run, uv tool run. The subcommand is looked for among the first three positional words
// (an option value before it would otherwise hide it).
func checkPackageRunners(scripts []*script, _ string, _ Config) string {
	return eachCmd(scripts, func(_ *simpleCmd, argv []string) string {
		if len(argv) == 0 {
			return ""
		}
		name := progName(argv[0])
		subs, ok := packageRunners[name]
		if !ok {
			return ""
		}
		var pos []string
		for _, a := range argv[1:] {
			if !strings.HasPrefix(a, "-") && len(pos) < 3 {
				pos = append(pos, strings.ToLower(a))
			}
		}
		joined := " " + strings.Join(pos, " ") + " "
		for _, s := range subs {
			if s == "" || strings.Contains(joined, " "+s+" ") {
				return "blocked by guard: `" + strings.TrimSpace(name+" "+s) + "` fetches and runs a package by name " +
					"(npx, pnpm dlx, yarn dlx, bunx, uvx, pipx run …); a job does not use package runners."
			}
		}
		return ""
	})
}

// checkNestedAgents: a job is a leaf. claude/claude-code/codex/gemini/aider/cursor-agent are rejected as a
// command (also behind env/command/exec and as a path); as an argument they are caught by the
// indirect scan in unverifiable.
func checkNestedAgents(scripts []*script, _ string, _ Config) string {
	return eachCmd(scripts, func(_ *simpleCmd, argv []string) string {
		if len(argv) > 0 && isAgent(progName(argv[0])) {
			return "blocked by guard: `" + base(argv[0]) + "` starts a nested agent. A job is a leaf: it does the work itself " +
				"and reports; only the orchestrator starts jobs."
		}
		return ""
	})
}

// outOfScopeTools start processes outside the job's cgroup scope: the user's service manager and session
// bus (a new transient unit), and daemons or schedulers that run a command for the caller in their own
// cgroup (cron, at, tmux/screen servers, pm2, containers, ssh, logind/machined), or a new namespace that
// blinds the cgroup check (unshare, nsenter). Owner decision 2026-10-17 (docs/DECISIONS.md).
var outOfScopeTools = map[string]bool{
	"systemd-run": true, "busctl": true, "dbus-send": true, "gdbus": true, "qdbus": true, "qdbus-qt5": true,
	"qdbus6": true, "dbus-run-session": true,
	"crontab": true, "at": true, "batch": true, "tmux": true, "screen": true, "pm2": true, "docker": true,
	"docker-compose": true, "podman": true, "podman-compose": true, "ssh": true, "machinectl": true,
	"loginctl": true, "unshare": true, "nsenter": true,
}

// cgroupControlFiles are the cgroup v2 core files: writing there moves processes between cgroups
// (procs, threads), kills or freezes a cgroup, or changes a subtree, wherever the file system is mounted.
var cgroupControlFiles = map[string]bool{
	"cgroup.procs": true, "cgroup.threads": true, "cgroup.subtree_control": true, "cgroup.kill": true,
	"cgroup.freeze": true, "cgroup.type": true, "cgroup.max.depth": true, "cgroup.max.descendants": true,
}

// pathSegs returns the non-empty segments of a cleaned slash path.
func pathSegs(p string) []string {
	var out []string
	for _, s := range strings.Split(filepath.ToSlash(filepath.Clean(p)), "/") {
		if s != "" && s != "." {
			out = append(out, s)
		}
	}
	return out
}

// hasSegs reports whether want occurs as consecutive segments of segs.
func hasSegs(segs []string, want ...string) bool {
	for i := 0; i+len(want) <= len(segs); i++ {
		if slices.Equal(segs[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

// viaProcRoot reports a path through /proc/<pid>/root or /proc/<pid>/cwd (another process's view of
// the file system, which a lexical check cannot follow).
func viaProcRoot(segs []string) bool {
	for i := 0; i+2 < len(segs); i++ {
		if segs[i] == "proc" && (segs[i+2] == "root" || segs[i+2] == "cwd") {
			return true
		}
	}
	return false
}

// optValue returns the value of `--opt=value`, else w.
func optValue(w string) string {
	if strings.HasPrefix(w, "-") {
		if _, v, ok := strings.Cut(w, "="); ok {
			return v
		}
	}
	return w
}

// pathForms returns the spellings of a word a cgroup check must see: the value (of `--opt=value`) as
// written and cleaned, made absolute against dir when dir is known, and with symlinks resolved. A path
// whose symlinks take more than the resolution budget may lead anywhere: its resolved form is the
// cgroup file system itself (fail closed).
func pathForms(w, dir string) [][]string {
	w = optValue(w)
	if w == "" {
		return nil
	}
	forms := [][]string{pathSegs(w)}
	if dir != "" || filepath.IsAbs(w) {
		abs := w
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(dir, abs)
		}
		r, ok := resolveBounded(abs, "/")
		if !ok {
			r = "/sys/fs/cgroup"
		}
		forms = append(forms, pathSegs(abs), pathSegs(r))
	}
	return forms
}

// cgroupPath reports a word naming the cgroup file system or one of its control files in the spellings:
// a sys/fs/cgroup segment run (also after `//`, `/./`, `x/..`, relative to the call's directory, or
// through a symlink), a cgroup core file base name (cgroupControlFiles), or any path through
// /proc/<pid>/root or /proc/<pid>/cwd.
func cgroupPath(w, dir string) bool {
	for _, segs := range pathForms(w, dir) {
		if len(segs) == 0 {
			continue
		}
		if hasSegs(segs, "sys", "fs", "cgroup") || cgroupControlFiles[segs[len(segs)-1]] || viaProcRoot(segs) {
			return true
		}
	}
	return false
}

// sysDirTarget reports a word that is /sys or /sys/fs (from there a relative path reaches the cgroup
// file system unseen; as a symlink target it is a new name for it), absolute or relative to dir. With
// root, / itself counts too (`ln -s / r` makes r/sys/fs/cgroup).
func sysDirTarget(w, dir string, root bool) bool {
	if dir == "" && !filepath.IsAbs(optValue(w)) {
		return false
	}
	forms := pathForms(w, dir)
	if len(forms) == 0 { // an empty word (`touch ''`)
		return false
	}
	for _, segs := range forms[1:] {
		if slices.Equal(segs, []string{"sys"}) || slices.Equal(segs, []string{"sys", "fs"}) || (root && len(segs) == 0) {
			return true
		}
	}
	return false
}

func cgroupBlocked(w string) string {
	return "blocked by guard: `" + w + "` reaches the cgroup file system; a job does not move processes out of its scope."
}

func unresolvedBlocked(w string) string {
	return "blocked by guard: guard cannot verify the write target `" + w + "`: it is computed in this call (a " +
		"substitution, a variable set here, a positional or indirect expansion, a symlink to a variable target), or " +
		"it is a variable, `~user` or a path relative after a `cd` the guard cannot follow in a call that mentions the " +
		"cgroup file system; write the path out literally."
}

var (
	// reCgroupMention is a call that names the cgroup file system or /sys in some spelling: an
	// unresolvable write target there could land in it.
	reCgroupMention = regexp.MustCompile(`(?i)cgroup|(^|[^a-z0-9_.-])sys/|/sys($|[^a-z0-9_.-])|/proc/[^/\s]+/(root|cwd)`)
	// reMaybeEmpty is an expansion that may be empty: `${…}`, `$NAME`, a positional or special
	// parameter (an unset one splits a literal path unseen: `/sy${N}s/fs/cg"$N"roup`).
	reMaybeEmpty = regexp.MustCompile(`\$\{[^}]*\}|\$[A-Za-z_][A-Za-z0-9_]*|\$[0-9@*#?!-]`)
	// reAnsiEscape is ANSI-C quoting with an escape (`$'\x73ys'`): its value is not decoded here.
	reAnsiEscape = regexp.MustCompile(`\$'[^']*\\`)
	// reHomeSet is a call that may change HOME (then `~` is not the guard's HOME).
	reHomeSet = regexp.MustCompile(`(^|[^$A-Za-z0-9_{])HOME([^A-Za-z0-9_]|$)`)
)

// cgroupMention reports a call that names the cgroup file system or /sys (reCgroupMention) as written,
// with ANSI-C quoting that has an escape, or once quotes, backslashes and expansions that may be empty
// are removed (`/sy${N}s/fs/cg"$N"roup`, `/sy$'s'/fs`).
func cgroupMention(cmd string) bool {
	if reCgroupMention.MatchString(cmd) || reAnsiEscape.MatchString(cmd) {
		return true
	}
	s := strings.NewReplacer("$'", "'", `$"`, `"`).Replace(cmd)
	s = reMaybeEmpty.ReplaceAllString(s, "")
	s = strings.NewReplacer(`"`, "", "'", "", `\`, "").Replace(s)
	return reCgroupMention.MatchString(s)
}

// cgroupWriters are programs whose arguments name files they write or create: a dynamic argument of one
// of them is an unresolvable write target.
var cgroupWriters = map[string]bool{
	"tee": true, "dd": true, "cp": true, "mv": true, "install": true, "ln": true, "rsync": true, "truncate": true,
	"sed": true, "mkdir": true, "rmdir": true, "touch": true, "chmod": true, "chown": true, "mkfifo": true,
	"mknod": true, "rm": true,
}

// cdView resolves the directories a call may change to (cd/pushd/popd in any script of the call, in any
// order: a cd inside a subshell or after a failing one still counts, fail closed).
type cdView struct {
	dirs    []string // candidate working directories ("" = the call's directory is unknown: lexical checks only)
	unknown bool     // some cd target cannot be resolved statically (variable, `cd -`, popd, CDPATH, `~user`)
	sawCD   bool
	// PWD, OLDPWD and DIRSTACK change with a cd; they count as set in the call (localVars).
	home string // HOME for `~` ("" = not usable)
}

// maxCdDirs bounds the candidate directories (each cd multiplies them).
const maxCdDirs = 64

// tilde expands a leading `~` / `~/…` with the guard's HOME; ok is false for `~user`, `~+`, `~-` or
// when HOME may be changed by the call.
func (v *cdView) tilde(w string) (string, bool) {
	if !strings.HasPrefix(w, "~") {
		return w, true
	}
	if v.home == "" || (w != "~" && !strings.HasPrefix(w, "~/")) {
		return "", false
	}
	return v.home + w[1:], true
}

// cdTarget returns the directory argument of a cd/pushd/popd argv; ok is false when it cannot be
// resolved statically.
func (v *cdView) cdTarget(argv []string, dyn []bool, cdpath bool) (string, bool) {
	name := progName(argv[0])
	if name == "popd" {
		return "", false
	}
	for k := 1; k < len(argv); k++ {
		a := argv[k]
		if a == "--" {
			continue
		}
		if a == "-" || strings.HasPrefix(a, "+") || (name == "pushd" && strings.HasPrefix(a, "-") && len(a) > 1 && a[1] >= '0' && a[1] <= '9') {
			return "", false // OLDPWD, the directory stack
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			continue // -L, -P, -e, -@, -n
		}
		if dyn[k] {
			return "", false
		}
		t, ok := v.tilde(a)
		if !ok {
			return "", false
		}
		if cdpath && !filepath.IsAbs(t) && t != "." && t != ".." && !strings.HasPrefix(t, "./") && !strings.HasPrefix(t, "../") {
			return "", false
		}
		return t, true
	}
	if name == "pushd" {
		return "", false // swaps the top two directories
	}
	return v.home, v.home != "" // cd alone: HOME
}

// cdEnv is what a cd in cmd depends on beyond its argument: HOME for `cd` and `~` ("" when the call may
// set HOME itself), and whether CDPATH or cdable_vars may move a bare relative target.
func cdEnv(cmd string) (home string, cdpath bool) {
	if h := os.Getenv("HOME"); filepath.IsAbs(h) && !reHomeSet.MatchString(cmd) {
		home = filepath.Clean(h)
	}
	return home, strings.Contains(cmd, "CDPATH") || os.Getenv("CDPATH") != "" || strings.Contains(cmd, "cdable_vars")
}

func newCdView(scripts []*script, cmd string, cfg Config) cdView {
	v := cdView{dirs: []string{cfg.Dir}}
	home, cdpath := cdEnv(cmd)
	v.home = home
	trailing := trailingPopd(scripts)
	var targets []string
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				st := split(c.words).start
				argv, dyn := c.words[st:], c.dyn[st:]
				if len(argv) == 0 {
					continue
				}
				switch progName(argv[0]) {
				case "cd", "pushd", "popd", "chdir":
					v.sawCD = true
					if c == trailing {
						continue // nothing runs after it: where it goes does not matter
					}
					if t, ok := v.cdTarget(argv, dyn, cdpath); ok {
						targets = append(targets, t)
					} else {
						v.unknown = true
					}
				}
			}
		}
	}
	// Every order of the targets: repeat until no new directory appears (bounded).
	for round := 0; round <= len(targets) && !v.unknown; round++ {
		grown := false
		for _, t := range targets {
			for _, d := range v.dirs {
				nd := filepath.Clean(t)
				if !filepath.IsAbs(t) {
					if d == "" {
						continue // the call's directory is unknown: relative targets stay lexical
					}
					nd = filepath.Join(d, t)
				}
				if !slices.Contains(v.dirs, nd) {
					if len(v.dirs) >= maxCdDirs {
						v.unknown = true
						break
					}
					v.dirs, grown = append(v.dirs, nd), true
				}
			}
		}
		if !grown {
			break
		}
	}
	return v
}

// trailingPopd returns the popd that is the very last command of the call (`pushd d && make > log &&
// popd`), or nil. A trap may run code after it, so a call with a trap has none; a popd inside a loop or a
// function body is followed by `done` or `}`, so it is not last.
func trailingPopd(scripts []*script) *simpleCmd {
	if len(scripts) == 0 || len(scripts[0].pipelines) == 0 {
		return nil
	}
	if eachCmd(scripts, func(_ *simpleCmd, argv []string) string {
		if len(argv) > 0 && progName(argv[0]) == "trap" {
			return "trap"
		}
		return ""
	}) != "" {
		return nil
	}
	pls := scripts[0].pipelines
	cmds := pls[len(pls)-1].cmds
	c := cmds[len(cmds)-1]
	if argv := argvOf(c.words); len(argv) > 0 && progName(argv[0]) == "popd" {
		return c
	}
	return nil
}

var (
	// reVarRef is a variable expansion `$NAME` / `${NAME…}`.
	reVarRef = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)`)
	// reSpecialRef is a positional (`$1`, `$@`, `$*`) or indirect (`${!X}`) expansion: set by the call.
	reSpecialRef = regexp.MustCompile(`\$\{?[0-9@*]|\$\{!`)
	// reDefaultAssign is `${NAME:=…}` / `${NAME=…}`: an expansion that assigns NAME.
	reDefaultAssign = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*):?=`)
	// reLocalAssign is an assignment word `NAME=`, `NAME+=`, `NAME[i]=`.
	reLocalAssign = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)(\[[^]]*\])?\+?=`)
	reIdent       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// varNamers name the variables they set in their arguments (assignments, bare names, attached `-aNAME`).
var varNamers = map[string]bool{
	"export": true, "declare": true, "typeset": true, "local": true, "readonly": true, "read": true,
	"mapfile": true, "readarray": true, "getopts": true,
}

// localVars returns the variables a call may set itself (assignments, export/declare/local/typeset/readonly
// with a value or not, read, mapfile/readarray, getopts, for/select, printf -v, `${NAME:=…}`, a cd's PWD);
// all is true when any variable may be set unseen (`.`/`source` a file, a `declare -n` name reference).
func localVars(scripts []*script, cmd string, sawCD bool) (names map[string]bool, all bool) {
	names = map[string]bool{}
	add := func(w string) {
		if m := reLocalAssign.FindStringSubmatch(w); m != nil {
			names[m[1]] = true
		} else if reIdent.MatchString(w) {
			names[w] = true
		}
	}
	for _, m := range reDefaultAssign.FindAllStringSubmatch(cmd, -1) {
		names[m[1]] = true
	}
	if sawCD {
		names["PWD"], names["OLDPWD"], names["DIRSTACK"] = true, true, true
	}
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				v := split(c.words)
				for _, w := range c.words[:v.start] { // prefix assignments, `env NAME=…`
					if m := reLocalAssign.FindStringSubmatch(w); m != nil {
						names[m[1]] = true
					}
				}
				argv := c.words[v.start:]
				if len(argv) == 0 {
					continue
				}
				add(argv[0]) // `a[0]=x`, `a+=x` are not split off as assignments
				switch name := progName(argv[0]); {
				case name == "." || name == "source":
					all = true
				case name == "for" || name == "select":
					if len(argv) > 1 {
						add(argv[1])
					}
					names["REPLY"] = true
				case name == "printf":
					if t := printfTarget(argv[1:]); t != "" {
						add(strings.SplitN(t, "[", 2)[0])
					}
				case varNamers[name]:
					names["REPLY"], names["MAPFILE"], names["OPTARG"] = true, true, true
					for _, a := range argv[1:] {
						if strings.HasPrefix(a, "-") {
							if (name == "declare" || name == "typeset" || name == "local") && strings.ContainsRune(a, 'n') {
								all = true // name reference: assigning it assigns another variable
							}
							if len(a) > 2 {
								add(a[2:])
							}
							continue
						}
						add(a)
					}
				}
			}
		}
	}
	return names, all
}

// computedTarget reports a dynamic word whose value the call itself produces: a command or process
// substitution, a positional, indirect or `$_` expansion, or a variable the call sets (localVars).
// A plain variable from the environment (`"$OUT"`) is not computed.
func computedTarget(w string, names map[string]bool, all bool) bool {
	if strings.Contains(w, "$(") || strings.ContainsAny(w, "`") || strings.Contains(w, "<(") ||
		strings.Contains(w, ">(") || reSpecialRef.MatchString(w) {
		return true
	}
	for _, m := range reVarRef.FindAllStringSubmatch(w, -1) {
		if all || m[1] == "_" || names[m[1]] {
			return true
		}
	}
	return false
}

// dynLinkCmds returns the ln commands of the call with a dynamic argument: after such a link any
// literal path may lead into the cgroup file system (`ln -s "$S" x && echo 1 > x/y/pids.max`).
func dynLinkCmds(scripts []*script) map[*simpleCmd]bool {
	out := map[*simpleCmd]bool{}
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				st := split(c.words).start
				if st < len(c.words) && progName(c.words[st]) == "ln" && slices.Contains(c.dyn[st+1:], true) {
					out[c] = true
				}
			}
		}
	}
	return out
}

// checkUserManager: a job stays in its cgroup scope. The outOfScopeTools (service manager and session
// bus clients, cron/at, tmux/screen, pm2, docker/podman, ssh, machinectl/loginctl, unshare/nsenter) are
// rejected as commands (behind wrappers and xargs too: argvOf strips env/sudo/timeout/xargs/…); where
// another program runs them (find -exec, setsid, python -c, a copied binary) they and systemctl are
// caught by scopeToolArg in unverifiable. `systemctl --user` (also abbreviated `--u`, `--us`, `--use`,
// `--user=…`) is rejected (the system manager, without --user, cannot start units for the job). Then the
// cgroup file system (checkCgroupWrites). Not a sandbox: a script the job writes is not inspected.
func checkUserManager(scripts []*script, cmd string, cfg Config) string {
	if r := eachCmd(scripts, func(_ *simpleCmd, argv []string) string {
		if len(argv) == 0 {
			return ""
		}
		name := progName(argv[0])
		if outOfScopeTools[name] {
			return "blocked by guard: `" + name + "` starts processes outside the job's cgroup scope (service manager, " +
				"session bus, scheduler, terminal multiplexer, process manager, container, ssh or namespace); a job runs " +
				"its processes itself, inside its scope."
		}
		if name == "systemctl" {
			for _, a := range argv[1:] {
				opt, _, _ := strings.Cut(a, "=")
				if len(opt) >= 3 && strings.HasPrefix("--user", opt) {
					return "blocked by guard: `systemctl --user` controls the user's service manager; a job does not start " +
						"or stop units."
				}
			}
		}
		return ""
	}); r != "" {
		return r
	}
	return checkCgroupWrites(scripts, cmd, cfg)
}

// checkCgroupWrites rejects any write into the cgroup file system: an output redirection there, or such
// a path as an argument of a program that is not a plain reader (cgroupPath), checked
// against every directory a cd/pushd of the call may have moved to (cdView); a cd/pushd into /sys,
// /sys/fs or the cgroup file system, and a symlink to /, /sys or /sys/fs. Fail closed: a redirection
// target or a writer's argument (cgroupWriters) computed in the call (computedTarget) or written after an
// ln with a dynamic argument is rejected in any call; any variable, `~user` or relative-after-an-
// unresolvable-cd target is rejected when the call mentions the cgroup file system (cgroupMention) or
// has a cd the guard cannot resolve. A sed script is not a write target unless it may hold a w/W file
// (sedScriptArgs, reSedWrite); sed's file operands are.
func checkCgroupWrites(scripts []*script, cmd string, cfg Config) string {
	v := newCdView(scripts, cmd, cfg)
	for _, d := range v.dirs[1:] {
		if sysDirTarget(d, "/", false) || cgroupPath(d, "/") {
			return cgroupBlocked("cd " + d)
		}
	}
	if v.sawCD && strings.Contains(cmd, "CDPATH") {
		return "blocked by guard: a cd with CDPATH set in the same call resolves where the guard cannot see; " +
			"cd to an absolute or ./relative path."
	}
	risky := v.unknown || cgroupMention(cmd)
	names, allVars := localVars(scripts, cmd, v.sawCD)
	links := dynLinkCmds(scripts)
	var cur *simpleCmd
	// check reports a word that reaches the cgroup file system, or that is unresolvable: computed in the
	// call, written after a symlink to a dynamic target, or any variable in a risky call.
	check := func(w string, dyn, write bool) string {
		if write && len(links) > 0 && !links[cur] && !isDigits(w) && !strings.HasPrefix(w, "-") && w != "/dev/null" {
			return unresolvedBlocked(w)
		}
		if dyn {
			if write && (risky || computedTarget(w, names, allVars)) {
				return unresolvedBlocked(w)
			}
			if write {
				// An unset variable may hide a cgroup control file name (`x/cgroup${N}.procs`).
				for _, d := range v.dirs {
					if cgroupPath(reMaybeEmpty.ReplaceAllString(w, ""), d) {
						return cgroupBlocked(w)
					}
				}
			}
			return ""
		}
		t, ok := v.tilde(optValue(w))
		if !ok {
			if write && risky {
				return unresolvedBlocked(w)
			}
			return ""
		}
		if t != optValue(w) {
			w = t
		}
		if v.unknown && write && !filepath.IsAbs(w) && !strings.HasPrefix(w, "-") && !isDigits(w) {
			return unresolvedBlocked(w)
		}
		for _, d := range v.dirs {
			if cgroupPath(w, d) {
				return cgroupBlocked(w)
			}
		}
		return ""
	}
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				cur = c
				for k, t := range c.outRedirs {
					if r := check(t, c.outDyn[k], true); r != "" {
						return r
					}
				}
				st := split(c.words).start
				argv, dyn := c.words[st:], c.dyn[st:]
				if len(argv) == 0 {
					continue
				}
				name := progName(argv[0])
				if gitFileReaders[name] {
					continue
				}
				var sedArgs map[int]bool
				if name == "sed" {
					sedArgs = sedScriptArgs(argv)
				}
				for k := 1; k < len(argv); k++ {
					a := argv[k]
					write := cgroupWriters[name]
					if script, ok := sedArgs[k]; ok {
						write = script && reSedWrite.MatchString(a)
					}
					if name == "cd" || name == "pushd" || name == "popd" || name == "chdir" {
						if !dyn[k] && cgroupPath(a, "") {
							return cgroupBlocked(a)
						}
						continue // resolved by cdView
					}
					if r := check(a, dyn[k], write); r != "" {
						return r
					}
					if !dyn[k] {
						for _, d := range v.dirs {
							if sysDirTarget(a, d, name == "ln") {
								return cgroupBlocked(a)
							}
						}
					}
				}
			}
		}
	}
	return ""
}

// reSedWrite is a sed script that may hold a w/W command or an s///w flag (a file sed writes): a w at the
// start or after `;`, a brace, a newline, an address, `!` or a closing delimiter with flags. Approximate,
// errs toward a write.
var reSedWrite = regexp.MustCompile(`(^|[;{}\n/0-9$!,]\s*|/[gpiImMe0-9]*)[wW]`)

// sedScriptArgs returns the indices of sed's arguments that are not file operands: true for a script
// (the first operand without -e/-f, the value of -e/--expression), false for an option value that is no
// write target (-f/--file, -l/--line-length, an empty BSD -i suffix).
func sedScriptArgs(argv []string) map[int]bool {
	out := map[int]bool{}
	explicit, operand := false, false
	firstOperand := func(k int) {
		if !explicit && !operand {
			out[k], operand = true, true
		}
	}
	for k := 1; k < len(argv); k++ {
		a := argv[k]
		switch {
		case a == "--":
			for k++; k < len(argv); k++ {
				firstOperand(k)
			}
		case strings.HasPrefix(a, "--"):
			opt, _, hasVal := strings.Cut(a[2:], "=")
			switch opt {
			case "expression", "file":
				explicit = true
				if !hasVal && k+1 < len(argv) {
					k++
				}
				out[k] = opt == "expression"
			case "line-length":
				if !hasVal && k+1 < len(argv) {
					k++
					out[k] = false
				}
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for j := 1; j < len(a); j++ {
				c := a[j]
				if c == 'i' { // the rest is the backup suffix; BSD takes it as the next word (`-i ''`)
					if j == len(a)-1 && k+1 < len(argv) && argv[k+1] == "" {
						k++
						out[k] = false
					}
					break
				}
				if c == 'e' || c == 'f' || c == 'l' { // the rest, or the next word, is the value
					explicit = explicit || c != 'l'
					if j == len(a)-1 && k+1 < len(argv) {
						k++
					}
					out[k] = c == 'e'
					break
				}
			}
		default:
			firstOperand(k)
		}
	}
	return out
}

// checkSelf: no nested jobs. agent-jobs as a command (by name or path, behind env/command/exec/nohup,
// inside sh -c, eval or a substitution) is rejected unless it is exactly `agent-jobs mcp --role job`;
// as an argument of another program it is caught by the indirect scan in unverifiable.
func checkSelf(scripts []*script, _ string, _ Config) string {
	return eachCmd(scripts, func(_ *simpleCmd, argv []string) string {
		if len(argv) > 0 && isSelf(progName(argv[0])) && !jobRoleMCP(argv[1:]) {
			return "blocked by guard: `" + base(argv[0]) + "` controls jobs. A job never starts, cancels or manages jobs " +
				"(no nested jobs); only the orchestrator does."
		}
		return ""
	})
}

var (
	// jobEnv are the agent-jobs variables: AGENT_JOBS_JOB marks every process of a job, the others
	// select state (AGENT_JOBS_HOME, AGENT_JOBS_AGENT, binaries).
	reJobVar    = regexp.MustCompile(`^AGENT_JOBS_[A-Za-z0-9_]*$`)
	reJobAssign = regexp.MustCompile(`^AGENT_JOBS_[A-Za-z0-9_]*\+?=`)
	reJobExpand = regexp.MustCompile(`\$\{AGENT_JOBS_[A-Za-z0-9_]*:?=`)
)

// varSetters take bare variable names that they set, export, remove or turn into an array
// (`mapfile`/`readarray NAME`: an array is never exported, so the marker would vanish from children).
// printf is not here: only `printf -v NAME` assigns (printfTarget).
var varSetters = map[string]bool{
	"unset": true, "export": true, "declare": true, "typeset": true, "local": true, "readonly": true,
	"read": true, "env": true, "mapfile": true, "readarray": true,
}

// printfTarget returns the variable of `printf -v NAME` / `printf -vNAME` ("" when there is none).
func printfTarget(args []string) string {
	for k := 0; k < len(args); k++ {
		a := args[k]
		switch {
		case a == "--" || !strings.HasPrefix(a, "-"):
			return ""
		case a == "-v":
			if k+1 < len(args) {
				return args[k+1]
			}
			return ""
		case strings.HasPrefix(a, "-v"):
			return a[2:]
		}
	}
	return ""
}

// checkJobEnv keeps the job marker: an unset (`unset`, `env -u`), an assignment (prefix, export,
// declare, ${VAR:=…}), `printf -v`, or a setter (also `mapfile`/`readarray`, also behind
// `builtin`/`command`) naming an AGENT_JOBS_* variable – bare or attached to an option (`-vX`, `-tX`)
// – is rejected in any call. Emptying the whole environment (`env -i`) is rejected by checkGitEnv.
func checkJobEnv(scripts []*script, _ string, _ Config) string {
	blocked := func(what string) string {
		return "blocked by guard: `" + what + "` changes the agent-jobs environment of the job (AGENT_JOBS_JOB marks " +
			"the job's processes). Leave AGENT_JOBS_* unchanged."
	}
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				v := split(c.words)
				for _, u := range v.envUnset {
					if reJobVar.MatchString(u) {
						return blocked("env -u " + u)
					}
				}
				name := ""
				if v.start < len(c.words) {
					name = progName(c.words[v.start])
				}
				if name == "printf" {
					if t := printfTarget(c.words[v.start+1:]); strings.HasPrefix(t, "AGENT_JOBS_") {
						return blocked("printf -v " + t)
					}
				}
				setter := varSetters[name]
				for k, w := range c.words {
					if reJobAssign.MatchString(w) || reJobExpand.MatchString(w) {
						n, _, _ := strings.Cut(w, "=")
						return blocked(n)
					}
					if !setter || k <= v.start {
						continue
					}
					if reJobVar.MatchString(w) || (strings.HasPrefix(w, "-") && strings.Contains(w, "AGENT_JOBS_")) {
						return blocked(name + " " + w)
					}
				}
			}
		}
	}
	return ""
}

var (
	// scrubbed are the variables the wrapper sets for git (GIT_CONFIG_GLOBAL, GIT_CONFIG_NOSYSTEM,
	// GIT_CONFIG_COUNT/KEY/VALUE for core.hooksPath, GIT_PAGER, GIT_EDITOR, GIT_TERMINAL_PROMPT, PAGER,
	// EDITOR, VISUAL) or that redirect git (GIT_DIR, GIT_EXEC_PATH, GIT_SSH_COMMAND, …).
	reScrubVar    = regexp.MustCompile(`^(GIT_[A-Za-z0-9_]*|PAGER|EDITOR|VISUAL)$`)
	reScrubAssign = regexp.MustCompile(`^(GIT_[A-Za-z0-9_]*|PAGER|EDITOR|VISUAL)\+?=`)
	reScrubExpand = regexp.MustCompile(`\$\{(GIT_[A-Za-z0-9_]*|PAGER|EDITOR|VISUAL):?=`)
)

func envBlocked(what string) string {
	return "blocked by guard: `" + what + "` changes the git environment the job runs with (own config, no hooks, " +
		"cat/true as pager and editor). Run git with the environment unchanged."
}

// checkGitEnv keeps the wrapper's git environment intact: an emptied environment (`env -i`, `env -`,
// `exec -c`) or an unset scrubbed variable (`env -u`, `unset`) is always rejected; in a call that runs
// git, any assignment to or mention of a scrubbed variable (prefix, env, export, declare, read,
// printf -v, ${VAR:=…}) and sudo/doas (environment reset) are rejected too.
func checkGitEnv(scripts []*script, _ string, _ Config) string {
	runsGit := false
	_ = eachCmd(scripts, func(_ *simpleCmd, argv []string) string {
		runsGit = runsGit || (len(argv) > 0 && isGitName(progName(argv[0])))
		return ""
	})
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				v := split(c.words)
				if v.envClear {
					return envBlocked("env -i / env - / exec -c")
				}
				for _, u := range v.envUnset {
					if reScrubVar.MatchString(u) {
						return envBlocked("env -u " + u)
					}
				}
				argv := c.words[v.start:]
				if len(argv) > 0 && progName(argv[0]) == "unset" {
					for _, a := range argv[1:] {
						if reScrubVar.MatchString(a) {
							return envBlocked("unset " + a)
						}
					}
				}
				if !runsGit {
					continue
				}
				if slices.Contains(v.wrappers, "sudo") || slices.Contains(v.wrappers, "doas") {
					return envBlocked("sudo/doas (resets the environment)")
				}
				for _, w := range c.words {
					if reScrubAssign.MatchString(w) || reScrubVar.MatchString(w) || reScrubExpand.MatchString(w) {
						name, _, _ := strings.Cut(w, "=")
						return envBlocked(name)
					}
				}
			}
		}
	}
	return ""
}

// reGitFiles matches the git files that make git run programs: the repository config, hooks and a
// worktree's own config (also via $GIT_DIR / ${GIT_DIR}).
var reGitFiles = regexp.MustCompile(`(?i)((^|/)\.git/(config|hooks|worktrees/[^/]+/(config|hooks))|\$\{?GIT_DIR\b)`)

// gitFileReaders only read the files named by their arguments, so naming .git/config there is harmless.
var gitFileReaders = map[string]bool{
	"echo": true, "printf": true, "grep": true, "egrep": true, "fgrep": true, "rg": true, "cat": true,
	"ls": true, "head": true, "tail": true, "wc": true, "file": true, "stat": true, "test": true, "[": true,
	"less": true, "diff": true, "pgrep": true, "ps": true, "which": true, "type": true, "whereis": true,
}

// gitDirTargets take a directory as a destination or a new working directory: with `.git` itself as
// an argument they reach its config and hooks under another spelling (`cd .git; echo > config`,
// `ln -s ../.git d`, `cp evil-config .git/`).
var gitDirTargets = map[string]bool{
	"cd": true, "pushd": true, "ln": true, "cp": true, "mv": true, "tee": true, "install": true, "rsync": true,
}

// pathOf returns the cleaned path of a word; for an option `--opt=value` or a `key=value` word
// (`dd of=…`) that of its value.
func pathOf(w string) string { return filepath.Clean(wordValue(w)) }

// wordValue returns a word as written, or for `--opt=value` / `key=value` the part after the first `=`.
func wordValue(w string) string {
	if _, v, ok := strings.Cut(w, "="); ok {
		return v
	}
	return w
}

// gitFile reports a word naming git's config, hooks or $GIT_DIR, also respelled (`.git//config`,
// `.git/./hooks`, `x/../.git/config`).
func gitFile(w string) bool { return reGitFiles.MatchString(w) || reGitFiles.MatchString(pathOf(w)) }

// gitDir reports a word naming a `.git` directory itself.
func gitDir(w string) bool { return strings.EqualFold(filepath.Base(pathOf(w)), ".git") }

// reBraceVar is a plain `${NAME}` expansion: its braces are not a brace expansion (its value is
// globbed like that of `$NAME`).
var reBraceVar = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// globBlocked is the reason for a glob/brace word where a write could land: the guard does not
// evaluate globs (dotglob, braces and `*` can all resolve to `.git`), it rejects them.
func globBlocked(w string) string {
	return "blocked by guard: `" + w + "` is a glob/brace word in a redirection or an argument of a program " +
		"that may write; the guard does not expand globs (one could resolve to .git). Write the paths out."
}

// dotGlobOn reports a command that makes globs match dot files (`.git`): bash `shopt -s dotglob`,
// a GLOBIGNORE assignment (bash then matches dot files), zsh `setopt globdots` / `set -o globdots`
// (zsh option names ignore case and underscores).
func dotGlobOn(c *simpleCmd, argv []string) bool {
	for _, w := range c.words {
		if strings.HasPrefix(w, "GLOBIGNORE=") {
			return true
		}
	}
	if len(argv) == 0 {
		return false
	}
	switch progName(argv[0]) {
	case "shopt", "setopt", "unsetopt", "set":
		for _, a := range argv[1:] {
			n := strings.ReplaceAll(strings.ToLower(a), "_", "")
			if strings.Contains(n, "dotglob") || strings.Contains(n, "globdots") {
				return true
			}
		}
	}
	return false
}

// checkGitFiles rejects writes into the repository config, hooks or $GIT_DIR: an output redirection
// to such a path or to `.git` (any program), such a path as an argument of a program that is not a
// plain reader (tee, cp, mv, ln, sed -i, chmod, …), and `.git` itself as the target of
// cd/pushd/ln/cp/mv/tee/install/rsync. Globs are not evaluated: any glob/brace word in a redirection
// target or in an argument of a non-reader is rejected (fail closed), as is turning on dotglob/globdots.
// git itself is checked by the config allowlist instead.
func checkGitFiles(scripts []*script, _ string, _ Config) string {
	blocked := func(p string) string {
		return "blocked by guard: `" + p + "` is git's config, hooks or $GIT_DIR; a job does not write there " +
			"(git would run programs from it)."
	}
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				for _, t := range c.outRedirs {
					if gitFile(t) || gitDir(t) {
						return blocked(t)
					}
					if hasGlob(reBraceVar.ReplaceAllString(t, "")) {
						return globBlocked(t)
					}
				}
				argv := argvOf(c.words)
				if dotGlobOn(c, argv) {
					return "blocked by guard: dotglob/globdots (or GLOBIGNORE) makes globs match .git; a job does not change glob options."
				}
				if len(argv) == 0 {
					continue
				}
				name := progName(argv[0])
				if isGitName(name) || gitFileReaders[name] {
					continue
				}
				// find without an action only matches names: `find . -name '*.go'` is a filter.
				findFilter := name == "find" && !slices.ContainsFunc(argv[1:], func(a string) bool {
					return findExec[a] || a == "-delete" || strings.HasPrefix(a, "-fprint") || a == "-fls"
				})
				for k, a := range argv[1:] {
					if gitFile(a) || (gitDirTargets[name] && gitDir(a)) {
						return blocked(a)
					}
					// Inline code (`python3 -c`, `perl -e`, `node -e`, the awk program) is one quoted word
					// the shell does not glob: its `*` is code, as in a script file (noise.go).
					if hasGlob(reBraceVar.ReplaceAllString(a, "")) && !findFilter && !inlineCode(name, argv, k+1) {
						return globBlocked(a)
					}
				}
			}
		}
	}
	return ""
}

// ghRead is the only gh surface a job may use: read-only subcommands. Everything else is rejected
// (fail closed): writes that publish or reach a human (pr/issue create, comment, edit, merge, review,
// close/reopen --comment, gist, repo, label, secret, workflow run, release, api, alias set …) and
// any subcommand gh adds later. A group with no subcommand (`gh pr`) only prints help.
var ghRead = map[string]map[string]bool{
	"":         {"": true},
	"help":     nil, // any
	"version":  nil,
	"status":   {"": true},
	"search":   nil,
	"browse":   nil,
	"pr":       {"": true, "view": true, "list": true, "diff": true, "checks": true, "status": true, "checkout": true},
	"issue":    {"": true, "view": true, "list": true, "status": true},
	"repo":     {"": true, "view": true, "list": true, "clone": true},
	"run":      {"": true, "view": true, "list": true, "watch": true, "download": true},
	"workflow": {"": true, "view": true, "list": true},
	"gist":     {"": true, "view": true, "list": true, "clone": true},
	"label":    {"": true, "list": true},
	"auth":     {"": true}, // no `auth status`: its -t/--show-token prints the token
	"alias":    {"": true, "list": true},
}

func ghAllowed(group, sub string) bool {
	subs, ok := ghRead[group]
	if !ok {
		return false
	}
	return subs == nil || subs[sub]
}

func gitConfigBlocked(what string) string {
	return "blocked by guard: `git " + what + "` is not on the allowlist of harmless git config keys " +
		"(git may run programs from config: hooks, pager, editor, ssh, credential helper, external diff, aliases)."
}

func outside(what string) string {
	return "blocked by guard: `" + what + "` reaches outside the job. A job commits locally only; " +
		"the orchestrator publishes after reading the report and the diff."
}

// gitOutside are git subcommands that publish, mail, serve or reach another system.
var gitOutside = map[string]bool{
	"push": true, "tag": true, "send-pack": true, "http-push": true, "send-email": true, "imap-send": true,
	"svn": true, "p4": true, "daemon": true, "request-pull": true, "instaweb": true, "web--browse": true,
	"cvsexportcommit": true, "cvsimport": true, "cvsserver": true, "archimport": true, "http-backend": true,
	"upload-pack": true, "receive-pack": true, "upload-archive": true, "shell": true,
}

// gitRunsCmd are git subcommands that run arbitrary commands, other git commands the guard does
// not see (for-each-repo), or that install or outlive the run (maintenance start, daemons).
var gitRunsCmd = map[string]bool{
	"filter-branch": true, "filter-repo": true, "difftool": true, "difftool--helper": true, "mergetool": true,
	"for-each-repo": true, "maintenance": true, "hook": true, "remote-ext": true, "remote-fd": true,
	"credential-cache--daemon": true, "fsmonitor--daemon": true,
}

// gitKnown are git's own commands (`git --list-cmds=main`, git 2.43, plus later built-ins). A
// subcommand outside this list is an alias or an external git-* program: rejected, so an alias in
// a config the guard does not see cannot resolve to anything. The commands denied by name
// (gitOutside, gitRunsCmd) are listed too (second block), so their own check decides and names the
// reason, and dropping one from its deny list is not hidden by this one.
var gitKnown = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range strings.Fields(`add am annotate apply archive bisect blame branch bugreport bundle cat-file
		check-attr check-ignore check-mailmap check-ref-format checkout checkout--worker checkout-index cherry
		cherry-pick clean clone column commit commit-graph commit-tree config count-objects credential
		credential-cache credential-store describe diagnose diff diff-files diff-index diff-tree fast-export
		fast-import fetch fetch-pack fmt-merge-msg for-each-ref format-patch fsck fsck-objects gc
		get-tar-commit-id grep hash-object help http-fetch index-pack init init-db interpret-trailers log
		ls-files ls-remote ls-tree mailinfo mailsplit merge merge-base merge-file merge-index merge-octopus
		merge-one-file merge-ours merge-recursive merge-recursive-ours merge-recursive-theirs merge-resolve
		merge-subtree merge-tree mktag mktree multi-pack-index mv name-rev notes pack-objects pack-redundant
		pack-refs patch-id pickaxe prune prune-packed pull quiltimport range-diff read-tree rebase reflog remote
		repack replace rerere reset restore rev-list rev-parse revert rm shortlog show show-branch show-index
		show-ref sparse-checkout stage stash status stripspace submodule submodule--helper subtree switch
		symbolic-ref unpack-file unpack-objects update-index update-ref update-server-info var verify-commit
		verify-pack verify-tag version whatchanged worktree write-tree replay refs backfill last-modified repo
		push tag send-pack http-push send-email imap-send svn p4 daemon request-pull instaweb web--browse
		cvsexportcommit cvsimport cvsserver archimport http-backend upload-pack receive-pack upload-archive shell
		filter-branch filter-repo difftool difftool--helper mergetool for-each-repo maintenance hook remote-ext
		remote-fd credential-cache--daemon fsmonitor--daemon`) {
		m[c] = true
	}
	return m
}()

// longPrefixOf reports a long option `--n[=v]` whose name n is a prefix of one of full: git's
// parse-options accepts unique abbreviations (`git rebase --ex=cmd` is `--exec=cmd`).
func longPrefixOf(a string, full ...string) bool {
	n, ok := strings.CutPrefix(a, "--")
	if !ok || n == "" {
		return false
	}
	n, _, _ = strings.Cut(n, "=")
	return slices.ContainsFunc(full, func(f string) bool { return strings.HasPrefix(f, n) })
}

// gitRunForm returns the form of a known subcommand that runs a command, or "".
func gitRunForm(g gitCall) string {
	var pos []string
	for _, a := range g.args {
		if longPrefixOf(a, "upload-pack", "receive-pack") {
			return g.sub + " --upload-pack/--receive-pack"
		}
		if !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
		}
	}
	switch g.sub {
	case "rebase":
		for _, a := range g.args {
			if longPrefixOf(a, "exec") || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a, 'x')) {
				return "rebase -x/--exec"
			}
		}
	case "submodule":
		if slices.Contains(pos, "foreach") {
			return "submodule foreach"
		}
	case "bisect":
		if len(pos) > 0 && pos[0] == "run" {
			return "bisect run"
		}
	case "archive":
		for _, a := range g.args {
			if longPrefixOf(a, "exec", "remote") {
				return "archive --exec/--remote"
			}
		}
	case "clone":
		for _, a := range g.args {
			if a == "-u" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a, 'u')) {
				return "clone -u"
			}
		}
	case "fetch", "fetch-pack", "ls-remote": // --exec is --upload-pack
		for _, a := range g.args {
			if longPrefixOf(a, "exec") {
				return g.sub + " --exec"
			}
		}
	case "grep": // -O / --open-files-in-pager runs a pager program
		for _, a := range g.args {
			if longPrefixOf(a, "open-files-in-pager") ||
				(strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a, 'O')) {
				return "grep -O/--open-files-in-pager"
			}
		}
	}
	switch g.sub {
	case "clone", "init", "init-db": // a template directory brings its own hooks and config
		for _, a := range g.args {
			if longPrefixOf(a, "template") {
				return g.sub + " --template"
			}
		}
	case "merge", "pull", "rebase", "cherry-pick", "revert":
		if s, ok := mergeStrategies(g); !ok {
			return g.sub + " -s/--strategy " + s
		}
	}
	return ""
}

// gitStrategies are git's own merge strategies; any other name runs a `git-merge-<name>` program.
var gitStrategies = map[string]bool{"resolve": true, "recursive": true, "ort": true, "octopus": true, "ours": true, "subtree": true}

// mergeStrategies checks every -s/--strategy value (also abbreviated `--strat=…`, attached `-sX`, in a
// short cluster) of merge/pull/rebase/cherry-pick/revert; it returns the first one that is not one of
// git's own strategies. -X/--strategy-option is a strategy's option and is left alone. cherry-pick and
// revert have -s as --signoff, so only their long form is a strategy.
func mergeStrategies(g gitCall) (string, bool) {
	short := g.sub == "merge" || g.sub == "pull" || g.sub == "rebase"
	for i := 0; i < len(g.args); i++ {
		a := g.args[i]
		var v string
		switch {
		case a == "--":
			return "", true
		case longPrefixOf(a, "strategy"):
			if _, val, ok := strings.Cut(a, "="); ok {
				v = val
			} else if i+1 < len(g.args) {
				i++
				v = g.args[i]
			}
		case short && strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a, 's'):
			v = a[strings.IndexByte(a, 's')+1:]
			if v == "" && i+1 < len(g.args) {
				i++
				v = g.args[i]
			}
		default:
			continue
		}
		if !gitStrategies[v] {
			return v, false
		}
	}
	return "", true
}

// cloneConfigs returns the `git clone -c/--config key=value` values (also `--conf=…`, attached `-ckv`,
// in a short cluster): clone writes them into the new repository's config.
func cloneConfigs(g gitCall) []string {
	var out []string
	for i := 0; i < len(g.args); i++ {
		a := g.args[i]
		next := func() string {
			if i+1 < len(g.args) {
				i++
				return g.args[i]
			}
			return ""
		}
		switch {
		case a == "--":
			return out
		case longPrefixOf(a, "config"):
			if _, val, ok := strings.Cut(a, "="); ok {
				out = append(out, val)
			} else {
				out = append(out, next())
			}
		case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--"):
			for k := 1; k < len(a); k++ {
				if strings.IndexByte("objc", a[k]) < 0 {
					continue
				}
				v := a[k+1:]
				if v == "" {
					v = next()
				}
				if a[k] == 'c' {
					out = append(out, v)
				}
				break
			}
		}
	}
	return out
}

func gitRunsBlocked(what string) string {
	return "blocked by guard: `git " + what + "` runs commands or git subcommands the guard does not see " +
		"(or is not one of git's own commands: aliases and external git-* programs are not allowed)."
}

// gitConfigSafe are the git config keys a job may set (`git -c`, `git config`): none of them makes
// git run a program. Everything else is rejected (fail closed): core.hooksPath, core.pager,
// core.editor, core.sshCommand, core.fsmonitor, credential.*, diff.external, *.textconv, filter.*,
// alias.*, include.*, url.* … Identity keys are allowed for `git config` only with the configured
// value (checkForeignIdentity); via `-c` they are rejected: the job's identity comes from the
// wrapper's GIT_CONFIG_GLOBAL.
var gitConfigSafe = map[string]bool{
	"user.name": true, "user.email": true, "commit.gpgsign": true, "commit.verbose": true, "commit.cleanup": true,
	"core.autocrlf": true, "core.safecrlf": true, "core.filemode": true, "core.quotepath": true,
	"color.ui": true, "diff.renames": true, "diff.algorithm": true, "diff.context": true, "diff.noprefix": true,
	"merge.conflictstyle": true, "rerere.enabled": true, "init.defaultbranch": true, "log.date": true,
	"status.short": true, "status.showuntrackedfiles": true, "advice.detachedhead": true,
}

func configKey(kv string) string {
	k, _, _ := strings.Cut(kv, "=")
	return strings.ToLower(strings.TrimSpace(k))
}

func checkNothingOut(scripts []*script, _ string, cfg Config) string {
	return eachCmd(scripts, func(_ *simpleCmd, argv []string) string {
		if g, ok := parseGit(argv); ok {
			for _, kv := range g.configs {
				k := configKey(kv)
				if identityField(k) != "" {
					return foreign("-c " + k + "; the job identity is set by the wrapper")
				}
				if !gitConfigSafe[k] {
					return gitConfigBlocked("-c " + k)
				}
			}
			if g.sub == "clone" {
				for _, kv := range cloneConfigs(g) {
					k := configKey(kv)
					_, v, _ := strings.Cut(kv, "=")
					if f := identityField(k); f != "" && !allowedValue(f, v, cfg) {
						return foreign("clone -c " + k)
					}
					if !gitConfigSafe[k] {
						return gitConfigBlocked("clone -c " + k)
					}
				}
			}
			if gitOutside[g.sub] {
				return outside("git " + g.sub)
			}
			if g.sub != "" && (gitRunsCmd[g.sub] || !gitKnown[g.sub]) {
				return gitRunsBlocked(g.sub)
			}
			if f := gitRunForm(g); f != "" {
				return gitRunsBlocked(f)
			}
			if g.sub == "config" {
				if slices.Contains(g.args, "--rename-section") || slices.Contains(g.args, "rename-section") {
					return gitConfigBlocked("config rename-section")
				}
				if k, _, set := gitConfigWrite(g.args); set && !gitConfigSafe[k] {
					return gitConfigBlocked("config " + k)
				}
			}
			return ""
		}
		if group, sub, ok := parseGh(argv); ok {
			if !ghAllowed(group, sub) {
				return outside(strings.TrimSpace("gh " + group + " " + sub))
			}
		}
		return ""
	})
}

// statusRead reports an unescaped `$?` / `${?}` (single-quoted text is already removed from raw).
func statusRead(raw string) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' {
			i++ // the next character is literal
			continue
		}
		if raw[i] == '$' && (strings.HasPrefix(raw[i+1:], "?") || strings.HasPrefix(raw[i+1:], "{?}")) {
			return true
		}
	}
	return false
}

// pipefailSet returns +1 / -1 when argv turns pipefail on / off, 0 otherwise.
func pipefailSet(argv []string) int {
	if len(argv) == 0 || argv[0] != "set" {
		return 0
	}
	r := 0
	for i := 1; i < len(argv)-1; i++ {
		a := argv[i]
		if len(a) > 1 && (a[0] == '-' || a[0] == '+') && !strings.HasPrefix(a, "--") &&
			strings.HasSuffix(a, "o") && argv[i+1] == "pipefail" {
			r = 1
			if a[0] == '+' {
				r = -1
			}
		}
	}
	return r
}

var (
	ctlOpen  = map[string]bool{"if": true, "while": true, "until": true, "for": true, "case": true, "select": true, "{": true}
	ctlClose = map[string]bool{"fi": true, "done": true, "esac": true, "}": true}
)

// checkStatusAfterPipe: pipefail counts as on only after a top-level, unconditional, foreground
// `set -o pipefail` (a simple command, not inside if/loop/braces/subshell, not after && / ||);
// any `set +o pipefail` anywhere later turns it off.
func checkStatusAfterPipe(scripts []*script, _ string, _ Config) string {
	for _, sc := range scripts {
		pipefail, afterPipe, ctl := false, false, 0
		for _, pl := range sc.pipelines {
			if afterPipe && !pipefail && statusRead(pl.raw()) {
				return "blocked by guard: `$?` right after a pipe is the status of the LAST stage only, " +
					"so a failure of an earlier stage looks like success. Use `set -o pipefail` (top level, before the pipe), " +
					"`${PIPESTATUS[@]}`, or check the command without a pipe."
			}
			for _, c := range pl.cmds {
				for _, w := range c.words {
					if ctlOpen[w] {
						ctl++
					} else if ctlClose[w] {
						ctl--
					} else if !keywords[w] {
						break
					}
				}
				switch pipefailSet(argvOf(c.words)) {
				case -1:
					pipefail = false
				case 1:
					top := len(pl.cmds) == 1 && len(c.words) > 0 && c.words[0] == "set" && ctl == 0 && pl.nest == 0 &&
						(pl.before == "" || pl.before == ";" || pl.before == "\n") && pl.after != "&"
					pipefail = pipefail || top
				}
			}
			afterPipe = len(pl.cmds) > 1
		}
	}
	return ""
}

// pgrepValueFlags are pgrep/pkill short flags that take a value.
const pgrepValueFlags = "dgGPstuUFcJ"

// pgrepFull returns the pattern of a `pgrep -f` call and whether it is case-insensitive.
func pgrepFull(argv []string) (pattern string, icase, ok bool) {
	if len(argv) == 0 || !isProg(argv[0], "pgrep") {
		return "", false, false
	}
	full := false
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--full":
			full = true
		case a == "--ignore-case":
			icase = true
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-") && len(a) > 1:
			full = full || strings.ContainsRune(a, 'f')
			icase = icase || strings.ContainsRune(a, 'i')
			if strings.ContainsRune(pgrepValueFlags, rune(a[len(a)-1])) {
				i++
			}
		default:
			pattern = a
		}
	}
	return pattern, icase, full
}

func checkSelfMatchingWait(scripts []*script, whole string, _ Config) string {
	for _, sc := range scripts {
		loop := false
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				// Loop keywords anywhere in the leading keywords: `then while …`, `{ until …`, `do while …`.
				for _, w := range c.words {
					if w == "while" || w == "until" {
						loop = true
					}
					if !keywords[w] {
						break
					}
				}
			}
		}
		if !loop {
			continue
		}
		r := eachCmd([]*script{sc}, func(_ *simpleCmd, argv []string) string {
			pat, icase, ok := pgrepFull(argv)
			if !ok {
				return ""
			}
			if icase {
				pat = "(?i)" + pat
			}
			re, err := regexp.Compile(pat)
			// The loop's own shell has the whole command in its argv: a pattern matching it never ends.
			if err != nil || re.MatchString(whole) {
				return "blocked by guard: this wait-loop's `pgrep -f` pattern matches the loop's own command line, " +
					"so it never ends. Wait on a PID (`wait $pid`, `kill -0 $pid`) or a file, or use a pattern " +
					"that cannot match itself (e.g. `[s]erver`)."
			}
			return ""
		})
		if r != "" {
			return r
		}
	}
	return ""
}

// staticHeredoc matches `$(cat <<'EOF' … EOF)`: a message whose text is literally in the command.
var staticHeredoc = regexp.MustCompile(`(?s)^\$\(cat <<-?[ \t]*['"]([A-Za-z_][A-Za-z0-9_]*)['"][ \t]*\n(.*)\n[ \t]*([A-Za-z_][A-Za-z0-9_]*)[ \t]*\n?[ \t]*\)$`)

// literalText reports whether a commit text word is fully visible to the guard.
func literalText(w string, dyn bool) bool {
	if !dyn {
		return true
	}
	m := staticHeredoc.FindStringSubmatch(w)
	if m == nil || m[1] != m[3] {
		return false
	}
	for _, line := range strings.Split(m[2], "\n") {
		if strings.TrimSpace(line) == m[1] {
			return false // the here-doc ends early; the rest would run
		}
	}
	return true
}

// commitTexts returns everything that ends up in a commit's message, author or trailers.
// A non-empty problem means the text cannot be known statically (fail closed).
func commitTexts(args []string, dyn []bool, cfg Config) (texts []string, problem string) {
	readFile := func(f string, d bool) {
		switch {
		case d:
			problem = "commit message file from a variable"
			return
		case f == "-" || f == "/dev/stdin" || strings.HasPrefix(f, "/dev/fd/") || strings.HasPrefix(f, "/proc/"):
			problem = "commit message from stdin; write it to a file first, then `git commit -F <file>`"
			return
		}
		if !filepath.IsAbs(f) && cfg.Dir != "" {
			f = filepath.Join(cfg.Dir, f)
		}
		b, err := os.ReadFile(f)
		if err != nil {
			problem = "the commit message file is unreadable; write the file in a separate call first"
			return
		}
		texts = append(texts, string(b))
	}
	text := func(w string, d bool) {
		if !literalText(w, d) {
			problem = "commit message from a variable or substitution; write it literally or use -F <file>"
			return
		}
		texts = append(texts, w)
	}
	reuse := func(form string) {
		problem = "commit message reused from another commit (" + form + "): its text and author are not visible to the guard; " +
			"write the message with -m or -F <file>"
	}
	for i := 0; i < len(args) && problem == ""; i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") && a != "--" {
			name, _, _ := strings.Cut(a[2:], "=")
			if reuseLong(name) {
				reuse("--" + name)
				return texts, problem
			}
			if abbrev(name) {
				problem = "abbreviated long option --" + name + "; write commit options in full"
				return texts, problem
			}
		}
		next := func() (string, bool) {
			if i+1 < len(args) {
				i++
				return args[i], dyn[i]
			}
			return "", false
		}
		switch {
		case a == "--":
			return texts, problem
		case a == "--message" || a == "--trailer" || a == "--author":
			text(next())
		case a == "--file" || a == "--template":
			readFile(next())
		case strings.HasPrefix(a, "--message="), strings.HasPrefix(a, "--trailer="), strings.HasPrefix(a, "--author="):
			text(a[strings.IndexByte(a, '=')+1:], dyn[i])
		case strings.HasPrefix(a, "--file="), strings.HasPrefix(a, "--template="):
			readFile(a[strings.IndexByte(a, '=')+1:], dyn[i])
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for k := 1; k < len(a); k++ {
				l := a[k]
				if l == 'C' || l == 'c' {
					reuse("-" + string(l))
					return texts, problem
				}
				if strings.IndexByte("mFt", l) < 0 {
					continue
				}
				v, d := a[k+1:], dyn[i]
				if v == "" {
					v, d = next()
				}
				switch l {
				case 'm':
					text(v, d)
				case 'F', 't':
					readFile(v, d)
				}
				break
			}
		}
	}
	return texts, problem
}

// commitReuse are the `git commit` long options that take the message (and with -C/-c the
// authorship) from another commit: --reuse-message, --reedit-message, --fixup (incl. amend:/reword:),
// --squash.
var commitReuse = []string{"reuse-message", "reedit-message", "fixup", "squash"}

func reuseLong(name string) bool { return slices.Contains(commitReuse, name) }

// commitTextOpts are the long options whose value ends up in the commit (or reuses another one).
var commitTextOpts = []string{"message", "file", "template", "trailer", "author", "reuse-message", "reedit-message", "fixup", "squash"}

// abbrev reports a long option git would expand (git accepts unique prefixes) into one of
// commitTextOpts: `--mess=…` is `--message=…`, `--reuse` is `--reuse-message`.
func abbrev(name string) bool {
	if name == "" || slices.Contains(commitTextOpts, name) {
		return false
	}
	return slices.ContainsFunc(commitTextOpts, func(o string) bool { return strings.HasPrefix(o, name) })
}

func checkOwnerInCommit(scripts []*script, _ string, cfg Config) string {
	return eachCmd(scripts, func(c *simpleCmd, argv []string) string {
		g, isGit := parseGit(argv)
		if !isGit || g.sub != "commit" {
			return ""
		}
		dyn := c.dyn[len(c.words)-len(argv):]
		texts, problem := commitTexts(g.args, dyn[g.subIdx+1:], cfg)
		if problem != "" {
			return "blocked by guard: " + problem + "."
		}
		low := strings.ToLower(strings.Join(texts, "\n"))
		for _, name := range cfg.OwnerNames {
			n := strings.ToLower(strings.TrimSpace(name))
			if n != "" && strings.Contains(low, n) {
				return fmt.Sprintf("blocked by guard: the owner's name (%q) must not appear in a commit.", name)
			}
		}
		return ""
	})
}

// gitConfigWrite returns the key (lower-cased) and value of a `git config` call that sets a value.
func gitConfigWrite(args []string) (key, value string, set bool) {
	var pos []string
	read := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--file", "-f", "--blob", "--type", "--default", "--comment", "--value":
			i++
			continue
		case "--get", "--get-all", "--get-regexp", "--get-urlmatch", "--unset", "--unset-all", "--list", "-l",
			"--rename-section", "--remove-section", "--edit", "-e", "--get-color", "--get-colorbool":
			read = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		pos = append(pos, a)
	}
	if len(pos) > 0 {
		switch pos[0] {
		case "set":
			pos = pos[1:]
		case "get", "list", "unset", "rename-section", "remove-section", "edit":
			return "", "", false
		}
	}
	if read || len(pos) < 2 {
		return "", "", false
	}
	return strings.ToLower(pos[0]), pos[1], true
}

// identityField maps a git config key to name/email when it sets an identity.
func identityField(key string) string {
	switch strings.ToLower(key) {
	case "user.name", "author.name", "committer.name":
		return "name"
	case "user.email", "author.email", "committer.email":
		return "email"
	}
	return ""
}

func allowedValue(field, v string, cfg Config) bool {
	switch field {
	case "name":
		return cfg.GitName != "" && v == cfg.GitName
	case "email":
		return cfg.GitEmail != "" && strings.EqualFold(v, cfg.GitEmail)
	}
	return false
}

func foreign(what string) string {
	return "blocked by guard: foreign git identity (" + what + "). Commits must use the repository's configured identity; " +
		"do not override user.name/user.email, --author or GIT_AUTHOR_*/GIT_COMMITTER_*."
}

var (
	reIdentityEnv = regexp.MustCompile(`^GIT_(AUTHOR|COMMITTER)_(NAME|EMAIL)=(.*)$`)
	reConfigKey   = regexp.MustCompile(`^GIT_CONFIG_KEY_[0-9]+=(.*)$`)
)

func checkForeignIdentity(scripts []*script, _ string, cfg Config) string {
	return eachCmd(scripts, func(c *simpleCmd, argv []string) string {
		// Environment: prefixes, `env`, `export` — any word that assigns an identity variable.
		for _, w := range c.words {
			if m := reIdentityEnv.FindStringSubmatch(w); m != nil && !allowedValue(strings.ToLower(m[2]), m[3], cfg) {
				return foreign("GIT_" + m[1] + "_" + m[2])
			}
			if m := reConfigKey.FindStringSubmatch(w); m != nil && identityField(m[1]) != "" {
				return foreign("GIT_CONFIG_KEY_* " + m[1])
			}
			if strings.HasPrefix(w, "GIT_CONFIG_PARAMETERS=") {
				low := strings.ToLower(w)
				for _, k := range []string{"user.", "author.", "committer.", "alias."} {
					if strings.Contains(low, k) {
						return foreign("GIT_CONFIG_PARAMETERS")
					}
				}
			}
		}
		g, ok := parseGit(argv)
		if !ok {
			return ""
		}
		switch g.sub {
		case "config":
			if k, v, set := gitConfigWrite(g.args); set {
				if f := identityField(k); f != "" && !allowedValue(f, v, cfg) {
					return foreign("git config " + k)
				}
			}
		case "commit":
			for i := 0; i < len(g.args); i++ {
				a := g.args[i]
				var v string
				switch {
				case a == "--author" && i+1 < len(g.args):
					v = g.args[i+1]
				case strings.HasPrefix(a, "--author="):
					v = strings.TrimPrefix(a, "--author=")
				default:
					continue
				}
				want := fmt.Sprintf("%s <%s>", cfg.GitName, cfg.GitEmail)
				if cfg.GitName == "" || cfg.GitEmail == "" || v != want {
					return foreign("--author")
				}
			}
		}
		return ""
	})
}

// backgroundBash reports a Bash input with a key run_in_background, in any case, set to anything but
// false or null: encoding/json matches a field ignoring case and keeps the last key, so a decoded struct
// would read `{"run_in_background":true,"Run_In_Background":false}` as false.
func backgroundBash(raw json.RawMessage) bool {
	var in map[string]json.RawMessage
	if json.Unmarshal(raw, &in) != nil {
		return true // an input the guard cannot read is refused
	}
	for k, v := range in {
		if !strings.EqualFold(k, "run_in_background") {
			continue
		}
		if s := strings.TrimSpace(string(v)); s != "false" && s != "null" {
			return true
		}
	}
	return false
}
