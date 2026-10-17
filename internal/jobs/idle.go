package jobs

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The idle-after-result rule: `claude -p` can stay alive after its final `result` event (seen with
// background subagents: the results were printed, then the process idled with only the job's MCP
// server as a child). The wrapper watches the stream while claude runs; once the last event is a
// `result`, no background task is listed, and for the whole grace period nothing new arrived and claude
// had no live descendant other than the job MCP server, it ends claude (SIGTERM, SIGKILL after
// IdleKillGrace) and the job is judged by that last result instead of waiting for the timeout.
// Linux only (/proc); elsewhere the rule is off and claude's own exit ends the job.

// DefaultIdleAfterResult is the grace period of the rule (config idle_after_result_seconds; -1 turns
// the rule off).
const DefaultIdleAfterResult = 3 * time.Minute

// IdleKillGrace is the wait between SIGTERM and SIGKILL of an idle claude (same as cancel).
const IdleKillGrace = 10 * time.Second

// IdleClosedMarker is the job-dir file the wrapper writes when it closed claude by the rule; it holds
// the one-line reason the daemon records in the job row. It belongs to one attempt: anything that runs
// claude again in the same job dir (a resume) must move it aside with the attempt's other files, or
// the next attempt's verdict reports the idle close.
const IdleClosedMarker = "closed_idle_after_result"

// ReasonIdleClosed starts the job-row message of a job closed by the rule.
const ReasonIdleClosed = "closed by the idle-after-result rule"

// reportHeading marks a final message that follows the report contract.
const reportHeading = "## Summary"

// streamWatch follows stream.jsonl from a reader of its own and keeps the state the rule needs.
type streamWatch struct {
	f          *os.File
	partial    []byte    // an incomplete last line
	lastResult bool      // the last complete event is a `result`
	bgTasks    int       // tasks in the last background_tasks_changed event
	active     time.Time // when new bytes last arrived or a busy descendant was last seen
}

type watchEvent struct {
	Type    string            `json:"type"`
	Subtype string            `json:"subtype"`
	Tasks   []json.RawMessage `json:"tasks"`
}

// poll reads what was appended since the last call.
func (w *streamWatch) poll(now time.Time) {
	buf := make([]byte, 64<<10)
	for {
		n, err := w.f.Read(buf)
		if n > 0 {
			w.active = now
			w.partial = append(w.partial, buf[:n]...)
			for {
				i := bytes.IndexByte(w.partial, '\n')
				if i < 0 {
					break
				}
				w.line(w.partial[:i])
				w.partial = w.partial[i+1:]
			}
		}
		if err != nil || n == 0 {
			return
		}
	}
}

func (w *streamWatch) line(b []byte) {
	if len(bytes.TrimSpace(b)) == 0 {
		return
	}
	var ev watchEvent
	if err := json.Unmarshal(b, &ev); err != nil {
		w.lastResult = false // unknown output counts as activity
		return
	}
	w.lastResult = ev.Type == "result"
	if ev.Type == "system" && ev.Subtype == "background_tasks_changed" {
		w.bgTasks = len(ev.Tasks)
	}
}

// done reports whether the stream says the job is done: the last event is a result and no
// background task is listed.
func (w *streamWatch) done() bool {
	return w.lastResult && len(w.partial) == 0 && w.bgTasks == 0
}

// watchIdle runs until done is closed; it ends claude (proc) when the rule applies and reports
// whether it did. Errors are logged; the rule then simply does not fire.
func watchIdle(proc *os.Process, streamPath string, grace time.Duration, done <-chan struct{}, log io.Writer) bool {
	if runtime.GOOS != "linux" || grace <= 0 {
		<-done
		return false
	}
	f, err := os.Open(streamPath)
	if err != nil {
		fmt.Fprintln(log, "run-wrapper: idle watch:", err)
		<-done
		return false
	}
	defer f.Close()
	self, _ := os.Executable()
	w := &streamWatch{f: f, active: time.Now()}
	tick := min(max(grace/10, 20*time.Millisecond), time.Second)
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-done:
			return false
		case <-t.C:
		}
		now := time.Now()
		w.poll(now)
		if !w.done() {
			continue
		}
		// A busy descendant counts as activity: the grace must pass with no output AND no live child
		// (claude may react to a background child that just exited).
		busy, mcp, err := liveDescendants(proc.Pid, self)
		if err != nil || len(busy) > 0 {
			w.active = now
			continue
		}
		if now.Sub(w.active) < grace {
			continue
		}
		fmt.Fprintf(log, "run-wrapper: %s: no output for %s after the last result, ending claude (pid %d; job MCP server %v)\n",
			ReasonIdleClosed, grace, proc.Pid, mcp)
		if err := proc.Signal(syscall.SIGTERM); errors.Is(err, os.ErrProcessDone) {
			return false // exited on its own meanwhile
		}
		select {
		case <-done:
		case <-time.After(IdleKillGrace):
			_ = proc.Kill()
			<-done
		}
		return true
	}
}

