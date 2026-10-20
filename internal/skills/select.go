package skills

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
)

// KindsFor maps a job kind to the catalog kinds its default skills carry: fix and general -> code and
// general, review -> review, research -> research. The job kind itself always matches too.
func KindsFor(jobKind string) []string {
	switch jobKind {
	case "fix", "general":
		return []string{jobKind, "code", "general"}
	case "":
		return nil
	}
	return []string{jobKind}
}

// Budget is the count and size limit of one job's skills.
type Budget struct {
	MaxCount int
	MaxKB    float64
}

// ErrRefused marks a selection the start must refuse (unknown name, budget).
var ErrRefused = errors.New("skills refused")

// Defaults returns the default skills of a job: core entries whose kinds match the job kind, plus
// core or active entries whose projects include the repository's folder name. Sorted by name.
func (c *Catalog) Defaults(jobKind, repo string) []string {
	if c == nil {
		return nil
	}
	kinds := KindsFor(jobKind)
	var out []string
	for _, e := range c.Entries {
		switch {
		case e.Status == StatusCore && slices.ContainsFunc(e.Kinds, func(k string) bool { return slices.Contains(kinds, k) }):
			out = append(out, e.Name)
		case repo != "" && (e.Status == StatusCore || e.Status == StatusActive) && slices.Contains(e.Projects, repo):
			out = append(out, e.Name)
		}
	}
	return out
}

// ParseEdits splits a comma list ("a,+b,-c") into edits; blanks are dropped.
func ParseEdits(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Select applies the edits to the defaults of a job and checks the result: "name" or "+name" adds,
// "-name" removes. An unknown or deprecated name refuses (with close names); a draft is allowed only
// when added by name; over the count or size budget refuses. The result is sorted by name.
func (c *Catalog) Select(jobKind, repo string, edits []string, b Budget) ([]Entry, error) {
	if c == nil {
		c = Load(nil)
	}
	chosen := map[string]bool{}
	for _, n := range c.Defaults(jobKind, repo) {
		chosen[n] = true
	}
	var errs []string
	for _, raw := range edits {
		op, name := '+', strings.TrimSpace(raw)
		if strings.HasPrefix(name, "+") || strings.HasPrefix(name, "-") {
			op, name = rune(name[0]), strings.TrimSpace(name[1:])
		}
		e, ok := c.Lookup(name)
		switch {
		case !ok:
			msg := fmt.Sprintf("unknown skill %q", name)
			if len(c.Roots) == 0 {
				msg += " (no skill catalog is configured: config skill_roots)"
			} else if close := c.Close(name, 5); len(close) > 0 {
				msg += " (close: " + strings.Join(close, ", ") + ")"
			}
			errs = append(errs, msg)
		case op == '-':
			delete(chosen, name)
		case e.Status == StatusDeprecated:
			msg := fmt.Sprintf("skill %q is deprecated and never selectable", name)
			if close := c.Close(name, 5); len(close) > 0 {
				msg += " (close: " + strings.Join(close, ", ") + ")"
			}
			errs = append(errs, msg)
		default:
			chosen[name] = true
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrRefused, strings.Join(errs, "; "))
	}
	out := make([]Entry, 0, len(chosen))
	total := 0.0
	for n := range chosen {
		e, _ := c.Lookup(n)
		out = append(out, e)
		total += e.SizeKB
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	if b.MaxCount > 0 && len(out) > b.MaxCount {
		return nil, fmt.Errorf("%w: %d skills selected (%s), the limit is %d per job (skills_max_per_job); remove some with -name",
			ErrRefused, len(out), strings.Join(Names(out), ", "), b.MaxCount)
	}
	if b.MaxKB > 0 && total > b.MaxKB {
		return nil, fmt.Errorf("%w: the selected skills total %.0f KB, the limit is %.0f KB per job (skills_max_kb)",
			ErrRefused, math.Ceil(total), b.MaxKB)
	}
	return out, nil
}

// Names returns the names of entries.
func Names(es []Entry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Name)
	}
	return out
}

// Close returns up to n selectable names close to name (edit distance ≤ 2, a shared prefix of 3
// or a substring), nearest first. Deprecated entries are never suggested.
func (c *Catalog) Close(name string, n int) []string {
	type cand struct {
		name string
		d    int
	}
	var cs []cand
	ln := strings.ToLower(name)
	for _, e := range c.Entries {
		if e.Status == StatusDeprecated || e.Name == name {
			continue
		}
		le := strings.ToLower(e.Name)
		d := levenshtein(ln, le)
		near := d <= 2 || (len(ln) >= 3 && (strings.Contains(le, ln) || strings.Contains(ln, le) ||
			strings.HasPrefix(le, ln[:3])))
		if near {
			cs = append(cs, cand{e.Name, d})
		}
	}
	sort.Slice(cs, func(a, b int) bool {
		if cs[a].d != cs[b].d {
			return cs[a].d < cs[b].d
		}
		return cs[a].name < cs[b].name
	})
	var out []string
	for i := 0; i < len(cs) && i < n; i++ {
		out = append(out, cs[i].name)
	}
	return out
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// BriefDeclinesSkills is the explicit no-skills signal accepted by orchestrator PreToolUse hooks.
// A whole line is required so prose mentioning the phrase cannot silently disable defaults.
func BriefDeclinesSkills(brief string) bool {
	for _, line := range strings.Split(brief, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 2)
		if len(parts) == 2 && strings.EqualFold(strings.TrimSpace(parts[0]), "skills") && strings.EqualFold(strings.TrimSpace(parts[1]), "none") {
			return true
		}
	}
	return false
}
