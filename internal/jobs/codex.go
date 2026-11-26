package jobs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/qwwiwi/agent-jobs/internal/casefold"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Codex runner (DEVIATIONS #5): a job runs on `codex exec` instead of `claude -p`. Codex has no turn or
// budget caps, so the job timeout is the only limit and the cost is recorded as unknown. The guard hook
// and the deny rules of settings.json are Claude features and do not run inside codex: the codex sandbox
// is the containment, and only read-only codex jobs are allowed unless codex_write_allowed is set.

// Codex sandbox modes passed to `codex exec --sandbox`.
const (
	CodexSandboxReadOnly = "read-only"
	CodexSandboxWrite    = "workspace-write"
)

// ReasoningEfforts are the accepted reasoning efforts per runner: codex's model_reasoning_effort values
// and the claude CLI's --effort values. DefaultReasoningEffort applies to a codex job when neither the
// job nor the config names one; a claude job takes its kind's default (effort_by_kind).
var ReasoningEfforts = map[string][]string{
	store.RunnerCodex:  {"low", "medium", "high"},
	store.RunnerClaude: {"low", "medium", "high", "xhigh", "max"},
}

// DefaultReasoningEffort is the codex reasoning effort when neither the job nor the config sets one.
const DefaultReasoningEffort = "high"

// IsCodexModel reports whether a model runs on codex (OpenAI models: gpt-*).
func IsCodexModel(model string) bool { return strings.HasPrefix(strings.ToLower(model), "gpt-") }

// ResolveRunner returns the runner of a job: the explicit runner if given, else codex for a gpt-*
// model and claude otherwise. A runner that does not fit the model is rejected.
func ResolveRunner(runner, model string) (string, error) {
	switch strings.TrimSpace(runner) {
	case "":
		if IsCodexModel(model) {
			return store.RunnerCodex, nil
		}
		return store.RunnerClaude, nil
	case store.RunnerClaude:
		if IsCodexModel(model) {
			return "", fmt.Errorf("model %q is a codex model; runner claude cannot run it", model)
		}
		return store.RunnerClaude, nil
	case store.RunnerCodex:
		if !IsCodexModel(model) {
			return "", fmt.Errorf("runner codex needs a codex model (gpt-…), got %q: give an explicit model "+
				"or configure one in the config's models table for this kind and complexity", model)
		}
		return store.RunnerCodex, nil
	default:
		return "", fmt.Errorf("runner %q must be claude or codex", runner)
	}
}

// CodexSandbox is the codex sandbox of a job kind: read-only for review, research and the gate kinds
// (slicing, judge), workspace-write for fix and general.
func CodexSandbox(kind string) string {
	if kind == "research" || readOnlyKind(kind) {
		return CodexSandboxReadOnly
	}
	return CodexSandboxWrite
}

// checkCodexJob refuses a codex job the config does not allow: a workspace-write job (fix/general)
// unless codex_write_allowed is set, since the guard hook does not run inside codex, and any codex job
// under codex_sandbox worktree-full-access without codex_write_allowed (no codex sandbox at all).
func (c Config) checkCodexJob(kind string) error {
	if c.codexSandboxMode() == CodexSandboxModeWorktreeFullAccess && !c.CodexWriteAllowed {
		return codexFullAccessRefusal
	}
	if CodexSandbox(kind) == CodexSandboxWrite && !c.CodexWriteAllowed {
		return fmt.Errorf("codex job of kind %s needs the workspace-write sandbox, which is off "+
			"(codex_write_allowed is false: the guard hook does not run inside codex); "+
			"use kind review or research for a read-only codex job, or run it on claude", kind)
	}
	return nil
}

// reasoningEffort returns the effort of a codex job: the job's own, else the config's, else the default.
func (c Config) reasoningEffort(job string) string {
	if job != "" {
		return job
	}
	if c.CodexReasoningEffort != "" {
		return c.CodexReasoningEffort
	}
	return DefaultReasoningEffort
}

// ValidateReasoningEffort rejects an effort the runner does not take: low|medium|high on codex,
// low|medium|high|xhigh|max on claude (empty is accepted: default).
func ValidateReasoningEffort(runner, e string) error {
	values, ok := ReasoningEfforts[runner]
	if !ok {
		return fmt.Errorf("runner %q has no reasoning efforts", runner)
	}
	if e != "" && !slices.Contains(values, e) {
		return fmt.Errorf("reasoning effort %q must be one of %s for a %s job", e, strings.Join(values, "|"), runner)
	}
	return nil
}

func (c Config) validateCodex() error {
	if err := ValidateReasoningEffort(store.RunnerCodex, c.CodexReasoningEffort); err != nil {
		return fmt.Errorf("codex_reasoning_effort: %w", err)
	}
	if err := validateCodexSandboxMode(c.CodexSandboxMode); err != nil {
		return err
	}
	for kind, row := range c.Models {
		for cx, model := range row {
			if _, ok := defaultModels[kind][cx]; !ok {
				return fmt.Errorf("models: unknown kind/complexity %q/%q", kind, cx)
			}
			if err := ValidateModel(model); err != nil {
				return fmt.Errorf("models.%s.%s: %w", kind, cx, err)
			}
		}
	}
	return nil
}

// codexWritableTmp is the temp dir codex's workspace-write sandbox keeps writable; a test hook (the
// tests' own temp dirs may lie in it), never user config.
var codexWritableTmp = "/tmp"

// codexReadDirsHome is the user's home dir the secret-dir rule of codex_read_dirs is checked against; a
// test hook, never user config.
var codexReadDirsHome = os.UserHomeDir

// homeSecretDirs are the dirs below the user's home that hold credentials or keys; with every
// ~/.claude* entry (homeSecretPrefix: ~/.claude, ~/.claude.json, ~/.claude-lab and its */secrets) and
// the agent-jobs home they are never named to a codex job as readable. The list is a minimum, not every
// place a tool may keep credentials: the owner names only package stores and caches.
var homeSecretDirs = []string{".secrets", ".ssh", ".gnupg", ".aws", filepath.Join(".config", "gh"),
	".codex", ".kube", ".docker", filepath.Join(".config", "gcloud")}

const homeSecretPrefix = ".claude"

// secretDirs are the user's home dirs (as configured and with links resolved) and the secret dirs a
// codex_read_dirs entry must neither be, hold nor lie in: homeSecretDirs, the existing ~/.claude*
// entries and the agent-jobs home (the state dir's parent), each also with its links resolved.
func secretDirs(stateDir string) (homes, secrets []string, err error) {
	home, err := codexReadDirsHome()
	if err != nil || !filepath.IsAbs(home) {
		return nil, nil, fmt.Errorf("home dir %q unknown (%v): codex_read_dirs cannot be checked against its secret dirs", home, err)
	}
	homes = []string{filepath.Clean(home)}
	if real, err := filepath.EvalSymlinks(home); err == nil && real != homes[0] {
		homes = append(homes, real)
	}
	for _, h := range homes {
		for _, s := range homeSecretDirs {
			secrets = append(secrets, filepath.Join(h, s))
		}
		if entries, err := os.ReadDir(h); err == nil {
			for _, e := range entries {
				// Folded where the file system ignores case: ~/.CLAUDE-lab is a ~/.claude* entry there (#122).
				if strings.HasPrefix(foldReadDirPath(e.Name()), homeSecretPrefix) {
					secrets = append(secrets, filepath.Join(h, e.Name()))
				}
			}
		}
	}
	secrets = append(secrets, filepath.Dir(filepath.Clean(stateDir)))
	for _, s := range slices.Clone(secrets) {
		if real, err := filepath.EvalSymlinks(s); err == nil && real != s {
			secrets = append(secrets, real)
		}
	}
	return homes, secrets, nil
}

