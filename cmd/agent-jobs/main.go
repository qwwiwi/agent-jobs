// Command agent-jobs runs background `claude -p` jobs for an agent:
// a SQLite queue, a per-minute daemon, a detached wrapper, a guard hook, notify and cleanup.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unicode"

	agentjobs "github.com/qwwiwi/agent-jobs"
	"github.com/qwwiwi/agent-jobs/internal/guard"
	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/mcp"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

const usage = `usage: agent-jobs <command> [flags]

  init       --agent A                     create state dir, copy owner-rules template,
                                           write orchestrator.md (main-session duties)
  start      --agent A --title T --kind K [--brief TEXT | --brief-file F | stdin]
             (--model M | --complexity light|normal|hard) [--repo PATH [--base REF]]
             [--max-turns N] [--max-budget-usd B] [--timeout-minutes M]
             [--runner claude|codex] [--reasoning-effort low|medium|high]
                                           a gpt-* model runs on codex exec (read-only kinds
                                           review/research unless codex_write_allowed)
  status     --agent A [<id>]              a job; without an id: the daily automation
                                           (prune + doctor procs) and its last run,
                                           missed checks, undelivered notifications
  list       --agent A [--limit N]
  result     --agent A <id>                print report.md
  cancel     --agent A <id>                cancel a queued/running job, or give up a stopped one
  stop       --agent A <id> [--reason R]   stop a queued/running job, keep work dir and session (resumable)
  resume     --agent A <id> (--context TEXT | --context-file F | stdin)
             [--max-turns N] [--max-budget-usd B] [--timeout-minutes M]
                                           continue a stopped/timeout/failed job in the same session
                                           with an «Orchestrator update»; limits default to what is left
  amend      --agent A <id> (--context TEXT | --context-file F | stdin)
                                           append context to a queued job's brief (keeps its place)
  profile    --agent A --from FILE [--from FILE]... [--force] [--dry-run]
                                           generate the job profile (owner rules, <= 8 KB) from the
                                           sections of the agent's own rules marked
                                           <!-- agent-jobs:profile --> ... <!-- /agent-jobs:profile -->
  card new   --agent A --repo PATH [--name N]   write a project card template for a repository
  card list  --agent A [--repo PATH]       list project cards (with --repo: the ones a job there gets)
  tick       --agent A                     one daemon pass
  daemon     --agent A                     tick every minute, forever
  prune      --agent A [--dry-run]         clean folders of jobs finished > 48 h ago (records and
                                           small top-level text files stay; stream.jsonl, stderr.log
                                           move gzipped into the log archive, see archive)
  archive    --agent A [--dry-run] [--json]  move the records of jobs prune would clean into the log
                                           archive (<archive>/<YYYY-MM>/<id>/) and drop months past
                                           log_archive_days; --dry-run shows what would move
  stats      --agent A [--since D] [--by kind|model|skill] [--json] [--backfill [--all]]
                                           per-job summary: counts, success rate, cost and duration
                                           (median, p90), guard refusal rules, skills; --backfill
                                           first writes missing rows from job dirs and the archive
  intentions --agent A [--status open|done|dropped] [--json]
                                           the agent's own goals (intention_* MCP tools), oldest first
  schedules  --agent A [--json]            the agent's schedules (schedule_* MCP tools): cron in local
                                           time, delivery (session or job kind), next and last firing
  checks     --agent A [--json]            the agent's dead-man's switch checks (check_* MCP tools):
                                           cron, grace, state (ok/late/missed/waiting), last mark
  check mark --agent A NAME [--note T]     leave a completion mark for the check NAME
  notify-owner --agent A --text T [--urgency normal|high]
                                           one "[jobs:notify-owner] T" line into the notify target
                                           for the session to relay (rate limited, stored)
  doctor procs --agent A [--kill] [--json] processes that outlived their job or session, running
                                           rows with a dead pid; report only unless --kill
  version                                  print the version (also --version)
  run-wrapper <jobdir> --token T [--prompt F] [--fallback-prompt F --fallback-session ID] [--runner R] -- <claude|codex> <args...>
                                           (internal, started by the daemon)
  mcp        --agent A --role orchestrator|job   MCP server on stdio (orchestrator: job_* tools;
                                           job: only the tools allow-listed for jobs)
  guard      [--owner-name N]... [--git-name N] [--git-email E] [--git-dir D]... [--subagent-log F]
             [--subagent-db DB --subagent-job ID] [--protect PATH]...
                                           (PreToolUse hook, JSON on stdin)
`

