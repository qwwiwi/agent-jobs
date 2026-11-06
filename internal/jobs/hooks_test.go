package jobs

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

func TestCapItems(t *testing.T) {
	items := []string{"aaaa", "bbbb", "cccc", "dddd"}
	if got := capItems("H: ", items, 4, ", ", "!", 1000); got != "H: aaaa, bbbb, cccc, dddd!" {
		t.Errorf("fits: %q", got)
	}
	got := capItems("H: ", items, 4, ", ", "!", 40)
	if len(got) > 40 || !strings.HasPrefix(got, "H: aaaa, bbbb") || !strings.HasSuffix(got, "more, see job_list!") {
		t.Errorf("cut list: %q (%d bytes)", got, len(got))
	}
	if got := capItems("H: ", items[:2], 5, ", ", "", 1000); got != "H: aaaa, bbbb, +3 more, see job_list" {
		t.Errorf("caller's own cap counts in the tail: %q", got)
	}
	long := []string{strings.Repeat("é", 200)}
	got = capItems("H: ", long, 3, ", ", "", 80)
	if len(got) > 80 || !utf8.ValidString(got) || !strings.HasSuffix(got, "…, +2 more, see job_list") {
		t.Errorf("first item too long: %q (%d bytes)", got, len(got))
	}
	for max := 120; max < 400; max += 7 {
		var many []string
		for i := range 40 {
			many = append(many, fmt.Sprintf("item-%d-%s", i, strings.Repeat("x", i)))
		}
		out := capItems("[head] 40 things: ", many, 40, ", ", "", max)
		if len(out) > max || !strings.Contains(out, "more, see job_list") {
			t.Fatalf("max %d: %d bytes %q", max, len(out), out)
		}
	}
}

func TestCapText(t *testing.T) {
	if got := capText("short", 100); got != "short" {
		t.Errorf("short: %q", got)
	}
	got := capText(strings.Repeat("ж", 100), 50)
	if len(got) > 50 || !utf8.ValidString(got) || !strings.HasSuffix(got, truncationMarker) {
		t.Errorf("long: %q (%d bytes)", got, len(got))
	}
}

func TestHooksConfig(t *testing.T) {
	c, err := parseConfig(t.TempDir(), []byte(`{"hooks":{"repeat_after_seconds":60,"max_bytes":900}}`))
	if err != nil || c.Hooks.RepeatAfter().Seconds() != 60 || c.Hooks.Max() != 900 {
		t.Fatalf("explicit: %+v %v", c.Hooks, err)
	}
	c, err = parseConfig(t.TempDir(), nil)
	if err != nil || c.Hooks.RepeatAfter() != DefaultHookRepeatAfter || c.Hooks.Max() != DefaultHookMaxBytes {
		t.Fatalf("defaults: %+v %v", c.Hooks, err)
	}
	for _, bad := range []string{`{"hooks":{"max_bytes":100}}`, `{"hooks":{"max_bytes":-1}}`, `{"hooks":{"repeat_after_seconds":-5}}`} {
		if _, err := parseConfig(t.TempDir(), []byte(bad)); err == nil || !strings.Contains(err.Error(), "hooks.") {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

// The owner-waits Stop message lists every pending job without a cap before #166; now it respects the
// byte cap and counts the jobs it left out.
func TestStopHookDecisionByteCap(t *testing.T) {
	var pending []*store.OwnerPending
	for i := range 60 {
		pending = append(pending, &store.OwnerPending{JobID: fmt.Sprintf("job-%02d", i), Title: strings.Repeat("title ", 10),
			Status: store.StatusSucceeded, State: store.OwnerDelivered})
	}
	in := StopHookInput{HookEventName: "Stop", SessionID: "s"}
	out := StopHookDecisionCapped(in, pending, DefaultHookMaxBytes)
	if out == nil || len(out.Reason) > DefaultHookMaxBytes {
		t.Fatalf("reason over the cap: %v", out)
	}
	for _, want := range []string{"job job-00 ", "more, see job_list", "owner_replied"} {
		if !strings.Contains(out.Reason, want) {
			t.Errorf("reason lacks %q: %q", want, out.Reason)
		}
	}
	if def := StopHookDecision(in, pending); def == nil || def.Reason != out.Reason {
		t.Errorf("StopHookDecision must cap at the default")
	}
	small := StopHookDecisionCapped(in, pending, MinHookMaxBytes)
	if len(small.Reason) > MinHookMaxBytes || !strings.Contains(small.Reason, "owner_replied") {
		t.Errorf("minimum cap: %d bytes %q", len(small.Reason), small.Reason)
	}
	one := StopHookDecisionCapped(in, pending[:1], DefaultHookMaxBytes)
	if strings.Contains(one.Reason, "more, see job_list") {
		t.Errorf("one job needs no tail: %q", one.Reason)
	}
}