// secretOverlap is the home or secret dir path p is, holds or lies in ("" = none): a path holding a
// home holds all its secret dirs, and any path below a home whose first element starts with
// homeSecretPrefix is a ~/.claude* one, existing or not.
func secretOverlap(p string, homes, secrets []string) string {
	for _, h := range homes {
		if pathWithin(h, p) {
			return h
		}
		if rel, err := filepath.Rel(h, p); err == nil && pathWithin(p, h) {
			if first, _, _ := strings.Cut(rel, string(filepath.Separator)); strings.HasPrefix(first, homeSecretPrefix) {
				return filepath.Join(h, first)
			}
		}
	}
	for _, s := range secrets {
		if pathWithin(p, s) || pathWithin(s, p) {
			return s
		}
	}
	return ""
}

// validateCodexReadDirs checks config codex_read_dirs: absolute clean paths, not "/", not the agent's
// state dir, one holding it (the agent-jobs home, the user's home) or one inside it (other jobs' dirs),
// not in /tmp, which codex's workspace-write sandbox keeps writable, and not a secret dir (secretDirs),
// one holding it (the user's home, /home) or one inside it; each also with its links resolved
// (resolvePath: an entry whose links cannot be resolved is refused), and case-folded where the file
// system ignores case (codexReadDirsFoldCase).
func validateCodexReadDirs(stateDir string, dirs []string) error {
	if len(dirs) == 0 {
		return nil
	}
	state := filepath.Clean(stateDir)
	tmp := codexWritableTmp
	realTmp, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		realTmp = tmp
	}
	realState, err := resolvePath(state)
	if err != nil {
		realState = state
	}
	homes, secrets, err := secretDirs(state)
	if err != nil {
		return err
	}
	fold := foldReadDirPath
	fState, fRealState, fTmp, fRealTmp := fold(state), fold(realState), fold(tmp), fold(realTmp)
	for i := range homes {
		homes[i] = fold(homes[i])
	}
	for i := range secrets {
		secrets[i] = fold(secrets[i])
	}
	for _, d := range dirs {
		fd := fold(d)
		switch {
		case !filepath.IsAbs(d) || filepath.Clean(d) != d || strings.ContainsRune(d, 0):
			return fmt.Errorf("codex_read_dirs entry %q must be an absolute clean path", d)
		case d == "/":
			return errors.New(`codex_read_dirs entry "/" names the whole file system`)
		case pathWithin(fState, fd) || pathWithin(fd, fState):
			return fmt.Errorf("codex_read_dirs entry %q overlaps the agent-jobs state dir %s", d, state)
		case pathWithin(fd, fTmp):
			return fmt.Errorf("codex_read_dirs entry %q is in %s, which a workspace-write codex job can write", d, tmp)
		}
		if s := secretOverlap(fd, homes, secrets); s != "" {
			return fmt.Errorf("codex_read_dirs entry %q overlaps %s, which holds credentials, keys or agent configuration", d, s)
		}
		// The same with links resolved: the longest existing part's, the missing rest appended.
		real, err := resolvePath(d)
		if err != nil {
			return fmt.Errorf("codex_read_dirs entry %q cannot be resolved: %v", d, err)
		}
		fReal := fold(real)
		if pathWithin(fRealState, fReal) || pathWithin(fReal, fRealState) || pathWithin(fReal, fTmp) || pathWithin(fReal, fRealTmp) || real == "/" {
			return fmt.Errorf("codex_read_dirs entry %q resolves to %s, which overlaps the agent-jobs state dir, %s or /", d, real, tmp)
		}
		if s := secretOverlap(fReal, homes, secrets); s != "" {
			return fmt.Errorf("codex_read_dirs entry %q resolves to %s, which overlaps %s, which holds credentials, keys or agent configuration", d, real, s)
		}
	}
	return nil
}

// codexReadDirsFoldCase compares codex_read_dirs paths case-folded: macOS file systems ignore case by
// default, so ~/.SSH (or ~/.ſsh, U+017F folding to s) is ~/.ssh there. A test hook besides the platform
// default, never user config.
var codexReadDirsFoldCase = runtime.GOOS == "darwin"

func foldReadDirPath(p string) string {
	if codexReadDirsFoldCase {
		return casefold.Fold(p)
	}
	return p
}

// maxResolveLinks bounds the dangling links resolvePath follows.
const maxResolveLinks = 40

// resolvePath is the clean absolute path p with its links resolved as far as it exists: the longest
// existing ancestor resolved (filepath.EvalSymlinks) and the missing rest appended, so a link to the
// home with a missing ~/.claude-new below it resolves into the home. A dangling link on the way is
// followed to its target (one with ".." after a name is refused: the name may be a link, which makes
// ".." not lexical). Any error but a missing element (a file on the way, a link loop, no permission)
// is returned.
func resolvePath(p string) (string, error) {
	for range maxResolveLinks {
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			return real, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		// The longest ancestor that resolves, and the elements below it.
		dir, rest := filepath.Dir(p), []string{filepath.Base(p)}
		realDir, err := filepath.EvalSymlinks(dir)
		for errors.Is(err, fs.ErrNotExist) && dir != "/" {
			dir, rest = filepath.Dir(dir), append([]string{filepath.Base(dir)}, rest...)
			realDir, err = filepath.EvalSymlinks(dir)
		}
		if err != nil {
			return "", err
		}
		first, tail := filepath.Join(realDir, rest[0]), filepath.Join(rest[1:]...)
		fi, err := os.Lstat(first)
		if errors.Is(err, fs.ErrNotExist) {
			return filepath.Join(first, tail), nil
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			return "", fmt.Errorf("%s exists but does not resolve", first)
		}
		target, err := os.Readlink(first)
		if err != nil {
			return "", err
		}
		if dotDotAfterName(target) {
			return "", fmt.Errorf("dangling link %s -> %s: a target with .. after a name is not followed", first, target)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(realDir, target)
		}
		p = filepath.Join(target, tail)
	}
	return "", fmt.Errorf("%s: too many links", p)
}

// dotDotAfterName reports whether the link target has a ".." element after a name element (leading
// ".." elements of a relative target climb from the link's resolved dir and are lexical).
func dotDotAfterName(target string) bool {
	named := false
	for _, e := range strings.Split(target, "/") {
		switch e {
		case "", ".":
		case "..":
			if named {
				return true
			}
		default:
			named = true
		}
	}
	return false
}

