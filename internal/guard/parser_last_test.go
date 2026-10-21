package guard

import (
	"archive/tar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Commands in these regressions are inspected, never executed.
func TestParserFixNestedLinkOrder(t *testing.T) {
	state, _, cfg := stateReadSetup(t)
	controlArchive(t, filepath.Join(cfg.Dir, "links.tar"), []*tar.Header{
		{Name: "L", Linkname: state, Typeflag: tar.TypeSymlink, Mode: 0777},
	})
	for _, policy := range []Config{
		{Dir: cfg.Dir, Protected: []string{filepath.Join(state, "jobs.db")}},
		{Dir: cfg.Dir, StateDir: state, OwnDir: cfg.OwnDir},
	} {
		for _, command := range []string{
			`sh -c 'tar xf links.tar'; printf x > L/jobs.db`,
			`sh -c "sh -c 'tar xf links.tar'"; printf x > L/jobs.db`,
			`echo "$(tar xf links.tar)"; printf x > L/jobs.db`,
			"cat <<EOF\n$(tar xf links.tar)\nEOF\nprintf x > L/jobs.db",
			`eval 'tar xf links.tar'; printf x > L/jobs.db`,
			`sh -c 'tar xf links.tar' | cat; printf x > L/jobs.db`,
		} {
			if Check(command, policy) == "" {
				t.Errorf("nested link allowed: %s", command)
			}
		}
		if reason := Check(`printf x > L/jobs.db; sh -c 'tar xf links.tar'`, policy); reason != "" {
			t.Errorf("later nested extraction tainted earlier access: %s", reason)
		}
	}
}

func TestParserFixHardLinkArchives(t *testing.T) {
	state, _, cfg := stateReadSetup(t)
	controlArchive(t, filepath.Join(cfg.Dir, "hard.tar"), []*tar.Header{
		{Name: "hard", Typeflag: tar.TypeDir, Mode: 0700},
		{Name: "hard/jobs.db", Linkname: filepath.Join(state, "jobs.db"), Typeflag: tar.TypeLink, Mode: 0600},
	})
	for _, access := range []string{"printf x > hard/jobs.db", "cat hard/jobs.db"} {
		if Check("tar -xPf hard.tar; "+access, cfg) == "" {
			t.Errorf("hard-link access allowed: %s", access)
		}
	}
}

func TestParserFixFeederDeadline(t *testing.T) {
	previous := make([]*simpleCmd, 1000)
	for i := range previous {
		previous[i] = &simpleCmd{words: []string{"true"}}
	}
	calls := 0
	over := func() bool { calls++; return calls >= 8 }
	words, fed := commandInputWords(&simpleCmd{}, previous, over)
	if !fed || len(words) == 0 || len(words) >= len(previous) || calls > 9 {
		t.Fatalf("collection did not stop at deadline: words=%d fed=%v checks=%d", len(words), fed, calls)
	}
	// An expired collection must make the protected check refuse, even when none
	// of the words collected so far names state.
	_, _, _, cfg := protectedSetup(t)
	scripts := parseAll("true | python3")
	scripts[0].pipelines[0].cmds[1].incoming = previous
	old := protectedBudget
	protectedBudget = time.Nanosecond
	t.Cleanup(func() { protectedBudget = old })
	if reason := checkProtected(scripts, "", cfg); !strings.Contains(reason, "time budget") {
		t.Fatalf("expired check did not fail closed: %q", reason)
	}
}

func TestParserFixArchiveEarlierWrites(t *testing.T) {
	_, _, cfg := stateReadSetup(t)
	controlArchive(t, filepath.Join(cfg.Dir, "safe.tar"), []*tar.Header{{Name: "regular", Mode: 0600}})
	// A real file named "-" must never be used as evidence for stdin extraction.
	controlArchive(t, filepath.Join(cfg.Dir, "-"), []*tar.Header{{Name: "regular", Mode: 0600}})
	for _, producer := range []string{
		`printf replacement > safe.tar`, `: >> safe.tar`,
		`cp payload safe.tar`, `cp -t . input/safe.tar`, `mv payload safe.tar`,
		`install payload safe.tar`, `ln payload safe.tar`, `rsync payload safe.tar`,
		`curl -o safe.tar https://example.test/a`, `curl --output=safe.tar https://example.test/a`,
		`curl -fsSLo safe.tar https://example.test/a`, `curl -fsSLO https://example.test/safe.tar`,
		`wget -O safe.tar https://example.test/a`, `wget --output-document=safe.tar https://example.test/a`,
		`wget https://example.test/safe.tar`, `tee safe.tar`, `tee -a safe.tar`,
		`dd if=payload of=safe.tar`, `truncate -s 0 safe.tar`,
		`python3 -c "open('safe.tar','wb').write(data)"`,
		`sh -c 'printf replacement > safe.tar'`,
		`echo "$(tee safe.tar)"`,
	} {
		t.Run(producer, func(t *testing.T) {
			for _, access := range []string{"printf x > hard/jobs.db", "cat hard/jobs.db"} {
				if Check(producer+"; tar xf safe.tar; "+access, cfg) == "" {
					t.Errorf("modified archive trusted: %s", access)
				}
			}
		})
	}
	for _, command := range []string{
		`tar xf -; printf x > hard/jobs.db`,
		`tar xf - < safe.tar; cat hard/jobs.db`,
		`tar xf /dev/stdin < safe.tar; cat hard/jobs.db`,
		`tar xf /dev/fd/0 < safe.tar; cat hard/jobs.db`,
		`tar x; printf x > hard/jobs.db`,
		`tar xf safe.tar > safe.tar; printf x > hard/jobs.db`,
		`tar xf payload.tar; tar xf safe.tar; printf x > hard/jobs.db`,
	} {
		if Check(command, cfg) == "" {
			t.Errorf("untrusted archive allowed: %s", command)
		}
	}
	for _, command := range []string{
		`tar xf safe.tar; cat regular`,
		`cat safe.tar; tar xf safe.tar; cat regular`,
		`printf x > unrelated; tar xf safe.tar; cat regular`,
		`cp payload elsewhere; tar xf safe.tar; cat regular`,
		`curl --output=elsewhere https://example.test/a; tar xf safe.tar; cat regular`,
		`tar xf safe.tar; cat regular; printf replacement > safe.tar`,
	} {
		if reason := Check(command, cfg); reason != "" {
			t.Errorf("unmodified archive refused: %s: %s", command, reason)
		}
	}
}

func TestParserFixTarUnknownOptions(t *testing.T) {
	state, _, cfg := stateReadSetup(t)
	safe, trailing := filepath.Join(cfg.Dir, "safe.tar"), filepath.Join(cfg.Dir, "trailing.tar")
	controlArchive(t, safe, []*tar.Header{{Name: "regular", Mode: 0600}})
	controlArchive(t, trailing, []*tar.Header{{Name: "L", Linkname: state, Typeflag: tar.TypeSymlink, Mode: 0777}})
	data, err := os.ReadFile(trailing)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(safe, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	for _, options := range []string{
		`-xif safe.tar`, `-xf safe.tar --ignore-zeros`, `--ignore-zeros -xf safe.tar`,
		`-xf safe.tar --unmodelled-option`, `--extr --file=safe.tar`,
		`-xf safe.tar --files-from members`, `-xTf members safe.tar`,
	} {
		for _, access := range []string{"printf x > L/jobs.db", "cat L/jobs.db"} {
			if Check("tar "+options+"; "+access, cfg) == "" {
				t.Errorf("unmodelled tar option trusted: %s; %s", options, access)
			}
		}
	}
	if reason := Check("tar -xf safe.tar; cat regular", cfg); reason != "" {
		t.Errorf("default end-of-archive handling changed: %s", reason)
	}
}

func TestParserFixWrapperLongOptions(t *testing.T) {
	for _, prefix := range []string{
		"timeout --sig=KILL 2 ", "timeout --sig KILL 2 ", "timeout --kill-a 1 2 ",
		"nice --adj 5 ", "nice --adj=5 ", "stdbuf --out L ", "stdbuf --err=L ",
		"xargs --arg-file input ", "xargs --arg-f=input ", "xargs --max-a 1 ",
		"xargs --max-l 1 ", "xargs --process-slot-var slot ",
		"xargs --replace ", "xargs --replace=n ", "xargs --eof ",
		"sudo --user nobody --group nobody --chdir src ", "sudo --us=nobody ",
		"sudo --chroot root --prompt password --command-timeout 1 ",
	} {
		t.Run(prefix, func(t *testing.T) {
			if Check(prefix+"screen", Config{}) == "" {
				t.Error("wrapper hid executable")
			}
			if reason := Check(prefix+"go test ./...", Config{}); reason != "" {
				t.Errorf("ordinary wrapped command refused: %s", reason)
			}
		})
	}
	for _, command := range []string{
		"nice --unknown value go test", "timeout --unknown value 2 go test",
		"xargs --unknown value go test", "stdbuf --unknown value go test",
		"sudo --unknown value go test", "xargs --max 1 go test",
	} {
		if reason := Check(command, Config{}); !strings.Contains(reason, "wrapper option") {
			t.Errorf("unknown/ambiguous option did not fail closed: %s: %s", command, reason)
		}
	}
}
