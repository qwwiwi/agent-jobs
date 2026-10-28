package mcp

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// termMisuse are the forms that mix the two terms: a checklist is the acceptance list of a repository, a
// milestone a stage of work made of tasks, so neither is a part, a kind or a list of the other.
var termMisuse = regexp.MustCompile(`(?i)\b(` +
	`milestones?[ -]checklists?|checklists?[ -]milestones?|` + // "milestone checklist", "checklist milestone"
	`checklists? of (tasks|milestones|stages|jobs|steps)|tasks?[ -]checklists?\b|` + // a checklist as a task list
	`checklists? (stages?|phases?|steps?|tasks?)\b|stages? of (the |a )?checklists?|` + // a checklist as a stage
	`acceptance milestones?|milestones? of acceptance|milestones? (rows?|requirements?)\b|` + // a milestone as acceptance
	`milestones? (is|are) (the|an?) (acceptance|checklist)|checklists? (is|are) (the|an?) (stages?|milestones?))`)

// checklistInMilestoneTool are the only ways a milestone tool's description may name the checklist: the
// acceptance list it is tied to (its id, its rows, the file).
var checklistInMilestoneTool = regexp.MustCompile(`(?i)checklist_id|checklist rows?|tied to a checklist|brief/CHECKLIST\.md`)

func readRepoDoc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// markdownSection is the text of doc from the line heading up to the next heading of the same or a higher
// level outside a code fence ("" when doc has no such heading).
func markdownSection(doc, heading string) string {
	_, rest, ok := strings.Cut(doc, "\n"+heading+"\n")
	if !ok {
		return ""
	}
	level := len(heading) - len(strings.TrimLeft(heading, "#"))
	lines := strings.Split(rest, "\n")
	fenced := false
	for i, line := range lines {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
		}
		n := len(line) - len(strings.TrimLeft(line, "#"))
		if !fenced && n > 0 && n <= level && strings.HasPrefix(line[n:], " ") {
			return strings.Join(lines[:i], "\n")
		}
	}
	return rest
}

// The two terms are used one way everywhere the orchestrator or the owner reads them: README, GUIDE,
// PLAN, the skill, the server instructions, the MCP tool descriptions and schemas, and the prompts of
// the milestone features. «checklist» is only the acceptance list, «milestone» only a stage of work.
func TestMilestoneTermsDocumented(t *testing.T) {
	t.Run("[R06] checklist means the acceptance list and milestone a stage of work in docs, tools and prompts", func(t *testing.T) {
		m := newManager(t)
		res, err := connect(t, mustServer(t, m)).ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		readme, guide := readRepoDoc(t, "README.md"), readRepoDoc(t, "docs/GUIDE.md")
		sources := map[string]string{
			"README.md":                  readme,
			"docs/GUIDE.md":              guide,
			"docs/PLAN.md":               readRepoDoc(t, "docs/PLAN.md"),
			"skills/agent-jobs/SKILL.md": readRepoDoc(t, "skills/agent-jobs/SKILL.md"),
			"server instructions":        orchestratorInstructions,
			"skills picker, milestone":   jobs.SkillsPickerMilestoneNote,
			"skills picker, job":         jobs.SkillsPickerJobNote,
			"prior results":              jobs.PriorResultsIntro,
		}
		descriptions := map[string]string{}
		for _, tl := range res.Tools {
			descriptions[tl.Name] = tl.Description
			schema, err := json.Marshal(tl.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			sources["tool "+tl.Name] = tl.Description + "\n" + string(schema)
		}

		// 1. The definitions: GUIDE's term list and README's milestones section.
		for _, c := range []struct{ name, text, re string }{
			{"GUIDE ### Milestones", markdownSection(guide, "### Milestones"), `(?m)^- \*\*checklist\*\*: the acceptance list\b`},
			{"GUIDE ### Milestones", markdownSection(guide, "### Milestones"), `(?m)^- \*\*milestone\*\*: a stage of work\b`},
			{"GUIDE ### Milestones", markdownSection(guide, "### Milestones"), `(?m)^- \*\*task\*\*: one step of a milestone\b`},
			{"README ## Milestones", markdownSection(readme, "## Milestones: stages, tasks, jobs"), `\*\*milestones\*\* \(stages of work\b`},
			{"README ## Milestones", markdownSection(readme, "## Milestones: stages, tasks, jobs"), `\*\*checklist\*\* \(` + "`brief/CHECKLIST.md`" + `\) stays the acceptance list\b`},
		} {
			if !regexp.MustCompile(c.re).MatchString(c.text) {
				t.Errorf("%s: no line matching %s", c.name, c.re)
			}
		}

		// 2. No source mixes the terms.
		for name, text := range sources {
			for _, hit := range termMisuse.FindAllString(text, -1) {
				t.Errorf("%s: %q mixes checklist and milestone", name, hit)
			}
		}

		// 3. The milestone tools speak of milestones and tasks; the checklist only as the acceptance list
		// a milestone is tied to.
		for _, name := range []string{"milestone_create", "milestone_set", "milestone_get", "milestone_list"} {
			d, ok := descriptions[name]
			if !ok {
				t.Errorf("no tool %s", name)
				continue
			}
			if !strings.Contains(d, "milestone") {
				t.Errorf("%s: the description does not say milestone: %q", name, d)
			}
			if name != "milestone_list" && !strings.Contains(d, "task") {
				t.Errorf("%s: the description does not say task: %q", name, d)
			}
			if rest := checklistInMilestoneTool.ReplaceAllString(d, ""); strings.Contains(strings.ToLower(rest), "checklist") {
				t.Errorf("%s: names the checklist other than as the acceptance list it is tied to: %q", name, d)
			}
		}
		if !strings.Contains(descriptions["milestone_create"], "a stage of work") {
			t.Errorf("milestone_create does not say a milestone is a stage of work: %q", descriptions["milestone_create"])
		}

		// 4. The checklist tools speak of the acceptance list, never of milestones or stages.
		for _, name := range []string{"checklist_init", "checklist_get", "checklist_set"} {
			d, ok := descriptions[name]
			if !ok {
				t.Errorf("no tool %s", name)
				continue
			}
			if !strings.Contains(d, "acceptance list") {
				t.Errorf("%s: the description does not say acceptance list: %q", name, d)
			}
			if l := strings.ToLower(d); strings.Contains(l, "milestone") || strings.Contains(l, "stage") {
				t.Errorf("%s: a checklist tool speaks of milestones or stages: %q", name, d)
			}
		}

		// 5. The milestone prompts and the server's milestone line speak of milestones and tasks, never of a
		// checklist.
		_, line, _ := strings.Cut(orchestratorInstructions, "\nmilestone_create")
		line, _, _ = strings.Cut(line, "\n")
		for name, text := range map[string]string{
			"server instructions, milestone line": line,
			"skills picker, milestone":            jobs.SkillsPickerMilestoneNote,
			"prior results":                       jobs.PriorResultsIntro,
		} {
			if !strings.Contains(text, "milestone") || !strings.Contains(text, "task") {
				t.Errorf("%s: does not speak of milestone and task: %q", name, text)
			}
			if strings.Contains(strings.ToLower(text), "checklist") {
				t.Errorf("%s: a milestone prompt names a checklist: %q", name, text)
			}
		}
	})
}
