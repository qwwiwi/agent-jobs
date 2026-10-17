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
	// Tools that reach a human or outlive the run (own schedules, notifications).
	"CronCreate",
	"RemoteTrigger",
	"PushNotification",
}

// webTools are closed for every kind except research.
var webTools = []string{"WebFetch", "WebSearch"}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// GuardCommand is the PreToolUse hook command line for this agent's config.
func (m *Manager) GuardCommand() string {
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
	return strings.Join(parts, " ")
}

// Settings builds the job's only settings.json: deny rules + the guard hook.
func (m *Manager) Settings(kind string) map[string]any {
	deny := append([]string{}, denyRules...)
	if kind != "research" {
		deny = append(deny, webTools...)
	}
	return map[string]any{
		"permissions": map[string]any{"deny": deny},
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{
					"matcher": "*",
					"hooks":   []any{map[string]any{"type": "command", "command": m.GuardCommand()}},
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

// BuildPrompt assembles prompt.md: preamble, owner rules, task, kind checklist, report contract.
func (m *Manager) BuildPrompt(j *store.Job, ownerRules string) (string, error) {
	pre, err := template("prompts/preamble.md")
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
	fmt.Fprintf(&b, "%s\n\n## Owner rules\n\n%s\n\n## Task: %s\n\nJob id: %s · kind: %s\n\n%s\n\n%s\n\n%s\n\n%s\n",
		pre, ownerRules, j.Title, j.ID, j.Kind, work, strings.TrimSpace(j.Brief), check, contract)
	return b.String(), nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// prepare creates the job directory: prompt.md, settings.json, mcp.json, work/, base_commit.
func (m *Manager) prepare(j *store.Job, dir string) error {
	rules, err := m.readOwnerRules()
	if err != nil {
		return err
	}
	prompt, err := m.BuildPrompt(j, rules)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create job dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.md"), []byte(prompt), 0o600); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "settings.json"), m.Settings(j.Kind)); err != nil {
		return err
	}
	if err := m.writeGitFiles(dir); err != nil {
		return err
	}
	// Stage 1: no MCP servers. --strict-mcp-config makes this the complete list.
	if err := writeJSON(filepath.Join(dir, "mcp.json"), map[string]any{"mcpServers": map[string]any{}}); err != nil {
		return err
	}
	work := filepath.Join(dir, "work")
	if j.RepoPath == "" {
		return os.MkdirAll(work, 0o700)
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
