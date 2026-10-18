package jobs

import (
	"bufio"
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

// CodexArgs is the `codex exec` command line of a job: non-interactive, JSON events on stdout, the
// job's model and reasoning effort, the kind's sandbox, no git-repo check (a job without a repo works
// in an empty dir); the prompt arrives on stdin ("-"). The working dir is the job's work dir (set by
// the wrapper).
func CodexArgs(j *store.Job, effort string) []string {
	return []string{
		"exec", "--json", "-m", j.Model, "-c", "model_reasoning_effort=" + effort,
		"--sandbox", CodexSandbox(j.Kind), "--skip-git-repo-check", "-",
	}
}

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

// ParseCodexStream reads a `codex exec --json` stream: Found when a turn ended (turn.completed or
// turn.failed), IsError when the last turn failed, Text = the last agent message, Turns = the completed
// items other than reasoning (messages, commands, file changes, tool calls), and the token usage summed
// over the turns. The cost is never known (codex reports no price).
func ParseCodexStream(path string) StreamResult {
	out := StreamResult{Runner: store.RunnerCodex}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var ev codexEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "item.completed":
			if ev.Item.Type == "reasoning" || ev.Item.Type == "" {
				continue
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
	}
	return out
}

// errCodexResume is the refusal of a resume of a codex job.
var errCodexResume = errors.New("resume of a codex job is not supported: start a new job " +
	"(codex exec has a resume subcommand, but it is not verified to work under the job's containment yet)")

// writeCodexReport writes the last agent message of a codex stream as report.md when the job did not
// write report.md itself, and logs the token usage to stderr.log.
func writeCodexReport(jobDir string, log io.Writer) {
	res := ParseCodexStream(filepath.Join(jobDir, "stream.jsonl"))
	fmt.Fprintf(log, "run-wrapper: codex usage: input %d (cached %d), output %d tokens; %d items\n",
		res.InputTokens, res.CachedInputTokens, res.OutputTokens, res.Turns)
	if hasReport(jobDir) || res.Text == "" {
		return
	}
	if err := writeAtomic(filepath.Join(jobDir, "report.md"), []byte(res.Text+"\n")); err != nil {
		fmt.Fprintln(log, "run-wrapper: write report.md:", err)
	}
}
