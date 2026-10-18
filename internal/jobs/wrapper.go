package jobs

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// StreamResult is the final `result` event of claude's stream-json output (or, for a codex job, what
// ParseCodexStream reads from codex's JSON events).
type StreamResult struct {
	Found   bool
	Subtype string
	IsError bool
	Turns   int
	CostUSD float64
	Text    string
	// Runner is the program that wrote the stream; empty = claude.
	Runner string
	// Token usage (codex streams only).
	InputTokens, CachedInputTokens, OutputTokens int64
}

// program names the runner of a stream result in messages.
func (r StreamResult) program() string {
	if r.Runner == "" {
		return store.RunnerClaude
	}
	return r.Runner
}

// parseRunnerStream parses a job's stream.jsonl with the parser of its runner.
func parseRunnerStream(runner, path string) StreamResult {
	if runner == store.RunnerCodex {
		return ParseCodexStream(path)
	}
	return parseStream(path)
}

type resultEvent struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	NumTurns     int     `json:"num_turns"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Result       string  `json:"result"`
}

// ParseStream returns the last `result` event in stream.jsonl (zero value if none).
func ParseStream(path string) StreamResult {
	var out StreamResult
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !json.Valid(line) {
			continue
		}
		var ev resultEvent
		if err := json.Unmarshal(line, &ev); err != nil || ev.Type != "result" {
			continue
		}
		out = StreamResult{Found: true, Subtype: ev.Subtype, IsError: ev.IsError,
			Turns: ev.NumTurns, CostUSD: ev.TotalCostUSD, Text: ev.Result}
	}
	return out
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// WrapperInfo is wrapper.json: the wrapper registers itself here before claude starts, so a
// daemon that died between spawning it and recording the pid can adopt it on the next tick.
type WrapperInfo struct {
	PID   int    `json:"pid"`
	PGID  int    `json:"pgid"`
	Token string `json:"launch_token"`
}

// ReadWrapperInfo reads <jobdir>/wrapper.json.
func ReadWrapperInfo(jobDir string) (WrapperInfo, error) {
	var w WrapperInfo
	b, err := os.ReadFile(filepath.Join(jobDir, "wrapper.json"))
	if err != nil {
		return w, err
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return w, fmt.Errorf("parse wrapper.json: %w", err)
	}
	if w.PID <= 0 || w.Token == "" {
		return w, errors.New("wrapper.json is incomplete")
	}
	return w, nil
}

// admitLock takes <jobdir>/admit.lock exclusively. The wrapper holds it while it checks the job
// row and registers in wrapper.json; Cancel and the launch-grace failure hold it while they read
// wrapper.json and finish the row. So either the wrapper registers first (and is then found and
// killed), or the row is terminal first (and the wrapper refuses to start claude).
func admitLock(jobDir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(jobDir, "admit.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open admit lock: %w", err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("admit flock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// lockAdmitIfExists takes the admit lock when the job dir exists (no dir = no wrapper can exist).
func lockAdmitIfExists(jobDir string) (func(), error) {
	if _, err := os.Stat(jobDir); err != nil {
		return func() {}, nil
	}
	return admitLock(jobDir)
}

// admitted checks, in <state>/jobs.db (jobDir = <state>/jobs/<id>), that the job is still running
// under this launch token.
func admitted(jobDir, token string) error {
	abs, err := filepath.Abs(jobDir)
	if err != nil {
		return err
	}
	dbPath := filepath.Join(filepath.Dir(filepath.Dir(abs)), "jobs.db")
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("job database: %w", err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	j, err := st.Get(filepath.Base(abs))
	if err != nil {
		return err
	}
	if j.Status != store.StatusRunning || j.LaunchToken != token {
		return fmt.Errorf("job is %s under another launch or no longer running", j.Status)
	}
	return nil
}

// RunWrapper runs claude with prompt.md on stdin, stdout→stream.jsonl, stderr→stderr.log,
// cwd=<jobdir>/work; then extracts the final result text into report.md and writes exit_code.
// exit_code is written last, so its presence means the job's files are complete.
// It holds an exclusive lock on <jobdir>/wrapper.lock for its whole life: a second wrapper
// for the same job refuses to run, so a job is never executed twice. Before registering it checks
// (under admit.lock) that the row is still running under its token; a late wrapper whose job was
// cancelled or failed meanwhile exits 4 without running claude and without writing exit_code.
//
// opts.Prompt names the stdin file in the job dir (default prompt.md). With opts.FallbackPrompt and
// opts.FallbackSession (a resume launch: argv holds --resume <id>), a claude that fails before any
// progress (no assistant or result event: the session could not be resumed) is run once more in a
// fresh session: --resume <id> becomes --session-id <FallbackSession>, stdin is FallbackPrompt, the
// failed stream is kept as stream-resume-failed.jsonl and FallbackMarker records the new session id.
func RunWrapper(jobDir, token string, argv []string, opts WrapperOptions) int {
	if len(argv) == 0 || token == "" {
		fmt.Fprintln(os.Stderr, "run-wrapper: missing launch token or claude command")
		return 2
	}
	if err := opts.validate(); err != nil {
		fmt.Fprintln(os.Stderr, "run-wrapper:", err)
		return 2
	}
	lock, err := os.OpenFile(filepath.Join(jobDir, "wrapper.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run-wrapper:", err)
		return 2
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Fprintln(os.Stderr, "run-wrapper: another wrapper already runs this job:", err)
		return 3
	}
	if _, err := os.Stat(filepath.Join(jobDir, "exit_code")); err == nil {
		fmt.Fprintln(os.Stderr, "run-wrapper: job already ran (exit_code exists)")
		return 3
	}
	unlockAdmit, err := admitLock(jobDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run-wrapper:", err)
		return 2
	}
	if err := admitted(jobDir, token); err != nil {
		unlockAdmit()
		fmt.Fprintln(os.Stderr, "run-wrapper: refused:", err)
		return 4
	}
	pgid, err := syscall.Getpgid(0)
	if err != nil {
		pgid = os.Getpid()
	}
	info, _ := json.Marshal(WrapperInfo{PID: os.Getpid(), PGID: pgid, Token: token})
	err = writeAtomic(filepath.Join(jobDir, "wrapper.json"), info)
	unlockAdmit()
	if err != nil {
		fmt.Fprintln(os.Stderr, "run-wrapper: register:", err)
		return 2
	}
	stderrLog, err := os.OpenFile(filepath.Join(jobDir, "stderr.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run-wrapper:", err)
		return 2
	}
	defer stderrLog.Close()
	// The idle-after-result rule reads claude's result events; a codex job ends by itself or by the timeout.
	idle := time.Duration(0)
	if opts.Runner != store.RunnerCodex {
		idle = idleGrace(jobDir, stderrLog)
	}
	code, idleClosed := runClaude(jobDir, opts.prompt(), argv, idle, stderrLog)
	if !idleClosed && opts.FallbackPrompt != "" && code != 0 && code < 128 && !streamProgressed(filepath.Join(jobDir, "stream.jsonl")) {
		code, idleClosed = runFallback(jobDir, argv, opts, idle, stderrLog, code)
	}
	// Nothing the job started may outlive it: every other process of the wrapper's session goes now,
	// before exit_code says the job is over (a markerless orphan could otherwise start a nested job
	// once the session leader is gone; it also leaves no ghost preview servers or browsers).
	sw := sweepJob(jobDir, stderrLog)
	if sw.Signalled > 0 {
		fmt.Fprintf(stderrLog, "run-wrapper: ended %d leftover process(es) of the job's session\n", sw.Signalled)
	}
	if sw.Incomplete() {
		// Fail closed: exit_code is still written (the job is over for the daemon), but the marker makes
		// the daemon fail the job instead of reporting a clean success.
		fmt.Fprintf(stderrLog, "run-wrapper: sweep incomplete: survivors %v, error: %v\n", sw.Survivors, sw.Err)
		if err := writeSweepMarker(jobDir, sw); err != nil {
			fmt.Fprintln(stderrLog, "run-wrapper: write sweep marker:", err)
			return 2 // no exit_code: the daemon fails the job as a dead wrapper
		}
	}
	if opts.Runner == store.RunnerCodex {
		writeCodexReport(jobDir, stderrLog)
	} else if res := ParseStream(filepath.Join(jobDir, "stream.jsonl")); idleClosed {
		code = finishIdleClosed(jobDir, res, stderrLog)
	} else if text := res.Text; text != "" || !hasReport(jobDir) {
		if text == "" && res.Found && !res.IsError && res.Subtype == "success" {
			// A successful result without text and no report.md of the job's own: the last top-level
			// assistant text that is a report is the report.
			text = lastReportText(filepath.Join(jobDir, "stream.jsonl"))
		}
		if text != "" {
			if err := writeAtomic(filepath.Join(jobDir, "report.md"), []byte(text+"\n")); err != nil {
				fmt.Fprintln(stderrLog, "run-wrapper: write report.md:", err)
			}
		}
	}
	if err := writeAtomic(filepath.Join(jobDir, "exit_code"), []byte(fmt.Sprintf("%d\n", code))); err != nil {
		fmt.Fprintln(stderrLog, "run-wrapper: write exit_code:", err)
		return 2
	}
	return 0
}

// WrapperOptions are the run-wrapper options of a resume launch (see RunWrapper).
type WrapperOptions struct {
	Prompt          string // stdin file name in the job dir; empty = prompt.md
	FallbackPrompt  string // stdin file name for the fresh-session fallback; empty = no fallback
	FallbackSession string // the fallback's session id
	Runner          string // the program argv runs: claude (default) or codex
}

func (o WrapperOptions) prompt() string {
	if o.Prompt == "" {
		return "prompt.md"
	}
	return o.Prompt
}

func (o WrapperOptions) validate() error {
	for _, f := range []string{o.Prompt, o.FallbackPrompt} {
		if f != "" && (f != filepath.Base(f) || f == "." || f == "..") {
			return fmt.Errorf("prompt file %q must be a file name in the job dir", f)
		}
	}
	if (o.FallbackPrompt == "") != (o.FallbackSession == "") {
		return errors.New("--fallback-prompt and --fallback-session go together")
	}
	switch o.Runner {
	case "", store.RunnerClaude:
	case store.RunnerCodex:
		if o.FallbackPrompt != "" {
			return errors.New("a codex job has no resume fallback")
		}
	default:
		return fmt.Errorf("runner %q must be claude or codex", o.Runner)
	}
	if o.FallbackSession != "" && !ValidSessionID(o.FallbackSession) {
		return fmt.Errorf("fallback session %q is not a session id", o.FallbackSession)
	}
	return nil
}

// FallbackMarker is the job-dir file the wrapper writes when a resume fell back to a fresh session;
// it holds the new session id.
const FallbackMarker = "fallback_session"

// runFallback reruns claude in a fresh session after a failed resume (see RunWrapper).
func runFallback(jobDir string, argv []string, opts WrapperOptions, idle time.Duration, stderrLog *os.File, code int) (int, bool) {
	fresh := make([]string, 0, len(argv))
	replaced := false
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--resume" && i+1 < len(argv) && !replaced {
			fresh = append(fresh, "--session-id", opts.FallbackSession)
			i++
			replaced = true
			continue
		}
		fresh = append(fresh, argv[i])
	}
	if !replaced {
		return code, false
	}
	fmt.Fprintf(stderrLog, "run-wrapper: resume failed (exit %d, no progress); falling back to a fresh session %s\n",
		code, opts.FallbackSession)
	if err := os.Rename(filepath.Join(jobDir, "stream.jsonl"), filepath.Join(jobDir, "stream-resume-failed.jsonl")); err != nil {
		fmt.Fprintln(stderrLog, "run-wrapper: keep failed resume stream:", err)
	}
	if err := writeAtomic(filepath.Join(jobDir, FallbackMarker), []byte(opts.FallbackSession+"\n")); err != nil {
		fmt.Fprintln(stderrLog, "run-wrapper: write fallback marker:", err)
		return code, false
	}
	return runClaude(jobDir, opts.FallbackPrompt, fresh, idle, stderrLog)
}

// streamProgressed reports whether stream.jsonl holds an assistant or result event: claude got as
// far as the model. A resume that fails before that never ran a turn.
func streamProgressed(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var ev struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) == nil && (ev.Type == "assistant" || ev.Type == "result") {
			return true
		}
	}
	return false
}

// idleGrace is the idle-after-result grace from the agent's config.json (<state>/jobs/<id> = jobDir);
// an unreadable config falls back to the default.
func idleGrace(jobDir string, log io.Writer) time.Duration {
	abs, err := filepath.Abs(jobDir)
	if err == nil {
		var c Config
		if c, err = LoadConfig(filepath.Dir(filepath.Dir(abs))); err == nil {
			return c.IdleAfterResult()
		}
	}
	fmt.Fprintln(log, "run-wrapper: config (idle-after-result grace falls back to the default):", err)
	return DefaultIdleAfterResult
}

// runClaude runs claude and returns its exit code, and whether the idle-after-result rule ended it.
func runClaude(jobDir, promptFile string, argv []string, idle time.Duration, stderrLog *os.File) (int, bool) {
	stdin, err := os.Open(filepath.Join(jobDir, promptFile))
	if err != nil {
		fmt.Fprintln(stderrLog, "run-wrapper:", err)
		return 127, false
	}
	defer stdin.Close()
	stdout, err := os.Create(filepath.Join(jobDir, "stream.jsonl"))
	if err != nil {
		fmt.Fprintln(stderrLog, "run-wrapper:", err)
		return 127, false
	}
	defer stdout.Close()
	env, err := GitEnv(jobDir, os.Environ())
	if err != nil {
		fmt.Fprintln(stderrLog, "run-wrapper: git environment:", err)
		return 127, false
	}
	env, err = JobEnv(jobDir, env)
	if err != nil {
		fmt.Fprintln(stderrLog, "run-wrapper: job marker:", err)
		return 127, false
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = filepath.Join(jobDir, "work")
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderrLog
	closed, err := runIdleWatched(cmd, filepath.Join(jobDir, "stream.jsonl"), idle, stderrLog)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return 128 + int(ws.Signal()), closed
			}
			return ee.ExitCode(), closed
		}
		fmt.Fprintln(stderrLog, "run-wrapper: start claude:", err)
		return 127, false
	}
	return 0, closed
}

// JobEnvVar marks every process of a job: the wrapper sets AGENT_JOBS_JOB=<job id> on claude, and
// children inherit it. The agent-jobs binary refuses its control commands under it (no nested jobs);
// doctor procs will find a job's processes by it. The guard rejects unsetting or changing it.
const JobEnvVar = "AGENT_JOBS_JOB"

// InsideJob returns the job id when this process runs inside a job (the marker is set, even empty).
func InsideJob() (string, bool) { return os.LookupEnv(JobEnvVar) }

// userBusVars point at the user's D-Bus session (systemd user manager). They are removed from the job's
// environment: with them, `systemd-run --user` or a D-Bus call would start a process in a new unit,
// outside the job's scope (not a sandbox: a script can rebuild the address; the guard rejects the
// direct spellings).
var userBusVars = map[string]bool{"DBUS_SESSION_BUS_ADDRESS": true, "XDG_RUNTIME_DIR": true}

// JobEnv returns env without any inherited marker and without the user bus variables, plus
// AGENT_JOBS_JOB=<id of jobDir>.
func JobEnv(jobDir string, env []string) ([]string, error) {
	abs, err := filepath.Abs(jobDir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k != JobEnvVar && !userBusVars[k] {
			out = append(out, kv)
		}
	}
	return append(out, JobEnvVar+"="+filepath.Base(abs)), nil
}

// Job git environment: files in the job dir (written by prepare; created empty here if missing).
const (
	gitConfigFile = "gitconfig" // GIT_CONFIG_GLOBAL: only the job identity
	noHooksDir    = "no-hooks"  // core.hooksPath: empty, so repository hooks never run
)

// GitEnv returns env without any GIT_*, PAGER, EDITOR, VISUAL variable, plus the job's git
// environment: no system config, <jobdir>/gitconfig as the only global config, cat as pager, true as
// editor, no terminal prompt, and the gitOverrides (core.hooksPath=<jobdir>/no-hooks, no fsmonitor,
// ssh command or credential helper; cat/true as pager/editor) at command-line level (via
// GIT_CONFIG_COUNT), which wins over any repository config. So git run by the job does not execute
// hooks, a pager, an editor or a credential prompt the guard does not see.
func GitEnv(jobDir string, env []string) ([]string, error) {
	abs, err := filepath.Abs(jobDir)
	if err != nil {
		return nil, err
	}
	cfg, hooks := filepath.Join(abs, gitConfigFile), filepath.Join(abs, noHooksDir)
	if err := ensureGitFiles(cfg, hooks); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(env)+12)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_") || k == "PAGER" || k == "EDITOR" || k == "VISUAL" {
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+cfg,
		"GIT_PAGER=cat", "PAGER=cat",
		"GIT_EDITOR=true", "EDITOR=true", "VISUAL=true",
		"GIT_TERMINAL_PROMPT=0",
	)
	overrides := gitOverrides(hooks)
	out = append(out, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(overrides)))
	for i, kv := range overrides {
		out = append(out, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return out, nil
}

// gitOverrides are set at command-line level (GIT_CONFIG_COUNT), which beats repository config: a
// repo-local config the job edits directly cannot bring back hooks, an fsmonitor or ssh command, a
// credential helper, a pager or an editor.
//
// diff.external is deliberately NOT overridden: git has no value that disables it (an empty value
// makes every `git diff` fail with "cannot run : No such file or directory"). The daemon's own diffs
// pass --no-ext-diff --no-textconv instead; inside the job a repo-local diff.external is only reachable
// by editing .git/config directly, which the guard rejects as a direct spelling (not a sandbox).
func gitOverrides(hooks string) [][2]string {
	return [][2]string{
		{"core.hooksPath", hooks},
		{"core.fsmonitor", "false"},
		{"core.sshCommand", "false"},
		{"credential.helper", ""}, // an empty value resets the helper list
		{"core.pager", "cat"},
		{"core.editor", "true"},
	}
}

// ensureGitFiles creates an empty read-only gitconfig and an empty read-only hooks dir when missing.
func ensureGitFiles(cfg, hooks string) error {
	if _, err := os.Lstat(cfg); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(cfg, nil, 0o400); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := os.Mkdir(hooks, 0o500); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	fi, err := os.Lstat(hooks)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", hooks)
	}
	return nil
}