// version is set at build time: go build -ldflags "-X main.version=$(git describe --tags --always --dirty)".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	if reason := refuseInsideJob(cmd, args); reason != "" {
		fmt.Fprintln(os.Stderr, "agent-jobs:", reason)
		os.Exit(1)
	}
	var err error
	switch cmd {
	case "run-wrapper":
		os.Exit(runWrapper(args))
	case "guard":
		os.Exit(runGuard(args, os.Stdin))
	case "init":
		err = cmdInit(args)
	case "start":
		err = cmdStart(args)
	case "status":
		err = cmdStatus(args)
	case "list":
		err = cmdList(args)
	case "result":
		err = cmdResult(args)
	case "cancel":
		err = cmdCancel(args)
	case "stop":
		err = cmdStop(args)
	case "resume":
		err = cmdResume(args)
	case "amend":
		err = cmdAmend(args)
	case "profile":
		err = cmdProfile(args)
	case "card":
		err = cmdCard(args)
	case "tick":
		err = cmdTick(args)
	case "daemon":
		err = cmdDaemon(args)
	case "prune":
		err = cmdPrune(args)
	case "archive":
		err = cmdArchive(args)
	case "stats":
		err = cmdStats(args)
	case "intentions":
		err = cmdIntentions(args)
	case "schedules":
		err = cmdSchedules(args)
	case "checks":
		err = cmdChecks(args)
	case "check":
		err = cmdCheck(args)
	case "notify-owner":
		err = cmdNotifyOwner(args)
	case "doctor":
		err = cmdDoctor(args)
	case "version", "--version":
		fmt.Printf("agent-jobs %s\n", version)
		return
	case "mcp":
		err = cmdMCP(args)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent-jobs:", err)
		os.Exit(1)
	}
}

// refuseInsideJob: no nested jobs. Inside a job – the env marker AGENT_JOBS_JOB is set (the wrapper
// sets it on every job) or an ancestor process is the job wrapper (Linux /proc) – only the job's own
// MCP server (`mcp --role job`), the guard hook, run-wrapper and help run; every command that reads or
// changes the agent's jobs is refused. open() adds the session check once the agent is known.
func refuseInsideJob(cmd string, args []string) string {
	switch cmd {
	case "run-wrapper", "guard", "-h", "--help", "help", "version", "--version":
		return ""
	case "mcp":
		if mcpRole(args) == string(mcp.RoleJob) {
			return ""
		}
	}
	if err := jobs.CheckNotInJob(); err != nil {
		return err.Error()
	}
	return ""
}

// mcpRole returns the --role value of `mcp` args ("" when absent or unparsable).
func mcpRole(args []string) string {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	_ = fs.String("agent", "", "")
	role := fs.String("role", "", "")
	if _, err := parse(fs, args); err != nil {
		return ""
	}
	return *role
}

func runWrapper(args []string) int {
	const u = "usage: agent-jobs run-wrapper <jobdir> --token <launch-token> [--prompt F] " +
		"[--fallback-prompt F --fallback-session ID] [--runner claude|codex] -- <claude|codex> <args...>"
	if len(args) < 5 || args[1] != "--token" {
		fmt.Fprintln(os.Stderr, u)
		return 2
	}
	var opts jobs.WrapperOptions
	rest := args[3:]
	for len(rest) >= 2 && rest[0] != "--" {
		switch rest[0] {
		case "--prompt":
			opts.Prompt = rest[1]
		case "--fallback-prompt":
			opts.FallbackPrompt = rest[1]
		case "--fallback-session":
			opts.FallbackSession = rest[1]
		case "--runner":
			opts.Runner = rest[1]
		default:
			fmt.Fprintln(os.Stderr, u)
			return 2
		}
		rest = rest[2:]
	}
	if len(rest) < 2 || rest[0] != "--" {
		fmt.Fprintln(os.Stderr, u)
		return 2
	}
	return jobs.RunWrapper(args[0], args[2], rest[1:], opts)
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func runGuard(args []string, stdin io.Reader) int {
	fs := flag.NewFlagSet("guard", flag.ContinueOnError)
	var owners multi
	fs.Var(&owners, "owner-name", "owner name that must not appear in commits (repeatable)")
	name := fs.String("git-name", "", "the job's git user.name (git config may set only this value)")
	email := fs.String("git-email", "", "allowed git user.email")
	var gitDirs multi
	fs.Var(&gitDirs, "git-dir", "the job worktree's git dir; file tools never write inside it (repeatable)")
	subagentLog := fs.String("subagent-log", "", "the job's subagent log (at most 3 subagents per job; empty = none)")
	subagentDB := fs.String("subagent-db", "", "the agent's job database holding the job's monotonic subagent counter")
	subagentJob := fs.String("subagent-job", "", "the job id whose subagent counter --subagent-db holds")
	var protected multi
	fs.Var(&protected, "protect", "a path (file or dir) a job never writes (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var reserve func(int) (int, bool, error)
	if *subagentDB != "" || *subagentJob != "" {
		if *subagentDB == "" || *subagentJob == "" {
			fmt.Fprintln(os.Stderr, "guard: --subagent-db and --subagent-job go together")
			return 2
		}
		reserve = func(max int) (int, bool, error) {
			st, err := store.Open(*subagentDB)
			if err != nil {
				return 0, false, err
			}
			defer st.Close()
			return st.ReserveSubagent(*subagentJob, max)
		}
	}
	reason, err := guard.Decide(stdin, guard.Config{
		OwnerNames: owners, GitName: *name, GitEmail: *email, GitDirs: gitDirs, SubagentLog: *subagentLog,
		SubagentReserve: reserve, Protected: protected,
	})
	if err != nil {
		// Fail closed: an unparsable hook payload blocks the call.
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if reason != "" {
		fmt.Fprintln(os.Stderr, reason)
		return 2 // exit 2 = block the tool call, stderr goes back to the model
	}
	return 0
}

// parse parses flags allowing positional args before or after them.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func agentFlag(fs *flag.FlagSet) *string {
	return fs.String("agent", os.Getenv("AGENT_JOBS_AGENT"), "agent name (env AGENT_JOBS_AGENT)")
}

func open(agent string) (*jobs.Manager, error) {
	if agent == "" {
		return nil, errors.New("--agent is required")
	}
	m, err := jobs.Open(agent)
	if err != nil {
		return nil, err
	}
	if err := m.CheckNotInJobSession(); err != nil {
		_ = m.Close()
		return nil, err
	}
	return m, nil
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	agent := agentFlag(fs)
	pin := fs.Bool("pin-scope", false, "set cgroup_scope=required in config.json when unset and user scopes work here (installer)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	if *pin {
		msg, err := jobs.PinScopeRequired(m.StateDir)
		if err != nil {
			return err
		}
		fmt.Println(msg)
	}
	// The main session's duties ship with the tool and are rewritten on every init (not user-edited).
	ob, err := agentjobs.Templates.ReadFile("prompts/orchestrator.md")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.StateDir, 0o700); err != nil {
		return err
	}
	op := filepath.Join(m.StateDir, "orchestrator.md")
	if err := os.WriteFile(op, ob, 0o600); err != nil {
		return err
	}
	fmt.Printf("main-session duties: %s (add to the agent's instructions: read the report, «Not done / uncertain» first, check the diff)\n", op)
	p := m.Cfg.OwnerRulesPath
	if _, err := os.Stat(p); err == nil {
		fmt.Printf("state: %s\nowner rules already exist: %s\n", m.StateDir, p)
		return nil
	}
	b, err := agentjobs.Templates.ReadFile("rules/owner.md")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return err
	}
	fmt.Printf("state: %s\nowner rules template written: %s (edit it)\n", m.StateDir, p)
	return nil
}

func cmdStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	agent := agentFlag(fs)
	var r jobs.StartRequest
	fs.StringVar(&r.Title, "title", "", "job title")
	fs.StringVar(&r.Kind, "kind", "general", "review|fix|research|general")
	brief := fs.String("brief", "", "brief text")
	briefFile := fs.String("brief-file", "", "read brief from file")
	fs.StringVar(&r.RepoPath, "repo", "", "git repository for the worktree (optional)")
	fs.StringVar(&r.Base, "base", "", "base ref (default HEAD)")
	fs.StringVar(&r.Model, "model", "", "model (or give --complexity; one of them is required)")
	fs.StringVar(&r.Complexity, "complexity", "", "light|normal|hard: picks the model by kind when --model is not given")
	fs.IntVar(&r.MaxTurns, "max-turns", 0, fmt.Sprintf("default %d, ceiling %d", jobs.DefaultMaxTurns, jobs.CeilingMaxTurns))
	fs.Float64Var(&r.MaxBudgetUSD, "max-budget-usd", 0, fmt.Sprintf("default %g, ceiling %g", jobs.DefaultMaxBudgetUSD, jobs.CeilingMaxBudgetUSD))
	fs.IntVar(&r.TimeoutMinutes, "timeout-minutes", 0, fmt.Sprintf("default %d, ceiling %d", jobs.DefaultTimeoutMinutes, jobs.CeilingTimeoutMinutes))
	fs.StringVar(&r.Runner, "runner", "", "claude|codex (default: codex for a gpt-* model, else claude)")
	fs.StringVar(&r.ReasoningEffort, "reasoning-effort", "", "codex reasoning effort low|medium|high (default: config, else high)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	switch {
	case *brief != "":
		r.Brief = *brief
	case *briefFile != "":
		b, err := os.ReadFile(*briefFile)
		if err != nil {
			return err
		}
		r.Brief = string(b)
	default:
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		r.Brief = string(b)
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	j, err := m.Enqueue(r)
	if err != nil {
		return err
	}
	fmt.Println(j.ID)
	return nil
}