// liveDescendants lists the live (non-zombie) descendants of root: busy are all but the job MCP
// server (this binary running `mcp --role job`), which is listed in mcp; children of the MCP server
// count as busy. /proc is read once; a process that vanishes meanwhile is skipped.
func liveDescendants(root int, self string) (busy, mcp []int, err error) {
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, nil, err
	}
	children := map[int][]int{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if ppid, err := procParent(pid); err == nil {
			children[ppid] = append(children[ppid], pid)
		}
	}
	queue := append([]int(nil), children[root]...)
	for n := 0; len(queue) > 0 && n < 1<<16; n++ {
		pid := queue[0]
		queue = queue[1:]
		queue = append(queue, children[pid]...)
		st, _, err := procStat(pid)
		if err != nil || st == "Z" {
			continue
		}
		if isJobMCPServer(pid, self) {
			mcp = append(mcp, pid)
			continue
		}
		busy = append(busy, pid)
	}
	return busy, mcp, nil
}

// isJobMCPServer reports pid as this binary running `mcp --role job` (as written in the job's mcp.json).
func isJobMCPServer(pid int, self string) bool {
	exe, err := os.Readlink(filepath.Join(procRoot, strconv.Itoa(pid), "exe"))
	if err != nil || self == "" || !samePath(exe, self) {
		return false
	}
	argv, err := procCmdline(pid)
	return err == nil && len(argv) > 1 && argv[1] == "mcp" && mcpRoleJob(argv[2:])
}

func mcpRoleJob(args []string) bool {
	for i, a := range args {
		if a == "--role=job" || (a == "--role" && i+1 < len(args) && args[i+1] == "job") {
			return true
		}
	}
	return false
}

// runIdleWatched starts cmd, watches it under the rule and waits for it. It reports whether the rule
// ended claude, and cmd.Wait's error.
func runIdleWatched(cmd *exec.Cmd, streamPath string, grace time.Duration, log io.Writer) (bool, error) {
	if err := cmd.Start(); err != nil {
		return false, err
	}
	done := make(chan struct{})
	closed := make(chan bool, 1)
	go func() { closed <- watchIdle(cmd.Process, streamPath, grace, done, log) }()
	err := cmd.Wait()
	close(done)
	return <-closed, err
}

// finishIdleClosed settles a job the rule ended: report.md (the last result's text if it is a report;
// else a report.md the job wrote itself, left untouched; else the last top-level assistant text that is
// a report; else a short note), the marker with the reason, and the exit code the daemon judges: 0 for
// a successful last result with a report, 1 otherwise.
func finishIdleClosed(jobDir string, res StreamResult, log io.Writer) int {
	reportPath := filepath.Join(jobDir, "report.md")
	report, keep := "", false
	switch {
	case isReport(res.Text):
		report = res.Text
	case hasReport(jobDir):
		keep = true
	default:
		report = lastReportText(filepath.Join(jobDir, "stream.jsonl"))
	}
	found := keep || report != ""
	code, reason := 0, ReasonIdleClosed+" (result "+res.Subtype+")"
	switch {
	case !res.Found:
		code, reason = 1, ReasonIdleClosed+" (no result event)"
	case res.IsError || res.Subtype != "success":
		code = 1
	case !found:
		code, reason = 1, ReasonIdleClosed+" (result "+res.Subtype+", no report in the stream)"
	}
	if !found {
		report = "## Summary\nThe job ended without a report: claude went idle after its last result and was " +
			"closed by the idle-after-result rule; no final message with the report sections was found in " +
			"stream.jsonl.\n"
	}
	if !keep {
		if err := writeAtomic(reportPath, []byte(strings.TrimRight(report, "\n")+"\n")); err != nil {
			fmt.Fprintln(log, "run-wrapper: write report.md:", err)
		}
	}
	if err := writeAtomic(filepath.Join(jobDir, IdleClosedMarker), []byte(reason+"\n")); err != nil {
		fmt.Fprintln(log, "run-wrapper: write idle marker:", err)
	}
	return code
}

// isReport reports whether text follows the report contract: it starts with the report heading.
func isReport(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), reportHeading)
}

// lastReportText returns the last top-level assistant text block in stream.jsonl that is a report
// ("" if none). Subagent output (a non-null parent_tool_use_id) and text that only quotes the heading
// do not count.
func lastReportText(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var ev struct {
		Type    string          `json:"type"`
		Parent  json.RawMessage `json:"parent_tool_use_id"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	out := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		ev.Type, ev.Parent, ev.Message.Content = "", nil, nil
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil || ev.Type != "assistant" {
			continue
		}
		if p := strings.TrimSpace(string(ev.Parent)); p != "" && p != "null" {
			continue
		}
		for _, c := range ev.Message.Content {
			if c.Type == "text" && isReport(c.Text) {
				out = c.Text
			}
		}
	}
	return out
}

// idleClosedReason returns the reason the wrapper recorded when it closed claude by the rule, or "".
// The job dir is writable by the job, so the marker is read like the sweep marker: no links, no
// blocking, regular files only, capped. It only adds a message; the status comes from exit_code.
func idleClosedReason(dir string) string {
	f, err := os.OpenFile(filepath.Join(dir, IdleClosedMarker), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(f, 512))
	if s := Printable(string(b), 200); strings.HasPrefix(s, ReasonIdleClosed) {
		return s
	}
	return ""
}
