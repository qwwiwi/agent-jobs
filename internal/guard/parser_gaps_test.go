package guard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParserGapsExecutionRunners(t *testing.T) {
	for _, cmd := range []string{
		`vim -c '!tmux'`, `vi '+!screen'`, `nvim -c '!at'`, `ex '+!batch'`,
		`vim -c'!tmux'`, `vim --cmd '!screen'`,
		`find . -exec tmux new-session \;`, `find . -execdir screen \;`,
		`xargs tmux`, `env tmux`, `nice -n 5 tmux`, `timeout 2 tmux`,
		`nohup tmux`, `stdbuf -oL tmux`, `setsid tmux`, `watch tmux`,
		`script -c 'tmux'`, `busybox tmux`,
		`script -c 'screen -ls'`, `watch -n 2 'screen -ls'`,
		`perl -e'system("screen")'`, `perl -e 'system "screen"'`,
		`perl -e 'system("tmux")'`, `awk 'BEGIN { system("tmux") }'`,
		`perl -e 'system("screen")'`, `awk 'BEGIN { system("batch") }'`,
		`awk 'system("screen")'`,
	} {
		t.Run(cmd, func(t *testing.T) {
			if Check(cmd, Config{}) == "" {
				t.Fatal("execution runner allowed")
			}
		})
	}
	for _, cmd := range []string{`vim notes.txt`, `vim -c 'set number'`, `echo '!tmux'`,
		`find . -name tmux`, `make -C docker`, `go test -run Screen ./...`,
		`perl -e 'print "on screen"'`, `awk 'print "batch"'`,
		`perl -e 'print "system(\"screen\")"'`} {
		if reason := Check(cmd, Config{}); reason != "" {
			t.Errorf("%s: %s", cmd, reason)
		}
	}
}

func TestParserGapsQuotedPaths(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "a b,c")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Dir: root, Protected: []string{filepath.Join(state, "jobs.db")}}
	for _, path := range []string{root + "/a b,c/./jobs.db", "./a b,c/./jobs.db"} {
		for _, cmd := range []string{
			fmt.Sprintf(`python3 -c "open('%s','w')"`, path),
			fmt.Sprintf(`sqlite3 :memory: "ATTACH DATABASE 'file:%s' AS x"`, path),
			fmt.Sprintf(`sqlite3 :memory: "ATTACH DATABASE 'file:%s?mode=rw' AS x"`, path),
			fmt.Sprintf(`sqlite3 :memory: "ATTACH DATABASE file:'%s' AS x"`, path),
		} {
			t.Run(cmd, func(t *testing.T) {
				if Check(cmd, cfg) == "" {
					t.Fatal("quoted protected path allowed")
				}
			})
		}
	}
	// The same tokenizer protects reads when the private state has such a name.
	readCfg := Config{Dir: root, StateDir: state, OwnDir: filepath.Join(state, "jobs", "j")}
	if Check(`python3 -c "open('./a b,c/./jobs.db')"`, readCfg) == "" {
		t.Error("quoted state read allowed")
	}
	if reason := Check(`python3 -c "open('./other a,b.txt','w')"`, cfg); reason != "" {
		t.Fatal(reason)
	}
}

