package jobs

import (
	"encoding/json"
	"fmt"
	"os"
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
	"Bash(gh pr comment:*)",
	"Bash(gh pr review:*)",
	"Bash(gh issue create:*)",
	"Bash(gh issue comment:*)",
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
	return os.WriteFile(filepath.Join(dir, "base_commit"), []byte(j.BaseCommit+"\n"), 0o600)
}
