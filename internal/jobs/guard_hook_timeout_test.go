package jobs

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/guard"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// A PreToolUse hook that times out lets the tool run: the guard hook of every job kind carries an
// explicit timeout (in seconds) well above the guard's own deadline, past which it refuses.
func TestGuardHookTimeoutAboveDeadline(t *testing.T) {
	m := newManager(t)
	for _, kind := range []string{"fix", "review", "research", "general"} {
		j := insertJob(t, m, "hook-"+kind, store.StatusRunning, nil, func(j *store.Job) { j.Kind = kind })
		b, err := json.Marshal(m.JobSettings(j))
		if err != nil {
			t.Fatal(err)
		}
		var s struct {
			Hooks struct {
				PreToolUse []struct {
					Hooks []struct {
						Timeout int `json:"timeout"`
					} `json:"hooks"`
				} `json:"PreToolUse"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		got := time.Duration(s.Hooks.PreToolUse[0].Hooks[0].Timeout) * time.Second
		if got < guard.DecideBudget+10*time.Second {
			t.Errorf("%s: hook timeout %v, want at least %v", kind, got, guard.DecideBudget+10*time.Second)
		}
	}
}