func oneID(fs *flag.FlagSet, args []string) (string, error) {
	pos, err := parse(fs, args)
	if err != nil {
		return "", err
	}
	if len(pos) != 1 {
		return "", errors.New("exactly one job id expected")
	}
	return pos[0], nil
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Format("2006-01-02 15:04:05")
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	agent := agentFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return errors.New("at most one job id expected")
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	if len(pos) == 0 {
		if err := printDaily(m); err != nil {
			return err
		}
		if err := printMissedChecks(m); err != nil {
			return err
		}
		return printUndelivered(m)
	}
	j, err := m.Store.Get(pos[0])
	if err != nil {
		return err
	}
	fmt.Printf("id:        %s\ntitle:     %s\nkind:      %s\nstatus:    %s\nmodel:     %s\n", j.ID, j.Title, j.Kind, j.Status, j.Model)
	fmt.Printf("limits:    %s\n", jobs.LimitsText(j))
	fmt.Printf("usage:     %d turns · %s · %ds\n", j.Turns, costWord(j.CostUSD, j.CostIsUnknown()), j.DurationSec)
	fmt.Printf("repo:      %s\nbase:      %s\npid/pgid:  %d/%d\n", j.RepoPath, j.BaseCommit, j.PID, j.PGID)
	fmt.Printf("created:   %s\nstarted:   %s\nfinished:  %s\nnotified:  %s\ncleaned:   %s\n",
		fmtTime(&j.CreatedAt), fmtTime(j.StartedAt), fmtTime(j.FinishedAt), fmtTime(j.NotifiedAt), fmtTime(j.CleanedAt))
	fmt.Printf("session:   %s (attempt %d, %s)\n", j.SessionID, j.Attempt, j.RunMode)
	if c := jobs.Containment(j); c != "" {
		fmt.Printf("contain:   %s\n", c)
	}
	fmt.Printf("dir:       %s\n", m.JobDir(j.ID))
	if j.Error != "" {
		fmt.Printf("error:     %s\n", j.Error)
	}
	return nil
}

// printDaily is `status` without a job id: the daily automation (prune and doctor procs) and its
// last run.
func printDaily(m *jobs.Manager) error {
	d, err := m.Daily()
	if err != nil {
		return err
	}
	if !d.Enabled {
		fmt.Println("daily:     off (daily_off)")
	} else {
		fmt.Printf("daily:     prune + doctor procs (report only) once a day after %02d:00 local time\n", d.Hour)
	}
	if d.LastDay == "" {
		fmt.Println("last run:  never")
		return nil
	}
	state := "finished " + fmtTime(d.FinishedAt)
	if d.Running {
		state = "not finished (running, or the daemon stopped during it)"
	}
	fmt.Printf("last run:  %s, started %s, %s\n", d.LastDay, fmtTime(d.StartedAt), state)
	if r := d.Result; r != nil {
		fmt.Printf("prune:     %d jobs, %d bytes freed, %d jobs archived (%d bytes), %d archived months expired\n",
			r.JobsPruned, r.BytesFreed, r.JobsArchived, r.ArchivedBytes, r.ArchiveExpired)
		fmt.Printf("procs:     %d ghosts (%d job, %d leaders, %d suspects), %d dead rows\n",
			r.Ghosts, r.JobGhosts, r.Leaders, r.Suspects, r.DeadRows)
		for _, e := range []struct{ name, text string }{{"prune", r.PruneError}, {"procs", r.ProcsError}, {"run", r.Error}} {
			if e.text != "" {
				fmt.Printf("error:     %s: %s\n", e.name, e.text)
			}
		}
	}
	return nil
}

// printUndelivered is the end of `status` without a job id: notify_owner notifications of the last
// 24 h that did not reach the session.
func printUndelivered(m *jobs.Manager) error {
	list, err := m.UndeliveredNotifications()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("notify:    no undelivered owner notifications in the last 24 h")
		return nil
	}
	fmt.Printf("notify:    %d owner notifications not delivered in the last 24 h\n", len(list))
	for _, n := range list {
		from := "orchestrator"
		if n.JobID != "" {
			from = "job " + n.JobID
		}
		fmt.Printf("  #%d %s %s (%s): %s · error: %s\n", n.ID, fmtTime(&n.CreatedAt), from, n.Urgency, n.Text, n.Error)
	}
	return nil
}

// printMissedChecks is part of `status` without a job id: the checks whose latest expected time
// passed its grace without a mark.
func printMissedChecks(m *jobs.Manager) error {
	list, err := m.MissedChecks()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("checks:    no missed checks")
		return nil
	}
	fmt.Printf("checks:    %d missed\n", len(list))
	for _, v := range list {
		fmt.Printf("  %s expected %s (no mark within %dm), last mark %s", v.Name, v.LastExpectedAt, v.GraceMinutes, fmtTime(v.LastMarkAt))
		if v.LastAlertError != "" {
			fmt.Printf(" · alert not delivered: %s", jobs.Printable(v.LastAlertError, 80))
		}
		fmt.Println()
	}
	return nil
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	agent := agentFlag(fs)
	limit := fs.Int("limit", 20, "max rows")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	list, err := m.Store.List(*limit)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tKIND\tMODEL\tTURNS\tCOST\tCREATED\tCONTAIN\tTITLE")
	for _, j := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", j.ID, j.Status, j.Kind, j.Model, j.Turns,
			jobs.CostText(j.CostUSD, j.CostIsUnknown()),
			fmtTime(&j.CreatedAt), containShort(j), j.Title)
	}
	return w.Flush()
}

// costWord is a cost in a sentence: "$1.23", or "cost unknown".
func costWord(cost float64, unknown bool) string {
	if unknown {
		return "cost unknown"
	}
	return jobs.CostText(cost, false)
}

