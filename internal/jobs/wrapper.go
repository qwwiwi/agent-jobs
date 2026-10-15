package jobs

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// StreamResult is the final `result` event of claude's stream-json output.
type StreamResult struct {
	Found   bool
	Subtype string
	IsError bool
	Turns   int
	CostUSD float64
	Text    string
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
func RunWrapper(jobDir, token string, argv []string) int {
	if len(argv) == 0 || token == "" {
		fmt.Fprintln(os.Stderr, "run-wrapper: missing launch token or claude command")
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
	code := runClaude(jobDir, argv, stderrLog)
	res := ParseStream(filepath.Join(jobDir, "stream.jsonl"))
	if res.Text != "" {
		if err := writeAtomic(filepath.Join(jobDir, "report.md"), []byte(res.Text+"\n")); err != nil {
			fmt.Fprintln(stderrLog, "run-wrapper: write report.md:", err)
		}
	}
	if err := writeAtomic(filepath.Join(jobDir, "exit_code"), []byte(fmt.Sprintf("%d\n", code))); err != nil {
		fmt.Fprintln(stderrLog, "run-wrapper: write exit_code:", err)
		return 2
	}
	return 0
}

func runClaude(jobDir string, argv []string, stderrLog *os.File) int {
	stdin, err := os.Open(filepath.Join(jobDir, "prompt.md"))
	if err != nil {
		fmt.Fprintln(stderrLog, "run-wrapper:", err)
		return 127
	}
	defer stdin.Close()
	stdout, err := os.Create(filepath.Join(jobDir, "stream.jsonl"))
	if err != nil {
		fmt.Fprintln(stderrLog, "run-wrapper:", err)
		return 127
	}
	defer stdout.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = filepath.Join(jobDir, "work")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderrLog
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return ee.ExitCode()
		}
		fmt.Fprintln(stderrLog, "run-wrapper: start claude:", err)
		return 127
	}
	return 0
}
