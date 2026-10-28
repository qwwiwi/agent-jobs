package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Prepare step (issue #120): config prepare names, per repository, one shell command the wrapper runs
// in a new job's worktree before its runner (claude or codex) starts, e.g. `pnpm install
// --frozen-lockfile`, so a job whose runner has no network (the codex sandbox) still finds its
// dependencies. The daemon decides at the first launch of a job with a repository whose path matches
// an entry (the first matching entry wins) and passes --prepare to the wrapper; a resumed attempt keeps
// the prepared worktree and runs no prepare step, unless the previous attempt's prepare step did not
// pass (it failed, was stopped or its wrapper died in it: its runner never started, so the worktree is
// still the base commit's). The wrapper runs the command the way it runs
// acceptance_cmd: inside the job's own containment (its scope or session), in a process group of its
// own (recorded in the job row like the acceptance command's, so cancel, stop and the timeout end it
// even without a scope), without the guard and without a sandbox, with network; unlike acceptance_cmd it keeps the
// environment the runner gets (the real HOME, so package managers find their stores, caches and
// registry config), with the job's git environment, temp dir and marker. stdout and stderr go
// (tail-capped) to <jobdir>/prepare.log. A non-zero exit, a timeout or a step that cannot run fails the
// job before its runner starts ("prepare failed (...)"); the result goes into the wrapper's trusted
// result record, so job_status and job_result show it. The time it takes counts against the job's
// timeout_minutes. The command is the owner's (config.json, never the job's), and it runs before the
// job does anything: the repository's own scripts it starts (install hooks) run as the agent user.

// Prepare limits.
const (
	MaxPrepareCmdBytes           = 1 << 10
	DefaultPrepareTimeoutMinutes = 10
	CeilingPrepareTimeoutMinutes = 60
	PrepareLog                   = "prepare.log"
	prepareReasonPrefix          = "prepare "
	// prepareUnfinished is the Failure of the snapshot recorded before the command runs.
	prepareUnfinished = prepareReasonPrefix + "did not finish (the wrapper ended while it ran)"
)

// PrepareStep is one entry of config prepare.
type PrepareStep struct {
	// RepoGlob selects the repositories (filepath.Match): a pattern with a "/" is matched against the
	// job's repository path (its git top level), one without against the path's last element.
	RepoGlob string `json:"repo_glob"`
	// Dir is where the command runs, relative to the worktree ("" or "." = its top).
	Dir string `json:"dir"`
	// Command is one line run with /bin/sh -c.
	Command string `json:"command"`
	// TimeoutMinutes is the command's time limit: 0 = DefaultPrepareTimeoutMinutes, at most
	// CeilingPrepareTimeoutMinutes.
	TimeoutMinutes int `json:"timeout_minutes"`
}

// Timeout is the step's time limit.
func (p PrepareStep) Timeout() time.Duration {
	if p.TimeoutMinutes > 0 {
		return time.Duration(p.TimeoutMinutes) * time.Minute
	}
	return DefaultPrepareTimeoutMinutes * time.Minute
}

// Matches reports whether the step applies to the repository at repo.
func (p PrepareStep) Matches(repo string) bool {
	if repo == "" {
		return false
	}
	repo = filepath.Clean(repo)
	name := filepath.Base(repo)
	if strings.Contains(p.RepoGlob, "/") {
		name = repo
	}
	ok, err := filepath.Match(p.RepoGlob, name)
	return err == nil && ok
}

// PrepareFor returns the first prepare step whose repo_glob matches repo (nil = none).
func (c Config) PrepareFor(repo string) *PrepareStep {
	for i := range c.Prepare {
		if c.Prepare[i].Matches(repo) {
			s := c.Prepare[i]
			return &s
		}
	}
	return nil
}

func validatePrepare(steps []PrepareStep) error {
	for i, s := range steps {
		at := fmt.Sprintf("prepare[%d]", i)
		if strings.TrimSpace(s.RepoGlob) == "" {
			return fmt.Errorf("%s: repo_glob is required", at)
		}
		if _, err := filepath.Match(s.RepoGlob, ""); err != nil {
			return fmt.Errorf("%s: repo_glob %q: %w", at, s.RepoGlob, err)
		}
		if strings.ContainsAny(s.Command, "\n\r\x00") {
			return fmt.Errorf("%s: command must be one line (no newline, carriage return or NUL)", at)
		}
		if strings.TrimSpace(s.Command) == "" {
			return fmt.Errorf("%s: command is required", at)
		}
		if len(s.Command) > MaxPrepareCmdBytes || !utf8.ValidString(s.Command) {
			return fmt.Errorf("%s: command must be valid UTF-8 of at most %d bytes", at, MaxPrepareCmdBytes)
		}
		if s.Dir != "" {
			d := filepath.Clean(s.Dir)
			if filepath.IsAbs(s.Dir) || d == ".." || strings.HasPrefix(d, ".."+string(filepath.Separator)) ||
				strings.ContainsRune(s.Dir, 0) {
				return fmt.Errorf("%s: dir %q must be a path inside the worktree (relative, no ..)", at, s.Dir)
			}
		}
		if s.TimeoutMinutes < 0 || s.TimeoutMinutes > CeilingPrepareTimeoutMinutes {
			return fmt.Errorf("%s: timeout_minutes %d must be 0 (default %d) or 1..%d", at, s.TimeoutMinutes,
				DefaultPrepareTimeoutMinutes, CeilingPrepareTimeoutMinutes)
		}
	}
	return nil
}

// PrepareResult is how a job's prepare step ended, as its wrapper recorded it.
type PrepareResult struct {
	Command string `json:"command"`
	Dir     string `json:"dir,omitempty"`
	// Ran is false when the step did not get to run its command, or did not see it end (the reason is
	// in Failure).
	Ran bool `json:"ran"`
	// Exit is the exit code (128+signal when killed by a signal); -1 with TimedOut.
	Exit        int    `json:"exit"`
	TimedOut    bool   `json:"timed_out,omitempty"`
	DurationSec int    `json:"duration_sec"`
	Failure     string `json:"failure,omitempty"`
}

// prepareNote is the done-line note of a job its prepare step failed ("" otherwise): its error up to
// the first detail, e.g. "prepare failed (exit 4 after 2s)".
func prepareNote(j *store.Job) string {
	if j.Status != store.StatusFailed || !(strings.HasPrefix(j.Error, prepareReasonPrefix+"failed (") ||
		strings.HasPrefix(j.Error, prepareReasonPrefix+"did not run")) {
		return ""
	}
	note := RedactText(j.Error) // before any cut: a value cut short no longer matches its format
	if i := strings.IndexAny(note, ";:"); i > 0 {
		note = note[:i]
	}
	return RedactPrintable(strings.TrimSpace(note), 80)
}

// prepareFailure is the failure reason of a prepare result ("" = passed).
func prepareFailure(r PrepareResult) string {
	switch {
	case r.Failure != "":
		return r.Failure
	case r.TimedOut:
		return fmt.Sprintf("%sfailed (timeout after %ds); see %s", prepareReasonPrefix, r.DurationSec, PrepareLog)
	case r.Exit != 0:
		return fmt.Sprintf("%sfailed (exit %d after %ds); see %s", prepareReasonPrefix, r.Exit, r.DurationSec, PrepareLog)
	}
	return ""
}

// runPrepareStep is the wrapper's prepare step (the daemon launches the wrapper with --prepare). It
// returns the result to record (nil = no step applies any more: config.json changed since the launch)
// and a non-zero rc when a stop signal ended the run (the wrapper exits with it and does not start the
// runner). A failed step is a result with prepareFailure set; the caller ends the job with it.
// onStart gets the result before the command runs (Failure prepareUnfinished), for the record.
func runPrepareStep(jobDir, token string, st *store.Store, id string, log io.Writer,
	onStart func(PrepareResult)) (*PrepareResult, int) {
	ctx, release := catchStopSignals()
	res, signalled := prepareStep(ctx, jobDir, token, st, id, log, onStart)
	if release() || signalled {
		fmt.Fprintln(log, "run-wrapper: stopped by a signal during the prepare step; the runner is not started")
		if res != nil {
			res.Ran, res.Failure = false, prepareReasonPrefix+"stopped by a signal"
		}
		return res, 128 + int(syscall.SIGTERM)
	}
	return res, 0
}

func prepareStep(ctx context.Context, jobDir, token string, st *store.Store, id string, log io.Writer,
	onStart func(PrepareResult)) (*PrepareResult, bool) {
	fail := func(r *PrepareResult, format string, a ...any) (*PrepareResult, bool) {
		r.Failure = prepareReasonPrefix + "did not run: " + RedactPrintable(fmt.Sprintf(format, a...), 300)
		return r, false
	}
	j, err := st.Get(id)
	if err != nil {
		return fail(&PrepareResult{}, "job row unreadable: %v", err)
	}
	abs, err := filepath.Abs(jobDir)
	if err != nil {
		return fail(&PrepareResult{}, "%v", err)
	}
	c, err := LoadConfig(filepath.Dir(filepath.Dir(abs)))
	if err != nil {
		return fail(&PrepareResult{}, "config: %v", err)
	}
	step := c.PrepareFor(j.RepoPath)
	if step == nil {
		fmt.Fprintln(log, "run-wrapper: prepare: no prepare step matches the repository any more; skipped")
		return nil, false
	}
	r := &PrepareResult{Command: step.Command, Dir: step.Dir}
	work := filepath.Join(abs, "work")
	dir, err := prepareDir(work, step.Dir)
	if err != nil {
		return fail(r, "%v", err)
	}
	env, err := GitEnv(jobDir, os.Environ())
	if err == nil {
		env, err = JobEnv(jobDir, env)
	}
	var tmp string
	if err == nil {
		tmp, err = ensureJobTmp(jobDir)
	}
	if err != nil {
		return fail(r, "environment: %v", err)
	}
	env = TempEnv(env, tmp)
	if onStart != nil {
		started := *r
		started.Failure = prepareUnfinished
		onStart(started)
	}
	fmt.Fprintf(log, "run-wrapper: prepare: %s (in %s, timeout %s)\n", RedactPrintable(step.Command, 300), dir, step.Timeout())
	start := time.Now()
	out := &tailBuffer{max: AcceptanceLogBytes}
	recordGroup := func(pgid int) {
		if err := st.SetAcceptancePGID(id, token, pgid); err != nil {
			fmt.Fprintln(log, "run-wrapper: prepare: record process group:", err)
		}
	}
	exit := runShellGroup(ctx, dir, env, step.Command, step.Timeout(), out, log, recordGroup)
	dur := time.Since(start)
	body, dropped := keptTail(out)
	if dropped > 0 {
		body = append([]byte(fmt.Sprintf("[... %d earlier bytes cut, last %d kept ...]\n", dropped, len(body))), body...)
	}
	if err := writeAtomic(filepath.Join(abs, PrepareLog), body); err != nil {
		fmt.Fprintln(log, "run-wrapper: prepare: write log:", err)
	}
	fmt.Fprintf(log, "run-wrapper: prepare exit %d after %s\n", exit, dur.Round(time.Second))
	if exit == acceptanceSignalled {
		return r, true
	}
	r.Ran, r.Exit, r.TimedOut = true, exit, exit == store.AcceptanceTimedOut
	r.DurationSec = int(dur.Round(time.Second).Seconds())
	// What the command left running (a package manager's daemon) does not outlive the step: the
	// runner has not started, so every other process of the session is the step's.
	sw := sweepJob(jobDir, log, true)
	if sw.Signalled > 0 {
		fmt.Fprintf(log, "run-wrapper: prepare: ended %d leftover process(es)\n", sw.Signalled)
	}
	if sw.Incomplete() {
		if err := writeSweepMarker(jobDir, sw); err != nil {
			fmt.Fprintln(log, "run-wrapper: write sweep marker:", err)
		}
		r.Failure = prepareReasonPrefix + "failed: " + sweepResultFailure(sw)
		return r, false
	}
	recordGroup(0)
	return r, false
}

// prepareDir is the directory the step runs in: rel below work, an existing directory that, with its
// links resolved, is still inside the worktree.
func prepareDir(work, rel string) (string, error) {
	dir := filepath.Join(work, filepath.Clean("/" + rel)[1:])
	realWork, err := filepath.EvalSymlinks(work)
	if err != nil {
		return "", fmt.Errorf("worktree: %w", err)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("dir %q: %w", rel, err)
	}
	if real != realWork && !strings.HasPrefix(real, realWork+string(filepath.Separator)) {
		return "", fmt.Errorf("dir %q leads out of the worktree", rel)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("dir %q: %w", rel, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("dir %q is not a directory", rel)
	}
	return dir, nil
}

// PrepareResultOf is the latest prepare step of job j as its wrappers recorded it: the current
// launch's, else the newest archived attempt's that ran one. Nil = no step ran.
func (m *Manager) PrepareResultOf(j *store.Job) *PrepareResult {
	if raw, ok, err := m.Store.WrapperResult(j.ID, j.LaunchToken); err == nil && ok {
		if r := prepareOfRecord(raw); r != nil {
			return r
		}
	}
	for n := j.Attempt; n >= 1; n-- {
		if raw, err := m.Store.AttemptWrapperResult(j.ID, n); err == nil {
			if r := prepareOfRecord(raw); r != nil {
				return r
			}
		}
	}
	return nil
}

// prepareAgain reports whether a resumed attempt of j runs the prepare step again: the previous
// attempt ran one that did not pass (failed, stopped, or its wrapper died in it). Its runner never
// started then, so the worktree holds nothing of the job's own.
func (m *Manager) prepareAgain(j *store.Job) bool {
	raw, err := m.Store.AttemptWrapperResult(j.ID, j.Attempt-1)
	if err != nil {
		return false
	}
	r := prepareOfRecord(raw)
	return r != nil && prepareFailure(*r) != ""
}

func prepareOfRecord(raw string) *PrepareResult {
	var res StreamResult
	if raw == "" || json.Unmarshal([]byte(raw), &res) != nil {
		return nil
	}
	return res.Prepare
}
