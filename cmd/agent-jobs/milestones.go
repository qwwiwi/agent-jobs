package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/mcp"
)

// cmdMilestones is `agent-jobs milestones`: milestone_list as a table or JSON.
func cmdMilestones(args []string) error {
	fs := flag.NewFlagSet("milestones", flag.ContinueOnError)
	agent := agentFlag(fs)
	var r jobs.MilestoneListRequest
	fs.StringVar(&r.Status, "status", "", "only open|done|dropped (default all)")
	fs.StringVar(&r.Repo, "repo", "", "only the milestones of this repository (the project view)")
	fs.StringVar(&r.ChecklistID, "checklist-id", "", "only the milestones tied to this checklist id")
	fs.StringVar(&r.UpdatedSince, "updated-since", "", "only milestones changed at or after (RFC 3339 or Unix seconds)")
	fs.BoolVar(&r.Tree, "tree", false, "each milestone with its full tree (JSON; the table shows the tasks)")
	asJSON := fs.Bool("json", false, "print the list as JSON (same as milestone_list)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	out, err := m.ListMilestones(context.Background(), r)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tORDER\tREPO\tUPDATED\tTITLE")
	for _, v := range out.Milestones {
		repo := v.Repo
		if repo == "" {
			repo = "-"
		}
		fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\t%s\n", v.ID, v.Status, v.Order, repo, v.UpdatedAt, v.Title)
		for _, t := range v.Tasks {
			fmt.Fprintf(w, "\t  %s\t%s\t\t\t%s\n", t.Key, t.Status, t.Title)
		}
	}
	return w.Flush()
}

// cmdMilestone is `agent-jobs milestone <id>`: milestone_get as text or JSON; `milestone create` and
// `milestone set` are milestone_create and milestone_set (cmdMilestoneEdit).
func cmdMilestone(args []string) error {
	if len(args) > 0 && (args[0] == "create" || args[0] == "set") {
		return cmdMilestoneEdit(args[0], args[1:], os.Stdin)
	}
	fs := flag.NewFlagSet("milestone", flag.ContinueOnError)
	agent := agentFlag(fs)
	asJSON := fs.Bool("json", false, "print the tree as JSON (same as milestone_get)")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("usage: agent-jobs milestone --agent A <id> [--json]")
	}
	id, err := milestoneID(rest[0])
	if err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	v, err := m.MilestoneTree(context.Background(), id)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(v)
	}
	fmt.Printf("milestone %d · %s · %s\n", v.ID, v.Status, v.Title)
	if v.Goal != "" {
		fmt.Printf("goal: %s\n", v.Goal)
	}
	if v.Repo != "" {
		fmt.Printf("repo: %s", v.Repo)
		if v.Branch != "" {
			fmt.Printf(" · branch %s", v.Branch)
		}
		fmt.Println()
	}
	p := v.Progress
	acc := ""
	if v.ChecklistID != "" {
		met := "?"
		if p.AcceptanceMet != nil {
			met = strconv.Itoa(*p.AcceptanceMet)
		}
		acc = fmt.Sprintf(" · acceptance %s/%d (checklist %s)", met, p.AcceptanceTotal, v.ChecklistID)
	}
	fmt.Printf("tasks %d/%d done%s · ready: %s\n", p.TasksDone, p.TasksTotal, acc, strings.Join(v.Ready, ", "))
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "TASK\tSTATUS\tDEPENDS\tSKILLS\tJOBS\tTITLE")
	for _, t := range v.Tasks {
		var js []string
		for _, j := range t.Jobs {
			s := j.ID + " " + j.Status
			if j.Outcome != "" {
				s += "/" + j.Outcome
			}
			s += jobSkills(j.Skills, j.SkillsDelivered)
			js = append(js, s)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", t.Key, t.Status, dash(strings.Join(t.DependsOn, ",")),
			dash(strings.Join(t.Skills, ",")), dash(strings.Join(js, "; ")), t.Title)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(v.Uncovered) > 0 {
		fmt.Printf("uncovered: %s\n", strings.Join(v.Uncovered, ", "))
	}
	if v.Note != "" {
		fmt.Printf("note: %s\n", v.Note)
	}
	return nil
}

const milestoneEditUsage = "usage: agent-jobs milestone create --agent A --input F|- [--title T]\n" +
	"       agent-jobs milestone set --agent A <id> [--input F|-] [--title T] [--status S] [--close-note N]"

