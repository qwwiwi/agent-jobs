package jobs

import (
	"bufio"
	"bytes"
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
	"sync"
	"syscall"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// StreamResult is the final `result` event of claude's stream-json output (or, for a codex job, what
// ParseCodexStream reads from codex's JSON events).
type StreamResult struct {
	// Completion and provenance are written only by the wrapper into the token-bound DB record.
	Completed          bool
	ExitCode           int
	IdleReason         string
	SweepFailure       string
	FallbackSession    string
	Progressed         bool
	Tokens             TokenUsage
	TokensInvalid      bool
	ReportPresent      bool
	ReportContractNote string
	ReportText         string `json:"-"` // last report from trusted assistant stdout

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
	// Skipped counts lines over the line limit that were skipped unread (codex streams only).
	Skipped int
	// Err is why the stream could not be read to its end ("" = read through; codex streams only).
	Err string
}

// program names the runner of a stream result in messages.
func (r StreamResult) program() string {
	if r.Runner == "" {
		return store.RunnerClaude
	}
	return r.Runner
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
	sc.Buffer(make([]byte, 0, 1<<20), maxStreamLine)
	for sc.Scan() {
		if r, ok := parseResultLine(sc.Bytes()); ok {
			out = r
		}
	}
	return out
}

// maxStreamLine is the longest stream line read; a longer one is skipped.
const maxStreamLine = 64 << 20

// parseResultLine parses one stream line; ok when it is a `result` event.
func parseResultLine(line []byte) (StreamResult, bool) {
	if !bytes.Contains(line, []byte(`"result"`)) || !json.Valid(line) {
		return StreamResult{}, false
	}
	var ev resultEvent
	if err := json.Unmarshal(line, &ev); err != nil || ev.Type != "result" {
		return StreamResult{}, false
	}
	return StreamResult{Found: true, Subtype: ev.Subtype, IsError: ev.IsError,
		Turns: ev.NumTurns, CostUSD: ev.TotalCostUSD, Text: ev.Result}, true
}

// resultTee passes the runner's stdout through to stream.jsonl and keeps the last result event it saw
// on the way. stream.jsonl lies in the job dir, which the job can write (a result line appended there
// is the job's own); the pipe from the runner is the wrapper's, so only what the tee saw is trusted.
type resultTee struct {
	runner  string
	w       io.Writer
	line    []byte
	skip    bool // the current line is longer than maxStreamLine: dropped
	res     StreamResult
	mu      sync.Mutex // stdout copy and idle watcher run concurrently
	watch   streamWatch
	seen    map[string]bool
	observe func(StreamResult)
	tokens  streamTokenCounter
}

func (t *resultTee) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, err := t.w.Write(p)
	if n > 0 {
		t.watch.active = time.Now()
	}
	t.scan(p[:n])
	return n, err
}

func (t *resultTee) scan(p []byte) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		if !t.skip {
			limit := maxStreamLine
			if t.runner == store.RunnerCodex {
				limit = codexMaxLineBytes
			}
			if len(t.line)+len(chunk) > limit {
				t.skip, t.line = true, nil
				t.res.Progressed = true // unreadable output cannot prove a pre-model resume failure
			} else {
				t.line = append(t.line, chunk...)
			}
		}
		if i < 0 {
			return
		}
		t.end()
		p = p[i+1:]
	}
}

