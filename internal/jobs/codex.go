package jobs

import (
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
	"slices"
	"strings"
	"time"

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

// CodexSandbox is the codex sandbox of a job kind: read-only for review and research, workspace-write
// for fix and general.
func CodexSandbox(kind string) string {
	if kind == "review" || kind == "research" {
		return CodexSandboxReadOnly
	}
	return CodexSandboxWrite
}

// checkCodexJob refuses a codex job the config does not allow: a workspace-write job (fix/general)
// unless codex_write_allowed is set, since the guard hook does not run inside codex.
func (c Config) checkCodexJob(kind string) error {
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
// inherit the owner's codex config, rules, apps or plugins (this exact command line verified on
// codex-cli 0.162.1); system/managed config layers are ruled out by checkCodexSystemConfig and MCP
// servers from other config layers by checkCodexMCP, so the sandbox is the whole containment.
var CodexDisabledFeatures = []string{
	"apps", "browser_use", "browser_use_external", "browser_use_full_cdp_access", "computer_use",
	"image_generation", "multi_agent", "plugins", "plugin_sharing", "remote_plugin",
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

// CodexArgs is the `codex exec` command line of a job: non-interactive, JSON events on stdout,
// isolated from the owner's codex setup (no user config, rules or session files, no MCP servers, the
// CodexDisabledFeatures off, never asks for approval), no git-repo check (a job without a repo works in
// an empty dir), the kind's sandbox, the job's model and reasoning effort; the prompt arrives on stdin
// ("-"). The working dir is the job's work dir (set by the wrapper).
func CodexArgs(j *store.Job, effort string) []string {
	args := []string{
		"exec", "--json", "--ignore-user-config", "--ignore-rules", "--ephemeral", "--skip-git-repo-check",
		"-s", CodexSandbox(j.Kind), "-c", `approval_policy="never"`, "-c", "mcp_servers={}",
	}
	for _, f := range CodexDisabledFeatures {
		args = append(args, "--disable", f)
	}
	return append(args, "-m", j.Model, "-c", "model_reasoning_effort="+effort, "-")
}

// codexMaxLineBytes caps one line of a codex stream; a longer line (a huge command output) is skipped
// and counted, never held whole.
var codexMaxLineBytes = MaxStreamLineBytes

// codexEvent is one line of `codex exec --json` output (the fields this package reads).
type codexEvent struct {
	Type string `json:"type"`
	Item struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	Usage *struct {
		InputTokens       int64 `json:"input_tokens"`
		CachedInputTokens int64 `json:"cached_input_tokens"`
		OutputTokens      int64 `json:"output_tokens"`
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
	switch ev.Type {
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
		}
	case "turn.completed":
		out.Found, out.IsError, out.Subtype = true, false, "success"
		if ev.Usage != nil {
			out.InputTokens += ev.Usage.InputTokens
			out.CachedInputTokens += ev.Usage.CachedInputTokens
			out.OutputTokens += ev.Usage.OutputTokens
		}
	case "turn.failed":
		out.Found, out.IsError, out.Subtype = true, true, "turn_failed"
		if msg := RedactPrintable(ev.Error.Message, 200); msg != "" {
			out.Subtype += ": " + msg
		}
	}
}

// codexStreamFailure is why a codex stream does not count as a success whatever codex's exit code: it
// could not be read to its end, or its last turn has no terminal turn.completed event. "" = it does.
func codexStreamFailure(res StreamResult) string {
	switch {
	case res.Err != "":
		return "codex stream not read to its end: " + RedactPrintable(res.Err, 200)
	case !res.Found:
		return "codex stream has no terminal turn.completed event (codex stopped early or the stream is truncated)"
	}
	return ""
}

// errCodexResume is the refusal of a resume of a codex job.
var errCodexResume = errors.New("resume of a codex job is not supported: start a new job " +
	"(codex exec has a resume subcommand, but it is not verified to work under the job's containment yet)")

// writeCodexResultReport writes the final message from the trusted stdout result when the runner
// ended in a completed turn and the job did not write report.md itself, and logs token usage.
func writeCodexResultReport(jobDir string, res StreamResult, log io.Writer) {
	fmt.Fprintf(log, "run-wrapper: codex usage: input %d (cached %d), output %d tokens; %d items\n",
		res.InputTokens, res.CachedInputTokens, res.OutputTokens, res.Turns)
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
