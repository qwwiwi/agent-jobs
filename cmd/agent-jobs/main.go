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
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

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
  status     --agent A <id>
  list       --agent A [--limit N]
  result     --agent A <id>                print report.md
  cancel     --agent A <id>
  tick       --agent A                     one daemon pass
  daemon     --agent A                     tick every minute, forever
  prune      --agent A [--dry-run]         clean folders of jobs finished > 48 h ago (records and
                                           small top-level text files stay)
  doctor procs --agent A [--kill] [--json] processes that outlived their job or session, running
                                           rows with a dead pid; report only unless --kill
  version                                  print the version (also --version)
  run-wrapper <jobdir> --token T -- <claude> <args...>   (internal, started by the daemon)
  mcp        --agent A --role orchestrator|job   MCP server on stdio (orchestrator: job_* tools;
                                           job: only the tools allow-listed for jobs)
  guard      [--owner-name N]... [--git-name N] [--git-email E] [--git-dir D]...   (PreToolUse hook, JSON on stdin)
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
	case "tick":
		err = cmdTick(args)
	case "daemon":
		err = cmdDaemon(args)
	case "prune":
		err = cmdPrune(args)
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
	if len(args) < 5 || args[1] != "--token" || args[3] != "--" {
		fmt.Fprintln(os.Stderr, "usage: agent-jobs run-wrapper <jobdir> --token <launch-token> -- <claude> <args...>")
		return 2
	}
	return jobs.RunWrapper(args[0], args[2], args[4:])
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
	if err := fs.Parse(args); err != nil {
		return 2
	}
	reason, err := guard.Decide(stdin, guard.Config{OwnerNames: owners, GitName: *name, GitEmail: *email, GitDirs: gitDirs})
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
	if _, err := parse(fs, args); err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
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
	fmt.Printf("id:        %s\ntitle:     %s\nkind:      %s\nstatus:    %s\nmodel:     %s\n", j.ID, j.Title, j.Kind, j.Status, j.Model)
	fmt.Printf("limits:    %d turns · $%g · %dm\n", j.MaxTurns, j.MaxBudgetUSD, j.TimeoutMinutes)
	fmt.Printf("usage:     %d turns · $%.2f · %ds\n", j.Turns, j.CostUSD, j.DurationSec)
	fmt.Printf("repo:      %s\nbase:      %s\npid/pgid:  %d/%d\n", j.RepoPath, j.BaseCommit, j.PID, j.PGID)
	fmt.Printf("created:   %s\nstarted:   %s\nfinished:  %s\nnotified:  %s\ncleaned:   %s\n",
		fmtTime(&j.CreatedAt), fmtTime(j.StartedAt), fmtTime(j.FinishedAt), fmtTime(j.NotifiedAt), fmtTime(j.CleanedAt))
	fmt.Printf("dir:       %s\n", m.JobDir(j.ID))
	if j.Error != "" {
		fmt.Printf("error:     %s\n", j.Error)
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
	fmt.Fprintln(w, "ID\tSTATUS\tKIND\tMODEL\tTURNS\tCOST\tCREATED\tTITLE")
	for _, j := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t$%.2f\t%s\t%s\n", j.ID, j.Status, j.Kind, j.Model, j.Turns, j.CostUSD, fmtTime(&j.CreatedAt), j.Title)
	}
	return w.Flush()
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
	t := time.NewTicker(jobs.TickInterval)
	defer t.Stop()
	for {
		if err := m.Tick(); err != nil {
			m.Log.Printf("tick: %v", err)
		}
		select {
		case <-ctx.Done():
			// Jobs run in their own sessions: stopping the daemon does not touch them.
			m.Log.Printf("daemon stopping; running jobs keep running")
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
	return perr
}

func cmdDoctor(args []string) error {
	if len(args) == 0 || args[0] != "procs" {
		return errors.New("usage: agent-jobs doctor procs --agent A [--kill] [--json]")
	}
	fs := flag.NewFlagSet("doctor procs", flag.ContinueOnError)
	agent := agentFlag(fs)
	kill := fs.Bool("kill", false, "end the processes proven to belong to a finished job (suspects are never killed)")
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
