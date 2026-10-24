package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Acceptance command (Wave D): an optional shell command of the orchestrator's that a job
// must pass. After claude exits 0 with a report and the session sweep is complete, the wrapper runs it
// in the job's worktree, inside the job's own containment (the same scope or session; a process group
// of its own so a timeout ends all of it, recorded in the job row so cancel, stop and the daemon's
// timeout end it too), with a scrubbed environment (HOME a throwaway dir in the job dir) and without
// the guard. Exit 0 keeps the job's verdict; anything else fails the job with "acceptance failed
// (...)". The exit code and duration go into the job row (written by the wrapper after the command and
// its leftovers are gone), the output tail into acceptance.log.
//
// Off unless config acceptance_allowed is true: the command line is the orchestrator's, but what it
// runs (tests, build scripts) is the code the job wrote, outside the guard, as the agent user. The
// scrubbed environment keeps the wrapper's tokens and the real HOME out of reach of variables only; the
// file system is not sandboxed. Not a security boundary either way: a job can change the test it is
// checked with. It catches honest mistakes, not a hostile job.

// Acceptance limits.
const (
	MaxAcceptanceCmdBytes           = 1 << 10
	DefaultAcceptanceTimeoutMinutes = 15
	CeilingAcceptanceTimeoutMinutes = 60
	AcceptanceLogBytes              = 64 << 10
	AcceptanceLog                   = "acceptance.log"
	// acceptanceReadCap bounds a read of acceptance.log: the kept output and its header line.
	acceptanceReadCap       = AcceptanceLogBytes + 1<<10
	acceptanceKillGrace     = 5 * time.Second
	acceptanceDeadlineGrace = 2 * time.Minute // the wrapper's kill, sweep and bookkeeping after the timeout
	acceptanceReasonPrefix  = "acceptance "
	acceptanceHome          = "acceptance-home" // HOME (and TMPDIR under it) of the acceptance command
	// acceptanceSignalled is runAcceptanceCmd's result when the wrapper was told to stop (SIGTERM,
	// SIGINT, SIGHUP) while the command ran: the group is killed and nothing is recorded.
	acceptanceSignalled = -2
)

// AcceptanceRefusedKinds are the kinds that change no code; acceptance_cmd is refused for them.
var AcceptanceRefusedKinds = []string{"review", "research", KindSlicing, KindJudge}

// ValidateAcceptanceCmd checks an acceptance command for a job of kind and returns it trimmed ("" =
// none): one line (no newline, carriage return or NUL anywhere in the input, also not at either end),
// at most MaxAcceptanceCmdBytes, valid UTF-8, never for a review or research job, and only when
// allowed (config acceptance_allowed).
func ValidateAcceptanceCmd(kind, cmd string, allowed bool) (string, error) {
	// Checked before trimming: "make test\n" or "\rmake test" must not turn into a valid command, nor a
	// newline-only input into no command at all.
	if strings.ContainsAny(cmd, "\n\r\x00") {
		return "", errors.New("acceptance_cmd must be one line (no newline, carriage return or NUL)")
	}
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return "", nil
	}
	if !allowed {
		return "", errors.New("acceptance_cmd is off: set acceptance_allowed: true in config.json to allow it (the " +
			"command runs the job's code outside the guard)")
	}
	if slices.Contains(AcceptanceRefusedKinds, kind) {
		return "", fmt.Errorf("acceptance_cmd does not apply to a %s job: it changes no code, so there is nothing "+
			"to accept (acceptance_cmd is for fix and general jobs)", kind)
	}
	if len(cmd) > MaxAcceptanceCmdBytes {
		return "", fmt.Errorf("acceptance_cmd has %d bytes, at most %d", len(cmd), MaxAcceptanceCmdBytes)
	}
	if !utf8.ValidString(cmd) {
		return "", errors.New("acceptance_cmd is not valid UTF-8")
	}
	return cmd, nil
}

// AcceptanceTimeout is the acceptance command's time limit (acceptance_timeout_minutes, default 15).
func (c Config) AcceptanceTimeout() time.Duration {
	if c.AcceptanceTimeoutMinutes > 0 {
		return time.Duration(c.AcceptanceTimeoutMinutes) * time.Minute
	}
	return DefaultAcceptanceTimeoutMinutes * time.Minute
}