// cmdMilestoneEdit is `agent-jobs milestone create` (op "create", milestone_create) and `agent-jobs
// milestone set <id>` (op "set", milestone_set): the same manager calls, so the same validation, and the
// same JSON tree on stdout. --input is a file ("-" = stdin) with the tool's JSON input, or with only
// its tasks array; the flags set single fields over it.
func cmdMilestoneEdit(op string, args []string, stdin io.Reader) error {
	fs := flag.NewFlagSet("milestone "+op, flag.ContinueOnError)
	agent := agentFlag(fs)
	input := fs.String("input", "", "JSON file (- = stdin): the milestone_"+op+" input object, or only its tasks array")
	title := fs.String("title", "", "the milestone's title (over the input's)")
	status := new(string)
	closeNote := new(string)
	if op == "set" {
		status = fs.String("status", "", "open|done|dropped (over the input's)")
		closeNote = fs.String("close-note", "", "one line (over the input's)")
	}
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if (op == "create" && (len(pos) != 0 || *input == "")) || (op == "set" && len(pos) != 1) {
		return errors.New(milestoneEditUsage)
	}
	var raw []byte
	if *input != "" {
		if raw, err = readInput(*input, stdin); err != nil {
			return err
		}
	}
	fields, err := milestoneArgs(raw)
	if err != nil {
		return err
	}
	if *title != "" {
		fields["title"] = *title
	}
	ctx := context.Background()
	if op == "create" {
		var in jobs.MilestoneCreate
		if err := decodeMilestoneArgs("milestone_create", fields, &in); err != nil {
			return err
		}
		m, err := open(*agent)
		if err != nil {
			return err
		}
		defer m.Close()
		v, err := m.CreateMilestone(ctx, in)
		if err != nil {
			return err
		}
		return printJSON(v)
	}
	id, err := milestoneID(pos[0])
	if err != nil {
		return err
	}
	// An id in the input is the positional one or refused, whatever its value or type.
	if v, ok := fields["id"]; ok && !sameMilestoneID(v, id) {
		b, _ := json.Marshal(v)
		return fmt.Errorf("the input's id %s is not milestone %d", b, id)
	}
	fields["id"] = id
	if *status != "" {
		fields["status"] = *status
	}
	if *closeNote != "" {
		fields["close_note"] = *closeNote
	}
	var in jobs.MilestoneSet
	if err := decodeMilestoneArgs("milestone_set", fields, &in); err != nil {
		return err
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	v, err := m.SetMilestone(ctx, in)
	if err != nil {
		return err
	}
	return printJSON(v)
}

// readInput reads the file name, or stdin for "-".
func readInput(name string, stdin io.Reader) ([]byte, error) {
	if name == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		return b, nil
	}
	return os.ReadFile(name)
}

// milestoneArgs reads raw, the tool's JSON input object or only its tasks array, into the tool's
// arguments as decoded JSON with numbers as json.Number (their exact text); no input is no arguments.
// It must be one JSON value: anything after it, even a stray ] or }, is refused.
func milestoneArgs(raw []byte) (map[string]any, error) {
	if raw == nil {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("milestone input: %w", err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("milestone input: more than one JSON value, or data after it")
	}
	switch v := v.(type) {
	case map[string]any:
		return v, nil
	case []any:
		return map[string]any{"tasks": v}, nil
	}
	return nil, errors.New("milestone input: must be a JSON object (the tool's input) or array (only its tasks)")
}

// decodeMilestoneArgs validates args against the input schema of tool (the MCP tool's own schema, so the
// CLI refuses what the tool refuses: unknown or differently cased keys, null where the schema has no
// null, wrong types) and only then decodes them into in. Its numbers are float64 as the server sees
// them, but an int64 (the positional id) stays exact through both.
func decodeMilestoneArgs(tool string, args map[string]any, in any) error {
	v, err := floatNumbers(args)
	if err != nil {
		return fmt.Errorf("milestone input: %w", err)
	}
	view := v.(map[string]any)
	if err := mcp.ValidateMilestoneInput(tool, view); err != nil {
		return fmt.Errorf("milestone input: %w", err)
	}
	b, err := json.Marshal(view)
	if err != nil {
		return fmt.Errorf("milestone input: %w", err)
	}
	if err := json.Unmarshal(b, in); err != nil {
		return fmt.Errorf("milestone input: %w", err)
	}
	return nil
}

// floatNumbers is v with every json.Number as the float64 a plain JSON decode gives (out of range is
// refused, as that decode refuses it); everything else is kept.
func floatNumbers(v any) (any, error) {
	switch v := v.(type) {
	case json.Number:
		f, err := strconv.ParseFloat(v.String(), 64)
		if err != nil {
			return nil, fmt.Errorf("number %s: %w", v, err)
		}
		return f, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			var err error
			if out[k], err = floatNumbers(e); err != nil {
				return nil, err
			}
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			var err error
			if out[i], err = floatNumbers(e); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	return v, nil
}

// sameMilestoneID reports whether v, an id from the input, is exactly id: a number of the same value
// (1, 1.0 or 1e0 for 1), not one that only rounds to it as a float64 (#93); milestone_set compares alike.
func sameMilestoneID(v any, id int64) bool {
	n, ok := v.(json.Number)
	return ok && mcp.SameMilestoneID(n, id)
}

func milestoneID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("milestone id %q must be a positive number", s)
	}
	return id, nil
}

// jobSkills renders a job's skills for the JOBS column: one list when the skills selected at job_start
// and those linked for its latest attempt match, both labelled when they differ (a repository-owned
// skill is selected but not linked), so neither the selection nor the delivery is lost.
func jobSkills(selected, delivered []string) string {
	if len(selected) == 0 && len(delivered) == 0 {
		return ""
	}
	if slices.Equal(slices.Sorted(slices.Values(selected)), slices.Sorted(slices.Values(delivered))) {
		return " [" + strings.Join(selected, ",") + "]"
	}
	none := func(s []string) string {
		if len(s) == 0 {
			return "none"
		}
		return strings.Join(s, ",")
	}
	return " [selected " + none(selected) + "; delivered " + none(delivered) + "]"
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
