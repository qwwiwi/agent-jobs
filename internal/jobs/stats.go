package jobs

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/guard"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// MaxStreamLineBytes bounds one stream.jsonl line the stats parser looks at; a longer line (a huge
// tool result) is skipped without being held in memory.
const MaxStreamLineBytes = 8 << 20

// StreamStats is what the stats parser takes from one job's stream.
type StreamStats struct {
	ToolCalls     map[string]int
	Skills        map[string]int
	GuardRefusals int
	GuardRules    map[string]int
	Subagents     int // subagent calls (Agent/Task) the guard did not refuse
	ResultSubtype string
	Turns         int
	CostUSD       float64
	Found         bool // a result event was seen
	// Tokens sums the usage of the assistant messages (each message id once, its largest usage) of a
	// claude stream, or of the turn.completed events of a codex stream.
	Tokens TokenUsage
	// TokensInvalid: some usage cannot be real (a negative count, codex cached input above the input,
	// a sum past int64); Tokens is then not priced and not counted.
	TokensInvalid bool
}

// statsUsage is the usage of a claude assistant message or of a codex turn.completed event.
type statsUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CachedInputTokens        int64 `json:"cached_input_tokens"` // codex: part of input_tokens
}

type statsBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

type statsEvent struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	NumTurns     int     `json:"num_turns"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Message      *struct {
		ID      string          `json:"id"`
		Content json.RawMessage `json:"content"`
		Usage   *statsUsage     `json:"usage"`
	} `json:"message"`
	Usage *statsUsage `json:"usage"` // codex turn.completed
}

type statsInput struct {
	Skill    string `json:"skill"`
	Command  string `json:"command"`
	FilePath string `json:"file_path"`
	Path     string `json:"path"`
}

// readLines calls f for every line of r up to max bytes; longer lines are skipped (read through in
// chunks, never held whole), so memory stays bounded whatever the stream holds. It returns the number
// of lines skipped.
func readLines(r io.Reader, max int, f func([]byte)) (skipped int, err error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var buf []byte
	skip := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !skip {
			if len(buf)+len(chunk) > max {
				skip, buf = true, buf[:0]
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case err == nil:
			if !skip {
				f(bytes.TrimRight(buf, "\r\n"))
			} else {
				skipped++
			}
			buf, skip = buf[:0], false
		case errors.Is(err, bufio.ErrBufferFull):
		case errors.Is(err, io.EOF):
			if skip {
				skipped++
			} else if len(buf) > 0 {
				f(buf)
			}
			return skipped, nil
		default:
			return skipped, err
		}
	}
}

// skillFromPath returns the skill a path under a skills/ dir belongs to (skills/<name>/…), "" otherwise.
func skillFromPath(p string) string {
	parts := strings.Split(filepath.ToSlash(p), "/")
	for i := len(parts) - 2; i >= 0; i-- {
		if parts[i] == "skills" && parts[i+1] != "" {
			return strings.TrimSuffix(parts[i+1], ".md")
		}
	}
	return ""
}

// resultText returns the text of a tool_result content (a string or a list of text blocks).
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, x := range blocks {
		b.WriteString(x.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

// ParseStreamStats reads a claude stream-json log (one event per line) and counts tool calls by
// name, skills used, guard refusals by rule and subagents, and takes the last result event.
func ParseStreamStats(r io.Reader) (StreamStats, error) {
	st := StreamStats{ToolCalls: map[string]int{}, Skills: map[string]int{}, GuardRules: map[string]int{}}
	subagentCalls := map[string]bool{}
	// A claude message arrives as several assistant events (one per content block) carrying the same
	// id; its usage grows while it streams, so each id counts once with the largest value of each field.
	byID := map[string]TokenUsage{}
	var anon TokenUsage
	_, err := readLines(r, MaxStreamLineBytes, func(line []byte) {
		var ev statsEvent
		if json.Unmarshal(line, &ev) != nil {
			return
		}
		if ev.Type == "result" {
			st.Found, st.ResultSubtype, st.Turns, st.CostUSD = true, ev.Subtype, ev.NumTurns, ev.TotalCostUSD
			return
		}
		if ev.Type == "turn.completed" && ev.Usage != nil {
			u := ev.Usage
			if u.CachedInputTokens < 0 || u.CachedInputTokens > u.InputTokens {
				st.TokensInvalid = true
				return
			}
			var ok bool
			anon, ok = anon.Add(TokenUsage{Input: u.InputTokens - u.CachedInputTokens, Output: u.OutputTokens,
				CacheRead: u.CachedInputTokens})
			st.TokensInvalid = st.TokensInvalid || !ok
			return
		}
		if ev.Type == "assistant" && ev.Message != nil && ev.Message.Usage != nil {
			u := ev.Message.Usage
			t := TokenUsage{Input: u.InputTokens, Output: u.OutputTokens, CacheRead: u.CacheReadInputTokens,
				CacheWrite: u.CacheCreationInputTokens}
			switch id := ev.Message.ID; {
			case !t.Valid():
				st.TokensInvalid = true
			case id != "":
				o := byID[id]
				byID[id] = TokenUsage{max(o.Input, t.Input), max(o.Output, t.Output), max(o.CacheRead, t.CacheRead),
					max(o.CacheWrite, t.CacheWrite)}
			default:
				var ok bool
				anon, ok = anon.Add(t)
				st.TokensInvalid = st.TokensInvalid || !ok
			}
		}
		if ev.Message == nil || (ev.Type != "assistant" && ev.Type != "user") {
			return
		}
		var blocks []statsBlock
		if json.Unmarshal(ev.Message.Content, &blocks) != nil {
			return
		}
		for _, b := range blocks {
			switch b.Type {
			case "tool_use":
				if b.Name == "" {
					continue
				}
				st.ToolCalls[b.Name]++
				var in statsInput
				_ = json.Unmarshal(b.Input, &in)
				switch {
				case b.Name == "Skill":
					name := in.Skill
					if name == "" {
						name = in.Command
					}
					if name = strings.TrimSpace(name); name != "" {
						st.Skills[name]++
					}
				case guard.IsSubagentTool(b.Name):
					subagentCalls[b.ID] = true
					st.Subagents++
				case b.Name == "Read" || b.Name == "Glob" || b.Name == "Grep":
					p := in.FilePath
					if p == "" {
						p = in.Path
					}
					if s := skillFromPath(p); s != "" {
						st.Skills[s]++
					}
				}
			case "tool_result":
				if !b.IsError {
					continue
				}
				reason := guard.RefusalIn(resultText(b.Content))
				if reason == "" {
					continue
				}
				st.GuardRefusals++
				st.GuardRules[guard.RuleName(reason)]++
				if subagentCalls[b.ToolUseID] {
					st.Subagents--
				}
			}
		}
	})
	st.Tokens = anon
	for _, t := range byID {
		var ok bool
		st.Tokens, ok = st.Tokens.Add(t)
		st.TokensInvalid = st.TokensInvalid || !ok
	}
	return st, err
}

// streamSource opens a job's stream: <jobdir>/stream.jsonl, else the archived stream.jsonl.gz.
// Files are opened without following links. src is "job_dir", "archive" or "none" (nil reader).
func (m *Manager) streamSource(j *store.Job) (rc io.ReadCloser, src string, err error) {
	return m.openStream(j, "")
}

// openStream opens the stream of the current attempt (attempt "") or of the earlier attempt
// attempts/<attempt>/ (resumed jobs): in the job folder, else gzipped in the archive.
func (m *Manager) openStream(j *store.Job, attempt string) (rc io.ReadCloser, src string, err error) {
	rel := "stream.jsonl"
	if attempt != "" {
		if ok, err := realDir(filepath.Join(m.JobDir(j.ID), attemptsDir, attempt), false); err == nil && ok {
			rel = filepath.Join(attemptsDir, attempt, rel)
		} else {
			rel = "" // not in the job folder (pruned): only the archive may have it
		}
	}
	if rel != "" {
		f, ok, err := openRegular(filepath.Join(m.JobDir(j.ID), rel))
		if err != nil {
			return nil, "", err
		}
		if ok {
			return f, "job_dir", nil
		}
	}
	adir := m.archiveJobDir(j)
	if real, err := realDir(adir, false); err != nil || !real {
		return nil, "none", err
	}
	if attempt != "" {
		adir = filepath.Join(adir, attemptsDir, attempt)
		if real, err := realDir(adir, false); err != nil || !real {
			return nil, "none", err
		}
	}
	f, ok, err := openRegular(filepath.Join(adir, "stream.jsonl.gz"))
	if err != nil || !ok {
		return nil, "none", err
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, "", fmt.Errorf("archived stream of %s: %w", j.ID, err)
	}
	return gzReadCloser{zr, f}, "archive", nil
}

type gzReadCloser struct {
	*gzip.Reader
	f *os.File
}

func (g gzReadCloser) Close() error { return errors.Join(g.Reader.Close(), g.f.Close()) }

// hasReport reports whether a non-empty report.md exists in the job folder or the archive. A stop
// report the daemon wrote for an attempt that left none does not count.
func (m *Manager) hasReport(j *store.Job) bool {
	for _, p := range []string{filepath.Join(m.JobDir(j.ID), "report.md"), filepath.Join(m.archiveJobDir(j), "report.md")} {
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
			return !isStopReport(p)
		}
	}
	return false
}

// attemptNumbers lists the earlier attempts kept under root/attempts/<n>/ (real directories only,
// canonical decimal names, 1 <= n < below: the job's current attempt bounds what counts), ascending.
func attemptNumbers(root string, below int) []string {
	ad := filepath.Join(root, attemptsDir)
	if fi, err := os.Lstat(ad); err != nil || !fi.IsDir() {
		return nil
	}
	ents, err := os.ReadDir(ad)
	if err != nil {
		return nil
	}
	var ns []int
	for _, e := range ents {
		n, err := strconv.Atoi(e.Name())
		if err != nil || n < 1 || n >= below || strconv.Itoa(n) != e.Name() || !e.IsDir() {
			continue
		}
		ns = append(ns, n)
	}
	slices.Sort(ns)
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = strconv.Itoa(n)
	}
	return out
}

// RecordStats computes the stats row of a terminal job from its row and its stream (job folder or
// archive) and writes it. The caller holds the lifecycle lock (finish, cancel, stop, prune, backfill).
// A resumed job keeps one row: it is rewritten at the end of every attempt (a stop included) with the
// totals of all attempts so far (the attempts table plus the row; the streams of attempts/<n>/ plus the
// current one), so a resume never counts an attempt twice.
func (m *Manager) RecordStats(j *store.Job) (*store.JobStats, error) {
	if !finished(j.Status) {
		return nil, fmt.Errorf("job %s is %s, not finished", j.ID, j.Status)
	}
	st := &store.JobStats{
		ID: j.ID, Kind: j.Kind, Model: j.Model, Effort: j.ReasoningEffort, Status: j.Status, Turns: j.Turns, CostUSD: j.CostUSD,
		CostUnknown: j.CostIsUnknown(), DurationSec: j.DurationSec, CreatedAt: j.CreatedAt, StartedAt: j.StartedAt,
		FinishedAt: j.FinishedAt,
		ToolCalls:  map[string]int{}, Skills: map[string]int{}, GuardRules: map[string]int{}, HasReport: m.hasReport(j),
		RecordedAt: m.Now(), ReportOffContract: OffContractNote(j.Error) != "", DirBytes: j.DirBytes,
	}
	if j.RepoPath != "" {
		st.Repo = filepath.Base(j.RepoPath)
	}
	st.SkillsDelivered = slices.Clone(j.SkillsDelivered)
	st.Outcome, st.OutcomeNote = j.Outcome, j.OutcomeNote
	earlier, err := m.Store.Attempts(j.ID)
	if err != nil {
		return nil, err
	}
	// The cost is unknown when no attempt reported one (a codex job); a resumed job with a killed
	// attempt still counts the cost its other attempts reported.
	for _, a := range earlier {
		st.Turns += a.Turns
		st.CostUSD += a.CostUSD
		st.CostUnknown = st.CostUnknown && a.CostUnknown
		st.DurationSec += a.DurationSec
	}
	if len(earlier) > 0 && earlier[0].StartedAt != nil {
		st.StartedAt = earlier[0].StartedAt
	}
	parsed := map[string]*StreamStats{} // by attempt ("" = the current one)
	add := func(attempt string) error {
		rc, src, err := m.openStream(j, attempt)
		if err != nil {
			return err
		}
		if attempt == "" {
			st.Source = src
		}
		if rc == nil {
			return nil
		}
		ss, err := ParseStreamStats(rc)
		_ = rc.Close()
		if err != nil {
			return fmt.Errorf("parse stream of %s: %w", j.ID, err)
		}
		parsed[attempt] = &ss
		for k, v := range ss.ToolCalls {
			st.ToolCalls[k] += v
		}
		for k, v := range ss.Skills {
			st.Skills[k] += v
		}
		for k, v := range ss.GuardRules {
			st.GuardRules[k] += v
		}
		st.GuardRefusals += ss.GuardRefusals
		st.Subagents += ss.Subagents
		if !ss.TokensInvalid {
			// The totals saturate rather than wrap; each attempt is priced from its own stream.
			t, _ := TokenUsage{st.InputTokens, st.OutputTokens, st.CacheReadTokens, st.CacheWriteTokens}.Add(ss.Tokens)
			st.InputTokens, st.OutputTokens, st.CacheReadTokens, st.CacheWriteTokens = t.Input, t.Output, t.CacheRead,
				t.CacheWrite
		}
		if attempt == "" {
			st.ResultSubtype = ss.ResultSubtype
		}
		return nil
	}
	if err := add(""); err != nil {
		return nil, err
	}
	attempts := append(attemptNumbers(m.JobDir(j.ID), j.Attempt), attemptNumbers(m.archiveJobDir(j), j.Attempt)...)
	slices.Sort(attempts)
	for _, n := range slices.Compact(attempts) {
		if err := add(n); err != nil {
			return nil, err
		}
	}
	m.estimateCost(st, j, earlier, parsed)
	// The store's counter is what the guard allowed; it outlives the stream.
	if n, err := m.Store.SubagentCount(j.ID); err == nil && n > st.Subagents {
		st.Subagents = n
	}
	redactStats(st)
	if err := m.Store.PutStats(st); err != nil {
		return nil, err
	}
	return st, nil
}

// estimateCost prices, from their streams' token usage at list prices (Config.PriceFor), the attempts
// that ended without a reported cost, and sets the row's CostEstimatedUSD and CostSource: "result" when
// every attempt reported its cost, "estimated" when all the others could be priced, else "unknown" (no
// stream or no token usage left, or a model with no price row). A killed attempt's stream lies in the job dir, which the
// job could write: the estimate is a floor for people, never a billed figure.
func (m *Manager) estimateCost(st *store.JobStats, j *store.Job, earlier []store.Attempt, parsed map[string]*StreamStats) {
	unknown, priced := 0, 0
	var est float64
	price := func(model string, ss *StreamStats) {
		unknown++
		// No stream, or no usage in it (killed before its first message, a codex turn that never
		// completed), or usage that cannot be real: nothing to price, the cost stays unknown rather
		// than a "$0.00 est." or a figure from bad counts.
		if ss == nil || ss.TokensInvalid || ss.Tokens == (TokenUsage{}) {
			return
		}
		if p, ok := m.Config().PriceFor(model); ok {
			est += p.Cost(ss.Tokens)
			priced++
		}
	}
	for _, a := range earlier {
		if a.CostUnknown {
			price(a.Model, parsed[strconv.Itoa(a.N)])
		}
	}
	if j.CostIsUnknown() {
		price(j.Model, parsed[""])
	}
	st.CostEstimatedUSD = math.Round(est*1e4) / 1e4
	switch {
	case unknown == 0:
		st.CostSource = store.CostSourceResult
	case priced == unknown:
		st.CostSource = store.CostSourceEstimated
	default:
		st.CostSource = store.CostSourceUnknown
	}
}

func finished(status string) bool {
	return status != store.StatusQueued && status != store.StatusRunning
}

// recordStatsOf writes the stats row of job id after it reached a terminal status. A failure is
// logged, never fatal: the summary can be backfilled (`agent-jobs stats --backfill`).
func (m *Manager) recordStatsOf(id string) {
	j, err := m.Store.Get(id)
	if err == nil {
		_, err = m.RecordStats(j)
	}
	if err != nil {
		m.Log.Printf("job %s: stats: %v (agent-jobs stats --backfill retries)", id, err)
	}
}

// BackfillReport is the result of `agent-jobs stats --backfill`.
type BackfillReport struct {
	Recorded int      `json:"recorded"`
	Present  int      `json:"present"` // jobs that already had a row (kept unless all)
	Errors   []string `json:"errors"`
}

// BackfillStats writes the stats rows of terminal jobs that have none (all of them with all) from
// their job folders or the archive. Runs under the lifecycle lock, so prune does not move a stream
// away mid-read.
func (m *Manager) BackfillStats(all bool) (BackfillReport, error) {
	rep := BackfillReport{Errors: []string{}}
	unlock, err := m.lock()
	if err != nil {
		return rep, err
	}
	defer unlock()
	list, err := m.Store.ListTerminal()
	if err != nil {
		return rep, err
	}
	for _, j := range list {
		if !all {
			if _, err := m.Store.GetStats(j.ID); err == nil {
				rep.Present++
				continue
			}
		}
		if _, err := m.RecordStats(j); err != nil {
			rep.Errors = append(rep.Errors, err.Error())
			continue
		}
		rep.Recorded++
	}
	return rep, nil
}

// Stats grouping keys.
var StatsGroupBy = []string{"kind", "model", "skill", "effort", "repo"}

// Outcomes job_outcome records: what the orchestrator did with a job's result.
const (
	OutcomeAccepted  = "accepted"  // used as delivered
	OutcomeExtended  = "extended"  // used, and more work was added on top
	OutcomeReworked  = "reworked"  // the job's own work had to be fixed first
	OutcomeRerun     = "rerun"     // run again, on another model or with another brief
	OutcomeDiscarded = "discarded" // thrown away
)

// Outcomes lists the outcomes in their reporting order.
var Outcomes = []string{OutcomeAccepted, OutcomeExtended, OutcomeReworked, OutcomeRerun, OutcomeDiscarded}

// OutcomeUnrecorded is the key of the outcome shares for jobs that have a report and no outcome.
const OutcomeUnrecorded = "unrecorded"

// MaxOutcomeNoteRunes caps the note of job_outcome.
const MaxOutcomeNoteRunes = 300

// SetOutcome records what the orchestrator did with the result of a finished job of this agent
// (job_outcome); a later call overwrites it. Unknown ids, other agents' jobs and jobs that are still
// queued, running or stopped are refused.
func (m *Manager) SetOutcome(id, outcome, note string) error {
	id, outcome, note = strings.TrimSpace(id), strings.TrimSpace(outcome), strings.TrimSpace(note)
	if id == "" {
		return errors.New("id is required")
	}
	if !slices.Contains(Outcomes, outcome) {
		return fmt.Errorf("outcome %q must be one of %s", outcome, strings.Join(Outcomes, "|"))
	}
	if n := len([]rune(note)); n > MaxOutcomeNoteRunes {
		return fmt.Errorf("note is %d characters, at most %d", n, MaxOutcomeNoteRunes)
	}
	j, err := m.Store.Get(id)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("job %q not found", id)
	}
	if err != nil {
		return err
	}
	if j.Agent != m.Agent {
		return fmt.Errorf("job %s belongs to agent %s, not %s", id, j.Agent, m.Agent)
	}
	switch j.Status {
	case store.StatusQueued, store.StatusRunning, store.StatusStopped:
		return fmt.Errorf("job %s is %s: an outcome is recorded once the job has finished", id, j.Status)
	}
	if err := m.Store.SetOutcome(id, m.Agent, outcome, note, m.Now()); err != nil {
		if errors.Is(err, store.ErrImmutable) {
			return fmt.Errorf("job %s changed meanwhile and is not finished: %w", id, err)
		}
		return err
	}
	return nil
}

// StatsSummary aggregates a set of finished jobs. Percentiles are nearest-rank.
type StatsSummary struct {
	Jobs              int            `json:"jobs"`
	ByStatus          map[string]int `json:"by_status"`
	SuccessRate       float64        `json:"success_rate"` // succeeded / jobs
	CostTotalUSD      float64        `json:"cost_total_usd"`
	CostMedianUSD     float64        `json:"cost_median_usd"`
	CostP90USD        float64        `json:"cost_p90_usd"`
	DurationMedianSec int            `json:"duration_median_sec"`
	DurationP90Sec    int            `json:"duration_p90_sec"`
	Turns             int            `json:"turns"`
	GuardRefusals     int            `json:"guard_refusals"`
	Subagents         int            `json:"subagents"`
	WithoutReport     int            `json:"without_report"`
	// ReportOffContract counts the jobs whose report lacks the Summary or the Not done / uncertain section.
	ReportOffContract int `json:"report_off_contract"`
	// CostUnknownJobs counts the jobs whose cost is unknown; the cost figures leave them out.
	CostUnknownJobs int `json:"cost_unknown_jobs"`
	// CostEstimatedUSD is the part of CostTotalUSD estimated from token usage at list prices (attempts
	// without a reported cost); EstimatedJobs counts the jobs that carry such an estimate.
	CostEstimatedUSD float64 `json:"cost_estimated_usd"`
	EstimatedJobs    int     `json:"estimated_jobs"`
	// Outcomes counts the recorded outcomes (job_outcome); Unrecorded the jobs with a report and none.
	Outcomes   map[string]int `json:"outcomes"`
	Unrecorded int            `json:"unrecorded"`
	// OutcomeShares is each outcome's and "unrecorded"'s share of the jobs.
	OutcomeShares map[string]float64 `json:"outcome_shares"`
	// CostPerUsableResultUSD is the total cost / (accepted + extended); 0 when there is no usable result.
	CostPerUsableResultUSD float64 `json:"cost_per_usable_result_usd"`
}

// StatsGroup is the summary of one group (a kind, a model, a skill, an effort or a repo).
type StatsGroup struct {
	Key string `json:"key"`
	StatsSummary
}

// NameCount is a name with a count.
type NameCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// SkillUse is how often a skill was used: in how many jobs, how many calls, and how those jobs ended;
// Delivered counts the jobs it was linked into (E7), DeliveredUnused those of them that never used it.
type SkillUse struct {
	Name            string  `json:"name"`
	Jobs            int     `json:"jobs"`
	Uses            int     `json:"uses"`
	Succeeded       int     `json:"succeeded"`
	SuccessRate     float64 `json:"success_rate"`
	Delivered       int     `json:"delivered"`
	DeliveredUnused int     `json:"delivered_unused"`
}

// StatsReport is the output of `agent-jobs stats` and the job_stats MCP tool.
type StatsReport struct {
	Since      string       `json:"since,omitempty"`
	By         string       `json:"by"`
	Totals     StatsSummary `json:"totals"`
	Groups     []StatsGroup `json:"groups"`
	GuardRules []NameCount  `json:"guard_rules"`
	Skills     []SkillUse   `json:"skills"`
	Tools      []NameCount  `json:"tools"`
}

// TopN bounds the guard-rule and tool lists of a stats report.
const TopN = 20

// ParseSince reads a --since value: "<n>d", a Go duration ("36h") or a date (YYYY-MM-DD, local).
// Empty = no bound (zero time).
func ParseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if n, ok := strings.CutSuffix(s, "d"); ok {
		var d int
		if _, err := fmt.Sscanf(n, "%d", &d); err == nil && d >= 0 && fmt.Sprint(d) == n {
			return now.Add(-time.Duration(d) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, now.Location()); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("since %q: want <n>d, a duration such as 36h, or a date YYYY-MM-DD", s)
}

// Stats summarizes the stats rows of jobs finished at or after since, grouped by kind, model, skill or effort.
func (m *Manager) Stats(since time.Time, by string) (StatsReport, error) {
	return m.StatsContext(context.Background(), since, by)
}

// StatsContext is Stats, given up when ctx ends.
func (m *Manager) StatsContext(ctx context.Context, since time.Time, by string) (StatsReport, error) {
	rows, err := m.Store.ListStatsContext(ctx, since)
	if err != nil {
		return StatsReport{}, err
	}
	rep, err := Summarize(rows, by)
	if !since.IsZero() {
		rep.Since = since.UTC().Format(time.RFC3339)
	}
	return rep, err
}

// Summarize aggregates stats rows (pure: no I/O). by is kind, model, skill, effort or repo (default kind);
// with skill a job counts in every skill it used, and jobs without one under "(none)" (effort: jobs from
// before the effort was recorded; repo: jobs without a repository).
func Summarize(rows []*store.JobStats, by string) (StatsReport, error) {
	if by == "" {
		by = "kind"
	}
	if !slices.Contains(StatsGroupBy, by) {
		return StatsReport{}, fmt.Errorf("by %q must be one of %s", by, strings.Join(StatsGroupBy, "|"))
	}
	rep := StatsReport{By: by, Totals: summarize(rows), Groups: []StatsGroup{}, GuardRules: []NameCount{},
		Skills: []SkillUse{}, Tools: []NameCount{}}
	groups := map[string][]*store.JobStats{}
	rules, tools := map[string]int{}, map[string]int{}
	skills := map[string]*SkillUse{}
	for _, r := range rows {
		var keys []string
		switch by {
		case "kind":
			keys = []string{r.Kind}
		case "model":
			keys = []string{r.Model}
		case "effort":
			keys = []string{r.Effort}
			if r.Effort == "" {
				keys = []string{"(none)"} // a job from before the effort was recorded
			}
		case "repo":
			keys = []string{r.Repo}
			if r.Repo == "" {
				keys = []string{"(none)"}
			}
		case "skill":
			for s := range r.Skills {
				keys = append(keys, s)
			}
			if len(keys) == 0 {
				keys = []string{"(none)"}
			}
		}
		for _, k := range keys {
			groups[k] = append(groups[k], r)
		}
		for n, c := range r.GuardRules {
			rules[n] += c
		}
		for n, c := range r.ToolCalls {
			tools[n] += c
		}
		use := func(n string) *SkillUse {
			u := skills[n]
			if u == nil {
				u = &SkillUse{Name: n}
				skills[n] = u
			}
			return u
		}
		for n, c := range r.Skills {
			u := use(n)
			u.Jobs++
			u.Uses += c
			if r.Status == store.StatusSucceeded {
				u.Succeeded++
			}
		}
		for _, n := range r.SkillsDelivered {
			u := use(n)
			u.Delivered++
			if r.Skills[n] == 0 {
				u.DeliveredUnused++
			}
		}
	}
	for k, g := range groups {
		rep.Groups = append(rep.Groups, StatsGroup{Key: k, StatsSummary: summarize(g)})
	}
	sort.Slice(rep.Groups, func(a, b int) bool {
		ga, gb := rep.Groups[a], rep.Groups[b]
		if ga.Jobs != gb.Jobs {
			return ga.Jobs > gb.Jobs
		}
		return ga.Key < gb.Key
	})
	for _, u := range skills {
		u.SuccessRate = ratio(u.Succeeded, u.Jobs)
		rep.Skills = append(rep.Skills, *u)
	}
	sort.Slice(rep.Skills, func(a, b int) bool {
		sa, sb := rep.Skills[a], rep.Skills[b]
		if sa.Uses != sb.Uses {
			return sa.Uses > sb.Uses
		}
		if sa.Delivered != sb.Delivered {
			return sa.Delivered > sb.Delivered
		}
		return sa.Name < sb.Name
	})
	rep.GuardRules, rep.Tools = topN(rules, TopN), topN(tools, TopN)
	return rep, nil
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return math.Round(float64(a)/float64(b)*1000) / 1000
}

func topN(m map[string]int, n int) []NameCount {
	out := make([]NameCount, 0, len(m))
	for k, v := range m {
		out = append(out, NameCount{Name: k, Count: v})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Count != out[b].Count {
			return out[a].Count > out[b].Count
		}
		return out[a].Name < out[b].Name
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// percentile returns the nearest-rank p-th percentile (0 < p ≤ 1) of sorted values.
func percentile[T int | float64](sorted []T, p float64) T {
	if len(sorted) == 0 {
		var zero T
		return zero
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}

func summarize(rows []*store.JobStats) StatsSummary {
	s := StatsSummary{Jobs: len(rows), ByStatus: map[string]int{}, Outcomes: map[string]int{},
		OutcomeShares: map[string]float64{}}
	costs := make([]float64, 0, len(rows))
	durs := make([]int, 0, len(rows))
	for _, r := range rows {
		s.ByStatus[r.Status]++
		// A job whose unreported attempts were all priced counts at billed + estimated cost.
		estimated := r.CostSource == store.CostSourceEstimated
		switch {
		case r.CostUnknown && !estimated:
			s.CostUnknownJobs++
		default:
			c := r.CostUSD
			if estimated {
				c += r.CostEstimatedUSD
				s.CostEstimatedUSD += r.CostEstimatedUSD
				s.EstimatedJobs++
			}
			s.CostTotalUSD += c
			costs = append(costs, c)
		}
		switch {
		case r.Outcome != "":
			s.Outcomes[r.Outcome]++
		case r.HasReport:
			s.Unrecorded++
		}
		s.Turns += r.Turns
		s.GuardRefusals += r.GuardRefusals
		s.Subagents += r.Subagents
		if !r.HasReport {
			s.WithoutReport++
		}
		if r.ReportOffContract {
			s.ReportOffContract++
		}
		durs = append(durs, r.DurationSec)
	}
	slices.Sort(costs)
	slices.Sort(durs)
	s.SuccessRate = ratio(s.ByStatus[store.StatusSucceeded], s.Jobs)
	s.CostTotalUSD = math.Round(s.CostTotalUSD*1e4) / 1e4
	s.CostEstimatedUSD = math.Round(s.CostEstimatedUSD*1e4) / 1e4
	for _, o := range Outcomes {
		s.OutcomeShares[o] = ratio(s.Outcomes[o], s.Jobs)
	}
	s.OutcomeShares[OutcomeUnrecorded] = ratio(s.Unrecorded, s.Jobs)
	if usable := s.Outcomes[OutcomeAccepted] + s.Outcomes[OutcomeExtended]; usable > 0 {
		s.CostPerUsableResultUSD = math.Round(s.CostTotalUSD/float64(usable)*1e4) / 1e4
	}
	s.CostMedianUSD, s.CostP90USD = percentile(costs, 0.5), percentile(costs, 0.9)
	s.DurationMedianSec, s.DurationP90Sec = percentile(durs, 0.5), percentile(durs, 0.9)
	return s
}