func (c Config) validateAcceptance() error {
	if c.AcceptanceTimeoutMinutes < 0 || c.AcceptanceTimeoutMinutes > CeilingAcceptanceTimeoutMinutes {
		return fmt.Errorf("config.json: acceptance_timeout_minutes %d must be 0 (default %d) or 1..%d",
			c.AcceptanceTimeoutMinutes, DefaultAcceptanceTimeoutMinutes, CeilingAcceptanceTimeoutMinutes)
	}
	return nil
}

// jobDeadline is when the daemon times a running job out: its timeout from the start, or, once its
// acceptance command started, the later acceptance deadline the wrapper recorded with the start (from
// the acceptance timeout the wrapper applies, so a config changed while the daemon runs cannot cut it
// short). It only preselects: the decision is store.ClaimTimeout, made against the row as it is then.
func jobDeadline(j *store.Job) time.Time {
	d := j.StartedAt.Add(time.Duration(j.TimeoutMinutes) * time.Minute)
	if j.AcceptanceDeadline != nil && j.AcceptanceDeadline.After(d) {
		d = *j.AcceptanceDeadline
	}
	return d
}

// acceptanceDeadline is the deadline the wrapper records when it starts an acceptance command at now.
func acceptanceDeadline(now time.Time, timeout time.Duration) time.Time {
	return now.Add(timeout + acceptanceDeadlineGrace)
}

// AcceptanceFailure is the failure reason of a job whose acceptance command did not pass ("" = passed
// or none set): it did not run, timed out, or exited non-zero.
func AcceptanceFailure(j *store.Job) string {
	switch {
	case j.AcceptanceCmd == "":
		return ""
	case j.AcceptanceExit == nil:
		return acceptanceReasonPrefix + "did not run (see stderr.log)"
	case *j.AcceptanceExit == store.AcceptanceTimedOut:
		return fmt.Sprintf("%sfailed (timeout after %ds)", acceptanceReasonPrefix, j.AcceptanceDurationSec)
	case *j.AcceptanceExit != 0:
		return fmt.Sprintf("%sfailed (exit %d)", acceptanceReasonPrefix, *j.AcceptanceExit)
	}
	return ""
}

// IsAcceptanceFailure reports whether a job's error text is an acceptance failure.
func IsAcceptanceFailure(text string) bool { return strings.HasPrefix(text, acceptanceReasonPrefix) }

// acceptanceNote is the done-line note of a job whose acceptance failed ("" otherwise): the recorded
// exit, or, without one, the acceptance failure recorded as the job's error (did not run, result
// unreadable) up to its first detail.
func acceptanceNote(j *store.Job) string {
	if j.AcceptanceCmd == "" {
		return ""
	}
	if e := j.AcceptanceExit; e != nil && *e != 0 {
		if *e == store.AcceptanceTimedOut {
			return "acceptance failed (timeout)"
		}
		return fmt.Sprintf("acceptance failed (exit %d)", *e)
	}
	if !IsAcceptanceFailure(j.Error) {
		return ""
	}
	note := RedactText(j.Error) // before any cut: a value cut short no longer matches its format
	if i := strings.IndexAny(note, "(:;"); i > 0 {
		note = note[:i]
	}
	return RedactPrintable(strings.TrimSpace(note), 80)
}

// verdict is Verdict, then a succeeded verdict turns into a failure when a gate job's result was not
// stored (gateVerdict) or the job's acceptance command did not pass. The row is read again: the wrapper records the result right before exit_code, possibly
// after this tick listed the running jobs.
func (m *Manager) verdict(j *store.Job, code int, res StreamResult, dir string) (string, string) {
	status, msg := Verdict(code, res, dir)
	status, msg = m.gateVerdict(j, status, msg, res)
	if status != store.StatusSucceeded || j.AcceptanceCmd == "" {
		return m.completedReportContract(j, status, msg, res, dir)
	}
	fresh, err := m.Store.Get(j.ID)
	if err != nil {
		return store.StatusFailed, acceptanceReasonPrefix + "result unreadable: " + Printable(err.Error(), 200)
	}
	if reason := AcceptanceFailure(fresh); reason != "" {
		return store.StatusFailed, reason
	}
	return m.completedReportContract(j, status, msg, res, dir)
}

// completedReportContract uses the report validation captured before trusted completion.
func (m *Manager) completedReportContract(j *store.Job, status, msg string, res StreamResult, dir string) (string, string) {
	if !res.Completed {
		return m.reportContract(j, status, msg, dir)
	}
	return m.applyReportContract(j, status, msg, res.ReportContractNote)
}

