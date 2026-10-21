package guard

import (
	"archive/tar"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParserFixOptionBoundaries(t *testing.T) {
	for _, prefix := range []string{
		"xargs -I n ", "xargs -In ", "xargs -n 1 ", "xargs -n1 ",
		"xargs -L 1 ", "xargs -L1 ", "xargs -P 2 ", "xargs -P2 ",
		"xargs -d n ", "xargs -dn ", "xargs -a input ", "xargs -ainput ",
		"xargs --max-args 1 ", "xargs -i ", "xargs --replace ",
		"busybox xargs -In ", "nice -n 5 ", "nice -n5 ", "nice --adjustment 5 ",
		"timeout -s TERM -k 1 2 ", "timeout -sTERM -k1 2 ",
		"stdbuf -o L -e L -i 0 ", "stdbuf -oL -eL -i0 ",
		"stdbuf --output L --error L --input 0 ", "env -CsrcS ",
		"watch -p ", "watch -tn 2 ",
	} {
		t.Run(prefix, func(t *testing.T) {
			if Check(prefix+"screen", Config{}) == "" {
				t.Error("hidden executable allowed")
			}
			if reason := Check(prefix+"go test ./...", Config{}); reason != "" {
				t.Error(reason)
			}
		})
	}
	reviewCases(t, Config{}, []struct {
		name, cmd string
		block     bool
	}{
		{"vim_separate", `vim -e -s -c '!screen'`, true},
		{"vim_execute_function", `vim -c 'call execute("!screen")'`, true},
		{"vim_execute_data", `vim -c 'let msg = "execute(\"!screen\")"'`, false},
		{"script_output_data", `script --log-out screen -c 'echo ok'`, false},
		{"awk_attached", `gawk -e'BEGIN {system("screen")}'`, true},
		{"awk_long_attached", `gawk --source='BEGIN {system("screen")}'`, true},
		{"perl_quote_command", `perl -e 'system qq(screen)'`, true},
		{"perl_nested_data", `perl -e 'print q(system(q(screen)))'`, false},
	})
}

func TestParserFixCompoundInputs(t *testing.T) {
	_, _, _, cfg := protectedSetup(t)
	reviewCases(t, cfg, []struct {
		name, cmd string
		block     bool
	}{
		{"trailing_herestring", `{ { python3; }; } <<< "$HOME"`, true},
		{"subshell_process_input", `(python3) < <(printf '%s' "$HOME")`, true},
		{"multiple_consumers", `printf '%s' "$HOME" | { cat; python3; }`, true},
		{"group_to_group", `{ printf '%s' "$HOME"; } | { python3; }`, true},
		{"safe_nested", `printf 'print(1)' | { { python3; }; }`, false},
		{"unrelated_input", `printf '%s' "$HOME"; { python3; }`, false},
	})
}

func TestParserFixUnknownExtraction(t *testing.T) {
	state, _, cfg := stateReadSetup(t)
	if err := os.WriteFile(filepath.Join(cfg.Dir, "existing"), []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, producer := range []string{"tar xf missing.tar", "unzip -Psecret missing.zip"} {
		for _, access := range []string{"cat existing", "printf x > existing", "cat copy/jobs.db", "printf x > copy/config.json"} {
			t.Run(producer+"; "+access, func(t *testing.T) {
				for _, policy := range []Config{
					{Dir: cfg.Dir, Protected: []string{filepath.Join(state, "jobs.db")}},
					{Dir: cfg.Dir, StateDir: state, OwnDir: cfg.OwnDir},
				} {
					if policy.StateDir == "" && strings.HasPrefix(access, "cat ") {
						continue
					}
					if Check(producer+"; "+access, policy) == "" {
						t.Error("unknown extracted link allowed")
					}
				}
			})
		}
	}
	// A later extraction must not taint earlier accesses.
	if reason := Check("cat existing; tar xf missing.tar", cfg); reason != "" {
		t.Fatal(reason)
	}
	// A readable archive of regular files does not introduce unknown links.
	f, err := os.Create(filepath.Join(cfg.Dir, "safe.tar"))
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	if err := tw.WriteHeader(&tar.Header{Name: "regular", Mode: 0600, Size: 0}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if reason := Check("tar xf safe.tar; cat existing", cfg); reason != "" {
		t.Fatal(reason)
	}
}

func TestParserFixExtractionDotDot(t *testing.T) {
	_, _, cfg := stateReadSetup(t)
	if Check("tar xf missing.tar -C build; printf x > build/L/../../out.txt", cfg) == "" {
		t.Error("extracted link traversal cleaned too early")
	}
	if reason := Check("tar xf missing.tar -C build; printf x > ../out.txt", cfg); reason != "" {
		t.Fatal(reason)
	}
}

func TestParserFixGroupedXargsStateRead(t *testing.T) {
	_, _, cfg := stateReadSetup(t)
	cfg.Protected = nil
	for _, cmd := range []string{
		`echo "$HOME" | { xargs cat; }`,
		`echo "$HOME" | (xargs cat)`,
		`{ xargs cat; } <<< "$HOME"`,
	} {
		if Check(cmd, cfg) == "" {
			t.Errorf("grouped state reader allowed: %s", cmd)
		}
	}
}

func TestParserFixRenamedArchiveLinks(t *testing.T) {
	state, _, cfg := stateReadSetup(t)
	f, err := os.Create(filepath.Join(cfg.Dir, "renamed.tar"))
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	if err := tw.WriteHeader(&tar.Header{Name: "prefix/copy", Linkname: state, Typeflag: tar.TypeSymlink, Mode: 0777}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	for _, options := range []string{"--strip-components=1", "--transform=s,prefix/,,"} {
		if Check("tar xf renamed.tar "+options+"; printf x > copy/jobs.db", cfg) == "" {
			t.Errorf("renamed archive link allowed: %s", options)
		}
	}
}
