package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// decideBash runs a Bash call through Decide (the hook's entry point, with its time budget).
func decideBash(t *testing.T, cmd string, cfg Config) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": cfg.Dir, "tool_input": map[string]any{"command": cmd}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := Decide(strings.NewReader(string(b)), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Fix loop 2, item 5: a hook that runs past its timeout lets the tool run, so the guard decides
// within its own deadline: resolutions are memoized per call (5000 words through a 40-link chain),
// a command over the size cap is refused at once, and a decision past the deadline is a refusal.
func TestDecideDeadline(t *testing.T) {
	_, _, cfg := jobFolderSetup(t)
	if err := os.WriteFile(filepath.Join(cfg.Dir, "real.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		target := "l" + strconv.Itoa(i+1)
		if i == 39 {
			target = "real.txt"
		}
		mustSymlink(t, target, filepath.Join(cfg.Dir, "l"+strconv.Itoa(i)))
	}
	done := make(chan string, 1)
	start := time.Now()
	go func() { done <- decideBash(t, "rm -f"+strings.Repeat(" l0", 5000), cfg) }()
	select {
	case r := <-done:
		t.Logf("5000 words through 40 links decided in %v", time.Since(start))
		if r != "" {
			t.Errorf("rm -f l0 x5000 refused: %s", r)
		}
		if el := time.Since(start); el > decideBudget/2 {
			t.Errorf("5000 words through 40 links took %v", el)
		}
	case <-time.After(decideBudget + 5*time.Second):
		t.Fatalf("Decide did not return within %v", decideBudget+5*time.Second)
	}

	long := strings.Repeat("echo x; ", maxCommandBytes/8+1)
	start = time.Now()
	if r := decideBash(t, long, cfg); !strings.Contains(r, "longer than") {
		t.Errorf("a %d-byte command: %q", len(long), r)
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("the size cap took %v", el)
	}

	old := decideBudget
	decideBudget = time.Millisecond
	t.Cleanup(func() { decideBudget = old })
	start = time.Now()
	if r := decideBash(t, strings.Repeat("echo a b c d e f g; ", 3000), cfg); !strings.Contains(r, "time budget") {
		t.Errorf("past the deadline: %q", r)
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("the deadline refusal took %v", el)
	}
}
