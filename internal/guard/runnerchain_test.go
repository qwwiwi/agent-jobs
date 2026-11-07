package guard

import (
	"slices"
	"strings"
	"testing"
)

// walkChain reads each runner's own options and operands before the next program (issue #179).
func TestWalkChain(t *testing.T) {
	cases := []struct {
		name   string
		argv   string // split on spaces; all words static
		rsyncs []int  // argv indexes of the rsync words the chain runs
		tails  []int  // starts of unreadable spans
		legacy []int  // starts of programs left to the legacy heuristic
		data   []int  // runner option words, values and operands
	}{
		{"plain rsync", "rsync -a a b", []int{0}, nil, nil, nil},
		{"flock lock named parallel", "flock parallel rsync a b", []int{2}, nil, nil, []int{1}},
		{"flock lock named rsync runs parallel", "flock rsync parallel -j 2 rsync a", nil, []int{2}, nil, []int{1}},
		{"flock options", "flock -w 5 -x --nb ./l rsync a b", []int{6}, nil, nil, []int{1, 2, 3, 4, 5}},
		{"flock attached value", "flock -w5 --timeout=3 ./l rsync a b", []int{4}, nil, nil, []int{1, 2, 3}},
		{"flock -c", "flock ./l -c x", nil, []int{3}, nil, []int{1}},
		{"flock unknown option", "flock --zzz ./l rsync a b", nil, []int{0}, nil, nil},
		{"flock ambiguous prefix", "flock --n ./l rsync a b", nil, []int{0}, nil, nil},
		{"flock fd only", "flock 9", nil, nil, nil, []int{1}},
		{"setsid flags", "setsid -fw --ctty rsync a b", []int{3}, nil, nil, []int{1, 2}},
		{"setsid value rejected", "setsid --fork=1 rsync a b", nil, []int{0}, nil, nil},
		{"find two actions", "find . -exec rsync --exclude parallel a b ; -exec rsync c d ;", []int{3, 10}, nil, nil, nil},
		{"find exec parallel", "find . -exec parallel rsync a ::: h:/x ;", nil, []int{3}, nil, nil},
		{"nice timeout", "nice -n 5 timeout 30 rsync a b", []int{5}, nil, nil, []int{1, 2, 3, 4}},
		{"setsid unmodeled ionice", "setsid ionice -c 3 rsync a b", nil, nil, []int{1}, nil},
		{"setsid echo", "setsid echo rsync a b", nil, nil, []int{1}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			argv := strings.Fields(c.argv)
			ch := walkChain(argv, nil, nil, false)
			starts := func(s []chainSpan) []int {
				var out []int
				for _, x := range s {
					out = append(out, x.at)
				}
				return out
			}
			var data []int
			for k := range ch.data {
				data = append(data, k)
			}
			slices.Sort(data)
			if got := starts(ch.rsyncs); !slices.Equal(got, c.rsyncs) {
				t.Errorf("rsyncs = %v, want %v", got, c.rsyncs)
			}
			if got := starts(ch.tails); !slices.Equal(got, c.tails) {
				t.Errorf("tails = %v, want %v", got, c.tails)
			}
			if got := starts(ch.legacy); !slices.Equal(got, c.legacy) {
				t.Errorf("legacy = %v, want %v", got, c.legacy)
			}
			if !slices.Equal(data, c.data) {
				t.Errorf("data = %v, want %v", data, c.data)
			}
		})
	}
}

// An outer xargs, a nested xargs or an unquoted runner word make the command's argv unreadable.
func TestWalkChainStdinAndBare(t *testing.T) {
	ch := walkChain(strings.Fields("flock ./l rsync a"), nil, nil, true)
	if len(ch.rsyncs) != 1 || !ch.rsyncs[0].stdin {
		t.Errorf("outer xargs: rsyncs = %+v, want one stdin rsync", ch.rsyncs)
	}
	// split strips nice and xargs together: the rsync behind them gets stdin words.
	ch = walkChain(strings.Fields("setsid nice xargs rsync a"), nil, nil, false)
	if len(ch.rsyncs) != 1 || !ch.rsyncs[0].stdin {
		t.Errorf("nested xargs: rsyncs = %+v, want one stdin rsync", ch.rsyncs)
	}
	argv := strings.Fields("flock $L rsync a b")
	bare := []bool{false, true, false, false, false}
	ch = walkChain(argv, bare, bare, false)
	if len(ch.rsyncs) != 0 || len(ch.tails) != 1 || ch.tails[0].at != 0 {
		t.Errorf("bare lock file: rsyncs = %+v tails = %+v, want a tail at 0", ch.rsyncs, ch.tails)
	}
	// A quoted expansion with a static path prefix is the lock file; a bare `"$L"` may be an option.
	dyn := []bool{false, true, false, false, false}
	ch = walkChain(strings.Fields("flock ./locks/$L rsync a b"), dyn, nil, false)
	if len(ch.rsyncs) != 1 || ch.rsyncs[0].at != 2 || !ch.data[1] {
		t.Errorf("quoted lock file: rsyncs = %+v data = %v, want rsync at 2, data 1", ch.rsyncs, ch.data)
	}
	ch = walkChain(strings.Fields("flock $L rsync a b"), dyn, nil, false)
	if len(ch.rsyncs) != 0 || len(ch.tails) != 1 {
		t.Errorf("quoted bare-name lock file: rsyncs = %+v tails = %+v, want one tail", ch.rsyncs, ch.tails)
	}
	ch = walkChain(strings.Fields("find . -exec rsync a b ;"), nil, nil, true)
	if len(ch.rsyncs) != 0 || len(ch.tails) != 1 {
		t.Errorf("xargs find: rsyncs = %+v tails = %+v, want one tail", ch.rsyncs, ch.tails)
	}
}
