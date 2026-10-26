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
	"github.com/qwwiwi/agent-jobs/internal/httpapi"
	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/mcp"
	"github.com/qwwiwi/agent-jobs/internal/skills"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

const usage = `usage: agent-jobs <command> [flags]

  init       --agent A                     create state dir, copy owner-rules template,
                                           write orchestrator.md (main-session duties)
  start      --agent A --title T --kind K [--brief TEXT | --brief-file F | stdin]
             (--model M | --complexity light|normal|hard) [--repo PATH [--base REF]]
             [--max-turns N] [--max-budget-usd B] [--timeout-minutes M]
             [--runner claude|codex] [--reasoning-effort low|medium|high|xhigh|max] [--owner-waits]
             [--skills a,+b,-c] [--size small|large]
                                           a gpt-* model runs on codex exec (read-only kinds
                                           review/research unless codex_write_allowed);
                                           --owner-waits: the owner asked for it (Stop hook);
                                           --skills: edits to the default catalog skills
                                           --size: fix/general (Wave F; required with config
                                           size_required); large needs covers (job_start only)
             [--acceptance-cmd CMD]       fix/general: one-line shell command the job must
                                           pass in its worktree after claude exits 0
                                           (needs config acceptance_allowed)
  status     --agent A [<id>]              a job; without an id: the daily automation
                                           (prune + doctor procs) and its last run, missed checks,
                                           undelivered owner notifications, owner replies pending
  list       --agent A [--limit N] [--status S] [--json]
                                           newest jobs first; --json: the HTTP API's /v1/jobs view
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
  stats      --agent A [--since D] [--by kind|model|skill|effort|repo] [--json] [--backfill [--all]]
                                           per-job summary: counts, success rate, cost and duration
                                           (median, p90), guard refusal rules, skills (uses and
                                           delivered); --backfill first writes missing rows from
                                           job dirs and the archive
             --by skill --by-repo (or --by skill,repo) [--repo R] [--limit N]
                                           the skills rating: delivered, used, success rate, last
                                           used, median cost, stale flag and a per-repository
                                           breakdown (same as GET /v1/skills)
  skills list   --agent A [--kind K] [--repo R] [--status S] [--json]
  skills search --agent A QUERY [--kind K] [--repo R] [--status S] [--json]
                                           the skill catalog (config skill_roots, same as the
                                           skill_list MCP tool); search scores names, descriptions
                                           and SKILL.md text locally, best 10
  gate slicing --agent A --repo PATH [--checklist-id ID]
                                           the slicing check (Wave F, F3) of the repo's checklist
                                           as JSON (same as slicing_get): its lists and decisions
  gate briefing --agent A --repo PATH [--checklist-id ID]
                                           the briefing (Wave F, F8) of the repo's draft checklist
                                           as JSON (same as briefing_get): its question rounds
  gate verdict --agent A --repo PATH [--sha S] [--checklist-id ID]
                                           the latest blind judge verdict (Wave F, F5) as JSON
                                           (same as judge_get), with its manifest_check record
  gate release-check --agent A --repo PATH --sha S
                                           whether sha may ship (Wave F, F7; same as release_check):
                                           JSON, the owner line on stderr, exit 1 unless it passed
  gate retire --agent A REPO               owner only: take REPO (its main work tree path as stored,
                                           also when it is gone) out of the merge gate (F9 (b)); a
                                           later checklist approval gates it again
  intentions --agent A [--status open|done|dropped] [--json]
                                           the agent's own goals (intention_* MCP tools), oldest first
  milestones --agent A [--status S] [--repo R] [--checklist-id ID] [--updated-since T] [--tree] [--json]
                                           the milestones (milestone_list): by repo, order, creation
  milestone  --agent A <id> [--json]       one milestone's tree (milestone_get): tasks with their
                                           derived status and jobs, ready tasks, progress
  schedules  --agent A [--json]            the agent's schedules (schedule_* MCP tools): cron in local
                                           time, delivery (session or job kind), next and last firing
  checks     --agent A [--json]            the agent's dead-man's switch checks (check_* MCP tools):
                                           cron, grace, state (ok/late/missed/waiting), last mark
  check add --agent A NAME --cron C [--allowed-schedule-ids 1,2]
  check set --agent A NAME [--allowed-schedule-ids 1,2] [--cron C] [--grace-minutes N] [--enabled=false]
  check mark --agent A NAME [--note T]     leave a completion mark for the check NAME
  notify-owner --agent A --text T [--urgency normal|high]
                                           one "[jobs:notify-owner] T" line into the notify target
                                           for the session to relay (rate limited, stored)
  hook stop  --agent A                     Stop hook of the orchestrator session (JSON on stdin):
                                           blocks the stop once per turn while an owner-waits job's
                                           reply to the owner is pending (owner_replied clears it);
                                           holds only the session bound by hook bind
  hook bind  --agent A                     SessionStart hook of the orchestrator session (JSON on
                                           stdin): binds it as the session the Stop hook holds
  hook brief-reminder --agent A            UserPromptSubmit hook of the orchestrator session (JSON
                                           on stdin): one line with the size rule and whether the
                                           session's repo has an approved checklist; never blocks
  hook merge-gate --agent A                PreToolUse hook on Bash of the orchestrator session (JSON
                                           on stdin): in a repo with an approved checklist, blocks
                                           gh pr merge (exit 2) unless --match-head-commit <sha> names
                                           a sha release_check passed on; no-op inside a job
  hook skills-picker                       PreToolUse hook on milestone_create and job_start of the
                                           orchestrator session (JSON on stdin): asks for skills once
                                           per milestone (tasks without a skills list), not on
                                           job_start of a task; never blocks, reads no state
  doctor procs --agent A [--kill] [--json] processes that outlived their job or session, running
                                           rows with a dead pid; report only unless --kill
  heartbeat run-now --agent A [--dry-run]  one heartbeat now (config heartbeat; the interval is
                                           skipped, the other gates are not); --dry-run prints the
                                           gate result and the decision prompt, no model call
  heartbeat show ID --agent A [--json]     one stored decision (ID from "[jobs:heartbeat] wake ID"),
                                           its reason labelled untrusted model text
  http-api token --agent A --rotate        write a new bearer token for the read-only HTTP API
                                           (<state>/http-api.token, mode 600) and print it once
  version                                  print the version (also --version)
  run-wrapper <jobdir> --token T [--prompt F] [--fallback-prompt F --fallback-session ID] [--runner R] [--checklist-lock-dir D] [--manifest-check] -- <claude|codex> <args...>
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
	jobs.Version = version
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	if reason := refuseInsideJob(cmd, args); reason != "" {
		if silentInsideJob(cmd, args) {
			os.Exit(0)
		}
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
	case "skills":
		err = cmdSkills(args)
	case "stats":
		err = cmdStats(args)
	case "gate":
		err = cmdGate(args)
	case "intentions":
		err = cmdIntentions(args)
	case "milestones":
		err = cmdMilestones(args)
	case "milestone":
		err = cmdMilestone(args)
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
	case "hook":
		os.Exit(runHook(args, os.Stdin, os.Stdout, os.Stderr, open))
	case "http-api":
		err = cmdHTTPAPI(args)
	case "heartbeat":
		err = cmdHeartbeat(args)
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
		"[--fallback-prompt F --fallback-session ID] [--runner claude|codex] [--checklist-lock-dir D] [--manifest-check] -- <claude|codex> <args...>"
	if len(args) < 5 || args[1] != "--token" {
		fmt.Fprintln(os.Stderr, u)
		return 2
	}
	var opts jobs.WrapperOptions
	rest := args[3:]
	for len(rest) >= 2 && rest[0] != "--" {
		if rest[0] == "--manifest-check" {
			opts.ManifestCheck = true
			rest = rest[1:]
			continue
		}
		switch rest[0] {
		case "--prompt":
			opts.Prompt = rest[1]
		case "--fallback-prompt":
			opts.FallbackPrompt = rest[1]
		case "--fallback-session":
			opts.FallbackSession = rest[1]
		case "--runner":
			opts.Runner = rest[1]
		case "--checklist-lock-dir":
			opts.ChecklistLockDir = rest[1]
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
	var noRead multi
	fs.Var(&noRead, "no-read", "a directory a job never reads or writes, like the agent state (repeatable)")
	readOnly := fs.String("read-only", "", "the worktree of a review job: no tool call writes inside it")
	var readAllow multi
	fs.Var(&readAllow, "read-allow", "a path a gate job may read; given, every other path is refused (repeatable)")
	var sources multi
	fs.Var(&sources, "sources", "the job's cloned sources dir: read-only, its code never runs on the host (repeatable)")
	sandbox := fs.String("sandbox", "", "the job's sandbox dir: its copies run only in docker run --rm with one -v mount from it")
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
		SubagentReserve: reserve, Protected: protected, NoRead: noRead, ReadOnly: *readOnly, ReadAllow: readAllow,
		Sources: sources, Sandbox: *sandbox, JobID: guardJobID(*subagentJob),
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

// guardJobID is the job id the guard names sandbox containers by: the job marker the wrapper sets,
// else the daemon's --subagent-job.
func guardJobID(subagentJob string) string {
	if id, ok := jobs.InsideJob(); ok && id != "" {
		return id
	}
	return subagentJob
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
	fs.StringVar(&r.ReasoningEffort, "reasoning-effort", "", "reasoning effort: low|medium|high on codex (default: config, else high), "+
		"low|medium|high|xhigh|max on claude (default: effort_by_kind, else high; general medium)")
	fs.StringVar(&r.AcceptanceCmd, "acceptance-cmd", "", "fix/general: shell command (one line, max 1 KB) run in the worktree after claude exits 0; non-zero fails the job (needs config acceptance_allowed)")
	fs.BoolVar(&r.OwnerWaits, "owner-waits", false, "the owner asked for this job: its done line says «owner waits» and the Stop hook holds the session until owner_replied")
	fs.StringVar(&r.Size, "size", "", "small|large (Wave F): fix and general jobs declare it, required when config size_required is on; "+
		"large needs covers, which only job_start takes")
	r.SizeRule = true
	skillEdits := fs.String("skills", "", "edits to the default skills, comma separated: name or +name adds, -name removes (e.g. a,+b,-c)")
	var sources multi
	fs.Var(&sources, "source", "disabled until OS-level isolation lands (G02 deferred): any value is refused")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if len(sources) > 0 {
		return jobs.ErrSourcesDisabled // refused before the store is opened (Enqueue refuses it too)
	}
	r.Skills = skills.ParseEdits(*skillEdits)
	if len(sources) > 0 {
		r.Sources = sources
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
	if w := m.Cfg.BriefSizeWarning(r.Brief); w != "" {
		fmt.Fprintln(os.Stderr, "size_warning:", w)
	}
	if n := m.SameRepoNote(j); n != "" {
		fmt.Fprintln(os.Stderr, "note:", n)
	}
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
		if err := printQueue(m); err != nil {
			return err
		}
		if err := printDaily(m); err != nil {
			return err
		}
		if err := printMissedChecks(m); err != nil {
			return err
		}
		if err := printHeartbeat(m); err != nil {
			return err
		}
		printHTTPAPI(m)
		for _, w := range m.SkillWarnings() {
			fmt.Println("skills warning:", shown(w))
		}
		if err := printUndelivered(m); err != nil {
			return err
		}
		return printOwnerPending(m)
	}
	j, err := m.Store.Get(pos[0])
	if err != nil {
		return err
	}
	fmt.Printf("id:        %s\ntitle:     %s\nkind:      %s\nstatus:    %s\nmodel:     %s\n", j.ID, j.Title, j.Kind, j.Status, j.Model)
	if j.ReasoningEffort != "" {
		fmt.Printf("effort:    %s\n", j.ReasoningEffort)
	}
	fmt.Printf("limits:    %s\n", jobs.LimitsText(j))
	fmt.Printf("usage:     %d turns · %s · %ds\n", j.Turns, costWord(j.CostUSD, j.CostIsUnknown()), j.DurationSec)
	fmt.Printf("repo:      %s\nbase:      %s\npid/pgid:  %d/%d\n", j.RepoPath, j.BaseCommit, j.PID, j.PGID)
	fmt.Printf("created:   %s\nstarted:   %s\nfinished:  %s\nnotified:  %s\ncleaned:   %s\n",
		fmtTime(&j.CreatedAt), fmtTime(j.StartedAt), fmtTime(j.FinishedAt), fmtTime(j.NotifiedAt), fmtTime(j.CleanedAt))
	fmt.Printf("session:   %s (attempt %d, %s)\n", j.SessionID, j.Attempt, j.RunMode)
	if c := jobs.Containment(j); c != "" {
		fmt.Printf("contain:   %s\n", c)
	}
	if len(j.Skills) > 0 {
		fmt.Printf("skills:    %s (linked now: %s)\n", strings.Join(j.Skills, ", "), strings.Join(j.SkillsDelivered, ", "))
		if j.SkillsNote != "" {
			fmt.Printf("           %s\n", j.SkillsNote)
		}
	}
	if len(j.Sources) > 0 {
		fmt.Printf("sources:   %s\n", strings.Join(j.Sources, ", "))
	}
	fmt.Printf("dir:       %s\n", m.JobDir(j.ID))
	if j.AcceptanceCmd != "" {
		fmt.Printf("accept:    %s (%s)\n", j.AcceptanceCmd, acceptanceWord(j))
	}
	if j.Error != "" {
		fmt.Printf("error:     %s\n", j.Error)
	}
	return nil
}

// printQueue is `status` without a job id: running and queued jobs and max_running as config.json
// says now (the daemon uses the value it read at its start).
func printQueue(m *jobs.Manager) error {
	q, err := m.Queue()
	if err != nil {
		return err
	}
	fmt.Printf("queue:     %d running, %d queued (configured max_running %d)\n", q.Running, q.Queued, q.ConfiguredMaxRunning)
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

// printHeartbeat is the heartbeat part of `status` without a job id: on/off, the gate, wakes today,
// the next eligible time and the last decision.
func printHeartbeat(m *jobs.Manager) error {
	st, err := m.Heartbeat()
	if err != nil {
		return err
	}
	loc := m.Location()
	fmt.Printf("heartbeat: %s\n", jobs.HeartbeatText(st))
	if st.Enabled {
		fmt.Printf("goals:     %s\n", st.GoalsFile)
		if st.LastGate != "" {
			fmt.Printf("gate:      %s (%s)\n", st.LastGate, st.LastGateAt.In(loc).Format(time.DateTime))
		}
	}
	if st.Last != nil {
		fmt.Printf("decision:  %s\n", jobs.HeartbeatDecisionText(st.Last, loc))
	}
	return nil
}

// printHTTPAPI is the http api line of `status` without a job id, when http_api is enabled: where
// it listens, or "http api not serving: <reason>".
func printHTTPAPI(m *jobs.Manager) {
	c, ok := m.HTTPAPICheck()
	switch {
	case !ok:
	case c.OK:
		fmt.Printf("http api:  %s\n", c.Detail)
	default:
		fmt.Println(c.Detail)
	}
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
// passed its grace without a mark (a late mark shown, the time stays missed) and the missed
// alerts still pending, with their delivery state.
func printMissedChecks(m *jobs.Manager) error {
	list, err := m.MissedChecks()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("checks:    no missed checks")
		return nil
	}
	fmt.Printf("checks:    %d missed or alert pending\n", len(list))
	for _, v := range list {
		fmt.Printf("  %s · last mark %s\n", jobs.CheckViewText(v), fmtTime(v.LastMarkAt))
	}
	return nil
}

// printOwnerPending is the very end of `status` without a job id: owner-waits jobs whose result the
// owner has not been told yet (they hold the Stop hook until owner_replied or they expire), and the
// orchestrator session the Stop hook holds.
func printOwnerPending(m *jobs.Manager) error {
	list, _, err := m.OwnerPending()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("owner:     no owner replies pending")
	} else {
		fmt.Printf("owner:     %d owner replies pending (expire after %s; owner_replied clears them)\n", len(list), m.Cfg.OwnerWaitsMaxAge())
		for _, p := range list {
			fmt.Printf("  job %s %s, line %s, since %s: %s\n", p.JobID, p.Status, p.State, fmtTime(&p.CreatedAt),
				jobs.RedactPrintable(p.Title, jobs.MaxTitleRunes))
		}
	}
	session, at, err := m.Store.HookSession()
	if err != nil {
		return err
	}
	if session == "" {
		fmt.Println("hook:      no orchestrator session bound (hook bind not run): the Stop hook holds no session")
		return nil
	}
	fmt.Printf("hook:      the Stop hook holds session %s (bound %s)\n", jobs.Printable(session, 64), fmtTime(&at))
	return nil
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	agent := agentFlag(fs)
	limit := fs.Int("limit", 20, "max rows")
	status := fs.String("status", "", "only jobs with this status")
	asJSON := fs.Bool("json", false, "print the list as JSON (the HTTP API's /v1/jobs view)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	if *asJSON {
		out, err := httpapi.ListJobs(context.Background(), m, *status, *limit)
		if err != nil {
			return err
		}
		return printJSON(out)
	}
	var list []*store.Job
	if *status != "" {
		list, err = m.Store.ListRecentByStatus(*status, *limit)
	} else {
		list, err = m.Store.List(*limit)
	}
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

// acceptanceWord is how a job's acceptance command ended: not run, passed, timeout or exit N.
func acceptanceWord(j *store.Job) string {
	switch {
	case j.AcceptanceExit == nil:
		return "not run"
	case *j.AcceptanceExit == 0:
		return fmt.Sprintf("passed in %ds", j.AcceptanceDurationSec)
	case *j.AcceptanceExit == store.AcceptanceTimedOut:
		return fmt.Sprintf("timeout after %ds", j.AcceptanceDurationSec)
	}
	return fmt.Sprintf("exit %d after %ds", *j.AcceptanceExit, j.AcceptanceDurationSec)
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
	fmt.Println(m.DoneLineFor(j))
	if j.Error != "" {
		fmt.Println("error:", jobs.RedactText(j.Error))
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
	// The bounded reader of job_result: never through a link, never a FIFO or another non-regular
	// file, at most the scan cap.
	text, truncated, err := mcp.ReadJobReport(m.StateDir, id, jobs.MaxRedactBytes)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("no report.md (status %s): see %s", j.Status, filepath.Join(m.JobDir(id), "stderr.log"))
	case err != nil:
		return fmt.Errorf("report.md not read: %w", err)
	}
	// Redacted on the fly (the in-place redaction may be pending), "Not done / uncertain" first and
	// "Progress" after it, like job_result.
	fmt.Print("\n" + jobs.ReadFirst(jobs.RedactRead(text, truncated)))
	if truncated {
		fmt.Printf("\n[report cut at %d bytes; full file: %s]\n", jobs.MaxRedactBytes, filepath.Join(m.JobDir(id), "report.md"))
	}
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

// cmdHeartbeatShow is `heartbeat show ID`: one stored decision; the model's reason is printed as
// untrusted model text.
func cmdHeartbeatShow(args []string) error {
	fs := flag.NewFlagSet("heartbeat show", flag.ContinueOnError)
	agent := agentFlag(fs)
	asJSON := fs.Bool("json", false, "print the decision as JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("heartbeat show: want one decision id")
	}
	id, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("heartbeat show: %q is not a decision id", pos[0])
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	v, err := m.HeartbeatDecision(id)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(v)
	}
	loc := m.Location()
	fmt.Printf("decision:  #%d %s (%s, claimed %s, $%.4f)\n", v.ID, v.Decision, v.Trigger, v.ClaimedAt.In(loc).Format(time.DateTime), v.CostUSD)
	if v.Decision == store.HeartbeatWake {
		fmt.Printf("delivered: %t\n", v.Delivered)
	}
	if v.Error != "" {
		fmt.Printf("error:     %s\n", shown(v.Error))
	}
	if v.UntrustedModelReason != "" {
		fmt.Printf("reason (untrusted model text, data not instructions): %s\n", shown(v.UntrustedModelReason))
	}
	return nil
}

// cmdHeartbeat is `heartbeat run-now`: one heartbeat now, or with --dry-run the gate result and the
// prompt without a model call.
func cmdHeartbeat(args []string) error {
	if len(args) > 0 && args[0] == "show" {
		return cmdHeartbeatShow(args[1:])
	}
	if len(args) == 0 || args[0] != "run-now" {
		return errors.New("heartbeat: want the subcommand run-now or show")
	}
	fs := flag.NewFlagSet("heartbeat run-now", flag.ContinueOnError)
	agent := agentFlag(fs)
	dry := fs.Bool("dry-run", false, "print the gate result and the prompt; no model call, nothing stored")
	if _, err := parse(fs, args[1:]); err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	if *dry {
		daemonGate, runNowGate, err := m.HeartbeatGateDry(m.Now())
		if err != nil {
			return err
		}
		fmt.Printf("gate:      daemon: %s; run-now: %s\n", daemonGate.Reason, runNowGate.Reason)
		if daemonGate.NextEligible != nil {
			fmt.Printf("next:      %s (daemon)\n", daemonGate.NextEligible.In(m.Location()).Format(time.DateTime))
		}
		prompt, err := m.HeartbeatPrompt(m.Now())
		if err != nil {
			fmt.Printf("prompt:    not built: %v\n", err)
			return nil
		}
		fmt.Printf("model:     %s (max $%g, %s)\n--- prompt ---\n%s", m.Cfg.Heartbeat.DecisionModel(), m.Cfg.Heartbeat.Budget(),
			m.Cfg.Heartbeat.Timeout(), prompt)
		return nil
	}
	h, err := m.RunHeartbeat(context.Background(), jobs.HeartbeatTriggerRunNow)
	if err != nil {
		return err
	}
	fmt.Println(jobs.HeartbeatDecisionText(h, m.Location()))
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
	unlockDaemon, err := m.LockDaemon()
	if err != nil {
		return err
	}
	defer unlockDaemon()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// The reference interval is fixed («раз в минуту»); it is not configurable.
	m.Log.Printf("daemon started, interval %s, state %s", jobs.TickInterval, m.StateDir)
	// What the daemon runs: the orchestrator's MCP server compares itself with it (job_start, system_doctor).
	if err := m.WriteDaemonInfo(); err != nil {
		m.Log.Printf("%v (MCP servers cannot compare their version with the daemon's)", err)
	}
	m.WatchDaemonConfig()
	// The merge hook (F9 (b)) matches a gated repository by its stored names once its work tree is gone:
	// store them for approvals recorded before names were.
	if err := m.BackfillGateNames(); err != nil {
		m.Log.Printf("merge gate names: %v (approvals without stored names stay unsure for merges by name)", err)
	}
	// The brain proxy: jobs reach shared memory through the daemon and never hold the token.
	go func() {
		if err := m.ServeBrain(ctx); err != nil {
			m.Log.Printf("%v (jobs get no memory tools and no notify_owner until the daemon restarts)", err)
		}
	}()
	// The read-only HTTP API (off by default): served until ctx ends, waited for before the store closes.
	httpDone := make(chan struct{})
	go func() {
		defer close(httpDone)
		if !m.Config().HTTPAPI.Enabled {
			return
		}
		if err := httpapi.Serve(ctx, m, version); err != nil {
			m.Log.Printf("%v (no HTTP API until the daemon restarts)", err)
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
		// Heartbeat (off by default): the Go gate, then at most one small-model decision in the
		// background, so a slow decision never delays the next tick.
		m.StartHeartbeat(ctx)
		select {
		case <-ctx.Done():
			// Jobs run in their own sessions: stopping the daemon does not touch them. A daily run
			// in progress stops between jobs (ctx) and is waited for before the store closes.
			m.Log.Printf("daemon stopping; running jobs keep running")
			m.WaitDaily()
			m.WaitHeartbeat()
			<-httpDone
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

// cmdGate prints a gate job's stored result (Wave F): `gate slicing` (slicing_get), `gate briefing`
// (briefing_get) or `gate verdict` (judge_get); `gate release-check` prints release_check (F7) and exits
// non-zero unless it passed; `gate retire` takes a repository out of the merge gate (F9 (b)).
func cmdGate(args []string) error {
	if len(args) > 0 && args[0] == "release-check" {
		return cmdReleaseCheck(args[1:])
	}
	if len(args) > 0 && args[0] == "retire" {
		return cmdGateRetire(args[1:])
	}
	if len(args) == 0 || (args[0] != "slicing" && args[0] != "briefing" && args[0] != "verdict") {
		return errors.New("usage: agent-jobs gate slicing|briefing|verdict --agent A --repo PATH [--sha S] [--checklist-id ID] | " +
			"gate release-check --agent A --repo PATH --sha S | gate retire --agent A REPO")
	}
	fs := flag.NewFlagSet("gate "+args[0], flag.ContinueOnError)
	agent := agentFlag(fs)
	repo := fs.String("repo", "", "absolute path of the project's git work tree")
	id := fs.String("checklist-id", "", "default: the id of the repository's checklist")
	sha := ""
	if args[0] == "verdict" {
		fs.StringVar(&sha, "sha", "", "the verdict for this commit (default: the latest of any commit)")
	}
	if _, err := parse(fs, args[1:]); err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	if args[0] == "slicing" {
		out, err := mcp.SlicingGet(m, *repo, *id)
		if err != nil {
			return err
		}
		return printJSON(out)
	}
	if args[0] == "briefing" {
		out, err := mcp.BriefingGet(m, *repo, *id)
		if err != nil {
			return err
		}
		return printJSON(out)
	}
	out, err := mcp.JudgeGet(m, *repo, *id, sha)
	if err != nil {
		return err
	}
	return printJSON(out)
}

// errReleaseFailed is `gate release-check`'s error when the check ran and did not pass (the result is
// printed first).
var errReleaseFailed = errors.New("release check did not pass")

// cmdReleaseCheck is `agent-jobs gate release-check`: release_check as JSON, then the owner line on stderr.
func cmdReleaseCheck(args []string) error {
	fs := flag.NewFlagSet("gate release-check", flag.ContinueOnError)
	agent := agentFlag(fs)
	repo := fs.String("repo", "", "absolute path of the project's git work tree")
	sha := fs.String("sha", "", "the full commit id to ship")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	out, err := mcp.ReleaseCheck(m, *repo, *sha)
	if err != nil {
		return err
	}
	if err := printJSON(out); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, out.OwnerLine)
	if !out.Pass {
		return errReleaseFailed
	}
	return nil
}

// cmdGateRetire is `agent-jobs gate retire --agent A REPO`, the owner's way to take a repository out of the
// merge gate (a moved or deleted work tree, a repository no longer worked on). It is a CLI command only:
// no MCP tool reaches it, inside a job the CLI refuses every command, and the merge hook refuses it in the
// orchestrator session's Bash (jobs.RetireRefusal): the owner runs it in a terminal outside the session.
func cmdGateRetire(args []string) error {
	fs := flag.NewFlagSet("gate retire", flag.ContinueOnError)
	agent := agentFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: agent-jobs gate retire --agent A REPO (the repository's absolute path)")
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	repo, err := m.RetireGate(pos[0])
	if err != nil {
		return err
	}
	fmt.Printf("retired %s from the merge gate; a new checklist approval gates it again\n", repo)
	return nil
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
	fmt.Fprintln(w, "NAME\tSTATE\tCRON\tGRACE\tEXPECTED\tNEXT\tLAST MARK\tMARKER\tALLOWED SCHEDULE IDS\tUNTRUSTED_NOTE")
	for _, v := range list {
		exp, next := v.LastExpectedAt, v.NextExpectedAt
		if exp == "" {
			exp = "-"
		}
		if next == "" {
			next = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%dm\t%s\t%s\t%s\t%s\t%v\t%q\n", v.Name, jobs.CheckStateText(v), v.Cron, v.GraceMinutes, exp, next,
			fmtTime(v.LastMarkAt), v.LastMarkJob, v.AllowedScheduleIDs, jobs.RedactPrintable(v.LastMarkUntrustedNote, 60))
	}
	return w.Flush()
}

// cmdCheckEdit configures checks through the same owner-only manager API as MCP.
func cmdCheckEdit(op string, args []string) error {
	fs := flag.NewFlagSet("check "+op, flag.ContinueOnError)
	agent := agentFlag(fs)
	cron := fs.String("cron", "", "cron expression in local time")
	grace := fs.Int("grace-minutes", 0, "grace in minutes (default 60 on add)")
	enabled := fs.Bool("enabled", true, "enable or pause the check (set only)")
	allowed := fs.String("allowed-schedule-ids", "", "comma-separated permitted schedule ids; empty permits every job")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("exactly one check name expected")
	}
	var policies [][]int64
	var en *bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "allowed-schedule-ids" {
			policies = append(policies, []int64{})
		}
		if f.Name == "enabled" {
			en = enabled
		}
	})
	if len(policies) > 0 && *allowed != "" {
		for _, part := range strings.Split(*allowed, ",") {
			id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
			if err != nil || id <= 0 {
				return errors.New("allowed-schedule-ids must be comma-separated positive schedule ids")
			}
			policies[0] = append(policies[0], id)
		}
	}
	if op == "add" && en != nil {
		return errors.New("--enabled is supported by check set only")
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	var c *store.Check
	if op == "add" {
		c, err = m.AddCheck(pos[0], *cron, *grace, policies...)
	} else {
		c, err = m.SetCheck(pos[0], *cron, *grace, en, policies...)
	}
	if err != nil {
		return err
	}
	v, err := m.CheckViewOf(c)
	if err != nil {
		return err
	}
	return printJSON(mcp.CheckOutput{Check: v})
}

func cmdCheck(args []string) error {
	if len(args) > 0 && (args[0] == "add" || args[0] == "set") {
		return cmdCheckEdit(args[0], args[1:])
	}
	if len(args) == 0 || args[0] != "mark" {
		return errors.New("usage: agent-jobs check {add|set|mark} --agent A NAME [options]")
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
	if c.MarkResult == store.MarkNotStored {
		fmt.Printf("check %s: %s (its window already holds %d marks; this one was not stored)\n",
			c.Name, store.MarkNotStored, store.MaxMarksPerWindow)
		return nil
	}
	fmt.Printf("check %s marked at %s by %s\n", c.Name, fmtTime(c.LastMarkAt), c.LastMarkJob)
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
	by := fs.String("by", "kind", "group by kind|model|skill|effort|repo (skill,repo: the skills rating by repository)")
	byRepo := fs.Bool("by-repo", false, "with --by skill: the skills rating with a per-repository breakdown")
	repo := fs.String("repo", "", "with --by-repo: only jobs of this repository (folder name)")
	limit := fs.Int("limit", httpapi.DefaultSkillsLimit, "with --by-repo: at most this many skills")
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
	if *by == "skill,repo" {
		*by, *byRepo = "skill", true
	}
	if *byRepo {
		if *by != "skill" {
			return errors.New("--by-repo needs --by skill")
		}
		if *since == "" {
			t = m.Now().Add(-httpapi.DefaultStatsDays * 24 * time.Hour)
		}
		return printSkillRatings(m, t, *repo, *limit, *asJSON)
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
		cost := fmt.Sprintf("$%.2f", s.CostTotalUSD)
		if s.CostEstimatedUSD > 0 {
			cost += " (" + jobs.EstimatedCostText(s.CostEstimatedUSD) + ")"
		}
		perUsable := "-"
		if s.CostPerUsableResultUSD > 0 {
			perUsable = fmt.Sprintf("$%.2f", s.CostPerUsableResultUSD)
		}
		var outcomes []string
		for _, o := range jobs.Outcomes {
			if n := s.Outcomes[o]; n > 0 {
				outcomes = append(outcomes, fmt.Sprintf("%s %.0f%%", o, s.OutcomeShares[o]*100))
			}
		}
		if len(outcomes) == 0 {
			outcomes = []string{"-"}
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%.0f%%\t%s\t$%.2f\t$%.2f\t%d\t%ds\t%ds\t%d\t%d\t%s\t%d\t%s\n", key, s.Jobs,
			s.ByStatus[store.StatusSucceeded], s.SuccessRate*100, cost, s.CostMedianUSD, s.CostP90USD,
			s.CostUnknownJobs, s.DurationMedianSec, s.DurationP90Sec, s.GuardRefusals, s.ReportOffContract,
			strings.Join(outcomes, ", "), s.Unrecorded, perUsable)
	}
	// COST, MEDIAN and P90 are over the jobs with a known or estimated cost ("est." is the part priced
	// from token usage); COST? counts the others. OFF-CONTRACT counts the reports without a Summary or a
	// Not done / uncertain section. $/USABLE is the cost per accepted or extended result.
	fmt.Fprintln(w, strings.ToUpper(rep.By)+"\tJOBS\tOK\tRATE\tCOST\tMEDIAN\tP90\tCOST?\tDUR MEDIAN\tDUR P90\tREFUSALS\tOFF-CONTRACT\tOUTCOMES\tUNRECORDED\t$/USABLE")
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
		fmt.Println("\nskills (uses, jobs, success rate, delivered, delivered unused):")
		for _, s := range rep.Skills {
			fmt.Printf("  %-22s %d  %d  %.0f%%  %d  %d\n", shown(s.Name), s.Uses, s.Jobs, s.SuccessRate*100, s.Delivered, s.DeliveredUnused)
		}
	}
	return nil
}

// printSkillRatings is `stats --by skill --by-repo`: the skills rating (GET /v1/skills).
func printSkillRatings(m *jobs.Manager, since time.Time, repo string, limit int, asJSON bool) error {
	rep, err := m.SkillRatings(context.Background(), since, repo, limit)
	if err != nil {
		return err
	}
	if asJSON {
		return printJSON(rep)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "SKILL\tSTATUS\tDELIVERED\tUSED\tRATE\tMEDIAN COST\tLAST USED\tSTALE\tBY REPO")
	for _, s := range rep.Skills {
		var parts []string
		for _, r := range s.ByRepo {
			parts = append(parts, fmt.Sprintf("%s %d (%.0f%%)", shown(r.Repo), r.Used, r.SuccessRate*100))
		}
		last := s.LastUsed
		if last == "" {
			last = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%.0f%%\t$%.2f\t%s\t%t\t%s\n", shown(s.Name), s.Status, s.Delivered, s.Used,
			s.SuccessRate*100, s.MedianCostUSD, last, s.Stale, strings.Join(parts, ", "))
	}
	return w.Flush()
}

func cmdSkills(args []string) error {
	if len(args) == 0 || (args[0] != "list" && args[0] != "search") {
		return errors.New("usage: agent-jobs skills list|search --agent A [QUERY] [--kind K] [--repo R] [--status S] [--json]")
	}
	sub := args[0]
	fs := flag.NewFlagSet("skills "+sub, flag.ContinueOnError)
	agent := agentFlag(fs)
	var f skills.Filter
	fs.StringVar(&f.Kind, "kind", "", "a job kind or a catalog kind")
	fs.StringVar(&f.Repo, "repo", "", "repository path or folder name (skills whose projects include it)")
	fs.StringVar(&f.Status, "status", "", "core|active|draft|deprecated (default core and active)")
	asJSON := fs.Bool("json", false, "print as JSON (same as skill_list)")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	query := strings.Join(pos, " ")
	switch {
	case sub == "search" && strings.TrimSpace(query) == "":
		return errors.New("skills search needs a QUERY")
	case sub == "list" && len(pos) > 0:
		return errors.New("skills list takes no QUERY (use skills search)")
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	out, err := m.SkillList(context.Background(), query, f)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out)
	}
	for _, w := range out.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", shown(w))
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	head := "NAME\tSTATUS\tKB\tUSES\tRATE\tSTALE\tDESCRIPTION"
	if sub == "search" {
		head = "SCORE\t" + head
	}
	fmt.Fprintln(w, head)
	for _, s := range out.Skills {
		if sub == "search" {
			fmt.Fprintf(w, "%.2f\t", s.Score)
		}
		fmt.Fprintf(w, "%s\t%s\t%.0f\t%d\t%.0f%%\t%t\t%s\n", shown(s.Name), s.Status, s.SizeKB, s.Uses, s.SuccessRate*100,
			s.Stale, jobs.Printable(s.Description, 100))
	}
	return w.Flush()
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
	for _, id := range rep.PendingContainers {
		fmt.Printf("containers %s  sandbox containers not removed yet (containers.pending; retried every tick)\n", id)
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

// silentInsideJob reports whether a command refused inside a job ends there silently with exit 0: the
// brief reminder and the skills picker always exit 0, and the merge gate is a no-op in a job (a job never
// merges); they end before any state is opened.
func silentInsideJob(cmd string, args []string) bool {
	return cmd == "hook" && len(args) > 0 && (args[0] == "brief-reminder" || args[0] == "merge-gate" || args[0] == "skills-picker")
}

// runHook serves the hooks of the orchestrator session (Claude Code hook JSON on stdin):
//
//	hook bind --agent A   SessionStart hook: binds this session as the one the Stop hook holds, only
//	                      from the notify tmux pane ($TMUX_PANE) with its cwd inside orchestrator_dir
//	hook stop --agent A   Stop hook: prints the block decision while an owner reply is pending, for
//	                      the bound session only
//	hook brief-reminder --agent A
//	                      UserPromptSubmit hook: prints one reminder line (the size rule, whether the
//	                      repo at the session's cwd has an approved checklist), for the bound session only
//	hook merge-gate --agent A
//	                      PreToolUse hook on Bash: blocks gh pr merge in a repo with an approved checklist
//	                      unless release_check passed on its --match-head-commit sha (mergeGateHook)
//	hook skills-picker    PreToolUse hook on milestone_create and job_start: adds context asking for the
//	                      tasks' skills once per milestone (skillsPickerHook); always exits 0
//
// Both fail open: any error (bad or incomplete input, no store, a job session) exits 1 without
// output, a non-blocking hook error (exit 2 would block); a failed bind keeps the old binding. The
// brief reminder is softer still: it always exits 0, an error only goes to stderr, and a job context
// (main's job-role refusal, a job session) stays silent. It reads the store read-only (openReadOnly).
func runHook(args []string, stdin io.Reader, stdout, stderr io.Writer, opener func(string) (*jobs.Manager, error)) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if sub == "merge-gate" {
		return mergeGateHook(args[1:], stdin, stdout, stderr, openReadOnly)
	}
	if sub == "skills-picker" {
		return skillsPickerHook(args[1:], stdin, stdout, stderr)
	}
	if sub == "brief-reminder" {
		type answer struct {
			line string
			err  error
		}
		done := make(chan answer, 1)
		go func() {
			line, err := briefReminder(args[1:], stdin, openReadOnly)
			done <- answer{line, err}
		}()
		var a answer
		select {
		case a = <-done:
		case <-time.After(briefReminderTimeout):
			a.err = fmt.Errorf("no answer within %s", briefReminderTimeout)
		}
		switch {
		case errors.Is(a.err, jobs.ErrInJobSession):
			// A job session: silent, like the job-role refusal in main.
		case a.err != nil:
			fmt.Fprintln(stderr, "agent-jobs hook brief-reminder:", a.err, "(no reminder)")
		case a.line != "":
			if _, err := fmt.Fprintln(stdout, a.line); err != nil {
				fmt.Fprintln(stderr, "agent-jobs hook brief-reminder:", err)
			}
		}
		return 0
	}
	fail := func(err error) int {
		if sub == "bind" {
			fmt.Fprintln(stderr, "agent-jobs hook bind:", err, "(no session bound)")
		} else {
			fmt.Fprintln(stderr, "agent-jobs hook stop:", err, "(the stop is allowed)")
		}
		return 1
	}
	if sub != "stop" && sub != "bind" {
		return fail(errors.New("usage: agent-jobs hook stop|bind|brief-reminder|merge-gate|skills-picker --agent A (Claude Code Stop / " +
			"SessionStart / UserPromptSubmit / PreToolUse hook, JSON on stdin)"))
	}
	fs := flag.NewFlagSet("hook "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	agent := agentFlag(fs)
	pos, err := parse(fs, args[1:])
	if err != nil {
		return fail(err)
	}
	if len(pos) > 0 {
		return fail(errors.New("no positional arguments"))
	}
	if sub == "bind" {
		in, err := jobs.ParseSessionStartInput(stdin)
		if err != nil {
			return fail(err)
		}
		m, err := opener(*agent)
		if err != nil {
			return fail(err)
		}
		defer m.Close()
		// Nothing on stdout: a SessionStart hook's stdout becomes session context.
		if err := m.BindHookSession(in, os.Getenv("TMUX_PANE")); err != nil {
			return fail(err)
		}
		return 0
	}
	in, err := jobs.ParseStopHookInput(stdin)
	if err != nil {
		return fail(err)
	}
	if jobs.StopHookSkips(in) {
		return 0
	}
	m, err := opener(*agent)
	if err != nil {
		return fail(err)
	}
	defer m.Close()
	if holds, err := m.StopHookHolds(in.SessionID); err != nil || !holds {
		if err != nil {
			return fail(err)
		}
		return 0
	}
	pending, _, err := m.OwnerPending()
	if err != nil {
		return fail(err)
	}
	out := jobs.StopHookDecision(in, pending)
	if out == nil {
		return 0
	}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		return fail(err)
	}
	return 0
}

// mergeGateTimeout bounds the merge gate (store reads, git, checklist files): past it the call is blocked.
const mergeGateTimeout = 5 * time.Second

// mergeGateHook is `hook merge-gate` (Wave F, F9 (b)): exit 0 allows the Bash call, exit 2 blocks it with
// one line on stderr (Claude Code shows it to the model). A call that does not run `gh pr merge` (and
// cannot) is allowed before the store is opened; for one that does or may, every error blocks (fail
// closed): bad flags, an unreadable input, the store, a timeout. A job session (ErrInJobSession) is a
// no-op, like the job-role refusal in main. A merge it cannot read runs (exit 0) with a warning as hook
// JSON on stdout (mergeGateWarningJSON).
func mergeGateHook(args []string, stdin io.Reader, stdout, stderr io.Writer, opener func(string) (*jobs.Manager, error)) int {
	block := func(reason string) int {
		fmt.Fprintln(stderr, reason)
		return 2
	}
	fs := flag.NewFlagSet("hook merge-gate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	agent := agentFlag(fs)
	pos, flagErr := parse(fs, args)
	if flagErr == nil && len(pos) > 0 {
		flagErr = errors.New("no positional arguments")
	}
	in, err := jobs.ParsePreToolUseInput(stdin)
	if err != nil {
		return block(jobs.MergeGatePrefix + "unreadable hook input (" + jobs.Printable(err.Error(), 200) + ")")
	}
	if r := jobs.RetireRefusal(in.Command); in.ToolName == "Bash" && r != "" {
		return block(r)
	}
	if in.ToolName != "Bash" || !jobs.MergeGateNeeded(in.Command) {
		return 0
	}
	if flagErr != nil {
		return block(jobs.MergeGatePrefix + "the hook is misconfigured (" + flagErr.Error() + "); register it as " +
			"agent-jobs hook merge-gate --agent A")
	}
	type answer struct {
		reason  string
		warning string
		err     error
	}
	done := make(chan answer, 1)
	go func() {
		m, err := opener(*agent)
		if err != nil {
			done <- answer{"", "", err}
			return
		}
		defer m.Close()
		r, err := m.MergeGateCheck(in.Cwd, in.Command, os.Getenv("GH_REPO"))
		done <- answer{r.Refusal, r.Warning, err}
	}()
	var a answer
	select {
	case a = <-done:
	case <-time.After(mergeGateTimeout):
		a.err = fmt.Errorf("no answer within %s", mergeGateTimeout)
	}
	switch {
	case errors.Is(a.err, jobs.ErrInJobSession):
		return 0
	case a.err != nil:
		return block(jobs.MergeGatePrefix + "cannot check the merge (" + jobs.RedactPrintable(a.err.Error(), 300) + ")")
	case a.reason != "":
		return block(a.reason)
	case a.warning != "":
		// The command runs (exit 0). Claude Code drops the stderr of a hook that exits 0, so the warning is
		// hook JSON on stdout (mergeGateWarningJSON).
		if err := json.NewEncoder(stdout).Encode(mergeGateWarningJSON(a.warning)); err != nil {
			fmt.Fprintln(stderr, a.warning) // the transcript still has it; the warning never blocks
		}
	}
	return 0
}

// preToolUseOutput is the PreToolUse hook JSON the merge hook prints with a warning.
type preToolUseOutput struct {
	HookSpecificOutput preToolUseSpecific `json:"hookSpecificOutput"`
	SystemMessage      string             `json:"systemMessage"`
}

type preToolUseSpecific struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// mergeGateWarningJSON is the hook output for a merge the gate cannot read: hookSpecificOutput's
// additionalContext is added to the model's context (the form examples/hooks/prefer-jobs-reminder.sh
// uses, docs/WORKFLOW.md), systemMessage shows the same line to the user. It carries no
// permissionDecision: "allow" would skip the session's permission prompts for the command, and its
// permissionDecisionReason is shown to the user only, not to the model.
func mergeGateWarningJSON(warning string) preToolUseOutput {
	return preToolUseOutput{
		HookSpecificOutput: preToolUseSpecific{HookEventName: "PreToolUse", AdditionalContext: warning},
		SystemMessage:      warning,
	}
}

// briefReminderTimeout bounds the whole brief reminder (store reads, git, checklist files): past it the
// prompt goes on without a reminder.
const briefReminderTimeout = 2 * time.Second

// briefReminder is `hook brief-reminder`: the reminder line (UserPromptSubmit stdout becomes context),
// "" for a session other than the bound one.
func briefReminder(args []string, stdin io.Reader, opener func(string) (*jobs.Manager, error)) (string, error) {
	fs := flag.NewFlagSet("hook brief-reminder", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	agent := agentFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return "", err
	}
	if len(pos) > 0 {
		return "", errors.New("no positional arguments")
	}
	in, err := jobs.ParseUserPromptInput(stdin)
	if err != nil {
		return "", err
	}
	m, err := opener(*agent)
	if err != nil {
		return "", err
	}
	defer m.Close()
	return m.BriefReminder(in)
}

// skillsPickerHook is `hook skills-picker` (G01): the picker's context as PreToolUse additionalContext,
// nothing when it has none. It reads only its input (no state; --agent is accepted and not needed) and
// always exits 0: an error goes to stderr and the call goes on.
func skillsPickerHook(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hook skills-picker", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	agentFlag(fs)
	pos, err := parse(fs, args)
	if err == nil && len(pos) > 0 {
		err = errors.New("no positional arguments")
	}
	var msg string
	if err == nil {
		msg, err = jobs.SkillsPicker(stdin)
	}
	if err != nil {
		fmt.Fprintln(stderr, "agent-jobs hook skills-picker:", jobs.Printable(err.Error(), 300), "(no reminder)")
		return 0
	}
	if msg == "" {
		return 0
	}
	type specific struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	}
	b, err := json.Marshal(struct {
		HookSpecificOutput specific `json:"hookSpecificOutput"`
	}{specific{"PreToolUse", msg}})
	if err == nil {
		_, err = fmt.Fprintln(stdout, string(b))
	}
	if err != nil {
		fmt.Fprintln(stderr, "agent-jobs hook skills-picker:", err)
	}
	return 0
}

// openReadOnly is open for the brief reminder: the existing state for reads only (jobs.OpenReadOnly),
// never created or migrated, with the same job-session exclusion.
func openReadOnly(agent string) (*jobs.Manager, error) {
	if agent == "" {
		return nil, errors.New("--agent is required")
	}
	m, err := jobs.OpenReadOnly(agent)
	if err != nil {
		return nil, err
	}
	if err := m.CheckNotInJobSession(); err != nil {
		_ = m.Close()
		return nil, err
	}
	return m, nil
}

func cmdHTTPAPI(args []string) error {
	if len(args) == 0 || args[0] != "token" {
		return errors.New("usage: agent-jobs http-api token --agent A --rotate")
	}
	fs := flag.NewFlagSet("http-api token", flag.ContinueOnError)
	agent := agentFlag(fs)
	rotate := fs.Bool("rotate", false, "write a new token (the old one stops working) and print it once")
	if _, err := parse(fs, args[1:]); err != nil {
		return err
	}
	if !*rotate {
		return errors.New("the token is printed only when it is created: agent-jobs http-api token --agent A --rotate")
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	tok, err := httpapi.RotateToken(m.StateDir)
	if err != nil {
		return err
	}
	fmt.Println(tok)
	if !m.Config().HTTPAPI.Enabled {
		fmt.Fprintln(os.Stderr, "note: http_api.enabled is false in config.json; the daemon does not serve the API")
	}
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