// containShort is the containment column of `list`: scope, scope! (stop pending), session, or - (never launched).
func containShort(j *store.Job) string {
	switch {
	case j.ScopeUnit != "" && j.ScopePending:
		return "scope!"
	case j.ScopeUnit != "":
		return "scope"
	case jobs.Containment(j) == "":
		return "-"
	}
	return "session"
}

func cmdResult(args []string) error {
	fs := flag.NewFlagSet("result", flag.ContinueOnError)
	agent := agentFlag(fs)
	id, err := oneID(fs, args)
	if err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	j, err := m.Store.Get(id)
	if err != nil {
		return err
	}
	fmt.Println(jobs.DoneLine(j))
	if j.Error != "" {
		fmt.Println("error:", j.Error)
	}
	atts, err := m.Store.Attempts(id)
	if err != nil {
		return err
	}
	for _, a := range atts {
		cost := costWord(a.CostUSD, a.CostUnknown)
		fmt.Printf("attempt %d: %s %s · %d turns · %s · %ds · %s\n", a.N, a.Mode, a.Status, a.Turns, cost,
			a.DurationSec, filepath.Join(m.JobDir(id), jobs.AttemptReportName(a.N)))
	}
	if len(atts) > 0 {
		fmt.Printf("attempt %d: %s %s (current)\n", j.Attempt, j.RunMode, j.Status)
	}
	b, err := os.ReadFile(filepath.Join(m.JobDir(id), "report.md"))
	if err != nil {
		return fmt.Errorf("no report.md (status %s): see %s", j.Status, filepath.Join(m.JobDir(id), "stderr.log"))
	}
	fmt.Print("\n" + string(b))
	return nil
}

func cmdCancel(args []string) error {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	agent := agentFlag(fs)
	id, err := oneID(fs, args)
	if err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Cancel(id); err != nil {
		if errors.Is(err, store.ErrImmutable) {
			return fmt.Errorf("job %s already finished", id)
		}
		return err
	}
	fmt.Println("cancelled", id)
	return nil
}

func cmdStop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	agent := agentFlag(fs)
	reason := fs.String("reason", "", "why the job is stopped (recorded on the job)")
	id, err := oneID(fs, args)
	if err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	j, err := m.Stop(id, *reason)
	if err != nil {
		return err
	}
	fmt.Printf("stopped %s (%d turns, %s); resume with: agent-jobs resume --agent %s %s --context ...\n",
		j.ID, j.Turns, costWord(j.CostUSD, j.CostIsUnknown()), *agent, j.ID)
	return nil
}

// contextFlags adds --context / --context-file; readContext falls back to stdin.
func contextFlags(fs *flag.FlagSet) (*string, *string) {
	return fs.String("context", "", "the update text"), fs.String("context-file", "", "read the update from a file")
}

func readContext(text, file string) (string, error) {
	switch {
	case text != "":
		return text, nil
	case file != "":
		b, err := os.ReadFile(file)
		return string(b), err
	}
	b, err := io.ReadAll(os.Stdin)
	return string(b), err
}

func cmdResume(args []string) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	agent := agentFlag(fs)
	text, file := contextFlags(fs)
	var l jobs.Limits
	fs.IntVar(&l.MaxTurns, "max-turns", 0, fmt.Sprintf("default: what is left; ceiling %d", jobs.CeilingMaxTurns))
	fs.Float64Var(&l.MaxBudgetUSD, "max-budget-usd", 0, fmt.Sprintf("default: what is left; ceiling %g", jobs.CeilingMaxBudgetUSD))
	fs.IntVar(&l.TimeoutMinutes, "timeout-minutes", 0, fmt.Sprintf("default: what is left; ceiling %d", jobs.CeilingTimeoutMinutes))
	id, err := oneID(fs, args)
	if err != nil {
		return err
	}
	ctx, err := readContext(*text, *file)
	if err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	j, err := m.Resume(id, ctx, l)
	if err != nil {
		return err
	}
	fmt.Printf("resume queued %s (limits %d turns · $%g · %dm)\n", j.ID, j.MaxTurns, j.MaxBudgetUSD, j.TimeoutMinutes)
	return nil
}

func cmdAmend(args []string) error {
	fs := flag.NewFlagSet("amend", flag.ContinueOnError)
	agent := agentFlag(fs)
	text, file := contextFlags(fs)
	id, err := oneID(fs, args)
	if err != nil {
		return err
	}
	ctx, err := readContext(*text, *file)
	if err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	if _, err := m.Amend(id, ctx); err != nil {
		return err
	}
	fmt.Println("amended", id)
	return nil
}

