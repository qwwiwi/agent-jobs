package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Acceptance command (Wave D autopilot): an optional shell command of the orchestrator's that a job
// must pass. After claude exits 0 with a report and the session sweep is complete, the wrapper runs it
// in the job's worktree, inside the job's own containment (the same scope or session; a process group
// of its own so a timeout ends all of it), with a scrubbed environment and without the guard (the
// command is the orchestrator's, not the model's). Exit 0 keeps the job's verdict; anything else fails
// the job with "acceptance failed (...)". The exit code and duration go into the job row (written by
// the wrapper after the command and its leftovers are gone), the output tail into acceptance.log.
//
// Not a security boundary: the command runs the code the job wrote (a job can change the test it is
// checked with). It catches honest mistakes, not a hostile job.

// Acceptance limits.
const (
	MaxAcceptanceCmdBytes           = 1 << 10
	DefaultAcceptanceTimeoutMinutes = 15
	CeilingAcceptanceTimeoutMinutes = 60
	AcceptanceLogBytes              = 64 << 10
	AcceptanceLog                   = "acceptance.log"
	acceptanceKillGrace             = 5 * time.Second
	acceptanceDeadlineGrace         = 2 * time.Minute // the wrapper's kill, sweep and bookkeeping after the timeout
	acceptanceReasonPrefix          = "acceptance "
)

// AcceptanceRefusedKinds are the kinds that change no code; acceptance_cmd is refused for them.
var AcceptanceRefusedKinds = []string{"review", "research"}