// reportContract checks the report of a succeeded claude job against the report contract: one
// without a "## Summary" or "## Not done / uncertain" section keeps its status with the note added to
// its message (report_contract warn, the default) or fails with it (fail).
func (m *Manager) reportContract(j *store.Job, status, msg, dir string) (string, string) {
	if status != store.StatusSucceeded || j.Runner == store.RunnerCodex {
		return status, msg
	}
	report, err := readSmall(filepath.Join(dir, "report.md"), maxContractReportBytes+1)
	if err != nil || len(report) > maxContractReportBytes {
		return status, msg // Verdict saw a report; one unreadable now, or longer than the limit, is not judged here
	}
	return m.applyReportContract(j, status, msg, reportContractNote(report, j.Covers))
}

func (m *Manager) applyReportContract(j *store.Job, status, msg, note string) (string, string) {
	if status != store.StatusSucceeded || j.Runner == store.RunnerCodex {
		return status, msg
	}
	switch {
	case note == "":
		return status, msg
	case m.Config().ReportContract == ReportContractFail:
		return store.StatusFailed, note
	case msg != "":
		return status, msg + "; " + note
	}
	return status, note
}

// wrapperAcceptance is what the wrapper needs to run a job's acceptance command.
type wrapperAcceptance struct {
	cmd     string
	timeout time.Duration
	st      *store.Store
	id      string
}

// openAcceptance reads the job's acceptance command from <state>/jobs.db (jobDir = <state>/jobs/<id>)
// and the acceptance timeout from config.json. Permission requires both the launching daemon's
// authorization and the current disk opt-in. The returned store stays open for the result; close it.
func openAcceptance(jobDir, token string) (*wrapperAcceptance, error) {
	abs, err := filepath.Abs(jobDir)
	if err != nil {
		return nil, err
	}
	state := filepath.Dir(filepath.Dir(abs))
	st, err := store.Open(filepath.Join(state, "jobs.db"))
	if err != nil {
		return nil, err
	}
	j, err := st.Get(filepath.Base(abs))
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	a := &wrapperAcceptance{cmd: j.AcceptanceCmd, st: st, id: j.ID}
	if a.cmd == "" {
		return a, nil
	}
	auth, authErr := st.ReadLaunchAuthorization(j.ID, token)
	if authErr != nil || !auth.AcceptanceAllowed {
		_ = st.Close()
		return nil, errors.New("acceptance_allowed was not authorized by the launching daemon; the acceptance command is not run")
	}
	c, err := LoadConfig(state)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("config (acceptance timeout): %w", err)
	}
	if !c.AcceptanceAllowed {
		// Turned off after the job was queued: refuse (the job then fails as an acceptance that did not run).
		_ = st.Close()
		return nil, errors.New("acceptance_allowed is off in config.json; the acceptance command is not run")
	}
	a.timeout = c.AcceptanceTimeout()
	return a, nil
}

// runAcceptanceStep runs the job's acceptance command, if it has one, then sweeps the session again and
// records the result in the job row. It returns non-zero only when the wrapper must stop without
// exit_code (a sweep marker could not be written). Any other failure leaves the result unrecorded,
// which the daemon takes as an acceptance that did not run (fail closed).
//
// SIGTERM, SIGINT or SIGHUP while it runs (cancel, stop, the daemon's timeout) end the command's process
// group (TERM, then KILL after acceptanceKillGrace) and the wrapper returns 128+SIGTERM without a
// result or exit_code: the daemon is ending the job anyway.
func runAcceptanceStep(jobDir, token string, log *os.File, result ...*StreamResult) int {
	a, err := openAcceptance(jobDir, token)
	if err != nil {
		fmt.Fprintln(log, "run-wrapper: acceptance:", err)
		return 0
	}
	defer a.st.Close()
	if a.cmd == "" {
		return 0
	}
	// From here on a stop signal must not kill the wrapper before the command's group is gone.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	now := time.Now()
	if err := a.st.SetAcceptanceStarted(a.id, token, now, acceptanceDeadline(now, a.timeout)); err != nil {
		// ErrImmutable: the daemon already decided to time the job out (or it no longer runs).
		fmt.Fprintln(log, "run-wrapper: acceptance: record start (not run):", err)
		return 0
	}
	onStart := func(pgid int) {
		if err := a.st.SetAcceptancePGID(a.id, token, pgid); err != nil {
			fmt.Fprintln(log, "run-wrapper: acceptance: record process group:", err)
		}
	}
	exit, dur := runAcceptance(ctx, jobDir, a.cmd, a.timeout, log, onStart)
	if exit == acceptanceSignalled {
		fmt.Fprintln(log, "run-wrapper: stopped by a signal during the acceptance command; its process group is ended")
		return 128 + int(syscall.SIGTERM)
	}
	sw := sweepJob(jobDir, log)
	if sw.Signalled > 0 {
		fmt.Fprintf(log, "run-wrapper: ended %d leftover process(es) of the acceptance command\n", sw.Signalled)
	}
	if sw.Incomplete() {
		if len(result) > 0 {
			result[0].SweepFailure = sweepResultFailure(sw)
		}
		fmt.Fprintf(log, "run-wrapper: sweep after acceptance incomplete: survivors %v, error: %v\n", sw.Survivors, sw.Err)
		if err := writeSweepMarker(jobDir, sw); err != nil {
			fmt.Fprintln(log, "run-wrapper: write sweep marker:", err)
			return 2
		}
	}
	if err := a.st.SetAcceptanceResult(a.id, token, exit, int(dur.Round(time.Second).Seconds())); err != nil {
		fmt.Fprintln(log, "run-wrapper: acceptance: record result:", err)
	}
	return 0
}