func cmdProfile(args []string) error {
	fs := flag.NewFlagSet("profile", flag.ContinueOnError)
	agent := agentFlag(fs)
	var from multi
	fs.Var(&from, "from", "an instruction file of the agent with marked profile sections (repeatable)")
	force := fs.Bool("force", false, "replace a hand-written owner-rules file")
	dry := fs.Bool("dry-run", false, "print the profile instead of writing it")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *dry {
		out, err := jobs.GenerateProfile(from)
		if err != nil {
			return err
		}
		fmt.Print(out)
		fmt.Fprintf(os.Stderr, "job profile: %d of %d bytes\n", len(out), jobs.MaxOwnerRulesBytes)
		return nil
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	p, n, err := m.WriteProfile(from, *force)
	if err != nil {
		return err
	}
	fmt.Printf("job profile written: %s (%d of %d bytes)\n", p, n, jobs.MaxOwnerRulesBytes)
	return nil
}

func cmdCard(args []string) error {
	if len(args) == 0 {
		return errors.New("card: want `card new` or `card list`")
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("card "+sub, flag.ContinueOnError)
	agent := agentFlag(fs)
	repo := fs.String("repo", "", "repository path")
	var name *string
	if sub == "new" {
		name = fs.String("name", "", "card name (default: the repository directory name)")
	}
	if _, err := parse(fs, args); err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	switch sub {
	case "new":
		if *repo == "" {
			return errors.New("card new: --repo is required")
		}
		p, err := m.NewCard(*repo, *name)
		if err != nil {
			return err
		}
		fmt.Printf("project card written: %s (fill in the four sections; at most %d bytes)\n", p, jobs.MaxCardBytes)
		return nil
	case "list":
		var cards []jobs.Card
		if *repo != "" {
			top, err := jobs.RepoTopLevel(*repo)
			if err != nil {
				return err
			}
			if cards, err = m.CardsFor(top); err != nil {
				return err
			}
		} else if cards, err = m.LoadCards(); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "CARD\tBYTES\tMATCHES\tPATH")
		for _, c := range cards {
			keys := append(append([]string{}, c.Repos...), c.Remotes...)
			fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", c.Name, len(c.Body), strings.Join(keys, ","), c.Path)
		}
		return w.Flush()
	}
	return fmt.Errorf("card: unknown subcommand %q (want new or list)", sub)
}

func cmdTick(args []string) error {
	fs := flag.NewFlagSet("tick", flag.ContinueOnError)
	agent := agentFlag(fs)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	return m.Tick()
}

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	agent := agentFlag(fs)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// The reference interval is fixed («раз в минуту»); it is not configurable.
	m.Log.Printf("daemon started, interval %s, state %s", jobs.TickInterval, m.StateDir)
	// The brain proxy: jobs reach shared memory through the daemon and never hold the token.
	go func() {
		if err := m.ServeBrain(ctx); err != nil {
			m.Log.Printf("%v (jobs get no memory tools and no notify_owner until the daemon restarts)", err)
		}
	}()
	t := time.NewTicker(jobs.TickInterval)
	defer t.Stop()
	for {
		if err := m.Tick(); err != nil {
			m.Log.Printf("tick: %v", err)
		}
		// Once a day after daily_hour: prune and doctor procs (report only) in the background, so
		// the next ticks keep launching jobs while it runs.
		m.StartDaily(ctx)
		select {
		case <-ctx.Done():
			// Jobs run in their own sessions: stopping the daemon does not touch them. A daily run
			// in progress stops between jobs (ctx) and is waited for before the store closes.
			m.Log.Printf("daemon stopping; running jobs keep running")
			m.WaitDaily()
			return nil
		case <-t.C:
		}
	}
}

func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	agent := agentFlag(fs)
	roleFlag := fs.String("role", "", "orchestrator|job")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *agent == "" {
		return errors.New("--agent is required")
	}
	role, err := mcp.ParseRole(*roleFlag)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// stdout carries the protocol: nothing else may print there.
	return mcp.Serve(ctx, role, *agent)
}

func cmdPrune(args []string) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	agent := agentFlag(fs)
	dry := fs.Bool("dry-run", false, "only list what would be removed")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	rep, perr := m.Prune(*dry)
	if *asJSON {
		if err := printJSON(rep); err != nil {
			return err
		}
		return perr
	}
	verb := "removed"
	if *dry {
		verb = "would remove"
	}
	for _, j := range rep.Jobs {
		var n int64
		for _, e := range j.Removed {
			n += e.Bytes
		}
		fmt.Printf("%s  %s  %s %d entries (%d bytes)", j.ID, j.Status, verb, len(j.Removed), n)
		if j.Skipped != "" {
			fmt.Printf("  [%s]", j.Skipped)
		}
		if j.Error != "" {
			fmt.Printf("  error: %s", j.Error)
		}
		fmt.Println()
		for _, e := range j.Removed {
			fmt.Printf("    %s  %d  %s\n", e.Path, e.Bytes, e.Reason)
		}
	}
	fmt.Printf("prune: %d job folders, %s %d bytes\n", len(rep.Jobs), verb, rep.Bytes)
	printArchived(rep.ArchivedBytes, rep.ArchiveExpired, rep.RetentionOff, *dry)
	return perr
}