// pathWithin reports whether path is dir or lies below it (both clean and absolute).
func pathWithin(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, strings.TrimSuffix(dir, "/")+"/")
}

// checkCodexIdentity refuses a codex job whose commits would carry the owner's identity: codex runs
// without the guard hook, so its effective author and committer (`git var` in the worktree with the
// job's git environment: the job identity, else the repository's own config, else git's guess) must
// not name one of owners, nor carry the email of the repository's (repo; "" = none) local pair when
// that pair names one (jobIdentity dropped it as the owner's; its email need not contain an owner
// name). No identity at all passes: such a commit fails, it carries nobody's.
func checkCodexIdentity(jobDir, repo string, env, owners []string) error {
	if !slices.ContainsFunc(owners, func(n string) bool { return strings.TrimSpace(n) != "" }) {
		return nil
	}
	env, err := GitEnv(jobDir, env)
	if err != nil {
		return err
	}
	var ownerEmail string
	if repo != "" {
		name, email := repoLocalIdentity(repo, "user.name"), repoLocalIdentity(repo, "user.email")
		if namesOwner(owners, name, email) {
			ownerEmail = strings.TrimSpace(email)
		}
	}
	for _, v := range []string{"GIT_AUTHOR_IDENT", "GIT_COMMITTER_IDENT"} {
		out, _, err := runGitFor(gitTimeout, filepath.Join(jobDir, "work"), env, "var", v)
		if errors.Is(err, errGitTimeout) {
			return fmt.Errorf("check a codex job's commit identity: git var %s: %w after %s", v, err, gitTimeout)
		}
		if err != nil {
			continue
		}
		if namesOwner(owners, string(out)) {
			return fmt.Errorf("a codex job's commits would carry the owner's identity (its %s names one of owner_names: "+
				"the repository's own identity or git's guess is in force); set git_name and git_email in config.json", v)
		}
		if ownerEmail != "" && strings.EqualFold(identEmail(string(out)), ownerEmail) {
			return fmt.Errorf("a codex job's commits would carry the owner's identity (its %s has the email of the "+
				"repository's user.name/user.email, which names one of owner_names); set git_name and git_email in config.json", v)
		}
	}
	return nil
}

// identEmail is the email of a `git var GIT_*_IDENT` line ("Name <email> 1700000000 +0000"; "" = none).
func identEmail(ident string) string {
	_, rest, ok := strings.Cut(ident, "<")
	if !ok {
		return ""
	}
	email, _, ok := strings.Cut(rest, ">")
	if !ok {
		return ""
	}
	return strings.TrimSpace(email)
}

// codexReadDirsSection is the prompt section of a codex job that names config codex_read_dirs as
// readable ("" for a claude or gate job, or none configured): the exception to the preamble's rule to
// work only inside the job directory, read-only.
func codexReadDirsSection(j *store.Job, dirs []string) string {
	if j.Runner != store.RunnerCodex || IsGateKind(j.Kind) || len(dirs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Read-only directories\n\nBesides your job directory you may READ these directories (an exception to the rule " +
		"«Work only inside your own job directory»). Use them read-only: never write, create, change or delete anything in them " +
		"(for a package manager, point it at them as an offline store or cache, never as a target).\n\n")
	for _, d := range dirs {
		fmt.Fprintf(&b, "- `%s`\n", d)
	}
	b.WriteString("\n")
	return b.String()
}

func (c Config) codexBin() string {
	if b := os.Getenv("AGENT_JOBS_CODEX_BIN"); b != "" {
		return b
	}
	if c.CodexBin != "" {
		return c.CodexBin
	}
	return "codex"
}

// CodexDisabledFeatures are the codex features every codex job runs without (`--disable <f>` each):
// whatever could act outside the shell sandbox (apps, browsers, computer use, plugins, MCP installs,
// hooks, background daemons and automations, worktrees, sub-agents) or a one-shot job has no use for.
// With --ignore-user-config, --ignore-rules, --ephemeral and an empty mcp_servers table, a job does not
// inherit the owner's codex config, rules, apps or plugins (the command line before issue #243, without
// multi_agent_v2 and CodexAgentsOverride, verified on codex-cli 0.162.1); system/managed config layers are ruled out by checkCodexSystemConfig and MCP
// servers from other config layers by checkCodexMCP, so the sandbox is the whole containment.
var CodexDisabledFeatures = []string{
	"apps", "browser_use", "browser_use_external", "browser_use_full_cdp_access", "computer_use",
	"image_generation", "multi_agent", "multi_agent_v2", "plugins", "plugin_sharing", "remote_plugin",
	"skill_mcp_dependency_install", "daemon_auto_start", "in_app_local_automation", "goals",
	"realtime_conversation", "worktrees", "hooks",
}

// codexSystemConfigDir is where codex reads its system and managed config layers; a test hook, never
// user config.
var codexSystemConfigDir = "/etc/codex"

// checkCodexSystemConfig fails closed on codex's system and managed config layers (`config.toml`,
// `managed_config.toml`, `requirements.toml`, ... in /etc/codex): they are applied after the job's CLI
// overrides and can turn `apps` back on, whose implicit codex_apps server neither `--disable apps` nor
// `mcp list` covers. A missing or empty dir passes; anything in it, or a dir that cannot be read, refuses.
func checkCodexSystemConfig(dir string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("codex system config check: %w; codex jobs refuse to run unless %s is missing or empty", err, dir)
	case len(entries) == 0:
		return nil
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = Printable(e.Name(), 64)
	}
	return fmt.Errorf("codex has system or managed configuration in %s (%s); codex jobs refuse to run with it",
		dir, strings.Join(names, ", "))
}

// codexMCPTimeout bounds the `codex mcp list --json` check before a codex launch.
var codexMCPTimeout = 15 * time.Second

// checkCodexMCP fails closed on MCP servers a codex job could inherit: --ignore-user-config leaves the
// system and managed config layers active, and `-c mcp_servers={}` does not clear a table codex merges
// recursively, so the command line alone proves nothing. Before every launch the same binary runs
// `mcp list --json` (it starts no model) in the job's work dir with the job's environment; only an
// empty JSON array lets the job run. It reads every layer, the user one included, so it is stricter
// than the job's own command line: a codex job needs a codex setup without MCP servers.
func checkCodexMCP(bin, dir string, env []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), codexMCPTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "mcp", "list", "--json")
	cmd.Dir, cmd.Env = dir, env
	cmd.WaitDelay = time.Second // a child holding stdout open does not outlive the timeout
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	fail := func(why string) error {
		return fmt.Errorf("codex MCP check `%s mcp list --json` %s; codex jobs refuse to run unless it prints []",
			bin, why)
	}
	switch {
	case ctx.Err() != nil:
		return fail(fmt.Sprintf("timed out after %s", codexMCPTimeout))
	case err != nil:
		return fail(fmt.Sprintf("failed: %v: %s", err, Printable(stderr.String(), 200)))
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	var servers []struct {
		Name string `json:"name"`
	}
	if err := dec.Decode(&servers); err != nil || servers == nil || dec.More() {
		return fail(fmt.Sprintf("printed %q, not a JSON array", Printable(string(out), 200)))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fail(fmt.Sprintf("printed %q, not one JSON array", Printable(string(out), 200)))
	}
	if len(servers) == 0 {
		return nil
	}
	names := make([]string, len(servers))
	for i, s := range servers {
		names[i] = Printable(s.Name, 64)
		if names[i] == "" {
			names[i] = "(unnamed)"
		}
	}
	return fmt.Errorf("codex has MCP servers configured: %s; codex jobs refuse to run with inherited MCP servers",
		strings.Join(names, ", "))
}

