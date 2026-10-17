package guard

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// subagentKeys are the only Agent/Task input keys a job may send, all of them strings. Every other
// key is refused, among them isolation (worktree, remote), run_in_background and team_name, which
// would take the subagent out of the job.
var subagentKeys = map[string]bool{"description": true, "prompt": true, "subagent_type": true, "model": true}

// parseSubagentInput reads the tool input with an exact, case-sensitive key match, as Claude Code
// does: a token walk, because encoding/json matches keys case-insensitively and lets the last
// duplicate win, so {"isolation":"worktree","Isolation":""} would look harmless to a struct decode.
// It refuses a key outside subagentKeys, a duplicate key and a non-string value.
func parseSubagentInput(raw json.RawMessage) (map[string]string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("tool input is not a JSON object")
	}
	out := map[string]string{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, ok := kt.(string)
		if !ok {
			return nil, errors.New("tool input key is not a string")
		}
		if !subagentKeys[k] {
			return nil, fmt.Errorf("key %q is not allowed (only description, prompt, subagent_type, model)", k)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("duplicate key %q", k)
		}
		vt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		v, ok := vt.(string)
		if !ok {
			return nil, fmt.Errorf("key %q must be a string", k)
		}
		out[k] = v
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, errors.New("tool input is not a JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the tool input")
	}
	return out, nil
}

// CheckSubagent decides an Agent/Task call of a job. A subagent runs inside the job's own process,
// so it shares the job's budget, timeout, settings and this guard; it stays a plain foreground
// subagent of the job: the input may carry only description, prompt, subagent_type and model (no
// isolation, background run or team, which would run or write outside the job). Each allowed call is
// recorded in the log file; the fourth is refused. With no log file configured every subagent is
// refused (fail closed: the cap cannot be counted).
func CheckSubagent(raw json.RawMessage, cfg Config) string {
	in, err := parseSubagentInput(raw)
	if err != nil {
		return "agent-jobs guard: a job's subagent runs in the foreground inside the job: " + err.Error()
	}
	if cfg.SubagentLog == "" {
		return "agent-jobs guard: subagents are not counted for this call (no job subagent log), so none may start"
	}
	return reserveSubagent(cfg, in["subagent_type"], in["description"])
}

// reserveSubagent decides one subagent under an exclusive lock on the job's subagent log, so the
// parallel calls of one message are serialized. The cap is held by the monotonic counter in the job's
// store row (cfg.SubagentReserve, an UPDATE that only ever increments) when it is configured, else by
// the log's line count. The log must never be shorter than the counter: a log the job truncated or
// rewrote refuses every further subagent (tampering), and the log is also one of the guard's
// protected paths (file tools and literal Bash targets cannot write it).
func reserveSubagent(cfg Config, typ, desc string) string {
	f, err := os.OpenFile(cfg.SubagentLog, os.O_RDWR|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "agent-jobs guard: subagent log: " + err.Error()
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "agent-jobs guard: subagent log " + cfg.SubagentLog + " is not a regular file"
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return "agent-jobs guard: subagent log: " + err.Error()
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // released on close anyway
	lines := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			lines++
		}
	}
	if err := sc.Err(); err != nil {
		return "agent-jobs guard: subagent log: " + err.Error()
	}
	prev, allowed := lines, lines < MaxSubagentsPerJob
	if cfg.SubagentReserve != nil {
		if prev, allowed, err = cfg.SubagentReserve(MaxSubagentsPerJob); err != nil {
			return "agent-jobs guard: subagent counter: " + err.Error()
		}
		if lines < prev {
			return fmt.Sprintf("agent-jobs guard: the subagent log has %d entries but %d subagents were started: "+
				"it was shortened, so no more subagents start in this job", lines, prev)
		}
	}
	if !allowed {
		return fmt.Sprintf("agent-jobs guard: at most %d subagents per job, all %d are used; do the rest yourself",
			MaxSubagentsPerJob, MaxSubagentsPerJob)
	}
	line := fmt.Sprintf("%s\t%s\t%s\n", time.Now().UTC().Format(time.RFC3339), oneLine(typ), oneLine(desc))
	if _, err := f.WriteString(line); err != nil {
		return "agent-jobs guard: subagent log: " + err.Error()
	}
	return ""
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