func printArchived(bytes int64, expired []jobs.PruneEntry, retentionOff string, dry bool) {
	verb, gone := "moved", "removed"
	if dry {
		verb, gone = "would move", "would remove"
	}
	fmt.Printf("archive: %s %d bytes into the log archive\n", verb, bytes)
	for _, e := range expired {
		fmt.Printf("archive: %s %s  %d  %s\n", gone, e.Path, e.Bytes, e.Reason)
	}
	if retentionOff != "" {
		fmt.Printf("archive: %s\n", retentionOff)
	}
}

func cmdArchive(args []string) error {
	fs := flag.NewFlagSet("archive", flag.ContinueOnError)
	agent := agentFlag(fs)
	dry := fs.Bool("dry-run", false, "only list what would move")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	rep, aerr := m.Archive(*dry)
	if *asJSON {
		if err := printJSON(rep); err != nil {
			return err
		}
		return aerr
	}
	keep := "forever"
	if rep.Days > 0 {
		keep = fmt.Sprintf("%d days", rep.Days)
	}
	fmt.Printf("archive: %s (kept %s)\n", rep.Dir, keep)
	var n int64
	for _, j := range rep.Jobs {
		fmt.Printf("%s  -> %s", j.ID, j.Dest)
		if j.Already {
			fmt.Print("  [already archived]")
		}
		if j.Error != "" {
			fmt.Printf("  error: %s", j.Error)
		}
		fmt.Println()
		for _, e := range j.Moved {
			n += e.Bytes
			fmt.Printf("    %s  %d  %s\n", e.Path, e.Bytes, e.Reason)
		}
	}
	fmt.Printf("archive: %d jobs\n", len(rep.Jobs))
	printArchived(n, rep.Expired, rep.RetentionOff, *dry)
	return aerr
}