func TestParserGapsLinkProducers(t *testing.T) {
	_, _, cfg := stateReadSetup(t)
	source := t.TempDir()
	// The old link is harmless here; preserving its relative target at a new location
	// makes it point to the state. No protected source path appears in the command.
	if err := os.Symlink("../../../../thrall", filepath.Join(source, "L")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("safe-target", filepath.Join(source, "safe")); err != nil {
		t.Fatal(err)
	}
	for _, producer := range []string{
		"cp -a old/L L", "cp -P old/L L", "cp -d old/L L", "cp -r old/L L", "mv old/L L",
		"rsync -l old/L L", "rsync -a old/L L", "tar xf a.tar", "unzip a.zip", "bsdtar -xf a.tar", "cpio -id",
	} {
		producer = strings.ReplaceAll(producer, "old/", source+"/")
		cmd := producer + "; printf x > L/jobs.db"
		t.Run(producer, func(t *testing.T) {
			if Check(cmd, cfg) == "" {
				t.Fatal("link traversal allowed")
			}
		})
	}
	for _, cmd := range []string{"cp -a old/safe copy", "mv old/safe copy", "rsync -a src/ build/",
		"tar xf a.tar -C build; echo x > ../out.txt", "unzip a.zip -d build",
		"tar -xvC build -f a.tar; echo x > L/jobs.db",
		"unzip -qd build a.zip; echo x > L/jobs.db",
		"cpio -idmD build < a.cpio; echo x > L/jobs.db",
		"install old/safe regular; echo x > regular.txt", "cp -aL old/L regular; echo x > regular.txt"} {
		cmd = strings.ReplaceAll(cmd, "old/", source+"/")
		if reason := Check(cmd, cfg); reason != "" {
			t.Errorf("%s: %s", cmd, reason)
		}
	}
	for _, cmd := range []string{
		"tar -xvC build -f a.tar; printf x > build/L/jobs.db",
		"unzip -qd build a.zip; printf x > build/L/jobs.db",
		"cpio -idmD build < a.cpio; printf x > build/L/jobs.db",
	} {
		if Check(cmd, cfg) == "" {
			t.Errorf("extraction link traversal allowed: %s", cmd)
		}
	}
}

func TestParserGapsFeeders(t *testing.T) {
	_, _, _, cfg := protectedSetup(t)
	feed := `printf '%s\n' "$HOME"`
	for _, cmd := range []string{
		"{ " + feed + "; echo ok; } | python3",
		"( " + feed + "; echo ok ) | python3",
		"{ { " + feed + "; }; } | python3",
		"python3 <(" + feed + ")", "python3 < <(" + feed + ")",
		`python3 <(printf '%s' ")"; ` + feed + ")",
		"python3 <<< \"$HOME\"", "cat <(" + feed + ") | python3",
		"{ " + feed + "; } | xargs rm",
	} {
		t.Run(cmd, func(t *testing.T) {
			if Check(cmd, cfg) == "" {
				t.Fatal("compound/input feeder allowed")
			}
		})
	}
	for _, interp := range []string{"python3.14", "pypy", "pypy3", "node", "deno", "bun", "ruby", "perl", "php", "lua", "osascript", "Rscript", "/usr/bin/python3.14", "env python3"} {
		cmd := feed + " | " + interp
		t.Run(interp, func(t *testing.T) {
			if Check(cmd, cfg) == "" {
				t.Fatal("interpreter feeder allowed")
			}
		})
	}
	for _, shell := range []string{"sh", "bash", "zsh", "dash", "ksh", "/bin/bash", "env /bin/sh"} {
		if Check(shell+` -c 'tmux'`, cfg) == "" {
			t.Errorf("shell alias %s allowed", shell)
		}
	}
	for _, cmd := range []string{`{ echo ok; echo yes; } | python3`, `(echo ok) | pypy3`,
		`python3 <(echo 'print(1)')`, `lua <<< 'print(1)'`, `cat <(echo "$HOME")`,
		`{ echo "$HOME"; } | cat`, `echo "$HOME"; echo ok | pypy3`,
		`printf 'print(1 / 2)' | python3`, `python3 <(echo 'print(1 / 2)')`} {
		if reason := Check(cmd, cfg); reason != "" {
			t.Errorf("%s: %s", cmd, reason)
		}
	}
}

// These payloads already fail the state-read check. Keep that coverage without
// adding another producer rule for literal symlink calls or ordinary install.
func TestParserGapsAlreadyBlockedByStateRead(t *testing.T) {
	state, _, cfg := stateReadSetup(t)
	for _, cmd := range []string{
		"cp -a " + state + " copy", "mv " + state + " copy", "rsync -a " + state + " copy",
		"install /dev/null " + state + "/jobs.db",
		fmt.Sprintf(`python3 -c "import os; os.symlink('%s','L')"`, state),
		fmt.Sprintf(`perl -e 'symlink("%s", "L")'`, state),
		"{ cat " + state + "/jobs.db; } | python3", "python3 <(cat " + state + "/jobs.db)",
		"python3 <<< '" + state + "/jobs.db'",
		"tar xf a.tar; printf x > missing/../jobs.db",
		"cp -P lnk L; printf x > L/../jobs.db",
	} {
		if reason := checkStateRead(parseAll(cmd), cmd, cfg); !strings.Contains(reason, "blocked by guard") {
			t.Errorf("state-read did not block %s: %s", cmd, reason)
		}
	}
}
