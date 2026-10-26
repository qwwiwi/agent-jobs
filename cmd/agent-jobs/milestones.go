package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
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

// cmdMilestone is `agent-jobs milestone <id>`: milestone_get as text or JSON.
func cmdMilestone(args []string) error {
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
	id, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("milestone id %q must be a positive number", rest[0])
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
