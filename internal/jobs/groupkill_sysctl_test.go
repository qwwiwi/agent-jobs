package jobs

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// The macOS algorithm runs against fake sysctl identities on Linux as well: no /proc or pidfds.
func TestEndMarkedGroupBySysctl(t *testing.T) {
	const g, member = 1 << 22, 1<<22 + 1
	const id = "261021-000000-test"
	marked := sysctlInfo{pgid: g, env: []string{JobEnvVar + "=" + id}}
	for _, tc := range []struct {
		name, change string
		want         groupEnd
		wantSignals  []string
	}{
		{"gone", "gone", groupGone, nil},
		{"unmarked", "unmarked", groupRefused, nil},
		{"other job", "other job", groupRefused, nil},
		{"zombie", "zombie", groupRefused, nil},
		{"moved", "moved", groupRefused, nil},
		{"unreadable identity", "unreadable", groupRefused, nil},
		{"unreadable group", "listing fails", groupRefused, nil},
		{"empty listing while group lives", "empty", groupRefused, nil},
		{"ends on TERM", "TERM", groupEnded, []string{fmt.Sprintf("-%d:15", g)}},
		{"ends on KILL", "KILL", groupEnded, []string{fmt.Sprintf("-%d:15", g), fmt.Sprintf("-%d:9", g)}},
		{"marker lost after TERM", "lost", groupSurvived, []string{fmt.Sprintf("-%d:15", g)}},
		{"listing fails after TERM", "lost listing", groupSurvived, []string{fmt.Sprintf("-%d:15", g)}},
		{"marked member changes after TERM", "handoff", groupEnded, []string{fmt.Sprintf("-%d:15", g), fmt.Sprintf("-%d:9", g)}},
		{"signal error", "signal error", groupSurvived, []string{fmt.Sprintf("-%d:15", g), fmt.Sprintf("-%d:9", g)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newManager(t)
			m.KillGrace = 0
			oldRoot, oldExists, oldKill, oldOpen := procRoot, groupExists, killFn, pidfdOpen
			t.Cleanup(func() { procRoot, groupExists, killFn, pidfdOpen = oldRoot, oldExists, oldKill, oldOpen })
			procRoot = filepath.Join(t.TempDir(), "no-proc")
			pidfdOpen = func(int) (int, error) { t.Fatal("sysctl path opened a pidfd"); return -1, nil }
			live := tc.change != "gone"
			groupExists = func(got int) bool {
				if got != g {
					t.Fatalf("checked group %d, want %d", got, g)
				}
				return live
			}
			termSent := false
			fakeSysctl(t, func(pid int) (sysctlInfo, error) {
				p := marked
				if pid != member && !(tc.change == "handoff" && termSent && pid == member+1) {
					return sysctlInfo{pgid: g}, nil // an unmarked sibling does not invalidate ownership
				}
				switch tc.change {
				case "unmarked":
					p.env = nil
				case "other job":
					p.env = []string{JobEnvVar + "=other"}
				case "zombie":
					p.zombie = true
				case "moved":
					p.pgid++
				case "unreadable":
					return sysctlInfo{}, errors.New("unreadable identity")
				case "lost":
					if termSent {
						p.env = nil
					}
				}
				return p, nil
			}, func(int) ([]int, error) {
				if tc.change == "listing fails" || tc.change == "lost listing" && termSent {
					return nil, errors.New("listing unavailable")
				}
				if tc.change == "empty" {
					return nil, nil
				}
				if tc.change == "handoff" && termSent {
					return []int{member + 1}, nil
				}
				return []int{g, member}, nil
			})
			var sent []string
			killFn = func(pid int, sig syscall.Signal) error {
				if pid != -g {
					t.Fatalf("signal by pid %d instead of group %d", pid, g)
				}
				sent = append(sent, fmt.Sprintf("%d:%d", pid, sig))
				termSent = true
				if tc.change == "TERM" || (tc.change == "KILL" || tc.change == "handoff") && sig == syscall.SIGKILL {
					live = false
				}
				if tc.change == "signal error" {
					return syscall.EPERM
				}
				return nil
			}
			res, why := m.endMarkedGroupBySysctl(id, "test group", g, sysctlMarkedMember(id, g))
			if res != tc.want || !slices.Equal(slices.Compact(sent), tc.wantSignals) {
				t.Fatalf("result %v (%s), signals %v; want %v, %v", res, why, sent, tc.want, tc.wantSignals)
			}
			if (res == groupRefused || res == groupSurvived) && why == "" {
				t.Fatal("missing refusal/survival reason")
			}
			if tc.change == "signal error" && !strings.Contains(why, "operation not permitted") {
				t.Fatalf("missing signal error: %q", why)
			}
		})
	}
}