// end parses the line read so far (also a last line without a newline) and starts the next one.
func (t *resultTee) end() {
	if t.skip {
		t.tokens.invalid = true
		t.watch.lastResult = false
	} else {
		t.watch.line(t.line)
	}
	if t.runner == store.RunnerCodex {
		if t.skip {
			t.res.Skipped++
		} else {
			applyCodexLine(&t.res, t.line)
		}
	} else if !t.skip {
		var kind struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(t.line, &kind) == nil && (kind.Type == "assistant" || kind.Type == "result") {
			t.res.Progressed = true
		}
		var ev struct {
			Type    string          `json:"type"`
			Parent  json.RawMessage `json:"parent_tool_use_id"`
			Message struct {
				ID      string `json:"id"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(t.line, &ev) == nil {
			if ev.Type == "assistant" {
				if t.seen == nil {
					t.seen = map[string]bool{}
				}
				if !t.res.Found && (ev.Message.ID == "" || !t.seen[ev.Message.ID]) {
					t.res.Turns++
				}
				t.seen[ev.Message.ID] = true
				if parent := strings.TrimSpace(string(ev.Parent)); parent == "" || parent == "null" {
					for _, c := range ev.Message.Content {
						if c.Type == "text" && isReport(c.Text) {
							t.res.ReportText = c.Text
						}
					}
				}
			}
		}
		if r, ok := parseResultLine(t.line); ok {
			r.Progressed, r.ReportText = true, t.res.ReportText
			t.res = r
		}
	}
	if !t.skip && bytes.Contains(t.line, []byte(`"usage"`)) {
		var ev statsEvent
		if json.Unmarshal(t.line, &ev) == nil {
			t.tokens.apply(ev)
		}
	}
	t.res.Tokens, t.res.TokensInvalid = t.tokens.total, t.tokens.invalid
	if t.observe != nil {
		t.observe(t.res)
	}
	t.line, t.skip = t.line[:0], false
}

// idleState reads only bytes received from runner stdout, never the diagnostic file.
func (t *resultTee) idleState() (bool, time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.watch.lastResult && len(t.line) == 0 && !t.skip && t.watch.bgTasks == 0, t.watch.active
}

// openWrapperStore opens the protected state database for a job directory.
func openWrapperStore(jobDir string) (*store.Store, string, error) {
	abs, err := filepath.Abs(jobDir)
	if err != nil {
		return nil, "", err
	}
	st, err := store.Open(filepath.Join(filepath.Dir(filepath.Dir(abs)), "jobs.db"))
	return st, filepath.Base(abs), err
}

func storeResult(st *store.Store, id, token string, res StreamResult) error {
	res.Text = "" // reports stay out of the verdict record
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return st.SetWrapperResult(id, token, string(b))
}

// recordResult persists a token-bound progress or completion snapshot (also used by lifecycle tests).
func recordResult(jobDir, token string, res StreamResult) error {
	st, id, err := openWrapperStore(jobDir)
	if err != nil {
		return err
	}
	defer st.Close()
	return storeResult(st, id, token, res)
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
	if raw, ok, err := st.WrapperResult(j.ID, token); err != nil {
		return err
	} else if ok {
		var res StreamResult
		if json.Unmarshal([]byte(raw), &res) != nil || res.Completed {
			return errors.New("job already completed this launch")
		}
	}
	if j.Runner == store.RunnerCodex && CodexSandbox(j.Kind) == CodexSandboxWrite {
		a, err := st.ReadLaunchAuthorization(j.ID, token)
		if err != nil || !a.CodexWriteAllowed {
			return errors.New("codex_write_allowed was not authorized by the launching daemon")
		}
	}
	return nil
}

// RunWrapper runs claude with prompt.md on stdin, stdout→stream.jsonl, stderr→stderr.log,
// cwd=<jobdir>/work; then extracts the final result text into report.md and writes exit_code.
// The token-bound DB completion is authoritative; exit_code is diagnostic only.
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
	st, id, err := openWrapperStore(jobDir)
	if err != nil {
		fmt.Fprintln(stderrLog, "run-wrapper: result database:", err)
		return 2
	}
	defer st.Close()
	record := func(res StreamResult) error { return storeResult(st, id, token, res) }
	observe := func(res StreamResult) {
		if err := record(res); err != nil {
			fmt.Fprintln(stderrLog, "run-wrapper: record progress:", err)
		}
	}
	code, idleClosed, res := runClaude(jobDir, opts.prompt(), argv, opts.Runner, idle, stderrLog, observe)
	if !idleClosed && opts.FallbackPrompt != "" && code != 0 && code < 128 && !res.Progressed && !res.Found {
		code, idleClosed, res = runFallback(jobDir, argv, opts, idle, stderrLog, code, res, record)
	}
	// Nothing the job started may outlive it: every other process of the wrapper's session goes now,
	// before trusted completion says the job is over (a markerless orphan could otherwise start a nested job
	// once the session leader is gone; it also leaves no ghost preview servers or browsers).
	sw := sweepJob(jobDir, stderrLog)
	if sw.Signalled > 0 {
		fmt.Fprintf(stderrLog, "run-wrapper: ended %d leftover process(es) of the job's session\n", sw.Signalled)
	}
	if sw.Incomplete() {
		// Persist the actual sweep failure with completion; the marker is diagnostic.
		fmt.Fprintf(stderrLog, "run-wrapper: sweep incomplete: survivors %v, error: %v\n", sw.Survivors, sw.Err)
		if err := writeSweepMarker(jobDir, sw); err != nil {
			fmt.Fprintln(stderrLog, "run-wrapper: write sweep marker:", err)
			return 2 // no completion: the daemon fails the job as a dead wrapper
		}
	}
	if opts.Runner == store.RunnerCodex {
		writeCodexResultReport(jobDir, res, stderrLog)
	} else if idleClosed {
		code, res.IdleReason = finishIdleClosed(jobDir, res, stderrLog)
	} else if text := res.Text; text != "" || !hasReport(jobDir) {
		if text == "" && res.Found && !res.IsError && res.Subtype == "success" {
			// A successful result without text and no report.md of the job's own: the last top-level
			// assistant text that is a report is the report.
			text = res.ReportText
		}
		if text != "" {
			if err := writeAtomic(filepath.Join(jobDir, "report.md"), []byte(text+"\n")); err != nil {
				fmt.Fprintln(stderrLog, "run-wrapper: write report.md:", err)
			}
		}
	}
	// The acceptance command (if the job has one) runs only after a clean claude run: exit 0, a report,
	// a complete sweep. Its result goes into the job row before exit_code says the job is over.
	terminalOK := res.Found && !res.IsError && res.Subtype == "success"
	if opts.Runner == store.RunnerCodex {
		terminalOK = codexStreamFailure(res) == ""
	}
	if code == 0 && terminalOK && !sw.Incomplete() && hasReport(jobDir) {
		if rc := runAcceptanceStep(jobDir, token, stderrLog, &res); rc != 0 {
			return rc
		}
	}
	if sw.Incomplete() {
		res.SweepFailure = sweepResultFailure(sw)
	}
	res.Completed, res.ExitCode = true, code
	res.ReportPresent = hasReport(jobDir)
	if report, err := readSmall(filepath.Join(jobDir, "report.md"), maxContractReportBytes+1); err == nil && len(report) <= maxContractReportBytes {
		res.ReportContractNote = reportContractNote(report)
	}
	if err := record(res); err != nil {
		fmt.Fprintln(stderrLog, "run-wrapper: record completion:", err)
		return 2
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
func runFallback(jobDir string, argv []string, opts WrapperOptions, idle time.Duration, stderrLog *os.File, code int,
	res StreamResult, record func(StreamResult) error) (int, bool, StreamResult) {
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
		return code, false, res
	}
	fmt.Fprintf(stderrLog, "run-wrapper: resume failed (exit %d, no progress); falling back to a fresh session %s\n",
		code, opts.FallbackSession)
	if err := os.Rename(filepath.Join(jobDir, "stream.jsonl"), filepath.Join(jobDir, "stream-resume-failed.jsonl")); err != nil {
		fmt.Fprintln(stderrLog, "run-wrapper: keep failed resume stream:", err)
	}
	if err := writeAtomic(filepath.Join(jobDir, FallbackMarker), []byte(opts.FallbackSession+"\n")); err != nil {
		fmt.Fprintln(stderrLog, "run-wrapper: write fallback marker:", err)
	}
	// Persist actual fallback use before starting the fresh runner, including a later killed attempt.
	res = StreamResult{FallbackSession: opts.FallbackSession}
	if err := record(res); err != nil {
		fmt.Fprintln(stderrLog, "run-wrapper: record fallback:", err)
		return code, false, res
	}
	observe := func(r StreamResult) {
		r.FallbackSession = opts.FallbackSession
		if err := record(r); err != nil {
			fmt.Fprintln(stderrLog, "run-wrapper: record progress:", err)
		}
	}
	code, closed, res := runClaude(jobDir, opts.FallbackPrompt, fresh, opts.Runner, idle, stderrLog, observe)
	res.FallbackSession = opts.FallbackSession
	return code, closed, res
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

// teeWaitDelay bounds the wait for descendants that inherited runner stdout.
var teeWaitDelay = 10 * time.Second

// runClaude runs the job's runner (claude, or codex when runner is codex) and returns its exit code,
// whether the idle-after-result rule ended it, and the result parsed from its stdout.
func runClaude(jobDir, promptFile string, argv []string, runner string, idle time.Duration, stderrLog *os.File, observe ...func(StreamResult)) (int, bool, StreamResult) {
	stdin, err := os.Open(filepath.Join(jobDir, promptFile))
	if err != nil {
		fmt.Fprintln(stderrLog, "run-wrapper:", err)
		return 127, false, StreamResult{}
	}
	defer stdin.Close()
	stdout, err := os.OpenFile(filepath.Join(jobDir, "stream.jsonl"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintln(stderrLog, "run-wrapper:", err)
		return 127, false, StreamResult{}
	}
	defer stdout.Close()
	env, err := GitEnv(jobDir, os.Environ())
	if err != nil {
		fmt.Fprintln(stderrLog, "run-wrapper: git environment:", err)
		return 127, false, StreamResult{}
	}
	env, err = JobEnv(jobDir, env)
	if err != nil {
		fmt.Fprintln(stderrLog, "run-wrapper: job marker:", err)
		return 127, false, StreamResult{}
	}
	// Created by prepare; again here for an attempt prepared by an older build.
	tmp, err := ensureJobTmp(jobDir)
	if err != nil {
		fmt.Fprintln(stderrLog, "run-wrapper: job temp dir:", err)
		return 127, false, StreamResult{}
	}
	env = TempEnv(env, tmp)
	if runner != store.RunnerCodex {
		env = ForkSubagentEnv(env)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = filepath.Join(jobDir, "work")
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderrLog
	tee := &resultTee{w: stdout, runner: runner}
	tee.watch.active = time.Now()
	if len(observe) > 0 {
		tee.observe = observe[0]
	}
	if runner == store.RunnerCodex {
		tee.res.Runner = runner
	}
	cmd.Stdout, cmd.WaitDelay = tee, teeWaitDelay
	closed, err := runIdleWatched(cmd, tee, idle, stderrLog)
	tee.end()
	res := tee.res
	if errors.Is(err, exec.ErrWaitDelay) {
		fmt.Fprintf(stderrLog, "run-wrapper: runner stdout still open %s after exit; stopped reading\n", teeWaitDelay)
		err = nil // runner exited 0; sweep handles descendants holding the pipe
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return 128 + int(ws.Signal()), closed, res
			}
			return ee.ExitCode(), closed, res
		}
		fmt.Fprintln(stderrLog, "run-wrapper: run claude:", err)
		return 127, closed, res
	}
	return 0, closed, res
}

// JobTmpDir is the job's own temp dir inside its folder (<jobDir>/tmp): what the job's tools leave in
// the temp dir (go build work dirs of a killed `go test`) is pruned with the folder.
const JobTmpDir = "tmp"

// tempEnvKeys are the variables tools read for the temp dir (TempEnv sets them all).
var tempEnvKeys = []string{"TMPDIR", "GOTMPDIR", "TMP", "TEMP"}

// TempEnv returns env without any inherited tempEnvKeys, then each of them set to dir.
func TempEnv(env []string, dir string) []string {
	out := make([]string, 0, len(env)+len(tempEnvKeys))
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); !slices.Contains(tempEnvKeys, k) {
			out = append(out, kv)
		}
	}
	for _, k := range tempEnvKeys {
		out = append(out, k+"="+dir)
	}
	return out
}

// ensureJobTmp creates the job's temp dir (0700) if it is missing and returns its absolute path. The
// job can write its folder: anything there but a real directory (a symlink) is refused.
func ensureJobTmp(jobDir string) (string, error) {
	abs, err := filepath.Abs(jobDir)
	if err != nil {
		return "", err
	}
	p := filepath.Join(abs, JobTmpDir)
	if err := os.Mkdir(p, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a real directory (%s)", p, fi.Mode().Type())
	}
	return p, nil
}

// ForkSubagentEnvVar switches Claude Code's fork subagents (on by default in `claude -p` since the
// 2026-10-16 release); a claude job always runs with it set to 0 (owner decision 2026-10-18).
const ForkSubagentEnvVar = "CLAUDE_CODE_FORK_SUBAGENT"

// ForkSubagentEnv returns env with every inherited ForkSubagentEnvVar dropped and
// ForkSubagentEnvVar=0 appended, so claude starts no fork subagents. Ordinary subagents are unaffected.
func ForkSubagentEnv(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k != ForkSubagentEnvVar {
			out = append(out, kv)
		}
	}
	return append(out, ForkSubagentEnvVar+"=0")
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
