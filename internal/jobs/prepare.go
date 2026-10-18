package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	agentjobs "github.com/qwwiwi/agent-jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// denyRules is «Ничего наружу»: deny rules hold even under bypassPermissions.
var denyRules = []string{
	"Bash(git push:*)",
	"Bash(git tag:*)",
	"Bash(gh release:*)",
	"Bash(gh api:*)",
	"Bash(gh pr create:*)",
	"Bash(gh pr new:*)",
	"Bash(gh pr comment:*)",
	"Bash(gh pr edit:*)",
	"Bash(gh pr merge:*)",
	"Bash(gh pr review:*)",
	"Bash(gh issue create:*)",
	"Bash(gh issue new:*)",
	"Bash(gh issue comment:*)",
	"Bash(gh issue edit:*)",
	// close/reopen take --comment / -c: a job may not publish comments in any form.
	"Bash(gh pr close:*)",
	"Bash(gh pr reopen:*)",
	"Bash(gh issue close:*)",
	"Bash(gh issue reopen:*)",
	"Bash(gh gist create:*)",
	"Bash(gh gist new:*)",
	"Bash(gh gist edit:*)",
	// Attached / string forms the guard rejects as unverifiable (`git -cuser.name=x`, `env -S '…'`).
	"Bash(git -c*)",
	"Bash(git --config-env*)",
	"Bash(git --exec-path*)",
	"Bash(env -S*)",
	"Bash(env --split-string*)",
	// Prefix rules miss spellings like `git -C x push` or `gh pr -R o/r create`;
	// the guard hook parses every Bash command and blocks those too (fail closed).
	"Bash(git send-pack:*)",
	"Bash(git http-push:*)",
	// A job is a leaf: no nested agents (also rejected by the guard in any spelling).
	"Bash(claude:*)",
	"Bash(codex:*)",
	"Bash(gemini:*)",
	"Bash(aider:*)",
	"Bash(cursor-agent:*)",
	"Bash(claude-code:*)",
	"Bash(gemini-cli:*)",
	"Bash(aider-chat:*)",
	// The user's service manager and session bus would start processes outside the job's cgroup scope
	// (also rejected by the guard in any spelling).
	"Bash(systemd-run:*)",
	"Bash(systemctl --user:*)",
	"Bash(busctl:*)",
	"Bash(dbus-send:*)",
	"Bash(gdbus:*)",
	"Bash(qdbus:*)",
	"Bash(dbus-run-session:*)",
	"Bash(qdbus-qt5:*)",
	"Bash(qdbus6:*)",
	// Daemons and schedulers that run a command in their own cgroup, outside the job's scope, and new
	// namespaces (owner decision 2026-10-17; also rejected by the guard).
	"Bash(crontab:*)",
	"Bash(at:*)",
	"Bash(batch:*)",
	"Bash(tmux:*)",
	"Bash(screen:*)",
	"Bash(pm2:*)",
	"Bash(docker:*)",
	"Bash(docker-compose:*)",
	"Bash(podman:*)",
	"Bash(podman-compose:*)",
	"Bash(ssh:*)",
	"Bash(machinectl:*)",
	"Bash(loginctl:*)",
	"Bash(unshare:*)",
	"Bash(nsenter:*)",
	// Package runners fetch and run a package by name (also rejected by the guard).
	"Bash(npx:*)",
	"Bash(pnpx:*)",
	"Bash(bunx:*)",
	"Bash(uvx:*)",
	"Bash(npm exec:*)",
	"Bash(pnpm dlx:*)",
	"Bash(yarn dlx:*)",
	"Bash(bun x:*)",
	"Bash(pipx run:*)",
	"Bash(uv tool run:*)",
	// File tools never write inside a .git directory (also checked by the guard, symlinks resolved).
	"Edit(**/.git/**)",
	"Write(**/.git/**)",
	"MultiEdit(**/.git/**)",
	"NotebookEdit(**/.git/**)",
	// Tools that reach a human or outlive the run (own schedules, notifications).
	"CronCreate",
	"RemoteTrigger",
	"PushNotification",
}

// webTools are closed for every kind except research.
var webTools = []string{"WebFetch", "WebSearch"}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// GuardCommand is the PreToolUse hook command line for this agent's config and the job's git dirs,
// without a subagent log (every subagent refused). Jobs get JobGuardCommand.
func (m *Manager) GuardCommand(gitDirs ...string) string { return m.guardCommand("", nil, gitDirs...) }

// JobGuardCommand is GuardCommand with the job's subagent log, so the guard can hold the cap of
// guard.MaxSubagentsPerJob subagents per job.
func (m *Manager) JobGuardCommand(id string, gitDirs ...string) string {
	return m.guardCommand(id, m.skillProtected(nil), gitDirs...)
}

// ProtectedPaths are the agent-jobs state a job never writes: every job's subagent log and the job
// database (with its WAL files) that holds the monotonic subagent counter.
func (m *Manager) ProtectedPaths() []string {
	db := filepath.Join(m.StateDir, "jobs.db")
	return []string{filepath.Join(m.StateDir, "subagents"), db, db + "-wal", db + "-shm", db + "-journal"}
}