// CodexProgressDir is the folder in a codex write job's dir that holds its ProgressFile (issue #92).
// Under workspace-write codex lets a job write only its work dir (and the temp dirs), so the progress
// file the preamble asks for lives here, the one extra writable root (`--add-dir`): the rest of the job
// dir (prompt, settings, wrapper files, report.md, exit_code, attempts) stays read-only to the job.
// Read-only codex jobs get neither the folder nor the root.
const CodexProgressDir = "progress"

// CodexProgressPath is the progress file of a codex write job in jobDir, absolute (the job runs in work/).
func CodexProgressPath(jobDir string) string {
	return filepath.Join(absPath(jobDir), CodexProgressDir, ProgressFile)
}

// codexProgressRoot is the `--add-dir` value of a codex job under sandbox: the job's progress folder
// (absolute) under workspace-write, "" otherwise (read-only writes nothing; danger-full-access needs no
// root).
func codexProgressRoot(sandbox, jobDir string) string {
	if sandbox != CodexSandboxWrite {
		return ""
	}
	return filepath.Join(absPath(jobDir), CodexProgressDir)
}

// absPath is p made absolute, or p itself when the working dir cannot be read (the wrapper then refuses
// a relative root).
func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// codexProgressWritable reports whether j is a codex job of a write kind, whose launch gets the progress
// folder (prepare creates it) and whose prompt names it. Under worktree-full-access such a job runs with
// danger-full-access and no extra root, but its prompt names the same path.
func codexProgressWritable(j *store.Job) bool {
	return j.Runner == store.RunnerCodex && CodexSandbox(j.Kind) == CodexSandboxWrite
}

// preambleProgressRule starts the preamble's progress bullet (prompts/preamble.md): a claude job keeps
// ProgressFile in its job dir, which `--add-dir` makes writable to it.
const preambleProgressRule = "- Keep `PROGRESS.md` in the job directory up to date"

// codexPreamble returns the preamble of j with the progress bullet a codex job can follow (issue #92): a
// write kind keeps its progress file at CodexProgressPath, the one path outside work/ its sandbox lets
// it write; a read-only kind keeps none. A claude job's preamble is returned as it is.
func codexPreamble(pre string, j *store.Job, jobDir string, cfg Config) (string, error) {
	if j.Runner != store.RunnerCodex {
		return pre, nil
	}
	start := strings.Index(pre, preambleProgressRule)
	if start < 0 {
		return "", errors.New("template prompts/preamble.md: progress rule not found")
	}
	end := len(pre)
	if n := strings.IndexByte(pre[start:], '\n'); n >= 0 {
		end = start + n
	}
	rule := "- Your sandbox is read-only: keep no progress file (`" + ProgressFile + "` cannot be written); " +
		"the report's Progress section is your record."
	sandbox := cfg.codexPromptSettings(j.Kind).sandbox
	if sandbox == CodexSandboxFullAccess {
		rule = "- This is a read-only task: keep no progress file; the report's Progress section is your record."
	}
	if codexProgressWritable(j) {
		path := CodexProgressPath(jobDir)
		dir := filepath.Dir(path)
		where := "Its folder is the one path outside `work/` your sandbox lets you write; the rest of the job directory is read-only."
		if sandbox == CodexSandboxFullAccess {
			where = "Keep progress in this folder even though Codex does not confine writes in this mode."
		}
		rule = fmt.Sprintf("- Keep `%s` at `%s` up to date after each meaningful step: what is done, what is next, "+
			"what not to redo. %s Write it with a shell heredoc (`cat > %s <<'EOF'`).",
			ProgressFile, path, where, path)
		if sandbox == CodexSandboxWrite {
			rule += fmt.Sprintf("\n- You cannot commit: the repository's git directory is read-only in your sandbox (`git add` and `git commit` "+
				"fail). Leave your changes in `work/` and write the commit message to `%s`; when you have finished, the "+
				"wrapper stages repository changes in `work/` that are not ignored (`git add -A`) and makes one commit with that message.",
				filepath.Join(dir, CodexCommitMessageFile))
		}
	}
	pre = pre[:start] + rule + pre[end:]
	pre = strings.Replace(pre, "(`claude -p`)", "(`codex exec`)", 1)
	pre = strings.Replace(pre, "If something is impossible, stop and say so in the report instead of improvising around the rules.",
		"If a step is unavailable, complete feasible work and list the unavailable step and reason in the report; do not bypass the sandbox.", 1)
	if sandbox != CodexSandboxFullAccess || CodexSandbox(j.Kind) == CodexSandboxReadOnly {
		pre = strings.Replace(pre, "Commit locally if the brief asks for changes; the orchestrator reviews and ships.",
			"Do not commit yourself; follow Your sandbox below. The orchestrator reviews and ships.", 1)
	}
	return pre, nil
}

// CodexNetworkOverride is the `-c` override that lets commands under codex's workspace-write sandbox
// reach the network (issue #92: `npm ci` failed with EAI_AGAIN): key network_access of codex's
// sandbox_workspace_write table (codex-cli 0.162.1). Read-only codex jobs never get it; under
// danger-full-access codex confines no network anyway.
const CodexNetworkOverride = "sandbox_workspace_write.network_access=true"

// CodexAgentsOverride is the `-c` override that keeps sub-agents off for models whose catalog entry
// selects multi-agent V2 (issue #243): `--disable multi_agent` turns off only the V1 collab feature, and
// codex lets `agents.enabled=false` (MultiAgentVersion::Disabled) override the model's V2 (codex
// rust-v0.172.0, codex-rs/core/src/config/mod.rs Config::multi_agent_version_override).
const CodexAgentsOverride = "agents.enabled=false"

// CodexArgs is the `codex exec` command line of a job: non-interactive, JSON events on stdout,
// isolated from the owner's codex setup (no user config, rules or session files, no MCP servers, the
// CodexDisabledFeatures off, sub-agents off by CodexAgentsOverride, never asks for approval), no
// git-repo check (a job without a repo works in an empty dir), the given sandbox (the kind's, CodexSandbox, or CodexSandboxFullAccess under
// codex_sandbox worktree-full-access), under workspace-write the job's progress folder in jobDir as the
// one extra writable root (codexProgressRoot) and, with network, CodexNetworkOverride, the job's model
// and reasoning effort; the prompt arrives on stdin ("-"). The working dir is the job's work dir (set by
// the wrapper).
func CodexArgs(j *store.Job, effort, sandbox, jobDir string, network bool) []string {
	return codexArgs(j, effort, sandbox, jobDir, network, codexSession{ephemeral: true})
}

