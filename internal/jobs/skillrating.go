package jobs

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/skills"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Staleness of a catalog skill (E7): not verified for StaleVerifiedDays, or not used for StaleUnusedDays.
const (
	StaleVerifiedDays = 90
	StaleUnusedDays   = 60
	// skillUsageDays is how far back skill_list reads job_stats for uses and success rates.
	skillUsageDays = 365
)

// NotInCatalog is the status of a skill the stats know but the catalog does not (any more).
const NotInCatalog = "not_in_catalog"

func parseDay(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	if len(s) > 10 {
		if t, err := time.Parse("2006-01-02", s[:10]); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// SkillStale reports whether a catalog skill is stale: last_verified missing or older than 90 days,
// last used more than 60 days ago, or never used and verified more than 60 days ago.
func SkillStale(lastVerified string, lastUsed *time.Time, now time.Time) bool {
	day := 24 * time.Hour
	v, ok := parseDay(lastVerified)
	if !ok || now.Sub(v) > StaleVerifiedDays*day {
		return true
	}
	if lastUsed != nil {
		return now.Sub(*lastUsed) > StaleUnusedDays*day
	}
	return now.Sub(v) > StaleUnusedDays*day
}

// skillUse is the usage of one skill over a set of stats rows.
type skillUse struct {
	delivered, used, uses, succeeded int
	lastUsed                         *time.Time
	costs                            []float64
	byRepo                           map[string]*[2]int // repo -> used, succeeded
}

func rowTime(r *store.JobStats) time.Time {
	if r.FinishedAt != nil {
		return *r.FinishedAt
	}
	return r.CreatedAt
}

// skillUsage aggregates the rows by skill: rows counted only when counted(r) holds; lastUsed over all.
func skillUsage(rows []*store.JobStats, counted func(*store.JobStats) bool) map[string]*skillUse {
	out := map[string]*skillUse{}
	get := func(n string) *skillUse {
		u := out[n]
		if u == nil {
			u = &skillUse{byRepo: map[string]*[2]int{}}
			out[n] = u
		}
		return u
	}
	for _, r := range rows {
		t := rowTime(r)
		for n, c := range r.Skills {
			if c <= 0 {
				continue
			}
			u := get(n)
			if u.lastUsed == nil || t.After(*u.lastUsed) {
				tt := t
				u.lastUsed = &tt
			}
			if !counted(r) {
				continue
			}
			u.used++
			u.uses += c
			ok := r.Status == store.StatusSucceeded
			if ok {
				u.succeeded++
			}
			if !r.CostUnknown {
				u.costs = append(u.costs, r.CostUSD)
			}
			repo := r.Repo
			if repo == "" {
				repo = "(none)"
			}
			br := u.byRepo[repo]
			if br == nil {
				br = &[2]int{}
				u.byRepo[repo] = br
			}
			br[0]++
			if ok {
				br[1]++
			}
		}
		if counted(r) {
			for _, n := range r.SkillsDelivered {
				get(n).delivered++
			}
		}
	}
	return out
}

// SkillListItem is one skill as skill_list and `agent-jobs skills` show it: no paths, no body.
type SkillListItem struct {
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	Description  string   `json:"description"`
	Kinds        []string `json:"kinds"`
	SizeKB       float64  `json:"size_kb"`
	Risk         []string `json:"risk"`
	LastVerified string   `json:"last_verified,omitempty"`
	// Score is the search score (search only).
	Score float64 `json:"score,omitempty"`
	// Uses, Jobs and SuccessRate come from job_stats (the last 365 days): Skill tool calls, the jobs
	// that used the skill and the share of them that succeeded.
	Uses        int     `json:"uses"`
	Jobs        int     `json:"jobs"`
	SuccessRate float64 `json:"success_rate"`
	Stale       bool    `json:"stale"`
}

// SkillListOutput is the output of skill_list and `agent-jobs skills list|search --json`.
type SkillListOutput struct {
	Query    string          `json:"query,omitempty"`
	Skills   []SkillListItem `json:"skills"`
	Warnings []string        `json:"warnings,omitempty"`
}

// SkillList lists (no query) or searches (query) the catalog with the filter, each entry with its
// usage from job_stats and its stale flag. A search returns the best skills.SearchLimit hits.
func (m *Manager) SkillList(ctx context.Context, query string, f skills.Filter) (SkillListOutput, error) {
	out := SkillListOutput{Query: strings.TrimSpace(query), Skills: []SkillListItem{}}
	if err := f.Validate(); err != nil {
		return out, err
	}
	if f.Kind != "" && !slices.Contains(Kinds, f.Kind) && !skills.ValidName(f.Kind) {
		return out, fmt.Errorf("kind %q is not a job kind (%s) or a catalog kind", f.Kind, strings.Join(Kinds, "|"))
	}
	cat := m.Catalog()
	out.Warnings = cat.Warnings
	now := m.Now()
	rows, err := m.Store.ListStatsContext(ctx, now.Add(-skillUsageDays*24*time.Hour))
	if err != nil {
		return out, err
	}
	usage := skillUsage(rows, func(*store.JobStats) bool { return true })
	item := func(e skills.Entry, score float64) SkillListItem {
		it := SkillListItem{Name: e.Name, Status: e.Status, Description: e.Description, Kinds: e.Kinds, SizeKB: math.Round(e.SizeKB*10) / 10,
			Risk: e.Risk, LastVerified: e.LastVerified, Score: score}
		var last *time.Time
		if u := usage[e.Name]; u != nil {
			it.Uses, it.Jobs, it.SuccessRate, last = u.uses, u.used, ratio(u.succeeded, u.used), u.lastUsed
		}
		it.Stale = SkillStale(e.LastVerified, last, now)
		return it
	}
	if out.Query == "" {
		for _, e := range cat.List(f) {
			out.Skills = append(out.Skills, item(e, 0))
		}
		return out, nil
	}
	for _, h := range cat.Search(out.Query, f, skills.SearchLimit) {
		out.Skills = append(out.Skills, item(h.Entry, h.Score))
	}
	return out, nil
}

// SkillRepoUse is one repository's line of a skill's rating.
type SkillRepoUse struct {
	Repo        string  `json:"repo"`
	Used        int     `json:"used"`
	SuccessRate float64 `json:"success_rate"`
}

// SkillRating is one skill of the skills rating (GET /v1/skills, `agent-jobs stats --by skill --by-repo`).
type SkillRating struct {
	Name          string         `json:"name"`
	Status        string         `json:"status"`
	Stale         bool           `json:"stale"`
	Delivered     int            `json:"delivered"`
	Used          int            `json:"used"`
	Uses          int            `json:"uses"`
	SuccessRate   float64        `json:"success_rate"`
	LastUsed      string         `json:"last_used,omitempty"`
	MedianCostUSD float64        `json:"median_cost_usd"`
	ByRepo        []SkillRepoUse `json:"by_repo"`
}

// SkillRatingReport is the skills rating.
type SkillRatingReport struct {
	Since     string        `json:"since"`
	Repo      string        `json:"repo,omitempty"`
	Skills    []SkillRating `json:"skills"`
	Truncated bool          `json:"truncated,omitempty"`
}

// SkillRatings rates every skill of the catalog and every skill the stats know: jobs it was delivered
// to and used in since since (only jobs of repo, a folder name, when given), their success rate and
// median cost, the last use, the stale flag and a per-repository breakdown; sorted by used count.
// limit ≤ 0 = no limit.
func (m *Manager) SkillRatings(ctx context.Context, since time.Time, repo string, limit int) (SkillRatingReport, error) {
	now := m.Now()
	rep := SkillRatingReport{Since: since.UTC().Format(time.RFC3339), Repo: repo, Skills: []SkillRating{}}
	// Read back far enough to tell the last use of a skill unused in the window (stale flag).
	from := since
	if back := now.Add(-StaleVerifiedDays * 24 * time.Hour); back.Before(from) {
		from = back
	}
	rows, err := m.Store.ListStatsContext(ctx, from)
	if err != nil {
		return rep, err
	}
	usage := skillUsage(rows, func(r *store.JobStats) bool {
		return !rowTime(r).Before(since) && (repo == "" || r.Repo == repo)
	})
	cat := m.Catalog()
	names := map[string]bool{}
	for _, e := range cat.Entries {
		names[e.Name] = true
	}
	for n := range usage {
		names[n] = true
	}
	for n := range names {
		r := SkillRating{Name: n, Status: NotInCatalog, ByRepo: []SkillRepoUse{}}
		u := usage[n]
		if u == nil {
			u = &skillUse{byRepo: map[string]*[2]int{}}
		}
		if e, ok := cat.Lookup(n); ok {
			r.Status = e.Status
			r.Stale = SkillStale(e.LastVerified, u.lastUsed, now)
		} else {
			r.Stale = u.lastUsed == nil || now.Sub(*u.lastUsed) > StaleUnusedDays*24*time.Hour
		}
		r.Delivered, r.Used, r.Uses, r.SuccessRate = u.delivered, u.used, u.uses, ratio(u.succeeded, u.used)
		if u.lastUsed != nil {
			r.LastUsed = u.lastUsed.UTC().Format(time.RFC3339)
		}
		slices.Sort(u.costs)
		r.MedianCostUSD = math.Round(percentile(u.costs, 0.5)*1e4) / 1e4
		for name, c := range u.byRepo {
			r.ByRepo = append(r.ByRepo, SkillRepoUse{Repo: name, Used: c[0], SuccessRate: ratio(c[1], c[0])})
		}
		sort.Slice(r.ByRepo, func(a, b int) bool {
			if r.ByRepo[a].Used != r.ByRepo[b].Used {
				return r.ByRepo[a].Used > r.ByRepo[b].Used
			}
			return r.ByRepo[a].Repo < r.ByRepo[b].Repo
		})
		if repo != "" && r.Used == 0 && r.Delivered == 0 && !slices.Contains(catProjects(cat, n), repo) {
			continue // with a repo filter: only the skills that repository used, got or names
		}
		rep.Skills = append(rep.Skills, r)
	}
	sort.Slice(rep.Skills, func(a, b int) bool {
		sa, sb := rep.Skills[a], rep.Skills[b]
		if sa.Used != sb.Used {
			return sa.Used > sb.Used
		}
		if sa.Delivered != sb.Delivered {
			return sa.Delivered > sb.Delivered
		}
		return sa.Name < sb.Name
	})
	if limit > 0 && len(rep.Skills) > limit {
		rep.Skills, rep.Truncated = rep.Skills[:limit], true
	}
	return rep, nil
}

func catProjects(c *skills.Catalog, name string) []string {
	if e, ok := c.Lookup(name); ok {
		return e.Projects
	}
	return nil
}