// SubagentLogPath is <state>/subagents/<id>.log: one line per subagent the guard allowed. It lies
// outside the job directory (the job's --add-dir), so the job is not told to write there.
func (m *Manager) SubagentLogPath(id string) string {
	return filepath.Join(m.StateDir, "subagents", id+".log")
}

func (m *Manager) guardCommand(jobID string, extraProtected []string, gitDirs ...string) string {
	parts := []string{shellQuote(m.SelfBin), "guard"}
	for _, n := range m.Cfg.OwnerNames {
		parts = append(parts, "--owner-name", shellQuote(n))
	}
	if m.Cfg.GitName != "" {
		parts = append(parts, "--git-name", shellQuote(m.Cfg.GitName))
	}
	if m.Cfg.GitEmail != "" {
		parts = append(parts, "--git-email", shellQuote(m.Cfg.GitEmail))
	}
	for _, d := range gitDirs {
		parts = append(parts, "--git-dir", shellQuote(d))
	}
	if jobID != "" {
		parts = append(parts, "--subagent-log", shellQuote(m.SubagentLogPath(jobID)),
			"--subagent-db", shellQuote(filepath.Join(m.StateDir, "jobs.db")), "--subagent-job", shellQuote(jobID))
		for _, p := range append(m.ProtectedPaths(), extraProtected...) {
			parts = append(parts, "--protect", shellQuote(p))
		}
	}
	return strings.Join(parts, " ")
}

// Settings builds the job's only settings.json: deny rules + the guard hook (told the job's git dirs),
// with no subagent log (subagents refused). Jobs get JobSettings.
func (m *Manager) Settings(kind string, gitDirs ...string) map[string]any {
	return m.settings(kind, m.GuardCommand(gitDirs...))
}

// JobSettings is the settings.json of job j: Settings plus the job's subagent log in the guard hook.
// The subagent tool (Agent, formerly Task) is not denied: a job may use up to three subagents, which
// run inside the job's process, under these settings and this hook, within the job's limits.
// The catalog roots and the job's skill links are protected (deny rules for file tools, --protect for
// the guard): a job reads its skills but never changes the catalog through them.
func (m *Manager) JobSettings(j *store.Job, gitDirs ...string) map[string]any {
	protected := m.skillProtected(j)
	return m.settings(j.Kind, m.guardCommand(j.ID, protected, gitDirs...), skillDenyRules(protected)...)
}

func (m *Manager) settings(kind, guardCmd string, extraDeny ...string) map[string]any {
	deny := append(append([]string{}, denyRules...), extraDeny...)
	if kind != "research" {
		deny = append(deny, webTools...)
	}
	return map[string]any{
		// Settings given with --settings outrank project settings: a .claude/settings.json in the work dir
		// (the repository's, or one the job wrote before a resume) cannot turn the guard hook off.
		"disableAllHooks": false,
		"permissions":     map[string]any{"deny": deny},
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{
					"matcher": "*",
					"hooks":   []any{map[string]any{"type": "command", "command": guardCmd}},
				},
			},
		},
	}
}

