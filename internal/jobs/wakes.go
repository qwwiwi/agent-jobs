package jobs

import (
	"context"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Measuring the orchestrator session (#170): every line the daemon types into it is recorded by
// category with its bytes and the things it carried (batch), and so is every hook injection. The
// numbers back the savings claims (wakes, injected bytes) that were only estimates before.

// Categories of a delivery (a line typed into the session: one wake each).
const (
	WakeJobDone     = "job-done"
	WakeNotifyOwner = "notify-owner"
	WakeSchedule    = "schedule"
	WakeCheckMissed = "check-missed"
	WakeHeartbeat   = "heartbeat"
)

// Categories of a hook injection (text added to the session's context; not a wake).
const (
	InjectBriefReminder  = "hook:brief-reminder"
	InjectPendingOutcome = "hook:pending-outcome"
	InjectStop           = "hook:stop"
	InjectMergeGate      = "hook:merge-gate"
	InjectSkillsPicker   = "hook:skills-picker"
)

// WakeCategories are the delivery categories, in the order the report lists them.
var WakeCategories = []string{WakeJobDone, WakeNotifyOwner, WakeSchedule, WakeCheckMissed, WakeHeartbeat}

// InjectionCategories are the hook injection categories.
var InjectionCategories = []string{InjectBriefReminder, InjectPendingOutcome, InjectStop, InjectMergeGate, InjectSkillsPicker}

// recordWake stores one delivery that tmux accepted. A failure to store it is logged: counting never
// fails or repeats a delivery. A retry of the same delivery (same ref) is not counted again.
func (m *Manager) recordWake(category, ref string, members []string, bytes int) {
	w := store.Wake{
		At: m.Now(), Kind: store.WakeDelivery, Category: category, Bytes: bytes,
		BatchSize: max(len(members), 1), Members: strings.Join(members, ","), Ref: ref,
	}
	if refs, ok := strings.CutPrefix(ref, WakeJobDone+":"); ok && category == WakeJobDone {
		// A job-done line is retried after a failed mark, possibly regrouped (a member resumed in
		// between): its identity is that of each job, so a retry of already counted jobs adds nothing.
		w.MemberRefs = strings.Split(refs, ",")
	}
	_, err := m.Store.AddWake(w)
	if err != nil {
		m.Log.Printf("record %s delivery: %v", category, err)
	}
}

// RecordInjection stores the bytes of text a hook added to the session's context. Empty text is not
// an injection. One that cannot be stored is kept as a line in the dropped-injections file, so the
// report can say its numbers are a lower bound.
func (m *Manager) RecordInjection(category, text string) {
	if text == "" {
		return
	}
	if _, err := m.Store.AddWake(store.Wake{At: m.Now(), Kind: store.WakeInjection, Category: category, Bytes: len(text), BatchSize: 1}); err != nil {
		m.Log.Printf("record %s injection: %v", category, err)
		noteDroppedInjection(m.StateDir, category, len(text), m.Now())
	}
}

// RecordHookInjection is RecordInjection for a hook that holds a read-only manager: it opens the
// agent's database for this one write with a short busy wait and gives up quietly (the error is
// returned for the caller to show on stderr) when the database is busy or has no such table. An
// injection it could not store is counted in the dropped-injections file (see WakeStats).
func RecordHookInjection(agent, category, text string, now time.Time) error {
	if text == "" {
		return nil
	}
	dir, err := StateDir(agent)
	if err != nil {
		return err
	}
	err = recordQuick(dir, category, text, now)
	if err != nil {
		noteDroppedInjection(dir, category, len(text), now)
	}
	return err
}

func recordQuick(dir, category, text string, now time.Time) error {
	st, err := store.OpenQuickWriter(filepath.Join(dir, "jobs.db"), 300*time.Millisecond)
	if err != nil {
		return err
	}
	defer st.Close()
	_, err = st.AddWake(store.Wake{At: now, Kind: store.WakeInjection, Category: category, Bytes: len(text), BatchSize: 1})
	return err
}

// DroppedInjectionsFile is the file in the state dir that lists the injections that could not be
// stored, one "<unix time> <category> <bytes>" line each.
const DroppedInjectionsFile = "injections-dropped"

// maxDroppedFileBytes bounds the dropped-injections file; past it no line is added and only the
// saturation marker (SaturatedInjectionsFile) records that injections are still being lost.
const maxDroppedFileBytes = 1 << 20

// SaturatedInjectionsFile holds the unix time of the latest injection lost after the dropped file was
// full. It outlives the entries that age out of a report's window, so a report knows it is incomplete.
const SaturatedInjectionsFile = "injections-dropped.saturated"

// noteDroppedInjection appends one line to the dropped-injections file. Best effort by design: the
// hook this serves must never fail or wait for it.
func noteDroppedInjection(dir, category string, bytes int, at time.Time) {
	path := filepath.Join(dir, DroppedInjectionsFile)
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxDroppedFileBytes {
		_ = os.WriteFile(filepath.Join(dir, SaturatedInjectionsFile), []byte(strconv.FormatInt(at.Unix(), 10)+"\n"), 0o600)
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%d %s %d\n", at.Unix(), category, bytes)
}

// droppedSaturated reports whether an injection was lost, uncounted, at or after since because the
// dropped-injections file was full.
func droppedSaturated(dir string, since time.Time) bool {
	b, err := os.ReadFile(filepath.Join(dir, SaturatedInjectionsFile))
	if err != nil {
		return false
	}
	at, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return err == nil && at >= since.Unix()
}

// droppedInjections counts the lines of the dropped-injections file at or after since, and their bytes.
func droppedInjections(dir string, since time.Time) (n, bytes int) {
	b, err := os.ReadFile(filepath.Join(dir, DroppedInjectionsFile))
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		at, err1 := strconv.ParseInt(f[0], 10, 64)
		sz, err2 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil || at < since.Unix() {
			continue
		}
		n++
		bytes += sz
	}
	return n, bytes
}

// deliverCounted types line into the session under an already held tmux.lock, adds the pending SLO
// breach note to it (the note rides on this delivery, never a wake of its own) and, once tmux
// accepted it, records the delivery and marks the noted breaches. ref identifies the delivery for
// the retry rule of recordWake; members are the things it carries.
func (m *Manager) deliverCounted(ctx context.Context, target, category, ref string, members []string, line string) error {
	return m.deliver(ctx, target, category, ref, members, line, true)
}

// deliverPlain is deliverCounted for a line whose text is a fixed contract (the heartbeat wake line is
// matched exactly): it carries no breach note, which stays pending for the next delivery that may.
func (m *Manager) deliverPlain(ctx context.Context, target, category, ref string, members []string, line string) error {
	return m.deliver(ctx, target, category, ref, members, line, false)
}

func (m *Manager) deliver(ctx context.Context, target, category, ref string, members []string, line string, withNote bool) error {
	return m.deliverVia(ctx, category, ref, members, line, withNote, func(ctx context.Context, l string) (string, error) {
		return l, m.sendTmux(ctx, target, l)
	})
}

// deliverVia is deliver with the typing left to send, which returns the line it actually typed (a
// job-done line may gain an idle note inside the tmux delivery, #165), so the recorded bytes are the
// bytes the session received.
func (m *Manager) deliverVia(ctx context.Context, category, ref string, members []string, line string, withNote bool, send func(ctx context.Context, line string) (string, error)) error {
	var ids []int64
	if withNote {
		var note string
		note, ids = m.sloNote()
		if note != "" {
			line += " · " + note
		}
	}
	typed, err := send(ctx, line)
	if err != nil {
		return err
	}
	line = typed
	if err := m.Store.MarkSLOBreachesNoted(ids, m.Now()); err != nil {
		// The line is out: the next note leaves these breaches out and the mark is retried then.
		m.Log.Printf("slo breach note: %v", err)
		m.holdNoted(ids)
	}
	m.recordWake(category, ref, members, len(Printable(line, 0)))
	return nil
}

// holdNoted remembers breaches that a delivery carried and whose mark failed.
func (m *Manager) holdNoted(ids []int64) {
	m.noteMu.Lock()
	defer m.noteMu.Unlock()
	if m.noteUnmarked == nil {
		m.noteUnmarked = map[int64]bool{}
	}
	for _, id := range ids {
		m.noteUnmarked[id] = true
	}
}

// retryHeldNotes tries again to mark the breaches held by holdNoted and returns the ones still held.
func (m *Manager) retryHeldNotes() map[int64]bool {
	m.noteMu.Lock()
	defer m.noteMu.Unlock()
	if len(m.noteUnmarked) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(m.noteUnmarked))
	for id := range m.noteUnmarked {
		ids = append(ids, id)
	}
	if err := m.Store.MarkSLOBreachesNoted(ids, m.Now()); err != nil {
		m.Log.Printf("slo breach note: %v", err)
		return maps.Clone(m.noteUnmarked)
	}
	m.noteUnmarked = nil
	return nil
}

// sendCounted is deliverCounted under tmux.lock with the delivery timeout (sendTmuxLocked).
func (m *Manager) sendCounted(target, category, ref string, members []string, line string) error {
	return m.withTmuxLock(func(ctx context.Context) error {
		return m.deliverCounted(ctx, target, category, ref, members, line)
	})
}

// ---- reports ----

// WakeDay is one local day of the wake report: deliveries and injections counted per category.
type WakeDay struct {
	Day    string         `json:"day"`
	Counts map[string]int `json:"counts"`
	Total  int            `json:"total"`
}

// WakeCategoryStats is one category of the wake report. Count is deliveries (or injections); Members
// the things they carried (jobs of coalesced lines, alerts), so Members/Count is the batch size.
type WakeCategoryStats struct {
	Category   string  `json:"category"`
	Kind       string  `json:"kind"`
	Count      int     `json:"count"`
	Members    int     `json:"members"`
	Bytes      int     `json:"bytes"`
	BytesP50   int     `json:"bytes_p50"`
	BytesP95   int     `json:"bytes_p95"`
	BytesMax   int     `json:"bytes_max"`
	MeanBatch  float64 `json:"mean_batch"`
	LargestRun int     `json:"largest_batch"`
}

// WakeStatsReport is the output of `agent-jobs stats wakes` and the wake_stats MCP tool.
type WakeStatsReport struct {
	Since          time.Time           `json:"since,omitempty"`
	Days           []WakeDay           `json:"days"`
	Categories     []WakeCategoryStats `json:"categories"`
	Wakes          int                 `json:"wakes"`
	WakeBytes      int                 `json:"wake_bytes"`
	InjectionBytes int                 `json:"injection_bytes"`
	// InjectionsUnrecorded counts the hook injections that could not be stored (and their bytes): the
	// injection numbers above are a lower bound by this much.
	InjectionsUnrecorded     int `json:"injections_unrecorded"`
	InjectionBytesUnrecorded int `json:"injection_bytes_unrecorded"`
	// InjectionsUncounted is true when injections were lost beyond what the two counts above hold (the
	// dropped-injections file was full): the numbers are a lower bound even when the counts are 0.
	InjectionsUncounted bool `json:"injections_uncounted,omitempty"`
}

// WakeStats summarizes the recorded deliveries and injections at or after since (zero = all).
func (m *Manager) WakeStats(since time.Time) (WakeStatsReport, error) {
	rows, err := m.Store.Wakes(since)
	if err != nil {
		return WakeStatsReport{}, err
	}
	rep := WakeStatsReport{Days: []WakeDay{}, Categories: []WakeCategoryStats{}}
	if !since.IsZero() {
		rep.Since = since
	}
	loc := m.loc()
	days := map[string]*WakeDay{}
	byCat := map[string][]store.Wake{}
	for _, w := range rows {
		key := w.Kind + "/" + w.Category
		byCat[key] = append(byCat[key], w)
		day := w.At.In(loc).Format("2006-01-02")
		d := days[day]
		if d == nil {
			d = &WakeDay{Day: day, Counts: map[string]int{}}
			days[day] = d
		}
		d.Counts[w.Category]++
		d.Total++
		if w.Kind == store.WakeDelivery {
			rep.Wakes++
			rep.WakeBytes += w.Bytes
		} else {
			rep.InjectionBytes += w.Bytes
		}
	}
	for _, d := range days {
		rep.Days = append(rep.Days, *d)
	}
	sort.Slice(rep.Days, func(a, b int) bool { return rep.Days[a].Day < rep.Days[b].Day })
	rep.InjectionsUnrecorded, rep.InjectionBytesUnrecorded = droppedInjections(m.StateDir, since)
	rep.InjectionsUncounted = droppedSaturated(m.StateDir, since)
	order := append(slices.Clone(WakeCategories), InjectionCategories...)
	keys := make([]string, 0, len(byCat))
	for k := range byCat {
		keys = append(keys, k)
	}
	rank := func(k string) int {
		_, cat, _ := strings.Cut(k, "/")
		if i := slices.Index(order, cat); i >= 0 {
			return i
		}
		return len(order)
	}
	sort.Slice(keys, func(a, b int) bool {
		if ra, rb := rank(keys[a]), rank(keys[b]); ra != rb {
			return ra < rb
		}
		return keys[a] < keys[b]
	})
	for _, k := range keys {
		ws := byCat[k]
		cs := WakeCategoryStats{Category: ws[0].Category, Kind: ws[0].Kind, Count: len(ws)}
		sizes := make([]int, 0, len(ws))
		for _, w := range ws {
			sizes = append(sizes, w.Bytes)
			cs.Bytes += w.Bytes
			cs.Members += w.BatchSize
			cs.LargestRun = max(cs.LargestRun, w.BatchSize)
		}
		sort.Ints(sizes)
		cs.BytesP50, cs.BytesP95, cs.BytesMax = percentile(sizes, 0.50), percentile(sizes, 0.95), sizes[len(sizes)-1]
		cs.MeanBatch = math.Round(float64(cs.Members)/float64(cs.Count)*100) / 100
		rep.Categories = append(rep.Categories, cs)
	}
	return rep, nil
}

// ---- SLO ----

// SLOConfig is the slo section of config.json.
type SLOConfig struct {
	// Targets are the norms in seconds by SLO name, e.g. {"deploy": 600, "first_reply": 60}. A name
	// without a target is recorded and shown but never breaches.
	Targets map[string]float64 `json:"targets"`
	// Investigate starts a research job for a breach. Off by default.
	Investigate SLOInvestigateConfig `json:"investigate"`
}

// SLOInvestigateConfig is slo.investigate.
type SLOInvestigateConfig struct {
	Enabled bool `json:"enabled"`
	// CooldownMinutes is the minimum gap between two started investigations. 0 = 60.
	CooldownMinutes int `json:"cooldown_minutes"`
	// DailyMax caps the investigations started within 24 hours. 0 = 3.
	DailyMax int `json:"daily_max"`
	// Complexity of the research job (light|normal|hard). "" = light.
	Complexity string `json:"complexity"`
}

// Defaults of slo.investigate.
const (
	DefaultSLOCooldownMinutes = 60
	DefaultSLODailyMax        = 3
	// maxSLOSeconds bounds a recorded duration (30 days): a typo in the unit must not poison the percentiles.
	maxSLOSeconds = 30 * 24 * 3600
)

var reSLOName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

// ValidateSLOName checks an SLO name: lower-case letters, digits, '_', '.', '-', at most 64.
func ValidateSLOName(name string) error {
	if !reSLOName.MatchString(name) {
		return fmt.Errorf("slo name %q must be 1-64 characters: lower-case letters, digits, '_', '.', '-', starting with a letter or digit", Printable(name, 80))
	}
	return nil
}

func (c SLOConfig) validate() error {
	for name, v := range c.Targets {
		if err := ValidateSLOName(name); err != nil {
			return fmt.Errorf("slo.targets: %w", err)
		}
		if math.IsNaN(v) || v <= 0 || v > maxSLOSeconds {
			return fmt.Errorf("slo.targets.%s %v must be a number of seconds above 0 and at most %d", name, v, maxSLOSeconds)
		}
	}
	i := c.Investigate
	switch {
	case i.CooldownMinutes < 0:
		return fmt.Errorf("slo.investigate.cooldown_minutes %d must be 0 (default %d) or positive", i.CooldownMinutes, DefaultSLOCooldownMinutes)
	case i.DailyMax < 0:
		return fmt.Errorf("slo.investigate.daily_max %d must be 0 (default %d) or positive", i.DailyMax, DefaultSLODailyMax)
	case i.Complexity != "" && !slices.Contains([]string{"light", "normal", "hard"}, i.Complexity):
		return fmt.Errorf("slo.investigate.complexity %q must be light, normal or hard", Printable(i.Complexity, 40))
	}
	return nil
}

func (i SLOInvestigateConfig) cooldown() time.Duration {
	if i.CooldownMinutes > 0 {
		return time.Duration(i.CooldownMinutes) * time.Minute
	}
	return DefaultSLOCooldownMinutes * time.Minute
}

func (i SLOInvestigateConfig) dailyMax() int {
	if i.DailyMax > 0 {
		return i.DailyMax
	}
	return DefaultSLODailyMax
}

// SLORecord is the result of recording a sample.
type SLORecord struct {
	Name    string  `json:"name"`
	Seconds float64 `json:"seconds"`
	// Target is the configured norm (0 = none).
	Target float64 `json:"target,omitempty"`
	// Breach: the sample is over its norm; the next delivery to the session carries one line about it.
	Breach bool `json:"breach"`
}

// RecordSLO stores one duration for name (seconds). Over its configured norm it is also a breach, which
// the next delivery into the session mentions once.
func (m *Manager) RecordSLO(name string, seconds float64) (SLORecord, error) {
	if err := ValidateSLOName(name); err != nil {
		return SLORecord{}, err
	}
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds > maxSLOSeconds {
		return SLORecord{}, fmt.Errorf("slo seconds %v must be between 0 and %d", seconds, maxSLOSeconds)
	}
	target := m.Config().SLO.Targets[name]
	b, err := m.Store.AddSLOSample(name, seconds, target, m.Now())
	if err != nil {
		return SLORecord{}, err
	}
	return SLORecord{Name: name, Seconds: seconds, Target: target, Breach: b != nil}, nil
}

// SLOStat is one SLO name of `slo show`.
type SLOStat struct {
	Name  string  `json:"name"`
	Count int     `json:"count"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	Last  float64 `json:"last"`
	// LastAt is when the last sample was recorded (zero when there is none).
	LastAt time.Time `json:"last_at,omitempty"`
	// Target is the configured norm in seconds (0 = none).
	Target float64 `json:"target,omitempty"`
	// Over counts the samples above Target.
	Over int `json:"over"`
}

// SLOShow summarizes the samples at or after since (zero = all), one row per name: every recorded name
// and every configured target.
func (m *Manager) SLOShow(since time.Time) ([]SLOStat, error) {
	samples, err := m.Store.SLOSamples(since)
	if err != nil {
		return nil, err
	}
	targets := m.Config().SLO.Targets
	vals := map[string][]float64{}
	last := map[string]store.SLOSample{}
	for _, s := range samples {
		vals[s.Name] = append(vals[s.Name], s.Seconds)
		last[s.Name] = s // oldest first: the final one wins
	}
	names := map[string]bool{}
	for n := range vals {
		names[n] = true
	}
	for n := range targets {
		names[n] = true
	}
	out := make([]SLOStat, 0, len(names))
	for n := range names {
		st := SLOStat{Name: n, Count: len(vals[n]), Target: targets[n]}
		if l, ok := last[n]; ok {
			st.Last, st.LastAt = l.Seconds, l.At
		}
		sorted := slices.Clone(vals[n])
		sort.Float64s(sorted)
		st.P50, st.P95 = percentile(sorted, 0.50), percentile(sorted, 0.95)
		for _, v := range vals[n] {
			if st.Target > 0 && v > st.Target {
				st.Over++
			}
		}
		out = append(out, st)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// secText is a duration in seconds for a line: whole seconds without a fraction.
func secText(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0fs", v)
	}
	return fmt.Sprintf("%.1fs", v)
}

// secFixed is v with p decimals, trailing zeros dropped.
func secFixed(v float64, p int) string {
	s := strconv.FormatFloat(v, 'f', p, 64)
	if p > 0 {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// secPair writes a sample and its norm with the fewest decimals that tell them apart ("600.01s" against
// "600s", never "600.0s" against "600s").
func secPair(v, norm float64) (string, string) {
	p := 0
	for ; p < 6 && secFixed(v, p) == secFixed(norm, p); p++ {
	}
	return secFixed(v, p) + "s", secFixed(norm, p) + "s"
}

// maxNoteBytes bounds the breach note of one delivery: names that do not fit stay pending for the next
// delivery, so a burst never grows a line without limit.
const maxNoteBytes = 400

// sloNote is the one line about the breaches no delivery has carried yet ("" = none) and the ids of
// those it shows. Breaches of one name are folded into one entry (their count and the worst), so a
// burst of slow runs is one short note; entries beyond maxNoteBytes are left out and stay pending. A
// failure to read them is logged and gives no note: a delivery never waits on it.
func (m *Manager) sloNote() (string, []int64) {
	held := m.retryHeldNotes()
	bs, err := m.Store.UnnotedSLOBreaches()
	if err != nil {
		m.Log.Printf("slo breach note: %v", err)
		return "", nil
	}
	type entry struct {
		worst store.SLOBreach
		n     int
		ids   []int64
	}
	var order []string
	byName := map[string]*entry{}
	for _, b := range bs {
		if held[b.ID] {
			continue // already typed once; its mark is still failing
		}
		e := byName[b.Name]
		if e == nil {
			e = &entry{worst: b}
			byName[b.Name] = e
			order = append(order, b.Name)
		}
		e.n++
		e.ids = append(e.ids, b.ID)
		if b.Seconds > e.worst.Seconds {
			e.worst = b
		}
	}
	var parts []string
	var ids []int64
	size := len("slo breach: ")
	for _, n := range order {
		e := byName[n]
		secs, norm := secPair(e.worst.Seconds, e.worst.Target)
		p := fmt.Sprintf("%s %s > %s", n, secs, norm)
		if e.n > 1 {
			p = fmt.Sprintf("%s %dx, worst %s > %s", n, e.n, secs, norm)
		}
		if len(parts) > 0 && size+len(p)+2 > maxNoteBytes {
			break
		}
		size += len(p) + 2
		parts = append(parts, p)
		ids = append(ids, e.ids...)
	}
	if len(parts) == 0 {
		return "", nil
	}
	return "slo breach: " + strings.Join(parts, "; "), ids
}

// sloInvestigate starts one research job per breach when slo.investigate is on, within the cooldown
// (since the last started investigation, however long ago) and the daily cap. A breach that is not
// investigated for either reason is marked with the reason and never revisited: a burst of breaches
// has one cause. The breach is reserved in the store before its job is queued (ClaimSLOInvestigation)
// and linked after: a failure between the two leaves a reservation that still counts, so no breach
// ever starts a second job.
func (m *Manager) sloInvestigate() error {
	cfg := m.Config().SLO.Investigate
	if !cfg.Enabled {
		return nil
	}
	pending, err := m.Store.UninvestigatedSLOBreaches()
	if err != nil || len(pending) == 0 {
		return err
	}
	for _, b := range pending {
		now := m.Now()
		reserved, _, err := m.Store.ClaimSLOInvestigation(b.ID, now, cfg.cooldown(), cfg.dailyMax())
		if err != nil {
			return err
		}
		if !reserved {
			continue
		}
		complexity := cfg.Complexity
		if complexity == "" {
			complexity = "light"
		}
		j, err := m.Enqueue(StartRequest{
			Title: "SLO breach: " + b.Name, Kind: "research", Brief: m.sloBrief(b), Complexity: complexity,
		})
		if err != nil {
			m.Log.Printf("slo breach %d (%s): investigation not started: %v", b.ID, b.Name, err)
			// Released: a start that failed neither counts against the cap nor blocks the next breach.
			if serr := m.Store.SetSLOBreachInvestigated(b.ID, now, "", "start failed: "+RedactPrintable(err.Error(), 200)); serr != nil {
				return serr
			}
			continue
		}
		m.Log.Printf("slo breach %d (%s): investigation job %s queued", b.ID, b.Name, j.ID)
		if err := m.Store.SetSLOBreachInvestigated(b.ID, now, j.ID, ""); err != nil {
			return fmt.Errorf("slo breach %d: job %s queued but not linked (the reservation stays, no second job): %w", b.ID, j.ID, err)
		}
	}
	return nil
}

// sloBrief is the research job's brief for a breach: the numbers only, no instructions from data.
func (m *Manager) sloBrief(b store.SLOBreach) string {
	var recent []string
	if samples, err := m.Store.SLOSamples(b.At.Add(-7 * 24 * time.Hour)); err == nil {
		for _, s := range slices.Backward(samples) {
			if s.Name == b.Name && len(recent) < 10 {
				recent = append(recent, secText(s.Seconds))
			}
		}
	}
	secs, norm := secPair(b.Seconds, b.Target)
	return fmt.Sprintf("The SLO %q was breached: %s against a norm of %s (recorded %s). The last samples of this SLO, newest first: %s.\n\n"+
		"Research task, read-only: list the most likely causes of the slowdown that can be read from the materials you are given, "+
		"rank them by likelihood, and name one concrete check for each. Change nothing.",
		b.Name, secs, norm, b.At.UTC().Format(time.RFC3339), strings.Join(recent, ", "))
}

// idStrings is ids as decimal strings, in order.
func idStrings(ids []int64) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, strconv.FormatInt(id, 10))
	}
	return out
}

// idsText joins ids for a delivery identity.
func idsText(ids []string) string { return strings.Join(ids, ",") }

// sessionIDs are the ids of schedules fired into the session, as strings.
func sessionIDs(list []*store.Schedule) []string {
	out := make([]string, 0, len(list))
	for _, sc := range list {
		out = append(out, strconv.FormatInt(sc.ID, 10))
	}
	return out
}
