package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// pullAck acknowledges what one pull returns.
func pullAck(t *testing.T, dir string) []InboxEvent {
	t.Helper()
	var evs []InboxEvent
	_, err := InboxPull(dir, true, func(ev InboxEvent) error { evs = append(evs, ev); return nil })
	if err != nil {
		t.Fatalf("InboxPull: %v", err)
	}
	return evs
}

// Concurrent acknowledging pulls never hand out the same event twice, and together they hand out
// every event once.
func TestInboxPullConcurrentAckNoDuplicates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inbox")
	const n = 300
	t0 := time.Unix(1_800_000_000, 0)
	for i := range n {
		if err := WriteInboxEvent(dir, InboxEvent{ID: fmt.Sprintf("e%03d", i), Line: "line " + strconv.Itoa(i), TS: t0}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := InboxPull(dir, true, func(ev InboxEvent) error {
				mu.Lock()
				seen[ev.ID]++
				mu.Unlock()
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Fatalf("pulled %d distinct events, want %d", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("event %s handed out %d times", id, c)
		}
	}
}

// An event whose output failed is not acknowledged: it and the ones after it stay for the next pull.
func TestInboxPullAckKeepsEventsWhoseOutputFailed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inbox")
	t0 := time.Unix(1_800_000_000, 0)
	for i, id := range []string{"a", "b", "c"} {
		if err := WriteInboxEvent(dir, InboxEvent{ID: id, Line: "line " + id, TS: t0.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	_, err := InboxPull(dir, true, func(ev InboxEvent) error {
		calls++
		if ev.ID == "b" {
			return syscall.EPIPE
		}
		return nil
	})
	if err == nil || calls != 2 {
		t.Fatalf("pull with a failing output: err %v after %d calls", err, calls)
	}
	left := pullAck(t, dir)
	if len(left) != 2 || left[0].ID != "b" || left[1].ID != "c" {
		t.Fatalf("left after a failed output: %+v", left)
	}
}

// An inbox directory others can write to, or that is not the user's, is refused for writes and
// pulls with an error that says why.
func TestInboxDirPermissionsRefused(t *testing.T) {
	for _, mode := range []os.FileMode{0o777, 0o770, 0o702} {
		dir := filepath.Join(t.TempDir(), "inbox")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		err := WriteInboxEvent(dir, InboxEvent{ID: "x", Line: "l", TS: time.Now()})
		if err == nil || !strings.Contains(err.Error(), "writable by group or others") {
			t.Fatalf("write into a %o dir: %v", mode, err)
		}
		_, err = InboxPull(dir, false, func(InboxEvent) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "writable by group or others") {
			t.Fatalf("pull of a %o dir: %v", mode, err)
		}
	}
	if os.Getuid() == 0 {
		t.Skip("root owns everything")
	}
	if err := WriteInboxEvent("/usr", InboxEvent{ID: "x", Line: "l", TS: time.Now()}); err == nil || !strings.Contains(err.Error(), "not owned by") {
		t.Fatalf("write into a foreign dir: %v", err)
	}
	if _, err := InboxPull("/usr", false, func(InboxEvent) error { return nil }); err == nil || !strings.Contains(err.Error(), "not owned by") {
		t.Fatalf("pull of a foreign dir: %v", err)
	}
}