// codexSession is how a codex launch treats its thread: ephemeral (`--ephemeral`: no session files, so
// nothing to resume), else persisted as a rollout file under codex's home and, with resume, continuing
// that thread (`codex exec resume <thread_id>`).
type codexSession struct {
	ephemeral bool
	resume    string
}

// codexArgs is CodexArgs for the given session. A resume keeps every isolation flag and puts
// `resume <thread_id>` after them, right before the stdin prompt: `--sandbox` and `--add-dir` are
// options of `codex exec` itself, not global ones (codex-cli 0.172.0, exec/src/cli.rs
// mark_exec_global_args), so they must precede the subcommand; the explicit sandbox is what makes
// codex apply it to the resumed thread (exec/src/lib.rs thread_resume_params_from_config).
func codexArgs(j *store.Job, effort, sandbox, jobDir string, network bool, s codexSession) []string {
	args := []string{"exec", "--json", "--ignore-user-config", "--ignore-rules"}
	if s.ephemeral {
		args = append(args, "--ephemeral")
	}
	args = append(args, "--skip-git-repo-check",
		"-s", sandbox, "-c", `approval_policy="never"`, "-c", "mcp_servers={}")
	if root := codexProgressRoot(sandbox, jobDir); root != "" {
		args = append(args, "--add-dir", root)
	}
	if network && sandbox == CodexSandboxWrite {
		args = append(args, "-c", CodexNetworkOverride)
	}
	for _, f := range CodexDisabledFeatures {
		args = append(args, "--disable", f)
	}
	args = append(args, "-c", CodexAgentsOverride)
	args = append(args, "-m", j.Model, "-c", "model_reasoning_effort="+effort)
	if s.resume != "" {
		args = append(args, "resume", s.resume)
	}
	return append(args, "-")
}

// codexThreadIDRe matches a codex thread id: a lower-case UUID (codex makes version 7 ones,
// protocol/src/thread_id.rs).
var codexThreadIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidCodexThreadID reports whether s is a codex thread id (a lower-case UUID): the only thing ever
// passed to `codex exec resume`.
func ValidCodexThreadID(s string) bool { return codexThreadIDRe.MatchString(s) }

// codexResumableJob reports whether a codex job keeps its thread for a resume: codex_resumable is on
// and it is not a gate job (those are never resumed).
func (c Config) codexResumableJob(j *store.Job) bool {
	return j.Runner == store.RunnerCodex && c.CodexResumableOn() && !IsGateKind(j.Kind)
}

// codexHome is codex's home as a job's codex sees it: $CODEX_HOME of env, else ~/.codex
// (utils/home-dir/src/lib.rs find_codex_home).
func codexHome(env []string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "CODEX_HOME="); ok && v != "" {
			return v
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// codexRolloutExists reports whether the thread has a rollout file codex can resume:
// <codex home>/sessions/YYYY/MM/DD/rollout-<timestamp>-<thread_id>.jsonl (rollout/src/recorder.rs,
// rollout_file_name.rs), a regular file found by eachCodexRollout (no link followed, a symlinked codex
// home included). Only names are matched; the file is never read.
func codexRolloutExists(home, threadID string) bool {
	found := false
	_ = eachCodexRollout(home, threadID, func(int, string, string, *unix.Stat_t) error {
		found = true
		return errStopWalk
	})
	return found
}

// errStopWalk ends an eachCodexRollout walk early (not a failure).
var errStopWalk = errors.New("stop walk")

// errCodexHomeLink: codex's home is a symlink (it may point at an archive of sessions) or is not spelled
// canonically, so nothing behind it is walked, read or removed.
var errCodexHomeLink = errors.New("codex home is a symlink or not canonical, not walked")

// openCodexHome opens codex's home directory without following a final symlink: a symlinked home is
// errCodexHomeLink. Trailing separators are stripped first ("/" stays "/"): with "link/" the kernel
// resolves the link before either check could see it. The path is never normalized: the kernel resolves
// "link/../home" through the link while a lexical clean names another directory, so a home with a "."
// or ".." component is errCodexHomeLink too.
func openCodexHome(home string) (int, error) {
	if t := strings.TrimRight(home, "/"); t != "" {
		home = t
	} else {
		home = "/"
	}
	for _, c := range strings.Split(home, "/") {
		if c == "." || c == ".." {
			return -1, errCodexHomeLink
		}
	}
	if fi, err := os.Lstat(home); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return -1, errCodexHomeLink
	}
	fd, err := unix.Open(home, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ELOOP) {
		return -1, errCodexHomeLink // swapped for a link after the Lstat
	}
	return fd, err
}

// codexRolloutNameRe matches the name codex gives a rollout file, rollout-<YYYY-MM-DDThh-mm-ss>-<thread
// id>.jsonl (rollout_file_name.rs); the submatch is the thread id.
var codexRolloutNameRe = regexp.MustCompile(`^rollout-[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}-[0-9]{2}-[0-9]{2}-([0-9a-f-]{36})\.jsonl$`)

// eachCodexRollout calls fn for every regular file <home>/sessions/<Y>/<M>/<D>/<name> whose name is codex's
// rollout name for threadID, in name order. Every directory below home is opened relative to its parent
// with O_NOFOLLOW and every entry is lstat'ed through it: a symlinked directory is skipped, never
// followed, and anything but a regular file (a directory, a link) is passed over. fn gets the open date
// directory (valid during the call) to act on the entry by name, so what it does stays inside it. A
// directory that cannot be opened or read is skipped, as Glob skips it; an error of fn ends the walk.
// The home itself is opened without following a final symlink: a symlinked home is not walked at all
// (errCodexHomeLink).
func eachCodexRollout(home, threadID string, fn func(dirfd int, path, name string, st *unix.Stat_t) error) error {
	if home == "" || !ValidCodexThreadID(threadID) {
		return nil
	}
	homeFd, err := openCodexHome(home)
	if errors.Is(err, errCodexHomeLink) {
		return err
	}
	if err != nil {
		return nil
	}
	fd, err := openDirNoFollow(homeFd, "sessions")
	unix.Close(homeFd)
	if err != nil {
		return nil
	}
	return walkCodexRolloutDir(fd, filepath.Join(home, "sessions"), 0, threadID, fn)
}

