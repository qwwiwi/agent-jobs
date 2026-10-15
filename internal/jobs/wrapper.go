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

// RunWrapper runs claude with prompt.md on stdin, stdout→stream.jsonl, stderr→stderr.log,
// cwd=<jobdir>/work; then extracts the final result text into report.md and writes exit_code.
// exit_code is written last, so its presence means the job's files are complete.
func RunWrapper(jobDir string, argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "run-wrapper: missing claude command")
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
