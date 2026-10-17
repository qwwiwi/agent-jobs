package guard

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
)

// MaxSubagentsPerJob is the owner's cap (docs/DECISIONS.md, 2026-10-17): a job may use at most three
// subagents, inside its own turn, budget and time limits.
const MaxSubagentsPerJob = 3

// subagentTools are the Claude Code tools that start a subagent ("Task" is the older name of "Agent").
var subagentTools = map[string]bool{"Agent": true, "Task": true}

// IsSubagentTool reports whether a tool call starts a subagent.
func IsSubagentTool(name string) bool { return subagentTools[name] }

// subagentInput is the part of an Agent/Task call the guard checks.
type subagentInput struct {
	Description     string          `json:"description"`
	SubagentType    string          `json:"subagent_type"`
	Isolation       json.RawMessage `json:"isolation"`
	RunInBackground json.RawMessage `json:"run_in_background"`
	TeamName        json.RawMessage `json:"team_name"`
}

// present reports a JSON field that is set to something other than null, false or "".
func present(v json.RawMessage) bool {
	s := strings.TrimSpace(string(v))
	return s != "" && s != "null" && s != "false" && s != `""`
}

// CheckSubagent decides an Agent/Task call of a job. A subagent runs inside the job's own process,
// so it shares the job's budget, timeout, settings and this guard; it stays a foreground subagent of
// the job (no background run, no team, no isolation: a remote or worktree subagent would run or
// write outside the job). Each allowed call is recorded in the log file; the fourth is refused.
// With no log file configured every subagent is refused (fail closed: the cap cannot be counted).
func CheckSubagent(raw json.RawMessage, cfg Config) string {
	var in subagentInput
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return "agent-jobs guard: unreadable subagent call input"
		}
	}
	switch {
	case present(in.Isolation):
		return "agent-jobs guard: a job's subagent runs in the job itself; isolation (worktree, remote) is not allowed"
	case present(in.RunInBackground):
		return "agent-jobs guard: a job's subagent runs in the foreground, so its work is finished before the job's report"
	case present(in.TeamName):
		return "agent-jobs guard: a job does not start agent teams; at most three plain subagents"
	}
	if cfg.SubagentLog == "" {
		return "agent-jobs guard: subagents are not counted for this call (no job subagent log), so none may start"
	}
	n, err := reserveSubagent(cfg.SubagentLog, in.SubagentType, in.Description)
	if err != nil {
		return "agent-jobs guard: subagent log: " + err.Error()
	}
	if n > MaxSubagentsPerJob {
		return fmt.Sprintf("agent-jobs guard: at most %d subagents per job, all %d are used; do the rest yourself",
			MaxSubagentsPerJob, MaxSubagentsPerJob)
	}
	return ""
}

// reserveSubagent counts the subagents already recorded in path and, below the cap, records one more.
// It returns the number this call would be (cap+1 = refused, nothing recorded). The file is locked,
// so parallel calls of one message cannot pass the cap together.
func reserveSubagent(path, typ, desc string) (int, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is not a regular file", path)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return 0, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // released on close anyway
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			n++
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	if n >= MaxSubagentsPerJob {
		return n + 1, nil
	}
	line := fmt.Sprintf("%s\t%s\t%s\n", time.Now().UTC().Format(time.RFC3339), oneLine(typ), oneLine(desc))
	if _, err := f.WriteString(line); err != nil {
		return 0, err
	}
	return n + 1, nil
}

// oneLine keeps a log field on one line and short.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120])
	}
	if s == "" {
		s = "-"
	}
	return s
}