// walkCodexRolloutDir is eachCodexRollout for the open directory fd at path (closed on return): depth 0
// is sessions, 3 a day directory, whose rollout files are passed to fn.
func walkCodexRolloutDir(fd int, path string, depth int, threadID string, fn func(int, string, string, *unix.Stat_t) error) error {
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	names, _ := f.Readdirnames(-1)
	slices.Sort(names)
	for _, name := range names {
		if depth < 3 {
			sub, err := openDirNoFollow(fd, name)
			if err != nil {
				continue
			}
			if err := walkCodexRolloutDir(sub, filepath.Join(path, name), depth+1, threadID, fn); err != nil {
				return err
			}
			continue
		}
		if m := codexRolloutNameRe.FindStringSubmatch(name); m == nil || m[1] != threadID {
			continue
		}
		var st unix.Stat_t
		if unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
			continue
		}
		if err := fn(fd, filepath.Join(path, name), name, &st); err != nil {
			return err
		}
	}
	return nil
}

// codexUsage is a token usage as codex reports it (turn.completed, exec/src/exec_events.rs `Usage`):
// Input includes Cached, Output includes Reasoning.
type codexUsage struct {
	Input, Cached, Output, Reasoning int64
}

// total is what the token cap counts: input (cached included) plus output.
func (u codexUsage) total() int64 { return u.Input + u.Output }

// sub is u without base, no count below zero.
func (u codexUsage) sub(base codexUsage) codexUsage {
	return codexUsage{max(u.Input-base.Input, 0), max(u.Cached-base.Cached, 0), max(u.Output-base.Output, 0),
		max(u.Reasoning-base.Reasoning, 0)}
}

// codexUsageBase is what the resumed thread had used before this attempt, as far as the wrapper's
// records of the job's earlier attempts account for it: the last one that ran in the thread and saw its
// usage (CodexThreadUsage), or else the base it started from (CodexThreadBase, which counted what was
// unreported before it). codex reports a thread's total (exec/src/event_processor_with_jsonl_output.rs
// usage_from_last_total; a resumed thread restores it from its rollout), so an attempt's own usage is
// the total without its base. An attempt killed before its turn ended recorded no usage: the rollout
// (codexRolloutUsage) has it, see codexResumeBase.
func codexUsageBase(st *store.Store, id, threadID string, attempt int) codexUsage {
	for n := attempt - 1; n >= 1; n-- {
		raw, err := st.AttemptWrapperResult(id, n)
		if err != nil {
			continue
		}
		var res StreamResult
		if json.Unmarshal([]byte(raw), &res) != nil || res.CodexThreadID != threadID {
			continue
		}
		if res.CodexThreadUsage != nil {
			return *res.CodexThreadUsage
		}
		if res.CodexThreadBase != nil {
			return *res.CodexThreadBase
		}
	}
	return codexUsage{}
}

// codexResumeBase is the usage base of an attempt that resumes threadID, and the part of it no earlier
// attempt recorded (nil = none): the records' base (codexUsageBase), or the thread's total in its
// rollout file when that is more (an earlier attempt was killed before its turn reported its usage).
// The unrecorded part counts once, in this attempt's record (CodexUnreported), never against its cap.
// The rollout is read only under the read-only sandbox: under workspace-write (when codex's home lies in
// a writable root) and danger-full-access the job's commands can write it, so a forged total there could
// cancel the attempt's own usage and its cap. Those take the records' base alone: tokens of an attempt
// killed before its turn reported them are then counted in this attempt's usage and against its cap.
func codexResumeBase(st *store.Store, id, threadID string, attempt int, home, sandbox string) (codexUsage, *codexUsage) {
	recorded := codexUsageBase(st, id, threadID, attempt)
	if sandbox != CodexSandboxReadOnly {
		return recorded, nil
	}
	rollout, ok := codexRolloutUsage(home, threadID)
	if !ok || rollout.total() <= recorded.total() {
		return recorded, nil
	}
	unreported := rollout.sub(recorded)
	return rollout, &unreported
}

// codexRolloutMaxBytes bounds how much of a rollout file codexRolloutUsage reads; codexRolloutMaxLine
// bounds one line it parses (a token_count line is a few hundred bytes, longer ones are skipped).
const (
	codexRolloutMaxBytes = 256 << 20
	codexRolloutMaxLine  = 64 << 10
)

// codexRolloutUsage is the thread's total usage as its rollout file last recorded it (ok = one found):
// the total_token_usage of the last token_count event (a rollout line {"type":"event_msg","payload":
// {"type":"token_count","info":{"total_token_usage":{...}}}}, which codex writes after each model
// response). Only the rollout of threadID (a thread the job's own record names) is read, at most
// codexRolloutMaxBytes, and only its numbers are used.
func codexRolloutUsage(home, threadID string) (codexUsage, bool) {
	if home == "" || !ValidCodexThreadID(threadID) {
		return codexUsage{}, false
	}
	var best codexUsage
	found := false
	_ = eachCodexRollout(home, threadID, func(dirfd int, _, name string, _ *unix.Stat_t) error {
		if u, ok := rolloutFileUsage(dirfd, name); ok && (!found || u.total() > best.total()) {
			best, found = u, true
		}
		return nil
	})
	return best, found
}

// rolloutFileUsage is codexRolloutUsage for the file name in the open directory dirfd (a regular file
// only; a symlink is not followed, a fifo does not block).
func rolloutFileUsage(dirfd int, name string) (codexUsage, bool) {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return codexUsage{}, false
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return codexUsage{}, false
	}
	var last codexUsage
	found := false
	r := bufio.NewReaderSize(io.LimitReader(f, codexRolloutMaxBytes), codexRolloutMaxLine)
	for {
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			for errors.Is(err, bufio.ErrBufferFull) { // over the line limit: skipped to its end
				_, err = r.ReadSlice('\n')
			}
			if err != nil {
				break
			}
			continue
		}
		if bytes.Contains(line, []byte(`"token_count"`)) {
			if u, ok := tokenCountUsage(line); ok {
				last, found = u, true
			}
		}
		if err != nil {
			break
		}
	}
	return last, found
}