// ValidateAcceptanceCmd checks an acceptance command for a job of kind and returns it trimmed ("" =
// none): at most MaxAcceptanceCmdBytes, valid UTF-8, one line (no newline, carriage return or NUL),
// and never for a review or research job.
func ValidateAcceptanceCmd(kind, cmd string) (string, error) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return "", nil
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
	if strings.ContainsAny(cmd, "\n\r\x00") {
		return "", errors.New("acceptance_cmd must be one line (no newline, carriage return or NUL)")
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
// acceptance command started, at least the acceptance timeout (plus a grace) from then.
func (m *Manager) jobDeadline(j *store.Job) time.Time {
	d := j.StartedAt.Add(time.Duration(j.TimeoutMinutes) * time.Minute)
	if j.AcceptanceCmd != "" && j.AcceptanceStartedAt != nil {
		if a := j.AcceptanceStartedAt.Add(m.Cfg.AcceptanceTimeout() + acceptanceDeadlineGrace); a.After(d) {
			d = a
		}
	}
	return d
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

// acceptanceNote is the done-line note of a job whose acceptance failed ("" otherwise).
func acceptanceNote(j *store.Job) string {
	if j.AcceptanceExit == nil || *j.AcceptanceExit == 0 {
		return ""
	}
	if *j.AcceptanceExit == store.AcceptanceTimedOut {
		return "acceptance failed (timeout)"
	}
	return fmt.Sprintf("acceptance failed (exit %d)", *j.AcceptanceExit)
}

// verdict is Verdict, then a succeeded verdict turns into a failure when the job's acceptance command
// did not pass. The row is read again: the wrapper records the result right before exit_code, possibly
// after this tick listed the running jobs.
func (m *Manager) verdict(j *store.Job, code int, res StreamResult, dir string) (string, string) {
	status, msg := Verdict(code, res, dir)
	if status != store.StatusSucceeded || j.AcceptanceCmd == "" {
		return status, msg
	}
	fresh, err := m.Store.Get(j.ID)
	if err != nil {
		return store.StatusFailed, acceptanceReasonPrefix + "result unreadable: " + Printable(err.Error(), 200)
	}
	if reason := AcceptanceFailure(fresh); reason != "" {
		return store.StatusFailed, reason
	}
	return status, msg
}

// wrapperAcceptance is what the wrapper needs to run a job's acceptance command.
type wrapperAcceptance struct {
	cmd     string
	timeout time.Duration
	st      *store.Store
	id      string
}

// openAcceptance reads the job's acceptance command from <state>/jobs.db (jobDir = <state>/jobs/<id>)
// and the acceptance timeout from config.json. The returned store stays open for the result; close it.
func openAcceptance(jobDir string) (*wrapperAcceptance, error) {
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
	c, err := LoadConfig(state)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("config (acceptance timeout): %w", err)
	}
	a.timeout = c.AcceptanceTimeout()
	return a, nil
}

// runAcceptanceStep runs the job's acceptance command, if it has one, then sweeps the session again and
// records the result in the job row. It returns non-zero only when the wrapper must stop without
// exit_code (a sweep marker could not be written). Any other failure leaves the result unrecorded,
// which the daemon takes as an acceptance that did not run (fail closed).
func runAcceptanceStep(jobDir, token string, log *os.File) int {
	a, err := openAcceptance(jobDir)
	if err != nil {
		fmt.Fprintln(log, "run-wrapper: acceptance: job row:", err)
		return 0
	}
	defer a.st.Close()
	if a.cmd == "" {
		return 0
	}
	if err := a.st.SetAcceptanceStarted(a.id, token, time.Now()); err != nil {
		fmt.Fprintln(log, "run-wrapper: acceptance: record start:", err)
		return 0
	}
	exit, dur := runAcceptance(jobDir, a.cmd, a.timeout, log)
	sw := sweepJob(jobDir, log)
	if sw.Signalled > 0 {
		fmt.Fprintf(log, "run-wrapper: ended %d leftover process(es) of the acceptance command\n", sw.Signalled)
	}
	if sw.Incomplete() {
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

// runAcceptance runs cmd with /bin/sh -c in <jobDir>/work, in a process group of its own, stdin
// /dev/null, environment acceptanceEnv; stdout and stderr go (tail-capped) to acceptance.log. On the
// timeout the group gets SIGTERM, then SIGKILL after acceptanceKillGrace. It returns the exit code
// (128+signal when killed by a signal, store.AcceptanceTimedOut on the timeout) and the duration.
func runAcceptance(jobDir, cmdline string, timeout time.Duration, log io.Writer) (int, time.Duration) {
	start := time.Now()
	out := &tailBuffer{max: AcceptanceLogBytes}
	exit := runAcceptanceCmd(jobDir, cmdline, timeout, out, log)
	dur := time.Since(start)
	body := out.Bytes()
	if out.dropped > 0 {
		body = append([]byte(fmt.Sprintf("[... %d earlier bytes cut, last %d kept ...]\n", out.dropped, len(out.buf))), body...)
	}
	if err := writeAtomic(filepath.Join(jobDir, AcceptanceLog), body); err != nil {
		fmt.Fprintln(log, "run-wrapper: acceptance: write log:", err)
	}
	fmt.Fprintf(log, "run-wrapper: acceptance exit %d after %s\n", exit, dur.Round(time.Second))
	return exit, dur
}

func runAcceptanceCmd(jobDir, cmdline string, timeout time.Duration, out io.Writer, log io.Writer) int {
	env, err := acceptanceEnv(jobDir, os.Environ())
	if err != nil {
		fmt.Fprintln(log, "run-wrapper: acceptance environment:", err)
		return 127
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", cmdline)
	cmd.Dir = filepath.Join(jobDir, "work")
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	// After the grace Go kills the leader and stops waiting for pipes a straggler still holds.
	cmd.WaitDelay = acceptanceKillGrace
	err = cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // nothing of the group outlives it
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
// the wrapper's environment (tokens, API keys, CLAUDE_*, the user bus) is dropped.
var acceptanceEnvKeys = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "TZ", "TMPDIR", "TERM"}

// acceptanceEnv is the acceptance command's environment: the allow-listed variables of env, then the
// job's git environment (GitEnv) and the job marker (JobEnv), so it starts no nested job and doctor
// procs can find what it leaves.
func acceptanceEnv(jobDir string, env []string) ([]string, error) {
	var keep []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains(acceptanceEnvKeys, k) || strings.HasPrefix(k, "LC_") {
			keep = append(keep, kv)
		}
	}
	keep, err := GitEnv(jobDir, keep)
	if err != nil {
		return nil, err
	}
	return JobEnv(jobDir, keep)
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max     int
	buf     []byte
	dropped int64
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= t.max {
		t.dropped += int64(len(t.buf) + len(p) - t.max)
		t.buf = append(t.buf[:0], p[len(p)-t.max:]...)
		return n, nil
	}
	if over := len(t.buf) + len(p) - t.max; over > 0 {
		t.dropped += int64(over)
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

// Bytes returns the kept tail.
func (t *tailBuffer) Bytes() []byte { return append([]byte(nil), t.buf...) }

// ReadAcceptanceLogTail returns the last max bytes of <jobDir>/acceptance.log as valid UTF-8. The job
// dir is writable by the job: the log is opened without following links or blocking and read only when
// it is a regular file.
func ReadAcceptanceLogTail(jobDir string, max int64) (string, error) {
	f, err := os.OpenFile(filepath.Join(jobDir, AcceptanceLog), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", AcceptanceLog)
	}
	if off := fi.Size() - max; off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return "", err
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, max))
	if err != nil {
		return "", err
	}
	return strings.ToValidUTF8(string(b), "�"), nil
}