func template(name string) (string, error) {
	b, err := agentjobs.Templates.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("template %s: %w", name, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// readOwnerRules returns the owner rules or an error that stops the job from starting.
func (m *Manager) readOwnerRules() (string, error) {
	p := m.Cfg.OwnerRulesPath
	fi, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("owner rules %s unreadable: %w", p, err)
	}
	if fi.Size() > MaxOwnerRulesBytes {
		return "", fmt.Errorf("owner rules %s is %d bytes, limit is %d", p, fi.Size(), MaxOwnerRulesBytes)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("owner rules %s unreadable: %w", p, err)
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", fmt.Errorf("owner rules %s is empty", p)
	}
	return strings.TrimSpace(string(b)), nil
}

// BuildPrompt assembles prompt.md: preamble, subagent rules, owner rules (the job profile), the
// project cards that match the job's repository, the memory notes, task, kind checklist, report contract.
func (m *Manager) BuildPrompt(j *store.Job, ownerRules string, cards []Card, notes MemoryNotes) (string, error) {
	pre, err := template("prompts/preamble.md")
	if err != nil {
		return "", err
	}
	sub, err := template("prompts/subagents.md")
	if err != nil {
		return "", err
	}
	check, err := template("prompts/kind-" + j.Kind + ".md")
	if err != nil {
		return "", err
	}
	contract, err := template("prompts/report-contract.md")
	if err != nil {
		return "", err
	}
	work := "Your working directory is an empty directory."
	if j.RepoPath != "" {
		work = fmt.Sprintf("Your working directory is a git worktree of the repository at base commit %s (detached HEAD). "+
			"Commit locally; never push. The diff is measured from that base.", j.BaseCommit)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n%s%s\n\n## Owner rules\n\n%s\n\n", pre, skillsLine(j), sub, ownerRules)
	b.WriteString(RenderCards(cards))
	b.WriteString(RenderMemoryNotes(notes))
	fmt.Fprintf(&b, "## Task: %s\n\nJob id: %s · kind: %s\n\n%s\n\n%s\n\n%s\n\n%s\n",
		j.Title, j.ID, j.Kind, work, strings.TrimSpace(j.Brief), check, contract)
	return b.String(), nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// JobMCPServerName is the job-role server's key in the job's mcp.json.
const JobMCPServerName = "agent-jobs"

// JobMCPConfig is the job's mcp.json: one stdio server, `agent-jobs mcp --role job --agent <agent>`.
// The job-role server exposes only the tools allow-listed for jobs and refuses every other call.
func (m *Manager) JobMCPConfig() map[string]any {
	return map[string]any{"mcpServers": map[string]any{
		JobMCPServerName: map[string]any{
			"type":    "stdio",
			"command": m.SelfBin,
			"args":    []string{"mcp", "--role", "job", "--agent", m.Agent},
		},
	}}
}

// prepare creates the job directory: prompt.md, settings.json, mcp.json, work/, base_commit.
func (m *Manager) prepare(j *store.Job, token, dir string) error {
	rules, err := m.readOwnerRules()
	if err != nil {
		return err
	}
	cards, err := m.CardsFor(j.RepoPath)
	if err != nil {
		return err
	}
	// Memory notes never block the job: an unreachable brain is written into the brief instead.
	notes := m.LookupMemoryNotes(j)
	if notes.Err != nil {
		m.Log.Printf("job %s: memory notes unavailable: %v", j.ID, notes.Err)
	}
	prompt, err := m.BuildPrompt(j, rules, cards, notes)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create job dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(m.SubagentLogPath(j.ID)), 0o700); err != nil {
		return fmt.Errorf("create subagent log dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.md"), []byte(prompt), 0o600); err != nil {
		return err
	}
	if err := m.writeGitFiles(dir); err != nil {
		return err
	}
	// The job's only MCP server is agent-jobs itself in the job role (allow-listed tools only);
	// --strict-mcp-config makes this the complete list.
	if err := writeJSON(filepath.Join(dir, "mcp.json"), m.JobMCPConfig()); err != nil {
		return err
	}
	work := filepath.Join(dir, "work")
	settings := filepath.Join(dir, "settings.json")
	if j.RepoPath == "" {
		if err := os.MkdirAll(work, 0o700); err != nil {
			return err
		}
		if err := m.deliverSkills(j, token, work); err != nil {
			return err
		}
		return writeJSON(settings, m.JobSettings(j))
	}
	if _, err := os.Stat(work); err == nil {
		return fmt.Errorf("work dir %s already exists", work)
	}
	if _, err := git(j.RepoPath, "worktree", "add", "--detach", work, j.BaseCommit); err != nil {
		return err
	}
	if err := m.markWorktree(j, work); err != nil {
		return err
	}
	// The guard learns the worktree's real git dirs (its admin dir and the common dir), which need
	// not be named .git, so file tools cannot write there.
	gitDir, err := git(work, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	common, err := git(work, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if err := m.deliverSkills(j, token, work); err != nil {
		return err
	}
	if err := writeJSON(settings, m.JobSettings(j, gitDir, common)); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "base_commit"), []byte(j.BaseCommit+"\n"), 0o600)
}

// worktreeOwnerFile lives in the worktree's admin dir (<repo>/.git/worktrees/<name>/): git leaves
// it alone and deletes it together with the worktree, so it proves which job created the worktree.
const worktreeOwnerFile = "agent-jobs-owner"

// WorktreeInfo is worktree.json: what this job created, recorded at creation.
type WorktreeInfo struct {
	Path   string `json:"path"`
	GitDir string `json:"gitdir"`
	Head   string `json:"head"`
}

func (m *Manager) markWorktree(j *store.Job, work string) error {
	gitDir, err := git(work, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(gitDir, worktreeOwnerFile), []byte(j.ID+"\n"), 0o600); err != nil {
		return fmt.Errorf("mark worktree: %w", err)
	}
	return writeJSON(filepath.Join(m.JobDir(j.ID), "worktree.json"), WorktreeInfo{Path: work, GitDir: gitDir, Head: j.BaseCommit})
}

// writeGitFiles writes <jobdir>/gitconfig (the job identity from config, nothing else) and the empty
// <jobdir>/no-hooks dir; the wrapper points GIT_CONFIG_GLOBAL and core.hooksPath at them (GitEnv).
func (m *Manager) writeGitFiles(dir string) error {
	cfg := filepath.Join(dir, gitConfigFile)
	if err := os.WriteFile(cfg, nil, 0o600); err != nil {
		return fmt.Errorf("write gitconfig: %w", err)
	}
	for _, kv := range [][2]string{{"user.name", m.Cfg.GitName}, {"user.email", m.Cfg.GitEmail}} {
		if kv[1] == "" {
			continue
		}
		cmd := exec.Command("git", "config", "--file", cfg, kv[0], kv[1])
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("write gitconfig %s: %w: %s", kv[0], err, strings.TrimSpace(string(out)))
		}
	}
	if err := os.Chmod(cfg, 0o400); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(dir, noHooksDir), 0o500); err != nil {
		return fmt.Errorf("create no-hooks dir: %w", err)
	}
	return nil
}