// runAcceptance runs cmd with /bin/sh -c in <jobDir>/work, in a process group of its own (onStart,
// when set, gets its id), stdin /dev/null, environment acceptanceEnv; stdout and stderr go (tail-capped)
// to acceptance.log. On the timeout or when ctx ends the group gets SIGTERM, then SIGKILL after
// acceptanceKillGrace. It returns the exit code (128+signal when killed by a signal,
// store.AcceptanceTimedOut on the timeout, acceptanceSignalled when ctx ended) and the duration.
func runAcceptance(ctx context.Context, jobDir, cmdline string, timeout time.Duration, log io.Writer,
	onStart func(pgid int)) (int, time.Duration) {
	start := time.Now()
	out := &tailBuffer{max: AcceptanceLogBytes}
	exit := runAcceptanceCmd(ctx, jobDir, cmdline, timeout, out, log, onStart)
	dur := time.Since(start)
	body, dropped := out.Bytes(), out.dropped
	if dropped > 0 {
		// The first kept line was cut at its start: drop it whole, so a value split by the cut leaves
		// no part of itself (too short to match its format) in the log.
		cut := bytes.IndexByte(body, '\n') + 1
		if cut == 0 {
			cut = len(body)
		}
		// The dropped line completes the evicted text's last line: was a key block open at the cut
		// (its BEGIN line evicted, its END line not), what is kept of it goes until its END line.
		out.evicted.feed(body[:cut])
		body, dropped = body[cut:], dropped+int64(cut)
		if kept, n := redactPEM(string(body), out.evicted.open); n > 0 {
			body = []byte(kept)
		}
		body = append([]byte(fmt.Sprintf("[... %d earlier bytes cut, last %d kept ...]\n", dropped, len(body))), body...)
	}
	if err := writeAtomic(filepath.Join(jobDir, AcceptanceLog), body); err != nil {
		fmt.Fprintln(log, "run-wrapper: acceptance: write log:", err)
	}
	fmt.Fprintf(log, "run-wrapper: acceptance exit %d after %s\n", exit, dur.Round(time.Second))
	return exit, dur
}

func runAcceptanceCmd(parent context.Context, jobDir, cmdline string, timeout time.Duration, out io.Writer, log io.Writer,
	onStart func(pgid int)) int {
	env, err := acceptanceEnv(jobDir, os.Environ())
	if err != nil {
		fmt.Fprintln(log, "run-wrapper: acceptance environment:", err)
		return 127
	}
	if parent.Err() != nil {
		return acceptanceSignalled
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", cmdline)
	cmd.Dir = filepath.Join(jobDir, "work")
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	// After the grace Go kills the leader and stops waiting for pipes a straggler still holds.
	cmd.WaitDelay = acceptanceKillGrace
	if err = cmd.Start(); err == nil {
		if onStart != nil {
			onStart(cmd.Process.Pid) // Setpgid: the leader's pid is the group id
		}
		err = cmd.Wait()
	}
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // nothing of the group outlives it
	}
	if parent.Err() != nil {
		return acceptanceSignalled
	}
	if ctx.Err() != nil {
		return store.AcceptanceTimedOut
	}
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	fmt.Fprintln(log, "run-wrapper: acceptance: start:", err)
	return 127
}

