package guard

import (
	"archive/tar"
	"os"
	"path/filepath"
	"testing"
)

// These command strings are inspected by Check, never executed.
func TestClosureFixScheduling(t *testing.T) {
	state, _, cfg := stateReadSetup(t)
	controlArchive(t, filepath.Join(cfg.Dir, "links.tar"), []*tar.Header{
		{Name: "L", Linkname: state, Typeflag: tar.TypeSymlink, Mode: 0777},
	})
	for _, policy := range []struct {
		name string
		cfg  Config
	}{
		{"protected", Config{Dir: cfg.Dir, Protected: []string{filepath.Join(state, "jobs.db")}}},
		{"state", Config{Dir: cfg.Dir, StateDir: state, OwnDir: cfg.OwnDir}},
	} {
		t.Run(policy.name, func(t *testing.T) {
			for _, command := range []string{
				`trap 'printf x > L/jobs.db' EXIT; tar xf links.tar`,
				`trap 'cat L/jobs.db' EXIT; tar xf links.tar`,
				`sh -c 'while test ! -L L; do sleep 0.01; done; printf x > L/jobs.db' & tar xf links.tar; wait`,
				`sh -c 'while test ! -L L; do sleep 0.01; done; printf x > L/jobs.db' | tar xf links.tar`,
				"sh -c 'printf x > L/jobs.db' |\n tar xf links.tar",
				`trap 'cat L/jobs.db' ERR INT TERM; tar xf links.tar`,
				`trap -- '-missing; printf x > L/jobs.db' EXIT; tar xf links.tar`,
				`tar xf links.tar | sh -c 'printf x > L/jobs.db'`,
				`tar xf links.tar & cat L/jobs.db; wait`,
				`{ printf x > L/jobs.db; } | tar xf links.tar`,
				`{ printf x > L/jobs.db; } & tar xf links.tar; wait`,
				`trap 'cat ordinary' EXIT; ln -s ordinary L`,
				`cat ordinary & ln -s ordinary L; wait`,
				`cat ordinary | cp -s ordinary L`,
				`cat ordinary | cp -P ordinary L`,
			} {
				t.Run(command, func(t *testing.T) {
					if reason := Check(command, policy.cfg); reason == "" {
						t.Fatal("deferred/concurrent link access allowed")
					}
				})
			}
			for _, command := range []string{
				`trap 'printf x > regular' EXIT; printf x > ordinary`,
				`sh -c 'printf x > regular' & printf x > ordinary; wait`,
				`printf x | cat`,
				`printf x > L/jobs.db; tar xf links.tar`,
				`trap 'printf done' EXIT; tar xf links.tar`,
				`trap '' EXIT; tar xf links.tar`,
				`trap - EXIT; tar xf links.tar`,
				`trap -p EXIT; tar xf links.tar`,
				`printf x > regular; cp ordinary elsewhere`,
			} {
				if reason := Check(command, policy.cfg); reason != "" {
					t.Errorf("ordinary command refused: %s: %s", command, reason)
				}
			}
		})
	}
}

func TestClosureFixCurlOutput(t *testing.T) {
	_, _, cfg := stateReadSetup(t)
	if err := os.MkdirAll(filepath.Join(cfg.Dir, "build"), 0700); err != nil {
		t.Fatal(err)
	}
	controlArchive(t, filepath.Join(cfg.Dir, "safe.tar"), []*tar.Header{{Name: "regular", Mode: 0600}})
	controlArchive(t, filepath.Join(cfg.Dir, "x.tar"), []*tar.Header{{Name: "regular", Mode: 0600}})
	for _, output := range []string{
		`-o `, `-fsSL -o `, `-fsSLo`, `--fail --silent --show-error --location --output=`,
	} {
		command := `curl ` + output + `build/download https://example.test/a; tar xf safe.tar; cat regular`
		if reason := Check(command, cfg); reason != "" {
			t.Errorf("unrelated curl output tainted archive: %s: %s", command, reason)
		}
		for _, access := range []string{`printf x > L/jobs.db`, `cat L/jobs.db`} {
			command := `curl ` + output + `x.tar https://example.test/a; tar xf x.tar; ` + access
			if reason := Check(command, cfg); reason == "" {
				t.Errorf("downloaded archive trusted: %s", command)
			}
		}
	}
	for _, options := range []string{
		`-fsSLO`, `--remote-name`, `--output-dir build -o download`, `--unmodelled -o build/download`,
	} {
		command := `curl ` + options + ` https://example.test/a; tar xf safe.tar; cat regular`
		if reason := Check(command, cfg); reason == "" {
			t.Errorf("unknown curl output policy trusted: %s", command)
		}
	}
}
