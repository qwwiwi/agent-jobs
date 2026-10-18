package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

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

// ReasoningEfforts are the accepted codex reasoning efforts; DefaultReasoningEffort applies when neither
// the job nor the config names one.
var ReasoningEfforts = []string{"low", "medium", "high"}

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

// reloadCodexConfig reads and validates config.json for the launch of a codex job; a config that does
// not load refuses the launch (fail closed).
func reloadCodexConfig(stateDir string) (Config, error) {
	cfg, err := LoadConfig(stateDir)
	if err != nil {
		return Config{}, fmt.Errorf("codex job not launched: config reload failed: %w", err)
	}
	return cfg, nil
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

// ValidateReasoningEffort rejects an effort other than low|medium|high (empty is accepted: default).
func ValidateReasoningEffort(e string) error {
	if e != "" && !slices.Contains(ReasoningEfforts, e) {
		return fmt.Errorf("reasoning effort %q must be one of %s", e, strings.Join(ReasoningEfforts, "|"))
	}
	return nil
}

func (c Config) validateCodex() error {
	if err := ValidateReasoningEffort(c.CodexReasoningEffort); err != nil {
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
// With --ignore-user-config, --ignore-rules, --ephemeral and an empty mcp_servers table, a job never
// inherits the owner's codex config, rules, MCP servers, apps or plugins: the sandbox is the whole
// containment (this exact command line verified on codex-cli 0.162.1).
var CodexDisabledFeatures = []string{
	"apps", "browser_use", "browser_use_external", "browser_use_full_cdp_access", "computer_use",
	"image_generation", "multi_agent", "plugins", "plugin_sharing", "remote_plugin",
	"skill_mcp_dependency_install", "daemon_auto_start", "in_app_local_automation", "goals",
	"realtime_conversation", "worktrees", "hooks",
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
			if msg := Printable(ev.Error.Message, 200); msg != "" {
				out.Subtype += ": " + msg
			}
		}
	})
	out.Skipped = skipped
	if err != nil {
		out.Err = err.Error()
	}
	return out
}

// codexStreamFailure is why a codex stream does not count as a success whatever codex's exit code: it
// could not be read to its end, or its last turn has no terminal turn.completed event. "" = it does.
func codexStreamFailure(res StreamResult) string {
	switch {
	case res.Err != "":
		return "codex stream not read to its end: " + Printable(res.Err, 200)
	case !res.Found:
		return "codex stream has no terminal turn.completed event (codex stopped early or the stream is truncated)"
	}
	return ""
}

// errCodexResume is the refusal of a resume of a codex job.
var errCodexResume = errors.New("resume of a codex job is not supported: start a new job " +
	"(codex exec has a resume subcommand, but it is not verified to work under the job's containment yet)")

// writeCodexReport writes the last agent message of a codex stream as report.md when the stream ended
// in a completed turn and the job did not write report.md itself, and logs the token usage to
// stderr.log.
func writeCodexReport(jobDir string, log io.Writer) {
	res := ParseCodexStream(filepath.Join(jobDir, "stream.jsonl"))
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