// acceptanceEnvKeys are the variables an acceptance command inherits (plus LC_*); everything else of
// the wrapper's environment (tokens, API keys, CLAUDE_*, XDG_*, the user bus, the real HOME and
// TMPDIR) is dropped.
var acceptanceEnvKeys = []string{"PATH", "USER", "LOGNAME", "SHELL", "LANG", "TZ", "TERM"}

// acceptanceEnv is the acceptance command's environment: the allow-listed variables of env, HOME a
// fresh throwaway dir <jobDir>/acceptance-home (its tmp/ as the temp dir, TempEnv), so no tool finds the agent's
// dotfiles, credentials or state dir through HOME; then the job's git environment (GitEnv) and the job
// marker (JobEnv), so it starts no nested job and doctor procs can find what it leaves.
func acceptanceEnv(jobDir string, env []string) ([]string, error) {
	abs, err := filepath.Abs(jobDir)
	if err != nil {
		return nil, err
	}
	home := filepath.Join(abs, acceptanceHome)
	// Fresh for every run: nothing a previous run or the job planted there (an rc file) is picked up.
	if err := os.RemoveAll(home); err != nil {
		return nil, fmt.Errorf("reset %s: %w", acceptanceHome, err)
	}
	if err := os.MkdirAll(filepath.Join(home, "tmp"), 0o700); err != nil {
		return nil, err
	}
	keep := []string{"HOME=" + home}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains(acceptanceEnvKeys, k) || strings.HasPrefix(k, "LC_") {
			keep = append(keep, kv)
		}
	}
	keep, err = GitEnv(jobDir, TempEnv(keep, filepath.Join(home, "tmp")))
	if err != nil {
		return nil, err
	}
	return JobEnv(jobDir, keep)
}

// tailBuffer keeps the last max bytes written to it; evicted follows the PEM key block markers of
// what it dropped.
type tailBuffer struct {
	max     int
	buf     []byte
	dropped int64
	evicted pemTracker
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= t.max {
		t.evicted.feed(t.buf)
		t.evicted.feed(p[:len(p)-t.max])
		t.dropped += int64(len(t.buf) + len(p) - t.max)
		t.buf = append(t.buf[:0], p[len(p)-t.max:]...)
		return n, nil
	}
	if over := len(t.buf) + len(p) - t.max; over > 0 {
		t.evicted.feed(t.buf[:over])
		t.dropped += int64(over)
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

// Bytes returns the kept tail.
func (t *tailBuffer) Bytes() []byte { return append([]byte(nil), t.buf...) }

// ReadAcceptanceLogTail returns the end of <stateDir>/jobs/<id>/acceptance.log, at most max bytes, as
// valid UTF-8 with secret-looking values redacted (like report.md: at most acceptanceReadCap bytes
// are read, and the text is redacted before it is cut to max; a line cut at the start of the read or
// of the tail is dropped whole, so no part of a value split by a cut is shown; what a cut, this one
// or an earlier build's, left of a PEM key block is redacted on every read, see redactPEM). The job dir is writable by the job, so the log is opened like report.md (OpenJobFile: never
// following a symlinked jobs or job dir or log, and only a regular file of this user with one link).
func ReadAcceptanceLogTail(stateDir, id string, max int64) (string, error) {
	f, size, err := OpenJobFile(stateDir, id, AcceptanceLog, os.Getuid())
	if err != nil {
		return "", err
	}
	defer f.Close()
	off := size - acceptanceReadCap
	if off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return "", err
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, acceptanceReadCap))
	if err != nil {
		return "", err
	}
	s := strings.ToValidUTF8(string(b), "�")
	if off > 0 {
		s = dropFirstLine(s)
	}
	s = RedactText(s)
	if cut := int64(len(s)) - max; cut > 0 {
		head := s[cut-1]
		s = s[cut:]
		if head != '\n' {
			s = dropFirstLine(s)
		}
		s, _ = redactPEM(s, false)
	}
	return s, nil
}

// dropFirstLine drops the first line of s, cut at its start, whole: everything up to and including
// its first line break, all of s when it has none.
func dropFirstLine(s string) string {
	i := strings.IndexByte(s, '\n')
	if i < 0 {
		return ""
	}
	return s[i+1:]
}