func cmdIntentions(args []string) error {
	fs := flag.NewFlagSet("intentions", flag.ContinueOnError)
	agent := agentFlag(fs)
	status := fs.String("status", "", "only open|done|dropped (default all)")
	asJSON := fs.Bool("json", false, "print the list as JSON (same as intention_list)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	list, err := m.ListIntentions(*status)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(mcp.IntentionsOutput{Intentions: list})
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tPRIORITY\tDUE\tUPDATED\tTEXT")
	for _, it := range list {
		due := it.Due
		if due == "" {
			due = "-"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", it.ID, it.Status, it.Priority, due, fmtTime(&it.UpdatedAt), it.Text)
	}
	return w.Flush()
}

func cmdSchedules(args []string) error {
	fs := flag.NewFlagSet("schedules", flag.ContinueOnError)
	agent := agentFlag(fs)
	asJSON := fs.Bool("json", false, "print the list as JSON (same as schedule_list)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	list, err := m.ListSchedules()
	if err != nil {
		return err
	}
	out := mcp.SchedulesOutput{Schedules: make([]mcp.ScheduleView, 0, len(list))}
	for _, sc := range list {
		out.Schedules = append(out.Schedules, mcp.ScheduleViewOf(m, sc))
	}
	if *asJSON {
		return printJSON(out)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tENABLED\tCRON\tDELIVERY\tNEXT\tLAST\tPROMPT")
	for _, v := range out.Schedules {
		delivery := "session"
		if v.Kind != "" {
			delivery = "job " + v.Kind + " " + v.Model
		}
		next, last := v.NextFireAt, fmtTime(v.LastFiredAt)
		if next == "" {
			next = "-"
		}
		if v.LastError != "" {
			last += " (not delivered: " + jobs.Printable(v.LastError, 60) + ")"
		} else if v.LastJobID != "" {
			last += " (job " + v.LastJobID + ")"
		}
		fmt.Fprintf(w, "%d\t%t\t%s\t%s\t%s\t%s\t%s\n", v.ID, v.Enabled, v.Cron, delivery, next, last,
			jobs.Printable(v.Prompt, 80))
	}
	return w.Flush()
}

func cmdChecks(args []string) error {
	fs := flag.NewFlagSet("checks", flag.ContinueOnError)
	agent := agentFlag(fs)
	asJSON := fs.Bool("json", false, "print the list as JSON (same as check_list)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	list, err := m.CheckViews()
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(mcp.ChecksOutput{Checks: list})
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tCRON\tGRACE\tEXPECTED\tNEXT\tLAST MARK\tNOTE")
	for _, v := range list {
		exp, next := v.LastExpectedAt, v.NextExpectedAt
		if exp == "" {
			exp = "-"
		}
		if next == "" {
			next = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%dm\t%s\t%s\t%s\t%s\n", v.Name, v.State, v.Cron, v.GraceMinutes, exp, next,
			fmtTime(v.LastMarkAt), jobs.Printable(v.LastMarkNote, 60))
	}
	return w.Flush()
}

func cmdCheck(args []string) error {
	if len(args) == 0 || args[0] != "mark" {
		return errors.New("usage: agent-jobs check mark --agent A NAME [--note T]")
	}
	fs := flag.NewFlagSet("check mark", flag.ContinueOnError)
	agent := agentFlag(fs)
	note := fs.String("note", "", "optional one-line note (at most 300 characters)")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("exactly one check name expected")
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	c, err := m.MarkCheck(pos[0], *note, "")
	if err != nil {
		return err
	}
	fmt.Printf("check %s marked at %s\n", c.Name, fmtTime(c.LastMarkAt))
	return nil
}

func cmdNotifyOwner(args []string) error {
	fs := flag.NewFlagSet("notify-owner", flag.ContinueOnError)
	agent := agentFlag(fs)
	text := fs.String("text", "", "the message for the owner, one line (at most 300 characters)")
	urgency := fs.String("urgency", "", "normal|high (default normal)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return errors.New("no positional arguments: give the message with --text")
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	n, err := m.NotifyOwner("", *text, *urgency)
	if err != nil {
		return err
	}
	fmt.Printf("notification %d delivered: %s\n", n.ID, jobs.NotifyLine(n))
	return nil
}

func cmdStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	agent := agentFlag(fs)
	since := fs.String("since", "", "only jobs finished since: <n>d, a duration (36h) or a date YYYY-MM-DD")
	by := fs.String("by", "kind", "group by kind|model|skill")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	backfill := fs.Bool("backfill", false, "first write the missing summary rows from job dirs and the archive")
	all := fs.Bool("all", false, "with --backfill: rewrite every row, not only the missing ones")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	t, err := jobs.ParseSince(*since, m.Now())
	if err != nil {
		return err
	}
	if *backfill {
		b, err := m.BackfillStats(*all)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "backfill: %d rows written, %d present, %d errors\n", b.Recorded, b.Present, len(b.Errors))
		for _, e := range b.Errors {
			fmt.Fprintln(os.Stderr, "backfill:", e)
		}
	}
	rep, err := m.Stats(t, *by)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(rep)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	row := func(key string, s jobs.StatsSummary) {
		fmt.Fprintf(w, "%s\t%d\t%d\t%.0f%%\t$%.2f\t$%.2f\t$%.2f\t%d\t%ds\t%ds\t%d\n", key, s.Jobs,
			s.ByStatus[store.StatusSucceeded], s.SuccessRate*100, s.CostTotalUSD, s.CostMedianUSD, s.CostP90USD,
			s.CostUnknownJobs, s.DurationMedianSec, s.DurationP90Sec, s.GuardRefusals)
	}
	// COST, MEDIAN and P90 are over the jobs with a known cost; COST? counts the others (codex, killed).
	fmt.Fprintln(w, strings.ToUpper(rep.By)+"\tJOBS\tOK\tRATE\tCOST\tMEDIAN\tP90\tCOST?\tDUR MEDIAN\tDUR P90\tREFUSALS")
	for _, g := range rep.Groups {
		row(shown(g.Key), g.StatsSummary)
	}
	row("total", rep.Totals)
	if err := w.Flush(); err != nil {
		return err
	}
	if len(rep.GuardRules) > 0 {
		fmt.Println("\nguard refusals by rule:")
		for _, r := range rep.GuardRules {
			fmt.Printf("  %-22s %d\n", shown(r.Name), r.Count)
		}
	}
	if len(rep.Skills) > 0 {
		fmt.Println("\nskills (uses, jobs, success rate):")
		for _, s := range rep.Skills {
			fmt.Printf("  %-22s %d  %d  %.0f%%\n", shown(s.Name), s.Uses, s.Jobs, s.SuccessRate*100)
		}
	}
	return nil
}

// shown prints a name taken from a job's stream (a skill, a guard rule, a group key): as it is when
// every rune is printable, quoted otherwise, so a job cannot plant terminal escape sequences.
func shown(s string) string {
	if strings.IndexFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) < 0 {
		return s
	}
	return strconv.Quote(s)
}

func cmdDoctor(args []string) error {
	if len(args) == 0 || args[0] != "procs" {
		return errors.New("usage: agent-jobs doctor procs --agent A [--kill] [--json]")
	}
	fs := flag.NewFlagSet("doctor procs", flag.ContinueOnError)
	agent := agentFlag(fs)
	kill := fs.Bool("kill", false, "end the processes proven to belong to a finished job (leaders and suspects are never killed)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if _, err := parse(fs, args[1:]); err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	rep, err := m.DoctorProcs(*kill)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(rep)
	}
	if rep.Note != "" {
		fmt.Println("note:", rep.Note)
	}
	for _, r := range rep.DeadRows {
		fmt.Printf("row   %s  pid %d  %s  (%s)\n", r.ID, r.PID, r.Reason, r.Title)
	}
	for _, g := range rep.Ghosts {
		state := ""
		switch {
		case g.Killed:
			state = "  KILLED"
		case g.KillError != "":
			state = "  kill failed: " + g.KillError
		}
		fmt.Printf("%-7s pid %d  sid %d  %s  %s%s\n", g.Kind, g.PID, g.SID, g.Reason, g.Cmdline, state)
	}
	fmt.Printf("doctor procs: %d processes, %d dead rows, %d killed", len(rep.Ghosts), len(rep.DeadRows), rep.Killed)
	if !*kill && len(rep.Ghosts) > 0 {
		fmt.Print(" (report only; --kill ends the job ghosts)")
	}
	fmt.Println()
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
