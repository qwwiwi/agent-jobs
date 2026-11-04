package jobs

import (
	"testing"
)

// TestCoderJobsDefaultTurns: a coder job (fix, general) without max_turns gets the generous coder
// default, every other kind keeps DefaultMaxTurns, and an explicit max_turns always wins (#123).
func TestCoderJobsDefaultTurns(t *testing.T) {
	m := newManager(t)
	for _, c := range []struct {
		kind  string
		turns int
		want  int
	}{
		{"fix", 0, DefaultCoderMaxTurns},
		{"general", 0, DefaultCoderMaxTurns},
		{"review", 0, DefaultMaxTurns},
		{"research", 0, DefaultMaxTurns},
		{"fix", 30, 30},
		{"general", CeilingMaxTurns, CeilingMaxTurns},
	} {
		j := enqueue(t, m, "brief", func(r *StartRequest) { r.Kind, r.MaxTurns = c.kind, c.turns })
		if j.MaxTurns != c.want {
			t.Fatalf("kind %s, max_turns %d: got %d turns, want %d", c.kind, c.turns, j.MaxTurns, c.want)
		}
	}
	if DefaultCoderMaxTurns != 200 || DefaultCoderMaxTurns > CeilingMaxTurns {
		t.Fatalf("coder default %d (ceiling %d)", DefaultCoderMaxTurns, CeilingMaxTurns)
	}
	if _, err := m.Enqueue(StartRequest{Title: "t", Kind: "fix", Brief: "b", Model: "sonnet", MaxTurns: CeilingMaxTurns + 1}); err == nil {
		t.Fatal("max_turns above the ceiling accepted")
	}
}