// tokenCountUsage reads the total of a rollout token_count line (ok = it is one, with no negative count).
func tokenCountUsage(line []byte) (codexUsage, bool) {
	var ev struct {
		Type    string `json:"type"`
		Payload struct {
			Type string `json:"type"`
			Info *struct {
				Total struct {
					Input     int64 `json:"input_tokens"`
					Cached    int64 `json:"cached_input_tokens"`
					Output    int64 `json:"output_tokens"`
					Reasoning int64 `json:"reasoning_output_tokens"`
				} `json:"total_token_usage"`
			} `json:"info"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &ev) != nil || ev.Type != "event_msg" || ev.Payload.Type != "token_count" || ev.Payload.Info == nil {
		return codexUsage{}, false
	}
	t := ev.Payload.Info.Total
	if t.Input < 0 || t.Cached < 0 || t.Output < 0 || t.Reasoning < 0 {
		return codexUsage{}, false
	}
	return codexUsage{t.Input, t.Cached, t.Output, t.Reasoning}, true
}

// codexUsageText is a codex usage for people.
func codexUsageText(u codexUsage) string {
	return fmt.Sprintf("tokens in %d (cached %d) · out %d (reasoning %d)", u.Input, u.Cached, u.Output, u.Reasoning)
}

// codexTokenCapStop is CodexStop of an attempt over its token cap.
func codexTokenCapStop(used, limit int64) string {
	return fmt.Sprintf("codex token cap exceeded: the attempt used %d of %d tokens (config codex_max_tokens; input plus output, "+
		"as codex reports them), so the wrapper stopped it", used, limit)
}

// codexMaxLineBytes caps one line of a codex stream; a longer line (a huge command output) is skipped
// and counted, never held whole.
var codexMaxLineBytes = MaxStreamLineBytes

// codexEvent is one line of `codex exec --json` output (the fields this package reads).
type codexEvent struct {
	Type string `json:"type"`
	// thread.started: the thread's id (ThreadStartedEvent), what `codex exec resume` takes.
	ThreadID string `json:"thread_id"`
	Item     struct {
		Type string `json:"type"`
		Text string `json:"text"`
		// command_execution items: the command's output, exit code and status (completed, failed, declined).
		AggregatedOutput string `json:"aggregated_output"`
		ExitCode         *int   `json:"exit_code"`
		Status           string `json:"status"`
	} `json:"item"`
	Usage *struct {
		InputTokens       int64 `json:"input_tokens"`
		CachedInputTokens int64 `json:"cached_input_tokens"`
		OutputTokens      int64 `json:"output_tokens"`
		// Part of OutputTokens (codex-cli 0.172.0 `Usage`; absent in older streams).
		ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	} `json:"usage"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// ParseCodexStream reads a `codex exec --json` stream: Found when the last turn ended (turn.completed
// or turn.failed; a turn.started after it voids it, so a stream cut inside a later turn is not ended),
// IsError when that turn failed, Text = the last agent message, Turns = the completed items other than
// reasoning and error warnings (messages, commands, file changes, tool calls), and the token usage
// summed over the turns. Lines over codexMaxLineBytes are skipped and counted (Skipped); a read error
// is kept in Err. The cost is never known (codex reports no price).
func ParseCodexStream(path string) StreamResult {
	out := StreamResult{Runner: store.RunnerCodex}
	f, err := os.Open(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			out.Err = err.Error()
		}
		return out
	}
	defer f.Close()
	skipped, err := readLines(f, codexMaxLineBytes, func(line []byte) {
		applyCodexLine(&out, line)
	})
	out.Skipped = skipped
	if err != nil {
		out.Err = err.Error()
	}
	return out
}

func applyCodexLine(out *StreamResult, line []byte) {
	var ev codexEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return
	}
	if out.Results > 0 {
		out.AfterEnd++
	}
	switch ev.Type {
	case "thread.started":
		if ValidCodexThreadID(ev.ThreadID) {
			out.CodexThreadID = ev.ThreadID
		}
	case "turn.started":
		out.Found, out.IsError, out.Subtype = false, false, ""
	case "item.completed":
		// "error" items are warnings (e.g. unrecognized config settings), not work done.
		if ev.Item.Type == "reasoning" || ev.Item.Type == "error" || ev.Item.Type == "" {
			return
		}
		out.Turns++
		if ev.Item.Type == "agent_message" && strings.TrimSpace(ev.Item.Text) != "" {
			out.Text = ev.Item.Text
			out.CodexMessageBwrap = out.CodexMessageBwrap || strings.Contains(ev.Item.Text, "bwrap:")
		}
		if ev.Item.Type == "command_execution" {
			applyCodexCommand(out, ev)
		}
	case "turn.completed":
		out.Results++
		out.Found, out.IsError, out.Subtype = true, false, "success"
		if ev.Usage != nil {
			out.InputTokens += ev.Usage.InputTokens
			out.CachedInputTokens += ev.Usage.CachedInputTokens
			out.OutputTokens += ev.Usage.OutputTokens
			out.ReasoningTokens += ev.Usage.ReasoningOutputTokens
		}
	case "turn.failed":
		out.Results++
		out.Found, out.IsError, out.Subtype = true, true, "turn_failed"
		if msg := RedactPrintable(ev.Error.Message, 200); msg != "" {
			out.Subtype += ": " + msg
		}
	}
}

// applyCodexCommand counts a completed command_execution item: one with exit code 0 and no failed or
// declined status is a successful command, and the first bwrap error in a failed one's output is kept
// (issue #81). Other non-zero exits (rg without a match, a failing test) are ordinary evidence.
func applyCodexCommand(out *StreamResult, ev codexEvent) {
	out.CodexCommands++
	failed := ev.Item.ExitCode == nil || *ev.Item.ExitCode != 0 || ev.Item.Status == "failed" || ev.Item.Status == "declined"
	if !failed {
		out.CodexCommandsOK++
		return
	}
	if out.CodexSandboxError == "" {
		out.CodexSandboxError = bwrapErrorLine(ev.Item.AggregatedOutput, false)
	}
}

// codexSandboxHint is the fix a codex job whose sandbox did not start points to: the host first (on
// Ubuntu 24.04 AppArmor keeps codex's bwrap from creating its user namespace), worktree-full-access only
// as the last resort.
const codexSandboxHint = "on Ubuntu 24.04 the usual cause is kernel.apparmor_restrict_unprivileged_userns=1: " +
	"give codex's bwrap an AppArmor profile with `userns` (README, «Troubleshooting: codex sandbox and AppArmor»); config codex_sandbox \"" +
	CodexSandboxModeWorktreeFullAccess + "\" is the last resort"

// codexStreamFailure is why a codex stream does not count as a success whatever codex's exit code: it
// could not be read to its end; a command failed with a bwrap error; its last turn has no terminal
// turn.completed event; or it is a repo job (CodexRepo) in which not one command ran successfully (the
// sandbox did not start: nothing was read, so the report is not a result; codex under read-only may
// show no command item at all then). "" = it does. A stream with skipped lines is not judged on the
// last rule (a skipped line may be a command that worked). The final message is model text and never
// decides the verdict; a bwrap error quoted in it is only named in the reason.
func codexStreamFailure(res StreamResult) string {
	switch {
	case res.CodexStop != "":
		return res.CodexStop // the wrapper stopped it: over its token cap, or a resume in another thread
	case res.Err != "":
		return "codex stream not read to its end: " + RedactPrintable(res.Err, 200)
	case res.CodexSandboxError != "":
		return fmt.Sprintf("codex sandbox failed to start (%q); %s", res.CodexSandboxError, codexSandboxHint)
	case !res.Found:
		return "codex stream has no terminal turn.completed event (codex stopped early or the stream is truncated)"
	case res.CodexRepo && res.CodexCommandsOK == 0 && res.Skipped == 0:
		ran := "0 commands"
		if res.CodexCommands > 0 {
			ran = fmt.Sprintf("%d commands, none exited 0", res.CodexCommands)
		}
		quoted := ""
		if res.CodexMessageBwrap {
			quoted = "; codex's final message (untrusted model text) mentions bwrap"
		}
		return fmt.Sprintf("codex ran no command in the repository successfully (%s), so nothing was read; the codex "+
			"sandbox may not have started%s (see stream.jsonl); %s", ran, quoted, codexSandboxHint)
	}
	return ""
}

// codexRepoJob reports whether the wrapper's job has a repository: the job row's repo path, or, when the
// row cannot be read, a work/.git (fail closed: the stricter verdict).
func codexRepoJob(st *store.Store, id, jobDir string) bool {
	if j, err := st.Get(id); err == nil {
		return j.RepoPath != ""
	}
	_, err := os.Lstat(filepath.Join(jobDir, "work", ".git"))
	return err == nil
}

// writeCodexResultReport writes the final message from the trusted stdout result when the runner
// ended in a completed turn and the job did not write report.md itself, and logs token usage.
func writeCodexResultReport(jobDir string, res StreamResult, log io.Writer) {
	fmt.Fprintf(log, "run-wrapper: codex usage: input %d (cached %d), output %d (reasoning %d) tokens; %d items\n",
		res.InputTokens, res.CachedInputTokens, res.OutputTokens, res.ReasoningTokens, res.Turns)
	if res.Skipped > 0 {
		fmt.Fprintf(log, "run-wrapper: codex stream: %d line(s) over %d bytes skipped\n", res.Skipped, codexMaxLineBytes)
	}
	// Only a stream that ended in a completed turn has a final message: an earlier one is not the report.
	if reason := codexStreamFailure(res); reason != "" {
		fmt.Fprintln(log, "run-wrapper:", reason)
		return
	}
	if res.IsError || hasReport(jobDir) || res.Text == "" {
		return
	}
	if err := writeAtomic(filepath.Join(jobDir, "report.md"), []byte(res.Text+"\n")); err != nil {
		fmt.Fprintln(log, "run-wrapper: write report.md:", err)
	}
}

// codexLaunch is what a codex job's launch was authorized with: the launch config, the sandbox and the
// network of this attempt.
type codexLaunch struct {
	cfg     Config
	sandbox string
	network bool
}

// plan is the launch of j's codex attempt in session s, with prompt as the stdin file ("" = prompt.md).
func (c *codexLaunch) plan(m *Manager, j *store.Job, s codexSession, prompt ...string) launchPlan {
	p := launchPlan{bin: c.cfg.codexBin(), wrapper: []string{"--runner", store.RunnerCodex},
		args:            codexArgs(j, c.cfg.reasoningEffort(j.ReasoningEffort), c.sandbox, m.JobDir(j.ID), c.network, s),
		codexFullAccess: c.sandbox == CodexSandboxFullAccess}
	if len(prompt) > 0 && prompt[0] != "" {
		p.wrapper = append(p.wrapper, "--prompt", prompt[0])
	}
	return p
}

// resumePlan is the launch of a resumed codex attempt (prepareResume wrote both prompts): the thread of
// the job's last attempt is continued with the update when it is known and its rollout file is still
// there; otherwise (an ephemeral attempt, codex_resumable off now, session files removed) a fresh
// thread gets the brief, the previous attempt and the update.
func (c *codexLaunch) resumePlan(m *Manager, j *store.Job, token string) (launchPlan, error) {
	thread, why := j.SessionID, ""
	switch {
	case !c.cfg.codexResumableJob(j):
		why = "codex_resumable is off"
	case !ValidCodexThreadID(thread):
		why = "has no recorded codex thread"
	case !codexRolloutExists(codexHome(m.jobSpawnEnv()), thread):
		why = "has no rollout file under codex's sessions dir"
	}
	if why == "" {
		if err := m.Store.SetSession(j.ID, token, thread, store.RunResume); err != nil {
			return launchPlan{}, err
		}
		j.RunMode = store.RunResume
		return c.plan(m, j, codexSession{resume: thread}, resumePromptFile), nil
	}
	m.Log.Printf("job %s: codex thread %q %s, attempt %d starts a fresh thread", j.ID, thread, why, j.Attempt)
	j.SessionID, j.RunMode = "", store.RunFallback
	if err := m.Store.SetSession(j.ID, token, "", store.RunFallback); err != nil {
		return launchPlan{}, err
	}
	return c.plan(m, j, codexSession{ephemeral: !c.cfg.codexResumableJob(j)}, fallbackPromptFile), nil
}

// noteCodexThread records the thread of a codex attempt that is ending (the wrapper's trusted record of
// its thread.started event, kept only for a run that persisted it) as the job's session, so the next
// attempt can resume it. Called while the row still runs under its launch token.
func (m *Manager) noteCodexThread(j *store.Job, res StreamResult) {
	if j.Runner != store.RunnerCodex {
		return
	}
	thread := res.CodexThreadID
	if res.CodexThreadRejected {
		// A resume that codex ran in another thread: that thread has neither the brief nor the update, and
		// the one it was launched for could not be found. No session is kept, so the next attempt starts a
		// fresh thread with the full fallback prompt.
		thread = ""
	} else if !ValidCodexThreadID(thread) {
		return
	}
	if thread == j.SessionID {
		return
	}
	if err := m.Store.SetSession(j.ID, j.LaunchToken, thread, j.RunMode); err != nil {
		m.Log.Printf("job %s: record codex thread: %v", j.ID, err)
		return
	}
	j.SessionID = thread
}

// codexJobUsage sums the usage of a codex job's attempts from the wrapper's records (ok = one had any).
func (m *Manager) codexJobUsage(j *store.Job) (sum codexUsage, ok bool) {
	add := func(res StreamResult) {
		u := codexUsage{res.InputTokens, res.CachedInputTokens, res.OutputTokens, res.ReasoningTokens}
		if u != (codexUsage{}) {
			sum, ok = codexUsage{sum.Input + u.Input, sum.Cached + u.Cached, sum.Output + u.Output, sum.Reasoning + u.Reasoning}, true
		}
	}
	addAll := func(res StreamResult) {
		add(res)
		if res.CodexUnreported != nil {
			// Tokens of an earlier attempt that did not report them (killed mid-turn), recovered once.
			u := *res.CodexUnreported
			add(StreamResult{InputTokens: u.Input, CachedInputTokens: u.Cached, OutputTokens: u.Output, ReasoningTokens: u.Reasoning})
		}
	}
	for n := 1; n < j.Attempt; n++ {
		var res StreamResult
		if raw, err := m.Store.AttemptWrapperResult(j.ID, n); err == nil && json.Unmarshal([]byte(raw), &res) == nil {
			addAll(res)
		}
	}
	addAll(m.endedResult(j))
	return sum, ok
}

// codexDoneCost is the cost field of a codex job's job-done line: cost (its estimate, "" = unknown) and
// the job's token usage over all attempts, as the wrapper read it from codex's stdout.
func (m *Manager) codexDoneCost(j *store.Job, cost string) string {
	u, ok := m.codexJobUsage(j)
	if !ok {
		return cost
	}
	if cost == "" {
		cost = "cost " + CostText(j.CostUSD, j.CostIsUnknown())
	}
	return cost + " · " + codexUsageText(u)
}
