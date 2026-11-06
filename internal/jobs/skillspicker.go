package jobs

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/qwwiwi/agent-jobs/internal/skills"
)

// The skills picker (G01) is a PreToolUse hook of the orchestrator session on milestone_create and
// job_start (`agent-jobs hook skills-picker`). It asks which skills the work needs once per milestone:
// at milestone_create, for every task without a skills list, and never again on job_start of a task
// (the job gets the task's plan). A job_start of no milestone without skills is still asked. It only
// adds context for the model: it never blocks a call, needs no field and reads no state, so skills stay
// optional (no approval step).

// SkillsPickerPrefix starts the picker's context.
const SkillsPickerPrefix = "agent-jobs skills picker: "

// maxPickerKeys caps the task keys the picker names.
const maxPickerKeys = 20

// SkillsPickerMilestoneNote follows the task keys of a milestone_create without a full plan. The hook
// does not block, so the milestone is created as given: the plan follows in one milestone_set.
const SkillsPickerMilestoneNote = ". This call creates the milestone as given; plan their skills now, before the first " +
	"job_start, in one milestone_set (tasks [{key, skills}]; look them up with skill_list; job_start's syntax: " +
	"+name adds, -name removes, [] keeps the defaults of the job's kind). A job_start on a task then gets the " +
	"task's skills and is not asked again. Skills stay optional: a task without a list gets its jobs' defaults."

// SkillsPickerJobNote is the picker's context for a job_start of no milestone that names no skills.
const SkillsPickerJobNote = SkillsPickerPrefix + "this job_start names no skills, so the job gets the defaults of its " +
	"kind and repository. Check skill_list for skills to add (skills [\"+name\"]) or remove (\"-name\"); skills [] " +
	"keeps the defaults, a brief line skills: none gives none. For work of several tasks, plan the skills once per " +
	"task in milestone_create instead."

// isTool reports whether a hook's tool_name is the agent-jobs MCP tool name, under any server name
// (mcp__<server>__<name>).
func isTool(toolName, name string) bool {
	return toolName == name || strings.HasSuffix(toolName, "__"+name)
}

// SkillsPicker reads a PreToolUse hook input and returns the context the picker adds ("" = none).
// Another event or tool is no error: it gets nothing.
func SkillsPicker(r io.Reader) (string, error) { return SkillsPickerCapped(r, DefaultHookMaxBytes) }

// SkillsPickerCapped is SkillsPicker with the context at most maxBytes (config hooks.max_bytes): task keys
// that do not fit are counted in "and N more".
func SkillsPickerCapped(r io.Reader, maxBytes int) (string, error) {
	raw, err := readHookInput(r)
	if err != nil {
		return "", err
	}
	if *raw.HookEventName != "PreToolUse" || raw.ToolName == nil {
		return "", nil
	}
	switch {
	case isTool(*raw.ToolName, "milestone_create"):
		var in struct {
			Tasks []struct {
				Key    string           `json:"key"`
				Skills *json.RawMessage `json:"skills"`
			} `json:"tasks"`
		}
		if err := json.Unmarshal(raw.ToolInput, &in); err != nil {
			return "", fmt.Errorf("milestone_create input: %w", err)
		}
		var missing []string
		for _, t := range in.Tasks {
			if t.Skills == nil {
				missing = append(missing, Printable(strings.TrimSpace(t.Key), 40))
			}
		}
		if len(missing) == 0 {
			return "", nil
		}
		return capText(pickerMilestoneContext(missing, maxBytes), maxBytes), nil
	case isTool(*raw.ToolName, "job_start"):
		var in struct {
			Brief     string           `json:"brief"`
			Milestone json.Number      `json:"milestone"`
			Task      string           `json:"task"`
			Skills    *json.RawMessage `json:"skills"`
		}
		if err := json.Unmarshal(raw.ToolInput, &in); err != nil {
			return "", fmt.Errorf("job_start input: %w", err)
		}
		// A job on a task gets the task's plan (or job_start's own skills): it was asked at milestone_create.
		onTask := in.Milestone != "" && in.Milestone != "0" && strings.TrimSpace(in.Task) != ""
		if onTask || in.Skills != nil || skills.BriefDeclinesSkills(in.Brief) {
			return "", nil
		}
		return capText(SkillsPickerJobNote, maxBytes), nil
	}
	return "", nil
}

// pickerMilestoneHead and pickerCompactFoot are the fixed text of the milestone context; the compact foot
// replaces SkillsPickerMilestoneNote when a low hooks.max_bytes leaves no room for the full note.
const (
	pickerMilestoneHead = SkillsPickerPrefix + "skills are planned once per milestone, per task: tasks "
	pickerCompactFoot   = " have no skills list. Plan their skills now in one milestone_set (tasks [{key, skills}]; skill_list lists skills)."
)

// pickerMilestoneContext names the tasks without a skills list: at most maxPickerKeys keys, fewer when the
// context would pass maxBytes, always with "and N more" for the tasks left out. With the full note it
// is the context of old; a cap too low for the note takes the compact foot.
func pickerMilestoneContext(missing []string, maxBytes int) string {
	for _, foot := range []string{SkillsPickerMilestoneNote, pickerCompactFoot} {
		for n := min(len(missing), maxPickerKeys); n >= 0; n-- {
			keys := missing[:n:n]
			if n < len(missing) {
				keys = append(keys, fmt.Sprintf("and %d more", len(missing)-n))
			}
			msg := pickerMilestoneHead + strings.Join(keys, ", ") + " have no skills list" + foot
			if foot == pickerCompactFoot {
				msg = pickerMilestoneHead + strings.Join(keys, ", ") + foot
			}
			if len(msg) <= maxBytes {
				return msg
			}
		}
	}
	return pickerMilestoneHead + fmt.Sprintf("%d tasks", len(missing)) + pickerCompactFoot
}