func TestKillAcceptanceGroupBySysctl(t *testing.T) {
	const g, child = 1 << 22, 1<<22 + 1
	const id = "261021-000000-test"
	for _, leader := range []string{"marked", "unmarked", "gone", "zombie"} {
		t.Run(leader, func(t *testing.T) {
			m := newManager(t)
			m.KillGrace = 0
			oldExists, oldKill := groupExists, killFn
			t.Cleanup(func() { groupExists, killFn = oldExists, oldKill })
			live := true
			groupExists = func(int) bool { return live }
			fakeSysctl(t, func(pid int) (sysctlInfo, error) {
				p := sysctlInfo{pgid: g, env: []string{JobEnvVar + "=" + id}}
				if pid == g {
					switch leader {
					case "unmarked":
						p.env = nil
					case "gone":
						return sysctlInfo{}, syscall.ESRCH
					case "zombie":
						p.zombie = true
					}
				}
				return p, nil
			}, func(int) ([]int, error) { return []int{g, child}, nil })
			var sent []string
			killFn = func(pid int, sig syscall.Signal) error {
				sent = append(sent, fmt.Sprintf("%d:%d", pid, sig))
				live = false
				return nil
			}
			res, why := m.killAcceptanceGroupBySysctl(id, g)
			if leader == "unmarked" {
				if res != groupRefused || !strings.Contains(acceptanceGroupNote(g, res, why), "not signalled: its leader carries no marker") {
					t.Fatalf("missing leader refusal: %v %q", res, why)
				}
			} else if res != groupEnded || acceptanceGroupNote(g, res, why) != "" {
				t.Fatalf("unexpected result: %v %q", res, why)
			}
			want := []string{fmt.Sprintf("-%d:15", g)}
			if leader == "unmarked" {
				want = nil
			}
			if !slices.Equal(sent, want) {
				t.Fatalf("signals %v, want %v", sent, want)
			}
		})
	}
}

// Exercise Darwin's refusal/survival reporting on Linux, including marker loss after SIGTERM.
func TestAcceptanceSysctlFailureNotes(t *testing.T) {
	const g, child = 1 << 22, 1<<22 + 1
	const id = "261021-000000-test"
	for _, mode := range []string{"signal failure", "marker lost after TERM"} {
		t.Run(mode, func(t *testing.T) {
			m := newManager(t)
			m.KillGrace = 0
			oldExists, oldKill := groupExists, killFn
			t.Cleanup(func() { groupExists, killFn = oldExists, oldKill })
			groupExists = func(int) bool { return true }
			termSent := false
			fakeSysctl(t, func(pid int) (sysctlInfo, error) {
				if pid == g {
					return sysctlInfo{}, syscall.ESRCH
				}
				p := sysctlInfo{pgid: g, env: []string{JobEnvVar + "=" + id}}
				if mode == "marker lost after TERM" && termSent {
					p.env = nil
				}
				return p, nil
			}, func(int) ([]int, error) { return []int{child}, nil })
			killFn = func(_ int, sig syscall.Signal) error {
				termSent = true
				if mode == "signal failure" {
					return syscall.EPERM
				}
				if sig == syscall.SIGKILL {
					t.Fatal("signalled after marker loss")
				}
				return nil
			}
			res, why := m.killAcceptanceGroupBySysctl(id, g)
			note := acceptanceGroupNote(g, res, why)
			if res != groupSurvived || !strings.Contains(note, ReasonSurvivors) || !strings.Contains(note, "acceptance process group") {
				t.Fatalf("missing survivor note: %v %q", res, note)
			}
			want := "operation not permitted"
			if mode == "marker lost after TERM" {
				want = "no live member carries the job's marker"
			}
			if !strings.Contains(note, want) {
				t.Fatalf("note %q lacks %q", note, want)
			}
		})
	}
}
