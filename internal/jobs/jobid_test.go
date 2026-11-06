package jobs

import (
	"regexp"
	"testing"
	"time"
)

// The id is a timestamp plus a 16-bit random suffix (newID), so jobs started in the same second
// draw from 65536 values only: the insert's UNIQUE constraint on jobs.id is the check, and the
// enqueue must redraw the suffix instead of failing (#129). Here the clock is frozen, so all ids
// share the timestamp prefix and uniqueness rests on the suffix alone; without the redraw this
// fails on the ~30 collisions expected among 2000 ids drawn from the same second.
func TestJobIDUniqueFrozenClock(t *testing.T) {
	m := newManager(t)
	frozen := time.Date(2026, 11, 5, 23, 0, 35, 0, time.UTC)
	m.Now = func() time.Time { return frozen }
	want := regexp.MustCompile(`^261105-230035-[0-9a-f]{4}$`)
	seen := make(map[string]bool, 2000)
	for i := 0; i < 2000; i++ {
		j, err := m.Enqueue(StartRequest{Title: "unique ids", Kind: "general", Brief: "b", Model: "sonnet"})
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
		if !want.MatchString(j.ID) {
			t.Fatalf("enqueue %d: id %q is not <date-time>-<4 hex>", i, j.ID)
		}
		if seen[j.ID] {
			t.Fatalf("enqueue %d: duplicate id %s", i, j.ID)
		}
		seen[j.ID] = true
	}
}
